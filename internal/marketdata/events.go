package marketdata

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// Event types this package consumes. They are the public contract of
// api/events/v1 (docs/events.md); the names are repeated here because
// marketdata must not import internal/trading (docs/plan-v1.0.md §8).
const (
	EventOrderAccepted  = "order.accepted"
	EventOrderUpdated   = "order.updated"
	EventOrderFilled    = "order.filled"
	EventOrderCancelled = "order.cancelled"
	EventOrderRejected  = "order.rejected"
	EventTradeExecuted  = "trade.executed"
)

// Side of the book an order rests on.
type Side string

// Sides. The strings are the wire form of matching.Side.
const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

// Opposite returns the other side.
func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

func (s Side) valid() bool { return s == Buy || s == Sell }

// orderAccepted is the part of order.accepted the book needs.
type orderAccepted struct {
	OrderID     string        `json:"order_id"`
	Market      string        `json:"market"`
	Side        Side          `json:"side"`
	Type        string        `json:"type"`
	TimeInForce string        `json:"time_in_force"`
	Price       *money.Amount `json:"price"`
	Qty         *money.Amount `json:"qty"`
}

// orderProgress is the common shape of order.updated, order.filled and
// order.cancelled: what happened to one order. remaining_qty is absent on
// order.filled, which decodes as zero -- exactly what a filled order has.
type orderProgress struct {
	OrderID      string       `json:"order_id"`
	Market       string       `json:"market"`
	RemainingQty money.Amount `json:"remaining_qty"`
}

// tradeExecuted is trade.executed.
type tradeExecuted struct {
	TradeID      string       `json:"trade_id"`
	Market       string       `json:"market"`
	MakerOrderID string       `json:"maker_order_id"`
	TakerOrderID string       `json:"taker_order_id"`
	TakerSide    Side         `json:"taker_side"`
	Price        money.Amount `json:"price"`
	Qty          money.Amount `json:"qty"`
	QuoteQty     money.Amount `json:"quote_qty"`
}

// TradeTick is one execution as the aggregators see it: the trade.executed
// payload plus the envelope's time. Index is the position within the
// command; envelopes do not carry it (0), rows of trading.trades do.
type TradeTick struct {
	TradeID   string
	Market    string
	Seq       uint64
	Index     int
	Price     money.Amount
	Qty       money.Amount
	QuoteQty  money.Amount
	TakerSide Side
	At        time.Time
}

// TradeFromEnvelope decodes a trade.executed envelope.
func TradeFromEnvelope(e eventbus.Envelope) (TradeTick, error) {
	if e.EventType != EventTradeExecuted {
		return TradeTick{}, fmt.Errorf("marketdata: %s is not a trade", e.EventType)
	}
	var p tradeExecuted
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return TradeTick{}, fmt.Errorf("marketdata: decode %s: %w", e.EventType, err)
	}
	if e.Seq == nil {
		return TradeTick{}, fmt.Errorf("marketdata: trade %s without seq", p.TradeID)
	}
	if !p.Price.IsPositive() || !p.Qty.IsPositive() {
		return TradeTick{}, fmt.Errorf("marketdata: trade %s with non-positive price or qty", p.TradeID)
	}
	return TradeTick{
		TradeID: p.TradeID, Market: p.Market, Seq: *e.Seq,
		Price: p.Price, Qty: p.Qty, QuoteQty: p.QuoteQty, TakerSide: p.TakerSide, At: e.OccurredAt,
	}, nil
}

func decode[T any](e eventbus.Envelope) (T, error) {
	var v T
	if err := json.Unmarshal(e.Payload, &v); err != nil {
		return v, fmt.Errorf("marketdata: decode %s: %w", e.EventType, err)
	}
	return v, nil
}
