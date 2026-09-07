package sweep

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the sweeper's Prometheus collectors (docs/plan-v1.0.md §15).
//
// The one worth an alert is failed: a sweep that keeps failing means deposits
// are piling up on addresses the hot wallet cannot spend from, and the first
// visible symptom would be a withdrawal that cannot be paid.
type Metrics struct {
	planned    *prometheus.CounterVec
	confirmed  *prometheus.CounterVec
	failed     *prometheus.CounterVec
	unreadable *prometheus.CounterVec
	feeCeiling *prometheus.CounterVec
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
		unreadable: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sweeps_unreadable_total",
			Help: "Ticks in which an asset's on-chain balance could not be read, so it was not collected. " +
				"Alert on this rising steadily: it means a registry row names a contract that answers nothing, " +
				"and deposits in that asset are accumulating where the hot wallet cannot spend them.",
		}, []string{"asset"}),
		// Distinct from failed: nothing went wrong, an operator's ceiling said
		// the gas was not worth it. Sustained non-zero here with a falling
		// hot-wallet balance means the ceiling is set below the market.
		feeCeiling: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sweeps_fee_ceiling_waits_total",
			Help: "Times collection was deferred because fees exceeded ETH_MAX_FEE_PER_GAS.",
		}, []string{"asset"}),
	}
	if reg != nil {
		reg.MustRegister(m.planned, m.confirmed, m.failed, m.unreadable, m.feeCeiling)
	}
	return m
}
