package deposit

import "github.com/prometheus/client_golang/prometheus"

// Metrics are the scanner's Prometheus collectors (docs/plan-v1.0.md §15).
// chain_scanner_lag_blocks is the one with an alert on it: a scanner that
// falls behind is not crediting anyone.
type Metrics struct {
	head        prometheus.Gauge
	lastScanned prometheus.Gauge
	lag         prometheus.Gauge
	watched     prometheus.Gauge
	reorgs      prometheus.Counter
	unreadable  prometheus.Counter
	credited    *prometheus.CounterVec
}

// NewMetrics registers the collectors. A nil registerer returns unregistered
// collectors, which keeps tests from needing a registry.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		head: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "chain_head_block", Help: "Latest block the node reports.",
		}),
		lastScanned: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "chain_last_scanned_block", Help: "Latest block the scanner has processed.",
		}),
		lag: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "chain_scanner_lag_blocks", Help: "Blocks between the head and the scanner.",
		}),
		watched: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "chain_watched_addresses", Help: "Assigned deposit addresses the scanner matches against.",
		}),
		reorgs: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "chain_reorgs_total", Help: "Reorgs the scanner has rewound through.",
		}),
		unreadable: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "chain_unreadable_transfers_total",
			Help: "Transfers seen but not credited because they could not be decoded or represented.",
		}),
		credited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "deposits_credited_total", Help: "Deposits credited to an account.",
		}, []string{"asset"}),
	}
	if reg != nil {
		reg.MustRegister(m.head, m.lastScanned, m.lag, m.watched, m.reorgs, m.unreadable, m.credited)
	}
	return m
}

// The observers below are where uint64 becomes float64. Prometheus has no
// other numeric type, so the conversion is confined to this file rather than
// spread through the scanner (the same shape internal/trading/metrics.go uses).

func (m *Metrics) observeHead(block uint64) {
	m.head.Set(float64(block)) //nolint:forbidigo // Prometheus gauge, not money
}

func (m *Metrics) observeScanned(block, lag uint64) {
	m.lastScanned.Set(float64(block)) //nolint:forbidigo // Prometheus gauge, not money
	m.lag.Set(float64(lag))           //nolint:forbidigo // Prometheus gauge, not money
}

func (m *Metrics) observeWatched(n int) {
	m.watched.Set(float64(n)) //nolint:forbidigo // Prometheus gauge, not money
}
