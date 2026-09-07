package reconcile

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// Metrics are the reconciler's Prometheus collectors (docs/plan-v1.0.md §15).
type Metrics struct {
	diff    *prometheus.GaugeVec
	breaks  prometheus.Gauge
	balance *prometheus.GaugeVec
}

// NewMetrics registers the collectors. A nil registerer returns unregistered
// collectors, which keeps tests from needing a registry.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		diff: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "reconciliation_diff",
			Help: "Sign of the unexplained difference between the ledger and the chain, by asset: -1 short, 0 balanced, 1 over.",
		}, []string{"asset"}),
		breaks: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "reconciliation_breaks",
			Help: "Assets whose last reconciliation did not balance.",
		}),
		balance: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "hot_wallet_balance",
			Help: "The hot wallet's on-chain balance, by asset.",
		}, []string{"asset"}),
	}
	if reg != nil {
		reg.MustRegister(m.diff, m.breaks, m.balance)
	}
	return m
}

// observe records one line's verdict.
//
// The sign rather than the magnitude, for the reason ledger.ObserveTrialBalance
// gives: the question an alert asks is "is it zero", and a float64 cannot carry
// eighteen decimal places without inventing digits. The figures themselves are
// in the report, where they are exact.
func (m *Metrics) observe(l Line) {
	m.diff.WithLabelValues(l.Asset).Set(float64(l.Diff.Sign())) //nolint:forbidigo // Prometheus gauge, not money
}

func (m *Metrics) observeBreaks(n int) {
	m.breaks.Set(float64(n)) //nolint:forbidigo // Prometheus gauge, not money
}

// observeHotWallet is the one gauge here that is a quantity rather than a
// verdict: an operator watching for "top the hot wallet up" wants the number,
// and being a little wrong in the eighteenth decimal place does not change
// what they do about it.
func (m *Metrics) observeHotWallet(asset string, balance money.Amount) {
	v, err := strconv.ParseFloat(balance.String(), 64) //nolint:forbidigo // Prometheus gauge, not money
	if err != nil {
		return
	}
	m.balance.WithLabelValues(asset).Set(v)
}
