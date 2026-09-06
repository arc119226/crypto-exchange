package ledger

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics are the ledger's Prometheus instruments (docs/plan-v1.0.md §15).
type Metrics struct {
	trialDiff *prometheus.GaugeVec
	entries   *prometheus.CounterVec
}

// NewMetrics registers the ledger metrics on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		trialDiff: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ledger_trial_balance_diff",
			Help: "Σdebit − Σcredit per asset over all postings; anything but 0 is an alert.",
		}, []string{"asset"}),
		entries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ledger_entries_total",
			Help: "Journal entries posted, by kind.",
		}, []string{"kind"}),
	}
	reg.MustRegister(m.trialDiff, m.entries)
	return m
}

// ObserveTrialBalance refreshes ledger_trial_balance_diff from the database.
// The gauge is an approximation for Prometheus (float64) of an exact
// decimal; the exact figure is the admin API's trial balance.
func (s *Service) ObserveTrialBalance(ctx context.Context) error {
	if s.metrics == nil {
		return nil
	}
	lines, err := s.TrialBalance(ctx)
	if err != nil {
		return err
	}
	for _, l := range lines {
		v := 0.0
		if !l.Diff.IsZero() {
			// exact zero is what matters; the magnitude only needs to be visible
			v = 1
			if l.Diff.IsNegative() {
				v = -1
			}
		}
		s.metrics.trialDiff.WithLabelValues(l.Asset).Set(v)
	}
	return nil
}
