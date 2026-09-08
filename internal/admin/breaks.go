package admin

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// WatchLedgerBreaks runs one pass of the ledger's self-check and publishes
// a reconciliation.ledger_break_detected for every break it opened, in the
// transaction that opened it. The admin role calls it every thirty seconds
// next to the trial-balance gauge (docs/plan-v1.0.md §12, §15); a test calls
// it directly. It returns how many breaks it opened and resolved.
func (h *Handler) WatchLedgerBreaks(ctx context.Context) (opened, resolved int, err error) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	err = h.inTx(ctx, func(tx pgx.Tx) error {
		o, r, err := h.ledger.ReconcileBreaks(ctx, tx, now)
		if err != nil {
			return err
		}
		opened, resolved = len(o), len(r)
		for _, b := range o {
			evt, err := LedgerBreakEvent(h.tenant, b, now)
			if err != nil {
				return err
			}
			if _, err := (eventbus.Outbox{}).Append(ctx, tx, evt); err != nil {
				return err
			}
			telemetry.Logger(ctx).Error("ledger break detected: debits and credits disagree",
				slog.String("asset", b.Asset), slog.String("diff", b.Diff.String()), slog.String("break_id", b.ID))
		}
		for _, b := range r {
			telemetry.Logger(ctx).Info("ledger break resolved", slog.String("asset", b.Asset), slog.String("break_id", b.ID))
		}
		return nil
	})
	return opened, resolved, err
}
