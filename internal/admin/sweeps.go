package admin

import (
	"context"
	"fmt"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/chain/sweep"
)

// ListSweeps implements GET /admin/v1/sweeps.
//
// Read-only, and there is nothing here for an operator to decide. Unlike a
// withdrawal, a sweep authorises nothing and pays nobody: it moves the
// exchange's own custody between two of its own house accounts, so the chain
// role runs the whole machine and this endpoint only shows what it did.
func (h *Handler) ListSweeps(ctx context.Context, req gen.ListSweepsRequestObject) (gen.ListSweepsResponseObject, error) {
	limit, _ := page(req.Params.Limit, nil)
	rows, err := sweep.List(ctx, h.pool, h.tenant, limit)
	if err != nil {
		return nil, fmt.Errorf("list sweeps: %w", err)
	}
	out := make([]gen.Sweep, 0, len(rows))
	for _, s := range rows {
		item := gen.Sweep{
			ID: s.ID, ChainID: s.ChainID, FromAddress: s.FromAddress, Asset: s.Asset,
			Amount: s.Amount, Status: s.Status,
			CreatedAt: s.CreatedAt, UpdatedAt: &s.UpdatedAt,
		}
		if s.FailureReason != "" {
			item.FailureReason = &s.FailureReason
		}
		if s.TxHash != "" {
			item.TxHash = &s.TxHash
		}
		if s.GasFundingTxHash != "" {
			item.GasFundingTxHash = &s.GasFundingTxHash
		}
		if s.GasCost.IsPositive() {
			item.GasCost = &s.GasCost
		}
		if s.BlockNumber > 0 {
			item.BlockNumber = &s.BlockNumber
		}
		out = append(out, item)
	}
	return gen.ListSweeps200JSONResponse(gen.SweepList{Sweeps: out}), nil
}
