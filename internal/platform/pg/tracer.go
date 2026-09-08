package pg

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// queryTracer is the pgx.QueryTracer that gives every query a client span
// under the caller's span (docs/plan-v1.0.md §15). A query issued outside
// any sampled span gets no span at all: the check is one context lookup,
// so the tracer can stay attached while nothing is being traced. Statements
// sqlc generated carry their name on the first line ("-- name: GetOrder
// :one"), which becomes the span name; anything else is named by its first
// keyword (begin, commit, LISTEN, ...).
type queryTracer struct{}

type querySpanKey struct{}

// TraceQueryStart implements pgx.QueryTracer.
func (queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !trace.SpanFromContext(ctx).IsRecording() {
		return ctx
	}
	name := QueryName(data.SQL)
	ctx, span := otel.GetTracerProvider().Tracer("github.com/arc119226/crypto-exchange/internal/platform/pg").
		Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
			attribute.String("db.system.name", "postgresql"),
			attribute.String("db.operation.name", name),
		))
	return context.WithValue(ctx, querySpanKey{}, span)
}

// TraceQueryEnd implements pgx.QueryTracer.
func (queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span, ok := ctx.Value(querySpanKey{}).(trace.Span)
	if !ok {
		return
	}
	if data.Err != nil {
		span.RecordError(data.Err)
		span.SetStatus(codes.Error, data.Err.Error())
	}
	span.End()
}

type batchSpanKey struct{}

// TraceBatchStart implements pgx.BatchTracer: one client span per batch
// (one round trip), the queued statements recorded as events on it.
func (queryTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if !trace.SpanFromContext(ctx).IsRecording() {
		return ctx
	}
	name := BatchName(data.Batch)
	ctx, span := otel.GetTracerProvider().Tracer("github.com/arc119226/crypto-exchange/internal/platform/pg").
		Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(
			attribute.String("db.system.name", "postgresql"),
			attribute.String("db.operation.name", name),
			attribute.Int("db.operation.batch.size", data.Batch.Len()),
		))
	return context.WithValue(ctx, batchSpanKey{}, span)
}

// TraceBatchQuery implements pgx.BatchTracer.
func (queryTracer) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	span, ok := ctx.Value(batchSpanKey{}).(trace.Span)
	if !ok {
		return
	}
	if data.Err != nil {
		span.RecordError(data.Err, trace.WithAttributes(attribute.String("db.operation.name", QueryName(data.SQL))))
		span.SetStatus(codes.Error, data.Err.Error())
		return
	}
	span.AddEvent(QueryName(data.SQL))
}

// TraceBatchEnd implements pgx.BatchTracer.
func (queryTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	span, ok := ctx.Value(batchSpanKey{}).(trace.Span)
	if !ok {
		return
	}
	if data.Err != nil {
		span.RecordError(data.Err)
		span.SetStatus(codes.Error, data.Err.Error())
	}
	span.End()
}

// BatchName names a batch after its statements: "batch InsertJournalEntry+
// GetAccounts+LockBalances", at most three names then "+…".
func BatchName(b *pgx.Batch) string {
	if b == nil || len(b.QueuedQueries) == 0 {
		return "batch"
	}
	names := make([]string, 0, 4)
	for i, q := range b.QueuedQueries {
		if i == 3 {
			names = append(names, "…")
			break
		}
		names = append(names, QueryName(q.SQL))
	}
	return "batch " + strings.Join(names, "+")
}

// CountingTracer counts round trips to the server: one per statement run on
// its own, one per batch. It is how the integration tests pin the round-trip
// budget of the engine's hot paths, so a later change that adds a statement
// to the order flow fails a test instead of showing up in a load test.
type CountingTracer struct {
	n atomic.Int64
}

// RoundTrips is the count since the last Reset.
func (c *CountingTracer) RoundTrips() int64 { return c.n.Load() }

// Reset zeroes the count.
func (c *CountingTracer) Reset() { c.n.Store(0) }

// TraceQueryStart implements pgx.QueryTracer.
func (c *CountingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

// TraceQueryEnd implements pgx.QueryTracer.
func (*CountingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// TraceBatchStart implements pgx.BatchTracer.
func (c *CountingTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	c.n.Add(1)
	return ctx
}

// TraceBatchQuery implements pgx.BatchTracer.
func (*CountingTracer) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

// TraceBatchEnd implements pgx.BatchTracer.
func (*CountingTracer) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// QueryName returns the name a query is known by: the X of a leading
// "-- name: X :kind" comment (sqlc's convention), otherwise the first word
// of the statement, or "query" for an empty one.
func QueryName(sql string) string {
	s := strings.TrimSpace(sql)
	if rest, ok := strings.CutPrefix(s, "-- name:"); ok {
		line, _, _ := strings.Cut(rest, "\n")
		if name, _, _ := strings.Cut(strings.TrimSpace(line), " "); name != "" {
			return name
		}
	}
	if word, _, _ := strings.Cut(s, " "); word != "" {
		return strings.TrimRight(word, ";")
	}
	return "query"
}
