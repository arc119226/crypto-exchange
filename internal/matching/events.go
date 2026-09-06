package matching

import (
	"encoding/json"
	"fmt"

	"github.com/arc119226/crypto-exchange/internal/money"
)

// EventKind discriminates Event implementations (and their JSON form).
type EventKind string

// Event kinds. They map onto the event catalog of docs/plan-v1.0.md §7.2:
// accepted → order.accepted, trade → trade.executed, updated → order.updated,
// filled → order.filled, cancelled → order.cancelled, rejected →
// order.rejected. cancel_rejected has no public event: trading answers the
// cancel request with the order's current state.
const (
	KindAccepted       EventKind = "accepted"
	KindTrade          EventKind = "trade"
	KindUpdated        EventKind = "updated"
	KindFilled         EventKind = "filled"
	KindCancelled      EventKind = "cancelled"
	KindRejected       EventKind = "rejected"
	KindCancelRejected EventKind = "cancel_rejected"
)

// RejectReason explains why a new order produced no Accepted event.
type RejectReason string

// Reject reasons (docs/plan-v1.0.md §6.2 plus the additions of
// docs/domain.md E2).
const (
	RejectInvalidOrder     RejectReason = "invalid_order"
	RejectInvalidPriceTick RejectReason = "invalid_price_tick"
	RejectInvalidQtyStep   RejectReason = "invalid_qty_step"
	RejectBelowMinNotional RejectReason = "below_min_notional"
	RejectAboveMaxQty      RejectReason = "above_max_qty"
	RejectDuplicateOrderID RejectReason = "duplicate_order_id"
	RejectEmptyBook        RejectReason = "empty_book"
	RejectQuoteQtyTooSmall RejectReason = "quote_qty_too_small"
)

// CancelReason explains why an accepted order (or its remainder) left the
// book without trading.
type CancelReason string

// Cancel reasons.
const (
	CancelByUser          CancelReason = "user"
	CancelIOC             CancelReason = "ioc"
	CancelSelfTrade       CancelReason = "self_trade"
	CancelPriceProtection CancelReason = "price_protection"
)

// CancelRejectReason explains a refused cancel command.
type CancelRejectReason string

// Cancel-reject reasons.
const (
	CancelRejectUnknownOrder CancelRejectReason = "unknown_order"
	CancelRejectNotOwner     CancelRejectReason = "not_owner"
)

// Event is one output of Apply. Concrete types: Accepted, Trade, Updated,
// Filled, Cancelled, Rejected, CancelRejected.
type Event interface {
	Kind() EventKind
	// CommandSeq is the seq of the command that produced the event.
	CommandSeq() uint64
}

// Accepted is emitted first for every new order that is not rejected,
// including orders that fill immediately, so private-stream clients can map
// later events back to their client_order_id.
type Accepted struct {
	Seq         uint64       `json:"seq"`
	OrderID     OrderID      `json:"order_id"`
	AccountID   AccountID    `json:"account_id"`
	Side        Side         `json:"side"`
	Type        OrderType    `json:"type"`
	TimeInForce TimeInForce  `json:"time_in_force"`
	Price       money.Amount `json:"price"`
	Qty         money.Amount `json:"qty"`
	QuoteQty    money.Amount `json:"quote_qty"`
}

// Trade is one fill. Price is always the maker's (resting) price. Index
// numbers the trades within one command so the engine can derive
// deterministic trade ids.
type Trade struct {
	Seq            uint64       `json:"seq"`
	Index          int          `json:"index"`
	MakerOrderID   OrderID      `json:"maker_order_id"`
	TakerOrderID   OrderID      `json:"taker_order_id"`
	MakerAccountID AccountID    `json:"maker_account_id"`
	TakerAccountID AccountID    `json:"taker_account_id"`
	TakerSide      Side         `json:"taker_side"`
	Price          money.Amount `json:"price"`
	Qty            money.Amount `json:"qty"`
	QuoteQty       money.Amount `json:"quote_qty"`
	MakerRemaining money.Amount `json:"maker_remaining"`
}

// Updated is emitted for a partially filled order that stays in the book
// (a maker after a trade, or a taker that rests after partial fills).
type Updated struct {
	Seq          uint64       `json:"seq"`
	OrderID      OrderID      `json:"order_id"`
	AccountID    AccountID    `json:"account_id"`
	FilledQty    money.Amount `json:"filled_qty"`
	FilledQuote  money.Amount `json:"filled_quote"`
	RemainingQty money.Amount `json:"remaining_qty"`
}

// Filled is emitted when an order's quantity (or quote budget) is fully
// consumed.
type Filled struct {
	Seq         uint64       `json:"seq"`
	OrderID     OrderID      `json:"order_id"`
	AccountID   AccountID    `json:"account_id"`
	FilledQty   money.Amount `json:"filled_qty"`
	FilledQuote money.Amount `json:"filled_quote"`
}

// Cancelled is emitted when an order leaves the book with quantity left.
// RemainingQty is base; RemainingQuote is the unspent quote budget of a
// market buy (zero otherwise). The ledger releases the matching hold.
type Cancelled struct {
	Seq            uint64       `json:"seq"`
	OrderID        OrderID      `json:"order_id"`
	AccountID      AccountID    `json:"account_id"`
	Reason         CancelReason `json:"reason"`
	FilledQty      money.Amount `json:"filled_qty"`
	FilledQuote    money.Amount `json:"filled_quote"`
	RemainingQty   money.Amount `json:"remaining_qty"`
	RemainingQuote money.Amount `json:"remaining_quote"`
}

// Rejected is emitted instead of Accepted; nothing was held or traded.
type Rejected struct {
	Seq       uint64       `json:"seq"`
	OrderID   OrderID      `json:"order_id"`
	AccountID AccountID    `json:"account_id"`
	Reason    RejectReason `json:"reason"`
}

// CancelRejected is emitted for a cancel that found no matching resting
// order.
type CancelRejected struct {
	Seq     uint64             `json:"seq"`
	OrderID OrderID            `json:"order_id"`
	Reason  CancelRejectReason `json:"reason"`
}

// Kind implements Event.
func (Accepted) Kind() EventKind { return KindAccepted }

// Kind implements Event.
func (Trade) Kind() EventKind { return KindTrade }

// Kind implements Event.
func (Updated) Kind() EventKind { return KindUpdated }

// Kind implements Event.
func (Filled) Kind() EventKind { return KindFilled }

// Kind implements Event.
func (Cancelled) Kind() EventKind { return KindCancelled }

// Kind implements Event.
func (Rejected) Kind() EventKind { return KindRejected }

// Kind implements Event.
func (CancelRejected) Kind() EventKind { return KindCancelRejected }

// CommandSeq implements Event.
func (e Accepted) CommandSeq() uint64 { return e.Seq }

// CommandSeq implements Event.
func (e Trade) CommandSeq() uint64 { return e.Seq }

// CommandSeq implements Event.
func (e Updated) CommandSeq() uint64 { return e.Seq }

// CommandSeq implements Event.
func (e Filled) CommandSeq() uint64 { return e.Seq }

// CommandSeq implements Event.
func (e Cancelled) CommandSeq() uint64 { return e.Seq }

// CommandSeq implements Event.
func (e Rejected) CommandSeq() uint64 { return e.Seq }

// CommandSeq implements Event.
func (e CancelRejected) CommandSeq() uint64 { return e.Seq }

type eventEnvelope struct {
	Kind EventKind       `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// EncodeEvent serializes an event as {"kind": ..., "data": {...}}. The
// format is the golden-file and replay format; it is stable across runs
// because money amounts serialize canonically.
func EncodeEvent(e Event) ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return json.Marshal(eventEnvelope{Kind: e.Kind(), Data: data})
}

// DecodeEvent is the inverse of EncodeEvent.
func DecodeEvent(b []byte) (Event, error) {
	var env eventEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, err
	}
	var (
		ev  Event
		err error
	)
	switch env.Kind {
	case KindAccepted:
		var e Accepted
		err, ev = json.Unmarshal(env.Data, &e), e
	case KindTrade:
		var e Trade
		err, ev = json.Unmarshal(env.Data, &e), e
	case KindUpdated:
		var e Updated
		err, ev = json.Unmarshal(env.Data, &e), e
	case KindFilled:
		var e Filled
		err, ev = json.Unmarshal(env.Data, &e), e
	case KindCancelled:
		var e Cancelled
		err, ev = json.Unmarshal(env.Data, &e), e
	case KindRejected:
		var e Rejected
		err, ev = json.Unmarshal(env.Data, &e), e
	case KindCancelRejected:
		var e CancelRejected
		err, ev = json.Unmarshal(env.Data, &e), e
	default:
		return nil, fmt.Errorf("matching: unknown event kind %q", env.Kind)
	}
	if err != nil {
		return nil, err
	}
	return ev, nil
}
