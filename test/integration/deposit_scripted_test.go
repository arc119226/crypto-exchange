//go:build integration

package integration

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
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

// TestScriptedDepositEventsCarryTheAssetsRequiredConfirmations asks the
// producer, which is the part the contract tests never did.
//
// required_confirmations is declared required by all five deposit schemas, and
// nothing set it: every deposit event this exchange published carried a 0. A
// client told "2 of 0 confirmations" cannot render a progress bar, which is
// the whole reason the field is in the payload.
//
// The golden envelope test did not catch it because it builds the payload by
// hand -- deposit.Payload{..., Required: 6} -- so it proved the struct
// serializes and never asked what the scanner puts in it. A contract test that
// only ever sees hand-made values cannot fail on a contract the producer
// breaks. This one reads what landed in the outbox.
func TestScriptedDepositEventsCarryTheAssetsRequiredConfirmations(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: requiredConfirmations})
	ctx := context.Background()
	account, address := h.account(t, ctx)

	// detected, then confirming, then credited: every event on the path.
	h.fake.mine("b1", transfer(0, address, oneETH()))
	require.NoError(t, h.scanner.Tick(ctx))
	h.fake.mine("b2")
	require.NoError(t, h.scanner.Tick(ctx))
	h.fake.mine("b3")
	require.NoError(t, h.scanner.Tick(ctx))
	require.Equal(t, deposit.StatusCredited, h.status(t, ctx, account))

	rows, err := h.all.Query(ctx,
		`SELECT event_type, payload->>'required_confirmations', payload->>'confirmations'
		   FROM eventbus.outbox
		  WHERE event_type LIKE 'deposit.%' AND account_id = $1
		  ORDER BY id`, account)
	require.NoError(t, err)
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var eventType, required, confirmations string
		require.NoError(t, rows.Scan(&eventType, &required, &confirmations))
		assert.Equal(t, strconv.Itoa(requiredConfirmations), required,
			"%s must carry the asset's required_confirmations, not 0 (confirmations=%s)", eventType, confirmations)
		seen++
	}
	require.NoError(t, rows.Err())
	require.NotZero(t, seen, "no deposit events were emitted, so nothing was asserted")
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

// signedTransfer is a value transfer somebody actually signed, so the scanner
// can recover who sent it. The unsigned `transfer` above is enough for tests
// that only care about the recipient.
func signedTransfer(t *testing.T, key *ecdsa.PrivateKey, nonce uint64, to common.Address, wei *big.Int) *types.Transaction {
	t.Helper()
	signer := types.LatestSignerForChainID(big.NewInt(anvilChainID))
	tx, err := types.SignNewTx(key, signer, &types.LegacyTx{Nonce: nonce, To: &to, Value: wei, Gas: 21000})
	require.NoError(t, err)
	return tx
}

// TestScriptedIgnoresWhatTheExchangeSentItself.
//
// The sweeper funds a deposit address with ether from the hot wallet so the
// address can pay for its own token transfer (§6.4.3). On chain that is a
// plain value transfer into a watched address -- the exact shape this scanner
// looks for -- so it credited it as a deposit: free ether for the account, and
// custody_deposit_addresses booked twice for one movement, once by the sweeper
// and once here.
//
// It shipped in 4c-1 and nothing noticed until reconciliation compared the
// ledger with the chain and found the ledger higher by exactly the funding.
func TestScriptedIgnoresWhatTheExchangeSentItself(t *testing.T) {
	h := setupScripted(t, deposit.Config{DefaultConfirmations: 1})
	ctx := context.Background()
	account, address := h.account(t, ctx)

	hotKey, err := crypto.GenerateKey()
	require.NoError(t, err)
	hot := crypto.PubkeyToAddress(hotKey.PublicKey)
	_, err = h.all.Exec(ctx,
		`INSERT INTO chain.hot_wallets (tenant_id, chain_id, address, next_nonce)
		 VALUES ('default', $1, $2, 0)`, anvilChainID, strings.ToLower(hot.Hex()))
	require.NoError(t, err)

	h.fake.mine("gas-funding", signedTransfer(t, hotKey, 0, address, oneETH()))
	require.NoError(t, h.scanner.Tick(ctx))

	assert.Equal(t, 0, h.rows(t, ctx, account), "the exchange funding its own address is not a deposit")
	assert.Equal(t, "0", h.available(t, ctx, account, "ETH").String(), "and must not become the account's money")
	h.assertTrialBalanceZero(t, ctx)

	t.Run("a stranger sending to the same address still deposits", func(t *testing.T) {
		// The rule is about where the money came from, not about the address:
		// a guard that swallowed real deposits would be worse than the bug.
		strangerKey, err := crypto.GenerateKey()
		require.NoError(t, err)
		h.fake.mine("real-deposit", signedTransfer(t, strangerKey, 0, address, oneETH()))
		require.NoError(t, h.scanner.Tick(ctx))
		assert.Equal(t, 1, h.rows(t, ctx, account))
		assert.Equal(t, "1", h.available(t, ctx, account, "ETH").String())
		h.assertTrialBalanceZero(t, ctx)
	})
}

// TestScriptedAnchorsAtTheStartBlock is the 4d change to §6.4.1 step 5.
//
// The guard used to compare the genesis hash. Genesis is the deepest read in
// the startup path and the one a pruned node is least likely to serve, and a
// public testnet endpoint is a pool of backends that have pruned different
// depths -- the same request for block 0 answers a block one minute and
// "pruned history unavailable" the next. The anchor is now the block the
// scanner's own view starts at, which asks the same question at a depth the
// node still has.
func TestScriptedAnchorsAtTheStartBlock(t *testing.T) {
	ctx := context.Background()
	h := setupScriptedAt(t, 4)

	var block int64
	var hash string
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT anchor_block, anchor_hash FROM chain.chain_state WHERE chain_id = $1`,
		anvilChainID).Scan(&block, &hash))
	assert.Equal(t, int64(4), block, "the anchor is StartBlock, not genesis")

	// And it is the hash of that block, not of block 0 -- the point of the
	// change is lost if the recorded value still comes from the deepest read.
	genesis, err := h.fake.AnchorHash(ctx, 0)
	require.NoError(t, err)
	fourth, err := h.fake.AnchorHash(ctx, 4)
	require.NoError(t, err)
	assert.Equal(t, fourth, hash)
	assert.NotEqual(t, genesis, hash)
}

// TestScriptedAnchorRefusesADifferentChain: same height, different hash. The
// wiped-anvil case, and the reason the guard exists at all.
func TestScriptedAnchorRefusesADifferentChain(t *testing.T) {
	ctx := context.Background()
	h := setupScriptedAt(t, 4)

	_, err := h.all.Exec(ctx,
		`UPDATE chain.chain_state SET anchor_hash = $1 WHERE chain_id = $2`,
		fakeHash("99"), anvilChainID)
	require.NoError(t, err)

	err = h.scanner.Start(ctx)
	require.ErrorIs(t, err, deposit.ErrChainChanged)
	assert.Contains(t, err.Error(), "make reset")
}

// TestScriptedAnchorRefusesAMovedStartBlock is new behaviour, not a port of an
// old guard: ETH_SCAN_START_BLOCK decides what "already scanned" means on a
// fresh database, and until the anchor recorded it, editing it under a live
// one went unnoticed. The message has to name the recorded block, because the
// fix is a setting -- telling an operator to reset would cost them a database
// over a typo.
func TestScriptedAnchorRefusesAMovedStartBlock(t *testing.T) {
	ctx := context.Background()
	h := setupScriptedAt(t, 4)

	_, err := h.all.Exec(ctx,
		`UPDATE chain.chain_state SET anchor_block = 2 WHERE chain_id = $1`, anvilChainID)
	require.NoError(t, err)

	err = h.scanner.Start(ctx)
	require.ErrorIs(t, err, deposit.ErrChainChanged)
	assert.Contains(t, err.Error(), "anchored at block 2")
	assert.Contains(t, err.Error(), "ETH_SCAN_START_BLOCK is 4")
	assert.NotContains(t, err.Error(), "make reset")
}

// Every start says which chain it is on, not only the first. The anchor is
// recorded once, so "chain recorded" appears once in the life of a database --
// while docs/guides/sepolia.md tells an operator to look for that line every
// time they bring the exchange up, and to go hunting for a red error if it is
// missing. From the second start on there was no line and no error.
func TestScannerSaysItVerifiedTheChainOnEveryStart(t *testing.T) {
	ctx := context.Background()
	h := setupScriptedAt(t, 4)

	var logs strings.Builder
	again := deposit.New(h.all, h.fake, h.svc, registry.NewStore(h.all), deposit.Config{
		Tenant: "default", ChainID: anvilChainID, StartBlock: 4,
		DefaultConfirmations: requiredConfirmations,
	}, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	require.NoError(t, again.Start(ctx))

	out := logs.String()
	assert.Contains(t, out, "chain verified", out)
	assert.Contains(t, out, "anchor_block=4", "with the fields the first-start line carries")
	assert.Contains(t, out, "anchor_hash=0x")
	assert.NotContains(t, out, "chain recorded",
		"the anchor was already there: this start verified it, it did not record it")
}

// setupScriptedAt mines a few blocks before the scanner starts, so StartBlock
// can be something other than zero. With StartBlock 0 the anchor is genesis
// and the change under test is invisible.
func setupScriptedAt(t *testing.T, start uint64) scriptedHarness {
	t.Helper()
	lh := setupLedger(t)
	ctx := context.Background()

	fx, err := registry.LoadFixtures(fixtures)
	require.NoError(t, err)
	_, err = registry.Seed(ctx, lh.all, fx, registry.SeedOptions{
		TenantID: "default", RequiredConfirmations: requiredConfirmations,
	})
	require.NoError(t, err)

	fake := newFakeChain(anvilChainID)
	for i := uint64(1); i <= start+2; i++ {
		fake.mine(fmt.Sprintf("b%d", i))
	}
	cfg := deposit.Config{
		Tenant: "default", ChainID: anvilChainID, StartBlock: start,
		DefaultConfirmations: requiredConfirmations,
	}
	s := deposit.New(lh.all, fake, lh.svc, registry.NewStore(lh.all), cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, s.Start(ctx))
	return scriptedHarness{ledgerHarness: lh, fake: fake, scanner: s,
		addresses: chain.NewAddresses(lh.all, "default", anvilChainID)}
}
