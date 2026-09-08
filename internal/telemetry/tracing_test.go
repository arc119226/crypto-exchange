package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// installTestTracing puts a synchronous in-memory exporter behind the
// global provider, which is what SetupTracing would do with a real one.
func installTestTracing(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return exp
}

func TestTracingMiddlewareNamesSpanByRouteAndLogsTraceID(t *testing.T) {
	exp := installTestTracing(t)
	var buf bytes.Buffer
	base := NewLogger(&buf, slog.LevelInfo, "api", "dev")
	routeOf := func(*http.Request) string { return "/v1/orders/{id}" }
	h := CorrelationMiddleware(base)(TracingMiddleware(routeOf)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Logger(r.Context()).Info("in handler")
		w.WriteHeader(http.StatusNoContent)
	})))

	const parentTrace = "0af7651916cd43dd8448eb211c80319c"
	const parentSpan = "b7ad6b7169203331"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/orders/01ABC", nil)
	req.Header.Set("traceparent", "00-"+parentTrace+"-"+parentSpan+"-01")
	req.Header.Set(RequestIDHeader, "corr-1")
	h.ServeHTTP(rec, req)

	spans := exp.GetSpans()
	require.Len(t, spans, 1)
	s := spans[0]
	assert.Equal(t, "GET /v1/orders/{id}", s.Name)
	assert.Equal(t, trace.SpanKindServer, s.SpanKind)
	assert.Equal(t, parentTrace, s.SpanContext.TraceID().String())
	assert.Equal(t, parentSpan, s.Parent.SpanID().String())
	attrs := map[string]string{}
	for _, kv := range s.Attributes {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	assert.Equal(t, "/v1/orders/{id}", attrs["http.route"])
	assert.Equal(t, "204", attrs["http.response.status_code"])
	assert.Equal(t, "corr-1", attrs["exchange.correlation_id"])
	assert.Equal(t, codes.Unset, s.Status.Code)
	assert.Contains(t, buf.String(), `"trace_id":"`+parentTrace+`"`, "the request logger carries the trace id")

	// a 5xx marks the span failed
	exp.Reset()
	boom := TracingMiddleware(routeOf)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	boom.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/orders/x", nil))
	require.Len(t, exp.GetSpans(), 1)
	assert.Equal(t, codes.Error, exp.GetSpans()[0].Status.Code)
}

func TestStartSpanInjectExtractRoundTrip(t *testing.T) {
	exp := installTestTracing(t)
	ctx, endRoot := StartSpan(context.Background(), "root", SpanInternal, nil)
	rootID := TraceID(ctx)
	require.Len(t, rootID, 32)

	carrier := map[string]string{"Correlation-Id": "c"}
	InjectTrace(ctx, carrier)
	require.Contains(t, carrier, "traceparent")
	assert.Contains(t, carrier["traceparent"], rootID)

	// the far side: a fresh context, the carrier, a consumer span
	remote := ExtractTrace(context.Background(), carrier)
	child, endChild := StartSpan(remote, "consume", SpanConsumer, map[string]string{"k": "v"})
	assert.Equal(t, rootID, TraceID(child))
	endChild(errors.New("handler failed"))
	endRoot(nil)

	spans := exp.GetSpans()
	require.Len(t, spans, 2)
	byName := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	root, consume := byName["root"], byName["consume"]
	assert.Equal(t, root.SpanContext.SpanID(), consume.Parent.SpanID())
	assert.Equal(t, trace.SpanKindConsumer, consume.SpanKind)
	assert.Equal(t, codes.Error, consume.Status.Code)
	assert.Equal(t, "handler failed", consume.Status.Description)
	assert.Equal(t, codes.Unset, root.Status.Code)
}

func TestInjectWithoutSpanWritesNothing(t *testing.T) {
	installTestTracing(t)
	carrier := map[string]string{}
	InjectTrace(context.Background(), carrier)
	assert.Empty(t, carrier)
	assert.Equal(t, "", TraceID(context.Background()))
	assert.Equal(t, "", TraceID(ExtractTrace(context.Background(), map[string]string{"traceparent": "garbage"})))
}

func TestSetupTracingDisabledAndEnabled(t *testing.T) {
	stop, err := SetupTracing(context.Background(), TracingConfig{}, nil)
	require.NoError(t, err)
	require.NoError(t, stop(context.Background()), "no endpoint: nothing to stop")

	var posts atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/traces" {
			posts.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	_, err = SetupTracing(context.Background(), TracingConfig{Endpoint: "not a url"}, nil)
	require.Error(t, err)

	stop, err = SetupTracing(context.Background(), TracingConfig{Endpoint: collector.URL, ServiceName: "exchange-test", Version: "dev"}, slog.Default())
	require.NoError(t, err)
	_, end := StartSpan(context.Background(), "exported", SpanInternal, nil)
	end(nil)
	require.NoError(t, stop(context.Background()), "shutdown flushes the batch")
	assert.GreaterOrEqual(t, posts.Load(), int32(1), "the span reached the OTLP endpoint at /v1/traces")

	for in, want := range map[string]string{
		"http://jaeger:4318":            "http://jaeger:4318/v1/traces",
		"http://jaeger:4318/":           "http://jaeger:4318/v1/traces",
		"https://otel.example/custom/p": "https://otel.example/custom/p",
	} {
		got, err := tracesURL(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	installTestTracing(t) // leave the package with a live provider again
}
