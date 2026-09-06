package withdrawal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/signer"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/policy"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// DailyWindow is the span the daily limit is measured over. §6.4.2 says
// "daily"; a rolling 24 h window is used rather than a calendar day because a
// calendar boundary lets an account withdraw two days' worth in two minutes
// either side of midnight.
const DailyWindow = 24 * time.Hour

// Config parameterises the worker.
type Config struct {
	Tenant string
	// Batch is how many withdrawals one tick claims.
	Batch int32
	// NativeAsset is the symbol gas is denominated in. Gas is always paid in
	// the chain's own coin, whatever the withdrawal moved, so a USDC
	// withdrawal still books its gas here.
	NativeAsset string
}

// KYCReader supplies the account's KYC level. It is an interface so the chain
// role does not depend on the whole auth service for one integer.
type KYCReader interface {
	// KYCLevelOf returns the KYC level of the account's owner.
	KYCLevelOf(ctx context.Context, accountID string) (int16, error)
}

// LimitReader supplies the configured withdrawal ceilings. Kept apart from
// registry.Reader so the in-memory registry cache the engine uses does not
// have to grow a method only the withdrawal worker calls.
type LimitReader interface {
	GetWithdrawalLimit(ctx context.Context, tenantID, symbol string, kycLevel int16) (registry.WithdrawalLimit, error)
}

// Worker moves withdrawals from requested to funds_locked (§6.4.2). It runs in
// the chain role and holds no keys: the signer picks the row up afterwards.
type Worker struct {
	db      *pgxpool.Pool
	cfg     Config
	reg     registry.Reader
	limits  LimitReader
	ledger  *ledger.Service
	kyc     KYCReader
	policy  policy.WithdrawalPolicy
	audit   *audit.Recorder
	log     *slog.Logger
	metrics *Metrics

	// The send half. All nil in a deployment with no node configured, which
	// leaves the policy half running on its own: deciding withdrawals and
	// locking their funds needs no chain at all.
	chain  Chain
	signer signer.Signer
	nonces Nonces
	send   SendConfig
}

// NewWorker builds the chain-side worker.
func NewWorker(db *pgxpool.Pool, cfg Config, reg registry.Reader, limits LimitReader, l *ledger.Service, kyc KYCReader, p policy.WithdrawalPolicy, rec *audit.Recorder, log *slog.Logger) *Worker {
	if cfg.Batch <= 0 {
		cfg.Batch = 50
	}
	if cfg.NativeAsset == "" {
		cfg.NativeAsset = "ETH"
	}
	return &Worker{db: db, cfg: cfg, reg: reg, limits: limits, ledger: l, kyc: kyc, policy: p, audit: rec, log: log, metrics: NewMetrics(nil)}
}

// WithSending gives the worker what it needs to sign, broadcast and track.
// Without it Send does nothing and withdrawals stop at funds_locked, which is
// exactly what a deployment without a node or a signer should do.
func (w *Worker) WithSending(c Chain, s signer.Signer, n Nonces, cfg SendConfig) *Worker {
	if cfg.ReplaceAfter <= 0 {
		cfg.ReplaceAfter = time.Minute
	}
	if cfg.MaxReplacements <= 0 {
		cfg.MaxReplacements = 3
	}
	w.chain, w.signer, w.nonces, w.send = c, s, n, cfg
	return w
}

// WithMetrics attaches Prometheus collectors.
func (w *Worker) WithMetrics(m *Metrics) *Worker {
	if m != nil {
		w.metrics = m
	}
	return w
}

// Tick advances every withdrawal that is waiting on this worker: a requested
// one gets a policy decision, an approved one gets its funds locked.
//
// Each withdrawal is its own transaction. One that fails does not hold up the
// rest, and a crash mid-batch leaves the others exactly where they were —
// which is the second iron rule of §6.4.2, that every state is committed
// before the next step runs.
func (w *Worker) Tick(ctx context.Context) error {
	rows, err := sqlcgen.New(w.db).ClaimWithdrawals(ctx, sqlcgen.ClaimWithdrawalsParams{
		TenantID: w.cfg.Tenant,
		Statuses: []string{StatusRequested, StatusAutoApproved, StatusApproved},
		Limit:    w.cfg.Batch,
	})
	if err != nil {
		return fmt.Errorf("withdrawal: claim: %w", err)
	}
	var firstErr error
	for _, row := range rows {
		if err := w.advance(ctx, row.ID); err != nil {
			// Keep going: one poisoned withdrawal must not stop the queue.
			w.log.Error("withdrawal step failed",
				slog.String("withdrawal_id", row.ID), slog.String("err", err.Error()))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if err := w.observeQueue(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// observeQueue publishes how many withdrawals are waiting for a person.
func (w *Worker) observeQueue(ctx context.Context) error {
	n, err := sqlcgen.New(w.db).CountWithdrawalsByStatus(ctx, sqlcgen.CountWithdrawalsByStatusParams{
		TenantID: w.cfg.Tenant, Statuses: []string{StatusPendingReview},
	})
	if err != nil {
		return fmt.Errorf("withdrawal: count pending review: %w", err)
	}
	w.metrics.observePending(int(n))
	return nil
}

// advance runs one step of one withdrawal under a row lock.
func (w *Worker) advance(ctx context.Context, id string) error {
	return inTx(ctx, w.db, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		row, err := q.GetWithdrawalForUpdate(ctx, sqlcgen.GetWithdrawalForUpdateParams{TenantID: w.cfg.Tenant, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // vanished between the claim and the lock
		}
		if err != nil {
			return fmt.Errorf("withdrawal: lock %s: %w", id, err)
		}
		switch row.Status {
		case StatusRequested:
			return w.decide(ctx, tx, row)
		case StatusAutoApproved, StatusApproved:
			return w.lockFunds(ctx, tx, row)
		default:
			// Someone else moved it between the claim and the lock.
			return nil
		}
	})
}

// decide runs the policy check (§6.4.2 requested -> policy_check -> ...).
func (w *Worker) decide(ctx context.Context, tx pgx.Tx, row sqlcgen.ChainWithdrawal) error {
	q := sqlcgen.New(tx)
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return fmt.Errorf("withdrawal: amount of %s: %w", row.ID, err)
	}
	asset, err := w.reg.GetAsset(ctx, w.cfg.Tenant, row.Asset)
	if err != nil {
		return fmt.Errorf("withdrawal: asset %s: %w", row.Asset, err)
	}
	account, err := w.ledger.Account(ctx, row.AccountID)
	if err != nil {
		return fmt.Errorf("withdrawal: account %s: %w", row.AccountID, err)
	}
	level, err := w.kyc.KYCLevelOf(ctx, row.AccountID)
	if err != nil {
		return fmt.Errorf("withdrawal: kyc level of %s: %w", row.AccountID, err)
	}
	limit, err := w.limitFor(ctx, row.Asset, level)
	if err != nil {
		return err
	}
	// The rolling total excludes this withdrawal, which is still `requested`
	// and therefore already counted by SumWithdrawnSince — subtract it back
	// out so the policy is not told the request happened twice.
	committed, err := w.withdrawnSince(ctx, tx, row.AccountID, row.Asset, time.Now().UTC().Add(-DailyWindow))
	if err != nil {
		return err
	}
	decision, reason := w.policy.Withdraw(policy.WithdrawalRequest{
		Asset: asset, Account: account, KYCLevel: level, Amount: amount,
		Limit: limit, WithdrawnToday: committed.Sub(amount),
	})

	next := string(decision)
	var failure *string
	if decision == policy.DecisionReject {
		failure = optString(FailurePolicy)
	}
	updated, err := q.UpdateWithdrawalStatus(ctx, sqlcgen.UpdateWithdrawalStatusParams{
		TenantID: w.cfg.Tenant, ID: row.ID, Status: next, FailureReason: failure,
	})
	if err != nil {
		return fmt.Errorf("withdrawal: decide %s: %w", row.ID, err)
	}
	if err := w.audit.Record(ctx, tx, audit.Event{
		ActorType: audit.ActorSystem, ActorID: "chain", Action: "withdrawal.policy_check",
		TargetType: "withdrawal", TargetID: row.ID,
		Before:        map[string]any{"status": row.Status},
		After:         map[string]any{"status": next, "reason": string(reason), "kyc_level": level},
		CorrelationID: deref(row.CorrelationID),
	}); err != nil {
		return fmt.Errorf("withdrawal: audit: %w", err)
	}
	w.log.Info("withdrawal decided",
		slog.String("withdrawal_id", row.ID), slog.String("decision", next),
		slog.String("reason", string(reason)), slog.String("asset", row.Asset),
		slog.String("amount", amount.String()))
	w.metrics.decided.WithLabelValues(row.Asset, next).Inc()
	return emit(ctx, tx, w.ledger, w.cfg.Tenant, EventStateChanged, updated, row.Status, string(reason))
}

// lockFunds moves available -> hold (§6.4.2 approved -> funds_locked).
//
// Nothing may be signed before this succeeds, which is why the hold and the
// state change are one transaction: a hold without the state would lock a
// user's funds for a withdrawal that never proceeds, and a state without the
// hold would let the signer spend money the ledger still says is available.
func (w *Worker) lockFunds(ctx context.Context, tx pgx.Tx, row sqlcgen.ChainWithdrawal) error {
	q := sqlcgen.New(tx)
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return fmt.Errorf("withdrawal: amount of %s: %w", row.ID, err)
	}
	entry, _, err := w.ledger.Hold(ctx, tx, ledger.HoldParams{
		AccountID: row.AccountID, Asset: row.Asset, Amount: amount,
		IdempotencyKey: "hold:withdrawal:" + row.ID,
		Ref:            ledger.Ref{Type: "withdrawal", ID: row.ID},
		CorrelationID:  deref(row.CorrelationID),
	})
	if errors.Is(err, ledger.ErrInsufficient) {
		return w.fail(ctx, tx, row, FailureInsufficientBalance)
	}
	if err != nil {
		return fmt.Errorf("withdrawal: hold %s: %w", row.ID, err)
	}
	updated, err := q.LockWithdrawalFunds(ctx, sqlcgen.LockWithdrawalFundsParams{
		TenantID: w.cfg.Tenant, ID: row.ID, HoldEntryID: &entry.ID,
	})
	if err != nil {
		return fmt.Errorf("withdrawal: lock funds %s: %w", row.ID, err)
	}
	if err := w.audit.Record(ctx, tx, audit.Event{
		ActorType: audit.ActorSystem, ActorID: "chain", Action: "withdrawal.funds_locked",
		TargetType: "withdrawal", TargetID: row.ID,
		Before:        map[string]any{"status": row.Status},
		After:         map[string]any{"status": StatusFundsLocked, "hold_entry_id": entry.ID},
		CorrelationID: deref(row.CorrelationID),
	}); err != nil {
		return fmt.Errorf("withdrawal: audit: %w", err)
	}
	w.log.Info("withdrawal funds locked",
		slog.String("withdrawal_id", row.ID), slog.String("asset", row.Asset),
		slog.String("amount", amount.String()), slog.Int64("hold_entry_id", entry.ID))
	w.metrics.decided.WithLabelValues(row.Asset, StatusFundsLocked).Inc()
	return emit(ctx, tx, w.ledger, w.cfg.Tenant, EventStateChanged, updated, row.Status, "")
}

// fail records a terminal failure with its reason.
func (w *Worker) fail(ctx context.Context, tx pgx.Tx, row sqlcgen.ChainWithdrawal, reason string) error {
	updated, err := sqlcgen.New(tx).UpdateWithdrawalStatus(ctx, sqlcgen.UpdateWithdrawalStatusParams{
		TenantID: w.cfg.Tenant, ID: row.ID, Status: StatusFailed, FailureReason: &reason,
	})
	if err != nil {
		return fmt.Errorf("withdrawal: fail %s: %w", row.ID, err)
	}
	if err := w.audit.Record(ctx, tx, audit.Event{
		ActorType: audit.ActorSystem, ActorID: "chain", Action: "withdrawal.failed",
		TargetType: "withdrawal", TargetID: row.ID,
		Before:        map[string]any{"status": row.Status},
		After:         map[string]any{"status": StatusFailed, "failure_reason": reason},
		CorrelationID: deref(row.CorrelationID),
	}); err != nil {
		return fmt.Errorf("withdrawal: audit: %w", err)
	}
	w.log.Warn("withdrawal failed",
		slog.String("withdrawal_id", row.ID), slog.String("reason", reason))
	w.metrics.decided.WithLabelValues(row.Asset, StatusFailed).Inc()
	return emit(ctx, tx, w.ledger, w.cfg.Tenant, EventStateChanged, updated, row.Status, reason)
}

// limitFor reads the configured ceiling, or nil when there is none.
func (w *Worker) limitFor(ctx context.Context, asset string, level int16) (*registry.WithdrawalLimit, error) {
	limit, err := w.limits.GetWithdrawalLimit(ctx, w.cfg.Tenant, asset, level)
	if errors.Is(err, registry.ErrNotFound) {
		return nil, nil //nolint:nilnil // "no row" is a value the policy handles, not an error
	}
	if err != nil {
		return nil, fmt.Errorf("withdrawal: limit for %s/%d: %w", asset, level, err)
	}
	return &limit, nil
}

func (w *Worker) withdrawnSince(ctx context.Context, tx pgx.Tx, accountID, asset string, since time.Time) (money.Amount, error) {
	total, err := sqlcgen.New(tx).SumWithdrawnSince(ctx, sqlcgen.SumWithdrawnSinceParams{
		TenantID: w.cfg.Tenant, AccountID: accountID, Asset: asset, CreatedAt: since,
	})
	if err != nil {
		return money.Zero, fmt.Errorf("withdrawal: daily total for %s: %w", accountID, err)
	}
	return pg.AmountFromNumeric(total)
}

// PostgresKYC reads the KYC level of the user behind an account.
type PostgresKYC struct{ db *pgxpool.Pool }

// NewPostgresKYC wraps a pool as a KYCReader.
func NewPostgresKYC(db *pgxpool.Pool) PostgresKYC { return PostgresKYC{db: db} }

// KYCLevelOf implements KYCReader. An account with no user behind it — a house
// account, which cannot withdraw anyway — reads as level 0, the level with the
// tightest limits, so the fallback is the conservative one.
func (k PostgresKYC) KYCLevelOf(ctx context.Context, accountID string) (int16, error) {
	level, err := sqlcgen.New(k.db).GetAccountKYCLevel(ctx, accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("withdrawal: kyc level of %s: %w", accountID, err)
	}
	return level, nil
}
