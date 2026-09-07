package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// Worker runs one reconciliation pass per tick.
type Worker struct {
	db      *pgxpool.Pool
	cfg     Config
	reg     registry.Reader
	ledger  *ledger.Service
	chain   Chain
	log     *slog.Logger
	metrics *Metrics
}

// New builds the reconciler.
func New(db *pgxpool.Pool, cfg Config, reg registry.Reader, l *ledger.Service, c Chain, log *slog.Logger) *Worker {
	if cfg.NativeAsset == "" {
		cfg.NativeAsset = "ETH"
	}
	return &Worker{db: db, cfg: cfg, reg: reg, ledger: l, chain: c, log: log, metrics: NewMetrics(nil)}
}

// WithMetrics attaches Prometheus collectors.
func (w *Worker) WithMetrics(m *Metrics) *Worker {
	if m != nil {
		w.metrics = m
	}
	return w
}

// Tick runs one pass and records it.
//
// A pass that cannot be trusted is dropped rather than reported: a node that
// will not answer, or a reorg part-way through the balance reads. Nothing
// downstream distinguishes "no report" from "a report that happens to be
// wrong", so the difference has to be made here.
func (w *Worker) Tick(ctx context.Context) error {
	started := time.Now().UTC()

	assets, err := w.assets(ctx)
	if err != nil {
		return err
	}
	if len(assets) == 0 {
		return nil
	}
	hot, err := w.hotWallet(ctx)
	if err != nil {
		return w.skipIfNotReadyYet(err)
	}
	head, err := w.chain.Head(ctx)
	if err != nil {
		return fmt.Errorf("reconcile: head: %w", err)
	}
	// Before anything is measured, not after: a gap fill's gas is a cost this
	// system knows about and had never booked, and reporting it as a break
	// would be reporting our own omission as a finding.
	if err := w.bookNonceFills(ctx, head, w.requiredFor(assets, w.cfg.NativeAsset)); err != nil {
		return err
	}
	frontiers, err := w.frontiers(ctx, head, assets)
	if err != nil {
		return w.skipIfNotReadyYet(err)
	}

	addresses, err := w.addresses(ctx)
	if err != nil {
		return err
	}
	onChain, err := w.readBalances(ctx, assets, frontiers, addresses, hot)
	if err != nil {
		if errors.Is(err, errChainMoved) {
			w.log.Info("dropped a reconciliation pass: the chain moved while it was reading",
				slog.String("err", err.Error()))
			return nil
		}
		return err
	}

	book, err := w.readLedger(ctx, assets, frontiers)
	if err != nil {
		return err
	}
	flight, err := w.inFlight(ctx, book.open, frontiers)
	if err != nil {
		return err
	}

	lines := make([]Line, 0, len(assets))
	for _, a := range assets {
		lines = append(lines, w.line(a, frontiers[a.Symbol], onChain[a.Symbol], book, flight))
	}
	return w.record(ctx, started, lines, onChain[w.cfg.NativeAsset].hot)
}

// assets are the registry rows this chain reconciles: every active one,
// including those the sweeper ignores. A threshold decides what is worth
// collecting; it says nothing about what the exchange is holding.
func (w *Worker) assets(ctx context.Context) ([]registry.Asset, error) {
	all, err := w.reg.ListAssets(ctx, w.cfg.Tenant)
	if err != nil {
		return nil, fmt.Errorf("reconcile: list assets: %w", err)
	}
	out := make([]registry.Asset, 0, len(all))
	for _, a := range all {
		if a.ChainID == w.cfg.ChainID && a.Status == registry.AssetActive {
			out = append(out, a)
		}
	}
	return out, nil
}

// skipIfNotReadyYet turns the two "this deployment does not have that row
// yet" errors into a skipped pass, and passes everything else through.
//
// The caller logs any error Tick returns at ERROR, once per interval, with no
// dedup. On a fresh database that meant an ERROR for the missing cursor until
// the scanner's first pass; in a deployment with no signer it meant one every
// five minutes forever, for a row that is never going to appear. Neither is a
// fault, and an ERROR that is always there is one an operator learns to scroll
// past -- which costs the ERROR that matters.
//
// INFO rather than silence: a reconciler that is not reconciling should say
// so, and say what would change it.
func (w *Worker) skipIfNotReadyYet(err error) error {
	switch {
	case errors.Is(err, errNoCursor):
		w.log.Info("skipped a reconciliation pass: the deposit scanner has not recorded a cursor yet",
			slog.Int64("chain_id", w.cfg.ChainID))
		return nil
	case errors.Is(err, errNoHotWallet):
		w.log.Info("skipped a reconciliation pass: no hot wallet is recorded for this chain, "+
			"so its balance cannot be counted; it is written the first time a signer starts",
			slog.Int64("chain_id", w.cfg.ChainID))
		return nil
	}
	return err
}

// hotWallet reads the address from chain.hot_wallets rather than asking the
// signer. That is what lets reconciliation work while the signer is down: the
// row outlives the process that wrote it, the money is still there, and a
// report is exactly what someone wants at that moment.
//
// It does not make reconciliation work in a deployment that has never had a
// signer, and the comment here used to imply that it did. Nothing writes that
// row until a nonce manager starts, so there is no address to read, and the
// hot wallet's balance is part of the chain total -- a report without it would
// invent a break out of money sitting exactly where it belongs. The caller
// skips the pass instead.
func (w *Worker) hotWallet(ctx context.Context) (common.Address, error) {
	row, err := sqlcgen.New(w.db).GetHotWallet(ctx, sqlcgen.GetHotWalletParams{
		TenantID: w.cfg.Tenant, ChainID: w.cfg.ChainID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return common.Address{}, errNoHotWallet
	}
	if err != nil {
		return common.Address{}, fmt.Errorf("reconcile: hot wallet: %w", err)
	}
	return common.HexToAddress(row.Address), nil
}

// addresses is every controlled deposit address, free pool slots included.
//
// The sweeper filters those out because there is nothing to collect from a
// slot nobody was given. Reconciliation must not: the point is to find money
// the ledger does not know about, and a free slot -- which the scanner also
// skips, so a deposit there is never credited -- is precisely where such money
// would be invisible.
func (w *Worker) addresses(ctx context.Context) ([]common.Address, error) {
	rows, err := sqlcgen.New(w.db).ListDepositAddresses(ctx, sqlcgen.ListDepositAddressesParams{
		TenantID: w.cfg.Tenant, ChainID: w.cfg.ChainID,
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile: list deposit addresses: %w", err)
	}
	out := make([]common.Address, 0, len(rows))
	for _, r := range rows {
		out = append(out, common.HexToAddress(r.Address))
	}
	return out, nil
}

// frontiers is the block each asset's balances are read at.
//
//	B = min(head − required + 1, last scanned block)
//
// The first term is the height the scanner, the withdrawal worker and the
// sweeper all book at: each computes head − block + 1 and compares it with the
// asset's required confirmations, so block <= head − required + 1 is exactly
// "already booked". Reading one block lower would open a window in which the
// ledger has recorded a movement the balances cannot show.
//
// The second term is there because the ledger's view of the chain is the scan
// cursor, not the node's head. Above the cursor the scanner has not even
// created rows, so a deposit there would be money the chain shows, the ledger
// has not credited, and nothing accounts for.
func (w *Worker) frontiers(ctx context.Context, head uint64, assets []registry.Asset) (map[string]uint64, error) {
	cursor, err := sqlcgen.New(w.db).GetScanCursor(ctx, sqlcgen.GetScanCursorParams{
		TenantID: w.cfg.Tenant, ChainID: w.cfg.ChainID,
	})
	// No cursor is a state, not a failure -- the scanner writes it on its
	// first pass, and reconciliation has its own clock. The deposit scanner
	// makes the same distinction for the same row.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNoCursor
	}
	if err != nil {
		return nil, fmt.Errorf("reconcile: scan cursor: %w", err)
	}
	scanned := uint64(cursor.LastScannedBlock) //nolint:gosec // CHECKed >= 0
	out := make(map[string]uint64, len(assets))
	for _, a := range assets {
		required := uint64(w.required(a)) //nolint:gosec // positive by construction
		if head+1 < required {
			return nil, fmt.Errorf("reconcile: head %d is below %s's %d confirmations", head, a.Symbol, required)
		}
		b := head - required + 1
		if scanned < b {
			b = scanned
		}
		out[a.Symbol] = b
	}
	return out, nil
}

// requiredFor is one asset's confirmation count, looked up by symbol.
func (w *Worker) requiredFor(assets []registry.Asset, symbol string) int32 {
	for _, a := range assets {
		if a.Symbol == symbol {
			return w.required(a)
		}
	}
	if w.cfg.DefaultConfirmations > 0 {
		return w.cfg.DefaultConfirmations
	}
	return 1
}

func (w *Worker) required(a registry.Asset) int32 {
	if a.RequiredConfirmations > 0 {
		return a.RequiredConfirmations
	}
	if w.cfg.DefaultConfirmations > 0 {
		return w.cfg.DefaultConfirmations
	}
	return 1
}

// held is one asset's on-chain side.
type held struct {
	total money.Amount
	// hot is the hot wallet's share of it, kept for the low-balance alert.
	hot money.Amount
}

// readBalances reads every address's balance of every asset, each pinned to
// that asset's frontier, and refuses the answer if the chain moved meanwhile.
//
// BalanceAt takes a block number, not a hash, so a reorg between the first
// read and the last would blend two chains without saying so. Bracketing the
// set with the frontier blocks' hashes is one extra call per distinct height
// and turns a silent wrong answer into a skipped tick.
func (w *Worker) readBalances(
	ctx context.Context, assets []registry.Asset, frontiers map[string]uint64,
	addresses []common.Address, hot common.Address,
) (map[string]held, error) {
	before, err := w.frontierHashes(ctx, frontiers)
	if err != nil {
		return nil, err
	}
	out := make(map[string]held, len(assets))
	for _, a := range assets {
		block := new(big.Int).SetUint64(frontiers[a.Symbol])
		total := money.Zero
		for _, addr := range addresses {
			v, err := w.balanceOf(ctx, a, addr, block)
			if err != nil {
				return nil, err
			}
			total = total.Add(v)
		}
		hotHeld, err := w.balanceOf(ctx, a, hot, block)
		if err != nil {
			return nil, err
		}
		out[a.Symbol] = held{total: total.Add(hotHeld), hot: hotHeld}
	}
	after, err := w.frontierHashes(ctx, frontiers)
	if err != nil {
		return nil, err
	}
	for number, hash := range before {
		if after[number] != hash {
			return nil, fmt.Errorf("%w: block %d was %s and is now %s", errChainMoved, number, hash, after[number])
		}
	}
	return out, nil
}

func (w *Worker) frontierHashes(ctx context.Context, frontiers map[string]uint64) (map[uint64]string, error) {
	out := make(map[uint64]string, len(frontiers))
	for _, number := range frontiers {
		if _, seen := out[number]; seen {
			continue
		}
		block, err := w.chain.BlockByNumber(ctx, number)
		if err != nil {
			return nil, fmt.Errorf("reconcile: block %d: %w", number, err)
		}
		out[number] = block.Hash
	}
	return out, nil
}

// balanceOf is one address's holding of one asset, as an Amount.
//
// An unreadable balance fails the pass rather than counting as zero. The
// sweeper can afford to skip an asset it cannot read -- it just collects
// nothing -- but a reconciler that read a misconfigured contract as zero would
// report the whole of that asset as missing.
func (w *Worker) balanceOf(ctx context.Context, a registry.Asset, addr common.Address, block *big.Int) (money.Amount, error) {
	if a.IsNative {
		wei, err := w.chain.BalanceAt(ctx, addr, block)
		if err != nil {
			return money.Zero, fmt.Errorf("reconcile: %w", err)
		}
		return evm.FromWei(wei, a.Scale)
	}
	if a.ContractAddress == nil {
		return money.Zero, fmt.Errorf("reconcile: %s has no contract address", a.Symbol)
	}
	units, err := w.chain.TokenBalanceAt(ctx, common.HexToAddress(*a.ContractAddress), addr, block)
	if err != nil {
		return money.Zero, fmt.Errorf("reconcile: %w", err)
	}
	return evm.FromWei(units, a.Scale)
}

// ledgerSide is everything read from the database in one snapshot.
type ledgerSide struct {
	custody    map[string]money.Amount
	uncredited map[string]money.Amount
	// above is the net effect the ledger booked above each asset's frontier.
	above map[string]money.Amount
	open  []sqlcgen.ListOpenChainWorkRow
}

// readLedger takes every database figure in one REPEATABLE READ transaction.
//
// It has to be one snapshot. Crediting a deposit moves it out of the pending
// set and into custody atomically; reading the two sides either side of that
// commit would show the amount in neither -- or in both -- and either way the
// report would name a break that does not exist. The transaction is read-only
// and short, so the isolation costs nothing worth counting.
func (w *Worker) readLedger(ctx context.Context, assets []registry.Asset, frontiers map[string]uint64) (ledgerSide, error) {
	out := ledgerSide{
		custody:    map[string]money.Amount{},
		uncredited: map[string]money.Amount{},
		above:      map[string]money.Amount{},
	}
	tx, err := w.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ledgerSide{}, fmt.Errorf("reconcile: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	custody, err := w.ledger.HouseBalancesTx(ctx, tx)
	if err != nil {
		return ledgerSide{}, err
	}
	for _, a := range assets {
		// A house account with no postings yet has no row at all, so every
		// asset starts at zero rather than being missing.
		out.custody[a.Symbol] = money.Zero
		out.uncredited[a.Symbol] = money.Zero
		out.above[a.Symbol] = money.Zero
	}
	for _, b := range custody {
		if b.Code != ledger.HouseCustodyHot && b.Code != ledger.HouseCustodyDepositAddresses {
			continue
		}
		if _, wanted := out.custody[b.Asset]; !wanted {
			continue
		}
		out.custody[b.Asset] = out.custody[b.Asset].Add(b.Balance)
	}

	for _, a := range assets {
		block := int64(frontiers[a.Symbol]) //nolint:gosec // block numbers are far below 2^63
		uncredited, err := q.SumUncreditedDeposits(ctx, sqlcgen.SumUncreditedDepositsParams{
			TenantID: w.cfg.Tenant, ChainID: w.cfg.ChainID, Asset: a.Symbol, BlockNumber: block,
		})
		if err != nil {
			return ledgerSide{}, fmt.Errorf("reconcile: uncredited %s: %w", a.Symbol, err)
		}
		if out.uncredited[a.Symbol], err = pg.AmountFromNumeric(uncredited); err != nil {
			return ledgerSide{}, err
		}

		above, err := w.aboveFrontier(ctx, q, a, block)
		if err != nil {
			return ledgerSide{}, err
		}
		out.above[a.Symbol] = above
	}

	if out.open, err = q.ListOpenChainWork(ctx, sqlcgen.ListOpenChainWorkParams{
		TenantID: w.cfg.Tenant, ChainID: w.cfg.ChainID,
	}); err != nil {
		return ledgerSide{}, fmt.Errorf("reconcile: open work: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ledgerSide{}, fmt.Errorf("reconcile: commit: %w", err)
	}
	return out, nil
}

// aboveFrontier is the net change the ledger applied to this asset's custody
// for transactions mined above the frontier.
//
// Credits raise it, withdrawals and gas lower it. Gas lands on the native
// asset's line whatever the transaction moved, which is why a token line never
// carries any.
func (w *Worker) aboveFrontier(ctx context.Context, q *sqlcgen.Queries, a registry.Asset, block int64) (money.Amount, error) {
	credited, err := q.SumCreditedDepositsAbove(ctx, sqlcgen.SumCreditedDepositsAboveParams{
		TenantID: w.cfg.Tenant, ChainID: w.cfg.ChainID, Asset: a.Symbol, BlockNumber: block,
	})
	if err != nil {
		return money.Zero, fmt.Errorf("reconcile: credited above %s: %w", a.Symbol, err)
	}
	withdrawn, err := q.SumConfirmedWithdrawalsAbove(ctx, sqlcgen.SumConfirmedWithdrawalsAboveParams{
		TenantID: w.cfg.Tenant, ChainID: w.cfg.ChainID, Asset: a.Symbol, BlockNumber: &block,
	})
	if err != nil {
		return money.Zero, fmt.Errorf("reconcile: withdrawn above %s: %w", a.Symbol, err)
	}
	in, err := pg.AmountFromNumeric(credited)
	if err != nil {
		return money.Zero, err
	}
	out, err := pg.AmountFromNumeric(withdrawn)
	if err != nil {
		return money.Zero, err
	}
	total := in.Sub(out)
	if a.Symbol != w.cfg.NativeAsset {
		return total, nil
	}
	gas, err := q.SumGasBookedAbove(ctx, sqlcgen.SumGasBookedAboveParams{
		TenantID: w.cfg.Tenant, ChainID: w.cfg.ChainID, AboveBlock: block,
	})
	if err != nil {
		return money.Zero, fmt.Errorf("reconcile: gas above: %w", err)
	}
	burned, err := pg.AmountFromNumeric(gas)
	if err != nil {
		return money.Zero, err
	}
	return total.Sub(burned), nil
}

// inFlight is what the chain has already spent at or below each asset's
// frontier that the ledger has not booked.
//
// Every open transaction is asked for its receipt -- the same call the worker
// that owns it will make. The row cannot answer instead: the cost is written
// only when it is booked, and no gas limit is stored to bound it with, so any
// figure derived from the row would be an estimate. This comparison has no
// tolerance to absorb one.
//
// A withdrawal is the only kind that takes value out of the addresses being
// counted. A sweep, a gas funding and a nonce fill each move it between two
// addresses that are both inside the total, so all they cost is gas.
func (w *Worker) inFlight(ctx context.Context, open []sqlcgen.ListOpenChainWorkRow, frontiers map[string]uint64) (map[string]money.Amount, error) {
	out := make(map[string]money.Amount, len(frontiers))
	for asset := range frontiers {
		out[asset] = money.Zero
	}
	native := w.cfg.NativeAsset
	for _, row := range open {
		if row.TxHash == nil {
			continue
		}
		receipt, err := w.chain.Receipt(ctx, *row.TxHash)
		if errors.Is(err, evm.ErrNotFound) {
			continue // not mined: the chain still holds it, and so does the ledger
		}
		if err != nil {
			return nil, fmt.Errorf("reconcile: receipt of %s: %w", *row.TxHash, err)
		}
		mined := receipt.BlockNumber.Uint64()
		gas := gasCost(receipt)
		if _, tracked := out[native]; tracked && mined <= frontiers[native] {
			out[native] = out[native].Add(gas)
		}
		if row.Kind != "withdrawal" {
			continue
		}
		if _, tracked := out[row.Asset]; !tracked || mined > frontiers[row.Asset] {
			continue
		}
		amount, err := pg.AmountFromNumeric(row.Amount)
		if err != nil {
			return nil, err
		}
		out[row.Asset] = out[row.Asset].Add(amount)
	}
	return out, nil
}

func (w *Worker) line(a registry.Asset, block uint64, chain held, book ledgerSide, flight map[string]money.Amount) Line {
	l := Line{
		Asset:         a.Symbol,
		BlockHeight:   int64(block), //nolint:gosec // block numbers are far below 2^63
		LedgerTotal:   book.custody[a.Symbol],
		ChainTotal:    chain.total,
		Uncredited:    book.uncredited[a.Symbol],
		AboveFrontier: book.above[a.Symbol],
		InFlight:      flight[a.Symbol],
	}
	l.Diff = l.ChainTotal.Sub(l.LedgerTotal).Add(l.AboveFrontier).Sub(l.Uncredited).Add(l.InFlight)
	return l
}

// record writes the pass, its breaks and the events they warrant, in one
// transaction. A break that is not recorded and not announced is the same as
// not having looked.
func (w *Worker) record(ctx context.Context, started time.Time, lines []Line, hotBalance money.Amount) error {
	previous, err := w.previousBreaks(ctx)
	if err != nil {
		return err
	}
	balanced := true
	for _, l := range lines {
		if l.Broken() {
			balanced = false
		}
		w.metrics.observe(l)
	}
	body, err := json.Marshal(lines)
	if err != nil {
		return fmt.Errorf("reconcile: marshal lines: %w", err)
	}
	finished := time.Now().UTC()
	return inTx(ctx, w.db, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		report, err := q.InsertReconciliationReport(ctx, sqlcgen.InsertReconciliationReportParams{
			TenantID: w.cfg.Tenant, ChainID: w.cfg.ChainID,
			StartedAt: started, FinishedAt: finished, Balanced: balanced, Lines: body,
		})
		if err != nil {
			return fmt.Errorf("reconcile: insert report: %w", err)
		}
		breaks := 0
		for _, l := range lines {
			if !l.Broken() {
				continue
			}
			breaks++
			if _, err := q.InsertReconciliationBreak(ctx, breakParams(w.cfg, report.ID, l)); err != nil {
				return fmt.Errorf("reconcile: insert break %s: %w", l.Asset, err)
			}
			// Quiet unless something changed. A break that stays open is real
			// and stays visible in the report and the metric; announcing it
			// again every few minutes only teaches people to filter it out.
			if prior, seen := previous[l.Asset]; seen && prior.Equal(l.Diff) {
				continue
			}
			if err := w.emitBreak(ctx, tx, report.ID, l); err != nil {
				return err
			}
		}
		w.metrics.observeBreaks(breaks)
		if !balanced {
			w.log.Error("reconciliation found a break",
				slog.Int("assets", breaks), slog.String("report_id", report.ID))
		}
		return w.checkHotWallet(ctx, tx, hotBalance)
	})
}

func breakParams(cfg Config, reportID string, l Line) sqlcgen.InsertReconciliationBreakParams {
	return sqlcgen.InsertReconciliationBreakParams{
		TenantID: cfg.Tenant, ReportID: reportID, ChainID: cfg.ChainID,
		Asset: l.Asset, BlockHeight: l.BlockHeight,
		LedgerTotal: pg.NumericFromAmount(l.LedgerTotal), ChainTotal: pg.NumericFromAmount(l.ChainTotal),
		Uncredited: pg.NumericFromAmount(l.Uncredited), AboveFrontier: pg.NumericFromAmount(l.AboveFrontier),
		InFlight: pg.NumericFromAmount(l.InFlight), Diff: pg.NumericFromAmount(l.Diff),
	}
}

// previousBreaks is what the last pass found, keyed by asset. Used only to
// decide whether a break is news.
func (w *Worker) previousBreaks(ctx context.Context) (map[string]money.Amount, error) {
	q := sqlcgen.New(w.db)
	report, err := q.GetLatestReconciliationReport(ctx, sqlcgen.GetLatestReconciliationReportParams{
		TenantID: w.cfg.Tenant, ChainID: w.cfg.ChainID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string]money.Amount{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reconcile: previous report: %w", err)
	}
	rows, err := q.ListReconciliationBreaks(ctx, sqlcgen.ListReconciliationBreaksParams{
		TenantID: w.cfg.Tenant, ReportID: report.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile: previous breaks: %w", err)
	}
	out := make(map[string]money.Amount, len(rows))
	for _, r := range rows {
		diff, err := pg.AmountFromNumeric(r.Diff)
		if err != nil {
			return nil, err
		}
		out[r.Asset] = diff
	}
	return out, nil
}

func inTx(ctx context.Context, db *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("reconcile: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
