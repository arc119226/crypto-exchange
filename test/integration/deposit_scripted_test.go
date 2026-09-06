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

	"github.com/arc119226/crypto-exchange/internal/chain"
	"github.com/arc119226/crypto-exchange/internal/chain/deposit"
	"github.com/arc119226/crypto-exchange/internal/chain/hdwallet"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// usdcContract is the address the dev fixtures give MockUSDC.
const usdcContract = "0x5FbDB2315678afecb367f032d93F642f64180aa3"

type scriptedHarness struct {
	ledgerHarness
	fake      *fakeChain
	scanner   *deposit.Scanner
	addresses *chain.Addresses
}

func setupScripted(t *testing.T, cfg deposit.Config) scriptedHarness {
	t.Helper()
	ctx := context.Background()
	lh := setupLedger(t)

	// The asset's registry value is what the scanner uses; the config is only
	// a fallback for an asset that does not state one. Seeding them to match
	// is what makes a test's DefaultConfirmations mean what it says.
	if cfg.DefaultConfirmations == 0 {
		cfg.DefaultConfirmations = requiredConfirmations
	}
	fx, err := registry.LoadFixtures(fixtures)
	require.NoError(t, err)
	_, err = registry.Seed(ctx, lh.all, fx, registry.SeedOptions{
		TenantID: "default", RequiredConfirmations: cfg.DefaultConfirmations,
	})
	require.NoError(t, err)

	w, err := hdwallet.FromMnemonic(testMnemonic)
	require.NoError(t, err)
	t.Cleanup(w.Close)
	_, err = hdwallet.NewPool(lh.all, w, "default", anvilChainID).Ensure(ctx, 8)
	require.NoError(t, err)

	fake := newFakeChain(anvilChainID)
	cfg.Tenant, cfg.ChainID = "default", anvilChainID
	s := deposit.New(lh.all, fake, lh.svc, registry.NewStore(lh.all), cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, s.Start(ctx))

	return scriptedHarness{ledgerHarness: lh, fake: fake, scanner: s,
		addresses: chain.NewAddresses(lh.all, "default", anvilChainID)}
}

func (h scriptedHarness) account(t *testing.T, ctx context.Context) (string, common.Address) {
	t.Helper()
	id := h.newSpot(t, ctx)
	addr, err := h.addresses.Assign(ctx, id)
	require.NoError(t, err)
	return id, common.HexToAddress(addr)
}

func (h scriptedHarness) available(t *testing.T, ctx context.Context, account, asset string) money.Amount {
	t.Helper()
	return h.balance(t, ctx, account, asset).Available
}

func (h scriptedHarness) status(t *testing.T, ctx context.Context, account string) string {
	t.Helper()
	var s string
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT status FROM chain.deposits WHERE account_id = $1 ORDER BY created_at DESC LIMIT 1`, account).Scan(&s))
	return s
}

func (h scriptedHarness) rows(t *testing.T, ctx context.Context, account string) int {
	t.Helper()
	var n int
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT count(*) FROM chain.deposits WHERE account_id = $1`, account).Scan(&n))
	return n
}

func (h scriptedHarness) cursorBlock(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	var n int64
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT last_scanned_block FROM chain.scan_cursors WHERE chain_id = $1`, anvilChainID).Scan(&n))
	return n
}

// TestScriptedConfirmations: a deposit is not money until the asset's
// required confirmations, and is money on the tick that reaches them
// (docs/domain.md §4: confirmations = head - block + 1).
func TestScriptedConfirmations(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: requiredConfirmations})
	ctx := context.Background()
	account, address := h.account(t, ctx)

	h.fake.mine("b1", transfer(0, address, oneETH()))
	require.NoError(t, h.scanner.Tick(ctx))
	assert.Equal(t, "0", h.available(t, ctx, account, "ETH").String(), "1 confirmation of 3")
	assert.Equal(t, deposit.StatusConfirming, h.status(t, ctx, account))

	h.fake.mine("b2")
	require.NoError(t, h.scanner.Tick(ctx))
	assert.Equal(t, "0", h.available(t, ctx, account, "ETH").String(), "2 of 3")

	h.fake.mine("b3")
	require.NoError(t, h.scanner.Tick(ctx))
	assert.Equal(t, "1", h.available(t, ctx, account, "ETH").String(), "3 of 3: credited")
	assert.Equal(t, deposit.StatusCredited, h.status(t, ctx, account))
	h.assertTrialBalanceZero(t, ctx)
	h.assertCacheMatchesPostings(t, ctx, account)

	t.Run("replaying the same blocks credits nothing more", func(t *testing.T) {
		for range 3 {
			require.NoError(t, h.scanner.Tick(ctx))
		}
		assert.Equal(t, "1", h.available(t, ctx, account, "ETH").String())
		h.assertTrialBalanceZero(t, ctx)
	})
}

// TestScriptedReorg is the drill of docs/domain.md §4, at the exact shape the
// document describes: a deposit seen on a branch that loses is orphaned, the
// cursor rewinds to the common ancestor, and when the same transaction
// reappears at a different height it is UPDATEd rather than INSERTed — the
// trap that would otherwise leave it uncreditable forever.
func TestScriptedReorg(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: requiredConfirmations, RingDepth: 16, OrphanExpiryBlocks: 4})
	ctx := context.Background()
	account, address := h.account(t, ctx)

	h.fake.mine("a1")
	h.fake.mine("a2")
	require.NoError(t, h.scanner.Tick(ctx))
	require.EqualValues(t, 2, h.cursorBlock(t, ctx))

	// the deposit lands on the branch that will lose
	tx := transfer(7, address, oneETH())
	h.fake.mine("a3-with-deposit", tx)
	require.NoError(t, h.scanner.Tick(ctx))
	require.Equal(t, deposit.StatusConfirming, h.status(t, ctx, account))
	require.EqualValues(t, 3, h.cursorBlock(t, ctx))

	// blocks 3.. are replaced by a different branch
	h.fake.rewind(2)
	h.fake.mine("b3")
	h.fake.mine("b4")
	require.NoError(t, h.scanner.Tick(ctx))

	assert.Equal(t, deposit.StatusOrphaned, h.status(t, ctx, account), "seen on a branch that lost")
	assert.Equal(t, "0", h.available(t, ctx, account, "ETH").String(), "an orphan is not money")
	assert.EqualValues(t, 4, h.cursorBlock(t, ctx), "rewound to the ancestor, then scanned the new branch")

	t.Run("the same transaction on the new chain is credited once", func(t *testing.T) {
		h.fake.mine("b5-with-the-same-deposit", tx)
		h.fake.mine("b6")
		h.fake.mine("b7")
		require.NoError(t, h.scanner.Tick(ctx))

		assert.Equal(t, deposit.StatusCredited, h.status(t, ctx, account))
		assert.Equal(t, "1", h.available(t, ctx, account, "ETH").String())
		assert.Equal(t, 1, h.rows(t, ctx, account),
			"the reappearance must UPDATE the existing row, never INSERT a second")
		h.assertTrialBalanceZero(t, ctx)
	})
}

// TestScriptedOrphanExpiry: an orphan that never comes back is dropped.
func TestScriptedOrphanExpiry(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: requiredConfirmations, RingDepth: 16, OrphanExpiryBlocks: 3})
	ctx := context.Background()
	account, address := h.account(t, ctx)

	h.fake.mine("a1")
	require.NoError(t, h.scanner.Tick(ctx))
	h.fake.mine("a2-with-deposit", transfer(1, address, oneETH()))
	require.NoError(t, h.scanner.Tick(ctx))
	require.Equal(t, deposit.StatusConfirming, h.status(t, ctx, account))

	h.fake.rewind(1)
	h.fake.mine("b2")
	require.NoError(t, h.scanner.Tick(ctx))
	require.Equal(t, deposit.StatusOrphaned, h.status(t, ctx, account))

	for i := range 5 {
		h.fake.mine("filler" + string(rune('a'+i)))
	}
	require.NoError(t, h.scanner.Tick(ctx))
	assert.Equal(t, deposit.StatusDropped, h.status(t, ctx, account))
	assert.Equal(t, "0", h.available(t, ctx, account, "ETH").String())
}

// TestScriptedCreditedDepositSurvivesAReorg: past the confirmation depth a
// reorg must not quietly take the money back. §6.4.1 routes that through the
// manual reversed path instead, so the scanner leaves it credited.
func TestScriptedCreditedDepositSurvivesAReorg(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: 1, RingDepth: 16})
	ctx := context.Background()
	account, address := h.account(t, ctx)

	h.fake.mine("a1")
	require.NoError(t, h.scanner.Tick(ctx))
	h.fake.mine("a2-with-deposit", transfer(3, address, oneETH()))
	require.NoError(t, h.scanner.Tick(ctx))
	require.Equal(t, deposit.StatusCredited, h.status(t, ctx, account))

	h.fake.rewind(1)
	h.fake.mine("b2")
	h.fake.mine("b3")
	require.NoError(t, h.scanner.Tick(ctx))

	assert.Equal(t, deposit.StatusCredited, h.status(t, ctx, account),
		"a credited deposit is not silently un-credited; that is the manual reversed path")
	assert.Equal(t, "1", h.available(t, ctx, account, "ETH").String())
	h.assertTrialBalanceZero(t, ctx)
}

// TestScriptedFailedTransferIsNotCredited: a transaction appears in its block
// whether or not it succeeded, unlike a log. An out-of-gas transfer moved
// nothing and must credit nothing.
func TestScriptedFailedTransferIsNotCredited(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: 1})
	ctx := context.Background()
	account, address := h.account(t, ctx)

	h.fake.mineFailed("a1-reverted", transfer(0, address, oneETH()))
	require.NoError(t, h.scanner.Tick(ctx))

	assert.Equal(t, 0, h.rows(t, ctx, account), "a failed transfer is not a deposit")
	assert.Equal(t, "0", h.available(t, ctx, account, "ETH").String())
}

// TestScriptedERC20Deposit covers the token path: a Transfer log to a watched
// address credits the matching registry asset at its own scale.
func TestScriptedERC20Deposit(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: 1})
	ctx := context.Background()
	account, address := h.account(t, ctx)

	// 250.5 USDC at scale 6
	h.fake.mineLogs("a1", erc20Log(common.HexToAddress(usdcContract),
		common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"), address, big.NewInt(250_500_000), 0))
	require.NoError(t, h.scanner.Tick(ctx))

	assert.Equal(t, deposit.StatusCredited, h.status(t, ctx, account))
	assert.Equal(t, "250.5", h.available(t, ctx, account, "USDC").String(), "decoded at the asset's scale, not ETH's")
	assert.Equal(t, "0", h.available(t, ctx, account, "ETH").String())
	h.assertTrialBalanceZero(t, ctx)
	h.assertCacheMatchesPostings(t, ctx, account)
}

// TestScriptedIgnoresOtherRecipients: traffic that merely shares the chain is
// not a deposit.
func TestScriptedIgnoresOtherRecipients(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: 1})
	ctx := context.Background()
	account, _ := h.account(t, ctx)
	stranger := common.HexToAddress("0x000000000000000000000000000000000000c0de")

	h.fake.mine("a1", transfer(0, stranger, oneETH()))
	h.fake.mineLogs("a2", erc20Log(common.HexToAddress(usdcContract),
		common.HexToAddress("0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"), stranger, big.NewInt(1), 0))
	require.NoError(t, h.scanner.Tick(ctx))

	assert.Equal(t, 0, h.rows(t, ctx, account))
	h.assertTrialBalanceZero(t, ctx)
}
