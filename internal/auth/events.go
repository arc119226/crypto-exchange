package auth

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
)

// Event types produced by administrator edits to users (docs/events.md).
// This package only builds the envelope; the admin write that appends it to
// the outbox lives in internal/admin, in the same transaction as the edit.
const (
	EventUserStatusUpdated   = "user.status_updated"
	EventUserKYCLevelUpdated = "user.kyc_level_updated"
)

// schemaVersion of the payloads below.
const schemaVersion = 1

// EventTypes lists what user edits publish (see trading.EventTypes).
func EventTypes() []string { return []string{EventUserStatusUpdated, EventUserKYCLevelUpdated} }

// UserStatusUpdatedPayload is user.status_updated. Both ends are named so a
// consumer can tell a freeze from a release without keeping state.
type UserStatusUpdatedPayload struct {
	UserID         string `json:"user_id"`
	Status         string `json:"status"`
	PreviousStatus string `json:"previous_status"`
	Version        int32  `json:"version"`
	Reason         string `json:"reason,omitempty"`
}

// UserKYCLevelUpdatedPayload is user.kyc_level_updated. The withdrawal
// policy reads the level on the next request; this tells a customer's
// system the limits it may quote have changed.
type UserKYCLevelUpdatedPayload struct {
	UserID           string `json:"user_id"`
	KYCLevel         int    `json:"kyc_level"`
	PreviousKYCLevel int    `json:"previous_kyc_level"`
	Version          int32  `json:"version"`
	Reason           string `json:"reason,omitempty"`
}

// UserStatusUpdatedEvent builds the envelope for a status change. No account
// and no sequence: a user is not an account, and the subject ends in the
// house scope like market.updated.
func UserStatusUpdatedEvent(tenantID string, before, after User, reason string, now time.Time) (eventbus.Envelope, error) {
	return userEvent(EventUserStatusUpdated, tenantID, now, UserStatusUpdatedPayload{
		UserID: after.ID, Status: after.Status, PreviousStatus: before.Status, Version: after.Version, Reason: reason,
	})
}

// UserKYCLevelUpdatedEvent builds the envelope for a KYC level change.
func UserKYCLevelUpdatedEvent(tenantID string, before, after User, reason string, now time.Time) (eventbus.Envelope, error) {
	return userEvent(EventUserKYCLevelUpdated, tenantID, now, UserKYCLevelUpdatedPayload{
		UserID: after.ID, KYCLevel: after.KYCLevel, PreviousKYCLevel: before.KYCLevel, Version: after.Version, Reason: reason,
	})
}

func userEvent(eventType, tenantID string, now time.Time, payload any) (eventbus.Envelope, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return eventbus.Envelope{}, fmt.Errorf("auth: encode %s: %w", eventType, err)
	}
	return eventbus.Envelope{
		EventID: eventbus.NewID(now), EventType: eventType, SchemaVersion: schemaVersion,
		TenantID: tenantID, OccurredAt: now, Payload: body,
	}, nil
}
