// Package withdrawal owns the withdrawal state machine of
// docs/plan-v1.0.md §6.4.2.
//
// This half of it never touches the chain and never touches a key: the api
// role records a request, and the chain role decides it against the policy and
// locks the funds in the ledger. Signing, broadcasting and tracking are the
// signer's, and pick the row up at funds_locked.
//
// The two iron rules of §6.4.2 are what the shape here is for: the ledger is
// locked before anything can be signed, and every state is committed before
// the next step runs, so a restart resumes from the row.
package withdrawal

import (
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// Statuses (docs/plan-v1.0.md §6.4.2). The states after StatusFundsLocked are
// declared here so the whole machine reads in one place, but nothing in this
// package produces them yet.
const (
	StatusRequested     = "requested"
	StatusPolicyCheck   = "policy_check"
	StatusAutoApproved  = "auto_approved"
	StatusPendingReview = "pending_review"
	StatusApproved      = "approved"
	StatusRejected      = "rejected"
	StatusFundsLocked   = "funds_locked"
	StatusSigned        = "signed"
	StatusBroadcast     = "broadcast"
	StatusConfirmed     = "confirmed"
	StatusFailed        = "failed"
)

// Failure reasons stored in failure_reason. Only the first is reachable here.
const (
	FailureInsufficientBalance = "insufficient_balance"
	FailurePolicy              = "policy"
	// FailureBroadcast is a transaction the node refused that the chain
	// confirms is not mined: the funds go back to available.
	FailureBroadcast = "broadcast"
	// FailureOnChain is a transaction that was mined and reverted. The gas is
	// spent and the amount stays in pending_withdrawal for a person to
	// resolve (§6.1.4 e).
	FailureOnChain = "on_chain"
	// FailureReplaced is a broadcast transaction cancelled by an admin with a
	// same-nonce self-transfer.
	FailureReplaced = "replaced"
)

// Errors callers distinguish.
var (
	// ErrNotFound is an unknown withdrawal id, or one belonging to another
	// account.
	ErrNotFound = errors.New("withdrawal: not found")
	// ErrIdempotencyMismatch is the same Idempotency-Key with a different
	// request. It maps to 422, never to a second withdrawal.
	ErrIdempotencyMismatch = errors.New("withdrawal: idempotency key reused with a different request")
	// ErrNotReviewable is an admin decision on a withdrawal that is not
	// waiting for one.
	ErrNotReviewable = errors.New("withdrawal: not awaiting review")
	// ErrInvalid is a request the API should not have accepted.
	ErrInvalid = errors.New("withdrawal: invalid request")
)

// Record is one withdrawal as the API and the CLI see it.
type Record struct {
	ID        string
	AccountID string
	Asset     string
	Amount    money.Amount
	ToAddress string
	ChainID   int64
	Status    string
	// FailureReason is set only for StatusFailed; a rejection carries its
	// reason in ReviewNote or in the policy reason recorded there.
	FailureReason string
	ReviewNote    string
	ReviewedBy    string
	ReviewedAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// CreateResult is what Create produced. Replayed reports that it returned an
// existing withdrawal rather than making a new one, so the API can answer 200
// instead of 201.
type CreateResult struct {
	Record   Record
	Replayed bool
}

func recordFrom(row sqlcgen.ChainWithdrawal) (Record, error) {
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return Record{}, err
	}
	rec := Record{
		ID: row.ID, AccountID: row.AccountID, Asset: row.Asset, Amount: amount,
		ToAddress: row.ToAddress, ChainID: row.ChainID, Status: row.Status,
		FailureReason: deref(row.FailureReason), ReviewNote: deref(row.ReviewNote),
		ReviewedBy: deref(row.ReviewedBy),
		CreatedAt:  row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	if row.ReviewedAt.Valid {
		at := row.ReviewedAt.Time
		rec.ReviewedAt = &at
	}
	return rec, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
