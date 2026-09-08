package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/webhook"
)

// RetentionConfig is how long the worker role keeps what only history
// needs (docs/plan-v1.0.md §7.3 sets the outbox at 30 days). Everything
// else the tables hold is state, not history, and is never pruned.
type RetentionConfig struct {
	// Interval between prune passes; a pass deletes in batches of BatchSize
	// until a table has nothing older left.
	Interval time.Duration `env:"INTERVAL" envDefault:"1h"`
	// Outbox keeps published outbox rows and consumer idempotency records
	// this long; the private stream's resume reads the outbox, so a resume
	// older than this answers resume_failed (docs/ws-api.md).
	Outbox time.Duration `env:"OUTBOX" envDefault:"720h"`
	// Webhook keeps delivery attempts and stored event bodies this long
	// (bodies still queued for an endpoint stay whatever their age).
	Webhook   time.Duration `env:"WEBHOOK" envDefault:"2160h"`
	BatchSize int32         `env:"BATCH_SIZE" envDefault:"5000"`
}

// pruneTable is one table's prune: delete up to limit rows older than cutoff.
type pruneTable struct {
	name   string
	cutoff time.Duration
	prune  func(ctx context.Context, cutoff time.Time, limit int32) (int64, error)
}

// pruneTables lists what retention removes, oldest kind of record first.
func pruneTables(pool *pgxpool.Pool, cfg RetentionConfig) []pruneTable {
	return []pruneTable{
		{"eventbus.outbox", cfg.Outbox, func(ctx context.Context, c time.Time, n int32) (int64, error) {
			return eventbus.PruneOutbox(ctx, pool, c, n)
		}},
		{"eventbus.processed_events", cfg.Outbox, func(ctx context.Context, c time.Time, n int32) (int64, error) {
			return eventbus.PruneProcessedEvents(ctx, pool, c, n)
		}},
		{"webhook.deliveries", cfg.Webhook, func(ctx context.Context, c time.Time, n int32) (int64, error) {
			return webhook.PruneDeliveries(ctx, pool, c, n)
		}},
		{"webhook.events", cfg.Webhook, func(ctx context.Context, c time.Time, n int32) (int64, error) {
			return webhook.PruneEvents(ctx, pool, c, n)
		}},
	}
}

// PruneRetention runs one pass over every retained table as of now and
// returns the rows deleted per table. The worker role calls it on
// RetentionConfig.Interval; tests and operators can call it directly.
func PruneRetention(ctx context.Context, pool *pgxpool.Pool, cfg RetentionConfig, now time.Time) (map[string]int64, error) {
	deleted := map[string]int64{}
	for _, t := range pruneTables(pool, cfg) {
		cutoff := now.Add(-t.cutoff)
		for {
			n, err := t.prune(ctx, cutoff, cfg.BatchSize)
			if err != nil {
				return deleted, fmt.Errorf("retention %s: %w", t.name, err)
			}
			deleted[t.name] += n
			if n < int64(cfg.BatchSize) {
				break
			}
		}
	}
	return deleted, nil
}

// retention is the worker role's prune loop with its instrument.
type retention struct {
	pool    *pgxpool.Pool
	cfg     RetentionConfig
	deleted *prometheus.CounterVec
}

func newRetention(pool *pgxpool.Pool, cfg RetentionConfig, reg prometheus.Registerer) *retention {
	r := &retention{pool: pool, cfg: cfg, deleted: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "retention_deleted_rows_total", Help: "Rows the retention job removed, by table.",
	}, []string{"table"})}
	reg.MustRegister(r.deleted)
	return r
}

// run prunes once at start and then on every interval until ctx ends.
func (r *retention) run(ctx context.Context, log *slog.Logger) error {
	log.Info("retention job running", slog.Duration("interval", r.cfg.Interval),
		slog.Duration("outbox", r.cfg.Outbox), slog.Duration("webhook", r.cfg.Webhook))
	tick := time.NewTicker(r.cfg.Interval)
	defer tick.Stop()
	for {
		deleted, err := PruneRetention(ctx, r.pool, r.cfg, time.Now())
		for table, n := range deleted {
			r.deleted.WithLabelValues(table).Add(float64(n))
		}
		if err != nil && ctx.Err() == nil {
			log.Error("retention pass failed", slog.String("err", err.Error()))
		} else if total(deleted) > 0 {
			log.Info("retention pass", slog.Any("deleted", deleted))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func total(m map[string]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}
