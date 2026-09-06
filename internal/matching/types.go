package matching

import (
	"errors"
	"fmt"
	"time"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// Side of an order.
type Side uint8

// Sides.
const (
	Buy Side = iota + 1
	Sell
)

// Opposite returns the side an order trades against.
func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

func (s Side) String() string {
	switch s {
	case Buy:
		return "buy"
	case Sell:
		return "sell"
	}
	return fmt.Sprintf("side(%d)", uint8(s))
}

// MarshalText implements encoding.TextMarshaler.
func (s Side) MarshalText() ([]byte, error) { return enumText(s.String(), s == Buy || s == Sell) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Side) UnmarshalText(b []byte) error {
	switch string(b) {
	case "buy":
		*s = Buy
	case "sell":
		*s = Sell
	default:
		return fmt.Errorf("%w: side %q", ErrInvalidCommand, b)
	}
	return nil
}

// OrderType is limit or market.
type OrderType uint8

// Order types.
const (
	Limit OrderType = iota + 1
	Market
)

func (t OrderType) String() string {
	switch t {
	case Limit:
		return "limit"
	case Market:
		return "market"
	}
	return fmt.Sprintf("type(%d)", uint8(t))
}

// MarshalText implements encoding.TextMarshaler.
func (t OrderType) MarshalText() ([]byte, error) {
	return enumText(t.String(), t == Limit || t == Market)
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (t *OrderType) UnmarshalText(b []byte) error {
	switch string(b) {
	case "limit":
		*t = Limit
	case "market":
		*t = Market
	default:
		return fmt.Errorf("%w: order type %q", ErrInvalidCommand, b)
	}
	return nil
}

// TimeInForce says what happens to the unfilled remainder: GTC rests in the
// book, IOC cancels it. Market orders are always IOC.
type TimeInForce uint8

// Time-in-force values. The zero value means "default" (GTC for limit).
const (
	GTC TimeInForce = iota + 1
	IOC
)

func (t TimeInForce) String() string {
	switch t {
	case GTC:
		return "gtc"
	case IOC:
		return "ioc"
	}
	return fmt.Sprintf("tif(%d)", uint8(t))
}

// MarshalText implements encoding.TextMarshaler.
func (t TimeInForce) MarshalText() ([]byte, error) {
	if t == 0 {
		return []byte(""), nil
	}
	return enumText(t.String(), t == GTC || t == IOC)
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (t *TimeInForce) UnmarshalText(b []byte) error {
	switch string(b) {
	case "", "gtc":
		*t = GTC
	case "ioc":
		*t = IOC
	default:
		return fmt.Errorf("%w: time in force %q", ErrInvalidCommand, b)
	}
	return nil
}

// SelfTradePolicy decides what happens when a new order would trade against
// a resting order of the same account.
type SelfTradePolicy uint8

// Self-trade policies. v1 implements CancelNewest (docs/plan-v1.0.md §6.3);
// Allow exists for tests; CancelOldest is rejected by New.
const (
	STPCancelNewest SelfTradePolicy = iota + 1
	STPAllow
	STPCancelOldest
)

func (p SelfTradePolicy) String() string {
	switch p {
	case STPCancelNewest:
		return "cancel_newest"
	case STPAllow:
		return "allow"
	case STPCancelOldest:
		return "cancel_oldest"
	}
	return fmt.Sprintf("stp(%d)", uint8(p))
}

// MarshalText implements encoding.TextMarshaler.
func (p SelfTradePolicy) MarshalText() ([]byte, error) {
	return enumText(p.String(), p >= STPCancelNewest && p <= STPCancelOldest)
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (p *SelfTradePolicy) UnmarshalText(b []byte) error {
	switch string(b) {
	case "cancel_newest":
		*p = STPCancelNewest
	case "allow":
		*p = STPAllow
	case "cancel_oldest":
		*p = STPCancelOldest
	default:
		return fmt.Errorf("%w: self trade policy %q", ErrInvalidCommand, b)
	}
	return nil
}

func enumText(s string, ok bool) ([]byte, error) {
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrInvalidCommand, s)
	}
	return []byte(s), nil
}

// OrderID is opaque to the book (a ULID assigned by internal/trading).
type OrderID string

// AccountID is the ledger account an order belongs to.
type AccountID string

// MarketConfig is the subset of the registry market the book needs.
type MarketConfig struct {
	Symbol          string          `json:"symbol"`
	PriceTick       money.Amount    `json:"price_tick"`
	QtyStep         money.Amount    `json:"qty_step"`
	MinNotional     money.Amount    `json:"min_notional"`
	MaxQty          *money.Amount   `json:"max_qty,omitempty"`
	MaxSlippageBps  *int32          `json:"max_slippage_bps,omitempty"`
	BaseScale       int32           `json:"base_scale"`
	QuoteScale      int32           `json:"quote_scale"`
	SelfTradePolicy SelfTradePolicy `json:"self_trade_policy"`
}

// Errors returned by New, Restore and Apply. They signal caller bugs
// (wrong seq, malformed command, unsupported config), never business
// outcomes: those are Rejected / CancelRejected events.
var (
	ErrInvalidConfig     = errors.New("matching: invalid market config")
	ErrUnsupportedSTP    = errors.New("matching: self trade policy not implemented")
	ErrInvalidCommand    = errors.New("matching: invalid command")
	ErrSeqNotIncreasing  = errors.New("matching: command seq must increase")
	ErrInvalidResting    = errors.New("matching: invalid resting order")
	ErrDuplicateOrderID  = errors.New("matching: duplicate order id")
	errInternalPrecision = errors.New("matching: internal precision error")
)

// Validate checks the config; New calls it.
func (c MarketConfig) Validate() error {
	switch {
	case c.Symbol == "":
		return fmt.Errorf("%w: empty symbol", ErrInvalidConfig)
	case !c.PriceTick.IsPositive():
		return fmt.Errorf("%w: price_tick must be positive", ErrInvalidConfig)
	case !c.QtyStep.IsPositive():
		return fmt.Errorf("%w: qty_step must be positive", ErrInvalidConfig)
	case c.MinNotional.IsNegative():
		return fmt.Errorf("%w: min_notional must not be negative", ErrInvalidConfig)
	case c.BaseScale < 0 || c.BaseScale > money.MaxScale || c.QuoteScale < 0 || c.QuoteScale > money.MaxScale:
		return fmt.Errorf("%w: scales must be within 0..%d", ErrInvalidConfig, money.MaxScale)
	case c.QtyStep.Scale() > c.BaseScale:
		return fmt.Errorf("%w: qty_step has more decimals than base scale", ErrInvalidConfig)
	case c.PriceTick.Scale()+c.QtyStep.Scale() > c.QuoteScale:
		// docs/plan-v1.0.md §6.5: guarantees price × qty is exact in quote scale.
		return fmt.Errorf("%w: scale(qty_step)+scale(price_tick) must be <= quote scale", ErrInvalidConfig)
	case c.MaxQty != nil && !c.MaxQty.IsPositive():
		return fmt.Errorf("%w: max_qty must be positive when set", ErrInvalidConfig)
	case c.MaxSlippageBps != nil && (*c.MaxSlippageBps < 1 || *c.MaxSlippageBps > 10000):
		return fmt.Errorf("%w: max_slippage_bps must be within 1..10000", ErrInvalidConfig)
	}
	switch c.SelfTradePolicy {
	case STPCancelNewest, STPAllow:
	case STPCancelOldest:
		return fmt.Errorf("%w: cancel_oldest", ErrUnsupportedSTP)
	default:
		return fmt.Errorf("%w: unknown self trade policy", ErrInvalidConfig)
	}
	return nil
}

// NewOrder is the payload of a new-order command.
//
// Limit orders carry Price and Qty (base). Market sells carry Qty (base).
// Market buys carry QuoteQty (the quote budget) and no Qty: the base amount
// is only known once the order trades (docs/plan-v1.0.md §6.3).
type NewOrder struct {
	OrderID     OrderID      `json:"order_id"`
	AccountID   AccountID    `json:"account_id"`
	Side        Side         `json:"side"`
	Type        OrderType    `json:"type"`
	TimeInForce TimeInForce  `json:"time_in_force,omitempty"`
	Price       money.Amount `json:"price"`
	Qty         money.Amount `json:"qty"`
	QuoteQty    money.Amount `json:"quote_qty"`
}

// Cancel is the payload of a cancel command. AccountID is optional; when
// set, the cancel is rejected unless it matches the resting order's owner.
type Cancel struct {
	OrderID   OrderID   `json:"order_id"`
	AccountID AccountID `json:"account_id,omitempty"`
}

// Command is one unit of work for Apply. Exactly one of New or Cancel is
// set. Seq and Timestamp are assigned by the caller.
type Command struct {
	Seq       uint64    `json:"seq"`
	Timestamp time.Time `json:"ts"`
	New       *NewOrder `json:"new,omitempty"`
	Cancel    *Cancel   `json:"cancel,omitempty"`
}

// RestingOrder is a limit order sitting in the book, as persisted by the
// engine and as returned by Snapshot. Only limit GTC orders rest.
type RestingOrder struct {
	OrderID     OrderID      `json:"order_id"`
	AccountID   AccountID    `json:"account_id"`
	Side        Side         `json:"side"`
	Price       money.Amount `json:"price"`
	Qty         money.Amount `json:"qty"`
	Remaining   money.Amount `json:"remaining"`
	FilledQty   money.Amount `json:"filled_qty"`
	FilledQuote money.Amount `json:"filled_quote"`
	Seq         uint64       `json:"seq"`
	Timestamp   time.Time    `json:"ts"`
}

// Equal compares every field using amount equality (1990 == 1990.00).
func (r RestingOrder) Equal(o RestingOrder) bool {
	return r.OrderID == o.OrderID && r.AccountID == o.AccountID && r.Side == o.Side &&
		r.Price.Equal(o.Price) && r.Qty.Equal(o.Qty) && r.Remaining.Equal(o.Remaining) &&
		r.FilledQty.Equal(o.FilledQty) && r.FilledQuote.Equal(o.FilledQuote) &&
		r.Seq == o.Seq && r.Timestamp.Equal(o.Timestamp)
}

// Snapshot is the full state of a book: every resting order, best price
// first, FIFO within a level.
type Snapshot struct {
	Symbol  string         `json:"symbol"`
	LastSeq uint64         `json:"last_seq"`
	Bids    []RestingOrder `json:"bids"`
	Asks    []RestingOrder `json:"asks"`
}

// Equal reports whether two snapshots describe the same book.
func (s Snapshot) Equal(o Snapshot) bool {
	if s.Symbol != o.Symbol || s.LastSeq != o.LastSeq || len(s.Bids) != len(o.Bids) || len(s.Asks) != len(o.Asks) {
		return false
	}
	for i := range s.Bids {
		if !s.Bids[i].Equal(o.Bids[i]) {
			return false
		}
	}
	for i := range s.Asks {
		if !s.Asks[i].Equal(o.Asks[i]) {
			return false
		}
	}
	return true
}

// Level is one aggregated price level of the depth view.
type Level struct {
	Price  money.Amount `json:"price"`
	Qty    money.Amount `json:"qty"`
	Orders int          `json:"orders"`
}

// Depth is the aggregated market-data view of a book.
type Depth struct {
	Symbol  string  `json:"symbol"`
	LastSeq uint64  `json:"last_seq"`
	Bids    []Level `json:"bids"`
	Asks    []Level `json:"asks"`
}
