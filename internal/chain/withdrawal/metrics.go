package withdrawal

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the withdrawal worker's Prometheus collectors
// (docs/plan-v1.0.md §15). The one worth an alert is the review queue: a
// withdrawal waiting for a person is a user waiting for their money, and
// nothing else in the system will notice.
type Metrics struct {
	decided *prometheus.CounterVec
	pending prometheus.Gauge
}

// NewMetrics registers the collectors. A nil registerer returns unregistered
// collectors, which keeps tests from needing a registry.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		decided: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "withdrawals_transitions_total",
			Help: "Withdrawal state transitions the chain worker made, by asset and resulting status.",
		}, []string{"asset", "status"}),
		pending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "withdrawals_pending_review",
			Help: "Withdrawals waiting for an administrator to approve or reject them.",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.decided, m.pending)
	}
	return m
}

// observePending is where an int becomes a float64. Prometheus has no other
// numeric type, so the conversion is confined to this file rather than spread
// through the worker (the same shape internal/chain/deposit/metrics.go uses).
func (m *Metrics) observePending(n int) {
	m.pending.Set(float64(n)) //nolint:forbidigo // Prometheus gauge, not money
}
