package policy

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

func withdrawable() registry.Asset {
	return registry.Asset{
		Symbol: "ETH", Scale: 18, WithdrawEnabled: true, Status: registry.AssetActive,
		MinWithdrawal: money.MustParse("0.01"),
	}
}

func limit(auto, daily string) *registry.WithdrawalLimit {
	return &registry.WithdrawalLimit{
		Asset: "ETH", AutoApproveLimit: money.MustParse(auto), DailyLimit: money.MustParse(daily),
	}
}

func request(amount string) WithdrawalRequest {
	return WithdrawalRequest{
		Asset: withdrawable(), Account: ledger.Account{Status: ledger.StatusActive},
		Amount: money.MustParse(amount), Limit: limit("0.1", "1"), WithdrawnToday: money.Zero,
	}
}

func TestWithdrawAutoApprovesWithinBothLimits(t *testing.T) {
	decision, reason := Basic{}.Withdraw(request("0.05"))
	assert.Equal(t, DecisionAutoApprove, decision)
	assert.Equal(t, Reason(""), reason, "an approved withdrawal has nothing to explain")
}

func TestWithdrawRefusesOnlyWhatIsNotAllowedAtAll(t *testing.T) {
	frozen := request("0.05")
	frozen.Account.Status = ledger.StatusFrozen
	d, r := Basic{}.Withdraw(frozen)
	assert.Equal(t, DecisionReject, d)
	assert.Equal(t, ReasonAccountFrozen, r)

	disabled := request("0.05")
	disabled.Asset.WithdrawEnabled = false
	d, r = Basic{}.Withdraw(disabled)
	assert.Equal(t, DecisionReject, d)
	assert.Equal(t, ReasonWithdrawalsDisabled, r)

	// An asset can have withdrawals enabled and still be disabled as a whole,
	// so the status is checked separately from the flag.
	off := request("0.05")
	off.Asset.Status = registry.AssetDisabled
	d, r = Basic{}.Withdraw(off)
	assert.Equal(t, DecisionReject, d)
	assert.Equal(t, ReasonWithdrawalsDisabled, r)

	d, r = Basic{}.Withdraw(request("0.001"))
	assert.Equal(t, DecisionReject, d)
	assert.Equal(t, ReasonBelowMinimum, r)
}

// A limit is a threshold for who decides, not a verdict on the withdrawal:
// everything that fails one queues for a person instead of being refused.
func TestWithdrawSendsEveryLimitBreachToReview(t *testing.T) {
	over := request("0.5") // above auto_approve_limit 0.1, below daily 1
	d, r := Basic{}.Withdraw(over)
	assert.Equal(t, DecisionReview, d)
	assert.Equal(t, ReasonAboveAutoApprove, r)

	manual := request("0.05")
	manual.Limit.RequireManualReview = true
	d, r = Basic{}.Withdraw(manual)
	assert.Equal(t, DecisionReview, d)
	assert.Equal(t, ReasonManualReviewRequired, r)

	unconfigured := request("0.05")
	unconfigured.Limit = nil
	d, r = Basic{}.Withdraw(unconfigured)
	assert.Equal(t, DecisionReview, d)
	assert.Equal(t, ReasonNoLimitConfigured, r,
		"an operator who configured no limit did not thereby allow everything")
}

// The daily limit counts this request too, so an account cannot cross it one
// auto-approvable withdrawal at a time.
func TestWithdrawCountsTheRequestAgainstTheDailyLimit(t *testing.T) {
	req := request("0.1") // exactly the per-request ceiling, so that one passes
	req.WithdrawnToday = money.MustParse("0.95")
	d, r := Basic{}.Withdraw(req)
	assert.Equal(t, DecisionReview, d)
	assert.Equal(t, ReasonDailyLimitExceeded, r)

	// landing exactly on the daily limit is still within it
	exact := request("0.05")
	exact.WithdrawnToday = money.MustParse("0.95")
	d, r = Basic{}.Withdraw(exact)
	assert.Equal(t, DecisionAutoApprove, d)
	assert.Equal(t, Reason(""), r)
}

// The policy is a pure function and must be total: a zero minimum must not
// let a zero-amount request through as "at least the minimum".
func TestWithdrawRejectsANonPositiveAmountEvenWithoutAMinimum(t *testing.T) {
	req := request("1")
	req.Asset.MinWithdrawal = money.Zero
	req.Amount = money.Zero
	d, r := Basic{}.Withdraw(req)
	assert.Equal(t, DecisionReject, d)
	assert.Equal(t, ReasonBelowMinimum, r)
}
