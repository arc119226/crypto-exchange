package webhook

import (
	"context"
	"fmt"
	"time"

	"github.com/arc119226/crypto-exchange/internal/webhook/sqlcgen"
)

// PruneDeliveries deletes up to limit delivery attempts older than cutoff
// and reports how many went.
func PruneDeliveries(ctx context.Context, db sqlcgen.DBTX, cutoff time.Time, limit int32) (int64, error) {
	n, err := sqlcgen.New(db).PruneDeliveries(ctx, sqlcgen.PruneDeliveriesParams{CreatedAt: cutoff, Limit: limit})
	if err != nil {
		return 0, fmt.Errorf("webhook: prune deliveries: %w", err)
	}
	return n, nil
}

// PruneEvents deletes up to limit stored event bodies older than cutoff
// that no queue row references any more.
func PruneEvents(ctx context.Context, db sqlcgen.DBTX, cutoff time.Time, limit int32) (int64, error) {
	n, err := sqlcgen.New(db).PruneEvents(ctx, sqlcgen.PruneEventsParams{CreatedAt: cutoff, Limit: limit})
	if err != nil {
		return 0, fmt.Errorf("webhook: prune events: %w", err)
	}
	return n, nil
}
