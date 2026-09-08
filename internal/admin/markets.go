package admin

import (
	"context"
	"errors"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

func toMarket(m registry.Market) gen.Market {
	return gen.Market{
		ID: m.ID, Symbol: m.Symbol, BaseAsset: m.BaseSymbol, QuoteAsset: m.QuoteSymbol,
		PriceTick: m.PriceTick, QtyStep: m.QtyStep, MinNotional: m.MinNotional,
		MakerBps: m.MakerBps, TakerBps: m.TakerBps, SelfTradePolicy: m.SelfTradePolicy,
		FeeSchedule: &m.FeeScheduleName, MaxQty: m.MaxQty, MaxSlippageBps: m.MaxSlippageBps,
		Status: gen.MarketStatus(m.Status), Version: m.Version,
	}
}

// ListMarkets implements GET /admin/v1/markets.
func (h *Handler) ListMarkets(ctx context.Context, _ gen.ListMarketsRequestObject) (gen.ListMarketsResponseObject, error) {
	markets, err := h.registry.ListMarkets(ctx, h.tenant)
	if err != nil {
		return nil, err
	}
	out := make([]gen.Market, 0, len(markets))
	for _, m := range markets {
		out = append(out, toMarket(m))
	}
	return gen.ListMarkets200JSONResponse(gen.MarketList{Markets: out}), nil
}

// SetMarketStatus implements PUT /admin/v1/markets/{symbol}/status.
func (h *Handler) SetMarketStatus(ctx context.Context, req gen.SetMarketStatusRequestObject) (gen.SetMarketStatusResponseObject, error) {
	instance := "/admin/v1/markets/" + req.Symbol + "/status"
	if req.Body == nil || req.Body.Reason == "" {
		return gen.SetMarketStatus400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "status and reason are required")}, nil
	}
	status := string(req.Body.Status)
	if !registry.ValidMarketStatus(status) {
		return gen.SetMarketStatus400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "unknown market status "+status)}, nil
	}
	market, err := h.setMarketStatus(ctx, req.Symbol, status, req.Body.Reason)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return gen.SetMarketStatus404ApplicationProblemPlusJSONResponse{NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, instance, "market "+req.Symbol+" does not exist")}, nil
	case errors.Is(err, registry.ErrInvalid):
		return gen.SetMarketStatus400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error())}, nil
	case err != nil:
		return nil, err
	}
	return gen.SetMarketStatus200JSONResponse(toMarket(market)), nil
}
