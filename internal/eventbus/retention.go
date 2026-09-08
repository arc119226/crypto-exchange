package eventbus

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/arc119226/crypto-exchange/internal/eventbus/sqlcgen"
)

// PruneOutbox deletes up to limit published outbox rows older than cutoff
// and reports how many went (docs/plan-v1.0.md §7.3 retention). Unpublished
// rows are left alone whatever their age.
func PruneOutbox(ctx context.Context, db sqlcgen.DBTX, cutoff time.Time, limit int32) (int64, error) {
	n, err := sqlcgen.New(db).PruneOutbox(ctx, sqlcgen.PruneOutboxParams{PublishedAt: pgtype.Timestamptz{Time: cutoff, Valid: true}, Limit: limit})
	if err != nil {
		return 0, fmt.Errorf("eventbus: prune outbox: %w", err)
	}
	return n, nil
}

// PruneProcessedEvents deletes up to limit consumer idempotency records
// older than cutoff.
func PruneProcessedEvents(ctx context.Context, db sqlcgen.DBTX, cutoff time.Time, limit int32) (int64, error) {
	n, err := sqlcgen.New(db).PruneProcessedEvents(ctx, sqlcgen.PruneProcessedEventsParams{ProcessedAt: cutoff, Limit: limit})
	if err != nil {
		return 0, fmt.Errorf("eventbus: prune processed events: %w", err)
	}
	return n, nil
}
