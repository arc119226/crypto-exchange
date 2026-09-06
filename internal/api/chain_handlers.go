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
