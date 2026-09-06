package eventbus

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/eventbus/sqlcgen"
)

// Outbox appends envelopes to eventbus.outbox inside the caller's
// transaction, so an event exists exactly when the state change it
// describes is committed.
type Outbox struct{}

// Append writes one envelope. The row id is returned for tests and logs; the
// envelope's EventID is what consumers deduplicate on.
func (Outbox) Append(ctx context.Context, tx pgx.Tx, e Envelope) (int64, error) {
	if err := e.Validate(); err != nil {
		return 0, err
	}
	headers, err := json.Marshal(map[string]string{"Correlation-Id": e.CorrelationID})
	if err != nil {
		return 0, fmt.Errorf("eventbus: headers: %w", err)
	}
	var seq *int64
	if e.Seq != nil {
		v := int64(*e.Seq) //nolint:gosec // engine seq is far below 2^63
		seq = &v
	}
	id, err := sqlcgen.New(tx).InsertOutbox(ctx, sqlcgen.InsertOutboxParams{
		EventID: e.EventID, EventType: e.EventType, SchemaVersion: int32(e.SchemaVersion), //nolint:gosec // validated >= 1, small
		TenantID: e.TenantID, MarketID: e.MarketID, AccountID: e.AccountID, Seq: seq, AccountSeq: e.AccountSeq,
		Subject: e.Subject(), Headers: headers, Payload: e.Payload, OccurredAt: e.OccurredAt,
		CorrelationID: Str(e.CorrelationID), CausationID: Str(e.CausationID),
	})
	if err != nil {
		return 0, fmt.Errorf("eventbus: append outbox: %w", err)
	}
	return id, nil
}

// AppendAll writes several envelopes in order.
func (o Outbox) AppendAll(ctx context.Context, tx pgx.Tx, es []Envelope) error {
	for _, e := range es {
		if _, err := o.Append(ctx, tx, e); err != nil {
			return err
		}
	}
	return nil
}

// MarkProcessed records that consumer handled event id inside tx. It
// returns false when the event was already recorded: the caller must then
// skip its side effects and ack (docs/plan-v1.0.md §7.3, processing consumers).
func MarkProcessed(ctx context.Context, tx pgx.Tx, consumer, eventID string) (bool, error) {
	n, err := sqlcgen.New(tx).MarkProcessed(ctx, sqlcgen.MarkProcessedParams{Consumer: consumer, EventID: eventID})
	if err != nil {
		return false, fmt.Errorf("eventbus: mark processed: %w", err)
	}
	return n == 1, nil
}

// envelopeFromRow rebuilds an envelope from an outbox row.
func envelopeFromRow(r sqlcgen.EventbusOutbox) Envelope {
	e := Envelope{
		EventID: r.EventID, EventType: r.EventType, SchemaVersion: int(r.SchemaVersion), TenantID: r.TenantID,
		MarketID: r.MarketID, AccountID: r.AccountID, AccountSeq: r.AccountSeq, OccurredAt: r.OccurredAt,
		Payload: json.RawMessage(r.Payload),
	}
	if r.Seq != nil {
		e.Seq = U64(uint64(*r.Seq)) //nolint:gosec // stored from a uint64
	}
	if r.CorrelationID != nil {
		e.CorrelationID = *r.CorrelationID
	}
	if r.CausationID != nil {
		e.CausationID = *r.CausationID
	}
	return e
}
