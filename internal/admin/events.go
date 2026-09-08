package admin

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// EventLedgerBreakDetected is the ledger failing to balance with itself:
// one asset whose debits and credits disagree (admin.ledger_breaks). The
// admin role finds it on its trial-balance loop, which is why it is this
// package's event and not the ledger's: the ledger keeps no outbox
// dependency, and the chain role -- which publishes the other break -- may
// be down or nodeless when the books go wrong, which is when this matters.
const EventLedgerBreakDetected = "reconciliation.ledger_break_detected"

// schemaVersion of the payload below.
const schemaVersion = 1

// EventTypes lists what this package publishes (see trading.EventTypes).
func EventTypes() []string { return []string{EventLedgerBreakDetected} }

// LedgerBreakPayload is reconciliation.ledger_break_detected. Edge-triggered
// like reconciliation.break_detected: sent when a break appears or its size
// changes, never for one that merely stays open.
type LedgerBreakPayload struct {
	BreakID    string       `json:"break_id"`
	Asset      string       `json:"asset"`
	Debits     money.Amount `json:"debits"`
	Credits    money.Amount `json:"credits"`
	Diff       money.Amount `json:"diff"`
	DetectedAt time.Time    `json:"detected_at"`
}

// LedgerBreakEvent builds the envelope for a break. A statement about the
// exchange's own books, scoped to no market and no account: the house scope.
func LedgerBreakEvent(tenantID string, b ledger.Break, now time.Time) (eventbus.Envelope, error) {
	body, err := json.Marshal(LedgerBreakPayload{
		BreakID: b.ID, Asset: b.Asset, Debits: b.Debits, Credits: b.Credits, Diff: b.Diff, DetectedAt: b.DetectedAt,
	})
	if err != nil {
		return eventbus.Envelope{}, fmt.Errorf("admin: encode %s: %w", EventLedgerBreakDetected, err)
	}
	return eventbus.Envelope{
		EventID: eventbus.NewID(now), EventType: EventLedgerBreakDetected, SchemaVersion: schemaVersion,
		TenantID: tenantID, OccurredAt: now, Payload: body,
	}, nil
}
