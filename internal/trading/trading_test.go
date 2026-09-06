package trading

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

func amt(s string) money.Amount { return money.MustParse(s) }

var ethUSDC = registry.Market{
	ID: "m1", Symbol: "ETH-USDC", BaseSymbol: "ETH", QuoteSymbol: "USDC", BaseScale: 18, QuoteScale: 6,
	PriceTick: amt("0.01"), QtyStep: amt("0.0001"), MinNotional: amt("5"), MakerBps: 10, TakerBps: 20,
	SelfTradePolicy: registry.STPCancelNewest, Status: registry.MarketActive,
}

func TestPlaceOrderRequestValidate(t *testing.T) {
	limit := PlaceOrderRequest{AccountID: "a", MarketSymbol: "ETH-USDC", ClientOrderID: "c1", Side: matching.Buy, Type: matching.Limit, Price: amt("2000"), Qty: amt("1")}
	require.NoError(t, limit.Validate())
	assert.Equal(t, matching.GTC, limit.effectiveTIF())

	mbuy := PlaceOrderRequest{AccountID: "a", MarketSymbol: "ETH-USDC", ClientOrderID: "c2", Side: matching.Buy, Type: matching.Market, QuoteQty: amt("500")}
	require.NoError(t, mbuy.Validate())
	assert.Equal(t, matching.IOC, mbuy.effectiveTIF())

	msell := PlaceOrderRequest{AccountID: "a", MarketSymbol: "ETH-USDC", ClientOrderID: "c3", Side: matching.Sell, Type: matching.Market, Qty: amt("0.5")}
	require.NoError(t, msell.Validate())

	bad := map[string]PlaceOrderRequest{
		"no account":        {MarketSymbol: "ETH-USDC", ClientOrderID: "c", Side: matching.Buy, Type: matching.Limit, Price: amt("1"), Qty: amt("1")},
		"no client id":      {AccountID: "a", MarketSymbol: "ETH-USDC", Side: matching.Buy, Type: matching.Limit, Price: amt("1"), Qty: amt("1")},
		"limit no price":    {AccountID: "a", MarketSymbol: "ETH-USDC", ClientOrderID: "c", Side: matching.Buy, Type: matching.Limit, Qty: amt("1")},
		"limit with quote":  {AccountID: "a", MarketSymbol: "ETH-USDC", ClientOrderID: "c", Side: matching.Buy, Type: matching.Limit, Price: amt("1"), Qty: amt("1"), QuoteQty: amt("1")},
		"market buy w/ qty": {AccountID: "a", MarketSymbol: "ETH-USDC", ClientOrderID: "c", Side: matching.Buy, Type: matching.Market, Qty: amt("1")},
		"market sell quote": {AccountID: "a", MarketSymbol: "ETH-USDC", ClientOrderID: "c", Side: matching.Sell, Type: matching.Market, QuoteQty: amt("1")},
		"negative":          {AccountID: "a", MarketSymbol: "ETH-USDC", ClientOrderID: "c", Side: matching.Buy, Type: matching.Limit, Price: amt("-1"), Qty: amt("1")},
		"bad side":          {AccountID: "a", MarketSymbol: "ETH-USDC", ClientOrderID: "c", Type: matching.Limit, Price: amt("1"), Qty: amt("1")},
	}
	for name, r := range bad {
		assert.ErrorIs(t, r.Validate(), ErrInvalidRequest, name)
	}
}

func TestHoldFor(t *testing.T) {
	cases := []struct {
		name   string
		req    PlaceOrderRequest
		asset  string
		amount string
	}{
		{"limit buy", PlaceOrderRequest{Side: matching.Buy, Type: matching.Limit, Price: amt("2000"), Qty: amt("1")}, "USDC", "2000"},
		{"limit sell", PlaceOrderRequest{Side: matching.Sell, Type: matching.Limit, Price: amt("1990"), Qty: amt("0.4")}, "ETH", "0.4"},
		{"market buy", PlaceOrderRequest{Side: matching.Buy, Type: matching.Market, QuoteQty: amt("796")}, "USDC", "796"},
		{"market sell", PlaceOrderRequest{Side: matching.Sell, Type: matching.Market, Qty: amt("0.25")}, "ETH", "0.25"},
	}
	for _, c := range cases {
		asset, amount := holdFor(ethUSDC, c.req)
		assert.Equal(t, c.asset, asset, c.name)
		assert.True(t, amt(c.amount).Equal(amount), "%s: %s", c.name, amount)
	}
}

func TestSameAsDetectsReusedClientOrderID(t *testing.T) {
	price, qty := amt("2000"), amt("1")
	o := Order{MarketSymbol: "ETH-USDC", Side: matching.Buy, Type: matching.Limit, TimeInForce: matching.GTC, Price: &price, Qty: &qty}
	same := PlaceOrderRequest{MarketSymbol: "ETH-USDC", Side: matching.Buy, Type: matching.Limit, Price: amt("2000.00"), Qty: amt("1.0")}
	assert.True(t, same.sameAs(o), "amount equality ignores trailing zeros")
	for name, r := range map[string]PlaceOrderRequest{
		"price":  {MarketSymbol: "ETH-USDC", Side: matching.Buy, Type: matching.Limit, Price: amt("2001"), Qty: amt("1")},
		"side":   {MarketSymbol: "ETH-USDC", Side: matching.Sell, Type: matching.Limit, Price: amt("2000"), Qty: amt("1")},
		"market": {MarketSymbol: "BTC-USDC", Side: matching.Buy, Type: matching.Limit, Price: amt("2000"), Qty: amt("1")},
		"ioc":    {MarketSymbol: "ETH-USDC", Side: matching.Buy, Type: matching.Limit, TimeInForce: matching.IOC, Price: amt("2000"), Qty: amt("1")},
	} {
		assert.False(t, r.sameAs(o), name)
	}
}

func TestMarketConfigFromRegistry(t *testing.T) {
	cfg, err := marketConfig(ethUSDC)
	require.NoError(t, err)
	assert.Equal(t, matching.STPCancelNewest, cfg.SelfTradePolicy)
	assert.Equal(t, int32(6), cfg.QuoteScale)
	bad := ethUSDC
	bad.SelfTradePolicy = "cancel_oldest"
	_, err = marketConfig(bad)
	assert.ErrorIs(t, err, matching.ErrUnsupportedSTP)
}

func TestFillOf(t *testing.T) {
	tr := Trade{MakerOrderID: "m", TakerOrderID: "t", MakerAccountID: "A", TakerAccountID: "B", TakerSide: matching.Buy,
		MakerFee: amt("0.796"), MakerFeeAsset: "USDC", TakerFee: amt("0.0008"), TakerFeeAsset: "ETH"}
	f, ok := FillOf(tr, "A")
	require.True(t, ok)
	assert.Equal(t, "m", f.OrderID)
	assert.Equal(t, matching.Sell, f.Side)
	assert.True(t, f.IsMaker)
	assert.Equal(t, "USDC", f.FeeAsset)
	f, ok = FillOf(tr, "B")
	require.True(t, ok)
	assert.Equal(t, "t", f.OrderID)
	assert.Equal(t, matching.Buy, f.Side)
	assert.False(t, f.IsMaker)
	_, ok = FillOf(tr, "C")
	assert.False(t, ok)
}

func TestStatusTerminal(t *testing.T) {
	assert.False(t, StatusOpen.Terminal())
	assert.False(t, StatusPartiallyFilled.Terminal())
	assert.True(t, StatusFilled.Terminal())
	assert.True(t, StatusCancelled.Terminal())
	assert.True(t, StatusRejected.Terminal())
	assert.False(t, Status("nope").Valid())
}
