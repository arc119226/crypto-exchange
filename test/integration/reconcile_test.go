//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/reconcile"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// reconcileHarness is the sweep harness plus the reconciler, which shares its
// scripted chain, its ledger and its addresses. Sharing them is the point: the
// comparison is only meaningful against the same world the sweeper moved.
type reconcileHarness struct {
	sweepHarness
	worker *reconcile.Worker
}

func setupReconcile(t *testing.T, min money.Amount) reconcileHarness {
	t.Helper()
	ctx := context.Background()
	sh := setupSweep(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	h := reconcileHarness{
		sweepHarness: sh,
		worker: reconcile.New(sh.all, reconcile.Config{
			Tenant: "default", ChainID: anvilChainID, NativeAsset: "ETH",
			DefaultConfirmations: 1, HotWalletMin: min,
		}, registry.NewStore(sh.all), sh.svc, sh.chain, log),
	}
	// The scanner's cursor is what the ledger knows about the chain; without a
	// row there is nothing to clamp the frontier to.
	h.setCursor(t, ctx, 1_000_000)
	return h
}

// setCursor writes the scan cursor the frontier is clamped to.
func (h reconcileHarness) setCursor(t *testing.T, ctx context.Context, block int64) {
	t.Helper()
	_, err := h.all.Exec(ctx, `
		INSERT INTO chain.scan_cursors (tenant_id, chain_id, last_scanned_block, last_block_hash)
		VALUES ('default', $1, $2, $3)
		ON CONFLICT (tenant_id, chain_id) DO UPDATE
		SET last_scanned_block = EXCLUDED.last_scanned_block`,
		anvilChainID, block, labelHash("cursor"))
	require.NoError(t, err)
}

// bookHotWalletOpening records the ether the scripted chain handed the hot
// wallet before the exchange existed.
//
// Every deployment has this moment: a faucet, or a transfer from cold storage,
// puts money in the hot wallet without a transaction this ledger produced.
// Until it is recorded the very first pass reports a break, and that break is
// correct -- which is why the tests book it the same way the e2e does rather
// than teaching the reconciler to ignore it.
func (h reconcileHarness) bookHotWalletOpening(t *testing.T, ctx context.Context) {
	t.Helper()
	balance, err := h.chain.Balance(ctx, h.hot)
	require.NoError(t, err)
	opening, err := evm.FromWei(balance, 18)
	require.NoError(t, err)
	h.bookCustody(t, ctx, ledger.HouseCustodyHot, "ETH", opening, ledger.Credit, "hot-opening")
}

func (h reconcileHarness) bookCustody(
	t *testing.T, ctx context.Context, code ledger.HouseCode, asset string,
	amount money.Amount, dir ledger.Direction, key string,
) {
	t.Helper()
	require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
		_, _, err := h.svc.AdjustHouse(ctx, tx, ledger.HouseAdjustParams{
			Code: code, Asset: asset, Amount: amount, Direction: dir,
			Reason: "test: value that entered custody outside the ledger", IdempotencyKey: key,
		})
		return err
	}))
}

// pass runs one reconciliation and returns the report it wrote.
func (h reconcileHarness) pass(t *testing.T, ctx context.Context) reconcile.Report {
	t.Helper()
	require.NoError(t, h.worker.Tick(ctx))
	report, err := reconcile.Latest(ctx, h.all, "default", anvilChainID)
	require.NoError(t, err)
	return report
}

func lineFor(t *testing.T, r reconcile.Report, asset string) reconcile.Line {
	t.Helper()
	for _, l := range r.Lines {
		if l.Asset == asset {
			return l
		}
	}
	t.Fatalf("no %s line in the report", asset)
	return reconcile.Line{}
}

// TestReconcileBalancesAfterADeposit is the base case: the ledger credited
// exactly what the chain shows, so nothing is left over.
func TestReconcileBalancesAfterADeposit(t *testing.T) {
	h := setupReconcile(t, money.Zero)
	ctx := context.Background()
	h.bookHotWalletOpening(t, ctx)
	h.deposited(t, ctx, "ETH", "2")

	report := h.pass(t, ctx)
	assert.True(t, report.Balanced, "report: %+v", report.Lines)
	eth := lineFor(t, report, "ETH")
	assert.True(t, eth.Diff.IsZero(), "ETH diff %s", eth.Diff)
	assert.Equal(t, "12", eth.ChainTotal.String(), "10 in the hot wallet and the 2 just deposited")
	assert.Equal(t, "12", eth.LedgerTotal.String())
}

// TestReconcileFindsMoneyTheLedgerWasNeverToldAbout is what 4c-1 deliberately
// left for this phase: a sweep is capped at what was credited, so a balance
// that arrived by a path the scanner cannot see stays on chain. Here it is.
func TestReconcileFindsMoneyTheLedgerWasNeverToldAbout(t *testing.T) {
	h := setupReconcile(t, money.Zero)
	ctx := context.Background()
	h.bookHotWalletOpening(t, ctx)
	_, address := h.deposited(t, ctx, "ETH", "2")

	// An internal contract transfer: the ether is there, no Transfer log said
	// so, and no chain.deposits row exists.
	h.chain.fund(address, toUnits(amt("0.75"), 18))

	report := h.pass(t, ctx)
	require.False(t, report.Balanced)
	eth := lineFor(t, report, "ETH")
	assert.Equal(t, "0.75", eth.Diff.String())
	assert.True(t, eth.Uncredited.IsZero(), "nothing is waiting to be credited")
	assert.True(t, eth.InFlight.IsZero(), "nothing is in flight")

	var breaks int
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT count(*) FROM admin.reconciliation_breaks WHERE report_id = $1 AND asset = 'ETH'`,
		report.ID).Scan(&breaks))
	assert.Equal(t, 1, breaks)
	assert.Equal(t, 1, h.outboxCount(t, ctx, reconcile.EventBreakDetected))

	// An operator who has established where it came from records it, and the
	// next pass balances. Nothing else changed: the fix is a ledger entry, not
	// a change to what reconciliation will accept.
	h.bookCustody(t, ctx, ledger.HouseCustodyDepositAddresses, "ETH", amt("0.75"), ledger.Credit, "found-it")
	after := h.pass(t, ctx)
	assert.True(t, after.Balanced, "report: %+v", after.Lines)
}

// TestReconcileStaysQuietWhileABreakIsUnchanged: an open break is still in the
// report and still in the metric, but it is not announced again. An alert that
// repeats every few minutes is one people build filters for.
func TestReconcileStaysQuietWhileABreakIsUnchanged(t *testing.T) {
	h := setupReconcile(t, money.Zero)
	ctx := context.Background()
	h.bookHotWalletOpening(t, ctx)
	_, address := h.deposited(t, ctx, "ETH", "1")
	h.chain.fund(address, toUnits(amt("0.5"), 18))

	first := h.pass(t, ctx)
	require.False(t, first.Balanced)
	require.Equal(t, 1, h.outboxCount(t, ctx, reconcile.EventBreakDetected))

	second := h.pass(t, ctx)
	require.False(t, second.Balanced)
	assert.Equal(t, 1, h.outboxCount(t, ctx, reconcile.EventBreakDetected),
		"the same break must not be announced twice")

	// It grows: that is news again.
	h.chain.fund(address, toUnits(amt("0.25"), 18))
	third := h.pass(t, ctx)
	require.False(t, third.Balanced)
	assert.Equal(t, "0.75", lineFor(t, third, "ETH").Diff.String())
	assert.Equal(t, 2, h.outboxCount(t, ctx, reconcile.EventBreakDetected))
}

// TestReconcileExplainsADepositStillConfirming: the chain shows it, the ledger
// has not credited it, and that is not a break -- it is the scanner doing
// exactly what it is supposed to.
func TestReconcileExplainsADepositStillConfirming(t *testing.T) {
	h := setupReconcile(t, money.Zero)
	ctx := context.Background()
	h.bookHotWalletOpening(t, ctx)
	account := h.newSpot(t, ctx)
	addr, err := h.addresses.Assign(ctx, account)
	require.NoError(t, err)

	h.insertDeposit(t, ctx, account, addr, "ETH", "3", "confirming")
	h.putOnChain("ETH", common.HexToAddress(addr), amt("3"))

	report := h.pass(t, ctx)
	eth := lineFor(t, report, "ETH")
	assert.Equal(t, "3", eth.Uncredited.String())
	assert.True(t, eth.Diff.IsZero(), "diff %s: an uncredited deposit is explained, not a break", eth.Diff)
	assert.True(t, report.Balanced)
}

// TestReconcileClampsTheFrontierToTheScanCursor.
//
// Above the cursor the scanner has not created a row at all, so a deposit
// there is money the chain shows, the ledger has not credited, and
// `uncredited` cannot account for -- there is nothing to sum. Reading at the
// node's head instead of the cursor would report it as a break every time the
// scanner fell one tick behind.
func TestReconcileClampsTheFrontierToTheScanCursor(t *testing.T) {
	h := setupReconcile(t, money.Zero)
	ctx := context.Background()
	h.bookHotWalletOpening(t, ctx)
	_, address := h.deposited(t, ctx, "ETH", "1")

	// Where the scanner has got to, and the state of the chain at that point.
	head, err := h.chain.Head(ctx)
	require.NoError(t, err)
	h.setCursor(t, ctx, int64(head))

	// Now a deposit lands in a later block that the scanner has not reached.
	h.chain.advance(5)
	h.chain.fund(address, toUnits(amt("4"), 18))

	report := h.pass(t, ctx)
	assert.True(t, report.Balanced, "report: %+v", report.Lines)
	assert.Equal(t, int64(head), lineFor(t, report, "ETH").BlockHeight,
		"the frontier is the cursor, not the head")
}

// TestReconcileDropsThePassWhenTheChainMovedUnderIt.
//
// BalanceAt takes a block number, not a hash. If the chain reorgs while a set
// of balances is being read, the answers come from two different chains and
// nothing in them says so. A pass nobody can trust is worse than no pass.
func TestReconcileDropsThePassWhenTheChainMovedUnderIt(t *testing.T) {
	h := setupReconcile(t, money.Zero)
	ctx := context.Background()
	h.bookHotWalletOpening(t, ctx)
	h.deposited(t, ctx, "ETH", "1")

	head, err := h.chain.Head(ctx)
	require.NoError(t, err)
	h.chain.reorgAfterRead(head)

	require.NoError(t, h.worker.Tick(ctx), "a moved chain is a skipped tick, not a failure")
	_, err = reconcile.Latest(ctx, h.all, "default", anvilChainID)
	assert.ErrorIs(t, err, reconcile.ErrNoReport, "nothing may be recorded from a pass that was dropped")
}

// TestReconcileAlertsOnceWhenTheHotWalletIsLow, and again only after it has
// recovered. The mark lives in the database, so a restart does not re-announce
// a condition that has not changed.
func TestReconcileAlertsOnceWhenTheHotWalletIsLow(t *testing.T) {
	h := setupReconcile(t, amt("25"))
	ctx := context.Background()
	h.bookHotWalletOpening(t, ctx) // 10 ETH, below the 25 floor

	h.pass(t, ctx)
	require.Equal(t, 1, h.outboxCount(t, ctx, reconcile.EventHotWalletLow))
	h.pass(t, ctx)
	assert.Equal(t, 1, h.outboxCount(t, ctx, reconcile.EventHotWalletLow), "still low is not news")

	// Topped up, and booked, so the balance moves without opening a break.
	h.chain.fund(h.hot, toUnits(amt("30"), 18))
	h.bookCustody(t, ctx, ledger.HouseCustodyHot, "ETH", amt("30"), ledger.Credit, "top-up")
	h.pass(t, ctx)
	assert.Equal(t, 1, h.outboxCount(t, ctx, reconcile.EventHotWalletLow), "recovering is not an alert")

	// And it can fire again once it has crossed back.
	h.chain.drain(h.hot, toUnits(amt("35"), 18))
	h.bookCustody(t, ctx, ledger.HouseCustodyHot, "ETH", amt("35"), ledger.Debit, "spent-it")
	h.pass(t, ctx)
	assert.Equal(t, 2, h.outboxCount(t, ctx, reconcile.EventHotWalletLow))
}

// TestReconcileBooksTheGasANonceFillBurned.
//
// A gap fill moves nothing and costs gas out of the hot wallet, and until this
// existed nothing booked it: internal/chain/hotwallet imports no ledger at
// all. Every fill therefore left custody_hot claiming ether the chain no
// longer had, permanently -- a break the system caused itself.
func TestReconcileBooksTheGasANonceFillBurned(t *testing.T) {
	h := setupReconcile(t, money.Zero)
	ctx := context.Background()
	h.bookHotWalletOpening(t, ctx)

	before := h.chain.mustBalance(t, ctx, h.hot)
	require.NoError(t, h.nonces.Recycle(ctx, 0, "startup_gap"))
	require.NoError(t, h.nonces.Recycle(ctx, 2, "startup_gap")) // a real hole: 1 is in flight
	h.chain.mineAll()
	h.chain.advance(1)
	spent := new(big.Int).Sub(before, h.chain.mustBalance(t, ctx, h.hot))
	require.Positive(t, spent.Sign(), "the fill must have cost something to be worth booking")

	report := h.pass(t, ctx)
	assert.True(t, report.Balanced, "the fill's gas is booked, so it is not a break: %+v", report.Lines)

	var status string
	var gas pgtype.Numeric
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT status, gas_cost FROM chain.nonce_fills WHERE nonce = 2`).Scan(&status, &gas))
	assert.Equal(t, "confirmed", status)
	booked, err := pg.AmountFromNumeric(gas)
	require.NoError(t, err)
	assert.True(t, booked.IsPositive(), "the cost is recorded on the row, not only in the ledger")
}

// TestReconcileRolePrivileges runs the real code through the real roles.
//
// This defect class has appeared five times: a statement that works as ex_all
// and fails with 42501 only once the deployment is split. The only test that
// catches it is one that runs the statement the code runs, as the role the
// code runs as.
func TestReconcileRolePrivileges(t *testing.T) {
	h := setupReconcile(t, amt("25"))
	ctx := context.Background()
	h.bookHotWalletOpening(t, ctx)
	h.deposited(t, ctx, "ETH", "1")

	chainPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_chain"), MaxConns: 4})
	require.NoError(t, err)
	defer chainPool.Close()
	adminPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_admin"), MaxConns: 4})
	require.NoError(t, err)
	defer adminPool.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("the chain role can run a pass", func(t *testing.T) {
		// Reads the registry, the addresses, the deposits, the hot wallet and
		// the ledger; writes the report, the breaks, the outbox and the fill
		// gas. All as ex_chain, which is the role that will actually do it.
		chainLedger := ledger.New(chainPool, "default")
		require.NoError(t, chainLedger.LoadHouseAccounts(ctx))
		w := reconcile.New(chainPool, reconcile.Config{
			Tenant: "default", ChainID: anvilChainID, NativeAsset: "ETH",
			DefaultConfirmations: 1, HotWalletMin: amt("25"),
		}, registry.NewStore(chainPool), chainLedger, h.chain, log)
		require.NoError(t, w.Tick(ctx))
	})

	t.Run("the admin role can read a report and book an adjustment", func(t *testing.T) {
		report, err := reconcile.Latest(ctx, adminPool, "default", anvilChainID)
		require.NoError(t, err)
		require.NotEmpty(t, report.Lines)

		adminLedger := ledger.New(adminPool, "default")
		require.NoError(t, adminLedger.LoadHouseAccounts(ctx))
		require.NoError(t, inTx(ctx, adminPool, func(tx pgx.Tx) error {
			_, _, err := adminLedger.AdjustHouse(ctx, tx, ledger.HouseAdjustParams{
				Code: ledger.HouseCustodyHot, Asset: "ETH", Amount: amt("1"),
				Direction: ledger.Credit, Reason: "privilege test",
				IdempotencyKey: "privileges:house-adjust",
			})
			return err
		}))
	})

	t.Run("the admin role may not write a report", func(t *testing.T) {
		// Reports are what was true at one moment. Admin displays them and
		// answers them with a ledger entry; it does not get to revise one.
		_, err := adminPool.Exec(ctx,
			`INSERT INTO admin.reconciliation_reports (tenant_id, chain_id, started_at, finished_at, balanced, lines)
			 VALUES ('default', $1, now(), now(), true, '[]'::jsonb)`, anvilChainID)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "42501", pgErr.Code, "a report is what was true at one moment; admin answers it, it does not revise it")
	})
}

// TestReconcileRefusesAnAssetItCannotRead: an unreadable balance fails the
// pass rather than counting as zero.
//
// The sweeper can afford to skip an asset it cannot read -- it just collects
// nothing that tick. A reconciler cannot: reading a misconfigured contract as
// zero would report the entire holding of that asset as missing, and send
// somebody looking for money that is exactly where it should be.
func TestReconcileRefusesAnAssetItCannotRead(t *testing.T) {
	h := setupReconcile(t, money.Zero)
	ctx := context.Background()
	h.bookHotWalletOpening(t, ctx)
	h.chain.failTokenBalance(h.usdc, assert.AnError)

	err := h.worker.Tick(ctx)
	require.ErrorIs(t, err, assert.AnError, "the failure has to reach the caller, not be counted as zero")
	_, latest := reconcile.Latest(ctx, h.all, "default", anvilChainID)
	assert.ErrorIs(t, latest, reconcile.ErrNoReport, "no report may be written from balances it could not read")
}

// outboxCount is how many of one event type are waiting to be published.
func (h reconcileHarness) outboxCount(t *testing.T, ctx context.Context, eventType string) int {
	t.Helper()
	var n int
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT count(*) FROM eventbus.outbox WHERE event_type = $1`, eventType).Scan(&n))
	return n
}
