package pg

import (
	"context"
	"strings"

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
