package trading

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// Event types produced by the engine (docs/plan-v1.0.md §7.2).
const (
	EventOrderAccepted  = "order.accepted"
	EventOrderUpdated   = "order.updated"
	EventOrderFilled    = "order.filled"
	EventOrderCancelled = "order.cancelled"
	EventOrderRejected  = "order.rejected"
	EventTradeExecuted  = "trade.executed"
	EventBalanceUpdated = "balance.updated"
)

// schemaVersion of every payload below. Adding a field keeps the version;
// changing a field's meaning bumps it (docs/plan-v1.0.md §7.1).
const schemaVersion = 1

// Payloads. Amounts serialize as decimal strings (money.Amount).

// OrderAcceptedPayload is order.accepted: emitted for every non-rejected
// order, including ones that fill in the same command, so private-stream
// clients can map later events to their client_order_id.
type OrderAcceptedPayload struct {
	OrderID       string        `json:"order_id"`
	ClientOrderID string        `json:"client_order_id"`
	AccountID     string        `json:"account_id"`
	Market        string        `json:"market"`
	Side          string        `json:"side"`
	Type          string        `json:"type"`
	TimeInForce   string        `json:"time_in_force"`
	Price         *money.Amount `json:"price"`
	Qty           *money.Amount `json:"qty"`
	QuoteQty      *money.Amount `json:"quote_qty"`
	Seq           uint64        `json:"seq"`
}

// OrderUpdatedPayload is order.updated: a partially filled order that stays open.
type OrderUpdatedPayload struct {
	OrderID      string       `json:"order_id"`
	AccountID    string       `json:"account_id"`
	Market       string       `json:"market"`
	Status       Status       `json:"status"`
	FilledQty    money.Amount `json:"filled_qty"`
	FilledQuote  money.Amount `json:"filled_quote"`
	RemainingQty money.Amount `json:"remaining_qty"`
	Seq          uint64       `json:"seq"`
}

// OrderFilledPayload is order.filled.
type OrderFilledPayload struct {
	OrderID     string       `json:"order_id"`
	AccountID   string       `json:"account_id"`
	Market      string       `json:"market"`
	FilledQty   money.Amount `json:"filled_qty"`
	FilledQuote money.Amount `json:"filled_quote"`
	Seq         uint64       `json:"seq"`
}

// OrderCancelledPayload is order.cancelled: user cancel, IOC remainder,
// self-trade prevention or price protection. Released is the hold given
// back to available.
type OrderCancelledPayload struct {
	OrderID        string                `json:"order_id"`
	AccountID      string                `json:"account_id"`
	Market         string                `json:"market"`
	Reason         matching.CancelReason `json:"reason"`
	FilledQty      money.Amount          `json:"filled_qty"`
	FilledQuote    money.Amount          `json:"filled_quote"`
	RemainingQty   money.Amount          `json:"remaining_qty"`
	RemainingQuote money.Amount          `json:"remaining_quote"`
	Released       money.Amount          `json:"released"`
	ReleasedAsset  string                `json:"released_asset"`
	Seq            uint64                `json:"seq"`
}

// OrderRejectedPayload is order.rejected. Seq is absent for orders refused
// before they reached the book (policy, malformed parameters).
type OrderRejectedPayload struct {
	OrderID       string       `json:"order_id"`
	ClientOrderID string       `json:"client_order_id"`
	AccountID     string       `json:"account_id"`
	Market        string       `json:"market"`
	Reason        RejectReason `json:"reason"`
	Seq           *uint64      `json:"seq"`
}

// TradeExecutedPayload is trade.executed.
type TradeExecutedPayload struct {
	TradeID        string       `json:"trade_id"`
	Market         string       `json:"market"`
	MakerOrderID   string       `json:"maker_order_id"`
	TakerOrderID   string       `json:"taker_order_id"`
	MakerAccountID string       `json:"maker_account_id"`
	TakerAccountID string       `json:"taker_account_id"`
	TakerSide      string       `json:"taker_side"`
	Price          money.Amount `json:"price"`
	Qty            money.Amount `json:"qty"`
	QuoteQty       money.Amount `json:"quote_qty"`
	MakerFee       money.Amount `json:"maker_fee"`
	MakerFeeAsset  string       `json:"maker_fee_asset"`
	TakerFee       money.Amount `json:"taker_fee"`
	TakerFeeAsset  string       `json:"taker_fee_asset"`
	Seq            uint64       `json:"seq"`
}

// BalanceUpdatedPayload is balance.updated: the account's balance in one
// asset after the command.
type BalanceUpdatedPayload struct {
	AccountID  string       `json:"account_id"`
	Asset      string       `json:"asset"`
	Available  money.Amount `json:"available"`
	Hold       money.Amount `json:"hold"`
	AccountSeq int64        `json:"account_seq"`
}

// eventBuilder collects the envelopes of one command; account-domain
// envelopes get their account_seq when they are appended (inside the
// transaction), so the builder only stores what is known before.
type eventBuilder struct {
	tenant      string
	market      string
	now         time.Time
	correlation string
	seq         *uint64
	items       []pendingEvent
}

type pendingEvent struct {
	eventType string
	account   string // "" for market-only events
	payload   any
	marketEvt bool // scope by market
}

func (b *eventBuilder) add(eventType, account string, payload any) {
	b.items = append(b.items, pendingEvent{eventType: eventType, account: account, payload: payload, marketEvt: true})
}

// addAccountOnly adds an event scoped by account (balance.updated).
func (b *eventBuilder) addAccountOnly(eventType, account string, payload any) {
	b.items = append(b.items, pendingEvent{eventType: eventType, account: account, payload: payload})
}

// envelope materialises one pending event; accountSeq is 0 for market-only
// events.
func (b *eventBuilder) envelope(p pendingEvent, accountSeq int64) (eventbus.Envelope, error) {
	body, err := json.Marshal(p.payload)
	if err != nil {
		return eventbus.Envelope{}, fmt.Errorf("trading: marshal %s: %w", p.eventType, err)
	}
	e := eventbus.Envelope{
		EventID: eventbus.NewID(b.now), EventType: p.eventType, SchemaVersion: schemaVersion, TenantID: b.tenant,
		AccountID: eventbus.Str(p.account), OccurredAt: b.now, CorrelationID: b.correlation, Payload: body,
	}
	if p.marketEvt {
		e.MarketID = eventbus.Str(b.market)
		e.Seq = b.seq
	}
	if p.account != "" {
		e.AccountSeq = eventbus.I64(accountSeq)
	}
	return e, nil
}

// balanceEvents turns the balances returned by the ledger during one
// command into one balance.updated per (account, asset), latest value wins.
func balanceEvents(entries []ledger.JournalEntry) []ledger.Balance {
	type key struct{ account, asset string }
	latest := map[key]ledger.Balance{}
	order := []key{}
	for _, e := range entries {
		for _, bal := range e.Balances {
			k := key{bal.AccountID, bal.Asset}
			if _, seen := latest[k]; !seen {
				order = append(order, k)
			}
			latest[k] = bal
		}
	}
	out := make([]ledger.Balance, 0, len(order))
	for _, k := range order {
		out = append(out, latest[k])
	}
	return out
}
