package admin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/chain/deposit"
	"github.com/arc119226/crypto-exchange/internal/chain/reconcile"
)

// GetSystemStatus implements GET /admin/v1/system/status.
func (h *Handler) GetSystemStatus(ctx context.Context, _ gen.GetSystemStatusRequestObject) (gen.GetSystemStatusResponseObject, error) {
	st, err := h.systemStatus(ctx)
	if err != nil {
		return nil, err
	}
	return gen.GetSystemStatus200JSONResponse(st), nil
}

// systemStatus is the dashboard: what an operator checks first thing. One
// read of each source; a source that is not wired on this deployment
// (deposits without a chain, say) contributes zero rather than failing the
// whole page.
func (h *Handler) systemStatus(ctx context.Context) (gen.SystemStatus, error) {
	now := time.Now().UTC()
	st := gen.SystemStatus{Database: gen.SystemStatusDatabaseOk, Now: now}

	lines, err := h.ledger.TrialBalance(ctx)
	if err != nil {
		return gen.SystemStatus{}, fmt.Errorf("system status: trial balance: %w", err)
	}
	st.LedgerBalanced = true
	for _, l := range lines {
		if !l.Diff.IsZero() {
			st.LedgerBalanced = false
		}
	}
	open, err := h.ledger.OpenBreaks(ctx)
	if err != nil {
		return gen.SystemStatus{}, fmt.Errorf("system status: %w", err)
	}
	st.OpenLedgerBreaks = int(open)

	switch report, err := reconcile.Latest(ctx, h.pool, h.tenant, h.chainID); {
	case errors.Is(err, reconcile.ErrNoReport):
	case err != nil:
		return gen.SystemStatus{}, fmt.Errorf("system status: %w", err)
	default:
		st.LastReconciliation = &gen.ReconciliationSummary{ID: report.ID, FinishedAt: report.FinishedAt, Balanced: report.Balanced}
	}

	if h.withdrawals != nil {
		n, err := h.withdrawals.PendingCount(ctx)
		if err != nil {
			return gen.SystemStatus{}, fmt.Errorf("system status: %w", err)
		}
		st.PendingWithdrawals = int(n)
	}
	if h.deposits != nil {
		n, err := h.deposits.CountInStatus(ctx, deposit.StatusDetected, deposit.StatusConfirming)
		if err != nil {
			return gen.SystemStatus{}, fmt.Errorf("system status: %w", err)
		}
		st.ConfirmingDeposits = int(n)
	}
	if h.webhooks != nil {
		n, err := h.webhooks.DeadSince(ctx, now.Add(-24*time.Hour))
		if err != nil {
			return gen.SystemStatus{}, fmt.Errorf("system status: %w", err)
		}
		st.DeadWebhookDeliveries24H = int(n)
	}
	// The newest dump is what a restore starts from, so it is what the
	// dashboard shows; the WAL archive's age is on the Grafana system board.
	backups, err := h.LatestBackups(ctx)
	if err != nil {
		return gen.SystemStatus{}, fmt.Errorf("system status: %w", err)
	}
	for _, b := range backups {
		if b.Kind == BackupDump {
			st.LastBackup = &gen.BackupSummary{Kind: gen.BackupSummaryKind(b.Kind), FinishedAt: b.FinishedAt, SizeBytes: b.SizeBytes, Location: b.Location}
		}
	}
	return st, nil
}
