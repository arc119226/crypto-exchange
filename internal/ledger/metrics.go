package ledger

import (
	"context"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/arc119226/crypto-exchange/internal/ledger/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// Metrics are the ledger's Prometheus instruments (docs/plan-v1.0.md §15).
type Metrics struct {
	trialDiff  *prometheus.GaugeVec
	entries    *prometheus.CounterVec
	feeRevenue *prometheus.CounterVec
	gasExpense *prometheus.CounterVec
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
		feeRevenue: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ledger_fee_revenue_total",
			Help: "Fee revenue credited to fee_revenue, by asset and where the fee came from.",
		}, []string{"asset", "source"}),
		gasExpense: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ledger_gas_expense_total",
			Help: "Gas debited to gas_expense, by asset -- always the chain's native coin.",
		}, []string{"asset"}),
	}
	reg.MustRegister(m.trialDiff, m.entries, m.feeRevenue, m.gasExpense)
	return m
}

// observeFees records what an entry moved through the two house accounts the
// revenue report reads (docs/plan-v1.0.md 23.5): credits to fee_revenue and
// debits to gas_expense. accounts are the rows GetAccounts returned for this
// entry's postings, which is where house_code lives -- a Posting carries only
// an account id.
//
// The counters carry the amount rather than a count, because 15 defines the
// net as the two subtracted within one asset and that only works if they are
// money. A float64 cannot hold eighteen decimal places, so these are an
// approximation for an operator watching a trend, and accumulating them
// compounds that: the exact figures are GET /admin/v1/reports/revenue, which
// is the number to quote. The counters exist to alert on and to plot.
func (m *Metrics) observeFees(e Entry, accounts []sqlcgen.LedgerAccount) {
	source := feeSource(e.RefType)
	for _, p := range e.Postings {
		// No house posting means no fee and no gas, which is every entry at
		// the shipped rates: the loop ends here and no series is created.
		if p.Bucket != BucketHouse {
			continue
		}
		switch code := houseCodeOf(accounts, p.AccountID); {
		case code == HouseFeeRevenue && p.Direction == Credit:
			addAmount(m.feeRevenue.WithLabelValues(p.Asset, source), p.Amount)
		case code == HouseGasExpense && p.Direction == Debit:
			addAmount(m.gasExpense.WithLabelValues(p.Asset), p.Amount)
		}
	}
}

// feeSource says where a fee came from. The entry kind cannot answer it: a
// trading fee rides inside a settle entry, a withdrawal fee is an entry of its
// own, and a deposit fee is a third posting inside the deposit's credit. The
// reference type is the one field that distinguishes all three.
//
// 15 documents source as trade|withdrawal|deposit. Anything else becomes
// "other" rather than being dropped, so revenue credited by a path nobody
// taught this about still shows up somewhere. The literals are spelled out
// because the depguard rules forbid internal/ledger importing internal/chain.
func feeSource(refType string) string {
	switch refType {
	case "trade", "withdrawal", "deposit":
		return refType
	default:
		return "other"
	}
}

// houseCodeOf finds a posting's house code among the entry's account rows. A
// linear scan over a handful of rows, deliberately without building a map:
// this runs inside Finish, which is on the engine's measured hot path.
func houseCodeOf(rows []sqlcgen.LedgerAccount, id string) HouseCode {
	for _, r := range rows {
		if r.ID == id && r.HouseCode != nil {
			return HouseCode(*r.HouseCode)
		}
	}
	return ""
}

// addAmount is the one float conversion on this path, for the reason
// reconcile's hot-wallet gauge gives: an operator comparing quantities can
// live without the eighteenth decimal place, and a metric that cannot carry a
// magnitude cannot be alerted on. A malformed amount is dropped rather than
// panicking -- Entry.Validate has already rejected those.
func addAmount(c prometheus.Counter, a money.Amount) {
	v, err := strconv.ParseFloat(a.String(), 64) //nolint:forbidigo // a Prometheus counter, not money
	if err != nil {
		return
	}
	c.Add(v)
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
