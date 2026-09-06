// Package eventbus is the transactional outbox and its JetStream relay
// (docs/plan-v1.0.md §7, ADR-0002). Producers append envelopes to the outbox
// inside their own Postgres transaction; the engine role's Relay publishes
// them in id order with Nats-Msg-Id deduplication. The envelope and the
// subject layout are external contracts: fields are only ever added, and a
// breaking change bumps SchemaVersion and adds a new schema file.
package eventbus

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// SubjectPrefix is the first two tokens of every subject.
const SubjectPrefix = "ex.v1"

// Envelope is the wire form of every event (docs/plan-v1.0.md §7.1).
//
// Seq is the per-market engine sequence for market-domain events (order.*,
// trade.*); AccountSeq is the per-account sequence for account-domain
// events (order.*, balance.*). Clients order by these, never by receipt.
type Envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	SchemaVersion int             `json:"schema_version"`
	TenantID      string          `json:"tenant_id"`
	MarketID      *string         `json:"market_id"`
	AccountID     *string         `json:"account_id"`
	Seq           *uint64         `json:"seq"`
	AccountSeq    *int64          `json:"account_seq,omitempty"`
	OccurredAt    time.Time       `json:"occurred_at"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	CausationID   string          `json:"causation_id,omitempty"`
	Payload       json.RawMessage `json:"payload"`
}

var (
	// ErrInvalidEnvelope marks an envelope that cannot be appended.
	ErrInvalidEnvelope = errors.New("eventbus: invalid envelope")

	eventTypePattern = regexp.MustCompile(`^[a-z_]+\.[a-z_]+$`)
	scopePattern     = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// Validate checks the envelope against the contract: two-segment event type,
// tenant, occurred_at, JSON payload, and a scope token that fits one NATS
// subject segment.
func (e Envelope) Validate() error {
	switch {
	case e.EventID == "":
		return fmt.Errorf("%w: event_id required", ErrInvalidEnvelope)
	case !eventTypePattern.MatchString(e.EventType):
		return fmt.Errorf("%w: event_type %q must match %s", ErrInvalidEnvelope, e.EventType, eventTypePattern)
	case e.SchemaVersion < 1:
		return fmt.Errorf("%w: schema_version must be >= 1", ErrInvalidEnvelope)
	case e.TenantID == "" || !scopePattern.MatchString(e.TenantID):
		return fmt.Errorf("%w: tenant_id %q", ErrInvalidEnvelope, e.TenantID)
	case e.OccurredAt.IsZero():
		return fmt.Errorf("%w: occurred_at required", ErrInvalidEnvelope)
	case len(e.Payload) == 0 || !json.Valid(e.Payload):
		return fmt.Errorf("%w: payload must be valid JSON", ErrInvalidEnvelope)
	}
	scope := e.Scope()
	if !scopePattern.MatchString(scope) {
		return fmt.Errorf("%w: scope %q is not a subject token", ErrInvalidEnvelope, scope)
	}
	return nil
}

// Domain is the part of the event type before the dot (order, trade, ...).
func (e Envelope) Domain() string {
	d, _, _ := strings.Cut(e.EventType, ".")
	return d
}

// Type is the part of the event type after the dot (accepted, executed, ...).
func (e Envelope) Type() string {
	_, t, _ := strings.Cut(e.EventType, ".")
	return t
}

// Scope is the last subject token: the market symbol for market-domain
// events, the account id for account-only events, "_" when neither applies.
func (e Envelope) Scope() string {
	switch {
	case e.MarketID != nil && *e.MarketID != "":
		return *e.MarketID
	case e.AccountID != nil && *e.AccountID != "":
		return *e.AccountID
	default:
		return "_"
	}
}

// Subject is ex.v1.<domain>.<type>.<tenant>.<scope> (docs/plan-v1.0.md §7.3).
func (e Envelope) Subject() string {
	return strings.Join([]string{SubjectPrefix, e.Domain(), e.Type(), e.TenantID, e.Scope()}, ".")
}

// Marshal serializes the envelope; the output is the JetStream message body
// and the webhook body.
func (e Envelope) Marshal() ([]byte, error) { return json.Marshal(e) }

// Unmarshal parses a serialized envelope.
func Unmarshal(b []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return Envelope{}, fmt.Errorf("eventbus: decode envelope: %w", err)
	}
	return e, nil
}

// idEntropy makes ULIDs monotonic within a millisecond so event ids sort in
// creation order even under load.
var (
	idMu      sync.Mutex
	idEntropy = ulid.Monotonic(rand.Reader, 0)
)

// NewID returns a fresh ULID (event ids, order ids, trade ids).
func NewID(now time.Time) string {
	idMu.Lock()
	defer idMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(now), idEntropy).String()
}

// Str returns a pointer to s, or nil for "" (nullable envelope fields).
func Str(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// U64 returns a pointer to v.
func U64(v uint64) *uint64 { return &v }

// I64 returns a pointer to v.
func I64(v int64) *int64 { return &v }
