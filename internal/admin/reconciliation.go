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
	return gen.GetReconciliation200JSONResponse(toReport(report)), nil
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

// ListReconciliationReports implements GET /admin/v1/reconciliation/reports.
func (h *Handler) ListReconciliationReports(ctx context.Context, req gen.ListReconciliationReportsRequestObject) (gen.ListReconciliationReportsResponseObject, error) {
	limit, offset := page(req.Params.Limit, req.Params.Offset)
	reports, err := reconcile.List(ctx, h.pool, h.tenant, h.chainID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list reconciliation reports: %w", err)
	}
	out := make([]gen.ReconciliationReport, 0, len(reports))
	for _, r := range reports {
		out = append(out, toReport(r))
	}
	return gen.ListReconciliationReports200JSONResponse(gen.ReconciliationReportList{Reports: out}), nil
}

// ListReconciliationBreaks implements GET /admin/v1/reconciliation/breaks:
// both families, each newest first, under the same page bounds.
func (h *Handler) ListReconciliationBreaks(ctx context.Context, req gen.ListReconciliationBreaksRequestObject) (gen.ListReconciliationBreaksResponseObject, error) {
	limit, offset := page(req.Params.Limit, req.Params.Offset)
	chain, err := reconcile.Breaks(ctx, h.pool, h.tenant, h.chainID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list chain breaks: %w", err)
	}
	ledgerBreaks, err := h.ledger.Breaks(ctx, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list ledger breaks: %w", err)
	}
	out := gen.ReconciliationBreaks{ChainBreaks: make([]gen.ChainBreak, 0, len(chain)), LedgerBreaks: make([]gen.LedgerBreak, 0, len(ledgerBreaks))}
	for _, b := range chain {
		out.ChainBreaks = append(out.ChainBreaks, toChainBreak(b))
	}
	for _, b := range ledgerBreaks {
		out.LedgerBreaks = append(out.LedgerBreaks, toLedgerBreak(b))
	}
	return gen.ListReconciliationBreaks200JSONResponse(out), nil
}

func toReport(report reconcile.Report) gen.ReconciliationReport {
	lines := make([]gen.ReconciliationLine, 0, len(report.Lines))
	for _, l := range report.Lines {
		lines = append(lines, toLine(l))
	}
	return gen.ReconciliationReport{
		ID: report.ID, ChainID: report.ChainID, StartedAt: report.StartedAt, FinishedAt: report.FinishedAt,
		Balanced: report.Balanced, Lines: lines,
	}
}

func toLine(l reconcile.Line) gen.ReconciliationLine {
	return gen.ReconciliationLine{
		Asset: l.Asset, BlockHeight: l.BlockHeight, LedgerTotal: l.LedgerTotal, ChainTotal: l.ChainTotal,
		Uncredited: l.Uncredited, AboveFrontier: l.AboveFrontier, InFlight: l.InFlight, Diff: l.Diff, Balanced: !l.Broken(),
	}
}

func toChainBreak(b reconcile.Break) gen.ChainBreak {
	return gen.ChainBreak{
		ID: b.ID, ReportID: b.ReportID, ChainID: b.ChainID, Asset: b.Line.Asset, BlockHeight: b.Line.BlockHeight,
		LedgerTotal: b.Line.LedgerTotal, ChainTotal: b.Line.ChainTotal, Uncredited: b.Line.Uncredited,
		AboveFrontier: b.Line.AboveFrontier, InFlight: b.Line.InFlight, Diff: b.Line.Diff, DetectedAt: b.DetectedAt,
	}
}

func toLedgerBreak(b ledger.Break) gen.LedgerBreak {
	return gen.LedgerBreak{ID: b.ID, Asset: b.Asset, Debits: b.Debits, Credits: b.Credits, Diff: b.Diff, DetectedAt: b.DetectedAt, ResolvedAt: b.ResolvedAt}
}
