//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/hdwallet"
	"github.com/arc119226/crypto-exchange/internal/chain/hotwallet"
	"github.com/arc119226/crypto-exchange/internal/chain/signer"
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// nonceHarness is the nonce manager against a scripted node.
//
// The three startup rules of §6.4.2 are the easiest thing in the chain role to
// get wrong and the hardest to reach with a real node: two of them need the
// chain and the database to disagree in a specific direction, which anvil will
// not do on request. Here each one is two lines.
type nonceHarness struct {
	ledgerHarness
	chain  *sendChain
	signer *signer.KeystoreSigner
	hot    common.Address
	log    *slog.Logger
}

func setupNonces(t *testing.T) nonceHarness {
	t.Helper()
	ctx := context.Background()
	lh := setupLedger(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	fx, err := registry.LoadFixtures(fixtures)
	require.NoError(t, err)
	_, err = registry.Seed(ctx, lh.all, fx, registry.SeedOptions{TenantID: "default", RequiredConfirmations: 1})
	require.NoError(t, err)

	w, err := hdwallet.FromMnemonic(testMnemonic)
	require.NoError(t, err)
	t.Cleanup(w.Close)
	s, err := signer.NewKeystoreSigner(lh.all, "default", anvilChainID, w, registry.NewStore(lh.all),
		audit.NewRecorder("default"), log)
	require.NoError(t, err)
	hot, err := s.HotWallet(ctx)
	require.NoError(t, err)

	return nonceHarness{ledgerHarness: lh, chain: newSendChain(anvilChainID), signer: s, hot: hot, log: log}
}

func (h nonceHarness) manager() *hotwallet.Manager {
	return hotwallet.New(h.all, "default", anvilChainID, h.hot, h.chain, h.signer, h.log)
}

func (h nonceHarness) setNextNonce(t *testing.T, ctx context.Context, n int64) {
	t.Helper()
	_, err := h.all.Exec(ctx,
		`INSERT INTO chain.hot_wallets (tenant_id, chain_id, address, next_nonce)
		 VALUES ('default', $1, $2, $3)
		 ON CONFLICT (tenant_id, chain_id) DO UPDATE SET next_nonce = EXCLUDED.next_nonce`,
		anvilChainID, lowerHex(h.hot), n)
	require.NoError(t, err)
}

func (h nonceHarness) storedNonce(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	var n int64
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT next_nonce FROM chain.hot_wallets WHERE chain_id = $1`, anvilChainID).Scan(&n))
	return n
}

func lowerHex(a common.Address) string {
	return "0x" + common.Bytes2Hex(a.Bytes())
}

// A fresh database adopts whatever the chain says, so an exchange pointed at a
// wallet that has already been used starts from where it actually is.
func TestNonceManagerAdoptsTheChainOnAFreshDatabase(t *testing.T) {
	h := setupNonces(t)
	ctx := context.Background()
	h.chain.pretendForeignSends(h.hot, 7)

	require.NoError(t, h.manager().Start(ctx))
	assert.Equal(t, int64(7), h.storedNonce(t, ctx), "not zero: the wallet has a history")
}

// The ordinary case: what we allocated is what the chain has seen.
func TestNonceManagerStartsWhenTheChainAgrees(t *testing.T) {
	h := setupNonces(t)
	ctx := context.Background()
	require.NoError(t, h.manager().Start(ctx))

	h.chain.pretendForeignSends(h.hot, 3)
	h.setNextNonce(t, ctx, 3)
	require.NoError(t, h.manager().Start(ctx), "chain 3, database 3: nothing to do")
	assert.Empty(t, h.fillCount(t, ctx), "and nothing to fill")
}

// The refusal of §6.4.2: the chain has seen more than this database ever
// allocated, so somebody else holds the key.
func TestNonceManagerRefusesWhenTheChainIsAhead(t *testing.T) {
	h := setupNonces(t)
	ctx := context.Background()
	require.NoError(t, h.manager().Start(ctx))

	// Five transactions we never sent.
	h.chain.pretendForeignSends(h.hot, 5)

	err := h.manager().Start(ctx)
	require.ErrorIs(t, err, hotwallet.ErrForeignTransaction)
	assert.Contains(t, err.Error(), "the node reports nonce 5")
	// Nothing was papered over: the stored nonce is untouched, so a restart
	// keeps refusing rather than quietly adopting the stranger's count.
	assert.Equal(t, int64(0), h.storedNonce(t, ctx))
	assert.Empty(t, h.fillCount(t, ctx), "and nothing was sent")
}

// A stored address that is not the one this signer derives is the same
// problem wearing different clothes: the nonce belongs to another wallet.
func TestNonceManagerRefusesAnotherWalletsNonce(t *testing.T) {
	h := setupNonces(t)
	ctx := context.Background()
	_, err := h.all.Exec(ctx,
		`INSERT INTO chain.hot_wallets (tenant_id, chain_id, address, next_nonce)
		 VALUES ('default', $1, '0x00000000000000000000000000000000000000ff', 4)`, anvilChainID)
	require.NoError(t, err)

	err = h.manager().Start(ctx)
	require.ErrorIs(t, err, hotwallet.ErrForeignTransaction)
	assert.Contains(t, err.Error(), "this signer derives")
}

// A nonce nothing is holding is a hole every later transaction queues behind,
// so startup closes it with a transfer that moves nothing.
func TestNonceManagerFillsAGapNothingIsHolding(t *testing.T) {
	h := setupNonces(t)
	ctx := context.Background()
	require.NoError(t, h.manager().Start(ctx))

	// The database allocated 0, 1 and 2; the chain has seen none of them, and
	// no withdrawal claims any.
	h.setNextNonce(t, ctx, 3)
	require.NoError(t, h.manager().Start(ctx))

	assert.Equal(t, 3, h.fillCount(t, ctx), "all three holes are closed")
	assert.Equal(t, 3, h.chain.sentCount(), "each with a real transaction")
	for _, tx := range []uint64{0, 1, 2} {
		assert.Equal(t, "startup_gap", h.fillReason(t, ctx, int64(tx)))
	}
	assert.Equal(t, int64(3), h.storedNonce(t, ctx), "the counter never moves backwards")
}

// A nonce a withdrawal is holding is not a gap — including one pinned to a
// withdrawal that has not been signed yet, which is the window the pinning
// exists for. Filling it would spend the nonce a signature is about to use.
func TestNonceManagerLeavesHeldNoncesAlone(t *testing.T) {
	h := setupSend(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-held")
	id := h.locked(t, ctx, account, "ETH", "0.05", "held-1")

	// Sign, then rewind to exactly the crash window: nonce 0 pinned to a
	// funds_locked withdrawal, with no signature on our side.
	require.NoError(t, h.worker.Send(ctx))
	_, err := h.all.Exec(ctx, `UPDATE chain.withdrawals
		SET status = 'funds_locked', raw_tx = NULL, tx_hash = NULL WHERE id = $1`, id)
	require.NoError(t, err)

	// Two more nonces were allocated and nothing is holding them.
	_, err = h.all.Exec(ctx,
		`UPDATE chain.hot_wallets SET next_nonce = 3 WHERE chain_id = $1`, anvilChainID)
	require.NoError(t, err)

	require.NoError(t, h.nonces.Start(ctx))

	fills := h.fills(t, ctx)
	assert.Len(t, fills, 2, "1 and 2 are holes, 0 is not")
	assert.Equal(t, "startup_gap", fills[1])
	assert.Equal(t, "startup_gap", fills[2])
	_, filled := fills[0]
	assert.False(t, filled, "the withdrawal's own nonce is left for the signature it is about to get")

	// And the withdrawal can still be signed, on that same nonce.
	require.NoError(t, h.worker.Send(ctx))
	status, nonce, _, _ := h.row(t, ctx, id)
	assert.Equal(t, withdrawal.StatusSigned, status)
	require.NotNil(t, nonce)
	assert.Equal(t, int64(0), *nonce)
}

// Recycling the last nonce out steps the counter back; recycling one with
// others already in flight fills it instead, because stepping back would hand
// the same number out twice.
func TestNonceManagerRecyclesOrFills(t *testing.T) {
	h := setupNonces(t)
	ctx := context.Background()
	m := h.manager()
	require.NoError(t, m.Start(ctx))

	var first, second uint64
	require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
		var err error
		first, err = m.Allocate(ctx, tx)
		if err != nil {
			return err
		}
		second, err = m.Allocate(ctx, tx)
		return err
	}))
	require.Equal(t, uint64(0), first)
	require.Equal(t, uint64(1), second)

	require.NoError(t, m.Recycle(ctx, second, "broadcast_failed"))
	assert.Equal(t, int64(1), h.storedNonce(t, ctx), "the last one out simply steps back")
	assert.Equal(t, 0, h.fillCount(t, ctx), "and costs no gas")

	// Allocate again so 0 is no longer the last one out.
	require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
		_, err := m.Allocate(ctx, tx)
		return err
	}))
	require.NoError(t, m.Recycle(ctx, first, "broadcast_failed"))
	assert.Equal(t, 1, h.fillCount(t, ctx), "a nonce with others behind it is filled, not reused")
	assert.Equal(t, "broadcast_failed", h.fillReason(t, ctx, 0))
	assert.Equal(t, int64(2), h.storedNonce(t, ctx), "and the counter still only goes forward")
}

// A fill that reached the chain but is not in the table would be filled again
// on the next start, with a nonce the chain has already consumed. The row is
// written first for exactly that reason; this pins that a recorded fill is
// treated as a holder.
func TestNonceManagerDoesNotRefillARecordedGap(t *testing.T) {
	h := setupNonces(t)
	ctx := context.Background()
	require.NoError(t, h.manager().Start(ctx))
	h.setNextNonce(t, ctx, 2)
	require.NoError(t, h.manager().Start(ctx))
	require.Equal(t, 2, h.fillCount(t, ctx))

	sent := h.chain.sentCount()
	require.NoError(t, h.manager().Start(ctx), "a second start finds the fills already recorded")
	assert.Equal(t, 2, h.fillCount(t, ctx), "and adds none")
	assert.Equal(t, sent, h.chain.sentCount())
}

func (h nonceHarness) fillCount(t *testing.T, ctx context.Context) int {
	t.Helper()
	var n int
	require.NoError(t, h.all.QueryRow(ctx, `SELECT count(*) FROM chain.nonce_fills`).Scan(&n))
	return n
}

func (h nonceHarness) fillReason(t *testing.T, ctx context.Context, nonce int64) string {
	t.Helper()
	var reason string
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT reason FROM chain.nonce_fills WHERE chain_id = $1 AND nonce = $2`,
		anvilChainID, nonce).Scan(&reason))
	return reason
}

// TestNonceManagerWaitsAboveTheFeeCeiling: a gap fill is the one transaction
// nothing else can proceed without -- every nonce behind it is stuck -- and it
// still respects ETH_MAX_FEE_PER_GAS.
//
// That is deliberate rather than an oversight in the other direction. A
// stop-loss that exempts the transaction most likely to be sent during a fee
// spike is not a stop-loss, and nothing is lost by waiting: the same ceiling
// has already stopped every withdrawal and every sweep, so the queue the fill
// would unblock is not moving anyway. Before 4d the manager never saw the
// ceiling at all.
func TestNonceManagerWaitsAboveTheFeeCeiling(t *testing.T) {
	h := setupNonces(t)
	ctx := context.Background()
	// The scripted chain suggests 2*baseFee + tip = 3.5 gwei.
	m := h.manager().WithMaxFee(gwei(3))
	require.NoError(t, m.Start(ctx))

	var first uint64
	require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
		var err error
		if first, err = m.Allocate(ctx, tx); err != nil {
			return err
		}
		_, err = m.Allocate(ctx, tx) // so `first` is no longer the last one out
		return err
	}))

	err := m.Recycle(ctx, first, "broadcast_failed")
	require.ErrorIs(t, err, evm.ErrFeeCeiling)
	assert.Equal(t, 0, h.fillCount(t, ctx), "nothing was sent")
	assert.Equal(t, int64(2), h.storedNonce(t, ctx), "and the counter did not move backwards over a gap")

	// The gap is still there to be filled when the market allows it.
	h.chain.setBaseFee(gwei(0))
	require.NoError(t, m.Recycle(ctx, first, "broadcast_failed"))
	assert.Equal(t, 1, h.fillCount(t, ctx))
	assert.Equal(t, "broadcast_failed", h.fillReason(t, ctx, int64(first)))
}
