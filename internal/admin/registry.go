package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// The rest of the registry (docs/plan-v1.0.md §6.5, §12): assets, fee
// schedules and withdrawal limits, and creating or editing a market. Every
// write goes through writes.go, where the engine's reload event and the
// audit row are part of the same transaction as the change.

// ListAssets implements GET /admin/v1/assets.
func (h *Handler) ListAssets(ctx context.Context, _ gen.ListAssetsRequestObject) (gen.ListAssetsResponseObject, error) {
	assets, err := h.registry.ListAssets(ctx, h.tenant)
	if err != nil {
		return nil, err
	}
	out := make([]gen.Asset, 0, len(assets))
	for _, a := range assets {
		out = append(out, toAsset(a))
	}
	return gen.ListAssets200JSONResponse(gen.AssetList{Assets: out}), nil
}

// GetAsset implements GET /admin/v1/assets/{symbol}.
func (h *Handler) GetAsset(ctx context.Context, req gen.GetAssetRequestObject) (gen.GetAssetResponseObject, error) {
	a, err := h.registry.GetAsset(ctx, h.tenant, req.Symbol)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return gen.GetAsset404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, "/admin/v1/assets/"+req.Symbol, "asset "+req.Symbol+" does not exist"),
		}, nil
	case err != nil:
		return nil, err
	}
	return gen.GetAsset200JSONResponse(toAsset(a)), nil
}

// CreateAsset implements POST /admin/v1/assets.
func (h *Handler) CreateAsset(ctx context.Context, req gen.CreateAssetRequestObject) (gen.CreateAssetResponseObject, error) {
	const instance = "/admin/v1/assets"
	if req.Body == nil || req.Body.Reason == "" {
		return gen.CreateAsset400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "the asset and a reason are required"),
		}, nil
	}
	b := req.Body
	in := assetInput(b.Symbol, gen.AssetRequest{
		Name: b.Name, ChainID: b.ChainID, ContractAddress: b.ContractAddress, IsNative: b.IsNative,
		Scale: b.Scale, DisplayScale: b.DisplayScale, RequiredConfirmations: b.RequiredConfirmations,
		MinDeposit: b.MinDeposit, MinWithdrawal: b.MinWithdrawal, WithdrawalFee: b.WithdrawalFee, SweepThreshold: b.SweepThreshold,
		DepositEnabled: b.DepositEnabled, WithdrawEnabled: b.WithdrawEnabled, Status: b.Status,
	})
	a, err := h.upsertAsset(ctx, in, true, b.Reason)
	switch {
	case errors.Is(err, errAlreadyExists):
		return gen.CreateAsset409ApplicationProblemPlusJSONResponse{
			ConflictApplicationProblemPlusJSONResponse: conflict(ctx, instance, "asset "+b.Symbol+" already exists; PUT /admin/v1/assets/"+b.Symbol+" edits it"),
		}, nil
	case errors.Is(err, registry.ErrInvalid):
		return gen.CreateAsset400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error()),
		}, nil
	case err != nil:
		return nil, fmt.Errorf("create asset %s: %w", b.Symbol, err)
	}
	return gen.CreateAsset201JSONResponse(toAsset(a)), nil
}

// UpdateAsset implements PUT /admin/v1/assets/{symbol}.
func (h *Handler) UpdateAsset(ctx context.Context, req gen.UpdateAssetRequestObject) (gen.UpdateAssetResponseObject, error) {
	instance := "/admin/v1/assets/" + req.Symbol
	if req.Body == nil || req.Body.Reason == "" {
		return gen.UpdateAsset400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "every field and a reason are required"),
		}, nil
	}
	a, err := h.upsertAsset(ctx, assetInput(req.Symbol, *req.Body), false, req.Body.Reason)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return gen.UpdateAsset404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, instance, "asset "+req.Symbol+" does not exist"),
		}, nil
	case errors.Is(err, registry.ErrInvalid):
		return gen.UpdateAsset400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error()),
		}, nil
	case err != nil:
		return nil, fmt.Errorf("update asset %s: %w", req.Symbol, err)
	}
	return gen.UpdateAsset200JSONResponse(toAsset(a)), nil
}

// GetMarket implements GET /admin/v1/markets/{symbol}.
func (h *Handler) GetMarket(ctx context.Context, req gen.GetMarketRequestObject) (gen.GetMarketResponseObject, error) {
	m, err := h.registry.GetMarket(ctx, h.tenant, req.Symbol)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return gen.GetMarket404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, "/admin/v1/markets/"+req.Symbol, "market "+req.Symbol+" does not exist"),
		}, nil
	case err != nil:
		return nil, err
	}
	return gen.GetMarket200JSONResponse(toMarket(m)), nil
}

// CreateMarket implements POST /admin/v1/markets.
func (h *Handler) CreateMarket(ctx context.Context, req gen.CreateMarketRequestObject) (gen.CreateMarketResponseObject, error) {
	const instance = "/admin/v1/markets"
	if req.Body == nil || req.Body.Reason == "" {
		return gen.CreateMarket400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "the market and a reason are required"),
		}, nil
	}
	b := req.Body
	in := marketInput(b.Symbol, gen.MarketRequest{
		BaseAsset: b.BaseAsset, QuoteAsset: b.QuoteAsset, PriceTick: b.PriceTick, QtyStep: b.QtyStep, MinNotional: b.MinNotional,
		MaxQty: b.MaxQty, MaxSlippageBps: b.MaxSlippageBps, FeeSchedule: b.FeeSchedule, SelfTradePolicy: b.SelfTradePolicy, Status: b.Status,
	})
	m, err := h.upsertMarket(ctx, in, true, b.Reason)
	switch {
	case errors.Is(err, errAlreadyExists):
		return gen.CreateMarket409ApplicationProblemPlusJSONResponse{
			ConflictApplicationProblemPlusJSONResponse: conflict(ctx, instance, "market "+b.Symbol+" already exists; PUT /admin/v1/markets/"+b.Symbol+" edits it"),
		}, nil
	case errors.Is(err, registry.ErrNotFound):
		// a referenced asset or fee schedule
		return gen.CreateMarket404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, instance, err.Error()),
		}, nil
	case errors.Is(err, registry.ErrInvalid):
		return gen.CreateMarket400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error()),
		}, nil
	case err != nil:
		return nil, fmt.Errorf("create market %s: %w", b.Symbol, err)
	}
	return gen.CreateMarket201JSONResponse(toMarket(m)), nil
}

// UpdateMarket implements PUT /admin/v1/markets/{symbol}.
func (h *Handler) UpdateMarket(ctx context.Context, req gen.UpdateMarketRequestObject) (gen.UpdateMarketResponseObject, error) {
	instance := "/admin/v1/markets/" + req.Symbol
	if req.Body == nil || req.Body.Reason == "" {
		return gen.UpdateMarket400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "every field and a reason are required"),
		}, nil
	}
	m, err := h.upsertMarket(ctx, marketInput(req.Symbol, *req.Body), false, req.Body.Reason)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return gen.UpdateMarket404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, instance, err.Error()),
		}, nil
	case errors.Is(err, registry.ErrInvalid):
		return gen.UpdateMarket400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error()),
		}, nil
	case err != nil:
		return nil, fmt.Errorf("update market %s: %w", req.Symbol, err)
	}
	return gen.UpdateMarket200JSONResponse(toMarket(m)), nil
}

// ListFeeSchedules implements GET /admin/v1/fee-schedules.
func (h *Handler) ListFeeSchedules(ctx context.Context, _ gen.ListFeeSchedulesRequestObject) (gen.ListFeeSchedulesResponseObject, error) {
	rows, err := h.registry.ListFeeSchedules(ctx, h.tenant)
	if err != nil {
		return nil, err
	}
	out := make([]gen.FeeSchedule, 0, len(rows))
	for _, f := range rows {
		out = append(out, toFeeSchedule(f))
	}
	return gen.ListFeeSchedules200JSONResponse(gen.FeeScheduleList{FeeSchedules: out}), nil
}

// CreateFeeSchedule implements POST /admin/v1/fee-schedules.
func (h *Handler) CreateFeeSchedule(ctx context.Context, req gen.CreateFeeScheduleRequestObject) (gen.CreateFeeScheduleResponseObject, error) {
	const instance = "/admin/v1/fee-schedules"
	if req.Body == nil || req.Body.Reason == "" {
		return gen.CreateFeeSchedule400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "name, maker_bps, taker_bps and reason are required"),
		}, nil
	}
	b := req.Body
	f, err := h.upsertFeeSchedule(ctx, registry.FeeScheduleInput{Name: b.Name, MakerBps: b.MakerBps, TakerBps: b.TakerBps}, true, b.Reason)
	switch {
	case errors.Is(err, errAlreadyExists):
		return gen.CreateFeeSchedule409ApplicationProblemPlusJSONResponse{
			ConflictApplicationProblemPlusJSONResponse: conflict(ctx, instance, "fee schedule "+b.Name+" already exists; PUT /admin/v1/fee-schedules/"+b.Name+" edits it"),
		}, nil
	case errors.Is(err, registry.ErrInvalid):
		return gen.CreateFeeSchedule400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error()),
		}, nil
	case err != nil:
		return nil, fmt.Errorf("create fee schedule %s: %w", b.Name, err)
	}
	return gen.CreateFeeSchedule201JSONResponse(toFeeSchedule(f)), nil
}

// UpdateFeeSchedule implements PUT /admin/v1/fee-schedules/{name}.
func (h *Handler) UpdateFeeSchedule(ctx context.Context, req gen.UpdateFeeScheduleRequestObject) (gen.UpdateFeeScheduleResponseObject, error) {
	instance := "/admin/v1/fee-schedules/" + req.Name
	if req.Body == nil || req.Body.Reason == "" {
		return gen.UpdateFeeSchedule400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "maker_bps, taker_bps and reason are required"),
		}, nil
	}
	f, err := h.upsertFeeSchedule(ctx, registry.FeeScheduleInput{Name: req.Name, MakerBps: req.Body.MakerBps, TakerBps: req.Body.TakerBps}, false, req.Body.Reason)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return gen.UpdateFeeSchedule404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, instance, "fee schedule "+req.Name+" does not exist"),
		}, nil
	case errors.Is(err, registry.ErrInvalid):
		return gen.UpdateFeeSchedule400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error()),
		}, nil
	case err != nil:
		return nil, fmt.Errorf("update fee schedule %s: %w", req.Name, err)
	}
	return gen.UpdateFeeSchedule200JSONResponse(toFeeSchedule(f)), nil
}

// ListWithdrawalLimits implements GET /admin/v1/withdrawal-limits.
func (h *Handler) ListWithdrawalLimits(ctx context.Context, _ gen.ListWithdrawalLimitsRequestObject) (gen.ListWithdrawalLimitsResponseObject, error) {
	rows, err := h.registry.ListWithdrawalLimits(ctx, h.tenant)
	if err != nil {
		return nil, err
	}
	out := make([]gen.WithdrawalLimit, 0, len(rows))
	for _, l := range rows {
		out = append(out, toWithdrawalLimit(l))
	}
	return gen.ListWithdrawalLimits200JSONResponse(gen.WithdrawalLimitList{WithdrawalLimits: out}), nil
}

// SetWithdrawalLimit implements PUT /admin/v1/withdrawal-limits/{asset}/{kyc_level}.
func (h *Handler) SetWithdrawalLimit(ctx context.Context, req gen.SetWithdrawalLimitRequestObject) (gen.SetWithdrawalLimitResponseObject, error) {
	instance := fmt.Sprintf("/admin/v1/withdrawal-limits/%s/%d", req.Asset, req.KycLevel)
	if req.Body == nil || req.Body.Reason == "" {
		return gen.SetWithdrawalLimit400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "both limits, require_manual_review and a reason are required"),
		}, nil
	}
	if req.KycLevel < 0 || req.KycLevel > 2 {
		return gen.SetWithdrawalLimit400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "kyc_level must be between 0 and 2"),
		}, nil
	}
	in := registry.WithdrawalLimitInput{
		Asset: string(req.Asset), KYCLevel: int16(req.KycLevel), //nolint:gosec // bounded just above
		AutoApproveLimit: req.Body.AutoApproveLimit, DailyLimit: req.Body.DailyLimit, RequireManualReview: req.Body.RequireManualReview,
	}
	l, err := h.upsertWithdrawalLimit(ctx, in, req.Body.Reason)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return gen.SetWithdrawalLimit404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, instance, "asset "+string(req.Asset)+" does not exist"),
		}, nil
	case errors.Is(err, registry.ErrInvalid):
		return gen.SetWithdrawalLimit400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error()),
		}, nil
	case err != nil:
		return nil, fmt.Errorf("set withdrawal limit %s/%d: %w", req.Asset, req.KycLevel, err)
	}
	return gen.SetWithdrawalLimit200JSONResponse(toWithdrawalLimit(l)), nil
}

// RequestReload implements POST /admin/v1/engine/reload.
func (h *Handler) RequestReload(ctx context.Context, req gen.RequestReloadRequestObject) (gen.RequestReloadResponseObject, error) {
	if req.Body == nil || req.Body.Reason == "" {
		return gen.RequestReload400ApplicationProblemPlusJSONResponse{
			BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, "/admin/v1/engine/reload", "a reason is required"),
		}, nil
	}
	evt, err := h.requestReload(ctx, req.Body.Reason)
	if err != nil {
		return nil, fmt.Errorf("request reload: %w", err)
	}
	return gen.RequestReload202JSONResponse(gen.ReloadAccepted{EventID: evt.EventID, OccurredAt: evt.OccurredAt}), nil
}

func assetInput(symbol string, b gen.AssetRequest) registry.AssetInput {
	return registry.AssetInput{
		Symbol: symbol, Name: b.Name, ChainID: b.ChainID, ContractAddress: b.ContractAddress, IsNative: b.IsNative,
		Scale: b.Scale, DisplayScale: b.DisplayScale, RequiredConfirmations: b.RequiredConfirmations,
		MinDeposit: b.MinDeposit, MinWithdrawal: b.MinWithdrawal, WithdrawalFee: b.WithdrawalFee, SweepThreshold: b.SweepThreshold,
		DepositEnabled: b.DepositEnabled, WithdrawEnabled: b.WithdrawEnabled, Status: string(b.Status),
	}
}

func marketInput(symbol string, b gen.MarketRequest) registry.MarketInput {
	return registry.MarketInput{
		Symbol: symbol, BaseSymbol: b.BaseAsset, QuoteSymbol: b.QuoteAsset,
		PriceTick: b.PriceTick, QtyStep: b.QtyStep, MinNotional: b.MinNotional, MaxQty: b.MaxQty, MaxSlippageBps: b.MaxSlippageBps,
		FeeSchedule: b.FeeSchedule, SelfTradePolicy: b.SelfTradePolicy, Status: string(b.Status),
	}
}

func toAsset(a registry.Asset) gen.Asset {
	return gen.Asset{
		ID: a.ID, Symbol: a.Symbol, Name: a.Name, ChainID: a.ChainID, ContractAddress: a.ContractAddress, IsNative: a.IsNative,
		Scale: a.Scale, DisplayScale: a.DisplayScale, RequiredConfirmations: a.RequiredConfirmations,
		MinDeposit: a.MinDeposit, MinWithdrawal: a.MinWithdrawal, WithdrawalFee: a.WithdrawalFee, SweepThreshold: a.SweepThreshold,
		DepositEnabled: a.DepositEnabled, WithdrawEnabled: a.WithdrawEnabled, Status: gen.AssetStatus(a.Status),
		Version: a.Version, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
}

func toFeeSchedule(f registry.FeeSchedule) gen.FeeSchedule {
	return gen.FeeSchedule{
		ID: f.ID, Name: f.Name, MakerBps: f.MakerBps, TakerBps: f.TakerBps, EffectiveFrom: f.EffectiveFrom,
		Version: f.Version, CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt,
	}
}

func toWithdrawalLimit(l registry.WithdrawalLimit) gen.WithdrawalLimit {
	return gen.WithdrawalLimit{
		Asset: l.Asset, KycLevel: int(l.KYCLevel), AutoApproveLimit: l.AutoApproveLimit, DailyLimit: l.DailyLimit,
		RequireManualReview: l.RequireManualReview, Version: l.Version, UpdatedAt: l.UpdatedAt,
	}
}
