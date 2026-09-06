package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/api"
	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

type fakeReader struct {
	assets  []registry.Asset
	markets []registry.Market
	err     error
}

func (f *fakeReader) ListAssets(context.Context, string) ([]registry.Asset, error) {
	return f.assets, f.err
}
func (f *fakeReader) GetAsset(_ context.Context, _, symbol string) (registry.Asset, error) {
	for _, a := range f.assets {
		if a.Symbol == symbol {
			return a, f.err
		}
	}
	return registry.Asset{}, registry.ErrNotFound
}
func (f *fakeReader) ListMarkets(context.Context, string) ([]registry.Market, error) {
	return f.markets, f.err
}
func (f *fakeReader) GetMarket(_ context.Context, _, symbol string) (registry.Market, error) {
	if f.err != nil {
		return registry.Market{}, f.err
	}
	for _, m := range f.markets {
		if m.Symbol == symbol {
			return m, nil
		}
	}
	return registry.Market{}, registry.ErrNotFound
}

func usdcAddr() *string { s := "0x5FbDB2315678afecb367f032d93F642f64180aa3"; return &s }

func fixtures() *fakeReader {
	return &fakeReader{
		assets: []registry.Asset{
			{Symbol: "ETH", Name: "Ether", ChainID: 31337, IsNative: true, Scale: 18, DisplayScale: 6, RequiredConfirmations: 1,
				MinDeposit: money.MustParse("0.001"), MinWithdrawal: money.MustParse("0.01"), WithdrawalFee: money.Zero,
				DepositEnabled: true, WithdrawEnabled: true, Status: registry.AssetActive},
			{Symbol: "USDC", Name: "USD Coin (mock)", ChainID: 31337, ContractAddress: usdcAddr(), Scale: 6, DisplayScale: 2, RequiredConfirmations: 1,
				MinDeposit: money.MustParse("1"), MinWithdrawal: money.MustParse("5"), WithdrawalFee: money.Zero,
				DepositEnabled: true, WithdrawEnabled: true, Status: registry.AssetActive},
		},
		markets: []registry.Market{
			{Symbol: "ETH-USDC", BaseSymbol: "ETH", QuoteSymbol: "USDC", BaseScale: 18, QuoteScale: 6,
				PriceTick: money.MustParse("0.01"), QtyStep: money.MustParse("0.0001"), MinNotional: money.MustParse("5"),
				MakerBps: 10, TakerBps: 20, SelfTradePolicy: registry.STPCancelNewest, Status: registry.MarketActive},
			{Symbol: "OLD-USDC", BaseSymbol: "OLD", QuoteSymbol: "USDC", BaseScale: 18, QuoteScale: 6,
				PriceTick: money.MustParse("0.01"), QtyStep: money.MustParse("0.0001"), MinNotional: money.MustParse("5"),
				MakerBps: 10, TakerBps: 20, SelfTradePolicy: registry.STPCancelNewest, Status: registry.MarketDelisted},
		},
	}
}

func newServer(t *testing.T, reader registry.Reader) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Use(telemetry.CorrelationMiddleware(slog.New(slog.NewTextHandler(io.Discard, nil))))
	api.Mount(r, api.NewHandler(api.Deps{Registry: reader, Tenant: "default"}))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url, requestID string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	if requestID != "" {
		req.Header.Set(telemetry.RequestIDHeader, requestID)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, body
}

func TestListMarkets(t *testing.T) {
	srv := newServer(t, fixtures())
	resp, body := get(t, srv.URL+"/v1/markets", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assert.NotEmpty(t, resp.Header.Get(telemetry.RequestIDHeader), "a correlation id is generated when none is sent")

	// The wire format is the contract: amounts are strings, optional limits are absent.
	assert.Contains(t, string(body), `"price_tick":"0.01"`)
	assert.Contains(t, string(body), `"qty_step":"0.0001"`)
	assert.Contains(t, string(body), `"min_notional":"5"`)
	assert.NotContains(t, string(body), `"max_qty"`)
	assert.NotContains(t, string(body), `"max_slippage_bps"`)
	assert.NotContains(t, string(body), "OLD-USDC", "delisted markets are hidden")

	var list gen.MarketList
	require.NoError(t, json.Unmarshal(body, &list))
	require.Len(t, list.Markets, 1)
	m := list.Markets[0]
	assert.Equal(t, "ETH-USDC", m.Symbol)
	assert.Equal(t, "ETH", m.BaseAsset)
	assert.Equal(t, "USDC", m.QuoteAsset)
	assert.True(t, m.PriceTick.Equal(money.MustParse("0.01")))
	assert.Equal(t, int32(10), m.MakerBps)
	assert.Equal(t, int32(20), m.TakerBps)
	assert.Equal(t, gen.SelfTradePolicyCancelNewest, m.SelfTradePolicy)
	assert.Equal(t, gen.MarketStatusActive, m.Status)
}

func TestGetMarket(t *testing.T) {
	srv := newServer(t, fixtures())

	resp, body := get(t, srv.URL+"/v1/markets/ETH-USDC", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var m gen.Market
	require.NoError(t, json.Unmarshal(body, &m))
	assert.Equal(t, "ETH-USDC", m.Symbol)

	for _, symbol := range []string{"NOPE-USDC", "OLD-USDC"} {
		resp, body = get(t, srv.URL+"/v1/markets/"+symbol, "corr-"+symbol)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, symbol)
		assert.Equal(t, api.ProblemContentType, resp.Header.Get("Content-Type"))
		var p gen.Problem
		require.NoError(t, json.Unmarshal(body, &p))
		assert.Equal(t, http.StatusNotFound, p.Status)
		assert.Equal(t, "Not Found", p.Title)
		assert.Equal(t, "about:blank", p.Type)
		assert.Equal(t, "/v1/markets/"+symbol, p.Instance)
		assert.Equal(t, "corr-"+symbol, p.CorrelationID, "problem echoes the caller's X-Request-Id")
		assert.Equal(t, "corr-"+symbol, resp.Header.Get(telemetry.RequestIDHeader))
	}
}

func TestListAssets(t *testing.T) {
	srv := newServer(t, fixtures())
	resp, body := get(t, srv.URL+"/v1/assets", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), `"contract_address":null`, "native coin has an explicit null address")
	assert.Contains(t, string(body), `"min_deposit":"0.001"`)

	var list gen.AssetList
	require.NoError(t, json.Unmarshal(body, &list))
	require.Len(t, list.Assets, 2)
	assert.Equal(t, "ETH", list.Assets[0].Symbol)
	assert.Nil(t, list.Assets[0].ContractAddress)
	assert.Equal(t, int64(31337), list.Assets[0].ChainID)
	assert.Equal(t, int32(18), list.Assets[0].Scale)
	require.NotNil(t, list.Assets[1].ContractAddress)
	assert.Equal(t, *usdcAddr(), *list.Assets[1].ContractAddress)
	assert.Equal(t, gen.AssetStatusActive, list.Assets[1].Status)
}

func TestReaderFailureIsAProblem(t *testing.T) {
	srv := newServer(t, &fakeReader{err: errors.New("connection reset")})
	for _, path := range []string{"/v1/assets", "/v1/markets", "/v1/markets/ETH-USDC"} {
		resp, body := get(t, srv.URL+path, "corr-500")
		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, path)
		assert.Equal(t, api.ProblemContentType, resp.Header.Get("Content-Type"), path)
		var p gen.Problem
		require.NoError(t, json.Unmarshal(body, &p), path)
		assert.Equal(t, "corr-500", p.CorrelationID)
		assert.Equal(t, path, p.Instance)
		assert.Empty(t, p.Detail, "internal errors must not leak to clients")
	}
}

func TestAmountWireFormatRejectsNumbers(t *testing.T) {
	var m gen.Market
	err := json.Unmarshal([]byte(`{"symbol":"ETH-USDC","price_tick":0.01}`), &m)
	require.Error(t, err, "money.Amount must reject JSON numbers so precision is never lost")
	require.NoError(t, json.Unmarshal([]byte(`{"symbol":"ETH-USDC","price_tick":"0.01"}`), &m))
	assert.Equal(t, "0.01", m.PriceTick.String())
}
