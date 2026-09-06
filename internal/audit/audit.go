// Package audit is the append-only trail of privileged actions
// (docs/plan-v1.0.md §14): admin writes, auth events, withdrawal state
// changes, signing requests and registry changes. Writers INSERT inside the
// same transaction as the change they describe; nobody updates or deletes.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/arc119226/crypto-exchange/internal/audit/sqlcgen"
)

// ActorType says who acted.
type ActorType string

// Actor types.
const (
	ActorUser   ActorType = "user"
	ActorAdmin  ActorType = "admin"
	ActorSystem ActorType = "system"
	ActorAPIKey ActorType = "api_key"
)

// Event is what a caller records. Before and After are marshalled to JSON
// (nil is stored as SQL NULL).
type Event struct {
	ActorType     ActorType
	ActorID       string
	Action        string // e.g. ledger.adjustment.create, account.status.update
	TargetType    string
	TargetID      string
	Before        any
	After         any
	IP            string
	CorrelationID string
}

// Record is a persisted audit event.
type Record struct {
	ID            int64
	TenantID      string
	ActorType     ActorType
	ActorID       string
	Action        string
	TargetType    string
	TargetID      string
	Before        json.RawMessage
	After         json.RawMessage
	IP            string
	CorrelationID string
	CreatedAt     time.Time
}

// Recorder writes and reads the audit trail of one tenant.
type Recorder struct {
	tenant string
}

// NewRecorder binds a Recorder to a tenant.
func NewRecorder(tenantID string) *Recorder { return &Recorder{tenant: tenantID} }

// Record appends one event using db (a pgx.Tx to make it atomic with the
// audited change, or a pool).
func (r *Recorder) Record(ctx context.Context, db sqlcgen.DBTX, e Event) (Record, error) {
	if e.Action == "" {
		return Record{}, fmt.Errorf("audit: action required")
	}
	switch e.ActorType {
	case ActorUser, ActorAdmin, ActorSystem, ActorAPIKey:
	default:
		return Record{}, fmt.Errorf("audit: unknown actor type %q", e.ActorType)
	}
	before, err := marshalNullable(e.Before)
	if err != nil {
		return Record{}, fmt.Errorf("audit: before: %w", err)
	}
	after, err := marshalNullable(e.After)
	if err != nil {
		return Record{}, fmt.Errorf("audit: after: %w", err)
	}
	row, err := sqlcgen.New(db).InsertAuditEvent(ctx, sqlcgen.InsertAuditEventParams{
		TenantID: r.tenant, ActorType: string(e.ActorType), ActorID: optString(e.ActorID), Action: e.Action,
		TargetType: optString(e.TargetType), TargetID: optString(e.TargetID), Before: before, After: after,
		Ip: optString(e.IP), CorrelationID: optString(e.CorrelationID),
	})
	if err != nil {
		return Record{}, fmt.Errorf("audit: insert: %w", err)
	}
	return recordFromRow(row), nil
}

// Filter selects audit events; empty strings match anything.
type Filter struct {
	Action     string
	TargetType string
	TargetID   string
	Limit      int32
	Offset     int32
}

// List returns events newest first.
func (r *Recorder) List(ctx context.Context, db sqlcgen.DBTX, f Filter) ([]Record, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	rows, err := sqlcgen.New(db).ListAuditEvents(ctx, sqlcgen.ListAuditEventsParams{
		TenantID: r.tenant, Action: f.Action, TargetType: f.TargetType, TargetID: f.TargetID, Limit: f.Limit, Offset: f.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("audit: list: %w", err)
	}
	out := make([]Record, 0, len(rows))
	for _, row := range rows {
		out = append(out, recordFromRow(row))
	}
	return out, nil
}

func recordFromRow(row sqlcgen.AuditAuditEvent) Record {
	return Record{
		ID: row.ID, TenantID: row.TenantID, ActorType: ActorType(row.ActorType), ActorID: deref(row.ActorID), Action: row.Action,
		TargetType: deref(row.TargetType), TargetID: deref(row.TargetID), Before: row.Before, After: row.After,
		IP: deref(row.Ip), CorrelationID: deref(row.CorrelationID), CreatedAt: row.CreatedAt,
	}
}

func marshalNullable(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
