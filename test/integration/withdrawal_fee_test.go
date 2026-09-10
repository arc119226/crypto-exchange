//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
	"github.com/arc119226/crypto-exchange/internal/ledger"
)

// The withdrawal fee through every ending of §6.4.2, against the scripted
// chain. The design rests on one sentence -- the fee belongs to the account
// until the transaction confirms -- and the risk the plan names is that one
// path forgets to give it back, which is "charged for something we never
// sent". Each ending gets a test, and every one of them ends with the
// cross-total invariant below, which fails if any path is missed.

// setAssetFees rewrites an asset's three rates. Declared on ledgerHarness
// rather than sendHarness because withdrawalHarness.svc shadows
// ledgerHarness.svc, and this needs neither.
//
// It must be called before the withdrawal is created: Create snapshots the fee
// onto the row, which is the whole point of the column.
func (h ledgerHarness) setAssetFees(t require.TestingT, ctx context.Context, symbol, flat string, wBps, dBps int32) {
	// The ::numeric cast is not decoration: pgx sends a bare Go string as
	// text, and Postgres will not put text into a numeric column.
	_, err := h.all.Exec(ctx,
		`UPDATE registry.assets
		    SET withdrawal_fee = $1::numeric, withdrawal_fee_bps = $2, deposit_fee_bps = $3
		  WHERE symbol = $4`,
		flat, wBps, dBps, symbol)
	require.NoError(t, err)
}

// entryByKey finds one of a withdrawal's entries by idempotency key. Declared
// on ledgerHarness, not sendHarness: withdrawalHarness.svc shadows
// ledgerHarness.svc, so inside a send test `h.svc` is the withdrawal service.
func (h ledgerHarness) entryByKey(t require.TestingT, ctx context.Context, id, key string) (ledger.JournalEntry, bool) {
	entries, err := h.svc.Entries(ctx, ledger.EntriesFilter{RefType: "withdrawal", RefID: id, Limit: 500})
	require.NoError(t, err)
	for _, e := range entries {
		if e.IdempotencyKey == key {
			return e, true
		}
	}
	return ledger.JournalEntry{}, false
}

// feeEntry is the withdrawal's own fee entry, the one the revenue report finds
// by kind.
func (h ledgerHarness) feeEntry(t require.TestingT, ctx context.Context, id string) (ledger.JournalEntry, bool) {
	return h.entryByKey(t, ctx, id, "withdrawal:fee:"+id)
}

// assertWithdrawalFeesMatchRevenue is the invariant the plan asks for, stated
// over every withdrawal at once rather than per path: fee_revenue holds
// exactly the fees of the withdrawals that confirmed, and nothing from any
// other ending. A path that forgot to refund shows up here even if its own
// test never looked at the fee.
func (h ledgerHarness) assertWithdrawalFeesMatchRevenue(t require.TestingT, ctx context.Context) {
	feeRevenue, err := h.svc.HouseAccount(ledger.HouseFeeRevenue)
	require.NoError(t, err)
	rows, err := h.all.Query(ctx,
		`SELECT w.id, w.status, w.fee::text,
		        COALESCE(SUM(p.amount) FILTER (WHERE p.direction = 'credit'), 0)::text
		   FROM chain.withdrawals w
		   LEFT JOIN ledger.journal_entries e
		          ON e.tenant_id = w.tenant_id AND e.ref_type = 'withdrawal' AND e.ref_id = w.id::text
		   LEFT JOIN ledger.postings p
		          ON p.entry_id = e.id AND p.account_id = $1
		  GROUP BY w.id, w.status, w.fee`, feeRevenue)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id, status, fee, credited string
		require.NoError(t, rows.Scan(&id, &status, &fee, &credited))
		want := "0"
		if status == withdrawal.StatusConfirmed {
			want = fee
		}
		assert.Equal(t, amt(want).String(), amt(credited).String(),
			"withdrawal %s is %s, so fee_revenue should hold %s of its fee", id, status, want)
	}
	require.NoError(t, rows.Err())
}

// The fee is held with the amount, stays in hold across the broadcast, and
// only becomes revenue at confirmation. The broadcast assertion is the one
// that pins the design: it moves the amount alone, which is what leaves the
// fee somewhere it can still be given back.
func TestWithdrawalFeeIsHeldWithTheAmountAndBecomesRevenueOnConfirmation(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	h.setAssetFees(t, ctx, "ETH", "0.001", 25, 0)
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-fee-confirm")

	rec := h.create(t, ctx, account, "ETH", "0.05", "fee-confirm-1")
	// 0.001 flat + 25 bps of 0.05 = 0.001125, snapshotted at request time.
	assert.Equal(t, "0.001125", rec.Fee.String())
	assert.Equal(t, "ETH", rec.FeeAsset)

	require.NoError(t, h.worker.Tick(ctx)) // decide
	require.NoError(t, h.worker.Tick(ctx)) // lock
	b := h.balance(t, ctx, account, "ETH")
	assert.Equal(t, "0.051125", b.Hold.String(), "amount + fee, both frozen")
	assert.Equal(t, "4.948875", b.Available.String())

	require.NoError(t, h.worker.Send(ctx)) // sign
	require.NoError(t, h.worker.Send(ctx)) // broadcast
	assert.Equal(t, "0.001125", h.balance(t, ctx, account, "ETH").Hold.String(),
		"the broadcast moves the amount alone; the fee stays where it can still come back")
	assert.Equal(t, "0.05", h.houseBalance(t, ctx, "pending_withdrawal", "ETH").String())
	_, ok := h.feeEntry(t, ctx, rec.ID)
	assert.False(t, ok, "nothing is charged before the chain confirms it")

	h.chain.mineAll()
	require.NoError(t, h.worker.Send(ctx)) // track
	require.Equal(t, withdrawal.StatusConfirmed, h.status(t, ctx, account, rec.ID))

	b = h.balance(t, ctx, account, "ETH")
	assert.Equal(t, "0", b.Hold.String())
	assert.Equal(t, "4.948875", b.Available.String(), "the fee is gone for good; the amount left the chain")
	assert.Equal(t, "0.001125", h.houseBalance(t, ctx, "fee_revenue", "ETH").String())
	assert.Equal(t, "0", h.houseBalance(t, ctx, "pending_withdrawal", "ETH").String())
	assert.True(t, h.houseBalance(t, ctx, "gas_expense", "ETH").IsPositive(), "and the gas was really paid")

	entry, ok := h.feeEntry(t, ctx, rec.ID)
	require.True(t, ok, "the fee has an entry of its own, which is how the revenue report finds it")
	assert.Equal(t, ledger.KindFee, entry.Kind)
	assert.Len(t, entry.Postings, 2)

	h.assertCacheMatchesPostings(t, ctx, account)
	h.assertTrialBalanceZero(t, ctx)
	h.assertWithdrawalFeesMatchRevenue(t, ctx)
}

// Every asset ships at a rate of zero, and the DoD is that nothing changes
// until an operator sets one. This is that assertion.
func TestWithdrawalWithNoFeeWritesNoFeeEntry(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-no-fee")

	rec := h.create(t, ctx, account, "ETH", "0.05", "no-fee-1")
	assert.Equal(t, "0", rec.Fee.String())

	require.NoError(t, h.worker.Tick(ctx))
	require.NoError(t, h.worker.Tick(ctx))
	assert.Equal(t, "0.05", h.balance(t, ctx, account, "ETH").Hold.String(), "the amount, and nothing more")

	require.NoError(t, h.worker.Send(ctx))
	require.NoError(t, h.worker.Send(ctx))
	h.chain.mineAll()
	require.NoError(t, h.worker.Send(ctx))

	assert.Equal(t, "4.95", h.balance(t, ctx, account, "ETH").Available.String())
	assert.Equal(t, "0", h.houseBalance(t, ctx, "fee_revenue", "ETH").String())
	_, ok := h.feeEntry(t, ctx, rec.ID)
	assert.False(t, ok, "a fee of nothing is not an entry")
	h.assertTrialBalanceZero(t, ctx)
	h.assertWithdrawalFeesMatchRevenue(t, ctx)
}

// The node refused the transaction, so nothing was sent and nothing may be
// charged. The whole hold comes back in one release.
func TestWithdrawalFeeComesBackWhenTheBroadcastIsRefused(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	h.setAssetFees(t, ctx, "ETH", "0.001", 25, 0)
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-fee-refused")
	id := h.locked(t, ctx, account, "ETH", "0.05", "fee-refused-1")

	require.NoError(t, h.worker.Send(ctx)) // sign
	h.chain.failNextSend(errors.New("intrinsic gas too low"))
	require.NoError(t, h.worker.Send(ctx)) // broadcast, refused

	b := h.balance(t, ctx, account, "ETH")
	assert.Equal(t, "5", b.Available.String(), "amount and fee, both back")
	assert.Equal(t, "0", b.Hold.String())
	assert.Equal(t, "0", h.houseBalance(t, ctx, "fee_revenue", "ETH").String())
	_, ok := h.feeEntry(t, ctx, id)
	assert.False(t, ok)

	h.assertCacheMatchesPostings(t, ctx, account)
	h.assertTrialBalanceZero(t, ctx)
	h.assertWithdrawalFeesMatchRevenue(t, ctx)
}

// A transaction that was mined and reverted is the one ending that waits for a
// person. The amount parks in pending_withdrawal and the fee stays on hold --
// which looks like a leak and is not: the operator has not yet said whether
// this withdrawal is over.
func TestWithdrawalRevertedOnChainParksTheFeeUntilAnOperatorDecides(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	h.setAssetFees(t, ctx, "ETH", "0.001", 25, 0)
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-fee-revert")
	id := h.locked(t, ctx, account, "ETH", "0.05", "fee-revert-1")

	require.NoError(t, h.worker.Send(ctx)) // sign
	require.NoError(t, h.worker.Send(ctx)) // broadcast
	h.chain.mineReverted()
	require.NoError(t, h.worker.Send(ctx)) // track
	require.Equal(t, withdrawal.StatusFailed, h.status(t, ctx, account, id))

	b := h.balance(t, ctx, account, "ETH")
	assert.Equal(t, "0.05", h.houseBalance(t, ctx, "pending_withdrawal", "ETH").String())
	assert.Equal(t, "4.948875", b.Available.String())
	assert.Equal(t, "0.001125", b.Hold.String(), "still the account's money, still frozen")
	assert.Equal(t, "0", h.houseBalance(t, ctx, "fee_revenue", "ETH").String())
	h.assertTrialBalanceZero(t, ctx)
	h.assertWithdrawalFeesMatchRevenue(t, ctx)

	t.Run("a refund gives both back", func(t *testing.T) {
		_, err := h.reviewer.RequestResolve(ctx, withdrawal.ResolveParams{
			ID: id, Action: withdrawal.ActionRefund, Note: "the destination contract rejected it",
			ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
		})
		require.NoError(t, err)
		require.NoError(t, h.worker.ApplyResolutions(ctx))

		b := h.balance(t, ctx, account, "ETH")
		assert.Equal(t, "5", b.Available.String())
		assert.Equal(t, "0", b.Hold.String())
		assert.Equal(t, "0", h.houseBalance(t, ctx, "pending_withdrawal", "ETH").String())
		assert.Equal(t, "0", h.houseBalance(t, ctx, "fee_revenue", "ETH").String())
		h.assertCacheMatchesPostings(t, ctx, account)
		h.assertTrialBalanceZero(t, ctx)
		h.assertWithdrawalFeesMatchRevenue(t, ctx)
	})
}

// Retry is the one resolution that deliberately does not touch the fee: the
// withdrawal is not over, so neither is the charge. The test then finishes the
// withdrawal, because "charged once" can only be asserted at an ending.
func TestWithdrawalRetryKeepsTheFeeOnHoldAndChargesItOnce(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	h.setAssetFees(t, ctx, "ETH", "0.001", 25, 0)
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-fee-retry")
	id := h.locked(t, ctx, account, "ETH", "0.05", "fee-retry-1")

	require.NoError(t, h.worker.Send(ctx))
	require.NoError(t, h.worker.Send(ctx))
	h.chain.mineReverted()
	require.NoError(t, h.worker.Send(ctx))

	_, err := h.reviewer.RequestResolve(ctx, withdrawal.ResolveParams{
		ID: id, Action: withdrawal.ActionRetry, Note: "the destination has been fixed",
		ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	require.NoError(t, err)
	require.NoError(t, h.worker.ApplyResolutions(ctx))

	status, nonce, _, _ := h.row(t, ctx, id)
	assert.Equal(t, withdrawal.StatusFundsLocked, status)
	assert.Nil(t, nonce)
	assert.Equal(t, "0.051125", h.balance(t, ctx, account, "ETH").Hold.String(),
		"the amount is back on hold beside the fee that never left it")
	h.assertWithdrawalFeesMatchRevenue(t, ctx)

	require.NoError(t, h.worker.Send(ctx)) // sign again
	require.NoError(t, h.worker.Send(ctx)) // broadcast again
	h.chain.mineAll()
	require.NoError(t, h.worker.Send(ctx)) // track
	require.Equal(t, withdrawal.StatusConfirmed, h.status(t, ctx, account, id))

	assert.Equal(t, "0.001125", h.houseBalance(t, ctx, "fee_revenue", "ETH").String(),
		"one fee, not two, even though it was sent twice")
	assert.Equal(t, "0", h.balance(t, ctx, account, "ETH").Hold.String())
	h.assertCacheMatchesPostings(t, ctx, account)
	h.assertTrialBalanceZero(t, ctx)
	h.assertWithdrawalFeesMatchRevenue(t, ctx)
}

// A cancellation displaces the user's transaction with the exchange's own, so
// the exchange sent nothing on their behalf and charges nothing. The refund
// reaches into two buckets: the amount is in pending_withdrawal, the fee never
// left hold.
func TestWithdrawalCancelNonceRefundsTheFeeFromHold(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	h.setAssetFees(t, ctx, "ETH", "0.001", 25, 0)
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-fee-cancel")
	id := h.locked(t, ctx, account, "ETH", "0.05", "fee-cancel-1")

	require.NoError(t, h.worker.Send(ctx)) // sign
	require.NoError(t, h.worker.Send(ctx)) // broadcast

	_, err := h.reviewer.RequestResolve(ctx, withdrawal.ResolveParams{
		ID: id, Action: withdrawal.ActionCancelNonce, Note: "stuck for hours",
		ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	require.NoError(t, err)
	require.NoError(t, h.worker.ApplyResolutions(ctx))
	assert.Equal(t, "0.001125", h.balance(t, ctx, account, "ETH").Hold.String(),
		"asking for a cancellation moves no money")

	var cancelHash *string
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT cancel_tx_hash FROM chain.withdrawals WHERE id = $1`, id).Scan(&cancelHash))
	require.NotNil(t, cancelHash)
	h.chain.mineOnly(common.HexToHash(*cancelHash))
	require.NoError(t, h.worker.Send(ctx))

	b := h.balance(t, ctx, account, "ETH")
	assert.Equal(t, "5", b.Available.String())
	assert.Equal(t, "0", b.Hold.String())
	assert.Equal(t, "0", h.houseBalance(t, ctx, "pending_withdrawal", "ETH").String())
	assert.Equal(t, "0", h.houseBalance(t, ctx, "fee_revenue", "ETH").String())

	cancelled, ok := h.entryByKey(t, ctx, id, "withdrawal:cancelled:"+id)
	require.True(t, ok, "the refund is one entry")
	assert.Len(t, cancelled.Postings, 4, "amount out of pending, fee out of hold, both into available")

	h.assertCacheMatchesPostings(t, ctx, account)
	h.assertTrialBalanceZero(t, ctx)
	h.assertWithdrawalFeesMatchRevenue(t, ctx)
}

// The snapshot is what the column is for: an operator raising the rate must
// not reprice a withdrawal somebody already agreed to.
func TestWithdrawalFeeIsSnapshottedNotRecomputed(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-fee-snapshot")

	h.setAssetFees(t, ctx, "ETH", "0.001", 25, 0)
	first := h.create(t, ctx, account, "ETH", "0.05", "snapshot-1")
	require.Equal(t, "0.001125", first.Fee.String())

	// Ten times the flat part and four times the rate.
	h.setAssetFees(t, ctx, "ETH", "0.01", 100, 0)
	second := h.create(t, ctx, account, "ETH", "0.05", "snapshot-2")
	assert.Equal(t, "0.0105", second.Fee.String(), "the new request pays the new rate")

	again, err := h.svc.Get(ctx, account, first.ID)
	require.NoError(t, err)
	assert.Equal(t, "0.001125", again.Fee.String(), "the one already in flight does not")

	// And the old price is what actually reaches fee_revenue.
	require.NoError(t, h.worker.Tick(ctx)) // decide both
	require.NoError(t, h.worker.Tick(ctx)) // lock both
	require.NoError(t, h.worker.Send(ctx))
	require.NoError(t, h.worker.Send(ctx))
	h.chain.mineAll()
	require.NoError(t, h.worker.Send(ctx))

	assert.True(t, h.houseBalance(t, ctx, "fee_revenue", "ETH").Cmp(amt("0.001125")) >= 0)
	h.assertTrialBalanceZero(t, ctx)
	h.assertWithdrawalFeesMatchRevenue(t, ctx)
}

// The precheck covers amount + fee. A balance that covers the amount alone has
// to be refused at the request, not discovered minutes later when the worker
// tries to lock funds it cannot find.
func TestWithdrawalRefusedWhenTheBalanceCoversTheAmountButNotTheFee(t *testing.T) {
	h := setupWithdrawal(t)
	ctx := context.Background()
	h.setAssetFees(t, ctx, "ETH", "0.001", 25, 0)
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "0.05", "faucet-exact")

	_, err := h.svc.Create(ctx, withdrawal.CreateParams{
		AccountID: account, Asset: "ETH", Amount: amt("0.05"),
		ToAddress: payoutAddress, IdempotencyKey: "exact-1",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, withdrawal.ErrInvalid)
	assert.Contains(t, err.Error(), "plus", "the message names the fee, so the user can see why")
	assert.Contains(t, err.Error(), "fee")

	assert.Equal(t, "0.05", h.balance(t, ctx, account, "ETH").Available.String(), "nothing was held")
	h.assertTrialBalanceZero(t, ctx)
}
