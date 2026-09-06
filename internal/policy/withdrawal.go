package policy

import (
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// Withdrawal denial and review reasons (docs/plan-v1.0.md §6.4.2). They are
// stored on the withdrawal and shown to the user, so they say what happened
// rather than what to do about it.
const (
	ReasonWithdrawalsDisabled  Reason = "withdrawals_disabled"
	ReasonBelowMinimum         Reason = "below_min_withdrawal"
	ReasonAboveAutoApprove     Reason = "above_auto_approve_limit"
	ReasonDailyLimitExceeded   Reason = "daily_limit_exceeded"
	ReasonManualReviewRequired Reason = "manual_review_required"
	ReasonNoLimitConfigured    Reason = "no_limit_configured"
)

// WithdrawalDecision is where the policy check sends a withdrawal. The values
// are the §6.4.2 status names, so the worker stores the decision verbatim.
type WithdrawalDecision string

// Decisions.
const (
	DecisionAutoApprove WithdrawalDecision = "auto_approved"
	DecisionReview      WithdrawalDecision = "pending_review"
	DecisionReject      WithdrawalDecision = "rejected"
)

// WithdrawalRequest is everything the policy is allowed to look at. Passing
// the rolling total in rather than a database handle keeps the decision a
// pure function, which is what makes it testable without Postgres.
type WithdrawalRequest struct {
	Asset    registry.Asset
	Account  ledger.Account
	KYCLevel int16
	Amount   money.Amount
	// Limit is the configured ceiling for this asset at this KYC level, or
	// nil when the operator has not configured one.
	Limit *registry.WithdrawalLimit
	// WithdrawnToday is what the account has already committed in the rolling
	// window for this asset, excluding this request.
	WithdrawnToday money.Amount
}

// WithdrawalPolicy decides whether a withdrawal may skip human review.
type WithdrawalPolicy interface {
	// Withdraw returns the decision and the reason behind it. The reason is
	// empty only for DecisionAutoApprove.
	Withdraw(req WithdrawalRequest) (WithdrawalDecision, Reason)
}

// Withdraw implements WithdrawalPolicy (docs/plan-v1.0.md §6.4.2).
//
// Only three things are refused outright: a frozen account, an asset that is
// not withdrawable, and an amount below the asset's minimum. Everything else
// that fails a limit goes to the review queue rather than being rejected,
// because a limit is a threshold for *who decides*, not a statement that the
// withdrawal is illegitimate — a person can approve an exception, and telling
// a user "no" when the honest answer is "not automatically" would be wrong.
func (Basic) Withdraw(req WithdrawalRequest) (WithdrawalDecision, Reason) {
	if req.Account.Status != ledger.StatusActive {
		return DecisionReject, ReasonAccountFrozen
	}
	if !req.Asset.WithdrawEnabled || req.Asset.Status != registry.AssetActive {
		return DecisionReject, ReasonWithdrawalsDisabled
	}
	// A non-positive amount cannot reach here through the API or the table
	// CHECK, but the policy is a pure function and stays total: with a zero
	// minimum the comparison below would otherwise let one through.
	if !req.Amount.IsPositive() || req.Amount.Cmp(req.Asset.MinWithdrawal) < 0 {
		return DecisionReject, ReasonBelowMinimum
	}
	// No configured limit is not "no limit". An operator who has not decided
	// what this KYC level may withdraw has not decided that it may withdraw
	// anything unattended.
	if req.Limit == nil {
		return DecisionReview, ReasonNoLimitConfigured
	}
	if req.Limit.RequireManualReview {
		return DecisionReview, ReasonManualReviewRequired
	}
	if req.Amount.Cmp(req.Limit.AutoApproveLimit) > 0 {
		return DecisionReview, ReasonAboveAutoApprove
	}
	// The daily limit is checked against the total *including* this request,
	// so an account cannot step over the line one small withdrawal at a time.
	if total := req.WithdrawnToday.Add(req.Amount); total.Cmp(req.Limit.DailyLimit) > 0 {
		return DecisionReview, ReasonDailyLimitExceeded
	}
	return DecisionAutoApprove, ""
}
