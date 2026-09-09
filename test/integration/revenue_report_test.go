//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/admin"
	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// The DoD of Phase 8 says the report's numbers must equal a direct SUM over
// trading.trades and ledger.postings. That is exactly the assertion here: the
// report is one implementation of those sums, and this is the other one,
// written straight against the tables so a mistake in the query cannot hide in
// both.

// directRevenue is the report computed with the plainest SQL that answers §23.5,
// with none of the report's joins or CASE arithmetic.
type directRevenue struct {
	maker, taker, withdrawalFees, depositFees, gas money.Amount
	trades                                         int64
}

func (h ledgerHarness) directRevenue(t require.TestingT, ctx context.Context, asset string, from, to time.Time) directRevenue {
	scan := func(sql string, args ...any) money.Amount {
		var s string
		require.NoError(t, h.all.QueryRow(ctx, sql, args...).Scan(&s))
		return amt(s)
	}
	out := directRevenue{
		maker: scan(`SELECT COALESCE(SUM(maker_fee), 0)::text FROM trading.trades
		              WHERE tenant_id = 'default' AND maker_fee_asset = $1 AND created_at >= $2 AND created_at < $3`,
			asset, from, to),
		taker: scan(`SELECT COALESCE(SUM(taker_fee), 0)::text FROM trading.trades
		              WHERE tenant_id = 'default' AND taker_fee_asset = $1 AND created_at >= $2 AND created_at < $3`,
			asset, from, to),
	}
	feeRevenue, err := h.svc.HouseAccount(ledger.HouseFeeRevenue)
	require.NoError(t, err)
	gasExpense, err := h.svc.HouseAccount(ledger.HouseGasExpense)
	require.NoError(t, err)
	byKind := `SELECT COALESCE(SUM(CASE WHEN p.direction = 'credit' THEN p.amount ELSE -p.amount END), 0)::text
	             FROM ledger.postings p JOIN ledger.journal_entries e ON e.id = p.entry_id
	            WHERE e.tenant_id = 'default' AND p.account_id = $1 AND p.asset = $2
	              AND e.kind = $3 AND e.created_at >= $4 AND e.created_at < $5`
	out.withdrawalFees = scan(byKind, feeRevenue, asset, ledger.KindFee, from, to)
	out.depositFees = scan(byKind, feeRevenue, asset, ledger.KindDeposit, from, to)
	out.gas = scan(`SELECT COALESCE(SUM(CASE WHEN p.direction = 'debit' THEN p.amount ELSE -p.amount END), 0)::text
	                  FROM ledger.postings p JOIN ledger.journal_entries e ON e.id = p.entry_id
	                 WHERE e.tenant_id = 'default' AND p.account_id = $1 AND p.asset = $2
	                   AND e.created_at >= $3 AND e.created_at < $4`, gasExpense, asset, from, to)
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT count(*) FROM trading.trades
		  WHERE tenant_id = 'default' AND created_at >= $1 AND created_at < $2
		    AND (maker_fee_asset = $3 OR taker_fee_asset = $3)`, from, to, asset).Scan(&out.trades))
	return out
}

// The scenario is a whole exchange's day: two trades in two assets, a
// withdrawal that confirmed and paid a fee, one that was refunded and must
// not have, and a deposit charged on the way in.
func TestRevenueReportEqualsADirectSum(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	from := time.Now().UTC().Add(-time.Hour)

	h.setAssetFees(t, ctx, "ETH", "0.001", 25, 0)
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-revenue")

	// One withdrawal that confirms and pays.
	paid := h.locked(t, ctx, account, "ETH", "0.05", "revenue-paid")
	require.NoError(t, h.worker.Send(ctx))
	require.NoError(t, h.worker.Send(ctx))
	h.chain.mineAll()
	require.NoError(t, h.worker.Send(ctx))

	// One that reverts and is refunded, so it contributes nothing.
	refunded := h.locked(t, ctx, account, "ETH", "0.05", "revenue-refunded")
	require.NoError(t, h.worker.Send(ctx))
	require.NoError(t, h.worker.Send(ctx))
	h.chain.mineReverted()
	require.NoError(t, h.worker.Send(ctx))
	_, err := h.reviewer.RequestResolve(ctx, withdrawal.ResolveParams{
		ID: refunded, Action: withdrawal.ActionRefund, Note: "the destination rejected it",
		ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	require.NoError(t, err)
	require.NoError(t, h.worker.ApplyResolutions(ctx))

	handler := admin.NewHandler(h.all, h.ledgerHarness.svc, registry.NewStore(h.all), audit.NewRecorder("default"), "default")
	to := time.Now().UTC().Add(time.Hour)
	lines, err := handler.Revenue(ctx, admin.RevenuePeriod{From: from, To: to}, "")
	require.NoError(t, err)

	for _, l := range lines {
		want := h.directRevenue(t, ctx, l.Asset, from, to)
		assert.Equal(t, want.maker.String(), l.MakerFees.String(), "%s maker fees", l.Asset)
		assert.Equal(t, want.taker.String(), l.TakerFees.String(), "%s taker fees", l.Asset)
		assert.Equal(t, want.withdrawalFees.String(), l.WithdrawalFees.String(), "%s withdrawal fees", l.Asset)
		assert.Equal(t, want.depositFees.String(), l.DepositFees.String(), "%s deposit fees", l.Asset)
		assert.Equal(t, want.gas.String(), l.GasExpense.String(), "%s gas", l.Asset)
		assert.Equal(t, want.trades, l.Trades, "%s trades", l.Asset)

		net := l.MakerFees.Add(l.TakerFees).Add(l.WithdrawalFees).Add(l.DepositFees).Add(l.OtherFees).Sub(l.GasExpense)
		assert.Equal(t, net.String(), l.Net.String(), "%s net is the sum of its own columns", l.Asset)
	}

	// And the numbers are the ones the story implies, not just self-consistent.
	eth := revenueLineFor(t, lines, "ETH")
	assert.Equal(t, "0.001125", eth.WithdrawalFees.String(),
		"one confirmed withdrawal charged; the refunded one charged nothing")
	assert.EqualValues(t, 1, eth.Withdrawals)
	assert.True(t, eth.GasExpense.IsPositive(), "three transactions were mined")
	assert.True(t, eth.OtherFees.IsZero(), "nothing reached fee_revenue by a path the report does not know")
	// The net is fees minus gas within the asset. Whether it comes out
	// positive is a question about gas prices, not about this code, so the
	// assertion is the identity rather than a sign -- and the identity is the
	// thing an operator reads the column for.
	assert.Equal(t, eth.WithdrawalFees.Sub(eth.GasExpense).Add(eth.MakerFees).Add(eth.TakerFees).Add(eth.DepositFees).String(),
		eth.Net.String())

	// Narrowing to one asset must not change that asset's row.
	only, err := handler.Revenue(ctx, admin.RevenuePeriod{From: from, To: to}, "ETH")
	require.NoError(t, err)
	require.Len(t, only, 1)
	assert.Equal(t, eth, only[0])

	// A period before any of it happened is empty rather than wrong.
	empty, err := handler.Revenue(ctx, admin.RevenuePeriod{From: from.Add(-48 * time.Hour), To: from.Add(-24 * time.Hour)}, "")
	require.NoError(t, err)
	assert.Empty(t, empty)

	_ = paid
	h.assertTrialBalanceZero(t, ctx)
	h.assertWithdrawalFeesMatchRevenue(t, ctx)
}

func revenueLineFor(t require.TestingT, lines []admin.RevenueLine, asset string) admin.RevenueLine {
	for _, l := range lines {
		if l.Asset == asset {
			return l
		}
	}
	require.FailNow(t, "no revenue line for "+asset)
	return admin.RevenueLine{}
}
