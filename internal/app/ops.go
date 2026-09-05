package app

import (
	"net/http"
	"net/http/pprof"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// newOpsMux serves /healthz, /readyz and /metrics (and pprof in dev) on the
// operations port every role exposes.
func newOpsMux(checker *Checker, reg *prometheus.Registry, withPprof bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", checker.HealthzHandler())
	mux.Handle("GET /readyz", checker.ReadyzHandler())
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	if withPprof {
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	}
	return mux
}
