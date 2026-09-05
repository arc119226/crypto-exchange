package matching_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// orderModel is what the property tests remember about every accepted order,
// independently of the book, to check conservation and price bounds.
type orderModel struct {
	o           matching.NewOrder
	tradedQty   money.Amount
	tradedQuote money.Amount
	terminal    bool
}

type propModel struct {
	cfg    matching.MarketConfig
	orders map[matching.OrderID]*orderModel
	live   []matching.OrderID // ids that may still be resting (candidates for cancel)
}

func (m *propModel) genCommand(t *rapid.T, seq uint64) matching.Command {
	accounts := []string{"A", "B", "C"}
	acct := rapid.SampledFrom(accounts).Draw(t, "account")
	id := matching.OrderID(fmt.Sprintf("o%d", seq))
	price := func() string {
		return fmt.Sprintf("%d.%02d", 1990+rapid.IntRange(0, 20).Draw(t, "p"), rapid.IntRange(0, 99).Draw(t, "c"))
	}
	qty := func() string { return fmt.Sprintf("0.%04d", rapid.IntRange(1, 9999).Draw(t, "q")) }
	kind := rapid.IntRange(0, 99).Draw(t, "kind")
	switch {
	case kind < 45:
		side := matching.Buy
		if rapid.Bool().Draw(t, "sell") {
			side = matching.Sell
		}
		c := limit(seq, string(id), acct, side, price(), qty())
		if rapid.IntRange(0, 4).Draw(t, "ioc") == 0 {
			c.New.TimeInForce = matching.IOC
		}
		return c
	case kind < 60:
		return marketBuy(seq, string(id), acct, fmt.Sprintf("%d.%02d", rapid.IntRange(5, 3000).Draw(t, "quote"), rapid.IntRange(0, 99).Draw(t, "qc")))
	case kind < 72:
		return marketSell(seq, string(id), acct, qty())
	case kind < 78:
		// an occasionally invalid order: off-tick price or off-step qty
		c := limit(seq, string(id), acct, matching.Buy, price()+"5", qty())
		return c
	default:
		if len(m.live) == 0 {
			return cancel(seq, "missing")
		}
		target := rapid.SampledFrom(m.live).Draw(t, "cancel")
		c := cancel(seq, string(target))
		if rapid.IntRange(0, 3).Draw(t, "owner") == 0 {
			c.Cancel.AccountID = matching.AccountID(acct) // sometimes the wrong owner
		}
		return c
	}
}

// checkEvents applies the event-level invariants and updates the model.
func (m *propModel) checkEvents(t require.TestingT, cmd matching.Command, evs []matching.Event) {
	require.NotEmpty(t, evs)
	for i, e := range evs {
		require.Equal(t, cmd.Seq, e.CommandSeq())
		switch ev := e.(type) {
		case matching.Accepted:
			require.Equal(t, 0, i, "Accepted is always first")
			m.orders[ev.OrderID] = &orderModel{o: *cmd.New, tradedQty: money.Zero, tradedQuote: money.Zero}
		case matching.Rejected:
			require.Len(t, evs, 1, "Rejected is the only event")
			require.Equal(t, 0, i)
		case matching.CancelRejected:
			require.Len(t, evs, 1)
		case matching.Trade:
			require.True(t, ev.Qty.IsPositive())
			require.True(t, ev.Qty.IsMultipleOf(m.cfg.QtyStep), "trade qty is a step multiple: %s", ev.Qty)
			require.True(t, ev.Price.Mul(ev.Qty).Equal(ev.QuoteQty), "quote = price × qty")
			require.LessOrEqual(t, ev.QuoteQty.Scale(), m.cfg.QuoteScale, "no rounding needed")
			maker, ok := m.orders[ev.MakerOrderID]
			require.True(t, ok, "maker %s known", ev.MakerOrderID)
			taker, ok := m.orders[ev.TakerOrderID]
			require.True(t, ok, "taker %s known", ev.TakerOrderID)
			require.Equal(t, matching.Limit, maker.o.Type, "only limit orders rest")
			require.True(t, ev.Price.Equal(maker.o.Price), "trade price is the maker's price")
			require.Equal(t, taker.o.Side, ev.TakerSide)
			require.NotEqual(t, taker.o.Side, maker.o.Side)
			if taker.o.Type == matching.Limit {
				if taker.o.Side == matching.Buy {
					require.LessOrEqual(t, ev.Price.Cmp(taker.o.Price), 0, "buy taker never pays above its limit")
				} else {
					require.GreaterOrEqual(t, ev.Price.Cmp(taker.o.Price), 0, "sell taker never sells below its limit")
				}
			}
			if m.cfg.SelfTradePolicy == matching.STPCancelNewest {
				require.NotEqual(t, ev.MakerAccountID, ev.TakerAccountID, "cancel_newest never self-trades")
			}
			maker.tradedQty = maker.tradedQty.Add(ev.Qty)
			maker.tradedQuote = maker.tradedQuote.Add(ev.QuoteQty)
			taker.tradedQty = taker.tradedQty.Add(ev.Qty)
			taker.tradedQuote = taker.tradedQuote.Add(ev.QuoteQty)
			require.True(t, ev.MakerRemaining.Equal(maker.o.Qty.Sub(maker.tradedQty)), "maker remaining is consistent")
		case matching.Updated:
			om := m.orders[ev.OrderID]
			require.NotNil(t, om)
			require.True(t, ev.FilledQty.Equal(om.tradedQty), "filled_qty == Σ trades (%s)", ev.OrderID)
			require.True(t, ev.FilledQuote.Equal(om.tradedQuote))
			require.True(t, ev.RemainingQty.Add(ev.FilledQty).Equal(om.o.Qty), "remaining + filled == qty")
			require.True(t, ev.RemainingQty.IsPositive())
		case matching.Filled:
			om := m.orders[ev.OrderID]
			require.NotNil(t, om)
			require.True(t, ev.FilledQty.Equal(om.tradedQty))
			require.True(t, ev.FilledQuote.Equal(om.tradedQuote))
			if om.o.Type == matching.Market && om.o.Side == matching.Buy {
				require.True(t, ev.FilledQuote.Equal(om.o.QuoteQty), "market buy filled means the whole budget was spent")
			} else {
				require.True(t, ev.FilledQty.Equal(om.o.Qty), "filled means qty fully traded")
			}
			require.False(t, om.terminal, "no event after a terminal one")
			om.terminal = true
		case matching.Cancelled:
			om := m.orders[ev.OrderID]
			require.NotNil(t, om)
			require.True(t, ev.FilledQty.Equal(om.tradedQty))
			require.True(t, ev.FilledQuote.Equal(om.tradedQuote))
			if om.o.Type == matching.Market && om.o.Side == matching.Buy {
				require.True(t, ev.RemainingQty.IsZero())
				require.True(t, ev.RemainingQuote.Add(ev.FilledQuote).Equal(om.o.QuoteQty), "quote budget is conserved")
				require.True(t, ev.RemainingQuote.IsPositive() || ev.Reason == matching.CancelSelfTrade)
			} else {
				require.True(t, ev.RemainingQuote.IsZero())
				require.True(t, ev.RemainingQty.Add(ev.FilledQty).Equal(om.o.Qty), "base qty is conserved")
				require.True(t, ev.RemainingQty.IsPositive(), "cancelled always leaves something to release")
			}
			if ev.Reason == matching.CancelPriceProtection {
				require.NotNil(t, m.cfg.MaxSlippageBps)
			}
			require.False(t, om.terminal)
			om.terminal = true
		default:
			t.Errorf("unknown event %T", e)
		}
	}
}

// checkBook applies the structural invariants of docs/plan-v1.0.md §6.3.
func (m *propModel) checkBook(t require.TestingT, b *matching.Book) {
	snap := b.Snapshot()
	bid, hasBid := b.BestBid()
	ask, hasAsk := b.BestAsk()
	if hasBid && hasAsk {
		require.Less(t, bid.Cmp(ask), 0, "best bid %s must be below best ask %s", bid, ask)
	}
	check := func(side []matching.RestingOrder, s matching.Side) {
		for i, r := range side {
			require.Equal(t, s, r.Side)
			require.True(t, r.Remaining.IsPositive())
			require.True(t, r.Remaining.IsMultipleOf(m.cfg.QtyStep))
			require.True(t, r.Qty.Equal(r.Remaining.Add(r.FilledQty)))
			om := m.orders[r.OrderID]
			require.NotNil(t, om)
			require.True(t, om.tradedQty.Equal(r.FilledQty), "book filled == Σ trades for %s", r.OrderID)
			require.False(t, om.terminal, "a terminal order must not rest")
			if i > 0 {
				prev := side[i-1]
				c := prev.Price.Cmp(r.Price)
				if s == matching.Buy {
					require.GreaterOrEqual(t, c, 0, "bids descend")
				} else {
					require.LessOrEqual(t, c, 0, "asks ascend")
				}
				if c == 0 {
					require.Less(t, prev.Seq, r.Seq, "FIFO within a level")
				}
			}
		}
	}
	check(snap.Bids, matching.Buy)
	check(snap.Asks, matching.Sell)
	require.Equal(t, len(snap.Bids)+len(snap.Asks), b.Len())

	// depth aggregates the snapshot
	d := b.Depth(0)
	agg := func(orders []matching.RestingOrder) []matching.Level {
		var out []matching.Level
		for _, r := range orders {
			if n := len(out); n > 0 && out[n-1].Price.Equal(r.Price) {
				out[n-1].Qty = out[n-1].Qty.Add(r.Remaining)
				out[n-1].Orders++
				continue
			}
			out = append(out, matching.Level{Price: r.Price, Qty: r.Remaining, Orders: 1})
		}
		return out
	}
	for _, pair := range []struct{ got, want []matching.Level }{{d.Bids, agg(snap.Bids)}, {d.Asks, agg(snap.Asks)}} {
		require.Equal(t, len(pair.want), len(pair.got))
		for i := range pair.want {
			require.True(t, pair.want[i].Price.Equal(pair.got[i].Price))
			require.True(t, pair.want[i].Qty.Equal(pair.got[i].Qty), "level qty == Σ remaining")
			require.Equal(t, pair.want[i].Orders, pair.got[i].Orders)
		}
	}
	// refresh the cancel candidates
	m.live = m.live[:0]
	for _, r := range snap.Bids {
		m.live = append(m.live, r.OrderID)
	}
	for _, r := range snap.Asks {
		m.live = append(m.live, r.OrderID)
	}
}

func runProperty(t *testing.T, cfg matching.MarketConfig) {
	rapid.Check(t, func(rt *rapid.T) {
		b, err := matching.New(cfg)
		require.NoError(rt, err)
		m := &propModel{cfg: cfg, orders: map[matching.OrderID]*orderModel{}}
		n := rapid.IntRange(1, 60).Draw(rt, "n")
		cmds := make([]matching.Command, 0, n)
		var encoded []string
		for i := 1; i <= n; i++ {
			cmd := m.genCommand(rt, uint64(i))
			cmds = append(cmds, cmd)
			evs, err := b.Apply(cmd)
			require.NoError(rt, err)
			m.checkEvents(rt, cmd, evs)
			m.checkBook(rt, b)
			encoded = append(encoded, encodeAll(t, evs))
		}
		// determinism: replaying the same commands yields the same events and book
		rb, err := matching.New(cfg)
		require.NoError(rt, err)
		for i, cmd := range cmds {
			evs, err := rb.Apply(cmd)
			require.NoError(rt, err)
			require.Equal(rt, encoded[i], encodeAll(t, evs), "replay diverged at command %d", i+1)
		}
		require.True(rt, rb.Snapshot().Equal(b.Snapshot()), "replayed snapshot differs")
		// recovery: restoring the snapshot rebuilds an equal book
		snap := b.Snapshot()
		restored, err := matching.New(cfg)
		require.NoError(rt, err)
		require.NoError(rt, restored.Restore(snap.LastSeq, append(append([]matching.RestingOrder{}, snap.Asks...), snap.Bids...)))
		require.True(rt, restored.Snapshot().Equal(snap))
	})
}

func TestProperty_CancelNewest(t *testing.T) { runProperty(t, ethUSDC(matching.STPCancelNewest)) }

func TestProperty_AllowSelfTrade(t *testing.T) { runProperty(t, ethUSDC(matching.STPAllow)) }

func TestProperty_WithBandAndMaxQty(t *testing.T) {
	cfg := ethUSDC(matching.STPCancelNewest)
	cfg.MaxSlippageBps = ptrI32(25)
	cfg.MaxQty = ptrAmt("0.5")
	runProperty(t, cfg)
}

func TestProperty_CoarseSteps(t *testing.T) {
	// a step that is not a power of ten exercises floorToStep
	cfg := matching.MarketConfig{
		Symbol: "X-Y", PriceTick: amt("0.5"), QtyStep: amt("0.0005"), MinNotional: amt("1"),
		BaseScale: 8, QuoteScale: 6, SelfTradePolicy: matching.STPCancelNewest,
	}
	runProperty(t, cfg)
}
