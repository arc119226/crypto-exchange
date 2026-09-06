// Package deposit detects incoming transfers on an EVM chain and credits them
// to the account that owns the receiving address (docs/plan-v1.0.md §6.4.1).
//
// It runs in the chain role and holds no keys: the addresses it watches were
// derived by the signer ahead of time and handed out by the api role.
package deposit

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// Event types produced by the scanner (docs/plan-v1.0.md §6.4.1, §7.2).
//
// deposit.dropped is missing from the §7.2 catalog but present in the state
// machine and in docs/events.md; the catalog is the stale one.
// deposit.confirmations_updated is deliberately not emitted: §6.4.1 marks it
// optional, and a message per confirmation per deposit is a lot of traffic for
// something a client can read from the deposit itself.
const (
	EventDetected = "deposit.detected"
	EventCredited = "deposit.credited"
	EventOrphaned = "deposit.orphaned"
	EventDropped  = "deposit.dropped"
	EventReversed = "deposit.reversed"
)

// schemaVersion of every payload below (docs/plan-v1.0.md §7.1).
const schemaVersion = 1

// EventTypes lists what the scanner publishes. api/events/v1 must hold exactly
// one schema per entry and test/contract fails if they drift apart: add a
// constant above, add it here, add its schema and its golden file.
func EventTypes() []string {
	return []string{EventDetected, EventCredited, EventOrphaned, EventDropped, EventReversed}
}

// Payload is the body of every deposit.* event. One shape for all five: a
// consumer that handles a deposit cares about the same fields whichever way it
// moved, and the differences live in the event type.
type Payload struct {
	DepositID     string       `json:"deposit_id"`
	AccountID     string       `json:"account_id"`
	Asset         string       `json:"asset"`
	Amount        money.Amount `json:"amount"`
	Address       string       `json:"address"`
	TxHash        string       `json:"tx_hash"`
	LogIndex      int32        `json:"log_index"`
	BlockNumber   uint64       `json:"block_number"`
	BlockHash     string       `json:"block_hash"`
	Confirmations int32        `json:"confirmations"`
	Status        string       `json:"status"`
	// Required is the asset's required_confirmations at the time, so a client
	// can render progress without reading the registry.
	Required int32 `json:"required_confirmations"`
}

// Event wraps a payload in an envelope. Deposits are account-scoped: they
// carry account_seq and no market or engine seq.
func Event(eventType, tenant string, p Payload, accountSeq int64, at time.Time) (eventbus.Envelope, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return eventbus.Envelope{}, fmt.Errorf("deposit: marshal %s: %w", eventType, err)
	}
	env := eventbus.Envelope{
		EventID:       eventbus.NewID(at),
		EventType:     eventType,
		SchemaVersion: schemaVersion,
		TenantID:      tenant,
		AccountID:     eventbus.Str(p.AccountID),
		AccountSeq:    eventbus.I64(accountSeq),
		OccurredAt:    at,
		Payload:       body,
	}
	if err := env.Validate(); err != nil {
		return eventbus.Envelope{}, err
	}
	return env, nil
}
