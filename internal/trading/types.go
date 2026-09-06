// Package trading owns orders and trades: the order state machine of
// docs/plan-v1.0.md §6.2, one runner goroutine per market that applies
// commands to the pure order book (internal/matching) inside a Postgres
// transaction together with the ledger entries and the outbox rows
// (ADR-0002), and the read side used by the public API.
package trading

import (
	"errors"
	"fmt"
	"time"

	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// Status of an order (docs/plan-v1.0.md §6.2). There is no persisted
// intermediate state: hold, matching and persistence happen in one
// transaction.
type Status string

// Order statuses.
const (
	StatusOpen            Status = "open"
	StatusPartiallyFilled Status = "partially_filled"
	StatusFilled          Status = "filled"
	StatusCancelled       Status = "cancelled"
	StatusRejected        Status = "rejected"
)

// Terminal reports whether no further transition is possible.
func (s Status) Terminal() bool {
	return s == StatusFilled || s == StatusCancelled || s == StatusRejected
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	switch s {
	case StatusOpen, StatusPartiallyFilled, StatusFilled, StatusCancelled, StatusRejected:
		return true
	}
	return false
}

// RejectReason explains a rejected order. The matching reasons
// (invalid_price_tick, below_min_notional, empty_book, ...) are recorded
// verbatim; the ones below come from policy and the ledger.
type RejectReason string

// Reject reasons added by trading (docs/plan-v1.0.md §6.2).
const (
	RejectMarketNotActive     RejectReason = "market_not_active"
	RejectInsufficientBalance RejectReason = "insufficient_balance"
	RejectAccountFrozen       RejectReason = "account_frozen"
	RejectPolicyDenied        RejectReason = "policy_denied"
)

// Errors returned to callers. Business rejections are not errors: they are
// orders in StatusRejected.
var (
	ErrInvalidRequest        = errors.New("trading: invalid request")
	ErrMarketNotFound        = errors.New("trading: market not found")
	ErrOrderNotFound         = errors.New("trading: order not found")
	ErrClientOrderIDMismatch = errors.New("trading: client_order_id already used with a different order")
	ErrEngineUnavailable     = errors.New("trading: engine unavailable")
	ErrSequenceConflict      = errors.New("trading: market sequence advanced by another writer")
	ErrBookInconsistent      = errors.New("trading: order book and database disagree")
)

// Order is the persisted order (trading.orders).
type Order struct {
	ID            string
	TenantID      string
	AccountID     string
	MarketID      string
	MarketSymbol  string
	ClientOrderID string
	Side          matching.Side
	Type          matching.OrderType
	TimeInForce   matching.TimeInForce
	Price         *money.Amount // nil for market orders
	Qty           *money.Amount // base; nil for market buys
	QuoteQty      *money.Amount // quote budget of a market buy
	FilledQty     money.Amount
	FilledQuote   money.Amount
	RemainingQty  money.Amount
	HoldAsset     string
	HoldAmount    money.Amount // frozen at acceptance
	HoldRemaining money.Amount // still frozen (0 once terminal)
	Status        Status
	RejectReason  RejectReason
	CancelReason  matching.CancelReason
	Seq           *uint64
	CorrelationID string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Trade is one persisted fill (trading.trades). Fees are in the asset each
// side received (docs/plan-v1.0.md §6.1.4 b).
type Trade struct {
	ID             string
	TenantID       string
	MarketID       string
	MarketSymbol   string
	Seq            uint64
	Index          int
	MakerOrderID   string
	TakerOrderID   string
	MakerAccountID string
	TakerAccountID string
	TakerSide      matching.Side
	Price          money.Amount
	Qty            money.Amount
	QuoteQty       money.Amount
	MakerFee       money.Amount
	MakerFeeAsset  string
	TakerFee       money.Amount
	TakerFeeAsset  string
	CreatedAt      time.Time
}

// PlaceOrderRequest is a new order as the API hands it to the engine.
// Amounts are zero when absent: limit orders carry Price and Qty, market
// sells Qty, market buys QuoteQty (docs/plan-v1.0.md §6.3).
type PlaceOrderRequest struct {
	AccountID     string
	MarketSymbol  string
	ClientOrderID string
	Side          matching.Side
	Type          matching.OrderType
	TimeInForce   matching.TimeInForce // zero = GTC for limit; market is always IOC
	Price         money.Amount
	Qty           money.Amount
	QuoteQty      money.Amount
	CorrelationID string
}

// MaxClientOrderIDLen mirrors the CHECK constraint on trading.orders.
const MaxClientOrderIDLen = 64

// Validate checks the request's shape; business rules (tick, step, min
// notional, balance, market status) are decided by the engine and recorded
// as rejections, not returned as errors.
func (r PlaceOrderRequest) Validate() error {
	switch {
	case r.AccountID == "":
		return fmt.Errorf("%w: account_id required", ErrInvalidRequest)
	case r.MarketSymbol == "":
		return fmt.Errorf("%w: market required", ErrInvalidRequest)
	case r.ClientOrderID == "" || len(r.ClientOrderID) > MaxClientOrderIDLen:
		return fmt.Errorf("%w: client_order_id must be 1..%d characters", ErrInvalidRequest, MaxClientOrderIDLen)
	case r.Side != matching.Buy && r.Side != matching.Sell:
		return fmt.Errorf("%w: side must be buy or sell", ErrInvalidRequest)
	case r.Type != matching.Limit && r.Type != matching.Market:
		return fmt.Errorf("%w: type must be limit or market", ErrInvalidRequest)
	case r.TimeInForce != 0 && r.TimeInForce != matching.GTC && r.TimeInForce != matching.IOC:
		return fmt.Errorf("%w: time_in_force must be gtc or ioc", ErrInvalidRequest)
	case r.Price.IsNegative() || r.Qty.IsNegative() || r.QuoteQty.IsNegative():
		return fmt.Errorf("%w: amounts must not be negative", ErrInvalidRequest)
	}
	switch {
	case r.Type == matching.Limit:
		if !r.Price.IsPositive() || !r.Qty.IsPositive() || r.QuoteQty.IsPositive() {
			return fmt.Errorf("%w: limit orders carry price and qty only", ErrInvalidRequest)
		}
	case r.Side == matching.Buy: // market buy
		if !r.QuoteQty.IsPositive() || r.Price.IsPositive() || r.Qty.IsPositive() {
			return fmt.Errorf("%w: market buys carry quote_qty only", ErrInvalidRequest)
		}
	default: // market sell
		if !r.Qty.IsPositive() || r.Price.IsPositive() || r.QuoteQty.IsPositive() {
			return fmt.Errorf("%w: market sells carry qty only", ErrInvalidRequest)
		}
	}
	for _, a := range []money.Amount{r.Price, r.Qty, r.QuoteQty} {
		if err := a.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
	}
	return nil
}

// effectiveTIF resolves the zero value: market orders are IOC, limit
// orders default to GTC.
func (r PlaceOrderRequest) effectiveTIF() matching.TimeInForce {
	if r.Type == matching.Market {
		return matching.IOC
	}
	if r.TimeInForce == 0 {
		return matching.GTC
	}
	return r.TimeInForce
}

// sameAs reports whether an existing order was placed by an identical
// request (client_order_id replay) as opposed to a reused id.
func (r PlaceOrderRequest) sameAs(o Order) bool {
	if o.MarketSymbol != r.MarketSymbol || o.Side != r.Side || o.Type != r.Type || o.TimeInForce != r.effectiveTIF() {
		return false
	}
	eq := func(p *money.Amount, a money.Amount) bool {
		if p == nil {
			return a.IsZero()
		}
		return p.Equal(a)
	}
	return eq(o.Price, r.Price) && eq(o.Qty, r.Qty) && eq(o.QuoteQty, r.QuoteQty)
}

// CancelRequest asks the engine to cancel one order of an account.
type CancelRequest struct {
	AccountID     string
	OrderID       string
	CorrelationID string
}

// PlaceOrderResult is what the engine returns for a new order: the order in
// its state right after the command (accepted-and-resting, filled,
// cancelled or rejected), the trades it produced as taker, and whether the
// request was a replay of an earlier one with the same client_order_id.
type PlaceOrderResult struct {
	Order    Order
	Trades   []Trade
	Replayed bool
}

// Fill is a trade seen from one account's side.
type Fill struct {
	Trade
	OrderID   string // this account's order
	Side      matching.Side
	IsMaker   bool
	Fee       money.Amount
	FeeAsset  string
	AccountID string
}

// FillOf projects a trade onto an account that took part in it.
func FillOf(t Trade, accountID string) (Fill, bool) {
	switch accountID {
	case t.MakerAccountID:
		return Fill{Trade: t, OrderID: t.MakerOrderID, Side: t.TakerSide.Opposite(), IsMaker: true, Fee: t.MakerFee, FeeAsset: t.MakerFeeAsset, AccountID: accountID}, true
	case t.TakerAccountID:
		return Fill{Trade: t, OrderID: t.TakerOrderID, Side: t.TakerSide, IsMaker: false, Fee: t.TakerFee, FeeAsset: t.TakerFeeAsset, AccountID: accountID}, true
	}
	return Fill{}, false
}
