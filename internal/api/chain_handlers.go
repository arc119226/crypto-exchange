package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/chain"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// GetDepositAddress implements GET /v1/deposit-address.
//
// The address is claimed from the pool the signer keeps stocked; this role
// holds no key material and cannot derive one (docs/plan-v1.0.md §6.4.1).
// The asset is only used to resolve the chain and to refuse an asset whose
// deposits are closed — one address serves every asset on that chain.
func (h *Handler) GetDepositAddress(ctx context.Context, req gen.GetDepositAddressRequestObject) (gen.GetDepositAddressResponseObject, error) {
	if h.d.Chain == nil {
		return gen.GetDepositAddress503ApplicationProblemPlusJSONResponse{
			ServiceUnavailableApplicationProblemPlusJSONResponse: h.unavailable(ctx, "deposits are not enabled on this deployment"),
		}, nil
	}
	p, err := requireScope(ctx, auth.ScopeRead)
	if err != nil {
		return gen.GetDepositAddress401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: h.unauthorized(ctx)}, nil
	}
	asset, err := h.d.Registry.GetAsset(ctx, h.d.Tenant, req.Params.Asset)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return gen.GetDepositAddress404ApplicationProblemPlusJSONResponse{
				NotFoundApplicationProblemPlusJSONResponse: h.notFound(ctx, "unknown asset "+req.Params.Asset),
			}, nil
		}
		return nil, fmt.Errorf("deposit address: asset: %w", err)
	}
	switch {
	case asset.Status != registry.AssetActive:
		return gen.GetDepositAddress422ApplicationProblemPlusJSONResponse{
			UnprocessableEntityApplicationProblemPlusJSONResponse: h.unprocessable(ctx, asset.Symbol+" is not active"),
		}, nil
	case !asset.DepositEnabled:
		return gen.GetDepositAddress422ApplicationProblemPlusJSONResponse{
			UnprocessableEntityApplicationProblemPlusJSONResponse: h.unprocessable(ctx, "deposits are closed for "+asset.Symbol),
		}, nil
	case asset.ChainID != h.d.Chain.ChainID():
		// The registry can hold assets of other chains before this deployment
		// serves them; handing out an address on the wrong chain would lose
		// the deposit.
		return gen.GetDepositAddress422ApplicationProblemPlusJSONResponse{
			UnprocessableEntityApplicationProblemPlusJSONResponse: h.unprocessable(ctx,
				fmt.Sprintf("%s lives on chain %d, this deployment serves chain %d", asset.Symbol, asset.ChainID, h.d.Chain.ChainID())),
		}, nil
	}

	// p.AccountID, never anything from the request: the caller can only ever
	// be given their own address.
	address, err := h.d.Chain.Assign(ctx, p.AccountID)
	if err != nil {
		if errors.Is(err, chain.ErrPoolEmpty) {
			return gen.GetDepositAddress503ApplicationProblemPlusJSONResponse{
				ServiceUnavailableApplicationProblemPlusJSONResponse: h.unavailable(ctx, "no deposit address available, retry shortly"),
			}, nil
		}
		return nil, fmt.Errorf("deposit address: %w", err)
	}
	return gen.GetDepositAddress200JSONResponse(gen.DepositAddress{
		Asset: asset.Symbol, ChainID: asset.ChainID, Address: address,
	}), nil
}

// ListDeposits implements GET /v1/deposits: the caller's own deposits only.
func (h *Handler) ListDeposits(ctx context.Context, req gen.ListDepositsRequestObject) (gen.ListDepositsResponseObject, error) {
	if h.d.Deposits == nil {
		return gen.ListDeposits503ApplicationProblemPlusJSONResponse{
			ServiceUnavailableApplicationProblemPlusJSONResponse: h.unavailable(ctx, "deposits are not enabled on this deployment"),
		}, nil
	}
	p, err := requireScope(ctx, auth.ScopeRead)
	if err != nil {
		return gen.ListDeposits401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: h.unauthorized(ctx)}, nil
	}
	limit, offset := page(req.Params.Limit, req.Params.Offset)
	rows, err := h.d.Deposits.ByAccount(ctx, p.AccountID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("deposits: %w", err)
	}
	required := h.requiredConfirmations(ctx)
	out := make([]gen.Deposit, 0, len(rows))
	for _, d := range rows {
		out = append(out, gen.Deposit{
			ID: d.ID, Asset: d.Asset, Amount: d.Amount, Address: d.Address,
			TxHash: d.TxHash, LogIndex: d.LogIndex, BlockNumber: d.BlockNumber,
			Confirmations: d.Confirmations, RequiredConfirmations: required[d.Asset],
			Status: d.Status, CreditedAt: d.CreditedAt, CreatedAt: d.CreatedAt,
		})
	}
	return gen.ListDeposits200JSONResponse(gen.DepositList{Deposits: out}), nil
}

// requiredConfirmations reads the registry once per request so a client can
// render progress without a second call. A registry error is not worth
// failing the listing over: the deposits themselves are the answer.
func (h *Handler) requiredConfirmations(ctx context.Context) map[string]int32 {
	out := map[string]int32{}
	assets, err := h.d.Registry.ListAssets(ctx, h.d.Tenant)
	if err != nil {
		return out
	}
	for _, a := range assets {
		out[a.Symbol] = a.RequiredConfirmations
	}
	return out
}
