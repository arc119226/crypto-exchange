package sweep

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// Event types (docs/plan-v1.0.md §7.2). Only the two ends are published: the
// intermediate states are the exchange rearranging its own custody, which no
// consumer outside the operator's dashboard has any interest in, and the
// audit trail already records them.
const (
	EventCompleted = "sweep.completed"
	EventFailed    = "sweep.failed"
)

// schemaVersion of the payload below (docs/plan-v1.0.md §7.1).
const schemaVersion = 1

// EventTypes lists what this package publishes. api/events/v1 must hold
// exactly one schema per entry; test/contract fails if they drift apart.
func EventTypes() []string {
	return []string{EventCompleted, EventFailed}
}

// Payload is the body of both events.
//
// There is no account_id, and that is the point: a sweep belongs to no user.
// It moves the exchange's own custody between two of its own accounts, and the
// user whose deposit address was emptied sees nothing change.
type Payload struct {
	SweepID     string       `json:"sweep_id"`
	ChainID     int64        `json:"chain_id"`
	FromAddress string       `json:"from_address"`
	Asset       string       `json:"asset"`
	Amount      money.Amount `json:"amount"`
	Status      string       `json:"status"`
	// TxHash is the sweep transaction; empty when it failed before one existed.
	TxHash string `json:"tx_hash,omitempty"`
	// GasCost is what the transaction burned, in the chain's native coin —
	// which is not necessarily Asset.
	GasCost money.Amount `json:"gas_cost"`
	// BlockNumber is set once the sweep is mined.
	BlockNumber int64 `json:"block_number,omitempty"`
	// Reason explains a failure; empty otherwise.
	Reason string `json:"reason,omitempty"`
}

// Event wraps a payload in an envelope. Sweeps are neither market- nor
// account-scoped, so the subject's last token is the house scope "_".
func Event(eventType, tenant string, p Payload, at time.Time) (eventbus.Envelope, error) {
	body, err := json.Marshal(p)
	if err != nil {
		return eventbus.Envelope{}, fmt.Errorf("sweep: marshal %s: %w", eventType, err)
	}
	env := eventbus.Envelope{
		EventID:       eventbus.NewID(at),
		EventType:     eventType,
		SchemaVersion: schemaVersion,
		TenantID:      tenant,
		OccurredAt:    at,
		Payload:       body,
	}
	if err := env.Validate(); err != nil {
		return eventbus.Envelope{}, err
	}
	return env, nil
}

// emit writes one event to the outbox inside the caller's transaction.
func (w *Worker) emit(ctx context.Context, tx pgx.Tx, row sqlcgen.ChainSweep, eventType, status, reason string, amount, gas money.Amount, block uint64) error {
	env, err := Event(eventType, w.cfg.Tenant, Payload{
		SweepID: row.ID, ChainID: row.ChainID, FromAddress: row.FromAddress,
		Asset: row.Asset, Amount: amount, Status: status, TxHash: deref(row.TxHash),
		GasCost: gas, BlockNumber: int64(block), Reason: reason, //nolint:gosec // block numbers are far below 2^63
	}, time.Now().UTC())
	if err != nil {
		return err
	}
	if _, err := (eventbus.Outbox{}).Append(ctx, tx, env); err != nil {
		return fmt.Errorf("sweep: outbox: %w", err)
	}
	return nil
}
