//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/deposit"
	"github.com/arc119226/crypto-exchange/internal/ledger"
)

// The reversed path of §6.4.1, which until now existed only as a status in a
// CHECK constraint and an event nobody could emit.
//
// The shape under test is the split: the scanner marks, a person confirms, the
// chain role posts. Nothing here happens without all three, and the middle one
// is deliberately a human.

func (h scriptedHarness) reviewer() *deposit.Reviewer {
	return deposit.NewReviewer(h.all, "default", audit.NewRecorder("default"))
}

// creditedThenReorged drives one deposit to credited and then rewinds the
// chain past its block, which is the situation the whole path exists for.
func (h scriptedHarness) creditedThenReorged(t *testing.T, ctx context.Context) (string, deposit.Record) {
	t.Helper()
	account, address := h.account(t, ctx)

	h.fake.mine("a1")
	require.NoError(t, h.scanner.Tick(ctx))
	h.fake.mine("a2-with-deposit", transfer(0, address, oneETH()))
	require.NoError(t, h.scanner.Tick(ctx))
	require.Equal(t, deposit.StatusCredited, h.status(t, ctx, account))

	// A branch that does not contain it wins.
	h.fake.rewind(1)
	h.fake.mine("b2")
	h.fake.mine("b3")
	require.NoError(t, h.scanner.Tick(ctx))

	recs := h.depositRows(t, ctx, account)
	require.Len(t, recs, 1)
	return account, recs[0]
}

// The scanner marks and stops. It does not reverse, and it must not: between
// the credit and the reorg the account may have spent the money, and no
// unattended process should decide what to do about that.
func TestAReorgedCreditedDepositIsMarkedAndLeftForAPerson(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: 1, RingDepth: 16, OrphanExpiryBlocks: 8})
	ctx := context.Background()
	account, rec := h.creditedThenReorged(t, ctx)

	assert.Equal(t, deposit.StatusCredited, rec.Status, "still credited: the ledger has not been told otherwise")
	require.NotNil(t, rec.ReorgedAtBlock)
	assert.EqualValues(t, 1, *rec.ReorgedAtBlock, "the height the chain rewound to")
	assert.Equal(t, "1", h.available(t, ctx, account, "ETH").String(), "the account still holds it")

	queue, err := h.reviewer().AwaitingReversal(ctx, 50, 0)
	require.NoError(t, err)
	require.Len(t, queue, 1)
	assert.Equal(t, rec.ID, queue[0].ID)

	// Ticking again changes nothing and does not re-mark it at a shallower
	// height: the first reorg is the one that matters.
	require.NoError(t, h.scanner.Tick(ctx))
	again := h.depositRows(t, ctx, account)[0]
	assert.EqualValues(t, 1, *again.ReorgedAtBlock)
	h.assertTrialBalanceZero(t, ctx)
}

// Confirmed, the reversal is the exact mirror of the credit -- including the
// fee, which the exchange gives back because it charged for delivering money
// the chain took away.
func TestAConfirmedReversalMirrorsTheCreditIncludingTheFee(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: 1, RingDepth: 16, OrphanExpiryBlocks: 8})
	ctx := context.Background()
	h.setAssetFees(t, ctx, "ETH", "0", 0, 25)
	account, rec := h.creditedThenReorged(t, ctx)

	require.NotNil(t, rec.Fee)
	require.Equal(t, "0.0025", rec.Fee.String())
	require.Equal(t, "0.9975", h.available(t, ctx, account, "ETH").String())
	require.Equal(t, "0.0025", h.houseBalance(t, ctx, "fee_revenue", "ETH").String())

	_, err := h.reviewer().RequestReversal(ctx, deposit.ReverseParams{
		ID: rec.ID, Note: "reorg 40 blocks deep, confirmed with the node operator",
		ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	require.NoError(t, err)
	assert.Equal(t, "0.9975", h.available(t, ctx, account, "ETH").String(),
		"asking moves no money; the chain role does that")

	require.NoError(t, h.scanner.Tick(ctx))

	reversed := h.depositRows(t, ctx, account)[0]
	assert.Equal(t, deposit.StatusReversed, reversed.Status)
	require.NotNil(t, reversed.ReversedAt)
	assert.Nil(t, reversed.CreditedAt, "0009 ties credited_at to the status")
	require.NotNil(t, reversed.Fee)
	assert.Equal(t, "0.0025", reversed.Fee.String(), "what was charged survives the reversal")
	assert.Equal(t, "0.9975", reversed.Credited.String())

	assert.Equal(t, "0", h.available(t, ctx, account, "ETH").String(), "the account gives back what it got")
	assert.Equal(t, "0", h.houseBalance(t, ctx, "fee_revenue", "ETH").String(), "and the exchange gives back the fee")
	assert.Equal(t, "0", h.houseBalance(t, ctx, "custody_deposit_addresses", "ETH").String(),
		"custody gives back the full amount the chain never delivered")

	h.assertTrialBalanceZero(t, ctx)
	h.assertCacheMatchesPostings(t, ctx, account)

	// Idempotent: the deposit is out of the queue and a further tick does not
	// post a second entry.
	queue, err := h.reviewer().AwaitingReversal(ctx, 50, 0)
	require.NoError(t, err)
	assert.Empty(t, queue)
	require.NoError(t, h.scanner.Tick(ctx))
	assert.Equal(t, "0", h.available(t, ctx, account, "ETH").String())
	h.assertTrialBalanceZero(t, ctx)
}

// The case the whole design is careful about: the account spent the money
// before anyone noticed. Balances may not go negative, so the reversal is
// refused rather than forced, and the decision goes back to a person.
func TestAReversalIsRefusedWhenTheMoneyHasAlreadyGone(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: 1, RingDepth: 16, OrphanExpiryBlocks: 8})
	ctx := context.Background()
	account, rec := h.creditedThenReorged(t, ctx)

	// Spent: the simplest stand-in for a trade or a withdrawal that already
	// took it out of the account.
	require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
		_, _, err := h.svc.Adjust(ctx, tx, ledger.AdjustParams{
			AccountID: account, Asset: "ETH", Amount: amt("1"), Direction: ledger.Debit,
			Reason: "spent it", IdempotencyKey: "spent-" + rec.ID,
		})
		return err
	}))
	require.Equal(t, "0", h.available(t, ctx, account, "ETH").String())

	_, err := h.reviewer().RequestReversal(ctx, deposit.ReverseParams{
		ID: rec.ID, Note: "reorg confirmed",
		ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	require.NoError(t, err)
	require.NoError(t, h.scanner.Tick(ctx), "a refused reversal is not a scanner failure")

	after := h.depositRows(t, ctx, account)[0]
	assert.Equal(t, deposit.StatusCredited, after.Status, "nothing was reversed")
	assert.Contains(t, after.ReversalError, "already been spent")
	assert.Contains(t, after.ReversalError, "0 ETH")

	// Still in the queue, so the alert stays lit and the decision is still
	// somebody's to make.
	queue, err := h.reviewer().AwaitingReversal(ctx, 50, 0)
	require.NoError(t, err)
	require.Len(t, queue, 1, "the request was cleared but the deposit was not")
	h.assertTrialBalanceZero(t, ctx)
}

// The queue is only for deposits the scanner marked, and only one person can
// ask. Both are enforced by the query's WHERE rather than by a check the
// caller could forget.
func TestOnlyAMarkedDepositCanBeReversedAndOnlyOnce(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: 1, RingDepth: 16, OrphanExpiryBlocks: 8})
	ctx := context.Background()
	r := h.reviewer()

	// A perfectly ordinary credited deposit is not reversible.
	account, address := h.account(t, ctx)
	h.fake.mine("b1", transfer(0, address, oneETH()))
	require.NoError(t, h.scanner.Tick(ctx))
	ordinary := h.depositRows(t, ctx, account)[0]
	_, err := r.RequestReversal(ctx, deposit.ReverseParams{
		ID: ordinary.ID, Note: "no", ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	assert.ErrorIs(t, err, deposit.ErrNotReversible)

	// A reason is required: a reversal with no explanation is unreadable a
	// year later, and this is exactly the entry somebody will be reading.
	_, err = r.RequestReversal(ctx, deposit.ReverseParams{
		ID: ordinary.ID, ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	assert.ErrorIs(t, err, deposit.ErrInvalid)

	// And a second ask on a marked one is refused rather than overwriting.
	h2 := setupScripted(t, deposit.Config{DefaultConfirmations: 1, RingDepth: 16, OrphanExpiryBlocks: 8})
	_, marked := h2.creditedThenReorged(t, ctx)
	r2 := h2.reviewer()
	_, err = r2.RequestReversal(ctx, deposit.ReverseParams{
		ID: marked.ID, Note: "first", ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	require.NoError(t, err)
	_, err = r2.RequestReversal(ctx, deposit.ReverseParams{
		ID: marked.ID, Note: "second", ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	assert.ErrorIs(t, err, deposit.ErrNotReversible)
}
