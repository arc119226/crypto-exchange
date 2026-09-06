package trading

import (
	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// holdFor returns what a new order freezes at acceptance (docs/domain.md
// §2): limit buy = price × qty of quote; limit sell = qty of base; market
// buy = quote budget; market sell = qty of base.
func holdFor(m registry.Market, r PlaceOrderRequest) (asset string, amount money.Amount) {
	if r.Side == matching.Buy {
		if r.Type == matching.Market {
			return m.QuoteSymbol, r.QuoteQty
		}
		return m.QuoteSymbol, r.Price.Mul(r.Qty)
	}
	return m.BaseSymbol, r.Qty
}

// marketConfig converts a registry market into the book's configuration.
func marketConfig(m registry.Market) (matching.MarketConfig, error) {
	var stp matching.SelfTradePolicy
	if err := stp.UnmarshalText([]byte(m.SelfTradePolicy)); err != nil {
		return matching.MarketConfig{}, err
	}
	cfg := matching.MarketConfig{
		Symbol: m.Symbol, PriceTick: m.PriceTick, QtyStep: m.QtyStep, MinNotional: m.MinNotional,
		MaxQty: m.MaxQty, MaxSlippageBps: m.MaxSlippageBps, BaseScale: m.BaseScale, QuoteScale: m.QuoteScale,
		SelfTradePolicy: stp,
	}
	return cfg, cfg.Validate()
}

// newOrderCommand builds the matching payload of a request.
func newOrderCommand(orderID string, r PlaceOrderRequest) *matching.NewOrder {
	return &matching.NewOrder{
		OrderID: matching.OrderID(orderID), AccountID: matching.AccountID(r.AccountID),
		Side: r.Side, Type: r.Type, TimeInForce: r.effectiveTIF(),
		Price: r.Price, Qty: r.Qty, QuoteQty: r.QuoteQty,
	}
}
