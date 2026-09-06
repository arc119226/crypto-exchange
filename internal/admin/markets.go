package admin

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

func toMarket(m registry.Market) gen.Market {
	return gen.Market{
		ID: m.ID, Symbol: m.Symbol, BaseAsset: m.BaseSymbol, QuoteAsset: m.QuoteSymbol,
		PriceTick: m.PriceTick, QtyStep: m.QtyStep, MinNotional: m.MinNotional,
		MakerBps: m.MakerBps, TakerBps: m.TakerBps, SelfTradePolicy: m.SelfTradePolicy,
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
//
// The status change, its audit record and the market.updated event are one
// transaction: the engine can only learn about a change that is committed,
// and a change can never be committed without the event that carries it
// (docs/plan-v1.0.md §7.3, ADR-0002).
func (h *Handler) SetMarketStatus(ctx context.Context, req gen.SetMarketStatusRequestObject) (gen.SetMarketStatusResponseObject, error) {
	instance := "/admin/v1/markets/" + req.Symbol + "/status"
	if req.Body == nil || req.Body.Reason == "" {
		return gen.SetMarketStatus400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "status and reason are required")}, nil
	}
	status := string(req.Body.Status)
	if !registry.ValidMarketStatus(status) {
		return gen.SetMarketStatus400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "unknown market status "+status)}, nil
	}

	var market registry.Market
	err := h.inTx(ctx, func(tx pgx.Tx) error {
		before, err := h.registry.GetMarket(ctx, h.tenant, req.Symbol)
		if err != nil {
			return err
		}
		after, changed, err := h.registry.SetMarketStatus(ctx, tx, h.tenant, req.Symbol, status)
		if err != nil {
			return err
		}
		market = after
		if !changed {
			return nil // already in that status: no audit row, no event
		}
		if err := h.audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorAPIKey, ActorID: actorID, Action: "market.status.update",
			TargetType: "market", TargetID: after.Symbol,
			Before:        map[string]any{"status": before.Status, "version": before.Version},
			After:         map[string]any{"status": after.Status, "version": after.Version, "reason": req.Body.Reason},
			CorrelationID: telemetry.CorrelationID(ctx),
		}); err != nil {
			return err
		}
		evt, err := registry.MarketUpdatedEvent(h.tenant, after, []string{"status"}, req.Body.Reason, time.Now().UTC().Truncate(time.Microsecond))
		if err != nil {
			return err
		}
		evt.CorrelationID = telemetry.CorrelationID(ctx)
		_, err = eventbus.Outbox{}.Append(ctx, tx, evt)
		return err
	})
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
