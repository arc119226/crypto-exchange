package sweep

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the sweeper's Prometheus collectors (docs/plan-v1.0.md §15).
//
// The one worth an alert is failed: a sweep that keeps failing means deposits
// are piling up on addresses the hot wallet cannot spend from, and the first
// visible symptom would be a withdrawal that cannot be paid.
type Metrics struct {
	planned   *prometheus.CounterVec
	confirmed *prometheus.CounterVec
	failed    *prometheus.CounterVec
}

// NewMetrics registers the collectors. A nil registerer returns unregistered
// collectors, which keeps tests from needing a registry.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		planned: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sweeps_planned_total",
			Help: "Deposit addresses found worth emptying, by asset.",
		}, []string{"asset"}),
		confirmed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sweeps_confirmed_total",
			Help: "Sweeps that reached the hot wallet, by asset.",
		}, []string{"asset"}),
		failed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sweeps_failed_total",
			Help: "Sweeps that did not complete, by asset and reason.",
		}, []string{"asset", "reason"}),
	}
	if reg != nil {
		reg.MustRegister(m.planned, m.confirmed, m.failed)
	}
	return m
}
