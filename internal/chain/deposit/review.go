package deposit

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
)

// ErrInvalid is a request that is wrong on its face.
var ErrInvalid = errors.New("deposit: invalid request")

// ErrNotReversible is a deposit that is not waiting for this decision -- it
// was never reorged, it has already been reversed, or somebody has already
// asked. Separated from ErrInvalid because the caller answers 409, not 400:
// the request was well formed, the world had moved.
var ErrNotReversible = errors.New("deposit: not awaiting a reversal")

// Reviewer is the admin side of the reversed path (§6.4.1). It runs in the
// admin role, whose only write to chain.deposits is the three request columns
// (migration 0024): confirming a reversal records a decision, and the chain
// role posts the entry on its next tick.
type Reviewer struct {
	db     *pgxpool.Pool
	tenant string
	audit  *audit.Recorder
}

// NewReviewer builds the admin-side reviewer.
func NewReviewer(db *pgxpool.Pool, tenant string, rec *audit.Recorder) *Reviewer {
	return &Reviewer{db: db, tenant: tenant, audit: rec}
}

// AwaitingReversal is the queue: credited deposits whose block a reorg took
// away, oldest reorg first.
func (r *Reviewer) AwaitingReversal(ctx context.Context, limit, offset int32) ([]Record, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := sqlcgen.New(r.db).ListDepositsAwaitingReversal(ctx, sqlcgen.ListDepositsAwaitingReversalParams{
		TenantID: r.tenant, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, fmt.Errorf("deposit: list awaiting reversal: %w", err)
	}
	return records(rows)
}

// ReverseParams is an operator confirming that a reorged deposit should be
// undone.
type ReverseParams struct {
	ID        string
	Note      string
	ActorType audit.ActorType
	ActorID   string
	IP        string
}

// RequestReversal records the decision. It moves no money: the chain role
// posts the reversing entry on its next tick, and may still refuse if the
// account has spent what it was credited.
//
// The note is required and there is no approve/reject pair, because there is
// nothing to reject: leaving a reorged deposit alone is what happens if nobody
// acts. Asking is the whole decision.
func (r *Reviewer) RequestReversal(ctx context.Context, p ReverseParams) (Record, error) {
	if p.Note == "" {
		return Record{}, fmt.Errorf("%w: a reversal must say why", ErrInvalid)
	}
	var out Record
	err := inTx(ctx, r.db, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		updated, err := q.RequestDepositReversal(ctx, sqlcgen.RequestDepositReversalParams{
			ID: p.ID, TenantID: r.tenant,
			ReversalRequestedBy: &p.ActorID, ReversalNote: &p.Note,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// Either it does not exist, or it is not in the state this asks
			// for. The query's WHERE says which conditions matter; the caller
			// does not need to distinguish, because the answer is the same.
			return ErrNotReversible
		}
		if err != nil {
			return fmt.Errorf("deposit: request reversal %s: %w", p.ID, err)
		}
		if err := r.audit.Record(ctx, tx, audit.Event{
			ActorType: p.ActorType, ActorID: p.ActorID, Action: "deposit.reversal_requested",
			TargetType: "deposit", TargetID: p.ID,
			Before: map[string]any{"status": updated.Status, "reorged_at_block": updated.ReorgedAtBlock},
			After:  map[string]any{"reversal_requested": true, "note": p.Note},
			IP:     p.IP, CorrelationID: deref(updated.CorrelationID),
		}); err != nil {
			return fmt.Errorf("deposit: audit: %w", err)
		}
		rec, err := records([]sqlcgen.ChainDeposit{updated})
		if err != nil {
			return err
		}
		out = rec[0]
		return nil
	})
	return out, err
}

// inTx runs fn in a transaction. The Scanner has one of its own bound to its
// pool; the Reviewer runs in a different role and a different process, so it
// needs its own rather than sharing.
func inTx(ctx context.Context, db *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("deposit: begin: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}
