//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
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

// withdrawalConfirmations is what the assets are seeded with here. Two, so the
// test can show a mined transaction that is not yet settled without spending
// much wall clock.
const withdrawalConfirmations = 2

// TestWithdrawalAgainstAnvil is the one thing the scripted chain cannot prove:
// that the bytes this exchange signs are a transaction a real node accepts and
// really moves money.
//
// Everything else about the send half — the failure paths, the races, the
// nonce rules — is pinned in withdrawal_send_test.go, where each of them is a
// method call rather than a piece of luck. What is left for a real node is the
// signature itself, and the only assertion that can check it is the
// destination's own balance.
func TestWithdrawalAgainstAnvil(t *testing.T) {
	h := setupAnvilSend(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-anvil-withdraw")

	destination := common.HexToAddress(payoutAddress)
	before := h.anvil.Balance(t, destination)

	w := h.create(t, ctx, account, "ETH", "0.05", "anvil-withdraw-1")
	require.NoError(t, h.worker.Tick(ctx)) // decide
	require.NoError(t, h.worker.Tick(ctx)) // lock funds
	require.Equal(t, withdrawal.StatusFundsLocked, h.status(t, ctx, account, w.ID))

	require.NoError(t, h.worker.Send(ctx)) // sign
	require.Equal(t, withdrawal.StatusSigned, h.status(t, ctx, account, w.ID))
	require.NoError(t, h.worker.Send(ctx)) // broadcast: anvil accepts the raw tx
	require.Equal(t, withdrawal.StatusBroadcast, h.status(t, ctx, account, w.ID))

	h.anvil.Mine(t, 1)
	require.NoError(t, h.worker.Send(ctx))
	assert.Equal(t, withdrawal.StatusBroadcast, h.status(t, ctx, account, w.ID),
		"mined is not settled: one confirmation of two")

	h.anvil.Mine(t, 1)
	require.NoError(t, h.worker.Send(ctx))
	assert.Equal(t, withdrawal.StatusConfirmed, h.status(t, ctx, account, w.ID))

	// The assertion this whole test exists for.
	after := h.anvil.Balance(t, destination)
	moved := new(big.Int).Sub(after, before)
	got, err := evm.FromWei(moved, 18)
	require.NoError(t, err)
	assert.Equal(t, "0.05", got.String(), "the destination really received the withdrawal")

	// And the gas anvil actually charged is booked in the native coin, not
	// invented: it is whatever the receipt said.
	gas := h.houseBalance(t, ctx, "gas_expense", "ETH")
	assert.True(t, gas.IsPositive(), "gas was really spent, got %s", gas)
	h.assertCacheMatchesPostings(t, ctx, account)
	h.assertTrialBalanceZero(t, ctx)
}

// TestWithdrawalNonceManagerAgainstAnvil pins the reconciliation against a
// node that keeps its own count, which is the only place the two can really
// disagree.
func TestWithdrawalNonceManagerAgainstAnvil(t *testing.T) {
	h := setupAnvilSend(t)
	ctx := context.Background()

	// Nothing sent yet, and nothing allocated: the ordinary case.
	require.NoError(t, h.nonces.Start(ctx))

	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-anvil-nonce")
	id := h.locked(t, ctx, account, "ETH", "0.05", "anvil-nonce-1")
	require.NoError(t, h.worker.Send(ctx)) // sign, taking nonce 0
	require.NoError(t, h.worker.Send(ctx)) // broadcast
	h.anvil.Mine(t, 1)

	// The chain has now seen exactly what we allocated, so a restart is quiet.
	require.NoError(t, h.nonces.Start(ctx))
	var fills int
	require.NoError(t, h.all.QueryRow(ctx, `SELECT count(*) FROM chain.nonce_fills`).Scan(&fills))
	assert.Equal(t, 0, fills, "nothing to fill when the counts agree")

	h.anvil.Mine(t, 1)
	require.NoError(t, h.worker.Send(ctx))
	assert.Equal(t, withdrawal.StatusConfirmed, h.status(t, ctx, account, id))
}

// anvilSendHarness is the send half against a real node.
type anvilSendHarness struct {
	withdrawalHarness
	anvil  *anvil
	nonces *hotwallet.Manager
	hot    common.Address
}

func setupAnvilSend(t *testing.T) anvilSendHarness {
	t.Helper()
	ctx := context.Background()
	a := startAnvil(t)
	h := setupWithdrawal(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	_, err := h.all.Exec(ctx,
		`UPDATE registry.assets SET required_confirmations = $1`, withdrawalConfirmations)
	require.NoError(t, err)

	w, err := hdwallet.FromMnemonic(testMnemonic)
	require.NoError(t, err)
	t.Cleanup(w.Close)
	store := registry.NewStore(h.all)
	s, err := signer.NewKeystoreSigner(h.all, "default", anvilChainID, w, store, audit.NewRecorder("default"), log)
	require.NoError(t, err)
	hot, err := s.HotWallet(ctx)
	require.NoError(t, err)

	// The hot wallet is derived from the seed, so it is not one of anvil's
	// pre-funded accounts and starts with nothing. Give it enough to send the
	// withdrawal and pay for it.
	a.SendETH(t, a.Accounts(t)[0], hot, oneETH())
	a.Mine(t, 1)

	client := a.client(t)
	nonces := hotwallet.New(h.all, "default", anvilChainID, hot, client, s, log)
	require.NoError(t, nonces.Start(ctx))
	h.worker = h.worker.WithSending(client, s, nonces, withdrawal.SendConfig{
		ChainID: anvilChainID, DefaultConfirmations: withdrawalConfirmations,
	})
	return anvilSendHarness{withdrawalHarness: h, anvil: a, nonces: nonces, hot: hot}
}

// locked drives a withdrawal to funds_locked.
func (h anvilSendHarness) locked(t *testing.T, ctx context.Context, account, asset, amount, key string) string {
	t.Helper()
	w := h.create(t, ctx, account, asset, amount, key)
	require.NoError(t, h.worker.Tick(ctx))
	require.NoError(t, h.worker.Tick(ctx))
	require.Equal(t, withdrawal.StatusFundsLocked, h.status(t, ctx, account, w.ID))
	return w.ID
}
