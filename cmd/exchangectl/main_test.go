package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const marketsJSON = `{"markets":[{"symbol":"ETH-USDC","base_asset":"ETH","quote_asset":"USDC","price_tick":"0.01","qty_step":"0.0001","min_notional":"5","maker_bps":10,"taker_bps":20,"self_trade_policy":"cancel_newest","status":"active"}]}`
const assetsJSON = `{"assets":[{"symbol":"ETH","name":"Ether","chain_id":31337,"contract_address":null,"is_native":true,"scale":18,"display_scale":6,"required_confirmations":1,"min_deposit":"0.001","min_withdrawal":"0.01","withdrawal_fee":"0","deposit_enabled":true,"withdraw_enabled":true,"status":"active"}]}`

func fakeAPI(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/markets", func(w http.ResponseWriter, r *http.Request) {
		assert.NotEmpty(t, r.Header.Get("X-Request-Id"), "every request carries a correlation id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(marketsJSON))
	})
	mux.HandleFunc("GET /v1/markets/{symbol}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("symbol") == "ETH-USDC" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"symbol":"ETH-USDC","base_asset":"ETH","quote_asset":"USDC","price_tick":"0.01","qty_step":"0.0001","min_notional":"5","maker_bps":10,"taker_bps":20,"self_trade_policy":"cancel_newest","status":"active"}`))
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"about:blank","title":"Not Found","status":404,"detail":"market NOPE does not exist","instance":"/v1/markets/NOPE","correlation_id":"corr-xyz"}`))
	})
	mux.HandleFunc("GET /v1/assets", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(assetsJSON))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestMarketsList(t *testing.T) {
	srv := fakeAPI(t)
	out, err := run(t, "--base-url", srv.URL, "markets", "list")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 2)
	assert.True(t, strings.HasPrefix(lines[0], "SYMBOL"), lines[0])
	assert.Contains(t, lines[1], "ETH-USDC")
	assert.Contains(t, lines[1], "0.01")
	assert.Contains(t, lines[1], "10/20")

	out, err = run(t, "--base-url", srv.URL, "--output", "json", "markets", "list")
	require.NoError(t, err)
	assert.Contains(t, out, `"price_tick": "0.01"`, "amounts stay strings through the generated client")
}

func TestMarketsGet(t *testing.T) {
	srv := fakeAPI(t)
	out, err := run(t, "--base-url", srv.URL, "markets", "get", "ETH-USDC")
	require.NoError(t, err)
	assert.Contains(t, out, "ETH-USDC")

	_, err = run(t, "--base-url", srv.URL, "markets", "get", "NOPE")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Not Found: market NOPE does not exist")
	assert.Contains(t, err.Error(), "HTTP 404")
	assert.Contains(t, err.Error(), "correlation_id=corr-xyz")
}

func TestAssetsList(t *testing.T) {
	srv := fakeAPI(t)
	out, err := run(t, "--base-url", srv.URL, "assets", "list")
	require.NoError(t, err)
	assert.Contains(t, out, "ETH")
	assert.Contains(t, out, "31337")
	assert.Contains(t, out, "0.001")
}

func TestUnreachableAPIIsFriendly(t *testing.T) {
	_, err := run(t, "--base-url", "http://127.0.0.1:1", "markets", "list")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot reach the API at http://127.0.0.1:1")
	assert.Contains(t, err.Error(), "make up-single")
}

func TestBadOutputFlag(t *testing.T) {
	_, err := run(t, "--output", "yaml", "markets", "list")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown --output "yaml"`)
}
