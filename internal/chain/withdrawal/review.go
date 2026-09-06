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
	ID      string
	AdminID string
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
			ReviewedBy: &p.AdminID, ReviewNote: optString(p.Note),
		})
		if err != nil {
			return fmt.Errorf("withdrawal: review %s: %w", p.ID, err)
		}
		action := "withdrawal.reject"
		if p.Approve {
			action = "withdrawal.approve"
		}
		if err := r.audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorAdmin, ActorID: p.AdminID, Action: action,
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
