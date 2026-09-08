package withdrawal

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/ledger"
)

// Reviewer is the admin side of the state machine: the approve/reject queue of
// docs/plan-v1.0.md §6.4.2. It runs in the admin role, which may write the
// review columns but cannot lock funds or sign — approving only marks the
// withdrawal for the chain worker to pick up.
type Reviewer struct {
	db     *pgxpool.Pool
	tenant string
	ledger *ledger.Service
	audit  *audit.Recorder
}

// NewReviewer builds the admin-side reviewer.
func NewReviewer(db *pgxpool.Pool, tenant string, l *ledger.Service, rec *audit.Recorder) *Reviewer {
	return &Reviewer{db: db, tenant: tenant, ledger: l, audit: rec}
}

// Pending lists the review queue, oldest first: the person who has waited
// longest is served first.
func (r *Reviewer) Pending(ctx context.Context, limit int32) ([]Record, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := sqlcgen.New(r.db).ListWithdrawalsByStatus(ctx, sqlcgen.ListWithdrawalsByStatusParams{
		TenantID: r.tenant, Statuses: []string{StatusPendingReview}, Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("withdrawal: list pending review: %w", err)
	}
	return records(rows)
}

// PendingCount is how many withdrawals are waiting for a person.
func (r *Reviewer) PendingCount(ctx context.Context) (int64, error) {
	n, err := sqlcgen.New(r.db).CountWithdrawalsByStatus(ctx, sqlcgen.CountWithdrawalsByStatusParams{
		TenantID: r.tenant, Statuses: []string{StatusPendingReview},
	})
	if err != nil {
		return 0, fmt.Errorf("withdrawal: count pending: %w", err)
	}
	return n, nil
}

// Get returns any withdrawal, whoever owns it. The admin API is the one caller
// allowed to look across accounts.
func (r *Reviewer) Get(ctx context.Context, id string) (Record, error) {
	row, err := sqlcgen.New(r.db).GetWithdrawal(ctx, sqlcgen.GetWithdrawalParams{TenantID: r.tenant, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("withdrawal: get %s: %w", id, err)
	}
	return recordFrom(row)
}

// ReviewParams is one admin decision.
type ReviewParams struct {
	ID string
	// AdminID is the reviewing user, when there is one. The admin API still
	// authenticates with a static key, so it is empty there and the audit
	// trail carries the actor instead.
	AdminID string
	// ActorType and ActorID are what the audit trail records, which is always
	// filled even when AdminID is not.
	ActorType audit.ActorType
	ActorID   string
	// Approve is the decision itself; a rejection carries the reason in Note.
	Approve bool
	Note    string
	IP      string
}

// Review approves or rejects a withdrawal awaiting review.
//
// Approving does not lock funds: it moves the row to `approved` and the chain
// worker takes it from there. That separation is what the column grants
// enforce — admin may write the review columns, the chain worker may write the
// hold — so no single role can both authorise a withdrawal and act on it.
func (r *Reviewer) Review(ctx context.Context, p ReviewParams) (Record, error) {
	var out Record
	err := inTx(ctx, r.db, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		row, err := q.GetWithdrawalForUpdate(ctx, sqlcgen.GetWithdrawalForUpdateParams{TenantID: r.tenant, ID: p.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("withdrawal: lock %s: %w", p.ID, err)
		}
		// Only a withdrawal that is actually waiting can be decided. Without
		// this an approval could resurrect one that was already rejected, or
		// race the worker that is mid-lock.
		if row.Status != StatusPendingReview {
			return fmt.Errorf("%w: %s is %s", ErrNotReviewable, p.ID, row.Status)
		}
		status := StatusRejected
		var failure *string
		if p.Approve {
			status = StatusApproved
		} else {
			failure = optString(FailurePolicy)
		}
		updated, err := q.ReviewWithdrawal(ctx, sqlcgen.ReviewWithdrawalParams{
			TenantID: r.tenant, ID: p.ID, Status: status, FailureReason: failure,
			ReviewedBy: optString(p.AdminID), ReviewNote: optString(p.Note),
		})
		if err != nil {
			return fmt.Errorf("withdrawal: review %s: %w", p.ID, err)
		}
		action := "withdrawal.reject"
		if p.Approve {
			action = "withdrawal.approve"
		}
		if err := r.audit.Record(ctx, tx, audit.Event{
			ActorType: p.ActorType, ActorID: p.ActorID, Action: action,
			TargetType: "withdrawal", TargetID: p.ID,
			Before:        map[string]any{"status": row.Status},
			After:         map[string]any{"status": status, "note": p.Note},
			IP:            p.IP,
			CorrelationID: deref(row.CorrelationID),
		}); err != nil {
			return fmt.Errorf("withdrawal: audit: %w", err)
		}
		if err := emit(ctx, tx, r.ledger, r.tenant, EventStateChanged, updated, row.Status, p.Note); err != nil {
			return err
		}
		out, err = recordFrom(updated)
		return err
	})
	return out, err
}

// RequestResolve records what an operator wants done about a withdrawal the
// machine could not finish (docs/plan-v1.0.md §6.4.2 resolve).
//
// It records the request and stops. Every one of the four actions needs a node
// and a key — even a refund has to know whether the transaction is really
// finished — and the admin role has neither, nor the grants on the transaction
// columns. So this writes the four resolve columns it is allowed to write and
// the chain worker applies them, exactly as approving a withdrawal marks it
// for the worker rather than locking the funds here.
//
// The legality of the action is checked now so the operator gets a 409 while
// they are still looking at the screen, and checked again when it is applied,
// because the withdrawal can move in between.
func (r *Reviewer) RequestResolve(ctx context.Context, p ResolveParams) (Record, error) {
	if p.Note == "" {
		return Record{}, fmt.Errorf("%w: a resolution must say why", ErrInvalid)
	}
	var out Record
	err := inTx(ctx, r.db, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		row, err := q.GetWithdrawalForUpdate(ctx, sqlcgen.GetWithdrawalForUpdateParams{TenantID: r.tenant, ID: p.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("withdrawal: lock %s: %w", p.ID, err)
		}
		if err := resolvable(row, p.Action); err != nil {
			return err
		}
		// A request already waiting would be silently replaced, and the
		// operator would never learn which of the two was applied.
		if row.ResolveAction != nil {
			return fmt.Errorf("%w: %s already has a %s waiting to be applied",
				ErrNotResolvable, p.ID, *row.ResolveAction)
		}
		updated, err := q.RequestWithdrawalResolve(ctx, sqlcgen.RequestWithdrawalResolveParams{
			TenantID: r.tenant, ID: p.ID, ResolveAction: optString(string(p.Action)),
			ResolveNote: optString(p.Note), ResolveRequestedBy: optString(p.ActorID),
		})
		if err != nil {
			return fmt.Errorf("withdrawal: request resolve %s: %w", p.ID, err)
		}
		if err := r.audit.Record(ctx, tx, audit.Event{
			ActorType: p.ActorType, ActorID: p.ActorID, Action: "withdrawal.resolve_requested",
			TargetType: "withdrawal", TargetID: p.ID,
			Before: map[string]any{"status": row.Status, "failure_reason": deref(row.FailureReason)},
			After:  map[string]any{"resolve_action": string(p.Action), "note": p.Note},
			IP:     p.IP, CorrelationID: deref(row.CorrelationID),
		}); err != nil {
			return fmt.Errorf("withdrawal: audit: %w", err)
		}
		out, err = recordFrom(updated)
		return err
	})
	return out, err
}
