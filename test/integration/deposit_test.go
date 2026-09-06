//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"strings"
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

// requiredConfirmations is deliberately more than one so "not credited yet" is
// a state the tests can actually observe. compose runs anvil at 1.
const requiredConfirmations = 3

// The ERC-20 path is exercised in scripts/e2e.sh rather than here: it needs a
// deployed token, and compose already deploys MockUSDC with forge. This suite
// owns what only precise block control can prove — confirmations and reorgs —
// and pays no price for leaving the token to the stack that has one.
type depositHarness struct {
	ledgerHarness
	anvil     *anvil
	scanner   *deposit.Scanner
	addresses *chain.Addresses
	pool      *hdwallet.Pool
	store     *registry.Store
}

func setupDeposit(t *testing.T) depositHarness {
	t.Helper()
	ctx := context.Background()
	lh := setupLedger(t)
	a := startAnvil(t)

	fx, err := registry.LoadFixtures(fixtures)
	require.NoError(t, err)
	_, err = registry.Seed(ctx, lh.all, fx, registry.SeedOptions{
		TenantID: "default", RequiredConfirmations: requiredConfirmations,
	})
	require.NoError(t, err)

	w, err := hdwallet.FromMnemonic(testMnemonic)
	require.NoError(t, err)
	t.Cleanup(w.Close)
	pool := hdwallet.NewPool(lh.all, w, "default", anvilChainID)
	_, err = pool.Ensure(ctx, 8)
	require.NoError(t, err)

	store := registry.NewStore(lh.all)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := deposit.New(lh.all, a.client(t), lh.svc, store, deposit.Config{
		Tenant: "default", ChainID: anvilChainID,
		// a small ring so the "deeper than we can resolve" branch is reachable
		RingDepth: 16, OrphanExpiryBlocks: 4, DefaultConfirmations: requiredConfirmations,
	}, log)
	require.NoError(t, s.Start(ctx))

	return depositHarness{ledgerHarness: lh, anvil: a, scanner: s, pool: pool, store: store,
		addresses: chain.NewAddresses(lh.all, "default", anvilChainID)}
}

// fundedAccount opens a spot account, claims a deposit address for it and
// returns both.
func (h depositHarness) fundedAccount(t *testing.T, ctx context.Context) (string, common.Address) {
	t.Helper()
	account := h.newSpot(t, ctx)
	addr, err := h.addresses.Assign(ctx, account)
	require.NoError(t, err)
	return account, common.HexToAddress(addr)
}

func (h depositHarness) ethBalance(t *testing.T, ctx context.Context, account string) money.Amount {
	t.Helper()
	return h.balance(t, ctx, account, "ETH").Available
}

func oneETH() *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil) }

// TestDepositCreditsAfterConfirmations is the core DoD of docs/plan-v1.0.md
// §12 Phase 4a: nothing is credited before the asset's required confirmations,
// and it is credited as soon as they are reached.
func TestDepositCreditsAfterConfirmations(t *testing.T) {
	h := setupDeposit(t)
	ctx := context.Background()
	account, address := h.fundedAccount(t, ctx)
	from := h.anvil.Accounts(t)[0]

	h.anvil.SendETH(t, from, address, oneETH())
	h.anvil.Mine(t, 1) // the deposit is now in the head block: 1 confirmation

	require.NoError(t, h.scanner.Tick(ctx))
	assert.Equal(t, "0", h.ethBalance(t, ctx, account).String(), "one confirmation is not enough")
	assert.Equal(t, deposit.StatusConfirming, h.depositStatus(t, ctx, account))

	h.anvil.Mine(t, 1) // 2
	require.NoError(t, h.scanner.Tick(ctx))
	assert.Equal(t, "0", h.ethBalance(t, ctx, account).String(), "two confirmations is not enough")

	h.anvil.Mine(t, 1) // 3 == required
	require.NoError(t, h.scanner.Tick(ctx))
	assert.Equal(t, "1", h.ethBalance(t, ctx, account).String(), "credited at the required confirmations")
	assert.Equal(t, deposit.StatusCredited, h.depositStatus(t, ctx, account))

	// docs/plan-v1.0.md §6.1.4(d): exactly two postings, and the money comes
	// out of custody rather than from nowhere.
	h.assertTrialBalanceZero(t, ctx)
	h.assertCacheMatchesPostings(t, ctx, account)

	t.Run("further ticks credit nothing more", func(t *testing.T) {
		h.anvil.Mine(t, 3)
		require.NoError(t, h.scanner.Tick(ctx))
		require.NoError(t, h.scanner.Tick(ctx))
		assert.Equal(t, "1", h.ethBalance(t, ctx, account).String(), "the idempotency key holds")
		h.assertTrialBalanceZero(t, ctx)
	})
}

// TestDepositReorgOrphansAndRecovers walks the drill in docs/domain.md §4: a
// deposit seen on a branch that loses is orphaned and the cursor rewinds, and
// when the same transaction reappears it is UPDATEd rather than INSERTed — the
// trap that would otherwise make it uncreditable forever.
func TestDepositReorgOrphansAndRecovers(t *testing.T) {
	h := setupDeposit(t)
	ctx := context.Background()
	account, address := h.fundedAccount(t, ctx)
	from := h.anvil.Accounts(t)[0]

	h.anvil.Mine(t, 2) // some history to rewind to
	require.NoError(t, h.scanner.Tick(ctx))
	snapshot := h.anvil.Snapshot(t)

	h.anvil.SendETH(t, from, address, oneETH())
	h.anvil.Mine(t, 1)
	require.NoError(t, h.scanner.Tick(ctx))
	require.Equal(t, deposit.StatusConfirming, h.depositStatus(t, ctx, account),
		"seen but not yet credited, which is what makes it orphanable")
	cursorBefore := h.cursor(t, ctx)

	// the branch carrying the deposit loses
	h.anvil.Revert(t, snapshot)
	h.anvil.Mine(t, 2) // a different branch of the same height
	require.NoError(t, h.scanner.Tick(ctx))

	assert.Equal(t, deposit.StatusOrphaned, h.depositStatus(t, ctx, account))
	assert.Equal(t, "0", h.ethBalance(t, ctx, account).String(), "an orphan is not money")
	assert.Less(t, h.cursor(t, ctx), cursorBefore,
		"the cursor rewound rather than marching past the abandoned blocks")

	t.Run("the same transaction on the new chain is credited", func(t *testing.T) {
		h.anvil.SendETH(t, from, address, oneETH())
		h.anvil.Mine(t, requiredConfirmations)
		require.NoError(t, h.scanner.Tick(ctx))
		require.NoError(t, h.scanner.Tick(ctx))

		assert.Equal(t, deposit.StatusCredited, h.depositStatus(t, ctx, account))
		assert.Equal(t, "1", h.ethBalance(t, ctx, account).String())
		assert.Equal(t, 1, h.depositRows(t, ctx, account),
			"the reappearance must UPDATE the existing row, never INSERT a second")
		h.assertTrialBalanceZero(t, ctx)
	})
}

// TestDepositDropsOrphansThatNeverReturn covers the last transition of §6.4.1.
func TestDepositDropsOrphansThatNeverReturn(t *testing.T) {
	h := setupDeposit(t)
	ctx := context.Background()
	account, address := h.fundedAccount(t, ctx)
	from := h.anvil.Accounts(t)[0]

	h.anvil.Mine(t, 2)
	require.NoError(t, h.scanner.Tick(ctx))
	snapshot := h.anvil.Snapshot(t)

	h.anvil.SendETH(t, from, address, oneETH())
	h.anvil.Mine(t, 1)
	require.NoError(t, h.scanner.Tick(ctx))
	require.Equal(t, deposit.StatusConfirming, h.depositStatus(t, ctx, account))

	h.anvil.Revert(t, snapshot)
	h.anvil.Mine(t, 2)
	require.NoError(t, h.scanner.Tick(ctx))
	require.Equal(t, deposit.StatusOrphaned, h.depositStatus(t, ctx, account))

	// OrphanExpiryBlocks is 4 in this harness
	h.anvil.Mine(t, 6)
	require.NoError(t, h.scanner.Tick(ctx))
	assert.Equal(t, deposit.StatusDropped, h.depositStatus(t, ctx, account))
	assert.Equal(t, "0", h.ethBalance(t, ctx, account).String())
}

// TestDepositIgnoresUnwatchedAddresses: the scanner must not credit anyone for
// traffic that is merely on the same chain.
func TestDepositIgnoresUnwatchedAddresses(t *testing.T) {
	h := setupDeposit(t)
	ctx := context.Background()
	account, _ := h.fundedAccount(t, ctx)
	accounts := h.anvil.Accounts(t)

	h.anvil.SendETH(t, accounts[0], accounts[1], oneETH())
	h.anvil.Mine(t, requiredConfirmations+1)
	require.NoError(t, h.scanner.Tick(ctx))

	assert.Equal(t, "0", h.ethBalance(t, ctx, account).String())
	assert.Equal(t, 0, h.depositRows(t, ctx, account))
	h.assertTrialBalanceZero(t, ctx)
}

// TestScannerRefusesADifferentChain is the guard of §6.4.1 step 5: a wiped
// anvil against a surviving database must stop rather than silently skip every
// deposit between the old cursor and the new chain's head.
func TestScannerRefusesADifferentChain(t *testing.T) {
	h := setupDeposit(t)
	ctx := context.Background()

	_, err := h.all.Exec(ctx,
		`UPDATE chain.chain_state SET genesis_hash = $1 WHERE chain_id = $2`,
		fakeHash("11"), anvilChainID)
	require.NoError(t, err)

	err = h.scanner.Start(ctx)
	require.ErrorIs(t, err, deposit.ErrChainChanged)
	assert.Contains(t, err.Error(), "make reset")
}

func TestScannerRefusesAChainBehindTheCursor(t *testing.T) {
	h := setupDeposit(t)
	ctx := context.Background()

	head, err := h.anvil.client(t).Head(ctx)
	require.NoError(t, err)
	_, err = h.all.Exec(ctx,
		`INSERT INTO chain.scan_cursors (tenant_id, chain_id, last_scanned_block, last_block_hash)
		 VALUES ('default', $1, $2, $3)
		 ON CONFLICT (tenant_id, chain_id) DO UPDATE SET last_scanned_block = excluded.last_scanned_block`,
		anvilChainID, int64(head+1000), fakeHash("22"))
	require.NoError(t, err)

	require.ErrorIs(t, h.scanner.Start(ctx), deposit.ErrChainChanged)
}

// --- helpers -------------------------------------------------------------

// fakeHash builds a well-formed but impossible block hash, so the CHECK
// constraint accepts it and the scanner still refuses the chain.
func fakeHash(b string) string { return "0x" + strings.Repeat(b, 32) }

func (h depositHarness) depositStatus(t *testing.T, ctx context.Context, account string) string {
	t.Helper()
	var status string
	err := h.all.QueryRow(ctx,
		`SELECT status FROM chain.deposits WHERE account_id = $1 ORDER BY created_at DESC LIMIT 1`, account).Scan(&status)
	require.NoError(t, err, "no deposit recorded for %s", account)
	return status
}

func (h depositHarness) depositRows(t *testing.T, ctx context.Context, account string) int {
	t.Helper()
	var n int
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT count(*) FROM chain.deposits WHERE account_id = $1`, account).Scan(&n))
	return n
}

func (h depositHarness) cursor(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	var n int64
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT last_scanned_block FROM chain.scan_cursors WHERE chain_id = $1`, anvilChainID).Scan(&n))
	return n
}
