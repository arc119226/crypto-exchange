package eventbus

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnvelopeSubjectAndValidation(t *testing.T) {
	ts := time.Date(2026, 9, 6, 8, 15, 23, 412_000_000, time.UTC)
	e := Envelope{
		EventID: "01J8Z2K3M4N5P6Q7R8S9T0V1W2", EventType: "trade.executed", SchemaVersion: 1, TenantID: "default",
		MarketID: Str("ETH-USDC"), Seq: U64(18234), OccurredAt: ts, CorrelationID: "req-1",
		Payload: json.RawMessage(`{"trade_id":"t1","price":"1990.00"}`),
	}
	require.NoError(t, e.Validate())
	assert.Equal(t, "ex.v1.trade.executed.default.ETH-USDC", e.Subject())
	assert.Equal(t, StreamTrading, StreamFor(e.Subject()))

	// account-only event scopes by account id; no market
	b := Envelope{
		EventID: "01J8Z2K3M4N5P6Q7R8S9T0V1W3", EventType: "balance.updated", SchemaVersion: 1, TenantID: "default",
		AccountID: Str("0f3f1c8e-6c4b-4b9e-9a4b-1f3d3c1a2b3c"), AccountSeq: I64(7), OccurredAt: ts, Payload: json.RawMessage(`{}`),
	}
	require.NoError(t, b.Validate())
	assert.Equal(t, "ex.v1.balance.updated.default.0f3f1c8e-6c4b-4b9e-9a4b-1f3d3c1a2b3c", b.Subject())

	// serialized form is the external contract: fixed key set and order
	body, err := e.Marshal()
	require.NoError(t, err)
	assert.JSONEq(t, `{
	  "event_id":"01J8Z2K3M4N5P6Q7R8S9T0V1W2","event_type":"trade.executed","schema_version":1,"tenant_id":"default",
	  "market_id":"ETH-USDC","account_id":null,"seq":18234,"occurred_at":"2026-09-06T08:15:23.412Z","correlation_id":"req-1",
	  "payload":{"trade_id":"t1","price":"1990.00"}}`, string(body))
	back, err := Unmarshal(body)
	require.NoError(t, err)
	assert.Equal(t, e.EventID, back.EventID)
	assert.Equal(t, uint64(18234), *back.Seq)
	assert.Nil(t, back.AccountID)

	for name, bad := range map[string]Envelope{
		"three segments": {EventID: "x", EventType: "order.a.b", SchemaVersion: 1, TenantID: "default", OccurredAt: ts, Payload: json.RawMessage(`{}`)},
		"upper case":     {EventID: "x", EventType: "Order.accepted", SchemaVersion: 1, TenantID: "default", OccurredAt: ts, Payload: json.RawMessage(`{}`)},
		"no payload":     {EventID: "x", EventType: "order.accepted", SchemaVersion: 1, TenantID: "default", OccurredAt: ts},
		"bad json":       {EventID: "x", EventType: "order.accepted", SchemaVersion: 1, TenantID: "default", OccurredAt: ts, Payload: json.RawMessage(`{`)},
		"scope with dot": {EventID: "x", EventType: "order.accepted", SchemaVersion: 1, TenantID: "default", MarketID: Str("ETH.USDC"), OccurredAt: ts, Payload: json.RawMessage(`{}`)},
		"no id":          {EventType: "order.accepted", SchemaVersion: 1, TenantID: "default", OccurredAt: ts, Payload: json.RawMessage(`{}`)},
		"version 0":      {EventID: "x", EventType: "order.accepted", TenantID: "default", OccurredAt: ts, Payload: json.RawMessage(`{}`)},
	} {
		assert.ErrorIs(t, bad.Validate(), ErrInvalidEnvelope, name)
	}
}

func TestStreamConfigsCoverCatalogDomains(t *testing.T) {
	for domain, stream := range map[string]string{
		"order": StreamTrading, "trade": StreamTrading, "ledger": StreamTrading, "balance": StreamTrading,
		"deposit": StreamChain, "withdrawal": StreamChain, "sweep": StreamChain, "alert": StreamChain,
		"market": StreamRegistry, "asset": StreamRegistry, "fee_schedule": StreamRegistry, "user": StreamRegistry, "reconciliation": StreamRegistry,
	} {
		assert.Equal(t, stream, StreamFor("ex.v1."+domain+".x.default.scope"), domain)
	}
	assert.Equal(t, "", StreamFor("ex.v1.unknown.x.default.scope"))
	assert.Equal(t, "", StreamFor("other.subject"))
	for _, cfg := range StreamConfigs() {
		assert.Equal(t, DuplicateWindow, cfg.Duplicates, cfg.Name)
		assert.Positive(t, cfg.MaxAge, cfg.Name)
	}
}

func TestNewIDIsMonotonic(t *testing.T) {
	now := time.Now()
	prev := NewID(now)
	for i := 0; i < 1000; i++ {
		id := NewID(now)
		require.Len(t, id, 26)
		require.Greater(t, id, prev)
		prev = id
	}
}
