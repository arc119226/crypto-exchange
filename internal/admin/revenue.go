package admin

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/admin/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// RevenueLine is one asset's row of the revenue report (docs/plan-v1.0.md
// 23.5). Every amount is in the asset named by Asset, and Net is the
// subtraction within that asset -- there is no conversion between assets and
// deliberately so: converting needs a price source, which v1.1 does not have.
//
// The consequence is worth stating where an operator will read it: gas is paid
// in the chain's native coin whatever was withdrawn, so a USDC row shows fee
// revenue with no gas against it and the ETH row carries the gas for every
// withdrawal on the chain. Neither row alone says whether withdrawals are
// profitable.
type RevenueLine struct {
	Asset string
	// MakerFees and TakerFees are summed from trading.trades rather than from
	// the ledger: a settle entry credits fee_revenue with these same two
	// numbers, so counting both would double them, and only the trade row
	// knows which side was the maker.
	MakerFees money.Amount
	TakerFees money.Amount
	// WithdrawalFees is fee_revenue inside entries of kind "fee", DepositFees
	// inside kind "deposit".
	WithdrawalFees money.Amount
	DepositFees    money.Amount
	// OtherFees is fee_revenue credited by anything else -- an operator
	// adjustment posted against the account, or a fee source added later that
	// nobody taught this report about. It is zero in normal operation, and it
	// exists so that revenue can never be silently missing from the total.
	OtherFees money.Amount
	// GasExpense is every gas_expense debit in the period, which includes the
	// gas spent on sweeps and nonce fills as well as on withdrawals (23.5
	// defines it that way: it is what the exchange paid the chain, whatever
	// the reason).
	GasExpense money.Amount
	// Net is fees minus gas within this asset.
	Net money.Amount
	// Trades counts the trades charged in this asset, including those charged
	// zero -- which is every trade until an operator sets a rate. Withdrawals
	// and Deposits count only fee-BEARING events, because a zero fee posts
	// nothing at all: at the shipped rates they are zero while trades is not.
	// Three counts rather than one: 23.3 prices the withdrawal fee against gas
	// per withdrawal, and that division needs the withdrawal count by itself.
	Trades      int64
	Withdrawals int64
	Deposits    int64
}

// RevenuePeriod is the half-open window [From, To) a report covers.
type RevenuePeriod struct {
	From time.Time
	To   time.Time
}

// DefaultRevenueWindow is how far back a report reaches when the caller names
// no period. Thirty days is long enough that a stack which withdraws rarely
// still shows something, and short enough to stay a report about now.
const DefaultRevenueWindow = 30 * 24 * time.Hour

// Revenue reads the report for a period, optionally narrowed to one asset.
//
// An asset appears in the result if it appears in any of the three sources, so
// a row can have gas and no revenue (the native coin on a stack with the fee
// rates at zero) or revenue and no gas (an ERC-20). Rows are ordered by asset.
func (h *Handler) Revenue(ctx context.Context, p RevenuePeriod, asset string) ([]RevenueLine, error) {
	q := sqlcgen.New(h.pool)
	lines := map[string]*RevenueLine{}
	line := func(a string) *RevenueLine {
		if l, ok := lines[a]; ok {
			return l
		}
		l := &RevenueLine{
			Asset: a, MakerFees: money.Zero, TakerFees: money.Zero,
			WithdrawalFees: money.Zero, DepositFees: money.Zero, OtherFees: money.Zero,
			GasExpense: money.Zero, Net: money.Zero,
		}
		lines[a] = l
		return l
	}

	trades, err := q.RevenueTradingFees(ctx, sqlcgen.RevenueTradingFeesParams{
		TenantID: h.tenant, FromTime: p.From, ToTime: p.To, Asset: asset,
	})
	if err != nil {
		return nil, fmt.Errorf("admin: revenue trading fees: %w", err)
	}
	for _, r := range trades {
		maker, err := pg.AmountFromNumeric(r.MakerFees)
		if err != nil {
			return nil, fmt.Errorf("admin: maker fees of %s: %w", r.Asset, err)
		}
		taker, err := pg.AmountFromNumeric(r.TakerFees)
		if err != nil {
			return nil, fmt.Errorf("admin: taker fees of %s: %w", r.Asset, err)
		}
		l := line(r.Asset)
		l.MakerFees, l.TakerFees, l.Trades = maker, taker, r.Trades
	}

	fees, err := q.RevenueFeeRevenue(ctx, sqlcgen.RevenueFeeRevenueParams{
		TenantID: h.tenant, FromTime: p.From, ToTime: p.To, Asset: asset,
	})
	if err != nil {
		return nil, fmt.Errorf("admin: revenue fee revenue: %w", err)
	}
	for _, r := range fees {
		amount, err := pg.AmountFromNumeric(r.Net)
		if err != nil {
			return nil, fmt.Errorf("admin: %s fees of %s: %w", r.Kind, r.Asset, err)
		}
		l := line(r.Asset)
		switch r.Kind {
		case ledger.KindFee:
			l.WithdrawalFees, l.Withdrawals = l.WithdrawalFees.Add(amount), l.Withdrawals+r.Entries
		case ledger.KindDeposit:
			l.DepositFees, l.Deposits = l.DepositFees.Add(amount), l.Deposits+r.Entries
		default:
			l.OtherFees = l.OtherFees.Add(amount)
		}
	}

	gas, err := q.RevenueGasExpense(ctx, sqlcgen.RevenueGasExpenseParams{
		TenantID: h.tenant, FromTime: p.From, ToTime: p.To, Asset: asset,
	})
	if err != nil {
		return nil, fmt.Errorf("admin: revenue gas expense: %w", err)
	}
	for _, r := range gas {
		amount, err := pg.AmountFromNumeric(r.Net)
		if err != nil {
			return nil, fmt.Errorf("admin: gas expense of %s: %w", r.Asset, err)
		}
		line(r.Asset).GasExpense = amount
	}

	out := make([]RevenueLine, 0, len(lines))
	for _, l := range lines {
		l.Net = l.MakerFees.Add(l.TakerFees).Add(l.WithdrawalFees).Add(l.DepositFees).Add(l.OtherFees).Sub(l.GasExpense)
		out = append(out, *l)
	}
	// By asset, so the report is stable enough to diff between runs.
	slices.SortFunc(out, func(a, b RevenueLine) int { return strings.Compare(a.Asset, b.Asset) })
	return out, nil
}

// revenuePeriod resolves the optional bounds a caller gave into the half-open
// window the query takes. Both default rather than erroring: a revenue report
// with no arguments should show the operator something, and the last 30 days
// ending now is the window an operator checking in would have asked for.
//
// It refuses an inverted or empty window instead of quietly returning nothing,
// because a report of zero and a report over a nonsense period look identical
// once they reach a screen.
func revenuePeriod(from, to *time.Time, now time.Time) (RevenuePeriod, error) {
	p := RevenuePeriod{To: now}
	if to != nil {
		p.To = *to
	}
	p.From = p.To.Add(-DefaultRevenueWindow)
	if from != nil {
		p.From = *from
	}
	if !p.From.Before(p.To) {
		return RevenuePeriod{}, fmt.Errorf("from %s is not before to %s",
			p.From.Format(time.RFC3339), p.To.Format(time.RFC3339))
	}
	return p, nil
}

// revenueCSVHeader names the columns in the order writeRevenueCSV writes them.
// It is the CSV's contract: an operator's spreadsheet reads by position.
var revenueCSVHeader = []string{
	"asset", "maker_fees", "taker_fees", "withdrawal_fees", "deposit_fees",
	"other_fees", "gas_expense", "net", "trades", "withdrawals", "deposits",
}

// writeRevenueCSV renders the report, amounts as the same decimal strings the
// JSON carries. Shared by the spec'd .csv operation and the back office's own
// download route, which cannot use the spec'd one: /admin/v1 is behind the
// API key header, and a browser following a link sends no header.
func writeRevenueCSV(w io.Writer, lines []RevenueLine) error {
	c := csv.NewWriter(w)
	if err := c.Write(revenueCSVHeader); err != nil {
		return err
	}
	for _, l := range lines {
		if err := c.Write([]string{
			l.Asset, l.MakerFees.String(), l.TakerFees.String(), l.WithdrawalFees.String(),
			l.DepositFees.String(), l.OtherFees.String(), l.GasExpense.String(), l.Net.String(),
			strconv.FormatInt(l.Trades, 10), strconv.FormatInt(l.Withdrawals, 10),
			strconv.FormatInt(l.Deposits, 10),
		}); err != nil {
			return err
		}
	}
	c.Flush()
	return c.Error()
}

// GetRevenueReport implements GET /admin/v1/reports/revenue.
func (h *Handler) GetRevenueReport(ctx context.Context, req gen.GetRevenueReportRequestObject) (gen.GetRevenueReportResponseObject, error) {
	const instance = "/admin/v1/reports/revenue"
	lines, period, err := h.revenueFor(ctx, req.Params.From, req.Params.To, req.Params.Asset)
	switch {
	case errors.Is(err, errBadPeriod):
		return gen.GetRevenueReport400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error()),
		}, nil
	case err != nil:
		return nil, err
	}
	out := make([]gen.RevenueLine, 0, len(lines))
	for _, l := range lines {
		out = append(out, toRevenueLine(l))
	}
	return gen.GetRevenueReport200JSONResponse(gen.RevenueReport{
		From: period.From, To: period.To, Lines: out,
	}), nil
}

// GetRevenueReportCsv implements GET /admin/v1/reports/revenue.csv.
func (h *Handler) GetRevenueReportCsv(ctx context.Context, req gen.GetRevenueReportCsvRequestObject) (gen.GetRevenueReportCsvResponseObject, error) {
	const instance = "/admin/v1/reports/revenue.csv"
	lines, _, err := h.revenueFor(ctx, req.Params.From, req.Params.To, req.Params.Asset)
	switch {
	case errors.Is(err, errBadPeriod):
		return gen.GetRevenueReportCsv400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error()),
		}, nil
	case err != nil:
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeRevenueCSV(&buf, lines); err != nil {
		return nil, fmt.Errorf("admin: render revenue csv: %w", err)
	}
	return gen.GetRevenueReportCsv200TextCsvResponse{Body: &buf, ContentLength: int64(buf.Len())}, nil
}

// errBadPeriod separates the caller's mistake from ours, so the handler can
// answer 400 rather than 500.
var errBadPeriod = errors.New("admin: bad revenue period")

// revenueFor is the half both handlers share: resolve the period, read the
// report. now() is read once so the two default bounds cannot straddle a tick.
func (h *Handler) revenueFor(ctx context.Context, from, to *time.Time, asset *string) ([]RevenueLine, RevenuePeriod, error) {
	period, err := revenuePeriod(from, to, time.Now().UTC())
	if err != nil {
		return nil, RevenuePeriod{}, fmt.Errorf("%w: %s", errBadPeriod, err)
	}
	lines, err := h.Revenue(ctx, period, deref(asset))
	if err != nil {
		return nil, RevenuePeriod{}, err
	}
	return lines, period, nil
}

func toRevenueLine(l RevenueLine) gen.RevenueLine {
	return gen.RevenueLine{
		Asset: l.Asset, MakerFees: l.MakerFees, TakerFees: l.TakerFees,
		WithdrawalFees: l.WithdrawalFees, DepositFees: l.DepositFees, OtherFees: l.OtherFees,
		GasExpense: l.GasExpense, Net: l.Net,
		Trades: l.Trades, Withdrawals: l.Withdrawals, Deposits: l.Deposits,
	}
}
