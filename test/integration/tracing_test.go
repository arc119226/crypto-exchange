//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

// installTracing puts a synchronous in-memory exporter behind the global
// provider for the duration of one test, the way SetupTracing does with a
// collector. Tests that install it must not run in parallel with each
// other; the integration package runs with -p 1 and no t.Parallel.
func installTracing(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample())))
	})
	return exp
}

func spansByName(exp *tracetest.InMemoryExporter) map[string][]tracetest.SpanStub {
	out := map[string][]tracetest.SpanStub{}
	for _, s := range exp.GetSpans() {
		out[s.Name] = append(out[s.Name], s)
	}
	return out
}

// TestTraceContextFollowsTheOutboxToConsumers: the span that appends an
// event is the parent of every consumer's span for it -- the traceparent
// is stored in the outbox row's headers, the relay copies it onto the NATS
// message, and both consumer kinds extract it (docs/plan-v1.0.md §15).
func TestTraceContextFollowsTheOutboxToConsumers(t *testing.T) {
	exp := installTracing(t)
	h := setupTrading(t)
	url := startNATS(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	nc, err := nats.Connect(url)
	require.NoError(t, err)
	defer nc.Drain() //nolint:errcheck
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	require.NoError(t, eventbus.EnsureStreams(ctx, js))
	stream, err := js.Stream(ctx, eventbus.StreamTrading)
	require.NoError(t, err)
	require.NoError(t, stream.Purge(ctx))

	const consumerName = "trace-test-durable"
	_ = stream.DeleteConsumer(ctx, consumerName) // a shared local server may hold one from an earlier run
	durableTrace := make(chan string, 1)
	durable, err := eventbus.Subscribe(ctx, js, eventbus.ConsumerConfig{
		Durable: consumerName, Stream: eventbus.StreamTrading, FilterSubjects: []string{eventbus.SubjectPrefix + ".order.*.default.*"},
	}, h.log, func(ctx context.Context, _ eventbus.Envelope) error {
		durableTrace <- telemetry.TraceID(ctx)
		return nil
	})
	require.NoError(t, err)
	defer durable.Stop()
	orderedTrace := make(chan string, 1)
	ordered, err := eventbus.SubscribeOrdered(ctx, js, eventbus.OrderedConfig{
		Name: "trace-test-ordered", Stream: eventbus.StreamTrading, FilterSubjects: []string{eventbus.SubjectPrefix + ".order.*.default.*"},
	}, h.log, func(ctx context.Context, _ eventbus.Envelope) {
		orderedTrace <- telemetry.TraceID(ctx)
	})
	require.NoError(t, err)
	defer ordered.Stop()

	// the producer: one order placed under a request span
	buyer := h.account(t, ctx, map[string]string{"USDC": "10000"})
	rctx, endRequest := telemetry.StartSpan(ctx, "POST /v1/orders", telemetry.SpanServer, nil)
	requestTrace := telemetry.TraceID(rctx)
	require.NotEmpty(t, requestTrace)
	res, err := h.svc.PlaceOrder(rctx, limit(buyer, "traced", matching.Buy, "1990", "0.1"))
	require.NoError(t, err)
	require.Equal(t, trading.StatusOpen, res.Order.Status)
	endRequest(nil)

	// the row carries the trace context ...
	var headers map[string]string
	var subject string
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT subject, headers FROM eventbus.outbox WHERE event_type = 'order.accepted' ORDER BY id DESC LIMIT 1`).Scan(&subject, &headers))
	require.Contains(t, headers["traceparent"], requestTrace)
	assert.Equal(t, res.Order.CorrelationID, headers["Correlation-Id"])

	// ... the relay copies it onto the message ...
	relay := eventbus.NewRelay(h.all, eventbus.NewJetStreamPublisher(js), eventbus.RelayConfig{}, h.log)
	_, err = relay.Drain(ctx)
	require.NoError(t, err)
	msg, err := stream.GetLastMsgForSubject(ctx, subject)
	require.NoError(t, err)
	assert.Contains(t, msg.Header.Get("traceparent"), requestTrace)
	assert.Equal(t, res.Order.CorrelationID, msg.Header.Get("Correlation-Id"))

	// ... and both consumer kinds run their handler inside that trace
	for name, ch := range map[string]chan string{"durable": durableTrace, "ordered": orderedTrace} {
		select {
		case got := <-ch:
			assert.Equal(t, requestTrace, got, "%s consumer", name)
		case <-time.After(20 * time.Second):
			t.Fatalf("%s consumer never ran", name)
		}
	}
	require.Eventually(t, func() bool { return len(spansByName(exp)["consume order.accepted"]) >= 2 }, 5*time.Second, 50*time.Millisecond)
	by := spansByName(exp)
	request := by["POST /v1/orders"][0]
	require.Len(t, by["engine place"], 1, "the runner continues the request's trace across its command queue")
	engine := by["engine place"][0]
	assert.Equal(t, request.SpanContext.SpanID(), engine.Parent.SpanID())
	for _, c := range by["consume order.accepted"] {
		assert.Equal(t, trace.SpanKindConsumer, c.SpanKind)
		assert.Equal(t, request.SpanContext.TraceID(), c.SpanContext.TraceID())
		assert.Equal(t, engine.SpanContext.SpanID(), c.Parent.SpanID(), "the consumer span hangs under the engine span that appended the event")
	}
	// the engine's queries are not traced here: setupTrading opens its
	// pool without the query tracer. TestQueryTracer covers pg on its own.
}

// TestTraceContextCrossesTheCommandBus: the api role's client span and the
// engine's server span for one command share a trace, parent to child,
// through the NATS request headers.
func TestTraceContextCrossesTheCommandBus(t *testing.T) {
	exp := installTracing(t)
	h := setupBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	buyer := h.account(t, ctx, map[string]string{"USDC": "10000"})

	rctx, end := telemetry.StartSpan(as(buyer), "POST /v1/orders", telemetry.SpanServer, nil)
	res, err := h.remote.PlaceOrder(rctx, limit(buyer, "bus-traced", matching.Buy, "1990", "0.1"))
	require.NoError(t, err)
	require.Equal(t, trading.StatusOpen, res.Order.Status)
	end(nil)

	require.Eventually(t, func() bool { return len(spansByName(exp)["cmdbus place"]) >= 2 }, 5*time.Second, 50*time.Millisecond)
	by := spansByName(exp)
	request := by["POST /v1/orders"][0]
	var client, server tracetest.SpanStub
	for _, s := range by["cmdbus place"] {
		switch s.SpanKind {
		case trace.SpanKindClient:
			client = s
		case trace.SpanKindServer:
			server = s
		}
	}
	require.True(t, client.SpanContext.IsValid(), "client span")
	require.True(t, server.SpanContext.IsValid(), "server span")
	assert.Equal(t, request.SpanContext.SpanID(), client.Parent.SpanID())
	assert.Equal(t, client.SpanContext.SpanID(), server.Parent.SpanID())
	assert.Equal(t, request.SpanContext.TraceID(), server.SpanContext.TraceID())
}
