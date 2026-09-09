package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/chain"
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
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
			Fee: d.Fee, CreditedAmount: d.Credited,
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

// CreateWithdrawal implements POST /v1/withdrawals.
//
// The api role only records the request (docs/plan-v1.0.md §5.1): nothing is
// held and nothing is signed here, and this role has no UPDATE on the table at
// all. The chain worker applies the withdrawal policy next.
func (h *Handler) CreateWithdrawal(ctx context.Context, req gen.CreateWithdrawalRequestObject) (gen.CreateWithdrawalResponseObject, error) {
	if h.d.Withdrawals == nil {
		return gen.CreateWithdrawal503ApplicationProblemPlusJSONResponse{
			ServiceUnavailableApplicationProblemPlusJSONResponse: h.unavailable(ctx, "withdrawals are not enabled on this deployment"),
		}, nil
	}
	p, err := requireScope(ctx, auth.ScopeWithdraw)
	if errors.Is(err, errUnauthenticated) {
		return gen.CreateWithdrawal401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: h.unauthorized(ctx)}, nil
	}
	if err != nil {
		// An API key without the withdraw scope is authenticated but not
		// allowed, which is the whole point of scoping keys separately.
		return gen.CreateWithdrawal403ApplicationProblemPlusJSONResponse{ForbiddenApplicationProblemPlusJSONResponse: h.forbidden(ctx, "the withdraw scope is required")}, nil
	}
	if req.Body == nil {
		return gen.CreateWithdrawal400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: h.badRequest(ctx, "a request body is required")}, nil
	}
	res, err := h.d.Withdrawals.Create(ctx, withdrawal.CreateParams{
		AccountID: p.AccountID, UserID: p.UserID, Asset: req.Body.Asset,
		Amount: req.Body.Amount, ToAddress: req.Body.ToAddress,
		IdempotencyKey: req.Params.IdempotencyKey,
		IP:             reqInfo(ctx).ip, CorrelationID: telemetry.CorrelationID(ctx),
	})
	switch {
	case errors.Is(err, withdrawal.ErrIdempotencyMismatch):
		// 422, not 409: the key is not in conflict with itself, the request
		// body is inconsistent with what that key already stands for. Same
		// answer client_order_id gives for the same mistake.
		return gen.CreateWithdrawal422ApplicationProblemPlusJSONResponse{
			UnprocessableEntityApplicationProblemPlusJSONResponse: h.unprocessable(ctx,
				"this Idempotency-Key was used for a different withdrawal"),
		}, nil
	case errors.Is(err, withdrawal.ErrInvalid):
		return gen.CreateWithdrawal422ApplicationProblemPlusJSONResponse{
			UnprocessableEntityApplicationProblemPlusJSONResponse: h.unprocessable(ctx, strings.TrimPrefix(err.Error(), "withdrawal: ")),
		}, nil
	case err != nil:
		return nil, fmt.Errorf("create withdrawal: %w", err)
	}
	body := toWithdrawal(res.Record)
	if res.Replayed {
		return gen.CreateWithdrawal200JSONResponse(body), nil
	}
	return gen.CreateWithdrawal201JSONResponse(body), nil
}

// ListWithdrawals implements GET /v1/withdrawals: the caller's own only.
func (h *Handler) ListWithdrawals(ctx context.Context, req gen.ListWithdrawalsRequestObject) (gen.ListWithdrawalsResponseObject, error) {
	if h.d.Withdrawals == nil {
		return gen.ListWithdrawals503ApplicationProblemPlusJSONResponse{
			ServiceUnavailableApplicationProblemPlusJSONResponse: h.unavailable(ctx, "withdrawals are not enabled on this deployment"),
		}, nil
	}
	p, err := requireScope(ctx, auth.ScopeRead)
	if err != nil {
		return gen.ListWithdrawals401ApplicationProblemPlusJSONResponse{UnauthorizedApplicationProblemPlusJSONResponse: h.unauthorized(ctx)}, nil
	}
	limit, _ := page(req.Params.Limit, nil)
	rows, err := h.d.Withdrawals.List(ctx, p.AccountID, limit)
	if err != nil {
		return nil, fmt.Errorf("withdrawals: %w", err)
	}
	out := make([]gen.Withdrawal, 0, len(rows))
	for _, w := range rows {
		out = append(out, toWithdrawal(w))
	}
	return gen.ListWithdrawals200JSONResponse(gen.WithdrawalList{Withdrawals: out}), nil
}

func toWithdrawal(w withdrawal.Record) gen.Withdrawal {
	out := gen.Withdrawal{
		ID: w.ID, Asset: w.Asset, Amount: w.Amount, Fee: w.Fee, FeeAsset: w.FeeAsset,
		ToAddress: w.ToAddress, ChainID: w.ChainID, Status: w.Status,
		CreatedAt: w.CreatedAt, UpdatedAt: &w.UpdatedAt,
	}
	if w.FailureReason != "" {
		out.FailureReason = &w.FailureReason
	}
	if w.ReviewNote != "" {
		out.ReviewNote = &w.ReviewNote
	}
	if w.TxHash != "" {
		out.TxHash = &w.TxHash
	}
	return out
}
