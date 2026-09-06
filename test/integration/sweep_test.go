//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain"
	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/hdwallet"
	"github.com/arc119226/crypto-exchange/internal/chain/hotwallet"
	"github.com/arc119226/crypto-exchange/internal/chain/signer"
	"github.com/arc119226/crypto-exchange/internal/chain/sweep"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// sweepHarness is the collection half against a scripted node: the real
// signer, the real nonce allocator, the real worker, and a chain that actually
// moves balances so a sweep can be checked by where the money ends up.
type sweepHarness struct {
	ledgerHarness
	chain     *sendChain
	worker    *sweep.Worker
	addresses *chain.Addresses
	hot       common.Address
	usdc      common.Address
}

func setupSweep(t *testing.T) sweepHarness {
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
	_, err = hdwallet.NewPool(lh.all, w, "default", anvilChainID).Ensure(ctx, 8)
	require.NoError(t, err)

	store := registry.NewStore(lh.all)
	s, err := signer.NewKeystoreSigner(lh.all, "default", anvilChainID, w, store, audit.NewRecorder("default"), log)
	require.NoError(t, err)
	hot, err := s.HotWallet(ctx)
	require.NoError(t, err)

	node := newSendChain(anvilChainID)
	// The hot wallet pays for gas funding, so it needs ether of its own.
	node.fund(hot, new(big.Int).Mul(big.NewInt(10), oneETH()))
	nonces := hotwallet.New(lh.all, "default", anvilChainID, hot, node, s, log)
	require.NoError(t, nonces.Start(ctx))

	worker := sweep.New(lh.all, sweep.Config{
		Tenant: "default", ChainID: anvilChainID, NativeAsset: "ETH", DefaultConfirmations: 1,
	}, store, lh.svc, node, s, nonces, audit.NewRecorder("default"), log)

	return sweepHarness{
		ledgerHarness: lh, chain: node, worker: worker, hot: hot,
		addresses: chain.NewAddresses(lh.all, "default", anvilChainID),
		usdc:      common.HexToAddress(usdcContract),
	}
}

// deposited opens an account, claims its deposit address, credits the deposit
// in the ledger the way the scanner would, and puts the matching balance on
// the scripted chain. That pairing is the whole point: the ledger and the
// chain have to agree before a sweep is legitimate.
func (h sweepHarness) deposited(t *testing.T, ctx context.Context, asset, amount string) (string, common.Address) {
	t.Helper()
	account := h.newSpot(t, ctx)
	addr, err := h.addresses.Assign(ctx, account)
	require.NoError(t, err)
	address := common.HexToAddress(addr)
	h.creditDeposit(t, ctx, account, addr, asset, amount)
	return account, address
}

// creditDeposit records a credited deposit and the on-chain balance behind it.
func (h sweepHarness) creditDeposit(t *testing.T, ctx context.Context, account, address, asset, amount string) {
	t.Helper()
	h.insertDeposit(t, ctx, account, address, asset, amount, "credited")
	h.creditLedger(t, ctx, account, address, asset, amount)
	h.putOnChain(asset, common.HexToAddress(address), amt(amount))
}

// creditLedger posts the deposit the way the scanner's Credit does: the money
// arrives at custody:deposit_addresses and lands in the user's available.
func (h sweepHarness) creditLedger(t *testing.T, ctx context.Context, account, address, asset, amount string) {
	t.Helper()
	require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
		_, _, err := h.svc.Credit(ctx, tx, ledger.CreditParams{
			AccountID: account, Asset: asset, Amount: amt(amount),
			Source: ledger.HouseCustodyDepositAddresses, Kind: "deposit",
			IdempotencyKey: "deposit:" + address + ":" + asset + ":" + amount,
			Ref:            ledger.Ref{Type: "deposit", ID: address + ":" + asset + ":" + amount},
			Reason:         "on-chain deposit",
		})
		return err
	}))
}

// insertDeposit writes the chain.deposits row the scanner would have written.
// The sweeper reads this table to decide what the ledger already knows about,
// so a test that credits the ledger without it would be testing a state the
// system cannot reach.
func (h sweepHarness) insertDeposit(t *testing.T, ctx context.Context, account, address, asset, amount, status string) {
	t.Helper()
	// credited_at is not decoration: 0009 requires it to be set exactly when
	// the status is credited.
	_, err := h.all.Exec(ctx, `INSERT INTO chain.deposits
		(tenant_id, chain_id, account_id, address, asset, amount, tx_hash, log_index,
		 block_number, block_hash, confirmations, status, credited_at)
		VALUES ('default', $1, $2, $3, $4, $5::numeric, $6, -1, 1, $7, 1, $8,
		        CASE WHEN $8 = 'credited' THEN now() END)`,
		anvilChainID, account, strings.ToLower(address), asset, amount,
		labelHash("tx-"+address+asset+amount), labelHash("block-1"), status)
	require.NoError(t, err)
}

// creditPending credits a deposit that was left confirming.
func (h sweepHarness) creditPending(ctx context.Context, account, address, asset, amount string) error {
	if _, err := h.all.Exec(ctx,
		`UPDATE chain.deposits SET status = 'credited', credited_at = now()
		 WHERE address = $1 AND asset = $2 AND amount = $3::numeric`,
		strings.ToLower(address), asset, amount); err != nil {
		return err
	}
	return inTx(ctx, h.all, func(tx pgx.Tx) error {
		_, _, err := h.svc.Credit(ctx, tx, ledger.CreditParams{
			AccountID: account, Asset: asset, Amount: amt(amount),
			Source: ledger.HouseCustodyDepositAddresses, Kind: "deposit",
			IdempotencyKey: "deposit:" + address + ":" + asset + ":" + amount,
			Ref:            ledger.Ref{Type: "deposit", ID: address + ":" + asset + ":" + amount},
			Reason:         "on-chain deposit",
		})
		return err
	})
}

// labelHash builds a 32-byte hex hash from a label, so the CHECK constraints
// are satisfied without any of these looking like a real transaction.
func labelHash(label string) string {
	return strings.ToLower(common.BytesToHash([]byte(label)).Hex())
}

// toUnits converts an Amount to the asset's smallest unit.
func toUnits(a money.Amount, scale int32) *big.Int {
	out, err := evm.ToWei(a, scale)
	if err != nil {
		panic(err)
	}
	return out
}

// mustBalance is Balance with the error asserted away.
func (c *sendChain) mustBalance(t *testing.T, ctx context.Context, a common.Address) *big.Int {
	t.Helper()
	out, err := c.Balance(ctx, a)
	require.NoError(t, err)
	return out
}

// putOnChain adds the asset to the scripted chain at the right scale.
func (h sweepHarness) putOnChain(asset string, at common.Address, amount money.Amount) {
	if asset == "ETH" {
		h.chain.fund(at, toUnits(amount, 18))
		return
	}
	h.chain.fundToken(h.usdc, at, toUnits(amount, 6))
}

// sweepWithStatus finds the one sweep in a given state, and fails if there is
// not exactly one.
func (h sweepHarness) sweepWithStatus(t *testing.T, ctx context.Context, status string) sweep.Record {
	t.Helper()
	var found []sweep.Record
	for _, s := range h.sweeps(t, ctx) {
		if s.Status == status {
			found = append(found, s)
		}
	}
	require.Len(t, found, 1, "expected exactly one %s sweep", status)
	return found[0]
}

func (h sweepHarness) sweeps(t *testing.T, ctx context.Context) []sweep.Record {
	t.Helper()
	out, err := sweep.List(ctx, h.all, "default", 50)
	require.NoError(t, err)
	return out
}

// The ordinary native path: an address over the threshold is emptied into the
// hot wallet, and no user balance moves.
func TestSweepCollectsEtherIntoTheHotWallet(t *testing.T) {
	h := setupSweep(t)
	ctx := context.Background()
	account, _ := h.deposited(t, ctx, "ETH", "2")

	before := h.balance(t, ctx, account, "ETH")
	hotBefore := h.chain.mustBalance(t, ctx, h.hot)
	require.NoError(t, h.worker.Tick(ctx)) // plan
	rows := h.sweeps(t, ctx)
	require.Len(t, rows, 1)
	assert.Equal(t, sweep.StatusRequested, rows[0].Status)
	assert.Equal(t, "ETH", rows[0].Asset)
	// The address must keep enough to pay for the transaction it is about to
	// send, so the sweep is a little under what it holds.
	assert.True(t, rows[0].Amount.Cmp(amt("2")) < 0, "the gas budget is left behind, got %s", rows[0].Amount)
	assert.True(t, rows[0].Amount.Cmp(amt("1.99")) > 0, "and not much more than that, got %s", rows[0].Amount)

	require.NoError(t, h.worker.Tick(ctx)) // sign and broadcast
	assert.Equal(t, sweep.StatusBroadcast, h.sweeps(t, ctx)[0].Status)
	h.chain.mineAll()
	require.NoError(t, h.worker.Tick(ctx)) // confirm

	row := h.sweeps(t, ctx)[0]
	require.Equal(t, sweep.StatusConfirmed, row.Status)

	// The money really moved on the chain. The hot wallet started with ether
	// of its own -- it has to pay for gas funding -- so this is the change,
	// not the balance.
	held, err := h.chain.Balance(ctx, h.hot)
	require.NoError(t, err)
	gained := new(big.Int).Sub(held, hotBefore)
	assert.Equal(t, toUnits(row.Amount, 18).String(), gained.String(),
		"the hot wallet gained exactly what the sweep says it sent")

	// And the user saw nothing at all.
	after := h.balance(t, ctx, account, "ETH")
	assert.Equal(t, before.Available.String(), after.Available.String(), "a sweep never touches a user balance")
	assert.Equal(t, "2", after.Available.String())

	// Custody moved from the addresses to the hot wallet, less the gas.
	assert.True(t, h.houseBalance(t, ctx, "custody_hot", "ETH").IsPositive())
	assert.True(t, h.houseBalance(t, ctx, "gas_expense", "ETH").IsPositive())
	h.assertCacheMatchesPostings(t, ctx, account)
	h.assertTrialBalanceZero(t, ctx)
	assertNoSecondSweep(t, ctx, h)
}

// A token sweep takes two transactions, and the first one is the hot wallet
// paying so the address can pay.
func TestSweepFundsGasThenCollectsAToken(t *testing.T) {
	h := setupSweep(t)
	ctx := context.Background()
	account, address := h.deposited(t, ctx, "USDC", "500")

	require.NoError(t, h.worker.Tick(ctx)) // plan
	rows := h.sweeps(t, ctx)
	require.Len(t, rows, 1)
	assert.Equal(t, "USDC", rows[0].Asset)
	assert.Equal(t, "500", rows[0].Amount.String(), "a token sweep takes the whole balance: its gas is in ether")

	require.NoError(t, h.worker.Tick(ctx)) // fund the address
	rows = h.sweeps(t, ctx)
	assert.Equal(t, sweep.StatusRequested, rows[0].Status, "still requested until the funding is mined")
	assert.NotEmpty(t, rows[0].GasFundingTxHash)
	assert.Equal(t, "0", h.chain.mustBalance(t, ctx, address).String(),
		"the address has nothing until the funding is mined")

	h.chain.mineAll()
	require.NoError(t, h.worker.Tick(ctx)) // the funding confirms
	assert.Equal(t, sweep.StatusGasFunded, h.sweeps(t, ctx)[0].Status)
	assert.True(t, h.chain.mustBalance(t, ctx, address).Sign() > 0, "the address can now pay for itself")

	require.NoError(t, h.worker.Tick(ctx)) // sign and broadcast the transfer
	assert.Equal(t, sweep.StatusBroadcast, h.sweeps(t, ctx)[0].Status)
	h.chain.mineAll()
	require.NoError(t, h.worker.Tick(ctx)) // confirm
	require.Equal(t, sweep.StatusConfirmed, h.sweeps(t, ctx)[0].Status)

	tokens, err := h.chain.TokenBalance(ctx, h.usdc, h.hot)
	require.NoError(t, err)
	assert.Equal(t, "500000000", tokens.String(), "500 USDC at six decimals reached the hot wallet")

	// The deposit put 500 USDC into custody:deposit_addresses and the sweep
	// took it out again, so that account is back to nothing and the hot wallet
	// holds it. That round trip is what sweeping is.
	assert.Equal(t, "0", h.houseBalance(t, ctx, "custody_deposit_addresses", "USDC").String())
	assert.Equal(t, "500", h.houseBalance(t, ctx, "custody_hot", "USDC").String())
	// The gas is in ETH even though the sweep moved USDC — two entries in two
	// assets, which is why they cannot be merged into one.
	assert.True(t, h.houseBalance(t, ctx, "gas_expense", "ETH").IsPositive())
	assert.Equal(t, "0", h.houseBalance(t, ctx, "gas_expense", "USDC").String())

	assert.Equal(t, "500", h.balance(t, ctx, account, "USDC").Available.String(), "the user saw nothing")
	h.assertTrialBalanceZero(t, ctx)
}

// An address holding less than the threshold is left alone: the gas would cost
// more than the sweep recovers.
func TestSweepLeavesDustAlone(t *testing.T) {
	h := setupSweep(t)
	ctx := context.Background()
	// The seeded ETH threshold is 0.05.
	h.deposited(t, ctx, "ETH", "0.01")

	require.NoError(t, h.worker.Tick(ctx))
	assert.Empty(t, h.sweeps(t, ctx), "below the threshold is not worth a transaction")
	h.assertTrialBalanceZero(t, ctx)
}

// The rule that keeps custody:deposit_addresses honest: an address with a
// deposit the ledger has not credited yet is not swept at all.
func TestSweepSkipsAnAddressWithAnUncreditedDeposit(t *testing.T) {
	h := setupSweep(t)
	ctx := context.Background()
	account, address := h.deposited(t, ctx, "ETH", "2")

	// A second deposit arrives and is still confirming.
	h.insertDeposit(t, ctx, account, address.Hex(), "ETH", "1", "confirming")
	h.putOnChain("ETH", address, amt("1"))

	require.NoError(t, h.worker.Tick(ctx))
	assert.Empty(t, h.sweeps(t, ctx),
		"sweeping now would cross the credit that is about to happen")

	// Once it is credited the address is sweepable again, for the whole lot.
	require.NoError(t, h.creditPending(ctx, account, address.Hex(), "ETH", "1"))
	require.NoError(t, h.worker.Tick(ctx))
	rows := h.sweeps(t, ctx)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].Amount.Cmp(amt("2.9")) > 0, "both deposits, got %s", rows[0].Amount)
	h.assertTrialBalanceZero(t, ctx)
}

// The other rule: the chain balance can be higher than what the ledger was
// credited for -- an internal contract transfer the scanner never saw -- and
// the excess is not swept, because custody:deposit_addresses was never
// credited for it.
func TestSweepDoesNotTakeMoreThanTheLedgerKnowsAbout(t *testing.T) {
	h := setupSweep(t)
	ctx := context.Background()
	_, address := h.deposited(t, ctx, "ETH", "1")

	// Two more ether appear from nowhere the scanner can see.
	h.chain.fund(address, new(big.Int).Mul(big.NewInt(2), oneETH()))

	require.NoError(t, h.worker.Tick(ctx))
	rows := h.sweeps(t, ctx)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].Amount.Cmp(amt("1")) <= 0,
		"only the credited ether may be swept, got %s", rows[0].Amount)

	require.NoError(t, h.worker.Tick(ctx))
	h.chain.mineAll()
	require.NoError(t, h.worker.Tick(ctx))
	require.Equal(t, sweep.StatusConfirmed, h.sweeps(t, ctx)[0].Status)

	// The excess is still sitting there, where reconciliation can see it.
	left, err := h.chain.Balance(ctx, address)
	require.NoError(t, err)
	assert.True(t, left.Cmp(new(big.Int).Mul(big.NewInt(2), oneETH())) >= 0,
		"the money nobody credited stays put, got %s wei", left)
	assert.False(t, h.houseBalance(t, ctx, "custody_deposit_addresses", "ETH").IsNegative(),
		"custody never claims a transfer it did not receive")
	h.assertTrialBalanceZero(t, ctx)
}

// An asset whose balance cannot be read is skipped for the tick, and does not
// stop the assets that can be read.
//
// This is the shape CI found on anvil, where the registry names a MockUSDC
// address that the testcontainer's chain has never had deployed. In a real
// deployment it is a registry row with the wrong contract address: the sweeper
// cannot know what is there, so it must not sweep it -- but letting one bad
// row stop collecting everything else would turn a misconfigured token into a
// hot wallet that slowly runs dry.
func TestSweepSkipsAnAssetItCannotRead(t *testing.T) {
	h := setupSweep(t)
	ctx := context.Background()
	account, address := h.deposited(t, ctx, "ETH", "2")
	h.creditDeposit(t, ctx, account, strings.ToLower(address.Hex()), "USDC", "500")

	h.chain.failTokenBalance(h.usdc, errors.New("evm: balanceOf returned 0 bytes, not 32"))

	require.NoError(t, h.worker.Tick(ctx),
		"one unreadable asset is a registry problem, not a failed scan")

	rows := h.sweeps(t, ctx)
	require.Len(t, rows, 1, "the ether was still collected")
	assert.Equal(t, "ETH", rows[0].Asset)

	// And it stays skipped rather than half-planned: nothing claims to know
	// how much USDC is there.
	require.NoError(t, h.worker.Tick(ctx))
	for _, r := range h.sweeps(t, ctx) {
		assert.NotEqual(t, "USDC", r.Asset, "an asset we cannot read is not swept")
	}
	h.assertTrialBalanceZero(t, ctx)
}

// One sweep at a time per address and asset: a slow tick must not start a
// second one that races the first for the same nonce.
func TestSweepDoesNotStartASecondOneWhileTheFirstIsInFlight(t *testing.T) {
	h := setupSweep(t)
	ctx := context.Background()
	h.deposited(t, ctx, "ETH", "2")

	require.NoError(t, h.worker.Tick(ctx))
	require.Len(t, h.sweeps(t, ctx), 1)
	require.NoError(t, h.worker.Tick(ctx)) // broadcasts, and plans again
	require.NoError(t, h.worker.Tick(ctx))
	assert.Len(t, h.sweeps(t, ctx), 1, "the partial unique index is doing its job")
	h.assertTrialBalanceZero(t, ctx)
}

// A sweep that reverts on chain books the gas that was really spent and leaves
// the money where it is, so the next scan tries again.
func TestSweepThatRevertsBooksGasAndRetriesLater(t *testing.T) {
	h := setupSweep(t)
	ctx := context.Background()
	account, _ := h.deposited(t, ctx, "ETH", "2")

	require.NoError(t, h.worker.Tick(ctx))
	require.NoError(t, h.worker.Tick(ctx))
	h.chain.mineReverted()
	require.NoError(t, h.worker.Tick(ctx))

	failed := h.sweepWithStatus(t, ctx, sweep.StatusFailed)
	assert.Equal(t, sweep.FailureOnChain, failed.FailureReason)
	assert.True(t, h.houseBalance(t, ctx, "gas_expense", "ETH").IsPositive(), "the gas was really burned")
	assert.Equal(t, "2", h.balance(t, ctx, account, "ETH").Available.String(), "and the user still saw nothing")

	// The unique index only covers sweeps in flight, so the same tick that
	// failed this one already planned its replacement: the money is still
	// sitting there and still over the threshold.
	assert.Len(t, h.sweeps(t, ctx), 2, "a failed sweep frees the address for another attempt")
	h.sweepWithStatus(t, ctx, sweep.StatusRequested)
	h.assertTrialBalanceZero(t, ctx)
}

// A crash between signing and recording the broadcast must re-send the same
// transaction, not sign a second one on a different nonce.
func TestSweepRecoversFromACrashBetweenSigningAndBroadcast(t *testing.T) {
	h := setupSweep(t)
	ctx := context.Background()
	h.deposited(t, ctx, "ETH", "2")

	require.NoError(t, h.worker.Tick(ctx)) // plan
	require.NoError(t, h.worker.Tick(ctx)) // sign and broadcast
	id := h.sweeps(t, ctx)[0].ID
	var nonce int64
	var hash string
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT nonce, tx_hash FROM chain.sweeps WHERE id = $1`, id).Scan(&nonce, &hash))

	// Rewind to what a crash after signing would have left: the nonce and the
	// bytes on the row, the status still where it was.
	_, err := h.all.Exec(ctx, `UPDATE chain.sweeps SET status = 'requested' WHERE id = $1`, id)
	require.NoError(t, err)

	require.NoError(t, h.worker.Tick(ctx))
	var again int64
	var recovered string
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT nonce, tx_hash FROM chain.sweeps WHERE id = $1`, id).Scan(&again, &recovered))
	assert.Equal(t, nonce, again, "the same nonce")
	assert.Equal(t, hash, recovered, "and the very transaction that already exists")

	var signatures int
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT count(*) FROM chain.signing_log WHERE kind = 'sweep' AND ref_id = $1`, id).Scan(&signatures))
	assert.Equal(t, 1, signatures, "one intent, one signature")
}

// assertNoSecondSweep proves an emptied address is not swept again: the
// remaining dust is below the threshold and the ledger ceiling is spent.
func assertNoSecondSweep(t *testing.T, ctx context.Context, h sweepHarness) {
	t.Helper()
	require.NoError(t, h.worker.Tick(ctx))
	assert.Len(t, h.sweeps(t, ctx), 1, "an emptied address is not worth emptying again")
}

// TestSweepRolePrivileges runs the sweep path under the login roles a split
// deployment actually uses.
//
// Every test above connects as ex_all. Five defects of exactly this shape have
// now reached CI rather than production -- the audit_events failure of 3c, the
// ledger.accounts one of 4a-2, two in 4b-1 and one in 4b-2 -- and each time the
// lesson was that the test has to run the statement the code runs, not
// something that resembles it.
func TestSweepRolePrivileges(t *testing.T) {
	h := setupSweep(t)
	ctx := context.Background()
	account, address := h.deposited(t, ctx, "ETH", "2")

	chainPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_chain"), MaxConns: 4})
	require.NoError(t, err)
	defer chainPool.Close()
	signerPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_signer"), MaxConns: 4})
	require.NoError(t, err)
	defer signerPool.Close()
	adminPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_admin"), MaxConns: 4})
	require.NoError(t, err)
	defer adminPool.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w, err := hdwallet.FromMnemonic(testMnemonic)
	require.NoError(t, err)
	defer w.Close()

	var id string
	t.Run("the chain role can plan and run a sweep", func(t *testing.T) {
		// Reads the address pool, the deposits, the registry; inserts the
		// sweep; posts to the ledger; writes audit rows and the outbox. All of
		// it as ex_chain, which is the role that will actually do it.
		chainLedger := ledger.New(chainPool, "default")
		require.NoError(t, chainLedger.LoadHouseAccounts(ctx))
		s, err := signer.NewKeystoreSigner(signerPool, "default", anvilChainID, w,
			registry.NewStore(signerPool), audit.NewRecorder("default"), log)
		require.NoError(t, err)
		nonces := hotwallet.New(chainPool, "default", anvilChainID, h.hot, h.chain, s, log)
		require.NoError(t, nonces.Start(ctx))
		worker := sweep.New(chainPool, sweep.Config{
			Tenant: "default", ChainID: anvilChainID, NativeAsset: "ETH", DefaultConfirmations: 1,
		}, registry.NewStore(chainPool), chainLedger, h.chain, s, nonces,
			audit.NewRecorder("default"), log)

		require.NoError(t, worker.Tick(ctx)) // plan
		require.NoError(t, worker.Tick(ctx)) // sign (as ex_signer) and broadcast
		h.chain.mineAll()
		require.NoError(t, worker.Tick(ctx)) // confirm, and post

		rows := h.sweeps(t, ctx)
		require.NotEmpty(t, rows)
		id = rows[0].ID
		assert.Equal(t, sweep.StatusConfirmed, rows[0].Status)
		assert.Equal(t, "2", h.balance(t, ctx, account, "ETH").Available.String(), "the user saw nothing")
		h.assertTrialBalanceZero(t, ctx)
	})

	t.Run("the signer role cannot write a sweep", func(t *testing.T) {
		// It signs with a deposit address's own key, which is the most
		// powerful thing in the system. It must still not be able to say a
		// sweep happened, or to invent one to sign.
		for _, stmt := range []string{
			`UPDATE chain.sweeps SET status = 'confirmed' WHERE id = $1`,
			`UPDATE chain.sweeps SET amount = 999 WHERE id = $1`,
		} {
			_, err := signerPool.Exec(ctx, stmt, id)
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr, stmt)
			assert.Equal(t, "42501", pgErr.Code, stmt)
		}
		_, err := signerPool.Exec(ctx, `INSERT INTO chain.sweeps
			(chain_id, address_id, from_address, asset, amount)
			SELECT $1, id, address, 'ETH', 1 FROM chain.deposit_addresses LIMIT 1`, anvilChainID)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "42501", pgErr.Code, "a sweep the signer invented is one it could then sign")
	})

	t.Run("the admin role may look but not touch", func(t *testing.T) {
		rows, err := sweep.List(ctx, adminPool, "default", 10)
		require.NoError(t, err)
		require.NotEmpty(t, rows, "the admin API reads this table")
		assert.Equal(t, strings.ToLower(address.Hex()), rows[0].FromAddress)

		// There is nothing to approve, so there is nothing to write.
		_, err = adminPool.Exec(ctx, `UPDATE chain.sweeps SET status = 'failed' WHERE id = $1`, id)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "42501", pgErr.Code)
	})
}
