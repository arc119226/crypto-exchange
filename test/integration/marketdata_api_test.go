//go:build integration

package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/marketdata"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// TestMarketDataREST: the ticker and klines endpoints serve what the worker
// persisted, with the gap filling and bounds docs/api-conventions.md
// describes.
func TestMarketDataREST(t *testing.T) {
	h := setupAPI(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	market := h.cache.Markets()[0]

	// before any trade: an empty ticker, no candles
	r := h.do(t, http.MethodGet, "/v1/markets/"+market.Symbol+"/ticker", cred{}, nil)
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	tk := decode[gen.Ticker](t, r)
	assert.Nil(t, tk.LastPrice)
	assert.True(t, tk.Volume.IsZero())
	r = h.do(t, http.MethodGet, "/v1/markets/"+market.Symbol+"/klines?interval=1m&limit=5", cred{}, nil)
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	assert.Empty(t, decode[gen.KlineList](t, r).Klines, "no close to carry: nothing to fill")

	seller := h.register(t, "md-seller@example.com")
	buyer := h.register(t, "md-buyer@example.com")
	h.fund(t, ctx, seller.AccountID, "ETH", "5", "faucet:md:eth")
	h.fund(t, ctx, buyer.AccountID, "USDC", "50000", "faucet:md:usdc")
	h.place(t, bearer(seller), limitOrder("s1", "sell", "2000", "1"), http.StatusCreated)
	h.place(t, bearer(seller), limitOrder("s2", "sell", "2010", "1"), http.StatusCreated)
	h.place(t, bearer(buyer), limitOrder("b1", "buy", "2010", "1.5"), http.StatusCreated) // 2000 x 1, 2010 x 0.5

	// the worker folds; here the test drives one directly
	writer := marketdata.NewKlineWriter(marketdata.NewStore(h.all, "default"), registry.NewStore(h.all), "default", 500, h.log)
	for i := 0; i < 5; i++ {
		if _, caughtUp, err := writer.Tick(ctx); caughtUp || err != nil {
			require.NoError(t, err)
			break
		}
	}

	r = h.do(t, http.MethodGet, "/v1/markets/"+market.Symbol+"/ticker", cred{}, nil)
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	tk = decode[gen.Ticker](t, r)
	require.NotNil(t, tk.LastPrice)
	eq(t, "2010", *tk.LastPrice)
	eq(t, "2000", *tk.Open)
	eq(t, "2010", *tk.High)
	eq(t, "2000", *tk.Low)
	eq(t, "1.5", tk.Volume)
	eq(t, "3005", tk.QuoteVolume)
	eq(t, "10", *tk.Change)
	eq(t, "0.5", *tk.ChangePct)
	assert.EqualValues(t, 2, tk.Trades)
	assert.Contains(t, string(r.body), `"last_price":"2010"`)

	r = h.do(t, http.MethodGet, "/v1/markets/"+market.Symbol+"/klines?interval=1m&limit=3", cred{}, nil)
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	kl := decode[gen.KlineList](t, r)
	assert.Equal(t, gen.KlineInterval("1m"), kl.Interval)
	require.NotEmpty(t, kl.Klines)
	require.LessOrEqual(t, len(kl.Klines), 3)
	last := kl.Klines[len(kl.Klines)-1]
	eq(t, "2010", last.Close)
	var traded int64
	for _, k := range kl.Klines {
		traded += k.Trades
	}
	assert.EqualValues(t, 2, traded, "both trades land in the last three minutes")
	for _, iv := range []string{"5m", "15m", "1h", "1d"} {
		r = h.do(t, http.MethodGet, "/v1/markets/"+market.Symbol+"/klines?interval="+iv+"&limit=2", cred{}, nil)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		ks := decode[gen.KlineList](t, r).Klines
		require.NotEmpty(t, ks, iv)
		eq(t, "1.5", ks[len(ks)-1].Volume)
	}

	// a range in the past is filled from the last close before it
	from := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Hour)
	r = h.do(t, http.MethodGet, "/v1/markets/"+market.Symbol+"/klines?interval=1h&from="+from.Format(time.RFC3339)+"&to="+from.Add(2*time.Hour).Format(time.RFC3339), cred{}, nil)
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	assert.Empty(t, decode[gen.KlineList](t, r).Klines, "nothing traded before the range: nothing to carry")

	r = h.do(t, http.MethodGet, "/v1/markets/"+market.Symbol+"/klines?interval=2m", cred{}, nil)
	assert.Equal(t, http.StatusBadRequest, r.status)
	r = h.do(t, http.MethodGet, "/v1/markets/NOPE-USDC/klines?interval=1m", cred{}, nil)
	assert.Equal(t, http.StatusNotFound, r.status)
	r = h.do(t, http.MethodGet, "/v1/markets/NOPE-USDC/ticker", cred{}, nil)
	assert.Equal(t, http.StatusNotFound, r.status)
}
