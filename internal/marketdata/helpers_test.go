package marketdata_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/marketdata"
	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/money"
)

const market = "ETH-USDC"

var t0 = time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)

func amt(s string) money.Amount { return money.MustParse(s) }

func ptr(s string) *money.Amount { a := amt(s); return &a }

func ts(seq uint64) time.Time { return t0.Add(time.Duration(seq) * time.Second) }

// envelope builds one event the way trading.eventBuilder does for a
// market-scoped event.
func envelope(t testing.TB, eventType string, seq uint64, at time.Time, payload any) eventbus.Envelope {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	m := market
	return eventbus.Envelope{
		EventID: eventbus.NewID(at), EventType: eventType, SchemaVersion: 1, TenantID: "default",
		MarketID: &m, Seq: eventbus.U64(seq), OccurredAt: at, Payload: body,
	}
}

// accepted, trade, updated, filled, cancelled, rejected mirror the payload
// shapes of internal/trading/events.go.
func accepted(t testing.TB, seq uint64, id, side, typ, tif string, price, qty *money.Amount) eventbus.Envelope {
	t.Helper()
	return envelope(t, marketdata.EventOrderAccepted, seq, ts(seq), map[string]any{
		"order_id": id, "client_order_id": "c-" + id, "account_id": "A", "market": market,
		"side": side, "type": typ, "time_in_force": tif, "price": price, "qty": qty, "quote_qty": nil, "seq": seq,
	})
}

func trade(t testing.TB, seq uint64, id, maker, taker, takerSide string, price, qty money.Amount) eventbus.Envelope {
	t.Helper()
	return envelope(t, marketdata.EventTradeExecuted, seq, ts(seq), map[string]any{
		"trade_id": id, "market": market, "maker_order_id": maker, "taker_order_id": taker,
		"maker_account_id": "M", "taker_account_id": "T", "taker_side": takerSide,
		"price": price, "qty": qty, "quote_qty": price.Mul(qty), "maker_fee": "0", "maker_fee_asset": "USDC",
		"taker_fee": "0", "taker_fee_asset": "USDC", "seq": seq,
	})
}

func updated(t testing.TB, seq uint64, id string, remaining money.Amount) eventbus.Envelope {
	t.Helper()
	return envelope(t, marketdata.EventOrderUpdated, seq, ts(seq), map[string]any{
		"order_id": id, "account_id": "A", "market": market, "status": "partially_filled",
		"filled_qty": "0", "filled_quote": "0", "remaining_qty": remaining, "seq": seq,
	})
}

func filled(t testing.TB, seq uint64, id string) eventbus.Envelope {
	t.Helper()
	return envelope(t, marketdata.EventOrderFilled, seq, ts(seq), map[string]any{
		"order_id": id, "account_id": "A", "market": market, "filled_qty": "0", "filled_quote": "0", "seq": seq,
	})
}

func cancelled(t testing.TB, seq uint64, id, reason string, remaining money.Amount) eventbus.Envelope {
	t.Helper()
	return envelope(t, marketdata.EventOrderCancelled, seq, ts(seq), map[string]any{
		"order_id": id, "account_id": "A", "market": market, "reason": reason,
		"filled_qty": "0", "filled_quote": "0", "remaining_qty": remaining, "remaining_quote": "0",
		"released": "0", "released_asset": "USDC", "seq": seq,
	})
}

func rejected(t testing.TB, seq uint64, id string) eventbus.Envelope {
	t.Helper()
	return envelope(t, marketdata.EventOrderRejected, seq, ts(seq), map[string]any{
		"order_id": id, "client_order_id": "c-" + id, "account_id": "A", "market": market, "reason": "empty_book", "seq": seq,
	})
}

// envelopesOf converts the events of one matching command into the
// envelopes the runner would append, in the same order
// (internal/trading/runner.go persistAccepted / cancelOrder / writeRejected).
// seq is the projector-side seq; it differs from cmd.Seq when a fixture
// contains rejected cancels, which the runner rolls back without consuming a
// seq while matching.Book still advances.
func envelopesOf(t testing.TB, cmd matching.Command, evs []matching.Event, seq uint64) []eventbus.Envelope {
	t.Helper()
	at := ts(seq)
	out := make([]eventbus.Envelope, 0, len(evs))
	for _, e := range evs {
		switch ev := e.(type) {
		case matching.Accepted:
			var price, qty, quote *money.Amount
			if ev.Price.IsPositive() {
				p := ev.Price
				price = &p
			}
			if ev.Qty.IsPositive() {
				q := ev.Qty
				qty = &q
			}
			if ev.QuoteQty.IsPositive() {
				q := ev.QuoteQty
				quote = &q
			}
			out = append(out, envelope(t, marketdata.EventOrderAccepted, seq, at, map[string]any{
				"order_id": string(ev.OrderID), "client_order_id": "c-" + string(ev.OrderID), "account_id": string(ev.AccountID),
				"market": market, "side": ev.Side.String(), "type": ev.Type.String(), "time_in_force": ev.TimeInForce.String(),
				"price": price, "qty": qty, "quote_qty": quote, "seq": seq,
			}))
		case matching.Trade:
			out = append(out, envelope(t, marketdata.EventTradeExecuted, seq, at, map[string]any{
				"trade_id": string(ev.TakerOrderID) + "-" + string(ev.MakerOrderID), "market": market,
				"maker_order_id": string(ev.MakerOrderID), "taker_order_id": string(ev.TakerOrderID),
				"maker_account_id": string(ev.MakerAccountID), "taker_account_id": string(ev.TakerAccountID),
				"taker_side": ev.TakerSide.String(), "price": ev.Price, "qty": ev.Qty, "quote_qty": ev.QuoteQty,
				"maker_fee": "0", "maker_fee_asset": "USDC", "taker_fee": "0", "taker_fee_asset": "USDC", "seq": seq,
			}))
		case matching.Updated:
			out = append(out, envelope(t, marketdata.EventOrderUpdated, seq, at, map[string]any{
				"order_id": string(ev.OrderID), "account_id": string(ev.AccountID), "market": market, "status": "partially_filled",
				"filled_qty": ev.FilledQty, "filled_quote": ev.FilledQuote, "remaining_qty": ev.RemainingQty, "seq": seq,
			}))
		case matching.Filled:
			out = append(out, envelope(t, marketdata.EventOrderFilled, seq, at, map[string]any{
				"order_id": string(ev.OrderID), "account_id": string(ev.AccountID), "market": market,
				"filled_qty": ev.FilledQty, "filled_quote": ev.FilledQuote, "seq": seq,
			}))
		case matching.Cancelled:
			out = append(out, envelope(t, marketdata.EventOrderCancelled, seq, at, map[string]any{
				"order_id": string(ev.OrderID), "account_id": string(ev.AccountID), "market": market, "reason": string(ev.Reason),
				"filled_qty": ev.FilledQty, "filled_quote": ev.FilledQuote, "remaining_qty": ev.RemainingQty, "remaining_quote": ev.RemainingQuote,
				"released": "0", "released_asset": "USDC", "seq": seq,
			}))
		case matching.Rejected:
			out = append(out, envelope(t, marketdata.EventOrderRejected, seq, at, map[string]any{
				"order_id": string(ev.OrderID), "client_order_id": "c-" + string(ev.OrderID), "account_id": string(ev.AccountID),
				"market": market, "reason": string(ev.Reason), "seq": seq,
			}))
		case matching.CancelRejected:
			t.Fatalf("command %d: cancel rejected commands are not persisted; skip them before converting", cmd.Seq)
		}
	}
	return out
}

// depthMaps flattens a depth into price -> qty per side.
func depthMaps(d marketdata.Depth) (bids, asks map[string]money.Amount) {
	bids, asks = map[string]money.Amount{}, map[string]money.Amount{}
	for _, l := range d.Bids {
		bids[l.Price.String()] = l.Qty
	}
	for _, l := range d.Asks {
		asks[l.Price.String()] = l.Qty
	}
	return bids, asks
}

// requireSameDepth compares the shadow book with the engine's aggregated
// view: same levels, same quantities, same order counts.
func requireSameDepth(t require.TestingT, want matching.Depth, got marketdata.Depth) {
	require.Len(t, got.Bids, len(want.Bids), "bid levels")
	require.Len(t, got.Asks, len(want.Asks), "ask levels")
	for i := range want.Bids {
		require.Equal(t, want.Bids[i].Price.String(), got.Bids[i].Price.String(), "bid %d price", i)
		require.True(t, want.Bids[i].Qty.Equal(got.Bids[i].Qty), "bid %s qty: want %s got %s", want.Bids[i].Price, want.Bids[i].Qty, got.Bids[i].Qty)
		require.Equal(t, want.Bids[i].Orders, got.Bids[i].Orders, "bid %s orders", want.Bids[i].Price)
	}
	for i := range want.Asks {
		require.Equal(t, want.Asks[i].Price.String(), got.Asks[i].Price.String(), "ask %d price", i)
		require.True(t, want.Asks[i].Qty.Equal(got.Asks[i].Qty), "ask %s qty: want %s got %s", want.Asks[i].Price, want.Asks[i].Qty, got.Asks[i].Qty)
		require.Equal(t, want.Asks[i].Orders, got.Asks[i].Orders, "ask %s orders", want.Asks[i].Price)
	}
}

func requireSameMaps(t require.TestingT, want, got map[string]money.Amount, what string) {
	require.Len(t, got, len(want), "%s: level count", what)
	for p, q := range want {
		g, ok := got[p]
		require.True(t, ok, "%s: level %s missing after deltas", what, p)
		require.True(t, q.Equal(g), "%s: level %s want %s got %s", what, p, q, g)
	}
}

// live returns a Live projector at seq 0 with an empty book.
func live(t testing.TB) *marketdata.BookProjector {
	t.Helper()
	p := marketdata.NewBookProjector(market, nil)
	p.MarkRebuilding()
	_, err := p.Restore(marketdata.BookSnapshot{Market: market})
	require.NoError(t, err)
	return p
}

func apply(t testing.TB, p *marketdata.BookProjector, es ...eventbus.Envelope) []marketdata.Delta {
	t.Helper()
	var out []marketdata.Delta
	for _, e := range es {
		ds, err := p.Apply(e)
		require.NoError(t, err)
		out = append(out, ds...)
	}
	return out
}
