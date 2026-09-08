package matching_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// docs/plan-v1.0.md §6.1.4 (b)+(c), recomputed in docs/domain.md §1.3.
func TestPlanExample_PartialFillWithPriceImprovement(t *testing.T) {
	b := newBook(t, ethUSDC(matching.STPCancelNewest))

	evs := apply(t, b, limit(1, "S1", "S", matching.Sell, "1990", "0.4"))
	assert.Equal(t, []matching.EventKind{matching.KindAccepted}, kinds(evs))

	evs = apply(t, b, limit(2, "B1", "B", matching.Buy, "2000", "1.0"))
	require.Equal(t, []matching.EventKind{matching.KindAccepted, matching.KindTrade, matching.KindFilled, matching.KindUpdated}, kinds(evs))
	tr := evs[1].(matching.Trade)
	assert.Equal(t, matching.OrderID("S1"), tr.MakerOrderID)
	assert.Equal(t, matching.OrderID("B1"), tr.TakerOrderID)
	assert.Equal(t, matching.Buy, tr.TakerSide)
	eq(t, "1990", tr.Price, "trade price is the maker's price")
	eq(t, "0.4", tr.Qty)
	eq(t, "796", tr.QuoteQty)
	eq(t, "0", tr.MakerRemaining)
	assert.Equal(t, 0, tr.Index)
	filled := evs[2].(matching.Filled)
	assert.Equal(t, matching.OrderID("S1"), filled.OrderID)
	eq(t, "0.4", filled.FilledQty)
	eq(t, "796", filled.FilledQuote)
	upd := evs[3].(matching.Updated)
	assert.Equal(t, matching.OrderID("B1"), upd.OrderID)
	eq(t, "0.4", upd.FilledQty)
	eq(t, "796", upd.FilledQuote)
	eq(t, "0.6", upd.RemainingQty)

	snap := b.Snapshot()
	require.Len(t, snap.Bids, 1)
	assert.Empty(t, snap.Asks)
	eq(t, "2000", snap.Bids[0].Price)
	eq(t, "0.6", snap.Bids[0].Remaining, "hold for the rest is 0.6 × 2000 = 1200 (domain.md 1.3 b)")
	eq(t, "1.0", snap.Bids[0].Qty)
	eq(t, "796", snap.Bids[0].FilledQuote)
	assert.Equal(t, uint64(2), snap.LastSeq)
	best, ok := b.BestBid()
	require.True(t, ok)
	eq(t, "2000", best)
	_, ok = b.BestAsk()
	assert.False(t, ok)

	evs = apply(t, b, cancel(3, "B1"))
	require.Equal(t, []matching.EventKind{matching.KindCancelled}, kinds(evs))
	c := evs[0].(matching.Cancelled)
	assert.Equal(t, matching.CancelByUser, c.Reason)
	eq(t, "0.6", c.RemainingQty, "ledger releases 0.6 × 2000 = 1200 USDC")
	eq(t, "0.4", c.FilledQty)
	eq(t, "0", c.RemainingQuote)
	assert.Equal(t, 0, b.Len())
}

// docs/domain.md §1.4: market buy for 1000 USDC across two levels.
func TestDomainExample_MarketBuyAcrossTwoLevels(t *testing.T) {
	b := newBook(t, ethUSDC(matching.STPCancelNewest))
	apply(t, b, limit(1, "S1", "S1", matching.Sell, "1990", "0.3"))
	apply(t, b, limit(2, "S2", "S2", matching.Sell, "1995", "0.5"))

	evs := apply(t, b, marketBuy(3, "B", "B", "1000"))
	require.Equal(t, []matching.EventKind{
		matching.KindAccepted, matching.KindTrade, matching.KindFilled, matching.KindTrade, matching.KindUpdated, matching.KindCancelled,
	}, kinds(evs))
	acc := evs[0].(matching.Accepted)
	assert.Equal(t, matching.Market, acc.Type)
	assert.Equal(t, matching.IOC, acc.TimeInForce, "market orders are always IOC")
	eq(t, "1000", acc.QuoteQty)

	t1 := evs[1].(matching.Trade)
	eq(t, "1990", t1.Price)
	eq(t, "0.3", t1.Qty)
	eq(t, "597", t1.QuoteQty)
	t2 := evs[3].(matching.Trade)
	assert.Equal(t, 1, t2.Index)
	eq(t, "1995", t2.Price)
	eq(t, "0.2020", t2.Qty, "floor(403/1995, 0.0001)")
	eq(t, "402.99", t2.QuoteQty)
	eq(t, "0.2980", t2.MakerRemaining)
	upd := evs[4].(matching.Updated)
	assert.Equal(t, matching.OrderID("S2"), upd.OrderID)
	eq(t, "0.2980", upd.RemainingQty)

	c := evs[5].(matching.Cancelled)
	assert.Equal(t, matching.CancelIOC, c.Reason)
	eq(t, "0.5020", c.FilledQty)
	eq(t, "999.99", c.FilledQuote)
	eq(t, "0", c.RemainingQty)
	eq(t, "0.01", c.RemainingQuote, "0.01 < 1995 × 0.0001: dust is released")

	d := b.Depth(10)
	require.Len(t, d.Asks, 1)
	eq(t, "1995", d.Asks[0].Price)
	eq(t, "0.2980", d.Asks[0].Qty)
	assert.Equal(t, 1, d.Asks[0].Orders)
	assert.Empty(t, d.Bids)
}

// docs/domain.md §3: STP cancel_newest after a real fill against another account.
func TestSelfTradePrevention(t *testing.T) {
	t.Run("cancel_newest cancels the taker remainder and leaves the resting order", func(t *testing.T) {
		b := newBook(t, ethUSDC(matching.STPCancelNewest))
		apply(t, b, limit(1, "A1", "A", matching.Sell, "2000", "1"))
		apply(t, b, limit(2, "B1", "B", matching.Sell, "1999", "0.5"))
		evs := apply(t, b, limit(3, "A2", "A", matching.Buy, "2000", "2"))
		require.Equal(t, []matching.EventKind{matching.KindAccepted, matching.KindTrade, matching.KindFilled, matching.KindCancelled}, kinds(evs))
		tr := evs[1].(matching.Trade)
		assert.Equal(t, matching.OrderID("B1"), tr.MakerOrderID)
		eq(t, "1999", tr.Price)
		eq(t, "0.5", tr.Qty)
		c := evs[3].(matching.Cancelled)
		assert.Equal(t, matching.CancelSelfTrade, c.Reason)
		eq(t, "1.5", c.RemainingQty)
		eq(t, "0.5", c.FilledQty)
		eq(t, "999.5", c.FilledQuote)
		snap := b.Snapshot()
		require.Len(t, snap.Asks, 1)
		assert.Equal(t, matching.OrderID("A1"), snap.Asks[0].OrderID, "resting order untouched")
		eq(t, "1", snap.Asks[0].Remaining)
		assert.Empty(t, snap.Bids)
	})
	t.Run("allow trades against the same account", func(t *testing.T) {
		b := newBook(t, ethUSDC(matching.STPAllow))
		apply(t, b, limit(1, "A1", "A", matching.Sell, "2000", "1"))
		apply(t, b, limit(2, "B1", "B", matching.Sell, "1999", "0.5"))
		evs := apply(t, b, limit(3, "A2", "A", matching.Buy, "2000", "2"))
		require.Equal(t, []matching.EventKind{
			matching.KindAccepted, matching.KindTrade, matching.KindFilled, matching.KindTrade, matching.KindFilled, matching.KindUpdated,
		}, kinds(evs))
		self := evs[3].(matching.Trade)
		assert.Equal(t, self.MakerAccountID, self.TakerAccountID)
		eq(t, "1", self.Qty)
		upd := evs[5].(matching.Updated)
		eq(t, "1.5", upd.FilledQty)
		eq(t, "2999.5", upd.FilledQuote)
		eq(t, "0.5", upd.RemainingQty)
		snap := b.Snapshot()
		require.Len(t, snap.Bids, 1)
		assert.Empty(t, snap.Asks)
	})
}

func TestPriceTimePriority(t *testing.T) {
	b := newBook(t, ethUSDC(matching.STPCancelNewest))
	apply(t, b, limit(1, "X", "X", matching.Buy, "2000", "0.3"))
	apply(t, b, limit(2, "Y", "Y", matching.Buy, "1999", "0.3"))
	apply(t, b, limit(3, "Z", "Z", matching.Buy, "2000", "0.3"))

	d := b.Depth(0)
	require.Len(t, d.Bids, 2)
	eq(t, "2000", d.Bids[0].Price)
	eq(t, "0.6", d.Bids[0].Qty)
	assert.Equal(t, 2, d.Bids[0].Orders)
	eq(t, "1999", d.Bids[1].Price)

	evs := apply(t, b, marketSell(4, "M1", "M", "0.5"))
	require.Equal(t, []matching.EventKind{matching.KindAccepted, matching.KindTrade, matching.KindFilled, matching.KindTrade, matching.KindUpdated, matching.KindFilled}, kinds(evs))
	assert.Equal(t, matching.OrderID("X"), evs[1].(matching.Trade).MakerOrderID, "same price: earliest seq first")
	assert.Equal(t, matching.OrderID("Z"), evs[3].(matching.Trade).MakerOrderID, "then the later order at the same price, before the worse price")
	eq(t, "0.2", evs[3].(matching.Trade).Qty)
	eq(t, "0.1", evs[3].(matching.Trade).MakerRemaining)
	f := evs[5].(matching.Filled)
	assert.Equal(t, matching.OrderID("M1"), f.OrderID)
	eq(t, "0.5", f.FilledQty)
	eq(t, "1000", f.FilledQuote)

	evs = apply(t, b, marketSell(5, "M2", "M", "0.5"))
	require.Equal(t, []matching.EventKind{matching.KindAccepted, matching.KindTrade, matching.KindFilled, matching.KindTrade, matching.KindFilled, matching.KindCancelled}, kinds(evs))
	assert.Equal(t, matching.OrderID("Z"), evs[1].(matching.Trade).MakerOrderID)
	assert.Equal(t, matching.OrderID("Y"), evs[3].(matching.Trade).MakerOrderID, "worse price only after 2000 is exhausted")
	eq(t, "1999", evs[3].(matching.Trade).Price)
	c := evs[5].(matching.Cancelled)
	assert.Equal(t, matching.CancelIOC, c.Reason)
	eq(t, "0.4", c.FilledQty)
	eq(t, "799.7", c.FilledQuote)
	eq(t, "0.1", c.RemainingQty, "no liquidity left: market remainder is cancelled")
	assert.Equal(t, 0, b.Len())

	evs = apply(t, b, marketSell(6, "M3", "M", "0.1"))
	require.Equal(t, []matching.EventKind{matching.KindRejected}, kinds(evs))
	assert.Equal(t, matching.RejectEmptyBook, evs[0].(matching.Rejected).Reason)
}

func TestLimitOrderSweepsLevelsThenRests(t *testing.T) {
	b := newBook(t, ethUSDC(matching.STPCancelNewest))
	apply(t, b, limit(1, "S1", "S", matching.Sell, "1990", "0.1"))
	apply(t, b, limit(2, "S2", "S", matching.Sell, "1995", "0.1"))
	apply(t, b, limit(3, "S3", "S", matching.Sell, "2005", "0.1"))
	apply(t, b, limit(4, "S4", "S", matching.Sell, "2020", "0.1")) // beyond the limit, must stay

	evs := apply(t, b, limit(5, "B", "B", matching.Buy, "2010", "0.5"))
	require.Equal(t, []matching.EventKind{
		matching.KindAccepted,
		matching.KindTrade, matching.KindFilled,
		matching.KindTrade, matching.KindFilled,
		matching.KindTrade, matching.KindFilled,
		matching.KindUpdated,
	}, kinds(evs))
	prices := []string{"1990", "1995", "2005"}
	for i, want := range prices {
		tr := evs[1+2*i].(matching.Trade)
		eq(t, want, tr.Price, "trade %d at the maker's price, below the 2010 limit", i)
		assert.Equal(t, i, tr.Index)
	}
	upd := evs[7].(matching.Updated)
	eq(t, "0.3", upd.FilledQty)
	eq(t, "599", upd.FilledQuote, "199 + 199.5 + 200.5")
	eq(t, "0.2", upd.RemainingQty)

	snap := b.Snapshot()
	require.Len(t, snap.Bids, 1)
	eq(t, "2010", snap.Bids[0].Price)
	eq(t, "0.2", snap.Bids[0].Remaining)
	require.Len(t, snap.Asks, 1)
	assert.Equal(t, matching.OrderID("S4"), snap.Asks[0].OrderID)
	bid, _ := b.BestBid()
	ask, _ := b.BestAsk()
	assert.Less(t, bid.Cmp(ask), 0, "book never crosses")
}

func TestLimitIOCCancelsRemainder(t *testing.T) {
	b := newBook(t, ethUSDC(matching.STPCancelNewest))
	apply(t, b, limit(1, "S1", "S", matching.Sell, "2000", "0.1"))
	evs := apply(t, b, limitIOC(2, "B", "B", matching.Buy, "2000", "0.3"))
	require.Equal(t, []matching.EventKind{matching.KindAccepted, matching.KindTrade, matching.KindFilled, matching.KindCancelled}, kinds(evs))
	assert.Equal(t, matching.IOC, evs[0].(matching.Accepted).TimeInForce)
	c := evs[3].(matching.Cancelled)
	assert.Equal(t, matching.CancelIOC, c.Reason)
	eq(t, "0.1", c.FilledQty)
	eq(t, "0.2", c.RemainingQty)
	assert.Equal(t, 0, b.Len(), "IOC never rests")
}

func TestEqualPricesTradeAndNonCrossingOrdersRest(t *testing.T) {
	b := newBook(t, ethUSDC(matching.STPCancelNewest))
	apply(t, b, limit(1, "B1", "B", matching.Buy, "1999", "0.1"))
	evs := apply(t, b, limit(2, "S1", "S", matching.Sell, "2001", "0.1"))
	assert.Equal(t, []matching.EventKind{matching.KindAccepted}, kinds(evs), "1999 bid vs 2001 ask: no trade")
	assert.Equal(t, 2, b.Len())

	// docs/domain.md §9 Q1: a limit that exactly meets the resting price trades.
	evs = apply(t, b, limit(3, "S2", "S", matching.Sell, "1999", "0.1"))
	require.Equal(t, []matching.EventKind{matching.KindAccepted, matching.KindTrade, matching.KindFilled, matching.KindFilled}, kinds(evs))
	eq(t, "1999", evs[1].(matching.Trade).Price)
	assert.Equal(t, matching.OrderID("B1"), evs[2].(matching.Filled).OrderID, "maker filled first")
	assert.Equal(t, matching.OrderID("S2"), evs[3].(matching.Filled).OrderID, "then the taker")
	assert.Equal(t, 1, b.Len())
}

func TestPriceProtectionBand(t *testing.T) {
	cfg := ethUSDC(matching.STPCancelNewest)
	cfg.MaxSlippageBps = ptrI32(100) // 1% around the best price at arrival

	t.Run("market buy stops above best × 1.01", func(t *testing.T) {
		b := newBook(t, cfg)
		apply(t, b, limit(1, "S1", "S", matching.Sell, "2000", "0.1"))
		apply(t, b, limit(2, "S2", "S", matching.Sell, "2010", "0.1"))
		apply(t, b, limit(3, "S3", "S", matching.Sell, "2030", "0.1"))
		evs := apply(t, b, marketBuy(4, "B", "B", "10000"))
		require.Equal(t, []matching.EventKind{matching.KindAccepted, matching.KindTrade, matching.KindFilled, matching.KindTrade, matching.KindFilled, matching.KindCancelled}, kinds(evs))
		c := evs[5].(matching.Cancelled)
		assert.Equal(t, matching.CancelPriceProtection, c.Reason)
		eq(t, "0.2", c.FilledQty)
		eq(t, "401", c.FilledQuote)
		eq(t, "9599", c.RemainingQuote, "2030 > 2020 bound: the rest of the budget is released")
		assert.Equal(t, 1, b.Len(), "S3 stays")
	})
	t.Run("market sell stops below best × 0.99", func(t *testing.T) {
		b := newBook(t, cfg)
		apply(t, b, limit(1, "B1", "B", matching.Buy, "2000", "0.1"))
		apply(t, b, limit(2, "B2", "B", matching.Buy, "1985", "0.1"))
		apply(t, b, limit(3, "B3", "B", matching.Buy, "1975", "0.1"))
		evs := apply(t, b, marketSell(4, "S", "S", "0.5"))
		c := evs[len(evs)-1].(matching.Cancelled)
		assert.Equal(t, matching.CancelPriceProtection, c.Reason)
		eq(t, "0.2", c.FilledQty)
		eq(t, "398.5", c.FilledQuote)
		eq(t, "0.3", c.RemainingQty, "1975 < 1980 bound")
	})
	t.Run("limit orders are not subject to the band", func(t *testing.T) {
		b := newBook(t, cfg)
		apply(t, b, limit(1, "S1", "S", matching.Sell, "2000", "0.1"))
		apply(t, b, limit(2, "S2", "S", matching.Sell, "2100", "0.1"))
		evs := apply(t, b, limit(3, "B", "B", matching.Buy, "2100", "0.2"))
		require.Equal(t, matching.KindFilled, evs[len(evs)-1].Kind())
	})
}

func TestMarketOrderRejections(t *testing.T) {
	b := newBook(t, ethUSDC(matching.STPCancelNewest))
	evs := apply(t, b, marketBuy(1, "B", "B", "100"))
	require.Equal(t, []matching.EventKind{matching.KindRejected}, kinds(evs))
	assert.Equal(t, matching.RejectEmptyBook, evs[0].(matching.Rejected).Reason)

	apply(t, b, limit(2, "S1", "S", matching.Sell, "100000", "0.1"))
	evs = apply(t, b, marketBuy(3, "B2", "B", "5"))
	require.Equal(t, []matching.EventKind{matching.KindRejected}, kinds(evs))
	assert.Equal(t, matching.RejectQuoteQtyTooSmall, evs[0].(matching.Rejected).Reason,
		"5 USDC buys floor(0.00005, 0.0001) = 0 ETH at 100000: nothing to hold, so reject instead of cancel")
	assert.Equal(t, uint64(3), b.LastSeq(), "rejected commands still consume their seq")
}

func TestValidateNewOrder(t *testing.T) {
	cfg := ethUSDC(matching.STPCancelNewest)
	cfg.MaxQty = ptrAmt("100")
	good := matching.NewOrder{OrderID: "o", AccountID: "a", Side: matching.Buy, Type: matching.Limit, TimeInForce: matching.GTC, Price: amt("2000"), Qty: amt("0.5")}
	assert.Equal(t, matching.RejectReason(""), matching.ValidateNewOrder(cfg, good))

	mut := func(f func(o *matching.NewOrder)) matching.NewOrder { o := good; f(&o); return o }
	cases := map[string]struct {
		o    matching.NewOrder
		want matching.RejectReason
	}{
		"missing order id":        {mut(func(o *matching.NewOrder) { o.OrderID = "" }), matching.RejectInvalidOrder},
		"missing account":         {mut(func(o *matching.NewOrder) { o.AccountID = "" }), matching.RejectInvalidOrder},
		"unknown side":            {mut(func(o *matching.NewOrder) { o.Side = 0 }), matching.RejectInvalidOrder},
		"unknown type":            {mut(func(o *matching.NewOrder) { o.Type = 0 }), matching.RejectInvalidOrder},
		"limit without price":     {mut(func(o *matching.NewOrder) { o.Price = money.Zero }), matching.RejectInvalidOrder},
		"limit with quote qty":    {mut(func(o *matching.NewOrder) { o.QuoteQty = amt("1") }), matching.RejectInvalidOrder},
		"negative qty":            {mut(func(o *matching.NewOrder) { o.Qty = amt("-1") }), matching.RejectInvalidOrder},
		"price off tick":          {mut(func(o *matching.NewOrder) { o.Price = amt("2000.005") }), matching.RejectInvalidPriceTick},
		"price too many decimals": {mut(func(o *matching.NewOrder) { o.Price = amt("2000.0000001") }), matching.RejectInvalidPriceTick},
		"qty off step":            {mut(func(o *matching.NewOrder) { o.Qty = amt("0.00005") }), matching.RejectInvalidQtyStep},
		"below min notional":      {mut(func(o *matching.NewOrder) { o.Price = amt("1"); o.Qty = amt("4.9999") }), matching.RejectBelowMinNotional},
		"above max qty":           {mut(func(o *matching.NewOrder) { o.Qty = amt("100.0001") }), matching.RejectAboveMaxQty},
		"market with price": {mut(func(o *matching.NewOrder) {
			o.Type = matching.Market
			o.TimeInForce = 0
		}), matching.RejectInvalidOrder},
		"market gtc": {mut(func(o *matching.NewOrder) {
			o.Type = matching.Market
			o.Price = money.Zero
			o.Qty = money.Zero
			o.QuoteQty = amt("10")
		}), matching.RejectInvalidOrder},
		"market buy with qty": {mut(func(o *matching.NewOrder) {
			o.Type = matching.Market
			o.TimeInForce = 0
			o.Price = money.Zero
			o.QuoteQty = amt("10")
		}), matching.RejectInvalidOrder},
		"market buy below min notional": {mut(func(o *matching.NewOrder) {
			o.Type = matching.Market
			o.TimeInForce = 0
			o.Price = money.Zero
			o.Qty = money.Zero
			o.QuoteQty = amt("4.99")
		}), matching.RejectBelowMinNotional},
		"market buy quote too precise": {mut(func(o *matching.NewOrder) {
			o.Type = matching.Market
			o.TimeInForce = 0
			o.Price = money.Zero
			o.Qty = money.Zero
			o.QuoteQty = amt("10.0000001")
		}), matching.RejectInvalidOrder},
		"market sell with quote": {mut(func(o *matching.NewOrder) {
			o.Type = matching.Market
			o.Side = matching.Sell
			o.TimeInForce = 0
			o.Price = money.Zero
			o.QuoteQty = amt("1")
		}), matching.RejectInvalidOrder},
		"market sell off step": {mut(func(o *matching.NewOrder) {
			o.Type = matching.Market
			o.Side = matching.Sell
			o.TimeInForce = 0
			o.Price = money.Zero
			o.Qty = amt("0.00001")
		}), matching.RejectInvalidQtyStep},
		"market sell above max": {mut(func(o *matching.NewOrder) {
			o.Type = matching.Market
			o.Side = matching.Sell
			o.TimeInForce = 0
			o.Price = money.Zero
			o.Qty = amt("101")
		}), matching.RejectAboveMaxQty},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, c.want, matching.ValidateNewOrder(cfg, c.o))
		})
	}

	t.Run("a rejected order emits only Rejected and holds nothing", func(t *testing.T) {
		b := newBook(t, cfg)
		bad := limit(1, "o", "a", matching.Buy, "2000.005", "0.5")
		evs := apply(t, b, bad)
		require.Equal(t, []matching.EventKind{matching.KindRejected}, kinds(evs))
		assert.Equal(t, matching.RejectInvalidPriceTick, evs[0].(matching.Rejected).Reason)
		assert.Equal(t, 0, b.Len())
	})
	t.Run("duplicate order id is rejected", func(t *testing.T) {
		b := newBook(t, cfg)
		apply(t, b, limit(1, "dup", "a", matching.Buy, "1990", "0.5"))
		evs := apply(t, b, limit(2, "dup", "a", matching.Buy, "1991", "0.5"))
		require.Equal(t, []matching.EventKind{matching.KindRejected}, kinds(evs))
		assert.Equal(t, matching.RejectDuplicateOrderID, evs[0].(matching.Rejected).Reason)
		assert.Equal(t, 1, b.Len())
	})
}

func TestCancel(t *testing.T) {
	b := newBook(t, ethUSDC(matching.STPCancelNewest))
	evs := apply(t, b, cancel(1, "nope"))
	require.Equal(t, []matching.EventKind{matching.KindCancelRejected}, kinds(evs))
	assert.Equal(t, matching.CancelRejectUnknownOrder, evs[0].(matching.CancelRejected).Reason)

	apply(t, b, limit(2, "B1", "B", matching.Buy, "1990", "0.5"))
	wrongOwner := cancel(3, "B1")
	wrongOwner.Cancel.AccountID = "X"
	evs = apply(t, b, wrongOwner)
	assert.Equal(t, matching.CancelRejectNotOwner, evs[0].(matching.CancelRejected).Reason)
	assert.Equal(t, 1, b.Len())

	owner := cancel(4, "B1")
	owner.Cancel.AccountID = "B"
	evs = apply(t, b, owner)
	require.Equal(t, []matching.EventKind{matching.KindCancelled}, kinds(evs))
	eq(t, "0.5", evs[0].(matching.Cancelled).RemainingQty)
	assert.Equal(t, 0, b.Len())
	_, ok := b.Order("B1")
	assert.False(t, ok)

	evs = apply(t, b, cancel(5, "B1"))
	assert.Equal(t, matching.KindCancelRejected, evs[0].Kind(), "cancelling twice is not an error")
}

func TestApplyAndConfigErrors(t *testing.T) {
	b := newBook(t, ethUSDC(matching.STPCancelNewest))
	apply(t, b, limit(5, "B1", "B", matching.Buy, "1990", "0.5"))
	_, err := b.Apply(limit(5, "B2", "B", matching.Buy, "1990", "0.5"))
	assert.ErrorIs(t, err, matching.ErrSeqNotIncreasing)
	_, err = b.Apply(limit(4, "B2", "B", matching.Buy, "1990", "0.5"))
	assert.ErrorIs(t, err, matching.ErrSeqNotIncreasing)
	_, err = b.Apply(matching.Command{Seq: 6})
	assert.ErrorIs(t, err, matching.ErrInvalidCommand)
	both := limit(6, "B3", "B", matching.Buy, "1990", "0.5")
	both.Cancel = &matching.Cancel{OrderID: "B1"}
	_, err = b.Apply(both)
	assert.ErrorIs(t, err, matching.ErrInvalidCommand)
	assert.Equal(t, uint64(5), b.LastSeq(), "failed commands do not advance seq")

	bad := ethUSDC(matching.STPCancelNewest)
	bad.QuoteScale = 5 // 2 + 4 > 5 violates the precision rule
	_, err = matching.New(bad)
	assert.ErrorIs(t, err, matching.ErrInvalidConfig)
	_, err = matching.New(ethUSDC(matching.STPCancelOldest))
	assert.ErrorIs(t, err, matching.ErrUnsupportedSTP)
	for _, f := range []func(*matching.MarketConfig){
		func(c *matching.MarketConfig) { c.Symbol = "" },
		func(c *matching.MarketConfig) { c.PriceTick = money.Zero },
		func(c *matching.MarketConfig) { c.QtyStep = amt("-1") },
		func(c *matching.MarketConfig) { c.MinNotional = amt("-1") },
		func(c *matching.MarketConfig) { c.BaseScale = 19 },
		func(c *matching.MarketConfig) { c.BaseScale = 2 }, // step 0.0001 needs 4 decimals
		func(c *matching.MarketConfig) { c.MaxQty = ptrAmt("0") },
		func(c *matching.MarketConfig) { c.MaxSlippageBps = ptrI32(0) },
		func(c *matching.MarketConfig) { c.SelfTradePolicy = 0 },
	} {
		c := ethUSDC(matching.STPCancelNewest)
		f(&c)
		_, err := matching.New(c)
		assert.Error(t, err)
	}
}

func TestRestoreRebuildsTheSameBook(t *testing.T) {
	cfg := ethUSDC(matching.STPCancelNewest)
	b := newBook(t, cfg)
	apply(t, b, limit(1, "B1", "B", matching.Buy, "1990", "0.5"))
	apply(t, b, limit(2, "S1", "S", matching.Sell, "2010", "0.4"))
	apply(t, b, limit(3, "B2", "B", matching.Buy, "1990", "0.25"))
	apply(t, b, limit(4, "S2", "S", matching.Sell, "2005", "0.1"))
	apply(t, b, limit(5, "B3", "B", matching.Buy, "1995", "0.2"))
	apply(t, b, marketSell(6, "M", "M", "0.3")) // partially fills B3 (0.2) and B1 (0.1)
	snap := b.Snapshot()
	require.Len(t, snap.Bids, 2)
	eq(t, "0.4", snap.Bids[0].Remaining)
	eq(t, "0.1", snap.Bids[0].FilledQty)

	// shuffle the input order: Restore must sort by seq itself
	orders := append([]matching.RestingOrder{}, snap.Asks...)
	orders = append(orders, snap.Bids[1], snap.Bids[0])
	rb := newBook(t, cfg)
	require.NoError(t, rb.Restore(6, orders))
	assert.True(t, rb.Snapshot().Equal(snap), "restored snapshot differs:\n%+v\n%+v", rb.Snapshot(), snap)
	assert.Equal(t, b.Depth(0), rb.Depth(0))
	assert.Equal(t, uint64(6), rb.LastSeq())

	// both books evolve identically afterwards
	e1 := apply(t, b, marketBuy(7, "MB", "M", "1000"))
	e2 := apply(t, rb, marketBuy(7, "MB", "M", "1000"))
	assert.Equal(t, encodeAll(t, e1), encodeAll(t, e2))
	assert.True(t, rb.Snapshot().Equal(b.Snapshot()))

	t.Run("lastSeq is at least the highest restored seq", func(t *testing.T) {
		rb := newBook(t, cfg)
		require.NoError(t, rb.Restore(1, orders))
		assert.Equal(t, uint64(4), rb.LastSeq(), "B3 (seq 5) was filled; the highest resting seq is S2 (4)")
	})
	t.Run("restore errors", func(t *testing.T) {
		rb := newBook(t, cfg)
		require.ErrorIs(t, rb.Restore(1, []matching.RestingOrder{orders[0], orders[0]}), matching.ErrDuplicateOrderID)
		rb = newBook(t, cfg)
		badQty := orders[0]
		badQty.Qty = amt("99")
		require.ErrorIs(t, rb.Restore(1, []matching.RestingOrder{badQty}), matching.ErrInvalidResting)
		badPrice := orders[0]
		badPrice.Price = amt("2000.001")
		require.ErrorIs(t, rb.Restore(1, []matching.RestingOrder{badPrice}), matching.ErrInvalidResting)
		zero := orders[0]
		zero.Remaining = money.Zero
		zero.FilledQty = zero.Qty
		require.ErrorIs(t, rb.Restore(1, []matching.RestingOrder{zero}), matching.ErrInvalidResting)
		require.ErrorIs(t, b.Restore(1, nil), matching.ErrInvalidResting, "non-empty book")
	})
}

func encodeAll(t *testing.T, evs []matching.Event) string {
	t.Helper()
	var buf bytes.Buffer
	for _, e := range evs {
		b, err := matching.EncodeEvent(e)
		require.NoError(t, err)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.String()
}

func TestEventJSONRoundTrip(t *testing.T) {
	events := []matching.Event{
		matching.Accepted{Seq: 1, OrderID: "o", AccountID: "a", Side: matching.Buy, Type: matching.Limit, TimeInForce: matching.GTC, Price: amt("1990.50"), Qty: amt("0.1"), QuoteQty: money.Zero},
		matching.Trade{Seq: 2, Index: 1, MakerOrderID: "m", TakerOrderID: "t", MakerAccountID: "ma", TakerAccountID: "ta", TakerSide: matching.Sell, Price: amt("1990"), Qty: amt("0.2"), QuoteQty: amt("398"), MakerRemaining: amt("0.3")},
		matching.Updated{Seq: 3, OrderID: "o", AccountID: "a", FilledQty: amt("0.1"), FilledQuote: amt("199"), RemainingQty: amt("0.4")},
		matching.Filled{Seq: 4, OrderID: "o", AccountID: "a", FilledQty: amt("0.5"), FilledQuote: amt("995")},
		matching.Cancelled{Seq: 5, OrderID: "o", AccountID: "a", Reason: matching.CancelIOC, FilledQty: amt("0.1"), FilledQuote: amt("199"), RemainingQty: money.Zero, RemainingQuote: amt("0.01")},
		matching.Rejected{Seq: 6, OrderID: "o", AccountID: "a", Reason: matching.RejectEmptyBook},
		matching.CancelRejected{Seq: 7, OrderID: "o", Reason: matching.CancelRejectUnknownOrder},
	}
	for _, e := range events {
		raw, err := matching.EncodeEvent(e)
		require.NoError(t, err)
		back, err := matching.DecodeEvent(raw)
		require.NoError(t, err, string(raw))
		assert.Equal(t, e.Kind(), back.Kind())
		assert.Equal(t, e.CommandSeq(), back.CommandSeq())
		again, err := matching.EncodeEvent(back)
		require.NoError(t, err)
		assert.JSONEq(t, string(raw), string(again))
	}
	assert.Contains(t, string(mustEncode(t, events[1])), `"price":"1990"`, "amounts are strings")
	_, err := matching.DecodeEvent([]byte(`{"kind":"bogus","data":{}}`))
	assert.Error(t, err)
	_, err = matching.DecodeEvent([]byte(`not json`))
	assert.Error(t, err)
}

func mustEncode(t *testing.T, e matching.Event) []byte {
	t.Helper()
	b, err := matching.EncodeEvent(e)
	require.NoError(t, err)
	return b
}

func TestEnumTextRoundTrip(t *testing.T) {
	for _, s := range []matching.Side{matching.Buy, matching.Sell} {
		b, err := s.MarshalText()
		require.NoError(t, err)
		var back matching.Side
		require.NoError(t, back.UnmarshalText(b))
		assert.Equal(t, s, back)
	}
	var side matching.Side
	assert.Error(t, side.UnmarshalText([]byte("short")))
	_, err := matching.Side(9).MarshalText()
	assert.Error(t, err)
	var typ matching.OrderType
	assert.Error(t, typ.UnmarshalText([]byte("stop")))
	var tif matching.TimeInForce
	require.NoError(t, tif.UnmarshalText([]byte("")))
	assert.Equal(t, matching.GTC, tif, "empty time_in_force defaults to GTC")
	assert.Error(t, tif.UnmarshalText([]byte("fok")))
	var stp matching.SelfTradePolicy
	require.NoError(t, stp.UnmarshalText([]byte("cancel_oldest")))
	assert.Equal(t, matching.STPCancelOldest, stp)
	assert.Error(t, stp.UnmarshalText([]byte("x")))
	assert.Equal(t, "buy", matching.Buy.String())
	assert.Equal(t, "market", matching.Market.String())
	assert.Equal(t, "ioc", matching.IOC.String())
	assert.Equal(t, "allow", matching.STPAllow.String())
	assert.Equal(t, matching.Sell, matching.Buy.Opposite())
	assert.Equal(t, matching.Buy, matching.Sell.Opposite())
}

func TestScriptRoundTripAndRun(t *testing.T) {
	script := matching.Script{Market: ethUSDC(matching.STPCancelNewest), Commands: []matching.Command{
		limit(1, "S1", "S", matching.Sell, "1990", "0.4"),
		limit(2, "B1", "B", matching.Buy, "2000", "1.0"),
		cancel(3, "B1"),
	}}
	var buf bytes.Buffer
	require.NoError(t, matching.WriteScript(&buf, script))
	back, err := matching.ReadScript(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, script.Market.Symbol, back.Market.Symbol)
	require.Len(t, back.Commands, 3)
	assert.Equal(t, uint64(2), back.Commands[1].Seq)
	assert.True(t, back.Commands[1].Timestamp.Equal(ts(2)))

	events, book, err := matching.Run(back)
	require.NoError(t, err)
	require.Len(t, events, 3)
	assert.Equal(t, matching.KindCancelled, events[2][0].Kind())
	assert.Equal(t, 0, book.Len())

	_, err = matching.ReadScript(bytes.NewReader([]byte(`{"seq":1,"cancel":{"order_id":"x"}}` + "\n")))
	assert.Error(t, err, "missing market header")
	_, err = matching.ReadScript(bytes.NewReader([]byte("{\"market\":{\"symbol\":\"X\"}}\n{\"seq\":1}\n")))
	assert.Error(t, err, "line with neither new nor cancel")
	_, err = matching.ReadScript(bytes.NewReader([]byte("{\"market\":{\"symbol\":\"X\"}}\nnot json\n")))
	assert.Error(t, err)

	// a script whose config is invalid fails in Run, not ReadScript
	broken := script
	broken.Market.QtyStep = money.Zero
	_, _, err = matching.Run(broken)
	assert.True(t, errors.Is(err, matching.ErrInvalidConfig))
	// a seq regression inside the script surfaces the command index
	regress := script
	regress.Commands = []matching.Command{limit(2, "a", "a", matching.Buy, "1990", "0.1"), limit(1, "b", "a", matching.Buy, "1990", "0.1")}
	_, _, err = matching.Run(regress)
	assert.ErrorIs(t, err, matching.ErrSeqNotIncreasing)
}

func TestAdvanceKeepsLastSeqInStep(t *testing.T) {
	b := newBook(t, ethUSDC(matching.STPCancelNewest))
	_, err := b.Apply(matching.Command{Seq: 1, Timestamp: time.Now(), New: &matching.NewOrder{OrderID: "a", AccountID: "x", Side: matching.Sell, Type: matching.Limit, TimeInForce: matching.GTC, Price: money.MustParse("2000"), Qty: money.MustParse("1")}})
	require.NoError(t, err)
	require.NoError(t, b.Advance(2), "a command the engine rejected before the book still took seq 2")
	assert.EqualValues(t, 2, b.LastSeq())
	assert.ErrorIs(t, b.Advance(4), matching.ErrSeqNotIncreasing, "no gaps")
	assert.ErrorIs(t, b.Advance(2), matching.ErrSeqNotIncreasing, "no repeats")
	_, err = b.Apply(matching.Command{Seq: 3, Timestamp: time.Now(), Cancel: &matching.Cancel{OrderID: "a"}})
	require.NoError(t, err, "the book continues from the advanced seq")
	assert.EqualValues(t, 3, b.LastSeq())
}
