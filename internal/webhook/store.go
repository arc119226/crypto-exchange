package webhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/platform/secretbox"
	"github.com/arc119226/crypto-exchange/internal/webhook/sqlcgen"
)

// Endpoint is a customer URL and what it subscribes to. The signing secret is
// not a field: it is returned once by Create and never read back out of this
// package except by the dispatcher, which needs it to sign.
type Endpoint struct {
	ID        string
	URL       string
	Events    []string
	Label     string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Endpoint statuses, matching migration 0016's CHECK.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// Delivery is one attempt, as an operator sees it.
type Delivery struct {
	ID             string
	EventID        string
	EventType      string
	RunID          string
	Attempt        int32
	Status         string
	ResponseStatus *int32
	Error          string
	Duration       time.Duration
	CreatedAt      time.Time
	DeliveredAt    *time.Time
}

// Replay is the run a replay started.
type Replay struct {
	EndpointID    string
	EventID       string
	RunID         string
	NextAttemptAt time.Time
}

// What the admin side can be told no for.
var (
	ErrNotFound = errors.New("webhook: no such endpoint or delivery")
	ErrInvalid  = errors.New("webhook: invalid endpoint")
	// ErrQueued means the event is still owed to this endpoint, so the
	// schedule will send it without anyone replaying anything.
	ErrQueued = errors.New("webhook: already queued for this endpoint")
	// ErrDisabled means a replay would have written a row nothing can reach:
	// ClaimDue skips inactive endpoints.
	ErrDisabled = errors.New("webhook: endpoint is disabled")
)

// Store is the admin half of this package: everything the operator API needs
// and nothing the dispatcher does. It is separate from Dispatcher because the
// two run in different roles -- admin has no NATS and never delivers, the
// worker has no business editing an endpoint (migration 0016 enforces both in
// the grants).
type Store struct {
	db      *pgxpool.Pool
	tenant  string
	master  []byte
	metrics *AdminMetrics
}

// NewStore wires the admin API to the webhook tables. master is
// WEBHOOK_SIGNING_KEY, needed only to seal new endpoints' secrets.
func NewStore(db *pgxpool.Pool, tenant string, master []byte) *Store {
	if tenant == "" {
		tenant = "default"
	}
	return &Store{db: db, tenant: tenant, master: master, metrics: NewAdminMetrics(nil)}
}

// WithMetrics attaches the Prometheus collector.
func (s *Store) WithMetrics(m *AdminMetrics) *Store {
	if m != nil {
		s.metrics = m
	}
	return s
}

// Create registers an endpoint and returns its signing secret, which is the
// only time the secret exists outside the database. Nothing stores it, nothing
// logs it, and no read path can produce it again.
func (s *Store) Create(ctx context.Context, tx pgx.Tx, u string, events []string, label string) (Endpoint, string, error) {
	if err := validate(u, events); err != nil {
		return Endpoint{}, "", err
	}
	secret, err := newSecret()
	if err != nil {
		return Endpoint{}, "", err
	}
	sealed, err := secretbox.Seal(s.master, secret)
	if err != nil {
		return Endpoint{}, "", fmt.Errorf("webhook: seal secret: %w", err)
	}
	row, err := sqlcgen.New(tx).CreateEndpoint(ctx, sqlcgen.CreateEndpointParams{
		TenantID: s.tenant, Url: u, SecretEnc: sealed, Events: events, Label: label,
	})
	if err != nil {
		return Endpoint{}, "", fmt.Errorf("webhook: create endpoint: %w", err)
	}
	return Endpoint{
		ID: row.ID, URL: row.Url, Events: row.Events, Label: row.Label,
		Status: row.Status, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, secret, nil
}

// List returns every endpoint, disabled ones included.
func (s *Store) List(ctx context.Context) ([]Endpoint, error) {
	rows, err := sqlcgen.New(s.db).ListEndpoints(ctx, s.tenant)
	if err != nil {
		return nil, fmt.Errorf("webhook: list endpoints: %w", err)
	}
	out := make([]Endpoint, 0, len(rows))
	for _, r := range rows {
		out = append(out, Endpoint{
			ID: r.ID, URL: r.Url, Events: r.Events, Label: r.Label,
			Status: r.Status, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
	}
	return out, nil
}

// Get returns one endpoint, or ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (Endpoint, error) {
	if !looksLikeUUID(id) {
		return Endpoint{}, ErrNotFound
	}
	row, err := sqlcgen.New(s.db).GetEndpoint(ctx, sqlcgen.GetEndpointParams{TenantID: s.tenant, ID: id})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Endpoint{}, ErrNotFound
	case err != nil:
		return Endpoint{}, fmt.Errorf("webhook: get endpoint: %w", err)
	}
	return Endpoint{
		ID: row.ID, URL: row.Url, Events: row.Events, Label: row.Label,
		Status: row.Status, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// Update replaces the mutable configuration wholesale, which is what the PUT
// it serves means. The secret is not among it.
func (s *Store) Update(ctx context.Context, tx pgx.Tx, id, u string, events []string, label string) (Endpoint, error) {
	if err := validate(u, events); err != nil {
		return Endpoint{}, err
	}
	if !looksLikeUUID(id) {
		return Endpoint{}, ErrNotFound
	}
	row, err := sqlcgen.New(tx).UpdateEndpoint(ctx, sqlcgen.UpdateEndpointParams{
		TenantID: s.tenant, ID: id, Url: u, Events: events, Label: label,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Endpoint{}, ErrNotFound
	case err != nil:
		return Endpoint{}, fmt.Errorf("webhook: update endpoint: %w", err)
	}
	return Endpoint{
		ID: row.ID, URL: row.Url, Events: row.Events, Label: row.Label,
		Status: row.Status, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

// SetStatus enables or disables an endpoint, and returns how many queued
// deliveries disabling it dropped.
//
// Dropping them is the point rather than a side effect. ClaimDue only looks at
// active endpoints, so a queued row for a disabled one is not pending -- it is
// unreachable. Nothing would ever deliver it, nothing would ever delete it
// (every delete path is inside the dispatcher's settle), it would block a
// later replay of the same event on the primary key, it would pin its
// webhook.events row through the foreign key, and re-enabling the endpoint
// later would fire a batch of stale events at a customer who had just turned
// their integration back on. The queue is what is owed; after a deliberate
// disable, nothing is.
func (s *Store) SetStatus(ctx context.Context, tx pgx.Tx, id, status string) (Endpoint, int64, error) {
	if status != StatusActive && status != StatusDisabled {
		return Endpoint{}, 0, fmt.Errorf("%w: unknown status %q", ErrInvalid, status)
	}
	if !looksLikeUUID(id) {
		return Endpoint{}, 0, ErrNotFound
	}
	q := sqlcgen.New(tx)
	row, err := q.SetEndpointStatus(ctx, sqlcgen.SetEndpointStatusParams{
		TenantID: s.tenant, ID: id, Status: status,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Endpoint{}, 0, ErrNotFound
	case err != nil:
		return Endpoint{}, 0, fmt.Errorf("webhook: set endpoint status: %w", err)
	}
	var dropped int64
	if status == StatusDisabled {
		dropped, err = q.DeleteQueuedForEndpoint(ctx, sqlcgen.DeleteQueuedForEndpointParams{
			TenantID: s.tenant, EndpointID: id,
		})
		if err != nil {
			return Endpoint{}, 0, fmt.Errorf("webhook: drop queued deliveries: %w", err)
		}
	}
	return Endpoint{
		ID: row.ID, URL: row.Url, Events: row.Events, Label: row.Label,
		Status: row.Status, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, dropped, nil
}

// Deliveries is what happened, newest first.
func (s *Store) Deliveries(ctx context.Context, endpointID string, limit, offset int32) ([]Delivery, error) {
	if !looksLikeUUID(endpointID) {
		return nil, ErrNotFound
	}
	rows, err := sqlcgen.New(s.db).ListDeliveries(ctx, sqlcgen.ListDeliveriesParams{
		TenantID: s.tenant, EndpointID: endpointID, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, fmt.Errorf("webhook: list deliveries: %w", err)
	}
	out := make([]Delivery, 0, len(rows))
	for _, r := range rows {
		d := Delivery{
			ID: r.ID, EventID: r.EventID, EventType: r.EventType, RunID: r.RunID,
			Attempt: r.Attempt, Status: r.Status, ResponseStatus: r.ResponseStatus,
			Error: r.Error, Duration: time.Duration(r.DurationMs) * time.Millisecond,
			CreatedAt: r.CreatedAt,
		}
		if r.DeliveredAt.Valid {
			t := r.DeliveredAt.Time
			d.DeliveredAt = &t
		}
		out = append(out, d)
	}
	return out, nil
}

// Replay sends an event to an endpoint again, by starting a new run of it.
//
// An operator points at a delivery row because that is what the list shows
// them; what is replayed is the event behind it. Nothing about the old
// delivery changes -- deliveries is append-only, and the replay's attempts
// join it as a new run.
func (s *Store) Replay(ctx context.Context, tx pgx.Tx, endpointID, deliveryID string) (Replay, error) {
	if !looksLikeUUID(endpointID) || !looksLikeUUID(deliveryID) {
		return Replay{}, ErrNotFound
	}
	q := sqlcgen.New(tx)
	d, err := q.GetDelivery(ctx, sqlcgen.GetDeliveryParams{
		TenantID: s.tenant, EndpointID: endpointID, ID: deliveryID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Replay{}, ErrNotFound
	case err != nil:
		return Replay{}, fmt.Errorf("webhook: get delivery: %w", err)
	}
	// Read the endpoint in the same transaction as the insert, so a disable
	// committing alongside cannot leave behind exactly the unreachable row
	// SetStatus exists to prevent.
	ep, err := q.GetEndpoint(ctx, sqlcgen.GetEndpointParams{TenantID: s.tenant, ID: endpointID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Replay{}, ErrNotFound
	case err != nil:
		return Replay{}, fmt.Errorf("webhook: get endpoint: %w", err)
	}
	if ep.Status != StatusActive {
		return Replay{}, ErrDisabled
	}
	row, err := q.EnqueueReplay(ctx, sqlcgen.EnqueueReplayParams{
		TenantID: s.tenant, EndpointID: endpointID, EventID: d.EventID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The key is taken, so this event is still owed to this endpoint and
		// the schedule will send it. Replaying would be asking for a second
		// copy of something already on its way.
		return Replay{}, ErrQueued
	case err != nil:
		return Replay{}, fmt.Errorf("webhook: enqueue replay: %w", err)
	}
	s.metrics.replays.Inc()
	return Replay{
		EndpointID: endpointID, EventID: d.EventID,
		RunID: row.RunID, NextAttemptAt: row.NextAttemptAt,
	}, nil
}

// QueuedAt says when the retry that blocked a replay is due, so the refusal
// can name it. ErrNotFound if the row has gone in the meantime.
func (s *Store) QueuedAt(ctx context.Context, endpointID, deliveryID string) (time.Time, error) {
	if !looksLikeUUID(endpointID) || !looksLikeUUID(deliveryID) {
		return time.Time{}, ErrNotFound
	}
	q := sqlcgen.New(s.db)
	d, err := q.GetDelivery(ctx, sqlcgen.GetDeliveryParams{
		TenantID: s.tenant, EndpointID: endpointID, ID: deliveryID,
	})
	if err != nil {
		return time.Time{}, ErrNotFound
	}
	row, err := q.GetQueued(ctx, sqlcgen.GetQueuedParams{
		TenantID: s.tenant, EndpointID: endpointID, EventID: d.EventID,
	})
	if err != nil {
		return time.Time{}, ErrNotFound
	}
	return row.NextAttemptAt, nil
}

// validate refuses what migration 0016's CHECKs would refuse anyway, so the
// caller gets a 400 that says which field rather than a constraint name.
func validate(u string, events []string) error {
	parsed, err := url.Parse(u)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("%w: url must be an absolute http:// or https:// address", ErrInvalid)
	}
	if len(events) == 0 {
		return fmt.Errorf("%w: at least one event type is required", ErrInvalid)
	}
	for _, e := range events {
		if strings.TrimSpace(e) == "" {
			return fmt.Errorf("%w: event types cannot be blank", ErrInvalid)
		}
	}
	return nil
}

// looksLikeUUID keeps a path parameter that cannot name a row from reaching
// Postgres, where it would come back as a type error and surface as a 500. An
// id that is not a uuid names nothing, which is what 404 means.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// newSecret returns a fresh 32-byte secret, hex-encoded. Same shape and same
// reasoning as internal/auth's: the server has to recompute the HMAC, so this
// is encrypted rather than hashed, and shown to the operator exactly once.
func newSecret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("webhook: secret: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
