//go:build integration

package integration

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/chain/deposit"
	"github.com/arc119226/crypto-exchange/internal/ledger"
)

// The deposit half of §6.1.4(i). Unlike a withdrawal fee this one comes out of
// what arrived, which makes one thing worth pinning above all: the custody
// account is debited the FULL amount the chain delivered, because the sweep
// cap and reconciliation both sum chain.deposits.amount. Debiting the credited
// amount instead would balance the entry and quietly break both.

// depositRows reads what the API and the back office see, including the two
// nullable fee columns.
func (h scriptedHarness) depositRows(t *testing.T, ctx context.Context, account string) []deposit.Record {
	t.Helper()
	recs, err := deposit.NewReader(h.all, "default").ByAccount(ctx, account, 10, 0)
	require.NoError(t, err)
	return recs
}

// assertDepositFeesMatchRevenue is the deposit counterpart of the withdrawal
// invariant: fee_revenue holds exactly the fees of the deposits that were
// credited, and nothing from one that was not.
func (h ledgerHarness) assertDepositFeesMatchRevenue(t require.TestingT, ctx context.Context) {
	feeRevenue, err := h.svc.HouseAccount(ledger.HouseFeeRevenue)
	require.NoError(t, err)
	rows, err := h.all.Query(ctx,
		`SELECT d.id, d.status, COALESCE(d.fee, 0)::text,
		        COALESCE(SUM(p.amount) FILTER (WHERE p.direction = 'credit'), 0)::text
		   FROM chain.deposits d
		   LEFT JOIN ledger.journal_entries e
		          ON e.tenant_id = d.tenant_id AND e.ref_type = 'deposit' AND e.ref_id = d.id
		   LEFT JOIN ledger.postings p
		          ON p.entry_id = e.id AND p.account_id = $1
		  GROUP BY d.id, d.status, d.fee`, feeRevenue)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id, status, fee, credited string
		require.NoError(t, rows.Scan(&id, &status, &fee, &credited))
		want := "0"
		if status == deposit.StatusCredited {
			want = fee
		}
		assert.Equal(t, amt(want).String(), amt(credited).String(),
			"deposit %s is %s, so fee_revenue should hold %s of its fee", id, status, want)
	}
	require.NoError(t, rows.Err())
}

// One arriving ether at 25 bps: the user is credited 0.9975, the exchange
// keeps 0.0025, and custody is charged the whole 1.
func TestDepositFeeComesOutOfWhatArrived(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: 1})
	ctx := context.Background()
	h.setAssetFees(t, ctx, "ETH", "0", 0, 25)
	account, address := h.account(t, ctx)

	h.fake.mine("b1", transfer(0, address, oneETH()))
	require.NoError(t, h.scanner.Tick(ctx))

	require.Equal(t, deposit.StatusCredited, h.status(t, ctx, account))
	assert.Equal(t, "0.9975", h.available(t, ctx, account, "ETH").String())
	assert.Equal(t, "0.0025", h.houseBalance(t, ctx, "fee_revenue", "ETH").String())
	assert.Equal(t, "1", h.houseBalance(t, ctx, "custody_deposit_addresses", "ETH").String(),
		"custody holds what the chain delivered, not what the user got: the sweep cap and reconciliation both sum the amount")

	recs := h.depositRows(t, ctx, account)
	require.Len(t, recs, 1)
	assert.Equal(t, "1", recs[0].Amount.String(), "amount stays what arrived")
	require.NotNil(t, recs[0].Fee)
	require.NotNil(t, recs[0].Credited)
	assert.Equal(t, "0.0025", recs[0].Fee.String())
	assert.Equal(t, "0.9975", recs[0].Credited.String())

	h.assertTrialBalanceZero(t, ctx)
	h.assertCacheMatchesPostings(t, ctx, account)
	h.assertDepositFeesMatchRevenue(t, ctx)
}

// The seeded rate is zero and the plan expects it to stay there, so this is
// the path every real deposit takes. It must be the two postings it always
// was -- not two postings and a zero one.
func TestDepositFeeAtZeroCreditsInFull(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: 1})
	ctx := context.Background()
	account, address := h.account(t, ctx)

	h.fake.mine("b1", transfer(0, address, oneETH()))
	require.NoError(t, h.scanner.Tick(ctx))

	assert.Equal(t, "1", h.available(t, ctx, account, "ETH").String())
	assert.Equal(t, "0", h.houseBalance(t, ctx, "fee_revenue", "ETH").String())
	recs := h.depositRows(t, ctx, account)
	require.Len(t, recs, 1)
	require.NotNil(t, recs[0].Fee)
	assert.Equal(t, "0", recs[0].Fee.String(), "computed and zero, which is not the same as absent")
	assert.Equal(t, "1", recs[0].Credited.String())

	h.assertTrialBalanceZero(t, ctx)
	h.assertDepositFeesMatchRevenue(t, ctx)
}

// Rounding a fee up means a small enough deposit rounds to the whole thing.
// Crediting the user nothing and booking their deposit as revenue is not a
// fee, so the deposit is credited in full instead.
func TestDepositTooSmallToChargeIsCreditedInFull(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: 1})
	ctx := context.Background()
	h.setAssetFees(t, ctx, "ETH", "0", 0, 1)
	account, address := h.account(t, ctx)

	// One wei. A hundredth of a percent of it rounds up to one wei, which is
	// the whole deposit.
	h.fake.mine("b1", transfer(0, address, big.NewInt(1)))
	require.NoError(t, h.scanner.Tick(ctx))

	recs := h.depositRows(t, ctx, account)
	require.Len(t, recs, 1)
	require.NotNil(t, recs[0].Fee)
	assert.Equal(t, "0", recs[0].Fee.String(), "confiscation is refused, not rounded to")
	assert.Equal(t, recs[0].Amount.String(), recs[0].Credited.String())
	assert.Equal(t, "0", h.houseBalance(t, ctx, "fee_revenue", "ETH").String())

	h.assertTrialBalanceZero(t, ctx)
	h.assertDepositFeesMatchRevenue(t, ctx)
}

// A token has six decimals, not eighteen, and the fee has to land on that
// grid: a fee the asset cannot represent could not be posted.
func TestDepositFeeOnAToken(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: 1})
	ctx := context.Background()
	h.setAssetFees(t, ctx, "USDC", "0", 0, 25)
	account, address := h.account(t, ctx)
	token := common.HexToAddress(usdcContract)

	// 250.5 USDC at six decimals.
	h.fake.mineLogs("b1", erc20Log(token, common.HexToAddress("0x1"), address, big.NewInt(250_500_000), 0))
	require.NoError(t, h.scanner.Tick(ctx))

	require.Equal(t, deposit.StatusCredited, h.status(t, ctx, account))
	assert.Equal(t, "249.87375", h.available(t, ctx, account, "USDC").String())
	assert.Equal(t, "0.62625", h.houseBalance(t, ctx, "fee_revenue", "USDC").String())

	h.assertTrialBalanceZero(t, ctx)
	h.assertDepositFeesMatchRevenue(t, ctx)
}

// A deposit that never became money must never have been charged, and when the
// same transaction reappears on the winning branch it is charged exactly once.
func TestAnOrphanedDepositIsNeverCharged(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: 3, RingDepth: 16, OrphanExpiryBlocks: 8})
	ctx := context.Background()
	h.setAssetFees(t, ctx, "ETH", "0", 0, 25)
	account, address := h.account(t, ctx)

	h.fake.mine("a1")
	h.fake.mine("a2")
	require.NoError(t, h.scanner.Tick(ctx))

	tx := transfer(7, address, oneETH())
	h.fake.mine("a3-with-deposit", tx)
	require.NoError(t, h.scanner.Tick(ctx))
	require.Equal(t, deposit.StatusConfirming, h.status(t, ctx, account))

	h.fake.rewind(2)
	h.fake.mine("b3")
	h.fake.mine("b4")
	require.NoError(t, h.scanner.Tick(ctx))

	require.Equal(t, deposit.StatusOrphaned, h.status(t, ctx, account))
	recs := h.depositRows(t, ctx, account)
	require.Len(t, recs, 1)
	assert.Nil(t, recs[0].Fee, "the columns exist exactly when credited_at does")
	assert.Nil(t, recs[0].Credited)
	assert.Equal(t, "0", h.houseBalance(t, ctx, "fee_revenue", "ETH").String())
	h.assertDepositFeesMatchRevenue(t, ctx)

	t.Run("and charged once when it comes back", func(t *testing.T) {
		h.fake.mine("b5-with-the-same-deposit", tx)
		h.fake.mine("b6")
		h.fake.mine("b7")
		require.NoError(t, h.scanner.Tick(ctx))

		require.Equal(t, deposit.StatusCredited, h.status(t, ctx, account))
		assert.Equal(t, "0.9975", h.available(t, ctx, account, "ETH").String())
		assert.Equal(t, "0.0025", h.houseBalance(t, ctx, "fee_revenue", "ETH").String(),
			"one fee for one deposit, however many branches it was seen on")
		h.assertTrialBalanceZero(t, ctx)
		h.assertDepositFeesMatchRevenue(t, ctx)
	})
}

// A reorg deeper than the confirmations does NOT un-credit a deposit: the
// scanner leaves credited rows alone because the money may already have been
// spent, and undoing it is the manual `reversed` path. That path is declared
// -- the status is in the schema and deposit.reversed has a schema and a
// golden envelope -- but nothing in this tree writes it. This test pins what
// actually happens today so the day it is implemented is a visible change.
func TestACreditedDepositKeepsItsFeeThroughAReorg(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: 1, RingDepth: 16, OrphanExpiryBlocks: 8})
	ctx := context.Background()
	h.setAssetFees(t, ctx, "ETH", "0", 0, 25)
	account, address := h.account(t, ctx)

	h.fake.mine("a1")
	require.NoError(t, h.scanner.Tick(ctx))
	h.fake.mine("a2-with-deposit", transfer(0, address, oneETH()))
	require.NoError(t, h.scanner.Tick(ctx))
	require.Equal(t, deposit.StatusCredited, h.status(t, ctx, account))

	h.fake.rewind(1)
	h.fake.mine("b2")
	h.fake.mine("b3")
	require.NoError(t, h.scanner.Tick(ctx))

	assert.Equal(t, deposit.StatusCredited, h.status(t, ctx, account),
		"still credited: reversing a credited deposit is an operator's decision, and there is no code for it yet")
	recs := h.depositRows(t, ctx, account)
	require.Len(t, recs, 1)
	require.NotNil(t, recs[0].Fee)
	assert.Equal(t, "0.0025", recs[0].Fee.String())
	assert.Equal(t, "0.9975", h.available(t, ctx, account, "ETH").String())
	h.assertTrialBalanceZero(t, ctx)
	h.assertDepositFeesMatchRevenue(t, ctx)
}
