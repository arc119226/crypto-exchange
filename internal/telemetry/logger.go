// Package telemetry provides structured logging, correlation-id propagation
// and Prometheus metrics shared by every role of the exchange binary.
package telemetry

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// NewLogger returns a JSON slog.Logger with the fixed attributes every log
// line must carry (docs/plan-v1.0.md §15): the timestamp is emitted as "ts"
// and role/version are attached to every record.
func NewLogger(w io.Writer, level slog.Level, role, version string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				a.Key = "ts"
			}
			return a
		},
	})
	return slog.New(h).With(slog.String("role", role), slog.String("version", version))
}

// ParseLevel converts "debug|info|warn|error" (case-insensitive) to a
// slog.Level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("telemetry: unknown log level %q", s)
}
