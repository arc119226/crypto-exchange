package registry

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
)

// Event types produced by registry changes (docs/plan-v1.0.md §7.2).
const (
	EventMarketUpdated = "market.updated"
)

// schemaVersion of the payloads below.
const schemaVersion = 1

// EventTypes lists what registry changes publish (see trading.EventTypes).
func EventTypes() []string { return []string{EventMarketUpdated} }

// MarketUpdatedPayload is market.updated. ChangedFields names what the
// write touched so a consumer can tell a status change from a fee change
// without diffing; the engine reloads on any of them.
type MarketUpdatedPayload struct {
	MarketID      string   `json:"market_id"`
	Symbol        string   `json:"symbol"`
	Status        string   `json:"status"`
	ChangedFields []string `json:"changed_fields"`
	Version       int32    `json:"version"`
	Reason        string   `json:"reason,omitempty"`
}

// MarketUpdatedEvent builds the envelope for a market change. The scope
// token is the symbol, so a consumer can filter one market's changes.
func MarketUpdatedEvent(tenantID string, m Market, changed []string, reason string, now time.Time) (eventbus.Envelope, error) {
	payload, err := json.Marshal(MarketUpdatedPayload{
		MarketID: m.ID, Symbol: m.Symbol, Status: m.Status, ChangedFields: changed, Version: m.Version, Reason: reason,
	})
	if err != nil {
		return eventbus.Envelope{}, fmt.Errorf("registry: encode %s: %w", EventMarketUpdated, err)
	}
	symbol := m.Symbol
	return eventbus.Envelope{
		EventID: eventbus.NewID(now), EventType: EventMarketUpdated, SchemaVersion: schemaVersion,
		TenantID: tenantID, MarketID: &symbol, OccurredAt: now, Payload: payload,
	}, nil
}
