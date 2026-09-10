package withdrawal

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// Event types (docs/plan-v1.0.md §6.4.2, §7.2).
//
// §6.4.2 asks for one event per transition. Two types carry that: requested
// marks the row coming into existence, state_changed carries every move after
// it, with the states in the payload. A separate type per state would multiply
// the contract by the length of a state machine that is still growing — the
// signer adds three more states — and a consumer that cares about withdrawals
// subscribes to all of them anyway.
const (
	EventRequested    = "withdrawal.requested"
	EventStateChanged = "withdrawal.state_changed"
)

// schemaVersion of the payload below (docs/plan-v1.0.md §7.1).
const schemaVersion = 1

// EventTypes lists what this package publishes. api/events/v1 must hold
// exactly one schema per entry; test/contract fails if they drift apart.
func EventTypes() []string {
	return []string{EventRequested, EventStateChanged}
}

// Payload is the body of both events.
type Payload struct {
	WithdrawalID string       `json:"withdrawal_id"`
	AccountID    string       `json:"account_id"`
	Asset        string       `json:"asset"`
	Amount       money.Amount `json:"amount"`
	// Fee is what the account is charged on top of Amount, quoted when the
	// request was accepted and never recomputed. It is carried on every
	// event rather than only the terminal one because a consumer reading
	// state_changed alone must be able to tell what the withdrawal costs.
	Fee       money.Amount `json:"fee"`
	FeeAsset  string       `json:"fee_asset"`
	ToAddress string       `json:"to_address"`
	ChainID   int64        `json:"chain_id"`
	Status    string       `json:"status"`
	// PreviousStatus is empty on withdrawal.requested. On state_changed it is
	// what the row moved away from, so a consumer can tell an approval from a
	// re-delivery without keeping its own copy of the machine.
	PreviousStatus string `json:"previous_status,omitempty"`
	// Reason explains a rejection, a review or a failure; empty otherwise.
	Reason string `json:"reason,omitempty"`
	// TxHash is the transaction now representing this withdrawal, once one
	// exists. It is the displacement's hash while a cancellation is in
	// flight, because that is the transaction whose fate decides the money.
	TxHash string `json:"tx_hash,omitempty"`
}

// Event wraps a payload in an envelope. Withdrawals are account-scoped, like
// deposits: account_seq, no market and no engine seq.
func Event(eventType, tenant string, p Payload, accountSeq int64, at time.Time) (eventbus.Envelope, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return eventbus.Envelope{}, fmt.Errorf("withdrawal: marshal %s: %w", eventType, err)
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
