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
func EventTypes() []string {
	return []string{EventMarketUpdated, EventAssetUpdated, EventFeeScheduleUpdated, EventRegistryReload}
}

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

// The other registry events. All three share market.updated's shape of
// subject: no account and no market scope for an asset or a fee schedule
// (the house scope "_"), and the engine reloads its whole cache on any of
// them, so which one it was only matters to a consumer that displays it.
const (
	EventAssetUpdated       = "asset.updated"
	EventFeeScheduleUpdated = "fee_schedule.updated"
	// EventRegistryReload is an operator asking every engine to reload now,
	// with no row changed: after a seed, or when in doubt.
	EventRegistryReload = "registry.reload"
)

// AssetUpdatedPayload is asset.updated.
type AssetUpdatedPayload struct {
	AssetID         string   `json:"asset_id"`
	Symbol          string   `json:"symbol"`
	Status          string   `json:"status"`
	DepositEnabled  bool     `json:"deposit_enabled"`
	WithdrawEnabled bool     `json:"withdraw_enabled"`
	ChangedFields   []string `json:"changed_fields"`
	Version         int32    `json:"version"`
	Reason          string   `json:"reason,omitempty"`
}

// FeeScheduleUpdatedPayload is fee_schedule.updated. Every market on the
// schedule pays the new rates from the next trade; no market.updated is
// sent for them, so a consumer that caches markets should reload on this.
type FeeScheduleUpdatedPayload struct {
	FeeScheduleID string   `json:"fee_schedule_id"`
	Name          string   `json:"name"`
	MakerBps      int32    `json:"maker_bps"`
	TakerBps      int32    `json:"taker_bps"`
	ChangedFields []string `json:"changed_fields"`
	Version       int32    `json:"version"`
	Reason        string   `json:"reason,omitempty"`
}

// RegistryReloadPayload is registry.reload.
type RegistryReloadPayload struct {
	Reason string `json:"reason,omitempty"`
}

// AssetUpdatedEvent builds the envelope for an asset change.
func AssetUpdatedEvent(tenantID string, a Asset, changed []string, reason string, now time.Time) (eventbus.Envelope, error) {
	return registryEvent(EventAssetUpdated, tenantID, now, AssetUpdatedPayload{
		AssetID: a.ID, Symbol: a.Symbol, Status: a.Status, DepositEnabled: a.DepositEnabled, WithdrawEnabled: a.WithdrawEnabled,
		ChangedFields: changed, Version: a.Version, Reason: reason,
	})
}

// FeeScheduleUpdatedEvent builds the envelope for a fee schedule change.
func FeeScheduleUpdatedEvent(tenantID string, f FeeSchedule, changed []string, reason string, now time.Time) (eventbus.Envelope, error) {
	return registryEvent(EventFeeScheduleUpdated, tenantID, now, FeeScheduleUpdatedPayload{
		FeeScheduleID: f.ID, Name: f.Name, MakerBps: f.MakerBps, TakerBps: f.TakerBps,
		ChangedFields: changed, Version: f.Version, Reason: reason,
	})
}

// RegistryReloadEvent builds the envelope for a reload request.
func RegistryReloadEvent(tenantID, reason string, now time.Time) (eventbus.Envelope, error) {
	return registryEvent(EventRegistryReload, tenantID, now, RegistryReloadPayload{Reason: reason})
}

func registryEvent(eventType, tenantID string, now time.Time, payload any) (eventbus.Envelope, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return eventbus.Envelope{}, fmt.Errorf("registry: encode %s: %w", eventType, err)
	}
	return eventbus.Envelope{
		EventID: eventbus.NewID(now), EventType: eventType, SchemaVersion: schemaVersion,
		TenantID: tenantID, OccurredAt: now, Payload: body,
	}, nil
}
