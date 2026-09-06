// Package policy holds the synchronous pre-trade and pre-withdrawal checks
// (docs/plan-v1.0.md §8, review finding 17). Phase 3 ships the minimal
// order policy: market status and account status. Limits, KYC levels and
// the withdrawal policy arrive with Phase 4/5; after-the-fact risk
// detection is out of scope for v1.
package policy

import (
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// Reason is a policy denial. The empty Reason means "allowed". Values are
// the reject reasons of docs/plan-v1.0.md §6.2 so trading can record them
// verbatim.
type Reason string

// Denial reasons.
const (
	ReasonMarketNotActive Reason = "market_not_active"
	ReasonAccountFrozen   Reason = "account_frozen"
	ReasonPolicyDenied    Reason = "policy_denied"
)

// OrderPolicy decides whether an account may place or cancel an order on a
// market right now. Implementations must be pure functions of their inputs
// so the engine can call them on its hot path.
type OrderPolicy interface {
	// NewOrder returns "" when the account may place a new order.
	NewOrder(market registry.Market, account ledger.Account) Reason
	// Cancel returns "" when cancels are accepted on the market.
	Cancel(market registry.Market) Reason
}

// Basic is the v1 policy: active markets take orders; halted and
// cancel_only markets take cancels only; frozen accounts may still cancel.
type Basic struct{}

// NewOrder implements OrderPolicy.
func (Basic) NewOrder(market registry.Market, account ledger.Account) Reason {
	if market.Status != registry.MarketActive {
		return ReasonMarketNotActive
	}
	if account.Status != ledger.StatusActive {
		return ReasonAccountFrozen
	}
	return ""
}

// Cancel implements OrderPolicy. A delisted market has no resting orders
// left by definition (docs/plan-v1.0.md §6.6), so cancels there are refused.
func (Basic) Cancel(market registry.Market) Reason {
	if market.Status == registry.MarketDelisted {
		return ReasonMarketNotActive
	}
	return ""
}
