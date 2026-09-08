package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/apiclient"
)

func fakeMarketDataAPI(t *testing.T) *httptest.Server {
	t.Helper()
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/markets/{symbol}/ticker", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("symbol") != "ETH-USDC" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(apiclient.Problem{Type: "about:blank", Title: "Not Found", Status: 404, Detail: "market NOPE-USDC does not exist"})
			return
		}
		writeJSON(w, http.StatusOK, apiclient.Ticker{Market: "ETH-USDC", LastPrice: amtP("2010"), Open: amtP("2000"), High: amtP("2020"), Low: amtP("1990"),
			Change: amtP("10"), ChangePct: amtP("0.5"), Volume: amt("3"), QuoteVolume: amt("6030"), Trades: 4, At: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)})
	})
	mux.HandleFunc("GET /v1/markets/{symbol}/klines", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		assert.Equal(t, "5m", q.Get("interval"))
		assert.Equal(t, "3", q.Get("limit"))
		assert.Equal(t, "2026-09-05T11:45:00Z", q.Get("from"))
		start := time.Date(2026, 9, 5, 11, 45, 0, 0, time.UTC)
		var ks []apiclient.Kline
		for i := 0; i < 3; i++ {
			ks = append(ks, apiclient.Kline{Market: "ETH-USDC", Interval: "5m", Start: start.Add(time.Duration(i) * 5 * time.Minute),
				Open: amt("2000"), High: amt("2010"), Low: amt("1995"), Close: amt("2005"), Volume: amt("1"), QuoteVolume: amt("2000"), Trades: int64(i)})
		}
		writeJSON(w, http.StatusOK, apiclient.KlineList{Market: "ETH-USDC", Interval: "5m", Klines: ks})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestTickerAndKlinesCommands(t *testing.T) {
	srv := fakeMarketDataAPI(t)
	out, err := run(t, "--base-url", srv.URL, "ticker", "ETH-USDC")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 2)
	assert.True(t, strings.HasPrefix(lines[0], "MARKET"), lines[0])
	assert.Contains(t, lines[1], "2010")
	assert.Contains(t, lines[1], "0.5")

	out, err = run(t, "--base-url", srv.URL, "--output", "json", "ticker", "ETH-USDC")
	require.NoError(t, err)
	assert.Contains(t, out, `"last_price": "2010"`)

	_, err = run(t, "--base-url", srv.URL, "ticker", "NOPE-USDC")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")

	out, err = run(t, "--base-url", srv.URL, "klines", "ETH-USDC", "--interval", "5m", "--limit", "3", "--from", "2026-09-05T11:45:00Z")
	require.NoError(t, err)
	lines = strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 4, out)
	assert.True(t, strings.HasPrefix(lines[0], "START"), lines[0])
	assert.Contains(t, lines[1], "2026-09-05T11:45:00Z")
	assert.Contains(t, lines[3], "2026-09-05T11:55:00Z")

	_, err = run(t, "--base-url", srv.URL, "klines", "ETH-USDC", "--from", "yesterday")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--from")
}
