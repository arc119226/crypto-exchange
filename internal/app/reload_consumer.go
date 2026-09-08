package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

// reloadConsumer is the durable consumer name on EX_REGISTRY.
const reloadConsumer = "engine-registry"

// reloadSkew is how much producer/consumer clock difference the coalescing
// tolerates. Skipping a needed reload is far worse than doing a redundant
// one, so the comparison errs towards reloading.
const reloadSkew = time.Second

// reloader turns registry events into Engine.Reload calls.
//
// A burst — an operator halting several markets, or a seed run — must not
// mean one full registry read per event, but nothing may be skipped either.
// Instead of debouncing on a timer (which would have to ack before the work
// happened) each event checks whether a reload has already *started* after
// it was published: if so the reload already saw the change and the event is
// simply acked, otherwise it reloads and any failure naks for redelivery.
type reloader struct {
	engine *trading.Engine
	log    *slog.Logger

	mu   sync.Mutex
	last time.Time // when the most recent reload started
}

func (r *reloader) handle(ctx context.Context, e eventbus.Envelope) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last.After(e.OccurredAt.Add(reloadSkew)) {
		r.log.Debug("registry event already covered by an earlier reload",
			slog.String("event_id", e.EventID), slog.String("event_type", e.EventType))
		return nil
	}
	started := time.Now().UTC()
	if err := r.engine.Reload(ctx); err != nil {
		return err
	}
	r.last = started
	r.log.Info("registry reloaded",
		slog.String("event_type", e.EventType), slog.String("event_id", e.EventID),
		slog.Any("markets", r.engine.Markets()))
	return nil
}

// newReloadConsumer subscribes the engine to registry changes so a market
// added, halted or delisted through the admin API takes effect without a
// restart (docs/plan-v1.0.md §6.6). Runners read the market from the
// registry cache per command, so reloading the cache is all it takes for a
// status change to apply to the very next order.
func newReloadConsumer(ctx context.Context, cfg Config, log *slog.Logger, js jetstream.JetStream, engine *trading.Engine) (*eventbus.Subscription, error) {
	r := &reloader{engine: engine, log: log}
	return eventbus.Subscribe(ctx, js, eventbus.ConsumerConfig{
		Durable: reloadConsumer,
		Stream:  eventbus.StreamRegistry,
		FilterSubjects: []string{
			eventbus.SubjectPrefix + ".market.*." + cfg.TenantID + ".*",
			eventbus.SubjectPrefix + ".asset.*." + cfg.TenantID + ".*",
			eventbus.SubjectPrefix + ".fee_schedule.*." + cfg.TenantID + ".*",
			// registry.reload: an operator asking for a reload with no row changed
			eventbus.SubjectPrefix + ".registry.*." + cfg.TenantID + ".*",
		},
	}, log, r.handle)
}
