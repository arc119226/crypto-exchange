package withdrawal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/signer"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// Action is what an operator asks for on a withdrawal that needs a person
// (docs/plan-v1.0.md §6.4.2 resolve).
type Action string

// Resolutions.
const (
	// ActionBump sends the transaction again with a higher fee, past the
	// automatic replacement limit.
	ActionBump Action = "bump"
	// ActionCancelNonce displaces a stuck transaction with a self-transfer on
	// the same nonce. The refund happens when that displacement is mined.
	ActionCancelNonce Action = "cancel_nonce"
	// ActionRefund returns a reverted withdrawal's amount to the user.
	ActionRefund Action = "refund"
	// ActionRetry sends a reverted withdrawal back for another attempt.
	ActionRetry Action = "retry"
)

// ErrNotResolvable is an action on a withdrawal it does not apply to.
var ErrNotResolvable = errors.New("withdrawal: not resolvable that way")

// resolvable reports whether an action applies to a withdrawal in this state.
//
// Both sides of the split call it: the admin API to answer 409 while the
// operator is still looking at the screen, and the chain role again before
// acting, because the withdrawal may have moved on in between — a bump asked
// for on a broadcast withdrawal that confirmed a second later is no longer a
// legal thing to do, and the second check is the one that decides.
func resolvable(row sqlcgen.ChainWithdrawal, action Action) error {
	switch action {
	case ActionBump:
		if row.Status != StatusBroadcast {
			return fmt.Errorf("%w: %s is %s, so there is nothing in flight to bump", ErrNotResolvable, row.ID, row.Status)
		}
		// Bidding against our own cancellation would leave two transactions
		// competing for the nonce with nobody able to say which wins.
		if row.CancelTxHash != nil {
			return fmt.Errorf("%w: %s is already being cancelled", ErrNotResolvable, row.ID)
		}
	case ActionCancelNonce:
		if row.Status != StatusBroadcast {
			return fmt.Errorf("%w: %s is %s, so there is no nonce to displace", ErrNotResolvable, row.ID, row.Status)
		}
	case ActionRefund, ActionRetry:
		if row.Status != StatusFailed || deref(row.FailureReason) != FailureOnChain {
			return fmt.Errorf("%w: %s is %s/%s; refund and retry are for a transaction that reverted on chain",
				ErrNotResolvable, row.ID, row.Status, deref(row.FailureReason))
		}
	default:
		return fmt.Errorf("%w: unknown action %q", ErrNotResolvable, action)
	}
	return nil
}

// ResolveParams is one operator decision.
type ResolveParams struct {
	ID     string
	Action Action
	Note   string
	// ActorType and ActorID go to the audit trail.
	ActorType audit.ActorType
	ActorID   string
	IP        string
}

// Resolve applies an operator's decision to a withdrawal that the machine
// could not finish on its own (§6.4.2).
//
// Which actions are legal depends on where the withdrawal is stuck, and the
// two states differ in where the money sits. A broadcast withdrawal's amount
// is in pending_withdrawal and its transaction may still be mined, so bump and
// cancel_nonce act on the chain. A failed(on_chain) withdrawal's transaction
// is finished and its amount is still in pending_withdrawal, so refund and
// retry act on the ledger.
func (w *Worker) Resolve(ctx context.Context, p ResolveParams) (Record, error) {
	if w.chain == nil {
		return Record{}, fmt.Errorf("%w: this deployment has no chain connection", ErrNotResolvable)
	}
	row, err := sqlcgen.New(w.db).GetWithdrawal(ctx, sqlcgen.GetWithdrawalParams{TenantID: w.cfg.Tenant, ID: p.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("withdrawal: read %s: %w", p.ID, err)
	}
	if err := resolvable(row, p.Action); err != nil {
		return Record{}, err
	}
	switch p.Action {
	case ActionBump:
		err = w.replace(ctx, row, "operator bump: "+p.Note)
	case ActionCancelNonce:
		err = w.cancelNonce(ctx, row, p)
	case ActionRefund, ActionRetry:
		err = w.settleFailed(ctx, row, p)
	}
	if err != nil {
		return Record{}, err
	}
	return w.reload(ctx, p.ID)
}

// ApplyResolutions runs the resolutions operators have asked for.
//
// The admin role records the request and stops there: it has no node, no key
// and no grant on the transaction columns, so it can only ask. This is where
// the asking becomes doing, and it runs on the chain role's clock — which also
// means an operator's decision survives a restart of this process instead of
// being lost inside one HTTP request.
func (w *Worker) ApplyResolutions(ctx context.Context) error {
	rows, err := sqlcgen.New(w.db).ClaimResolveRequests(ctx, sqlcgen.ClaimResolveRequestsParams{
		TenantID: w.cfg.Tenant, Limit: w.cfg.Batch,
	})
	if err != nil {
		return fmt.Errorf("withdrawal: claim resolve requests: %w", err)
	}
	var firstErr error
	for _, row := range rows {
		if err := w.applyResolution(ctx, row); err != nil {
			// One request that cannot be applied must not stop the others.
			w.log.Error("resolve request failed",
				slog.String("withdrawal_id", row.ID), slog.String("action", deref(row.ResolveAction)),
				slog.String("err", err.Error()))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// applyResolution runs one, and clears the request when there is nothing left
// to retry.
//
// A request that no longer applies is cleared with its reason recorded, so the
// operator who asked can see what happened. Anything else — an unreachable
// node, a signer that did not answer — leaves the request standing, because
// the next tick may well succeed and silently dropping an operator's decision
// is worse than repeating it.
func (w *Worker) applyResolution(ctx context.Context, row sqlcgen.ChainWithdrawal) error {
	action := Action(deref(row.ResolveAction))
	_, err := w.Resolve(ctx, ResolveParams{
		ID: row.ID, Action: action, Note: deref(row.ResolveNote),
		// The admin API is the only writer of resolve_requested_by, and it
		// authenticates with a static key.
		ActorType: audit.ActorAPIKey, ActorID: deref(row.ResolveRequestedBy),
	})
	q := sqlcgen.New(w.db)
	if errors.Is(err, ErrNotResolvable) {
		reason := err.Error()
		w.log.Warn("resolve request no longer applies",
			slog.String("withdrawal_id", row.ID), slog.String("action", string(action)),
			slog.String("reason", reason))
		if clearErr := q.ClearWithdrawalResolve(ctx, sqlcgen.ClearWithdrawalResolveParams{
			TenantID: w.cfg.Tenant, ID: row.ID, ResolveError: &reason,
		}); clearErr != nil {
			return fmt.Errorf("withdrawal: clear resolve request: %w", clearErr)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := q.ClearWithdrawalResolve(ctx, sqlcgen.ClearWithdrawalResolveParams{
		TenantID: w.cfg.Tenant, ID: row.ID,
	}); err != nil {
		return fmt.Errorf("withdrawal: clear resolve request: %w", err)
	}
	w.log.Info("resolve request applied",
		slog.String("withdrawal_id", row.ID), slog.String("action", string(action)))
	w.metrics.resolved.WithLabelValues(string(action)).Inc()
	return nil
}

// cancelNonce sends a self-transfer on the stuck transaction's nonce.
//
// No ledger posting happens here. The original transaction can still win the
// race until the displacement is actually mined, and refunding a user for
// money that then leaves anyway is the one mistake this whole path exists to
// avoid (§6.4.2, and the 2026-09-05 erratum in docs/domain.md E1: the funds
// are in pending_withdrawal by now, not on hold, so the eventual refund is a
// posting rather than a Release).
func (w *Worker) cancelNonce(ctx context.Context, row sqlcgen.ChainWithdrawal, p ResolveParams) error {
	if row.Nonce == nil {
		return fmt.Errorf("withdrawal: %s is broadcast without a nonce", row.ID)
	}
	nonce := uint64(*row.Nonce) //nolint:gosec // CHECKed >= 0
	fees, err := w.fees(ctx)
	if err != nil {
		return err
	}
	// A displacement must outbid the transaction it replaces, and the original
	// has already been bumped `replacements` times.
	//
	// If that price is above the ceiling the answer goes back to the
	// administrator rather than being clamped: they asked for this
	// cancellation knowing what it was for, and sending it at a price that
	// cannot displace anything would tell them it was done when it was not.
	fees = fees.Bump(int64(20 + 10*row.Replacements))
	if err := w.underCeiling(fees); err != nil {
		return err
	}
	hot := w.nonces.HotWallet()
	res, err := w.signer.Sign(ctx, signer.Request{
		Kind: signer.KindNonceFill, RefID: fmt.Sprintf("%d:%d", row.ChainID, nonce),
		ChainID: row.ChainID, To: hot, Value: money.Zero, Nonce: nonce,
		Gas: gasForNativeTransfer, TipCap: fees.TipCap, FeeCap: fees.FeeCap,
	})
	if err != nil {
		return fmt.Errorf("withdrawal: sign cancellation for %s: %w", row.ID, err)
	}
	if _, err := sqlcgen.New(w.db).InsertNonceFill(ctx, sqlcgen.InsertNonceFillParams{
		TenantID: w.cfg.Tenant, ChainID: row.ChainID, Nonce: int64(nonce), //nolint:gosec // node nonces are far below 2^63
		TxHash: res.TxHash, Reason: "cancel_nonce",
	}); err != nil {
		return fmt.Errorf("withdrawal: record cancellation: %w", err)
	}
	if err := w.chain.SendRawTransaction(ctx, res.RawTx); err != nil && !errors.Is(err, evm.ErrKnownTransaction) {
		return fmt.Errorf("withdrawal: send cancellation for %s: %w", row.ID, err)
	}
	return inTx(ctx, w.db, func(tx pgx.Tx) error {
		if _, err := sqlcgen.New(tx).MarkWithdrawalCancelling(ctx, sqlcgen.MarkWithdrawalCancellingParams{
			TenantID: w.cfg.Tenant, ID: row.ID, CancelTxHash: &res.TxHash,
		}); err != nil {
			return fmt.Errorf("withdrawal: mark cancelling %s: %w", row.ID, err)
		}
		if err := w.audit.Record(ctx, tx, audit.Event{
			ActorType: p.ActorType, ActorID: p.ActorID, Action: "withdrawal.cancel_nonce",
			TargetType: "withdrawal", TargetID: row.ID,
			Before: map[string]any{"status": row.Status, "tx_hash": deref(row.TxHash)},
			After:  map[string]any{"cancel_tx_hash": res.TxHash, "nonce": nonce, "note": p.Note},
			IP:     p.IP, CorrelationID: deref(row.CorrelationID),
		}); err != nil {
			return fmt.Errorf("withdrawal: audit: %w", err)
		}
		w.log.Warn("cancelling a stuck withdrawal with a same-nonce transfer",
			slog.String("withdrawal_id", row.ID), slog.Uint64("nonce", nonce),
			slog.String("cancel_tx_hash", res.TxHash))
		return nil
	})
}

// settleFailed refunds or retries a withdrawal whose transaction reverted
// (§6.1.4 e).
//
// Both move the amount out of pending_withdrawal; they differ in where it
// lands. A refund puts it back in available, ending the withdrawal. A retry
// puts it back on hold and returns the row to funds_locked, so the machine
// signs and sends it again with a fresh nonce.
func (w *Worker) settleFailed(ctx context.Context, row sqlcgen.ChainWithdrawal, p ResolveParams) error {
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return err
	}
	pending, err := w.ledger.HouseAccount(ledger.HousePendingWithdrawal)
	if err != nil {
		return err
	}
	bucket, action := ledger.BucketAvailable, "withdrawal.refund"
	if p.Action == ActionRetry {
		bucket, action = ledger.BucketHold, "withdrawal.retry"
	}
	return inTx(ctx, w.db, func(tx pgx.Tx) error {
		if _, _, err := w.ledger.Post(ctx, tx, ledger.Entry{
			IdempotencyKey: fmt.Sprintf("withdrawal:%s:%s", p.Action, row.ID), Kind: "withdrawal",
			RefType: "withdrawal", RefID: row.ID, Reason: string(p.Action) + " after an on-chain failure",
			CorrelationID: deref(row.CorrelationID),
			Postings: []ledger.Posting{
				{AccountID: pending, Asset: row.Asset, Bucket: ledger.BucketHouse, Direction: ledger.Debit, Amount: amount},
				{AccountID: row.AccountID, Asset: row.Asset, Bucket: bucket, Direction: ledger.Credit, Amount: amount},
			},
		}); err != nil {
			return fmt.Errorf("withdrawal: post %s %s: %w", p.Action, row.ID, err)
		}
		var updated sqlcgen.ChainWithdrawal
		if p.Action == ActionRetry {
			updated, err = sqlcgen.New(tx).RetryWithdrawal(ctx, sqlcgen.RetryWithdrawalParams{TenantID: w.cfg.Tenant, ID: row.ID})
		} else {
			// The withdrawal stays failed; the reason now says the money went
			// back rather than that it is waiting for someone.
			reason := FailureOnChain
			updated, err = sqlcgen.New(tx).UpdateWithdrawalStatus(ctx, sqlcgen.UpdateWithdrawalStatusParams{
				TenantID: w.cfg.Tenant, ID: row.ID, Status: StatusFailed, FailureReason: &reason,
			})
		}
		if err != nil {
			return fmt.Errorf("withdrawal: %s %s: %w", p.Action, row.ID, err)
		}
		if err := w.audit.Record(ctx, tx, audit.Event{
			ActorType: p.ActorType, ActorID: p.ActorID, Action: action,
			TargetType: "withdrawal", TargetID: row.ID,
			Before: map[string]any{"status": row.Status, "failure_reason": deref(row.FailureReason)},
			After:  map[string]any{"status": updated.Status, "note": p.Note},
			IP:     p.IP, CorrelationID: deref(row.CorrelationID),
		}); err != nil {
			return fmt.Errorf("withdrawal: audit: %w", err)
		}
		w.log.Warn("operator resolved a failed withdrawal",
			slog.String("withdrawal_id", row.ID), slog.String("action", string(p.Action)),
			slog.String("note", p.Note))
		w.metrics.decided.WithLabelValues(row.Asset, updated.Status).Inc()
		return emit(ctx, tx, w.ledger, w.cfg.Tenant, EventStateChanged, updated, row.Status, string(p.Action))
	})
}

// settleCancelled refunds a withdrawal whose cancellation has been mined
// (§6.1.4 e failed(replaced)).
//
// The money is in pending_withdrawal, not on hold, so this is a posting rather
// than a Release. That distinction is the 2026-09-05 erratum: releasing would
// move funds out of a bucket they left when the withdrawal was broadcast.
func (w *Worker) settleCancelled(ctx context.Context, row sqlcgen.ChainWithdrawal, gas money.Amount) error {
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return err
	}
	pending, err := w.ledger.HouseAccount(ledger.HousePendingWithdrawal)
	if err != nil {
		return err
	}
	hot, err := w.ledger.HouseAccount(ledger.HouseCustodyHot)
	if err != nil {
		return err
	}
	gasAccount, err := w.ledger.HouseAccount(ledger.HouseGasExpense)
	if err != nil {
		return err
	}
	return inTx(ctx, w.db, func(tx pgx.Tx) error {
		if _, _, err := w.ledger.Post(ctx, tx, ledger.Entry{
			IdempotencyKey: "withdrawal:cancelled:" + row.ID, Kind: "withdrawal",
			RefType: "withdrawal", RefID: row.ID, Reason: "cancelled by a same-nonce transaction",
			CorrelationID: deref(row.CorrelationID),
			Postings: []ledger.Posting{
				{AccountID: pending, Asset: row.Asset, Bucket: ledger.BucketHouse, Direction: ledger.Debit, Amount: amount},
				{AccountID: row.AccountID, Asset: row.Asset, Bucket: ledger.BucketAvailable, Direction: ledger.Credit, Amount: amount},
			},
		}); err != nil {
			return fmt.Errorf("withdrawal: post cancellation refund %s: %w", row.ID, err)
		}
		// The displacing transaction burned gas of its own.
		if err := w.postGas(ctx, tx, row, gasAccount, hot, gas); err != nil {
			return err
		}
		if err := sqlcgen.New(tx).UpdateNonceFillStatus(ctx, sqlcgen.UpdateNonceFillStatusParams{
			TenantID: w.cfg.Tenant, ChainID: row.ChainID, Nonce: *row.Nonce, Status: "confirmed",
		}); err != nil {
			return fmt.Errorf("withdrawal: mark fill confirmed: %w", err)
		}
		w.log.Warn("a cancelled withdrawal was refunded",
			slog.String("withdrawal_id", row.ID), slog.String("cancel_tx_hash", deref(row.CancelTxHash)))
		return w.failTx(ctx, tx, row, FailureReplaced)
	})
}

func (w *Worker) reload(ctx context.Context, id string) (Record, error) {
	row, err := sqlcgen.New(w.db).GetWithdrawal(ctx, sqlcgen.GetWithdrawalParams{TenantID: w.cfg.Tenant, ID: id})
	if err != nil {
		return Record{}, fmt.Errorf("withdrawal: reload %s: %w", id, err)
	}
	return recordFrom(row)
}
