package pg

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestQueryName(t *testing.T) {
	cases := map[string]string{
		"-- name: GetOrder :one\nSELECT * FROM trading.orders WHERE id = $1": "GetOrder",
		"  -- name: ListOpenOrdersForBook :many\nSELECT 1":                   "ListOpenOrdersForBook",
		"begin":                        "begin",
		"COMMIT;":                      "COMMIT",
		"LISTEN outbox":                "LISTEN",
		"SELECT pg_advisory_lock($1);": "SELECT",
		"":                             "query",
	}
	for sql, want := range cases {
		assert.Equal(t, want, QueryName(sql), "%q", sql)
	}
}

func TestQueryTracerSpansUnderARecordingParentOnly(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tr := queryTracer{}

	// no parent span: nothing is started, and End on that context is a no-op
	ctx := tr.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "-- name: GetOrder :one\nSELECT 1"})
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	assert.Empty(t, exp.GetSpans())

	// under a span: one client span named after the query, errors recorded
	root, span := tp.Tracer("test").Start(context.Background(), "place")
	ctx = tr.TraceQueryStart(root, nil, pgx.TraceQueryStartData{SQL: "-- name: InsertOrder :one\nINSERT ..."})
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: errors.New("duplicate key")})
	ctx = tr.TraceQueryStart(root, nil, pgx.TraceQueryStartData{SQL: "commit"})
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 3)
	byName := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	insert := byName["InsertOrder"]
	assert.Equal(t, trace.SpanKindClient, insert.SpanKind)
	assert.Equal(t, byName["place"].SpanContext.SpanID(), insert.Parent.SpanID())
	assert.Equal(t, codes.Error, insert.Status.Code)
	assert.Equal(t, codes.Unset, byName["commit"].Status.Code)
	assert.Equal(t, byName["place"].SpanContext.TraceID(), byName["commit"].SpanContext.TraceID())
}
