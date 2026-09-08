package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/chain/deposit"
	"github.com/arc119226/crypto-exchange/internal/chain/hotwallet"
	"github.com/arc119226/crypto-exchange/internal/ledger"
)

// What the chain role does, seen from the back office: deposits it has seen,
// the hot wallet it pays from. Reads only; the chain role is the one process
// with a node and a key (docs/plan-v1.0.md §6.4).

// ListDeposits implements GET /admin/v1/deposits.
func (h *Handler) ListDeposits(ctx context.Context, req gen.ListDepositsRequestObject) (gen.ListDepositsResponseObject, error) {
	if h.deposits == nil {
		return nil, errors.New("admin: deposits are not enabled on this deployment")
	}
	status, asset := "", ""
	if req.Params.Status != nil {
		status = string(*req.Params.Status)
	}
	if req.Params.Asset != nil {
		asset = *req.Params.Asset
	}
	limit, offset := page(req.Params.Limit, req.Params.Offset)
	rows, err := h.deposits.List(ctx, status, asset, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list deposits: %w", err)
	}
	out := make([]gen.AdminDeposit, 0, len(rows))
	for _, d := range rows {
		out = append(out, toDeposit(d))
	}
	return gen.ListDeposits200JSONResponse(gen.AdminDepositList{Deposits: out}), nil
}

// GetHotWallet implements GET /admin/v1/hot-wallet.
func (h *Handler) GetHotWallet(ctx context.Context, _ gen.GetHotWalletRequestObject) (gen.GetHotWalletResponseObject, error) {
	hw, err := h.hotWallet(ctx)
	switch {
	case errors.Is(err, hotwallet.ErrNoHotWallet):
		return gen.GetHotWallet404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, "/admin/v1/hot-wallet",
				"no hot wallet has been recorded for this chain; the chain role writes it on its first start"),
		}, nil
	case err != nil:
		return nil, err
	}
	return gen.GetHotWallet200JSONResponse(hw), nil
}

// hotWallet joins the chain role's row with the ledger's custody_hot balances.
func (h *Handler) hotWallet(ctx context.Context) (gen.HotWallet, error) {
	d, err := hotwallet.Describe(ctx, h.pool, h.tenant, h.chainID)
	if err != nil {
		return gen.HotWallet{}, err
	}
	house, err := h.ledger.HouseBalances(ctx)
	if err != nil {
		return gen.HotWallet{}, fmt.Errorf("hot wallet balances: %w", err)
	}
	out := gen.HotWallet{
		ChainID: d.ChainID, Address: d.Address, NextNonce: d.NextNonce, Low: d.LowAlertedAt != nil,
		LowAlertedAt: d.LowAlertedAt, UpdatedAt: d.UpdatedAt, Balances: []gen.HotWalletBalance{},
	}
	for _, b := range house {
		if b.Code == ledger.HouseCustodyHot {
			out.Balances = append(out.Balances, gen.HotWalletBalance{Asset: b.Asset, Balance: b.Balance})
		}
	}
	return out, nil
}

func toDeposit(d deposit.Record) gen.AdminDeposit {
	return gen.AdminDeposit{
		ID: d.ID, AccountID: d.AccountID, Asset: d.Asset, Amount: d.Amount, Address: d.Address, TxHash: d.TxHash,
		LogIndex: d.LogIndex, BlockNumber: d.BlockNumber, Confirmations: d.Confirmations,
		Status: gen.AdminDepositStatus(d.Status), CreditedAt: d.CreditedAt, CreatedAt: d.CreatedAt,
	}
}
