package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/platform/secretbox"
	"github.com/arc119226/crypto-exchange/internal/webhook/sqlcgen"
)

// maxResponseBody is how much of a customer's reply is read before giving up
// on it. Nothing is done with the body but log it, and an endpoint that
// answers with a gigabyte should cost us a kilobyte.
const maxResponseBody = 4 << 10

// Config is what the dispatcher needs to know.
type Config struct {
	Tenant string
	// Backoff is the delay before each retry: the first entry is the wait
	// after attempt 1 failed. Running off the end of it is what "dead" means,
	// so its length is the attempt budget (docs/plan-v1.0.md §7.6).
	Backoff []time.Duration
	// Timeout bounds one POST. A customer that accepts the connection and
	// then never answers must not hold a worker slot indefinitely.
	Timeout time.Duration
	// Batch is how many due deliveries one tick claims.
	Batch int32
	// MasterKey opens endpoints' sealed signing secrets (WEBHOOK_SIGNING_KEY).
	MasterKey []byte
}

// Dispatcher delivers events to customer endpoints.
//
// It is deliberately two halves that do not call each other. Enqueue runs
// from the JetStream consumer and does nothing but write rows; Deliver runs
// on its own clock and does nothing but send them. The split is what keeps
// JetStream's redelivery cheap: while an event is still queued, receiving it
// again re-runs an idempotent insert and nothing else.
//
// It is not a duplicate-suppression mechanism, and docs/webhooks.md says so to
// customers. Once a delivery finishes the queue row is gone, so a redelivery
// after that enqueues a fresh run and the endpoint is POSTed again with the
// same event_id. Delivery is at-least-once; the receiver deduplicates.
type Dispatcher struct {
	db      *pgxpool.Pool
	cfg     Config
	http    *http.Client
	log     *slog.Logger
	metrics *Metrics
	// now is overridden in tests that need to reason about the schedule.
	now func() time.Time
}

// New builds a dispatcher. The HTTP client is deliberately not
// http.DefaultClient: it has no timeout, and it follows redirects.
func New(db *pgxpool.Pool, cfg Config, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		db: db, cfg: cfg, log: log, metrics: NewMetrics(nil), now: func() time.Time { return time.Now().UTC() },
		http: &http.Client{
			Timeout: cfg.Timeout,
			// A webhook URL is operator-configured, but a redirect is chosen
			// by whoever answers it -- so following one lets the far end
			// point us anywhere, including at addresses only this network can
			// reach. Refusing redirects is the cheap half of SSRF defence;
			// the expensive half (resolving and rejecting private ranges) is
			// not done here because endpoints are created through the admin
			// API, not by users. If that ever changes, this is the comment
			// that says what is missing.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("webhook: refusing to follow a redirect")
			},
		},
	}
}

// WithClock replaces the dispatcher's clock, so a test can stand on either
// side of a rotation's grace period without waiting for it.
func (d *Dispatcher) WithClock(now func() time.Time) *Dispatcher {
	d.now = now
	return d
}

// WithMetrics attaches Prometheus collectors.
func (d *Dispatcher) WithMetrics(m *Metrics) *Dispatcher {
	if m != nil {
		d.metrics = m
	}
	return d
}

// Enqueue writes one queue row per endpoint that wants this event, and is the
// whole of what the JetStream handler does. It returns nil for an event
// nobody subscribes to, which acks it: an event with no subscribers is
// delivered, in the only sense that matters.
func (d *Dispatcher) Enqueue(ctx context.Context, e eventbus.Envelope) error {
	q := sqlcgen.New(d.db)
	endpoints, err := q.ListActiveEndpoints(ctx, d.cfg.Tenant)
	if err != nil {
		return fmt.Errorf("webhook: list endpoints: %w", err)
	}
	body, err := e.Marshal()
	if err != nil {
		return fmt.Errorf("webhook: marshal envelope: %w", err)
	}
	wanted := make([]sqlcgen.ListActiveEndpointsRow, 0, len(endpoints))
	for _, ep := range endpoints {
		if slices.Contains(ep.Events, e.EventType) {
			wanted = append(wanted, ep)
		}
	}
	if len(wanted) == 0 {
		// Nobody subscribes. Recording the body anyway would grow the table
		// with events no endpoint can ever be sent, and a subscription added
		// later starts from the stream, not from history.
		return nil
	}
	// The body first: the queue's foreign key points at it, and it is stored
	// once however many endpoints want it.
	if err := q.RecordEvent(ctx, sqlcgen.RecordEventParams{
		TenantID: d.cfg.Tenant, EventID: e.EventID, EventType: e.EventType, Body: body,
	}); err != nil {
		return fmt.Errorf("webhook: record event: %w", err)
	}
	for _, ep := range wanted {
		n, err := q.Enqueue(ctx, sqlcgen.EnqueueParams{
			TenantID: d.cfg.Tenant, EndpointID: ep.ID, EventID: e.EventID,
		})
		if err != nil {
			return fmt.Errorf("webhook: enqueue: %w", err)
		}
		if n == 0 {
			// Already queued: this is JetStream redelivering, which is
			// expected rather than exceptional.
			continue
		}
		d.metrics.queued.WithLabelValues(e.EventType).Inc()
	}
	return nil
}

// Deliver sends everything that is due and returns how many it attempted.
//
// Each delivery is its own transaction, so one endpoint timing out does not
// hold a lock over the others, and a crash loses at most the attempt in
// flight -- which the next tick retries, because the queue row is still there.
func (d *Dispatcher) Deliver(ctx context.Context) (int, error) {
	due, err := d.claim(ctx)
	if err != nil {
		return 0, err
	}
	for _, row := range due {
		if err := d.deliverOne(ctx, row); err != nil {
			if ctx.Err() != nil {
				return len(due), nil
			}
			// One endpoint's bookkeeping failing must not stop the others.
			d.log.Error("webhook delivery bookkeeping failed",
				slog.String("event_id", row.EventID), slog.String("err", err.Error()))
		}
	}
	return len(due), nil
}

// claim reads the due batch. The rows are read and the transaction closed
// before any HTTP happens: holding SKIP LOCKED rows across a network call to
// a stranger would mean one slow customer blocking the queue for everyone.
func (d *Dispatcher) claim(ctx context.Context) ([]sqlcgen.ClaimDueRow, error) {
	tx, err := d.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("webhook: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := sqlcgen.New(tx).ClaimDue(ctx, sqlcgen.ClaimDueParams{
		TenantID: d.cfg.Tenant, Limit: d.cfg.Batch,
	})
	if err != nil {
		return nil, fmt.Errorf("webhook: claim due: %w", err)
	}
	return rows, tx.Commit(ctx)
}

func (d *Dispatcher) deliverOne(ctx context.Context, row sqlcgen.ClaimDueRow) error {
	// The decrypted secret stays a local for the length of one signature and
	// is never put in a struct, which is the whole of its protection. A
	// redacting wrapper type would look like more, but nothing here logs an
	// endpoint, so it would be an unused type claiming to defend something --
	// the shape of guard docs/domain.md §22.8 is about.
	secret, err := secretbox.Open(d.cfg.MasterKey, row.SecretEnc)
	if err != nil {
		// The key is wrong or the row is corrupt. Neither is fixed by trying
		// again in a minute, so this counts as an attempt and burns budget
		// rather than spinning.
		return d.settle(ctx, row, attempt{status: StatusFailed, err: "cannot open the signing secret"})
	}
	secrets := []string{secret}
	// A rotated-out secret still signs until its grace period ends, so a
	// receiver that has not switched yet keeps verifying. The clock is ours,
	// not the database's: the row says when, and a test can move it.
	if row.PreviousSecretUntil.Valid && d.now().Before(row.PreviousSecretUntil.Time) && len(row.PreviousSecretEnc) > 0 {
		if previous, err := secretbox.Open(d.cfg.MasterKey, row.PreviousSecretEnc); err == nil {
			secrets = append(secrets, previous)
		}
	}
	res := d.post(ctx, row, secrets)
	return d.settle(ctx, row, res)
}

// attempt is the outcome of one POST.
type attempt struct {
	status   string
	code     int
	err      string
	duration time.Duration
}

func (d *Dispatcher) post(ctx context.Context, row sqlcgen.ClaimDueRow, secrets []string) attempt {
	sig, ts := SignAll(secrets, row.Body, d.now())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, row.Url, bytes.NewReader(row.Body))
	if err != nil {
		return attempt{status: StatusFailed, err: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(SignatureHeader, sig)
	req.Header.Set(TimestampHeader, ts)
	req.Header.Set(EventIDHeader, row.EventID)
	req.Header.Set(EventTypeHeader, row.EventType)

	started := time.Now()
	resp, err := d.http.Do(req)
	took := time.Since(started)
	if err != nil {
		// No response at all: a timeout, a refused connection, a bad name.
		// response_status stays null, which is how the table tells this apart
		// from a rejection.
		return attempt{status: StatusFailed, err: err.Error(), duration: took}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return attempt{status: StatusDelivered, code: resp.StatusCode, duration: took}
	}
	return attempt{
		status: StatusFailed, code: resp.StatusCode, duration: took,
		err: "endpoint answered " + resp.Status,
	}
}

// settle records the attempt and decides what the queue owes next, in one
// transaction: a delivery row without the matching queue change would either
// re-send something already delivered or lose the retry.
func (d *Dispatcher) settle(ctx context.Context, row sqlcgen.ClaimDueRow, a attempt) error {
	tx, err := d.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("webhook: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	// attempts counts the tries already made, so this one is attempts+1 and
	// the schedule step to wait afterwards is indexed by it.
	next := int(row.Attempts)
	final := a.status == StatusDelivered || next >= len(d.cfg.Backoff)
	status := a.status
	if final && a.status != StatusDelivered {
		status = StatusDead
	}

	p := sqlcgen.RecordAttemptParams{
		TenantID: row.TenantID, EndpointID: row.EndpointID, EventID: row.EventID,
		RunID: row.RunID, EventType: row.EventType, Attempt: row.Attempts, Status: status,
		Error: a.err, DurationMs: int32(a.duration.Milliseconds()), //nolint:gosec // a duration in ms is far below 2^31
	}
	if a.code != 0 {
		code := int32(a.code) //nolint:gosec // an HTTP status fits
		p.ResponseStatus = &code
	}
	if a.status == StatusDelivered {
		p.DeliveredAt = pgtype.Timestamptz{Time: d.now(), Valid: true}
	}
	if err := q.RecordAttempt(ctx, p); err != nil {
		return fmt.Errorf("webhook: record attempt: %w", err)
	}

	if final {
		if err := q.Dequeue(ctx, sqlcgen.DequeueParams{
			TenantID: row.TenantID, EndpointID: row.EndpointID, EventID: row.EventID,
			RunID: row.RunID,
		}); err != nil {
			return fmt.Errorf("webhook: dequeue: %w", err)
		}
	} else if err := q.RescheduleQueued(ctx, sqlcgen.RescheduleQueuedParams{
		TenantID: row.TenantID, EndpointID: row.EndpointID, EventID: row.EventID,
		RunID: row.RunID, NextAttemptAt: d.now().Add(d.cfg.Backoff[next]),
	}); err != nil {
		return fmt.Errorf("webhook: reschedule: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("webhook: commit: %w", err)
	}

	d.metrics.attempts.WithLabelValues(row.EventType, status).Inc()
	switch status {
	case StatusDelivered:
		d.log.Info("webhook delivered", slog.String("event_id", row.EventID),
			slog.String("event_type", row.EventType), slog.Int("attempt", int(row.Attempts)),
			slog.Int64("ms", a.duration.Milliseconds()))
	case StatusDead:
		// The one outcome an operator has to act on: after this the customer
		// never gets the event unless somebody replays it by hand.
		d.log.Error("webhook gave up: no attempts left",
			slog.String("event_id", row.EventID), slog.String("event_type", row.EventType),
			slog.Int("attempts", int(row.Attempts)+1), slog.String("err", a.err))
	default:
		d.log.Warn("webhook attempt failed, will retry",
			slog.String("event_id", row.EventID), slog.Int("attempt", int(row.Attempts)),
			slog.Duration("next_in", d.cfg.Backoff[next]), slog.String("err", a.err))
	}
	return nil
}

// Delivery statuses, matching migration 0016's CHECK.
const (
	StatusPending   = "pending"
	StatusDelivered = "delivered"
	StatusFailed    = "failed"
	StatusDead      = "dead"
)
