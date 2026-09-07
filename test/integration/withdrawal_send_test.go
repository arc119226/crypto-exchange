//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/hdwallet"
	"github.com/arc119226/crypto-exchange/internal/chain/hotwallet"
	"github.com/arc119226/crypto-exchange/internal/chain/signer"
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// sendHarness is the whole second half of §6.4.2 against a scripted node: the
// real signer with a real key, the real nonce manager, the real worker. Only
// the chain is fake, which is the point — every path here is one an anvil test
// can only reach by luck.
type sendHarness struct {
	withdrawalHarness
	chain  *sendChain
	signer *signer.KeystoreSigner
	nonces *hotwallet.Manager
	hot    common.Address
}

func setupSend(t *testing.T) sendHarness {
	t.Helper()
	return setupSendWith(t, withdrawal.SendConfig{})
}

func setupSendWith(t *testing.T, send withdrawal.SendConfig) sendHarness {
	t.Helper()
	ctx := context.Background()
	h := setupWithdrawal(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	w, err := hdwallet.FromMnemonic(testMnemonic)
	require.NoError(t, err)
	t.Cleanup(w.Close)
	store := registry.NewStore(h.all)
	s, err := signer.NewKeystoreSigner(h.all, "default", anvilChainID, w, store, audit.NewRecorder("default"), log)
	require.NoError(t, err)
	hot, err := s.HotWallet(ctx)
	require.NoError(t, err)

	chain := newSendChain(anvilChainID)
	// The scripted chain enforces balances, so the hot wallet has to actually
	// hold what it is asked to send. Generous amounts: these tests are about
	// the state machine, not about running out.
	chain.fund(hot, new(big.Int).Mul(big.NewInt(100), oneETH()))
	chain.fundToken(common.HexToAddress(usdcContract), hot, big.NewInt(1_000_000_000_000))
	nonces := hotwallet.New(h.all, "default", anvilChainID, hot, chain, s, log)
	require.NoError(t, nonces.Start(ctx))

	send.ChainID = anvilChainID
	if send.ReplaceAfter == 0 {
		// Long enough that no test replaces by accident; the ones that mean to
		// ask for it explicitly.
		send.ReplaceAfter = time.Hour
	}
	if send.DefaultConfirmations == 0 {
		send.DefaultConfirmations = 1
	}
	h.worker = h.worker.WithSending(chain, s, nonces, send)
	return sendHarness{withdrawalHarness: h, chain: chain, signer: s, nonces: nonces, hot: hot}
}

// locked drives a withdrawal to funds_locked, where the send half picks it up.
func (h sendHarness) locked(t *testing.T, ctx context.Context, account, asset, amount, key string) string {
	t.Helper()
	w := h.create(t, ctx, account, asset, amount, key)
	require.NoError(t, h.worker.Tick(ctx)) // decide
	require.NoError(t, h.worker.Tick(ctx)) // lock
	require.Equal(t, withdrawal.StatusFundsLocked, h.status(t, ctx, account, w.ID))
	return w.ID
}

// row reads a withdrawal straight from the table, including the columns the
// Record does not carry.
func (h sendHarness) row(t *testing.T, ctx context.Context, id string) (status string, nonce *int64, txHash *string, replacements int32) {
	t.Helper()
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT status, nonce, tx_hash, replacements FROM chain.withdrawals WHERE id = $1`, id).
		Scan(&status, &nonce, &txHash, &replacements))
	return
}

func (h sendHarness) nextNonce(t *testing.T, ctx context.Context) uint64 {
	t.Helper()
	var n int64
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT next_nonce FROM chain.hot_wallets WHERE chain_id = $1`, anvilChainID).Scan(&n))
	return uint64(n)
}

// houseBalance is one house account's balance in one asset, in the sign
// convention HouseBalances applies: positive means "more of what this account
// is for". A pair with no postings at all reads as zero rather than missing.
func (h ledgerHarness) houseBalance(t *testing.T, ctx context.Context, code, asset string) money.Amount {
	t.Helper()
	rows, err := h.svc.HouseBalances(ctx)
	require.NoError(t, err)
	for _, b := range rows {
		if string(b.Code) == code && b.Asset == asset {
			return b.Balance
		}
	}
	return money.Zero
}

func (h sendHarness) fillStatus(t *testing.T, ctx context.Context, nonce int64) string {
	t.Helper()
	var status string
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT status FROM chain.nonce_fills WHERE chain_id = $1 AND nonce = $2`,
		anvilChainID, nonce).Scan(&status))
	return status
}

func (h sendHarness) fills(t *testing.T, ctx context.Context) map[int64]string {
	t.Helper()
	rows, err := h.all.Query(ctx, `SELECT nonce, reason FROM chain.nonce_fills ORDER BY nonce`)
	require.NoError(t, err)
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var n int64
		var reason string
		require.NoError(t, rows.Scan(&n, &reason))
		out[n] = reason
	}
	require.NoError(t, rows.Err())
	return out
}

// The ordinary path: locked, signed, broadcast, mined, confirmed — with the
// §6.1.4(e) postings at each step and the gas booked separately.
func TestWithdrawalSendsAndConfirms(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-send")
	id := h.locked(t, ctx, account, "ETH", "0.05", "send-1")

	require.NoError(t, h.worker.Send(ctx))
	status, nonce, txHash, _ := h.row(t, ctx, id)
	assert.Equal(t, withdrawal.StatusSigned, status)
	require.NotNil(t, nonce, "the nonce is committed with the signature")
	require.NotNil(t, txHash)
	assert.Equal(t, int64(0), *nonce, "the first withdrawal takes nonce 0")
	assert.Equal(t, 0, h.chain.sentCount(), "signing does not broadcast")

	// Broadcast: hold -> pending_withdrawal, and the bytes reach the node.
	require.NoError(t, h.worker.Send(ctx))
	assert.Equal(t, withdrawal.StatusBroadcast, h.status(t, ctx, account, id))
	assert.Equal(t, 1, h.chain.sentCount())
	b := h.balance(t, ctx, account, "ETH")
	assert.Equal(t, "0", b.Hold.String(), "the hold is gone once the money is really moving")
	assert.Equal(t, "4.95", b.Available.String())
	h.assertTrialBalanceZero(t, ctx)

	// Unmined: nothing happens, and the transaction is not re-sent.
	require.NoError(t, h.worker.Send(ctx))
	assert.Equal(t, withdrawal.StatusBroadcast, h.status(t, ctx, account, id))
	assert.Equal(t, 1, h.chain.sentCount())

	h.chain.mineAll()
	require.NoError(t, h.worker.Send(ctx))
	assert.Equal(t, withdrawal.StatusConfirmed, h.status(t, ctx, account, id))

	// pending_withdrawal -> custody_hot, plus a gas entry in the native coin.
	assert.Equal(t, "0", h.houseBalance(t, ctx, "pending_withdrawal", "ETH").String(),
		"nothing is left in flight")
	gas := h.houseBalance(t, ctx, "gas_expense", "ETH")
	assert.True(t, gas.IsPositive(), "the gas the receipt reported is booked, got %s", gas)
	h.assertCacheMatchesPostings(t, ctx, account)
	h.assertTrialBalanceZero(t, ctx)
}

// A mined transaction is not a settled one: the withdrawal waits for the
// asset's confirmations before the money is booked as gone.
func TestWithdrawalWaitsForConfirmations(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	_, err := h.all.Exec(ctx,
		`UPDATE registry.assets SET required_confirmations = 3 WHERE symbol = 'ETH'`)
	require.NoError(t, err)

	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-confirmations")
	id := h.locked(t, ctx, account, "ETH", "0.05", "confirmations-1")

	require.NoError(t, h.worker.Send(ctx)) // sign
	require.NoError(t, h.worker.Send(ctx)) // broadcast
	h.chain.mineAll()                      // one confirmation: the block itself

	require.NoError(t, h.worker.Send(ctx))
	assert.Equal(t, withdrawal.StatusBroadcast, h.status(t, ctx, account, id), "one of three")
	h.chain.advance(1)
	require.NoError(t, h.worker.Send(ctx))
	assert.Equal(t, withdrawal.StatusBroadcast, h.status(t, ctx, account, id), "two of three")

	h.chain.advance(1)
	require.NoError(t, h.worker.Send(ctx))
	assert.Equal(t, withdrawal.StatusConfirmed, h.status(t, ctx, account, id))
	h.assertTrialBalanceZero(t, ctx)
}

// A crash between signing and recording must not strand the withdrawal.
//
// This is the case the pinned nonce exists for. Before the nonce was written
// to the row first, a restart here allocated a *different* nonce and asked the
// signer for attempt 0 again, which the signing log refuses — the withdrawal
// could never be signed at all.
func TestWithdrawalRecoversFromACrashBetweenSigningAndRecording(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-crash")
	id := h.locked(t, ctx, account, "ETH", "0.05", "crash-1")

	require.NoError(t, h.worker.Send(ctx))
	_, nonce, txHash, _ := h.row(t, ctx, id)
	require.NotNil(t, nonce)
	require.NotNil(t, txHash)

	// Rewind the row to exactly what a crash after the signer's own commit
	// would have left: the nonce pinned, the signature gone from our side but
	// still recorded in chain.signing_log.
	_, err := h.all.Exec(ctx, `UPDATE chain.withdrawals
		SET status = 'funds_locked', raw_tx = NULL, tx_hash = NULL WHERE id = $1`, id)
	require.NoError(t, err)

	require.NoError(t, h.worker.Send(ctx), "the retry must not be refused by the signing log")
	status, again, recovered, _ := h.row(t, ctx, id)
	assert.Equal(t, withdrawal.StatusSigned, status)
	require.NotNil(t, again)
	require.NotNil(t, recovered)
	assert.Equal(t, *nonce, *again, "the same nonce, not a new one")
	assert.Equal(t, *txHash, *recovered, "and the very transaction that already exists")

	var signatures int
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT count(*) FROM chain.signing_log WHERE kind = 'withdrawal' AND ref_id = $1`, id).Scan(&signatures))
	assert.Equal(t, 1, signatures, "one intent, one signature, however often it is asked for")
}

// A node that refuses the send releases the funds and gives the nonce back.
func TestWithdrawalBroadcastRefusedReleasesTheFunds(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-refused")
	id := h.locked(t, ctx, account, "ETH", "0.05", "refused-1")

	require.NoError(t, h.worker.Send(ctx)) // sign
	h.chain.failNextSend(errors.New("intrinsic gas too low"))
	require.NoError(t, h.worker.Send(ctx)) // broadcast, refused

	rec, err := h.svc.Get(ctx, account, id)
	require.NoError(t, err)
	assert.Equal(t, withdrawal.StatusFailed, rec.Status)
	assert.Equal(t, withdrawal.FailureBroadcast, rec.FailureReason)

	b := h.balance(t, ctx, account, "ETH")
	assert.Equal(t, "5", b.Available.String(), "the user has their money back")
	assert.Equal(t, "0", b.Hold.String())
	// Nonce 0 was the only one out, so it steps back rather than being filled.
	assert.Equal(t, uint64(0), h.nextNonce(t, ctx))
	assert.Empty(t, h.fills(t, ctx), "the last nonce out needs no filling")
	h.assertTrialBalanceZero(t, ctx)
}

// A transaction that reverts on chain spends its gas and leaves the amount for
// a person to decide about (§6.1.4 e).
func TestWithdrawalRevertsOnChainAndWaitsForAnOperator(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-revert")
	id := h.locked(t, ctx, account, "ETH", "0.05", "revert-1")

	require.NoError(t, h.worker.Send(ctx)) // sign
	require.NoError(t, h.worker.Send(ctx)) // broadcast
	h.chain.mineReverted()
	require.NoError(t, h.worker.Send(ctx)) // track

	rec, err := h.svc.Get(ctx, account, id)
	require.NoError(t, err)
	assert.Equal(t, withdrawal.StatusFailed, rec.Status)
	assert.Equal(t, withdrawal.FailureOnChain, rec.FailureReason)

	// The amount is neither the user's nor spent: it sits in the house until
	// an operator says which.
	assert.Equal(t, "0.05", h.houseBalance(t, ctx, "pending_withdrawal", "ETH").String())
	assert.Equal(t, "4.95", h.balance(t, ctx, account, "ETH").Available.String())
	assert.True(t, h.houseBalance(t, ctx, "gas_expense", "ETH").IsPositive(), "the gas was really spent")
	h.assertTrialBalanceZero(t, ctx)

	t.Run("refund gives it back", func(t *testing.T) {
		_, err := h.reviewer.RequestResolve(ctx, withdrawal.ResolveParams{
			ID: id, Action: withdrawal.ActionRefund, Note: "the token contract rejected it",
			ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
		})
		require.NoError(t, err)
		require.NoError(t, h.worker.ApplyResolutions(ctx))

		assert.Equal(t, "5", h.balance(t, ctx, account, "ETH").Available.String())
		assert.Equal(t, "0", h.houseBalance(t, ctx, "pending_withdrawal", "ETH").String())
		h.assertCacheMatchesPostings(t, ctx, account)
		h.assertTrialBalanceZero(t, ctx)
	})
}

// retry puts a reverted withdrawal back on hold and sends it again — with a
// fresh nonce, because the old one is spent.
func TestWithdrawalRetryAfterAnOnChainFailure(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-retry")
	id := h.locked(t, ctx, account, "ETH", "0.05", "retry-1")

	require.NoError(t, h.worker.Send(ctx))
	require.NoError(t, h.worker.Send(ctx))
	h.chain.mineReverted()
	require.NoError(t, h.worker.Send(ctx))
	require.Equal(t, withdrawal.StatusFailed, h.status(t, ctx, account, id))

	_, err := h.reviewer.RequestResolve(ctx, withdrawal.ResolveParams{
		ID: id, Action: withdrawal.ActionRetry, Note: "the destination has been fixed",
		ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	require.NoError(t, err)
	require.NoError(t, h.worker.ApplyResolutions(ctx))

	status, nonce, _, _ := h.row(t, ctx, id)
	assert.Equal(t, withdrawal.StatusFundsLocked, status)
	assert.Nil(t, nonce, "the spent nonce is cleared so a fresh one is allocated")
	assert.Equal(t, "0.05", h.balance(t, ctx, account, "ETH").Hold.String(), "back on hold, not available")

	require.NoError(t, h.worker.Send(ctx)) // sign again
	_, nonce, _, _ = h.row(t, ctx, id)
	require.NotNil(t, nonce)
	assert.Equal(t, int64(1), *nonce, "a new nonce, because the first is on the chain")
	h.assertTrialBalanceZero(t, ctx)
}

// An unmined transaction is re-sent with a higher fee, on the same nonce, up
// to the limit — and then waits for a person rather than bidding forever.
func TestWithdrawalReplacesThenStopsAtTheLimit(t *testing.T) {
	h := setupSendWith(t, withdrawal.SendConfig{ReplaceAfter: time.Nanosecond, MaxReplacements: 2})
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-replace")
	id := h.locked(t, ctx, account, "ETH", "0.05", "replace-1")

	require.NoError(t, h.worker.Send(ctx)) // sign
	require.NoError(t, h.worker.Send(ctx)) // broadcast
	_, nonce, first, _ := h.row(t, ctx, id)
	require.NotNil(t, nonce)
	firstFee := h.chain.lastSent().GasFeeCap()

	require.NoError(t, h.worker.Send(ctx)) // unmined for longer than the window
	_, again, second, replacements := h.row(t, ctx, id)
	assert.Equal(t, int32(1), replacements)
	assert.Equal(t, *nonce, *again, "a replacement keeps the nonce; that is what makes it a replacement")
	assert.NotEqual(t, *first, *second, "and is a different transaction")
	assert.Positive(t, h.chain.lastSent().GasFeeCap().Cmp(firstFee), "at a higher fee")
	assert.Equal(t, 1, h.chain.pooled(), "the replacement displaced the original")

	require.NoError(t, h.worker.Send(ctx))
	_, _, _, replacements = h.row(t, ctx, id)
	assert.Equal(t, int32(2), replacements)

	// At the limit it stops. Bidding against a stuck mempool forever is not a
	// strategy; a person decides between another bump and cancelling.
	sent := h.chain.sentCount()
	require.NoError(t, h.worker.Send(ctx))
	require.NoError(t, h.worker.Send(ctx))
	assert.Equal(t, sent, h.chain.sentCount(), "no further bids")
	assert.Equal(t, withdrawal.StatusBroadcast, h.status(t, ctx, account, id))

	t.Run("an operator can bump past the limit", func(t *testing.T) {
		_, err := h.reviewer.RequestResolve(ctx, withdrawal.ResolveParams{
			ID: id, Action: withdrawal.ActionBump, Note: "the fee market has calmed down",
			ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
		})
		require.NoError(t, err)
		require.NoError(t, h.worker.ApplyResolutions(ctx))
		assert.Equal(t, sent+1, h.chain.sentCount())
		_, _, _, replacements := h.row(t, ctx, id)
		assert.Equal(t, int32(3), replacements, "past MaxReplacements, because a person asked")
	})
}

// cancel_nonce displaces a stuck transaction, and the refund waits for the
// displacement to be mined — not for the request.
//
// The refund is a posting, not a Release: by now the money is in
// pending_withdrawal, which it left the hold for when the withdrawal was
// broadcast (the 2026-09-05 erratum, docs/domain.md E1).
func TestWithdrawalCancelNonceRefundsOnlyOnceTheDisplacementIsMined(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-cancel")
	id := h.locked(t, ctx, account, "ETH", "0.05", "cancel-1")

	require.NoError(t, h.worker.Send(ctx)) // sign
	require.NoError(t, h.worker.Send(ctx)) // broadcast
	_, nonce, original, _ := h.row(t, ctx, id)
	require.NotNil(t, nonce)

	_, err := h.reviewer.RequestResolve(ctx, withdrawal.ResolveParams{
		ID: id, Action: withdrawal.ActionCancelNonce, Note: "stuck for two hours",
		ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	require.NoError(t, err)
	require.NoError(t, h.worker.ApplyResolutions(ctx))

	var cancelHash *string
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT cancel_tx_hash FROM chain.withdrawals WHERE id = $1`, id).Scan(&cancelHash))
	require.NotNil(t, cancelHash)
	assert.NotEqual(t, *original, *cancelHash)

	// Nothing has moved yet: the original can still win the race, and
	// refunding a user for money that then leaves anyway is the one mistake
	// this whole path exists to avoid.
	assert.Equal(t, withdrawal.StatusBroadcast, h.status(t, ctx, account, id))
	assert.Equal(t, "0.05", h.houseBalance(t, ctx, "pending_withdrawal", "ETH").String())
	assert.Equal(t, "4.95", h.balance(t, ctx, account, "ETH").Available.String())
	require.NoError(t, h.worker.Send(ctx))
	assert.Equal(t, withdrawal.StatusBroadcast, h.status(t, ctx, account, id), "still undecided")

	// The displacement wins.
	h.chain.mineOnly(common.HexToHash(*cancelHash))
	require.NoError(t, h.worker.Send(ctx))

	rec, err := h.svc.Get(ctx, account, id)
	require.NoError(t, err)
	assert.Equal(t, withdrawal.StatusFailed, rec.Status)
	assert.Equal(t, withdrawal.FailureReplaced, rec.FailureReason)
	assert.Equal(t, *cancelHash, rec.TxHash, "the hash that decided the money is the one reported")
	assert.Equal(t, "5", h.balance(t, ctx, account, "ETH").Available.String(), "refunded in full")
	assert.Equal(t, "0", h.houseBalance(t, ctx, "pending_withdrawal", "ETH").String())
	assert.Equal(t, "cancel_nonce", h.fills(t, ctx)[*nonce], "the displacement is recorded against the nonce")
	assert.Equal(t, "confirmed", h.fillStatus(t, ctx, *nonce), "and settled once it is mined")
	h.assertCacheMatchesPostings(t, ctx, account)
	h.assertTrialBalanceZero(t, ctx)
}

// The original winning the race after a cancellation was asked for is the
// other half of that decision: the withdrawal confirms and nothing is
// refunded.
func TestWithdrawalCancelNonceLosesTheRace(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-race")
	id := h.locked(t, ctx, account, "ETH", "0.05", "race-1")

	require.NoError(t, h.worker.Send(ctx))
	require.NoError(t, h.worker.Send(ctx))
	_, _, original, _ := h.row(t, ctx, id)
	require.NotNil(t, original)

	_, err := h.reviewer.RequestResolve(ctx, withdrawal.ResolveParams{
		ID: id, Action: withdrawal.ActionCancelNonce, Note: "looked stuck",
		ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	require.NoError(t, err)
	require.NoError(t, h.worker.ApplyResolutions(ctx))

	// The original was mined after all.
	h.chain.mineOnly(common.HexToHash(*original))
	require.NoError(t, h.worker.Send(ctx))

	assert.Equal(t, withdrawal.StatusConfirmed, h.status(t, ctx, account, id))
	assert.Equal(t, "4.95", h.balance(t, ctx, account, "ETH").Available.String(), "the money left, as it was meant to")
	assert.Equal(t, "0", h.houseBalance(t, ctx, "pending_withdrawal", "ETH").String())
	h.assertTrialBalanceZero(t, ctx)
}

// A resolution the withdrawal has outgrown is cleared with its reason, not
// applied and not left to be retried forever.
func TestWithdrawalResolveNoLongerAppliesIsCleared(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-stale")
	id := h.locked(t, ctx, account, "ETH", "0.05", "stale-1")

	require.NoError(t, h.worker.Send(ctx))
	require.NoError(t, h.worker.Send(ctx))
	_, err := h.reviewer.RequestResolve(ctx, withdrawal.ResolveParams{
		ID: id, Action: withdrawal.ActionBump, Note: "seems slow",
		ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	require.NoError(t, err)

	// It confirms before anyone gets round to the bump.
	h.chain.mineAll()
	require.NoError(t, h.worker.Send(ctx))
	require.Equal(t, withdrawal.StatusConfirmed, h.status(t, ctx, account, id))

	require.NoError(t, h.worker.ApplyResolutions(ctx))
	var action, resolveErr *string
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT resolve_action, resolve_error FROM chain.withdrawals WHERE id = $1`, id).Scan(&action, &resolveErr))
	assert.Nil(t, action, "the request is not left to be retried forever")
	require.NotNil(t, resolveErr)
	assert.Contains(t, *resolveErr, "nothing in flight to bump")
	h.assertTrialBalanceZero(t, ctx)
}

// The admin API answers 409 for an action that does not apply, while the
// operator is still looking at the screen.
func TestWithdrawalResolveRejectsAnImpossibleAction(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-409")
	id := h.locked(t, ctx, account, "ETH", "0.05", "409-1")

	_, err := h.reviewer.RequestResolve(ctx, withdrawal.ResolveParams{
		ID: id, Action: withdrawal.ActionBump, Note: "nothing has been sent yet",
		ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	assert.ErrorIs(t, err, withdrawal.ErrNotResolvable)

	_, err = h.reviewer.RequestResolve(ctx, withdrawal.ResolveParams{
		ID: id, Action: withdrawal.ActionRefund, Note: "not failed either",
		ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	assert.ErrorIs(t, err, withdrawal.ErrNotResolvable)

	_, err = h.reviewer.RequestResolve(ctx, withdrawal.ResolveParams{
		ID: id, Action: "sudo-send-it", Note: "worth a try",
		ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	assert.ErrorIs(t, err, withdrawal.ErrNotResolvable)

	// And a resolution with no reason is refused before anything else.
	_, err = h.reviewer.RequestResolve(ctx, withdrawal.ResolveParams{
		ID: id, Action: withdrawal.ActionBump,
		ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	assert.ErrorIs(t, err, withdrawal.ErrInvalid)
}

// A USDC withdrawal spends ETH on gas: two entries in two assets, which is why
// they cannot be merged into one.
func TestWithdrawalOfATokenBooksGasInTheNativeCoin(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "USDC", "1000", "faucet-usdc")
	id := h.locked(t, ctx, account, "USDC", "50", "usdc-1")

	require.NoError(t, h.worker.Send(ctx))
	require.NoError(t, h.worker.Send(ctx))
	h.chain.mineAll()
	require.NoError(t, h.worker.Send(ctx))

	assert.Equal(t, withdrawal.StatusConfirmed, h.status(t, ctx, account, id))
	// custody_hot is an asset account and this deployment has never swept
	// anything into it, so paying 50 USDC out of it makes it negative. That is
	// honest rather than broken: sweeping arrives in 4c, and until it does the
	// hot wallet has spent more than collection has brought in. The trial
	// balance still sums to zero, which is the invariant that matters.
	assert.Equal(t, "-50", h.houseBalance(t, ctx, "custody_hot", "USDC").String())
	gas := h.houseBalance(t, ctx, "gas_expense", "ETH")
	assert.True(t, gas.IsPositive(), "gas is in ETH even for a USDC withdrawal")
	assert.Equal(t, "0", h.houseBalance(t, ctx, "gas_expense", "USDC").String())
	h.assertTrialBalanceZero(t, ctx)
}
