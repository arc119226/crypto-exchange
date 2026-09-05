package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// Handler implements gen.StrictServerInterface for one tenant.
type Handler struct {
	reg    registry.Reader
	tenant string
}

var _ gen.StrictServerInterface = (*Handler)(nil)

// NewHandler builds the public API handler over a registry reader.
func NewHandler(reg registry.Reader, tenantID string) *Handler {
	return &Handler{reg: reg, tenant: tenantID}
}

// ListAssets implements GET /v1/assets.
func (h *Handler) ListAssets(ctx context.Context, _ gen.ListAssetsRequestObject) (gen.ListAssetsResponseObject, error) {
	assets, err := h.reg.ListAssets(ctx, h.tenant)
	if err != nil {
		return nil, fmt.Errorf("list assets: %w", err)
	}
	out := make([]gen.Asset, 0, len(assets))
	for _, a := range assets {
		out = append(out, toAsset(a))
	}
	return gen.ListAssets200JSONResponse(gen.AssetList{Assets: out}), nil
}

// ListMarkets implements GET /v1/markets.
func (h *Handler) ListMarkets(ctx context.Context, _ gen.ListMarketsRequestObject) (gen.ListMarketsResponseObject, error) {
	markets, err := h.reg.ListMarkets(ctx, h.tenant)
	if err != nil {
		return nil, fmt.Errorf("list markets: %w", err)
	}
	out := make([]gen.Market, 0, len(markets))
	for _, m := range markets {
		if m.Status == registry.MarketDelisted {
			continue
		}
		out = append(out, toMarket(m))
	}
	return gen.ListMarkets200JSONResponse(gen.MarketList{Markets: out}), nil
}

// GetMarket implements GET /v1/markets/{symbol}.
func (h *Handler) GetMarket(ctx context.Context, req gen.GetMarketRequestObject) (gen.GetMarketResponseObject, error) {
	m, err := h.reg.GetMarket(ctx, h.tenant, req.Symbol)
	if errors.Is(err, registry.ErrNotFound) || (err == nil && m.Status == registry.MarketDelisted) {
		return gen.GetMarket404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: gen.NotFoundApplicationProblemPlusJSONResponse(
				NewProblem(ctx, http.StatusNotFound, "Not Found", "market "+req.Symbol+" does not exist", "/v1/markets/"+req.Symbol),
			),
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get market %s: %w", req.Symbol, err)
	}
	return gen.GetMarket200JSONResponse(toMarket(m)), nil
}

func toAsset(a registry.Asset) gen.Asset {
	return gen.Asset{
		Symbol:                a.Symbol,
		Name:                  a.Name,
		ChainID:               a.ChainID,
		ContractAddress:       a.ContractAddress,
		IsNative:              a.IsNative,
		Scale:                 a.Scale,
		DisplayScale:          a.DisplayScale,
		RequiredConfirmations: a.RequiredConfirmations,
		MinDeposit:            a.MinDeposit,
		MinWithdrawal:         a.MinWithdrawal,
		WithdrawalFee:         a.WithdrawalFee,
		DepositEnabled:        a.DepositEnabled,
		WithdrawEnabled:       a.WithdrawEnabled,
		Status:                gen.AssetStatus(a.Status),
	}
}

func toMarket(m registry.Market) gen.Market {
	return gen.Market{
		Symbol:          m.Symbol,
		BaseAsset:       m.BaseSymbol,
		QuoteAsset:      m.QuoteSymbol,
		PriceTick:       m.PriceTick,
		QtyStep:         m.QtyStep,
		MinNotional:     m.MinNotional,
		MaxQty:          m.MaxQty,
		MaxSlippageBps:  m.MaxSlippageBps,
		MakerBps:        m.MakerBps,
		TakerBps:        m.TakerBps,
		SelfTradePolicy: gen.SelfTradePolicy(m.SelfTradePolicy),
		Status:          gen.MarketStatus(m.Status),
	}
}
