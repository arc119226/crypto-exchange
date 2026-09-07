package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/webhook"
)

// webhookDurable is the durable consumer prefix; one per stream is derived
// from it (worker-webhook-trading and friends).
const webhookDurable = "worker-webhook"

// workerComponents is the worker role: the event consumers nobody is waiting
// on (docs/plan-v1.0.md §5.2). Today that is webhook delivery.
//
// This role has existed in AllRoles since Phase 3 and never had a case in
// run.go -- it fell through to "role not implemented yet". This is the first
// thing it actually does.
type workerComponents struct {
	dispatcher *webhook.Dispatcher
	interval   time.Duration
	subs       webhookSubs

	// The start-state shape is copied from chainComponents, deliberately and
	// exactly: newWorker only builds, bringUp does the waiting, and start runs
	// after the ops server is listening so /readyz can say why it is not up.
	// Doing that wrong in the chain role cost 4d-2b a defect where /readyz
	// refused the connection instead of answering.
	bringUp  func(context.Context) error
	started  chan struct{}
	startMu  sync.Mutex
	startErr error
}

// newWorker builds the role. It touches neither NATS nor the database.
func newWorker(cfg Config, log *slog.Logger, db *pgxpool.Pool, reg prometheus.Registerer, nc *nats.Conn) (*workerComponents, error) {
	if nc == nil {
		// Deliveries arrive over JetStream, so without NATS there is nothing
		// for this role to consume. Refusing to start is honest; pretending
		// to run would leave events silently undelivered.
		return nil, errors.New("config: NATS_URL is required for the worker role")
	}
	master, err := cfg.Webhook.Master()
	if err != nil {
		return nil, err
	}
	d := webhook.New(db, webhook.Config{
		Tenant: cfg.TenantID, Backoff: cfg.Webhook.Backoff, Timeout: cfg.Webhook.Timeout,
		Batch: cfg.Webhook.BatchSize, MasterKey: master,
	}, log).WithMetrics(webhook.NewMetrics(reg))

	w := &workerComponents{
		dispatcher: d, interval: cfg.Webhook.Interval,
		started:  make(chan struct{}),
		startErr: errors.New("the worker role has not finished starting"),
	}
	w.bringUp = func(ctx context.Context) error { return w.up(ctx, cfg, log, nc) }
	if len(master) == 0 {
		log.Warn("WEBHOOK_SIGNING_KEY is empty: endpoints cannot be signed for, so nothing will be delivered")
	}
	log.Info("delivering webhooks",
		slog.Duration("interval", cfg.Webhook.Interval),
		slog.Int("attempts", len(cfg.Webhook.Backoff)),
		slog.Duration("gives_up_after", totalBackoff(cfg.Webhook.Backoff)))
	return w, nil
}

// up connects to JetStream and subscribes. Both wait on another process, so
// neither belongs in newWorker.
func (w *workerComponents) up(ctx context.Context, cfg Config, log *slog.Logger, nc *nats.Conn) error {
	w.setStartErr(errors.New("connecting to JetStream"))
	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}
	// The worker can legitimately start before any engine, so it ensures the
	// streams itself rather than assuming somebody already did. Idempotent.
	w.setStartErr(errors.New("waiting for the event streams"))
	if err := retryUntil(ctx, log, "jetstream streams", func(ctx context.Context) error {
		return eventbus.EnsureStreams(ctx, js)
	}); err != nil {
		return err
	}
	w.setStartErr(errors.New("subscribing to the event streams"))
	subs, err := consumeForWebhooks(ctx, js, w.dispatcher, cfg.TenantID, webhookDurable, log)
	if err != nil {
		return fmt.Errorf("webhook consumers: %w", err)
	}
	w.subs = subs
	return nil
}

func (w *workerComponents) start(ctx context.Context) error {
	if err := w.bringUp(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		w.setStartErr(err)
		return err
	}
	w.setStartErr(nil)
	close(w.started)
	return nil
}

func (w *workerComponents) setStartErr(err error) {
	w.startMu.Lock()
	defer w.startMu.Unlock()
	w.startErr = err
}

// starting reports why the role is not up yet, or nil once it is.
func (w *workerComponents) starting() error {
	w.startMu.Lock()
	defer w.startMu.Unlock()
	if w.startErr == nil {
		return nil
	}
	return fmt.Errorf("still starting: %w", w.startErr)
}

func (w *workerComponents) awaitStart(ctx context.Context) bool {
	select {
	case <-w.started:
		return true
	case <-ctx.Done():
		return false
	}
}

// runDelivering sends what the consumers queued, on its own clock.
//
// A separate loop from the consumers on purpose: enqueuing is fast and
// database-only, delivering waits on strangers' servers. Sharing a goroutine
// would let one unreachable customer stop every other customer's events from
// being queued at all.
func (w *workerComponents) runDelivering(ctx context.Context, log *slog.Logger) error {
	if !w.awaitStart(ctx) {
		return nil
	}
	tick := time.NewTicker(w.interval)
	defer tick.Stop()
	for {
		if _, err := w.dispatcher.Deliver(ctx); err != nil && ctx.Err() == nil {
			log.Error("webhook delivery tick failed", slog.String("err", err.Error()))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// ready reports the role is up. The start check comes first for the same
// reason it does in the chain role: while starting, the reason is the only
// useful answer, and it is what lets /readyz answer at all.
func (w *workerComponents) ready(context.Context) error { return w.starting() }

func (w *workerComponents) close() {
	if w != nil {
		w.subs.Stop()
	}
}

// totalBackoff is how long the schedule keeps trying before dead, which is
// the number an operator actually wants to know.
func totalBackoff(steps []time.Duration) time.Duration {
	var total time.Duration
	for _, s := range steps {
		total += s
	}
	return total
}
