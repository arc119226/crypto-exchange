package app

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// newAPIServer builds the public REST listener. Phase 0 mounts only the
// middleware chain and RFC 7807 fallbacks; the OpenAPI handlers are mounted
// by the registry/API batch.
func newAPIServer(cfg Config, log *slog.Logger, m *telemetry.HTTPMetrics, _ *deps) *http.Server {
	r := chi.NewRouter()
	r.Use(telemetry.CorrelationMiddleware(log))
	r.Use(m.Middleware(func(req *http.Request) string {
		if rc := chi.RouteContext(req.Context()); rc != nil {
			return rc.RoutePattern()
		}
		return ""
	}))
	r.Use(recoverer())
	notFound := func(w http.ResponseWriter, req *http.Request) {
		writeProblem(w, req, http.StatusNotFound, "Not Found", "no route for "+req.Method+" "+req.URL.Path)
	}
	r.NotFound(notFound)
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		writeProblem(w, req, http.StatusMethodNotAllowed, "Method Not Allowed", "")
	})
	mountPublicAPI(r, cfg)
	// chi only builds its middleware chain once a route exists; this catch-all
	// guarantees unmatched paths still get correlation ids, metrics and a
	// problem+json body even before the OpenAPI routes are mounted.
	r.HandleFunc("/*", notFound)
	return &http.Server{Addr: cfg.HTTPAddr, Handler: r, ReadHeaderTimeout: 5 * time.Second}
}

// mountPublicAPI is replaced when internal/api lands; kept as a seam so
// the server wiring above does not change.
var mountPublicAPI = func(chi.Router, Config) {}

// problem is the RFC 7807 body used for router-level errors.
type problem struct {
	Type          string `json:"type"`
	Title         string `json:"title"`
	Status        int    `json:"status"`
	Detail        string `json:"detail,omitempty"`
	Instance      string `json:"instance,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem{
		Type: "about:blank", Title: title, Status: status, Detail: detail,
		Instance: r.URL.Path, CorrelationID: telemetry.CorrelationID(r.Context()),
	})
}

func recoverer() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					telemetry.Logger(r.Context()).Error("panic in handler", slog.Any("panic", rec))
					writeProblem(w, r, http.StatusInternalServerError, "Internal Server Error", "")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
