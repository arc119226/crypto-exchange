//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/marketdata"
	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

// TestKlineWriterAggregatesAndResumes: the worker folds trading.trades into
// marketdata.klines under its own grants, moves the cursor with the candles,
// counts nothing twice across ticks and restarts, and picks up new trades
// (docs/plan-v1.0.md §12 Phase 6 "重啟從最後一根 K 線的 seq 重放").
func TestKlineWriterAggregatesAndResumes(t *testing.T) {
	h := setupTrading(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	workerPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_worker"), MaxConns: 4})
	require.NoError(t, err)
	defer workerPool.Close()
	workerStore := marketdata.NewStore(workerPool, "default")
	writer := marketdata.NewKlineWriter(workerStore, registry.NewStore(workerPool), "default", 2, h.log)
	reader := marketdata.NewStore(h.all, "default")
	market := h.cache.Markets()[0]

	// nothing to fold yet: a tick is a no-op and the writer is caught up
	n, caughtUp, err := writer.Tick(ctx)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.True(t, caughtUp)

	buyer := h.account(t, ctx, map[string]string{"USDC": "100000"})
	seller := h.account(t, ctx, map[string]string{"ETH": "10"})
	h.place(t, ctx, limit(seller, "s1", matching.Sell, "2000", "1"))
	h.place(t, ctx, limit(seller, "s2", matching.Sell, "2010", "1"))
	s3 := h.place(t, ctx, limit(seller, "s3", matching.Sell, "2020", "1"))
	b1 := h.place(t, ctx, limit(buyer, "b1", matching.Buy, "2010", "1.5")) // two trades: 2000 x 1, 2010 x 0.5
	require.Len(t, b1.Trades, 2)
	b2 := h.place(t, ctx, limit(buyer, "b2", matching.Buy, "2020", "0.25")) // one trade at 2010
	require.Len(t, b2.Trades, 1)
	last, err := reader.MarketSeq(ctx, market.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 5, last)

	// batch size 2: three passes to cover five commands
	total := 0
	for i := 0; i < 5; i++ {
		n, caughtUp, err = writer.Tick(ctx)
		require.NoError(t, err)
		total += n
		if caughtUp {
			break
		}
	}
	assert.True(t, caughtUp)
	assert.Equal(t, 3, total, "three trades folded")
	cursor, err := reader.KlineCursor(ctx, market.Symbol)
	require.NoError(t, err)
	assert.EqualValues(t, 5, cursor, "the cursor reaches the market seq")

	now := time.Now().UTC()
	ones, err := reader.Klines(ctx, market.Symbol, marketdata.Interval1m, now.Add(-time.Hour), now.Add(time.Minute), 100)
	require.NoError(t, err)
	require.NotEmpty(t, ones)
	sum := func(cs []marketdata.Candle) (vol money.Amount, trades int64) {
		vol = money.Zero
		for _, c := range cs {
			vol = vol.Add(c.Volume)
			trades += c.Trades
		}
		return vol, trades
	}
	vol, trades := sum(ones)
	assert.Equal(t, "1.75", vol.String(), "Σ volume = Σ trade qty")
	assert.EqualValues(t, 3, trades)
	assert.Equal(t, "2000", ones[0].Open.String(), "the first trade opens")
	lastCandle := ones[len(ones)-1]
	assert.Equal(t, "2010", lastCandle.Close.String())
	for _, iv := range marketdata.Intervals() {
		cs, err := reader.Klines(ctx, market.Symbol, iv, now.Add(-48*time.Hour), now.Add(time.Minute), 100)
		require.NoError(t, err)
		v, n := sum(cs)
		assert.Equal(t, "1.75", v.String(), "%s holds every trade once", iv)
		assert.EqualValues(t, 3, n, iv)
		assert.Equal(t, "2010", cs[0].High.String(), iv)
		assert.Equal(t, "2000", cs[0].Low.String(), iv)
	}

	// idempotent: another tick, and a fresh writer, change nothing
	n, _, err = writer.Tick(ctx)
	require.NoError(t, err)
	assert.Zero(t, n)
	again := marketdata.NewKlineWriter(workerStore, registry.NewStore(workerPool), "default", 500, h.log)
	n, _, err = again.Tick(ctx)
	require.NoError(t, err)
	assert.Zero(t, n)
	ones2, err := reader.Klines(ctx, market.Symbol, marketdata.Interval1m, now.Add(-time.Hour), now.Add(time.Minute), 100)
	require.NoError(t, err)
	v2, _ := sum(ones2)
	assert.Equal(t, "1.75", v2.String())

	// new trades are picked up; a cancel advances the seq without trades
	_, err = h.svc.CancelOrder(ctx, trading.CancelRequest{AccountID: seller, OrderID: s3.Order.ID})
	require.NoError(t, err)
	b3 := h.place(t, ctx, limit(buyer, "b3", matching.Buy, "2010", "0.5")) // takes the 0.25 left of s2, rests 0.25
	require.Len(t, b3.Trades, 1)
	for i := 0; i < 5; i++ {
		if _, caughtUp, err = writer.Tick(ctx); caughtUp || err != nil {
			break
		}
	}
	require.NoError(t, err)
	cursor, err = reader.KlineCursor(ctx, market.Symbol)
	require.NoError(t, err)
	assert.EqualValues(t, 7, cursor)
	ones3, err := reader.Klines(ctx, market.Symbol, marketdata.Interval1m, now.Add(-time.Hour), now.Add(time.Minute), 100)
	require.NoError(t, err)
	v3, n3 := sum(ones3)
	assert.Equal(t, "2", v3.String())
	assert.EqualValues(t, 4, n3)

	// the ticker and the ring seed read what was written
	tk, err := reader.Ticker(ctx, market.Symbol, now.Add(time.Minute))
	require.NoError(t, err)
	require.NotNil(t, tk.Last)
	assert.Equal(t, "2010", tk.Last.String())
	assert.Equal(t, "2000", tk.Open.String())
	assert.Equal(t, "2", tk.Volume.String())
	assert.EqualValues(t, 4, tk.Trades)
	seed, err := reader.RingSeed(ctx, market.ID, market.Symbol, now)
	require.NoError(t, err)
	assert.EqualValues(t, 7, seed.Seq)
	assert.Empty(t, seed.Trades, "the writer is caught up: nothing unfolded")
	assert.NotEmpty(t, seed.Candles)
	_, err = reader.LastCloseBefore(ctx, market.Symbol, marketdata.Interval1m, now.Add(time.Hour))
	require.NoError(t, err)

	// a trade the writer has not folded yet shows up in the seed instead
	s4 := h.place(t, ctx, limit(seller, "s4", matching.Sell, "2000", "0.1")) // hits b3's resting bid
	require.Len(t, s4.Trades, 1)
	seed, err = reader.RingSeed(ctx, market.ID, market.Symbol, now)
	require.NoError(t, err)
	assert.EqualValues(t, 8, seed.Seq)
	require.Len(t, seed.Trades, 1)
	assert.EqualValues(t, 8, seed.Trades[0].Seq)

	t.Run("only the worker writes candles", func(t *testing.T) {
		streamPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_stream"), MaxConns: 2})
		require.NoError(t, err)
		defer streamPool.Close()
		err = marketdata.NewStore(streamPool, "default").ApplyCandles(ctx, market.Symbol, nil, 99)
		var pgErr *pgconn.PgError
		require.True(t, errors.As(err, &pgErr), "%v", err)
		assert.Equal(t, "42501", pgErr.Code, "ex_stream has no INSERT on marketdata.kline_cursors")
		_, err = marketdata.NewStore(streamPool, "default").Klines(ctx, market.Symbol, marketdata.Interval1m, now.Add(-time.Hour), now.Add(time.Minute), 10)
		require.NoError(t, err, "but it reads them")
	})
}

// TestStoreOpenOrdersSnapshotIsConsistent: the shadow book's snapshot
// carries the seq the orders are current at, and a book restored from it
// equals the engine's own depth.
func TestStoreOpenOrdersSnapshotIsConsistent(t *testing.T) {
	h := setupTrading(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	market := h.cache.Markets()[0]

	streamPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_stream"), MaxConns: 2})
	require.NoError(t, err)
	defer streamPool.Close()
	store := marketdata.NewStore(streamPool, "default")

	snap, err := store.OpenOrders(ctx, market.ID, market.Symbol)
	require.NoError(t, err)
	assert.Zero(t, snap.Seq)
	assert.Empty(t, snap.Orders)

	buyer := h.account(t, ctx, map[string]string{"USDC": "100000"})
	seller := h.account(t, ctx, map[string]string{"ETH": "10"})
	h.place(t, ctx, limit(seller, "s1", matching.Sell, "2000", "1"))
	h.place(t, ctx, limit(seller, "s2", matching.Sell, "2000", "0.5"))
	h.place(t, ctx, limit(seller, "s3", matching.Sell, "2010", "2"))
	h.place(t, ctx, limit(buyer, "b1", matching.Buy, "1990", "1"))
	h.place(t, ctx, limit(buyer, "b2", matching.Buy, "2000", "1.2")) // takes s1 and 0.2 of s2
	h.place(t, ctx, limit(buyer, "b3", matching.Buy, "1980", "3"))

	snap, err = store.OpenOrders(ctx, market.ID, market.Symbol)
	require.NoError(t, err)
	want, err := h.engine.Depth(ctx, market.Symbol, 0)
	require.NoError(t, err)
	assert.Equal(t, want.LastSeq, snap.Seq)

	book := marketdata.NewBook(market.Symbol)
	require.NoError(t, book.Restore(snap))
	got := book.Depth(0)
	require.Len(t, got.Bids, len(want.Bids))
	require.Len(t, got.Asks, len(want.Asks))
	for i := range want.Bids {
		assert.Equal(t, want.Bids[i].Price.String(), got.Bids[i].Price.String())
		assert.True(t, want.Bids[i].Qty.Equal(got.Bids[i].Qty), "bid %s", want.Bids[i].Price)
		assert.Equal(t, want.Bids[i].Orders, got.Bids[i].Orders)
	}
	for i := range want.Asks {
		assert.Equal(t, want.Asks[i].Price.String(), got.Asks[i].Price.String())
		assert.True(t, want.Asks[i].Qty.Equal(got.Asks[i].Qty), "ask %s", want.Asks[i].Price)
		assert.Equal(t, want.Asks[i].Orders, got.Asks[i].Orders)
	}

	seq, err := store.AccountSeq(ctx, buyer)
	require.NoError(t, err)
	assert.Positive(t, seq, "the buyer has private events by now")
	_, err = store.AccountSeq(ctx, "00000000-0000-0000-0000-000000000000")
	require.ErrorIs(t, err, marketdata.ErrAccountNotFound)
}
