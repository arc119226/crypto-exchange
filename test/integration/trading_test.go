//go:build integration

package integration

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"pgregory.net/rapid"

	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

const market = "ETH-USDC"

// tradingHarness is a migrated, seeded database with a running engine.
type tradingHarness struct {
	ledgerHarness
	store  *registry.Store
	cache  *registry.Cache
	engine *trading.Engine
	svc    *trading.Service
	log    *slog.Logger
}

func setupTrading(t *testing.T) *tradingHarness {
	t.Helper()
	ctx := context.Background()
	lh := setupLedger(t)
	fx, err := registry.LoadFixtures(fixtures)
	require.NoError(t, err)
	_, err = registry.Seed(ctx, lh.all, fx, registry.SeedOptions{TenantID: "default"})
	require.NoError(t, err)
	h := &tradingHarness{
		ledgerHarness: lh, store: registry.NewStore(lh.all), cache: registry.NewCache("default"),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	h.startEngine(t)
	return h
}

// startEngine starts a fresh engine (and service) over the same database.
func (h *tradingHarness) startEngine(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	eng := trading.NewEngine(h.all, h.svc2(), h.cache, h.store, "default", h.log).WithMetrics(trading.NewMetrics(prometheus.NewRegistry()))
	require.NoError(t, eng.Start(ctx))
	t.Cleanup(eng.Stop)
	h.engine = eng
	h.svc = trading.NewService(h.all, eng, h.cache, "default")
}

// crash simulates kill -9: the engine object is dropped without flushing
// anything (every command is already committed or not).
func (h *tradingHarness) crash(t *testing.T) {
	t.Helper()
	h.engine.Stop()
	h.startEngine(t)
}

func (h *tradingHarness) account(t *testing.T, ctx context.Context, funds map[string]string) string {
	t.Helper()
	id := h.newSpot(t, ctx)
	for asset, amount := range funds {
		h.fund(t, ctx, id, asset, amount, fmt.Sprintf("faucet:%s:%s", id, asset))
	}
	return id
}

func limit(account, cid string, side matching.Side, price, qty string) trading.PlaceOrderRequest {
	return trading.PlaceOrderRequest{AccountID: account, MarketSymbol: market, ClientOrderID: cid, Side: side, Type: matching.Limit, Price: amt(price), Qty: amt(qty)}
}

func marketBuy(account, cid, quote string) trading.PlaceOrderRequest {
	return trading.PlaceOrderRequest{AccountID: account, MarketSymbol: market, ClientOrderID: cid, Side: matching.Buy, Type: matching.Market, QuoteQty: amt(quote)}
}

func marketSell(account, cid, qty string) trading.PlaceOrderRequest {
	return trading.PlaceOrderRequest{AccountID: account, MarketSymbol: market, ClientOrderID: cid, Side: matching.Sell, Type: matching.Market, Qty: amt(qty)}
}

func (h *tradingHarness) place(t require.TestingT, ctx context.Context, req trading.PlaceOrderRequest) trading.PlaceOrderResult {
	res, err := h.svc.PlaceOrder(ctx, req)
	require.NoError(t, err, "place %s", req.ClientOrderID)
	return res
}

// assertHoldInvariant checks docs/plan-v1.0.md §6.1.5 invariant 3: for every
// (account, asset), Σ hold_remaining of open orders == balances.hold.
func (h *tradingHarness) assertHoldInvariant(t require.TestingT, ctx context.Context) {
	rows, err := h.all.Query(ctx, `
		WITH holds AS (
		  SELECT account_id, hold_asset AS asset, sum(hold_remaining) AS held
		    FROM trading.orders WHERE status IN ('open', 'partially_filled') GROUP BY 1, 2)
		SELECT b.account_id, b.asset, b.hold::text, COALESCE(h.held, 0)::text
		  FROM ledger.balances b
		  LEFT JOIN holds h ON h.account_id = b.account_id AND h.asset = b.asset
		 WHERE b.hold <> COALESCE(h.held, 0)
		UNION ALL
		SELECT h.account_id, h.asset, '(no balance row)', h.held::text
		  FROM holds h LEFT JOIN ledger.balances b ON b.account_id = h.account_id AND b.asset = h.asset
		 WHERE b.account_id IS NULL`)
	require.NoError(t, err)
	defer rows.Close()
	var violations []string
	for rows.Next() {
		var account, asset, hold, held string
		require.NoError(t, rows.Scan(&account, &asset, &hold, &held))
		violations = append(violations, fmt.Sprintf("%s %s: balances.hold=%s Σhold_remaining=%s", account, asset, hold, held))
	}
	require.Empty(t, violations, "hold invariant")
}

func (h *tradingHarness) assertTrialBalanceZero(t require.TestingT, ctx context.Context) {
	lines, err := h.svc2().TrialBalance(ctx)
	require.NoError(t, err)
	for _, l := range lines {
		require.Truef(t, l.Diff.IsZero(), "trial balance %s: debits %s credits %s", l.Asset, l.Debits, l.Credits)
	}
}

func (h *tradingHarness) svc2() *ledger.Service { return h.ledgerHarness.svc }

// assertSequenceConsistent checks market_sequences against the book and
// that order seqs are unique and never ahead of the market.
func (h *tradingHarness) assertSequenceConsistent(t require.TestingT, ctx context.Context) {
	snap, err := h.engine.Snapshot(ctx, market)
	require.NoError(t, err)
	var lastSeq int64
	require.NoError(t, h.all.QueryRow(ctx, `SELECT last_seq FROM trading.market_sequences s JOIN registry.markets m ON m.id = s.market_id WHERE m.symbol = $1`, market).Scan(&lastSeq))
	require.Equal(t, uint64(lastSeq), snap.LastSeq, "book seq vs market_sequences")
	var dupes, ahead int
	require.NoError(t, h.all.QueryRow(ctx, `SELECT count(*) - count(DISTINCT seq), count(*) FILTER (WHERE seq > $1) FROM trading.orders WHERE seq IS NOT NULL AND market_symbol = $2`, lastSeq, market).Scan(&dupes, &ahead))
	require.Zero(t, dupes, "duplicate order seqs")
	require.Zero(t, ahead, "order seq ahead of market sequence")
}

type outboxRow struct {
	EventType  string
	AccountID  *string
	Seq        *int64
	AccountSeq *int64
	Subject    string
}

func (h *tradingHarness) outbox(t require.TestingT, ctx context.Context) []outboxRow {
	rows, err := h.all.Query(ctx, `SELECT event_type, account_id, seq, account_seq, subject FROM eventbus.outbox ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		require.NoError(t, rows.Scan(&r.EventType, &r.AccountID, &r.Seq, &r.AccountSeq, &r.Subject))
		out = append(out, r)
	}
	return out
}

func eventTypes(rows []outboxRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.EventType)
	}
	return out
}

// TestTradingPlanExampleFlow is docs/plan-v1.0.md §6.1.4 (a)–(c) end to end
// through the engine: hold, settle with fees and price improvement, cancel.
func TestTradingPlanExampleFlow(t *testing.T) {
	h := setupTrading(t)
	ctx := context.Background()
	buyer := h.account(t, ctx, map[string]string{"USDC": "10000"})
	seller := h.account(t, ctx, map[string]string{"ETH": "1"})

	// S rests 0.4 ETH @ 1990 (maker)
	s1 := h.place(t, ctx, limit(seller, "s1", matching.Sell, "1990", "0.4"))
	assert.Equal(t, trading.StatusOpen, s1.Order.Status)
	assert.Empty(t, s1.Trades)
	eq(t, "0.4", h.balance(t, ctx, seller, "ETH").Hold)
	eq(t, "0.6", h.balance(t, ctx, seller, "ETH").Available)
	eq(t, "0.4", s1.Order.HoldRemaining)

	// B buys 1.0 ETH @ 2000 (taker): fills 0.4 @ 1990, rests 0.6 @ 2000
	b1 := h.place(t, ctx, limit(buyer, "b1", matching.Buy, "2000", "1"))
	require.Equal(t, trading.StatusPartiallyFilled, b1.Order.Status)
	require.Len(t, b1.Trades, 1)
	tr := b1.Trades[0]
	eq(t, "1990", tr.Price)
	eq(t, "0.4", tr.Qty)
	eq(t, "796", tr.QuoteQty)
	eq(t, "0.796", tr.MakerFee)
	assert.Equal(t, "USDC", tr.MakerFeeAsset)
	eq(t, "0.0008", tr.TakerFee)
	assert.Equal(t, "ETH", tr.TakerFeeAsset)
	assert.Equal(t, matching.Buy, tr.TakerSide)
	eq(t, "0.4", b1.Order.FilledQty)
	eq(t, "796", b1.Order.FilledQuote)
	eq(t, "0.6", b1.Order.RemainingQty)
	eq(t, "2000", b1.Order.HoldAmount)
	eq(t, "1200", b1.Order.HoldRemaining, "2000 − 796 − 4 (price improvement)")

	// balances exactly as the plan's worked example
	eq(t, "8004", h.balance(t, ctx, buyer, "USDC").Available)
	eq(t, "1200", h.balance(t, ctx, buyer, "USDC").Hold)
	eq(t, "0.3992", h.balance(t, ctx, buyer, "ETH").Available)
	eq(t, "795.204", h.balance(t, ctx, seller, "USDC").Available)
	eq(t, "0.6", h.balance(t, ctx, seller, "ETH").Available)
	eq(t, "0", h.balance(t, ctx, seller, "ETH").Hold)
	houses, err := h.svc2().HouseBalances(ctx)
	require.NoError(t, err)
	fees := map[string]money.Amount{}
	for _, hb := range houses {
		if hb.Code == ledger.HouseFeeRevenue {
			fees[hb.Asset] = hb.Balance
		}
	}
	eq(t, "0.796", fees["USDC"])
	eq(t, "0.0008", fees["ETH"])

	// the maker is filled and terminal
	s1After, err := h.svc.GetOrder(ctx, seller, s1.Order.ID)
	require.NoError(t, err)
	assert.Equal(t, trading.StatusFilled, s1After.Status)
	eq(t, "0", s1After.HoldRemaining)

	// depth shows the resting 0.6 @ 2000
	depth, err := h.svc.Depth(ctx, market, 5)
	require.NoError(t, err)
	require.Len(t, depth.Bids, 1)
	eq(t, "2000", depth.Bids[0].Price)
	eq(t, "0.6", depth.Bids[0].Qty)
	assert.Empty(t, depth.Asks)

	// fills seen from each side
	buyerFills, err := h.svc.ListFills(ctx, trading.FillsFilter{AccountID: buyer})
	require.NoError(t, err)
	require.Len(t, buyerFills, 1)
	assert.False(t, buyerFills[0].IsMaker)
	eq(t, "0.0008", buyerFills[0].Fee)
	sellerFills, err := h.svc.ListFills(ctx, trading.FillsFilter{AccountID: seller})
	require.NoError(t, err)
	require.Len(t, sellerFills, 1)
	assert.True(t, sellerFills[0].IsMaker)
	assert.Equal(t, matching.Sell, sellerFills[0].Side)

	// (c) cancel the remainder: 1200 USDC released
	cancelled, err := h.svc.CancelOrder(ctx, trading.CancelRequest{AccountID: buyer, OrderID: b1.Order.ID})
	require.NoError(t, err)
	assert.Equal(t, trading.StatusCancelled, cancelled.Status)
	assert.Equal(t, matching.CancelByUser, cancelled.CancelReason)
	eq(t, "0", cancelled.HoldRemaining)
	eq(t, "9204", h.balance(t, ctx, buyer, "USDC").Available)
	eq(t, "0", h.balance(t, ctx, buyer, "USDC").Hold)
	// cancelling again is idempotent
	again, err := h.svc.CancelOrder(ctx, trading.CancelRequest{AccountID: buyer, OrderID: b1.Order.ID})
	require.NoError(t, err)
	assert.Equal(t, trading.StatusCancelled, again.Status)

	h.assertTrialBalanceZero(t, ctx)
	h.assertHoldInvariant(t, ctx)
	h.assertSequenceConsistent(t, ctx)
	h.assertCacheMatchesPostings(t, ctx, buyer)
	h.assertCacheMatchesPostings(t, ctx, seller)

	// events: per command, in order; account_seq strictly increasing per account
	rows := h.outbox(t, ctx)
	types := eventTypes(rows)
	assert.Equal(t, []string{
		"order.accepted", "balance.updated", // s1
		"order.accepted", "trade.executed", "order.filled", "order.updated", "balance.updated", "balance.updated", "balance.updated", "balance.updated", // b1
		"order.cancelled", "balance.updated", // cancel
	}, types)
	perAccount := map[string]int64{}
	for _, r := range rows {
		if r.AccountID == nil {
			assert.Nil(t, r.AccountSeq, "%s has no account", r.EventType)
			continue
		}
		require.NotNil(t, r.AccountSeq, "%s account_seq", r.EventType)
		assert.Greater(t, *r.AccountSeq, perAccount[*r.AccountID], "%s account_seq must increase", r.EventType)
		perAccount[*r.AccountID] = *r.AccountSeq
	}
	assert.Equal(t, "ex.v1.trade.executed.default.ETH-USDC", rows[3].Subject)
	assert.Equal(t, "ex.v1.balance.updated.default."+seller, rows[1].Subject)
}

func TestTradingClientOrderIDIsIdempotent(t *testing.T) {
	h := setupTrading(t)
	ctx := context.Background()
	buyer := h.account(t, ctx, map[string]string{"USDC": "5000"})
	req := limit(buyer, "same", matching.Buy, "2000", "1")
	first := h.place(t, ctx, req)
	require.False(t, first.Replayed)
	for i := 0; i < 10; i++ {
		res := h.place(t, ctx, req)
		assert.True(t, res.Replayed, "replay %d", i)
		assert.Equal(t, first.Order.ID, res.Order.ID)
	}
	orders, err := h.svc.ListOrders(ctx, trading.OrdersFilter{AccountID: buyer})
	require.NoError(t, err)
	assert.Len(t, orders, 1, "ten replays create no second order")
	eq(t, "2000", h.balance(t, ctx, buyer, "USDC").Hold, "one hold only")
	entries, err := h.svc2().Entries(ctx, ledger.EntriesFilter{RefType: "order", RefID: first.Order.ID})
	require.NoError(t, err)
	assert.Len(t, entries, 1)

	// the same client id with different content is refused
	other := limit(buyer, "same", matching.Buy, "1999", "1")
	_, err = h.svc.PlaceOrder(ctx, other)
	assert.ErrorIs(t, err, trading.ErrClientOrderIDMismatch)
	h.assertHoldInvariant(t, ctx)
}

func TestTradingRejections(t *testing.T) {
	h := setupTrading(t)
	ctx := context.Background()
	poor := h.account(t, ctx, map[string]string{"USDC": "100"})
	rich := h.account(t, ctx, map[string]string{"USDC": "100000", "ETH": "10"})

	check := func(name string, req trading.PlaceOrderRequest, want trading.RejectReason, seqSet bool) {
		res, err := h.svc.PlaceOrder(ctx, req)
		require.NoError(t, err, name)
		assert.Equal(t, trading.StatusRejected, res.Order.Status, name)
		assert.Equal(t, want, res.Order.RejectReason, name)
		assert.Equal(t, seqSet, res.Order.Seq != nil, "%s seq", name)
		eq(t, "0", res.Order.HoldAmount, name)
		entries, err := h.svc2().Entries(ctx, ledger.EntriesFilter{RefType: "order", RefID: res.Order.ID})
		require.NoError(t, err)
		assert.Empty(t, entries, "%s must not post", name)
		// rejected orders are persisted and visible
		got, err := h.svc.GetOrder(ctx, req.AccountID, res.Order.ID)
		require.NoError(t, err)
		assert.Equal(t, trading.StatusRejected, got.Status)
	}
	check("insufficient", limit(poor, "r1", matching.Buy, "2000", "1"), trading.RejectInsufficientBalance, true)
	check("tick", limit(rich, "r2", matching.Buy, "2000.005", "1"), trading.RejectReason(matching.RejectInvalidPriceTick), true)
	check("step", limit(rich, "r3", matching.Buy, "2000", "1.00005"), trading.RejectReason(matching.RejectInvalidQtyStep), true)
	check("notional", limit(rich, "r4", matching.Buy, "2000", "0.0001"), trading.RejectReason(matching.RejectBelowMinNotional), true)
	check("empty book market buy", marketBuy(rich, "r5", "500"), trading.RejectReason(matching.RejectEmptyBook), true)
	check("empty book market sell", marketSell(rich, "r6", "1"), trading.RejectReason(matching.RejectEmptyBook), true)
	eq(t, "100", h.balance(t, ctx, poor, "USDC").Available)
	eq(t, "0", h.balance(t, ctx, poor, "USDC").Hold)
	eq(t, "0", h.balance(t, ctx, rich, "USDC").Hold)

	// frozen account: refused before the book, no seq consumed
	_, err := h.svc2().SetAccountStatus(ctx, h.all, poor, ledger.StatusFrozen)
	require.NoError(t, err)
	check("frozen", limit(poor, "r7", matching.Buy, "1", "5"), trading.RejectAccountFrozen, false)

	// halted market: new orders refused, cancels still work
	resting := h.place(t, ctx, limit(rich, "rest", matching.Sell, "3000", "1"))
	_, err = h.all.Exec(ctx, `UPDATE registry.markets SET status = 'halted' WHERE symbol = $1`, market)
	require.NoError(t, err)
	require.NoError(t, h.engine.Reload(ctx))
	check("halted", limit(rich, "r8", matching.Buy, "3000", "1"), trading.RejectMarketNotActive, false)
	cancelled, err := h.svc.CancelOrder(ctx, trading.CancelRequest{AccountID: rich, OrderID: resting.Order.ID})
	require.NoError(t, err)
	assert.Equal(t, trading.StatusCancelled, cancelled.Status)
	_, err = h.all.Exec(ctx, `UPDATE registry.markets SET status = 'active' WHERE symbol = $1`, market)
	require.NoError(t, err)
	require.NoError(t, h.engine.Reload(ctx))

	// cancels: unknown, foreign, malformed
	_, err = h.svc.CancelOrder(ctx, trading.CancelRequest{AccountID: rich, OrderID: "01J000000000000000000000NO"})
	assert.ErrorIs(t, err, trading.ErrOrderNotFound)
	mine := h.place(t, ctx, limit(rich, "mine", matching.Sell, "3000", "1"))
	_, err = h.svc.CancelOrder(ctx, trading.CancelRequest{AccountID: poor, OrderID: mine.Order.ID})
	assert.ErrorIs(t, err, trading.ErrOrderNotFound, "another account's order")
	_, err = h.svc.PlaceOrder(ctx, trading.PlaceOrderRequest{AccountID: rich, MarketSymbol: "NOPE-USDC", ClientOrderID: "x", Side: matching.Buy, Type: matching.Limit, Price: amt("1"), Qty: amt("1")})
	assert.ErrorIs(t, err, trading.ErrMarketNotFound)
	_, err = h.svc.PlaceOrder(ctx, trading.PlaceOrderRequest{AccountID: rich, MarketSymbol: market, ClientOrderID: "x", Side: matching.Buy, Type: matching.Limit, Qty: amt("1")})
	assert.ErrorIs(t, err, trading.ErrInvalidRequest)

	h.assertTrialBalanceZero(t, ctx)
	h.assertHoldInvariant(t, ctx)
	h.assertSequenceConsistent(t, ctx)
	for _, r := range h.outbox(t, ctx) {
		if r.EventType == "order.rejected" {
			require.NotNil(t, r.AccountID)
			require.NotNil(t, r.AccountSeq)
		}
	}
}

func TestTradingMarketOrdersAndIOC(t *testing.T) {
	h := setupTrading(t)
	ctx := context.Background()
	buyer := h.account(t, ctx, map[string]string{"USDC": "10000"})
	seller := h.account(t, ctx, map[string]string{"ETH": "5"})

	h.place(t, ctx, limit(seller, "a1", matching.Sell, "2000", "0.5"))
	h.place(t, ctx, limit(seller, "a2", matching.Sell, "2010", "0.5"))
	eq(t, "1", h.balance(t, ctx, seller, "ETH").Hold)

	// market buy with a 1500 USDC budget: 0.5 @ 2000 = 1000, then
	// floor(500 / 2010, 0.0001) = 0.2487 @ 2010 = 499.887; 0.113 left → released
	mb := h.place(t, ctx, marketBuy(buyer, "m1", "1500"))
	require.Len(t, mb.Trades, 2)
	eq(t, "0.5", mb.Trades[0].Qty)
	eq(t, "0.2487", mb.Trades[1].Qty)
	eq(t, "499.887", mb.Trades[1].QuoteQty)
	assert.Equal(t, trading.StatusCancelled, mb.Order.Status)
	assert.Equal(t, matching.CancelIOC, mb.Order.CancelReason)
	eq(t, "0.7487", mb.Order.FilledQty)
	eq(t, "1499.887", mb.Order.FilledQuote)
	eq(t, "0", mb.Order.HoldRemaining)
	eq(t, "1500", mb.Order.HoldAmount)
	eq(t, "8500.113", h.balance(t, ctx, buyer, "USDC").Available)
	eq(t, "0", h.balance(t, ctx, buyer, "USDC").Hold)
	// taker fee 20 bps in ETH, ceil to 18 decimals: 0.7487 × 0.002 = 0.0014974
	eq(t, "0.7472026", h.balance(t, ctx, buyer, "ETH").Available)
	eq(t, "0.2513", h.balance(t, ctx, seller, "ETH").Hold, "a2 remainder still frozen")

	// market sell by base qty into the buyer's bid
	h.place(t, ctx, limit(buyer, "bid", matching.Buy, "1990", "0.3"))
	ms := h.place(t, ctx, marketSell(seller, "m2", "1"))
	require.Len(t, ms.Trades, 1)
	eq(t, "0.3", ms.Trades[0].Qty)
	eq(t, "1990", ms.Trades[0].Price)
	assert.Equal(t, trading.StatusCancelled, ms.Order.Status, "0.7 ETH unfilled → IOC cancel")
	eq(t, "0.7", ms.Order.RemainingQty)
	eq(t, "0", ms.Order.HoldRemaining)

	// IOC limit: fills what crosses, cancels the rest
	ioc := trading.PlaceOrderRequest{AccountID: buyer, MarketSymbol: market, ClientOrderID: "ioc", Side: matching.Buy, Type: matching.Limit, TimeInForce: matching.IOC, Price: amt("2010"), Qty: amt("1")}
	res := h.place(t, ctx, ioc)
	require.Len(t, res.Trades, 1)
	eq(t, "0.2513", res.Trades[0].Qty)
	assert.Equal(t, trading.StatusCancelled, res.Order.Status)
	eq(t, "0", res.Order.HoldRemaining)

	// self-trade prevention: the buyer's own ask is not crossed
	h.place(t, ctx, limit(buyer, "own-ask", matching.Sell, "2050", "0.1"))
	stp := h.place(t, ctx, limit(buyer, "stp", matching.Buy, "2050", "0.2"))
	assert.Equal(t, trading.StatusCancelled, stp.Order.Status)
	assert.Equal(t, matching.CancelSelfTrade, stp.Order.CancelReason)
	assert.Empty(t, stp.Trades)

	h.assertTrialBalanceZero(t, ctx)
	h.assertHoldInvariant(t, ctx)
	h.assertSequenceConsistent(t, ctx)
	h.assertCacheMatchesPostings(t, ctx, buyer)
	h.assertCacheMatchesPostings(t, ctx, seller)
}

// TestTradingRestartRestoresBook is the Phase 3 DoD "kill the engine and
// restart: book and holds agree" (ADR-0002).
func TestTradingRestartRestoresBook(t *testing.T) {
	h := setupTrading(t)
	ctx := context.Background()
	accounts := make([]string, 4)
	for i := range accounts {
		accounts[i] = h.account(t, ctx, map[string]string{"USDC": "100000", "ETH": "50"})
	}
	var placed []trading.Order
	for i := 0; i < 60; i++ {
		acct := accounts[i%len(accounts)]
		var req trading.PlaceOrderRequest
		switch i % 5 {
		case 0, 1:
			req = limit(acct, fmt.Sprintf("b%d", i), matching.Buy, fmt.Sprintf("%d", 1900+(i%7)*10), fmt.Sprintf("0.%d", 1+i%9))
		case 2, 3:
			req = limit(acct, fmt.Sprintf("s%d", i), matching.Sell, fmt.Sprintf("%d", 1950+(i%7)*10), fmt.Sprintf("0.%d", 1+i%9))
		default:
			req = marketBuy(acct, fmt.Sprintf("m%d", i), "500")
		}
		res := h.place(t, ctx, req)
		placed = append(placed, res.Order)
		if i%7 == 6 { // cancel something that may still be open
			_, err := h.svc.CancelOrder(ctx, trading.CancelRequest{AccountID: placed[i-3].AccountID, OrderID: placed[i-3].ID})
			require.NoError(t, err)
		}
	}
	before, err := h.engine.Snapshot(ctx, market)
	require.NoError(t, err)
	require.NotEmpty(t, before.Bids)
	require.NotEmpty(t, before.Asks)
	h.assertHoldInvariant(t, ctx)

	h.crash(t)

	after, err := h.engine.Snapshot(ctx, market)
	require.NoError(t, err)
	assert.True(t, before.Equal(after), "book after restart differs\nbefore: %+v\nafter: %+v", before, after)
	h.assertHoldInvariant(t, ctx)
	h.assertTrialBalanceZero(t, ctx)
	h.assertSequenceConsistent(t, ctx)

	// the restored engine keeps trading and numbering where the old one stopped
	res := h.place(t, ctx, marketSell(accounts[0], "after-restart", "0.05"))
	require.NotEqual(t, trading.StatusRejected, res.Order.Status)
	require.NotNil(t, res.Order.Seq)
	assert.Equal(t, before.LastSeq+1, *res.Order.Seq)
	h.assertHoldInvariant(t, ctx)
	h.assertTrialBalanceZero(t, ctx)
	for _, a := range accounts {
		h.assertCacheMatchesPostings(t, ctx, a)
	}
}

// TestTradingPropertyRandomOperations drives random order flow (rapid) and
// checks the ledger and book invariants after every sequence, including a
// restart.
func TestTradingPropertyRandomOperations(t *testing.T) {
	h := setupTrading(t)
	ctx := context.Background()
	accounts := make([]string, 3)
	for i := range accounts {
		accounts[i] = h.account(t, ctx, map[string]string{"USDC": "1000000", "ETH": "1000"})
	}
	var seqNo int
	var open []trading.Order
	rapid.Check(t, func(rt *rapid.T) {
		seqNo++
		n := rapid.IntRange(3, 12).Draw(rt, "ops")
		for i := 0; i < n; i++ {
			acct := accounts[rapid.IntRange(0, len(accounts)-1).Draw(rt, "acct")]
			cid := fmt.Sprintf("p%d-%d", seqNo, i)
			var req trading.PlaceOrderRequest
			switch rapid.IntRange(0, 9).Draw(rt, "kind") {
			case 0, 1, 2, 3:
				price := fmt.Sprintf("%d.%02d", rapid.IntRange(1900, 2100).Draw(rt, "px"), rapid.IntRange(0, 99).Draw(rt, "cents"))
				qty := fmt.Sprintf("0.%04d", rapid.IntRange(30, 9999).Draw(rt, "qty"))
				req = limit(acct, cid, matching.Buy, price, qty)
			case 4, 5, 6, 7:
				price := fmt.Sprintf("%d.%02d", rapid.IntRange(1900, 2100).Draw(rt, "px"), rapid.IntRange(0, 99).Draw(rt, "cents"))
				qty := fmt.Sprintf("0.%04d", rapid.IntRange(30, 9999).Draw(rt, "qty"))
				req = limit(acct, cid, matching.Sell, price, qty)
			case 8:
				req = marketBuy(acct, cid, fmt.Sprintf("%d", rapid.IntRange(10, 3000).Draw(rt, "quote")))
			default:
				req = marketSell(acct, cid, fmt.Sprintf("0.%04d", rapid.IntRange(1, 9999).Draw(rt, "qty")))
			}
			if rapid.Bool().Draw(rt, "ioc") && req.Type == matching.Limit {
				req.TimeInForce = matching.IOC
			}
			res, err := h.svc.PlaceOrder(ctx, req)
			require.NoError(rt, err)
			if !res.Order.Status.Terminal() {
				open = append(open, res.Order)
			}
			if len(open) > 0 && rapid.IntRange(0, 3).Draw(rt, "cancel") == 0 {
				j := rapid.IntRange(0, len(open)-1).Draw(rt, "which")
				_, err := h.svc.CancelOrder(ctx, trading.CancelRequest{AccountID: open[j].AccountID, OrderID: open[j].ID})
				require.NoError(rt, err)
				open = append(open[:j], open[j+1:]...)
			}
		}
		h.assertHoldInvariant(rt, ctx)
	})
	before, err := h.engine.Snapshot(ctx, market)
	require.NoError(t, err)
	h.crash(t)
	after, err := h.engine.Snapshot(ctx, market)
	require.NoError(t, err)
	assert.True(t, before.Equal(after), "book after restart differs")
	h.assertTrialBalanceZero(t, ctx)
	h.assertHoldInvariant(t, ctx)
	h.assertSequenceConsistent(t, ctx)
	for _, a := range accounts {
		h.assertCacheMatchesPostings(t, ctx, a)
	}
}

// TestTradingConcurrentSubmit hammers one market from many goroutines; the
// runner serialises them and every invariant holds.
func TestTradingConcurrentSubmit(t *testing.T) {
	h := setupTrading(t)
	ctx := context.Background()
	const workers, perWorker = 40, 10
	accounts := make([]string, workers)
	for i := range accounts {
		accounts[i] = h.account(t, ctx, map[string]string{"USDC": "50000", "ETH": "20"})
	}
	var (
		g      errgroup.Group
		mu     sync.Mutex
		status = map[trading.Status]int{}
	)
	for w := 0; w < workers; w++ {
		w := w
		g.Go(func() error {
			for i := 0; i < perWorker; i++ {
				var req trading.PlaceOrderRequest
				price := fmt.Sprintf("%d", 1990+(w+i)%20)
				if (w+i)%2 == 0 {
					req = limit(accounts[w], fmt.Sprintf("c%d-%d", w, i), matching.Buy, price, "0.5")
				} else {
					req = limit(accounts[w], fmt.Sprintf("c%d-%d", w, i), matching.Sell, price, "0.5")
				}
				res, err := h.svc.PlaceOrder(ctx, req)
				if err != nil {
					return err
				}
				mu.Lock()
				status[res.Order.Status]++
				mu.Unlock()
			}
			return nil
		})
	}
	require.NoError(t, g.Wait())
	total := 0
	for _, n := range status {
		total += n
	}
	assert.Equal(t, workers*perWorker, total)
	assert.Zero(t, status[trading.StatusRejected])
	t.Logf("statuses: %v", status)
	h.assertTrialBalanceZero(t, ctx)
	h.assertHoldInvariant(t, ctx)
	h.assertSequenceConsistent(t, ctx)
	for _, a := range accounts {
		h.assertCacheMatchesPostings(t, ctx, a)
	}
	// every order seq is unique and dense up to last_seq
	rows, err := h.all.Query(ctx, `SELECT seq FROM trading.orders WHERE seq IS NOT NULL ORDER BY seq`)
	require.NoError(t, err)
	var seqs []int64
	for rows.Next() {
		var s int64
		require.NoError(t, rows.Scan(&s))
		seqs = append(seqs, s)
	}
	rows.Close()
	require.Len(t, seqs, workers*perWorker)
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i, s := range seqs {
		require.Equal(t, int64(i+1), s, "seq gap")
	}
}

// TestEngineSingleInstanceLock: a second engine cannot start while the
// first holds the advisory lock (docs/plan-v1.0.md §5.1 "exactly one").
func TestEngineSingleInstanceLock(t *testing.T) {
	h := setupTrading(t)
	second := trading.NewEngine(h.all, h.svc2(), registry.NewCache("default"), h.store, "default", h.log)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	err := second.Start(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	h.engine.Stop()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	require.NoError(t, second.Start(ctx2))
	second.Stop()
	h.startEngine(t) // leave a running engine for the cleanup order
}
