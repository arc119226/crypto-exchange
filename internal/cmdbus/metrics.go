package cmdbus

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics instrument both ends of the bus (docs/plan-v1.0.md §15). A nil
// *Metrics is valid and records nothing, so tests and the in-process bus do
// not need a registry.
type Metrics struct {
	clientRequests *prometheus.CounterVec
	clientLatency  *prometheus.HistogramVec
	serverRequests *prometheus.CounterVec
	serverLatency  *prometheus.HistogramVec
}

// NewMetrics registers the bus instruments on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		clientRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "trading_command_bus_client_requests_total",
			Help: "Commands sent to the engine over NATS, by op and outcome.",
		}, []string{"op", "status"}),
		clientLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "trading_command_bus_client_latency_seconds",
			Help:    "Round trip of one command from the api role, including the engine's database transaction.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"op"}),
		serverRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "trading_command_bus_requests_total",
			Help: "Commands answered by the engine, by op and outcome.",
		}, []string{"op", "status"}),
		serverLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "trading_command_bus_duration_seconds",
			Help:    "Time the engine took to answer one command.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		}, []string{"op"}),
	}
	reg.MustRegister(m.clientRequests, m.clientLatency, m.serverRequests, m.serverLatency)
	return m
}

func (m *Metrics) observeClient(op, status string, d time.Duration) {
	if m == nil {
		return
	}
	m.clientRequests.WithLabelValues(op, status).Inc()
	m.clientLatency.WithLabelValues(op).Observe(d.Seconds())
}

func (m *Metrics) observeServer(op, status string, d time.Duration) {
	if m == nil || op == "" {
		return
	}
	m.serverRequests.WithLabelValues(op, status).Inc()
	m.serverLatency.WithLabelValues(op).Observe(d.Seconds())
}
