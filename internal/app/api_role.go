package app

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/arc119226/crypto-exchange/internal/api"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// newAPIServer builds the public REST listener: correlation ids, metrics and
// panic recovery first, then the OpenAPI routes from internal/api, with RFC
// 7807 bodies for everything the router itself rejects (404/405/500).
// A nil reader (tests) leaves only the fallbacks mounted.
func newAPIServer(cfg Config, log *slog.Logger, m *telemetry.HTTPMetrics, reader registry.Reader) *http.Server {
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
		api.WriteProblem(w, req, http.StatusNotFound, "Not Found", "no route for "+req.Method+" "+req.URL.Path)
	}
	r.NotFound(notFound)
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		api.WriteProblem(w, req, http.StatusMethodNotAllowed, "Method Not Allowed", "")
	})
	if reader != nil {
		api.Mount(r, api.NewHandler(reader, cfg.TenantID))
	}
	if len(r.Routes()) == 0 {
		// chi only builds its middleware chain once a route exists; without
		// any route this catch-all keeps correlation ids, metrics and the
		// problem+json body for unmatched paths. It must not be registered
		// when real routes exist, or it would swallow 405s as 404s.
		r.HandleFunc("/*", notFound)
	}
	return &http.Server{Addr: cfg.HTTPAddr, Handler: r, ReadHeaderTimeout: 5 * time.Second}
}

func recoverer() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					telemetry.Logger(r.Context()).Error("panic in handler", slog.Any("panic", rec))
					api.WriteProblem(w, r, http.StatusInternalServerError, "Internal Server Error", "")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
