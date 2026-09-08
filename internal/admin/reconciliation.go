package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/chain/reconcile"
	"github.com/arc119226/crypto-exchange/internal/ledger"
)

// GetReconciliation implements GET /admin/v1/reconciliation.
//
// A read of the most recent pass, not a run of a new one. This role has no
// node, so it can no more measure an on-chain balance than it can sign a
// withdrawal; the chain role writes the reports and this shows them. §7.4
// lists a POST /reconciliation/run, which waits for the same intent-record
// pattern withdrawal resolve uses.
func (h *Handler) GetReconciliation(ctx context.Context, _ gen.GetReconciliationRequestObject) (gen.GetReconciliationResponseObject, error) {
	const instance = "/admin/v1/reconciliation"
	report, err := reconcile.Latest(ctx, h.pool, h.tenant, h.chainID)
	if errors.Is(err, reconcile.ErrNoReport) {
		return gen.GetReconciliation404ApplicationProblemPlusJSONResponse{
			NotFoundApplicationProblemPlusJSONResponse: notFound(ctx, instance,
				"no reconciliation pass has been recorded yet; the chain role writes one every ETH_RECONCILE_INTERVAL"),
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get reconciliation: %w", err)
	}
	lines := make([]gen.ReconciliationLine, 0, len(report.Lines))
	for _, l := range report.Lines {
		lines = append(lines, gen.ReconciliationLine{
			Asset: l.Asset, BlockHeight: l.BlockHeight,
			LedgerTotal: l.LedgerTotal, ChainTotal: l.ChainTotal,
			Uncredited: l.Uncredited, AboveFrontier: l.AboveFrontier,
			InFlight: l.InFlight, Diff: l.Diff, Balanced: !l.Broken(),
		})
	}
	return gen.GetReconciliation200JSONResponse(gen.ReconciliationReport{
		ID: report.ID, ChainID: report.ChainID,
		StartedAt: report.StartedAt, FinishedAt: report.FinishedAt,
		Balanced: report.Balanced, Lines: lines,
	}), nil
}

// CreateHouseAdjustment implements POST /admin/v1/ledger/house-adjustments.
//
// The way a break gets answered. Reconciliation reports what the ledger does
// not know about; this is how someone who has established where it came from
// tells the ledger. Deliberately not automatic: a system that closed its own
// breaks would be a system that never reports one.
func (h *Handler) CreateHouseAdjustment(ctx context.Context, req gen.CreateHouseAdjustmentRequestObject) (gen.CreateHouseAdjustmentResponseObject, error) {
	const instance = "/admin/v1/ledger/house-adjustments"
	if req.Body == nil {
		return gen.CreateHouseAdjustment400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "missing body")}, nil
	}
	b := req.Body
	if !b.Amount.IsPositive() {
		return gen.CreateHouseAdjustment400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "amount must be positive")}, nil
	}
	if b.Reason == "" {
		return gen.CreateHouseAdjustment400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, "reason is required")}, nil
	}
	key := b.IdempotencyKey
	if key == "" {
		key = "house_adjust:" + randomID()
	}
	entry, replayed, err := h.createHouseAdjustment(ctx, ledger.HouseAdjustParams{
		Code: ledger.HouseCode(b.Code), Asset: b.Asset, Amount: b.Amount,
		Direction: ledger.Direction(b.Direction), Reason: b.Reason, IdempotencyKey: key,
	})
	switch {
	case errors.Is(err, ledger.ErrReasonRequired), errors.Is(err, ledger.ErrInvalidEntry), errors.Is(err, ledger.ErrHouseAccount):
		return gen.CreateHouseAdjustment400ApplicationProblemPlusJSONResponse{BadRequestApplicationProblemPlusJSONResponse: badRequest(ctx, instance, err.Error())}, nil
	case err != nil:
		return nil, err
	}
	if replayed {
		return gen.CreateHouseAdjustment200JSONResponse(toEntry(entry)), nil
	}
	return gen.CreateHouseAdjustment201JSONResponse(toEntry(entry)), nil
}
