package telemetry

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// RequestIDHeader is the inbound/outbound header carrying the correlation id.
const RequestIDHeader = "X-Request-Id"

const maxRequestIDLen = 128

// CorrelationMiddleware reads X-Request-Id (or generates one), echoes it in
// the response, and stores both the id and a logger enriched with
// correlation_id in the request context.
func CorrelationMiddleware(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if !validRequestID(id) {
				id = newRequestID()
			}
			w.Header().Set(RequestIDHeader, id)
			ctx := WithCorrelationID(r.Context(), id)
			ctx = WithLogger(ctx, base.With(slog.String(CorrelationIDKey, id)))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c < 0x21 || c > 0x7e { // printable ASCII, no spaces
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable for any secure component.
		panic("telemetry: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Middleware records duration and in-flight gauge. routeOf is evaluated
// after the handler ran so routers (chi) can report the matched pattern
// instead of the raw path; it must never return an unbounded label such as
// the request path.
func (m *HTTPMetrics) Middleware(routeOf func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			m.inFlight.Inc()
			defer m.inFlight.Dec()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			route := routeOf(r)
			if route == "" {
				route = "unmatched"
			}
			m.duration.WithLabelValues(route, r.Method, strconv.Itoa(sw.status)).Observe(time.Since(start).Seconds())
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// WriteHeader records the first status code written.
func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write marks the implicit 200 when a handler writes without WriteHeader.
func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
