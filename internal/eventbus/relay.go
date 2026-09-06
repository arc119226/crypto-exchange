package eventbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/arc119226/crypto-exchange/internal/eventbus/sqlcgen"
)

// Publisher is what the relay publishes to. JetStream is the only
// implementation in v1; the interface keeps the outbox independent of it.
type Publisher interface {
	// Publish sends one envelope; msgID is the deduplication key.
	Publish(ctx context.Context, subject, msgID string, body []byte) error
}

// JetStreamPublisher publishes with Nats-Msg-Id so a republish inside the
// duplicate window is dropped by the server.
type JetStreamPublisher struct{ js jetstream.JetStream }

// NewJetStreamPublisher wraps a JetStream context.
func NewJetStreamPublisher(js jetstream.JetStream) *JetStreamPublisher {
	return &JetStreamPublisher{js: js}
}

// Publish implements Publisher.
func (p *JetStreamPublisher) Publish(ctx context.Context, subject, msgID string, body []byte) error {
	msg := &nats.Msg{Subject: subject, Data: body, Header: nats.Header{}}
	msg.Header.Set(jetstream.MsgIDHeader, msgID)
	if _, err := p.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("eventbus: publish %s: %w", subject, err)
	}
	return nil
}

// RelayConfig tunes the relay loop.
type RelayConfig struct {
	PollInterval time.Duration // fallback wake-up when no NOTIFY arrives (default 100 ms)
	BatchSize    int32         // rows per publish batch (default 100)
	Channel      string        // LISTEN channel (default outbox_new)
}

func (c RelayConfig) withDefaults() RelayConfig {
	if c.PollInterval <= 0 {
		c.PollInterval = 100 * time.Millisecond
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.Channel == "" {
		c.Channel = "outbox_new"
	}
	return c
}

// Relay moves unpublished outbox rows to the Publisher in id order. Exactly
// one relay runs per database (the engine role holds the advisory lock that
// guarantees a single engine); id order is the commit order only
// approximately, so consumers order by seq / account_seq.
type Relay struct {
	pool    *pgxpool.Pool
	pub     Publisher
	cfg     RelayConfig
	log     *slog.Logger
	metrics *Metrics
}

// NewRelay builds a relay.
func NewRelay(pool *pgxpool.Pool, pub Publisher, cfg RelayConfig, log *slog.Logger) *Relay {
	if log == nil {
		log = slog.Default()
	}
	return &Relay{pool: pool, pub: pub, cfg: cfg.withDefaults(), log: log}
}

// WithMetrics attaches Prometheus instruments.
func (r *Relay) WithMetrics(m *Metrics) *Relay {
	r.metrics = m
	return r
}

// Run publishes until ctx is cancelled. It holds one pooled connection for
// LISTEN and wakes up on notifications or every PollInterval, whichever
// comes first; a database error is logged and retried after PollInterval.
func (r *Relay) Run(ctx context.Context) error {
	for {
		err := r.listenLoop(ctx)
		if ctx.Err() != nil {
			return nil
		}
		r.log.Warn("outbox relay: listener failed, reconnecting", slog.String("err", err.Error()))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.cfg.PollInterval * 5):
		}
	}
}

func (r *Relay) listenLoop(ctx context.Context) error {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+r.cfg.Channel); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	lastMetrics := time.Time{}
	for {
		// drain everything that is pending, then wait for the next wake-up
		for {
			n, err := r.Drain(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				r.log.Warn("outbox relay: publish batch failed", slog.String("err", err.Error()))
				break
			}
			if n < int(r.cfg.BatchSize) {
				break
			}
		}
		if time.Since(lastMetrics) >= time.Second {
			r.observe(ctx)
			lastMetrics = time.Now()
		}
		wctx, cancel := context.WithTimeout(ctx, r.cfg.PollInterval)
		_, err := conn.Conn().WaitForNotification(wctx)
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("wait for notification: %w", err)
		}
	}
}

// Drain publishes one batch of unpublished rows and returns how many it
// published. It is exported for tests and for `exchangectl`-style tooling.
func (r *Relay) Drain(ctx context.Context) (int, error) {
	q := sqlcgen.New(r.pool)
	rows, err := q.ListUnpublished(ctx, r.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("eventbus: list unpublished: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		env := envelopeFromRow(row)
		body, err := env.Marshal()
		if err != nil {
			return len(ids), fmt.Errorf("eventbus: marshal %s: %w", row.EventID, err)
		}
		if err := r.pub.Publish(ctx, row.Subject, row.EventID, body); err != nil {
			// stop at the first failure so ids stay in order; what was
			// published is marked below and the rest is retried
			if len(ids) > 0 {
				if merr := q.MarkPublished(ctx, ids); merr != nil {
					return 0, fmt.Errorf("eventbus: mark published: %w (after publish error %v)", merr, err)
				}
			}
			return len(ids), err
		}
		ids = append(ids, row.ID)
	}
	if err := q.MarkPublished(ctx, ids); err != nil {
		return 0, fmt.Errorf("eventbus: mark published: %w", err)
	}
	if r.metrics != nil {
		r.metrics.published.Add(float64(len(ids)))
	}
	return len(ids), nil
}

func (r *Relay) observe(ctx context.Context) {
	if r.metrics == nil {
		return
	}
	b, err := sqlcgen.New(r.pool).OutboxBacklog(ctx)
	if err != nil {
		return
	}
	r.metrics.backlog.Set(float64(b.Backlog))
	r.metrics.lag.Set(b.OldestAgeSeconds)
}
