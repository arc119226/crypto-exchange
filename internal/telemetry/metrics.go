package telemetry

import (
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// NewRegistry returns a Prometheus registry pre-loaded with the Go runtime
// and process collectors.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}

// BuildInfo registers exchange_build_info{version,commit,go_version,role} = 1.
func BuildInfo(reg prometheus.Registerer, version, commit, role string) {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "exchange_build_info",
		Help: "Build information of the running exchange binary; value is always 1.",
	}, []string{"version", "commit", "go_version", "role"})
	reg.MustRegister(g)
	g.WithLabelValues(version, commit, runtime.Version(), role).Set(1)
}

// HTTPMetrics holds the per-route request metrics required by
// docs/plan-v1.0.md §15.
type HTTPMetrics struct {
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
}

// NewHTTPMetrics registers http_request_duration_seconds and
// http_requests_in_flight.
func NewHTTPMetrics(reg prometheus.Registerer) *HTTPMetrics {
	m := &HTTPMetrics{
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request latency by route pattern, method and status.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"route", "method", "status"}),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "http_requests_in_flight",
			Help: "HTTP requests currently being served.",
		}),
	}
	reg.MustRegister(m.duration, m.inFlight)
	return m
}
