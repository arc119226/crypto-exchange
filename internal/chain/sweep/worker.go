package sweep

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/signer"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// Worker plans and runs sweeps. It lives in the chain role, next to the
// withdrawal worker and sharing its node, signer and nonce allocator.
type Worker struct {
	db      *pgxpool.Pool
	cfg     Config
	reg     registry.Reader
	ledger  *ledger.Service
	chain   Chain
	signer  signer.Signer
	nonces  Nonces
	audit   *audit.Recorder
	log     *slog.Logger
	metrics *Metrics
}

// New builds the sweeper.
func New(db *pgxpool.Pool, cfg Config, reg registry.Reader, l *ledger.Service, c Chain, s signer.Signer, n Nonces, rec *audit.Recorder, log *slog.Logger) *Worker {
	if cfg.Batch <= 0 {
		cfg.Batch = 25
	}
	if cfg.NativeAsset == "" {
		cfg.NativeAsset = "ETH"
	}
	return &Worker{
		db: db, cfg: cfg, reg: reg, ledger: l, chain: c, signer: s, nonces: n,
		audit: rec, log: log, metrics: NewMetrics(nil),
	}
}

// WithMetrics attaches Prometheus collectors.
func (w *Worker) WithMetrics(m *Metrics) *Worker {
	if m != nil {
		w.metrics = m
	}
	return w
}

// Tick plans new sweeps and advances the ones already running.
//
// Planning first would let a freshly planned sweep be signed in the same tick;
// advancing first keeps each state one tick apart, which is the same
// "committed before the next step runs" discipline §6.4.2 asks of withdrawals
// and which makes a restart at any point resume from the row.
func (w *Worker) Tick(ctx context.Context) error {
	var firstErr error
	if err := w.advanceAll(ctx); err != nil {
		firstErr = err
	}
	if err := w.plan(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// advanceAll moves every sweep that is waiting on this worker.
func (w *Worker) advanceAll(ctx context.Context) error {
	rows, err := sqlcgen.New(w.db).ClaimSweeps(ctx, sqlcgen.ClaimSweepsParams{
		TenantID: w.cfg.Tenant,
		Statuses: []string{StatusRequested, StatusGasFunded, StatusBroadcast},
		Limit:    w.cfg.Batch,
	})
	if err != nil {
		return fmt.Errorf("sweep: claim: %w", err)
	}
	var firstErr error
	for _, row := range rows {
		if err := w.advance(ctx, row.ID); err != nil {
			// Keep going: one address that cannot be emptied must not stop the
			// rest, and every sweep is independent of every other.
			w.log.Error("sweep step failed",
				slog.String("sweep_id", row.ID), slog.String("address", row.FromAddress),
				slog.String("asset", row.Asset), slog.String("err", err.Error()))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (w *Worker) advance(ctx context.Context, id string) error {
	row, err := sqlcgen.New(w.db).GetSweep(ctx, sqlcgen.GetSweepParams{TenantID: w.cfg.Tenant, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sweep: read %s: %w", id, err)
	}
	asset, err := w.reg.GetAsset(ctx, w.cfg.Tenant, row.Asset)
	if err != nil {
		return fmt.Errorf("sweep: asset %s: %w", row.Asset, err)
	}
	switch row.Status {
	case StatusRequested:
		// A token sweep needs the address funded before it can pay for its own
		// transfer; a native one is already holding what it will spend.
		if !asset.IsNative && row.GasFundingTxHash == nil {
			return w.fundGas(ctx, row, asset)
		}
		if !asset.IsNative {
			return w.trackGasFunding(ctx, row)
		}
		return w.send(ctx, row, asset)
	case StatusGasFunded:
		return w.send(ctx, row, asset)
	case StatusBroadcast:
		return w.track(ctx, row, asset)
	default:
		return nil
	}
}

// plan looks for addresses worth emptying and records a sweep for each.
//
// Two rules decide what is sweepable, and both exist to keep
// custody:deposit_addresses honest rather than to be conservative for its own
// sake (§6.1.4 f):
//
//   - An address with a deposit the ledger has not credited yet is skipped
//     entirely. Sweeping it would move money the ledger is about to record
//     arriving, and the two would cross.
//   - What is swept is capped at what the ledger was actually credited for.
//     The chain balance can legitimately be higher — an internal contract
//     transfer the scanner does not see, which §4 puts out of scope without
//     preventing — and sweeping that excess would credit
//     custody:deposit_addresses for a transfer it never received.
func (w *Worker) plan(ctx context.Context) error {
	assets, err := w.reg.ListAssets(ctx, w.cfg.Tenant)
	if err != nil {
		return fmt.Errorf("sweep: list assets: %w", err)
	}
	eligible, err := w.eligibleAddresses(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	// Assets outside, addresses inside. The order matters: whether an asset
	// can be read at all is a property of the asset, so finding out once per
	// tick is both cheaper and the only way to skip it as a unit.
	for _, asset := range assets {
		if asset.ChainID != w.cfg.ChainID || asset.Status != registry.AssetActive {
			continue
		}
		if !asset.SweepThreshold.IsPositive() {
			// A zero threshold would sweep an address holding one wei, paying
			// more gas than it recovers, on every tick forever.
			continue
		}
		for _, addr := range eligible {
			err := w.planOne(ctx, addr, asset)
			if err == nil {
				continue
			}
			// An asset whose balance cannot be read is a registry problem, not
			// a transient one: the contract address is wrong, or names
			// something that is not a token on this chain. The sweeper does
			// not know how much is there, so it must not sweep it -- but
			// letting one bad row stop the assets that *can* be read would
			// turn a misconfigured token into a hot wallet that slowly runs
			// dry while every tick reports a failure that is not the real
			// problem. Skip the asset for this tick, loudly, and carry on.
			if errors.Is(err, errUnreadable) {
				w.log.Error("cannot read this asset's balance, so it is not being collected",
					slog.String("asset", asset.Symbol), slog.String("err", err.Error()))
				w.metrics.unreadable.WithLabelValues(asset.Symbol).Inc()
				break
			}
			w.log.Error("could not plan a sweep",
				slog.String("address", addr.Address), slog.String("asset", asset.Symbol),
				slog.String("err", err.Error()))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// eligibleAddresses is the set worth looking at this tick: assigned, and with
// nothing the ledger has yet to credit. Both are asset-independent, so they
// are decided once rather than once per asset.
func (w *Worker) eligibleAddresses(ctx context.Context) ([]sqlcgen.ChainDepositAddress, error) {
	q := sqlcgen.New(w.db)
	addresses, err := q.ListDepositAddresses(ctx, sqlcgen.ListDepositAddressesParams{
		TenantID: w.cfg.Tenant, ChainID: w.cfg.ChainID,
	})
	if err != nil {
		return nil, fmt.Errorf("sweep: list addresses: %w", err)
	}
	out := make([]sqlcgen.ChainDepositAddress, 0, len(addresses))
	for _, addr := range addresses {
		// A free pool slot has never been handed out, so nothing can have been
		// deposited to it.
		if addr.AccountID == nil {
			continue
		}
		unsettled, err := q.CountUnsettledDeposits(ctx, sqlcgen.CountUnsettledDepositsParams{
			TenantID: w.cfg.Tenant, ID: addr.ID,
		})
		if err != nil {
			return nil, fmt.Errorf("sweep: unsettled deposits of %s: %w", addr.Address, err)
		}
		if unsettled > 0 {
			continue
		}
		out = append(out, addr)
	}
	return out, nil
}

func (w *Worker) planOne(ctx context.Context, addr sqlcgen.ChainDepositAddress, asset registry.Asset) error {
	held, err := w.heldBy(ctx, addr.Address, asset)
	if err != nil {
		return fmt.Errorf("%w: %s on %s: %w", errUnreadable, asset.Symbol, addr.Address, err)
	}
	if held.Cmp(asset.SweepThreshold) < 0 {
		return nil
	}
	q := sqlcgen.New(w.db)
	credited, err := q.CreditedToAddress(ctx, sqlcgen.CreditedToAddressParams{
		TenantID: w.cfg.Tenant, ID: addr.ID, Asset: asset.Symbol,
	})
	if err != nil {
		return fmt.Errorf("sweep: credited to %s: %w", addr.Address, err)
	}
	swept, err := q.SweptFromAddress(ctx, sqlcgen.SweptFromAddressParams{
		TenantID: w.cfg.Tenant, AddressID: addr.ID, Asset: asset.Symbol,
		NativeAsset: w.cfg.NativeAsset,
	})
	if err != nil {
		return fmt.Errorf("sweep: already swept from %s: %w", addr.Address, err)
	}
	creditedAmount, err := pg.AmountFromNumeric(credited)
	if err != nil {
		return err
	}
	sweptAmount, err := pg.AmountFromNumeric(swept)
	if err != nil {
		return err
	}
	// The ceiling: what the ledger knows arrived, less what it knows has gone.
	ceiling := creditedAmount.Sub(sweptAmount)
	amount := held
	if ceiling.Cmp(amount) < 0 {
		amount = ceiling
	}
	if !amount.IsPositive() {
		return nil
	}
	// A native sweep must leave enough behind to pay for itself. Budget at the
	// fee *cap*, not the current price: a transaction that becomes unaffordable
	// when the base fee rises cannot be bumped, because bumping needs more
	// balance than the address has.
	if asset.IsNative {
		reserve, err := w.gasBudget(ctx, gasForNativeTransfer)
		if err != nil {
			return err
		}
		amount = amount.Sub(reserve)
		if !amount.IsPositive() || amount.Cmp(asset.SweepThreshold) < 0 {
			return nil
		}
	}
	row, err := q.InsertSweep(ctx, sqlcgen.InsertSweepParams{
		TenantID: w.cfg.Tenant, ChainID: w.cfg.ChainID, AddressID: addr.ID,
		FromAddress: addr.Address, Asset: asset.Symbol, Amount: pg.NumericFromAmount(amount),
	})
	if err != nil {
		// The partial unique index refuses a second sweep of an address this
		// one has not emptied yet. That is the index doing its job, not a
		// failure: the sweep already in flight will move the money.
		if isUniqueViolation(err) {
			return nil
		}
		return fmt.Errorf("sweep: record for %s: %w", addr.Address, err)
	}
	w.log.Info("sweep planned",
		slog.String("sweep_id", row.ID), slog.String("address", addr.Address),
		slog.String("asset", asset.Symbol), slog.String("amount", amount.String()),
		slog.String("held", held.String()))
	w.metrics.planned.WithLabelValues(asset.Symbol).Inc()
	return nil
}

// heldBy is the address's on-chain balance of one asset, as an Amount.
func (w *Worker) heldBy(ctx context.Context, addr string, asset registry.Asset) (money.Amount, error) {
	address := common.HexToAddress(addr)
	if asset.IsNative {
		wei, err := w.chain.Balance(ctx, address)
		if err != nil {
			return money.Zero, err
		}
		return evm.FromWei(wei, asset.Scale)
	}
	if asset.ContractAddress == nil {
		return money.Zero, fmt.Errorf("sweep: %s has no contract address", asset.Symbol)
	}
	units, err := w.chain.TokenBalance(ctx, common.HexToAddress(*asset.ContractAddress), address)
	if err != nil {
		return money.Zero, err
	}
	return evm.FromWei(units, asset.Scale)
}

// gasBudget is what a transaction of this size may cost at the current fee
// cap, in the native coin.
func (w *Worker) gasBudget(ctx context.Context, gas uint64) (money.Amount, error) {
	fees, err := w.fees(ctx)
	if err != nil {
		return money.Zero, err
	}
	wei := new(big.Int).Mul(new(big.Int).SetUint64(gas), fees.FeeCap)
	return evm.FromWei(wei, evm.MaxScale)
}

func (w *Worker) fees(ctx context.Context) (evm.Fees, error) {
	f, err := w.chain.SuggestFees(ctx)
	if err != nil {
		return evm.Fees{}, fmt.Errorf("sweep: fees: %w", err)
	}
	return f.CapAt(w.cfg.MaxFeePerGas), nil
}

// requiredConfirmations is the asset's threshold, or the configured default.
func (w *Worker) requiredConfirmations(asset registry.Asset) int32 {
	if asset.RequiredConfirmations > 0 {
		return asset.RequiredConfirmations
	}
	if w.cfg.DefaultConfirmations > 0 {
		return w.cfg.DefaultConfirmations
	}
	return 1
}

// confirmations reports how deep a receipt is, and whether it is deep enough.
func (w *Worker) confirmations(ctx context.Context, block uint64) (int32, error) {
	head, err := w.chain.Head(ctx)
	if err != nil {
		return 0, err
	}
	if head < block {
		return 0, nil // the node's head is behind its own receipt
	}
	return int32(head - block + 1), nil //nolint:gosec // bounded by the head
}

// Get returns one sweep.
func (w *Worker) Get(ctx context.Context, id string) (Record, error) {
	row, err := sqlcgen.New(w.db).GetSweep(ctx, sqlcgen.GetSweepParams{TenantID: w.cfg.Tenant, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("sweep: get %s: %w", id, err)
	}
	return recordFrom(row)
}

// List returns recent sweeps, newest first.
func List(ctx context.Context, db *pgxpool.Pool, tenant string, limit int32) ([]Record, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := sqlcgen.New(db).ListSweeps(ctx, sqlcgen.ListSweepsParams{TenantID: tenant, Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("sweep: list: %w", err)
	}
	return records(rows)
}

func inTx(ctx context.Context, db *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("sweep: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
