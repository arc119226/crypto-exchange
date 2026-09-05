package telemetry

import (
	"context"
	"log/slog"
)

type ctxKey int

const (
	correlationKey ctxKey = iota
	loggerKey
)

// CorrelationIDKey is the log attribute and header-derived field name.
const CorrelationIDKey = "correlation_id"

// WithCorrelationID stores the correlation id in ctx.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey, id)
}

// CorrelationID returns the correlation id stored in ctx, or "".
func CorrelationID(ctx context.Context) string {
	if v, ok := ctx.Value(correlationKey).(string); ok {
		return v
	}
	return ""
}

// WithLogger stores a request/command scoped logger in ctx.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// Logger returns the logger stored in ctx, falling back to slog.Default().
func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
