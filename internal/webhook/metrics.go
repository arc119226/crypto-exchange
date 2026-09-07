package webhook

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the counters the worker exports (docs/plan-v1.0.md §15).
type Metrics struct {
	queued   *prometheus.CounterVec
	attempts *prometheus.CounterVec
}

// NewMetrics registers the collectors. A nil registerer returns unregistered
// collectors, which keeps tests from needing a registry.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		queued: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "webhook_queued_total",
			Help: "Events accepted for delivery to an endpoint, by event type.",
		}, []string{"event_type"}),
		attempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "webhook_attempts_total",
			// status carries dead, which is the one an alert should watch:
			// past it the customer never receives the event.
			Help: "Delivery attempts by event type and outcome.",
		}, []string{"event_type", "status"}),
	}
	if reg != nil {
		reg.MustRegister(m.queued, m.attempts)
	}
	return m
}
