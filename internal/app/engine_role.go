package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

// engineComponents is what the engine role runs: the per-market runners and
// the outbox relay (docs/plan-v1.0.md §5.1).
type engineComponents struct {
	engine *trading.Engine
	relay  *eventbus.Relay // nil without NATS
}

// newEngine builds the trading engine: ledger, registry cache, metrics. It
// does not start it; Run does so it can abort on lock/restore errors.
func newEngine(cfg Config, log *slog.Logger, pool *pgxpool.Pool, l *ledger.Service, reg prometheus.Registerer) *trading.Engine {
	store := registry.NewStore(pool)
	cache := registry.NewCache(cfg.TenantID)
	return trading.NewEngine(pool, l, cache, store, cfg.TenantID, log).
		WithMetrics(trading.NewMetrics(reg)).
		WithQueueSize(cfg.Engine.QueueSize)
}

// newRelay wires the outbox relay to JetStream, declaring the streams first.
func newRelay(ctx context.Context, cfg Config, log *slog.Logger, pool *pgxpool.Pool, nc *nats.Conn, reg prometheus.Registerer) (*eventbus.Relay, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	if err := retryUntil(ctx, log, "jetstream streams", func(ctx context.Context) error {
		return eventbus.EnsureStreams(ctx, js)
	}); err != nil {
		return nil, err
	}
	relay := eventbus.NewRelay(pool, eventbus.NewJetStreamPublisher(js), eventbus.RelayConfig{
		PollInterval: cfg.Outbox.PollInterval, BatchSize: cfg.Outbox.BatchSize,
	}, log).WithMetrics(eventbus.NewMetrics(reg))
	return relay, nil
}
