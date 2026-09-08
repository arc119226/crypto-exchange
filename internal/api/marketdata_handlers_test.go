package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/api"
	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/marketdata"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

type fakeDepthCache struct {
	depth marketdata.Depth
	at    time.Time
	ok    bool
	err   error
	calls int
}

func (f *fakeDepthCache) GetDepth(context.Context, string) (marketdata.Depth, time.Time, bool, error) {
	f.calls++
	return f.depth, f.at, f.ok, f.err
}

type fakeMarketData struct {
	candles []marketdata.Candle
	prev    *money.Amount
	ticker  marketdata.Ticker
	got     struct {
		from, to time.Time
		limit    int32
	}
}

func (f *fakeMarketData) Klines(_ context.Context, _ string, _ marketdata.Interval, from, to time.Time, limit int32) ([]marketdata.Candle, error) {
	f.got.from, f.got.to, f.got.limit = from, to, limit
	return f.candles, nil
}

func (f *fakeMarketData) Ticker(context.Context, string, time.Time) (marketdata.Ticker, error) {
	return f.ticker, nil
}

func (f *fakeMarketData) LastCloseBefore(context.Context, string, marketdata.Interval, time.Time) (*money.Amount, error) {
	return f.prev, nil
}

var fixedNow = time.Date(2026, 9, 5, 12, 0, 30, 0, time.UTC)

func newMarketDataServer(t *testing.T, d api.Deps) *httptest.Server {
	t.Helper()
	d.Registry = fixtures()
	d.Tenant = "default"
	d.Now = func() time.Time { return fixedNow }
	r := chi.NewRouter()
	r.Use(telemetry.CorrelationMiddleware(slog.New(slog.NewTextHandler(io.Discard, nil))))
	api.Mount(r, api.NewHandler(d))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func TestGetDepthServesAFreshCacheAndFallsBackOtherwise(t *testing.T) {
	cached := marketdata.Depth{Market: "ETH-USDC", Seq: 42,
		Bids: []marketdata.Level{{Price: money.MustParse("2000"), Qty: money.MustParse("1"), Orders: 2}, {Price: money.MustParse("1999"), Qty: money.MustParse("3"), Orders: 1}},
		Asks: []marketdata.Level{{Price: money.MustParse("2001"), Qty: money.MustParse("0.5"), Orders: 1}}}

	t.Run("fresh", func(t *testing.T) {
		cache := &fakeDepthCache{depth: cached, at: fixedNow.Add(-2 * time.Second), ok: true}
		srv := newMarketDataServer(t, api.Deps{DepthCache: cache}) // no Trading: the cache is the only source
		resp, body := get(t, srv.URL+"/v1/markets/ETH-USDC/depth?limit=1", "")
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		var d gen.Depth
		require.NoError(t, json.Unmarshal(body, &d))
		assert.EqualValues(t, 42, d.LastSeq)
		require.Len(t, d.Bids, 1, "limit applies to the cached levels")
		assert.Equal(t, "2000", d.Bids[0].Price.String())
		assert.EqualValues(t, 2, d.Bids[0].Orders)
		assert.Len(t, d.Asks, 1)
		assert.Equal(t, 1, cache.calls)

		resp, _ = get(t, srv.URL+"/v1/markets/NOPE-USDC/depth", "")
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "unknown markets never reach the cache")
		assert.Equal(t, 1, cache.calls)
	})
	t.Run("stale", func(t *testing.T) {
		cache := &fakeDepthCache{depth: cached, at: fixedNow.Add(-11 * time.Second), ok: true}
		srv := newMarketDataServer(t, api.Deps{DepthCache: cache})
		resp, body := get(t, srv.URL+"/v1/markets/ETH-USDC/depth", "")
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "stale: the engine is asked, and this role has none: %s", body)
	})
	t.Run("absent", func(t *testing.T) {
		cache := &fakeDepthCache{ok: false}
		srv := newMarketDataServer(t, api.Deps{DepthCache: cache})
		resp, _ := get(t, srv.URL+"/v1/markets/ETH-USDC/depth", "")
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	})
	t.Run("cache failure is a miss", func(t *testing.T) {
		cache := &fakeDepthCache{err: assert.AnError}
		srv := newMarketDataServer(t, api.Deps{DepthCache: cache})
		resp, _ := get(t, srv.URL+"/v1/markets/ETH-USDC/depth", "")
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	})
	t.Run("custom freshness", func(t *testing.T) {
		cache := &fakeDepthCache{depth: cached, at: fixedNow.Add(-11 * time.Second), ok: true}
		srv := newMarketDataServer(t, api.Deps{DepthCache: cache, DepthFreshness: time.Minute})
		resp, _ := get(t, srv.URL+"/v1/markets/ETH-USDC/depth", "")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

func TestTickerAndKlines(t *testing.T) {
	last, open := money.MustParse("2010"), money.MustParse("2000")
	md := &fakeMarketData{ticker: marketdata.Ticker{Market: "ETH-USDC", Last: &last, Open: &open, Volume: money.MustParse("3"), QuoteVolume: money.MustParse("6000"), Trades: 4, At: fixedNow}}
	srv := newMarketDataServer(t, api.Deps{MarketData: md})

	resp, body := get(t, srv.URL+"/v1/markets/ETH-USDC/ticker", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var tk gen.Ticker
	require.NoError(t, json.Unmarshal(body, &tk))
	require.NotNil(t, tk.LastPrice)
	assert.Equal(t, "2010", tk.LastPrice.String())
	assert.Nil(t, tk.High, "absent when unknown")
	assert.EqualValues(t, 4, tk.Trades)
	assert.Contains(t, string(body), `"last_price":"2010"`, "amounts are strings")
	resp, _ = get(t, srv.URL+"/v1/markets/NOPE-USDC/ticker", "")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// klines: the range is [from, to) cut at limit buckets, gaps filled
	start := fixedNow.Add(-5 * time.Minute).Truncate(time.Minute)
	c := marketdata.NewCandle("ETH-USDC", marketdata.Interval1m, start.Add(2*time.Minute))
	c.Add(marketdata.TradeTick{Price: money.MustParse("2005"), Qty: money.MustParse("1"), QuoteQty: money.MustParse("2005"), At: start.Add(2 * time.Minute)})
	md.candles = []marketdata.Candle{c}
	prev := money.MustParse("1990")
	md.prev = &prev
	resp, body = get(t, srv.URL+"/v1/markets/ETH-USDC/klines?interval=1m&from="+start.Format(time.RFC3339)+"&limit=4", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var kl gen.KlineList
	require.NoError(t, json.Unmarshal(body, &kl))
	assert.Equal(t, gen.KlineInterval("1m"), kl.Interval)
	require.Len(t, kl.Klines, 4, "limit cuts the range at four buckets from `from`")
	assert.Equal(t, start, kl.Klines[0].Start)
	assert.Equal(t, "1990", kl.Klines[0].Close.String(), "the close before the range is carried in")
	assert.EqualValues(t, 0, kl.Klines[0].Trades)
	assert.Equal(t, "2005", kl.Klines[2].Close.String())
	assert.EqualValues(t, 1, kl.Klines[2].Trades)
	assert.Equal(t, "2005", kl.Klines[3].Close.String())
	assert.Equal(t, start, md.got.from)
	assert.Equal(t, start.Add(4*time.Minute), md.got.to)

	// defaults: to = now, from = to - limit x interval
	resp, body = get(t, srv.URL+"/v1/markets/ETH-USDC/klines?interval=1h", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, fixedNow, md.got.to)
	assert.Equal(t, fixedNow.Truncate(time.Hour).Add(-499*time.Hour), md.got.from, "500 buckets including the live one")

	for _, q := range []string{"interval=2m", "interval=1m&limit=0", "interval=1m&limit=1001", "interval=1m&from=2026-09-05T13:00:00Z&to=2026-09-05T12:00:00Z", ""} {
		resp, body = get(t, srv.URL+"/v1/markets/ETH-USDC/klines?"+q, "")
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "%s: %s", q, body)
	}
	resp, _ = get(t, srv.URL+"/v1/markets/NOPE-USDC/klines?interval=1m", "")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// without market data the endpoints say so
	none := newMarketDataServer(t, api.Deps{})
	resp, _ = get(t, none.URL+"/v1/markets/ETH-USDC/ticker", "")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	resp, _ = get(t, none.URL+"/v1/markets/ETH-USDC/klines?interval=1m", "")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}
