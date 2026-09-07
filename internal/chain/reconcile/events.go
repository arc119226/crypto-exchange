package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// Event types this package publishes.
const (
	EventBreakDetected = "reconciliation.break_detected"
	EventHotWalletLow  = "alert.hot_wallet_low"
)

// schemaVersion of the payloads below (docs/plan-v1.0.md §7.1).
const schemaVersion = 1

// EventTypes lists what this package publishes. api/events/v1 must hold
// exactly one schema per entry; test/contract fails if they drift apart.
func EventTypes() []string {
	return []string{EventBreakDetected, EventHotWalletLow}
}

// BreakPayload is reconciliation.break_detected.
//
// Every term is carried, not just the difference: a consumer that only knows
// "ETH is 3.2 short" cannot tell an uncredited deposit from a missing key,
// and those two want very different people woken up.
type BreakPayload struct {
	ReportID      string       `json:"report_id"`
	ChainID       int64        `json:"chain_id"`
	Asset         string       `json:"asset"`
	BlockHeight   int64        `json:"block_height"`
	LedgerTotal   money.Amount `json:"ledger_total"`
	ChainTotal    money.Amount `json:"chain_total"`
	Uncredited    money.Amount `json:"uncredited"`
	AboveFrontier money.Amount `json:"above_frontier"`
	InFlight      money.Amount `json:"in_flight"`
	Diff          money.Amount `json:"diff"`
}

// HotWalletLowPayload is alert.hot_wallet_low.
type HotWalletLowPayload struct {
	ChainID   int64        `json:"chain_id"`
	Address   string       `json:"address"`
	Asset     string       `json:"asset"`
	Balance   money.Amount `json:"balance"`
	Threshold money.Amount `json:"threshold"`
}

// Event wraps a payload in an envelope. Neither of these is market- or
// account-scoped -- they are statements about the exchange's own holdings --
// so the subject's last token is the house scope "_".
func Event(eventType, tenant string, payload any, at time.Time) (eventbus.Envelope, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return eventbus.Envelope{}, fmt.Errorf("reconcile: marshal %s: %w", eventType, err)
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

func (w *Worker) emitBreak(ctx context.Context, tx pgx.Tx, reportID string, l Line) error {
	env, err := Event(EventBreakDetected, w.cfg.Tenant, BreakPayload{
		ReportID: reportID, ChainID: w.cfg.ChainID, Asset: l.Asset, BlockHeight: l.BlockHeight,
		LedgerTotal: l.LedgerTotal, ChainTotal: l.ChainTotal, Uncredited: l.Uncredited,
		AboveFrontier: l.AboveFrontier, InFlight: l.InFlight, Diff: l.Diff,
	}, time.Now().UTC())
	if err != nil {
		return err
	}
	if _, err := (eventbus.Outbox{}).Append(ctx, tx, env); err != nil {
		return fmt.Errorf("reconcile: outbox: %w", err)
	}
	return nil
}

// checkHotWallet fires alert.hot_wallet_low on the way down and clears the
// mark on the way back up (§6.4.3).
//
// Edge-triggered, and the edge is remembered in chain.hot_wallets rather than
// in this process: an alert that repeats every tick is one people filter out,
// and one that repeats after every restart is the same thing more slowly.
func (w *Worker) checkHotWallet(ctx context.Context, tx pgx.Tx, balance money.Amount) error {
	w.metrics.observeHotWallet(w.cfg.NativeAsset, balance)
	if !w.cfg.HotWalletMin.IsPositive() {
		return nil
	}
	q := sqlcgen.New(tx)
	row, err := q.GetHotWallet(ctx, sqlcgen.GetHotWalletParams{TenantID: w.cfg.Tenant, ChainID: w.cfg.ChainID})
	if err != nil {
		return fmt.Errorf("reconcile: hot wallet: %w", err)
	}
	low := balance.Cmp(w.cfg.HotWalletMin) < 0
	if low == row.LowAlertedAt.Valid {
		return nil // nothing crossed
	}
	var at pgtype.Timestamptz
	if low {
		at = pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true}
	}
	if err := q.SetHotWalletLowAlerted(ctx, sqlcgen.SetHotWalletLowAlertedParams{
		TenantID: w.cfg.Tenant, ChainID: w.cfg.ChainID, LowAlertedAt: at,
	}); err != nil {
		return fmt.Errorf("reconcile: mark hot wallet alerted: %w", err)
	}
	if !low {
		w.log.Info("hot wallet balance is back above the minimum",
			slog.String("balance", balance.String()), slog.String("minimum", w.cfg.HotWalletMin.String()))
		return nil
	}
	w.log.Error("hot wallet is below the minimum and will stop paying withdrawals",
		slog.String("address", row.Address), slog.String("balance", balance.String()),
		slog.String("minimum", w.cfg.HotWalletMin.String()))
	env, err := Event(EventHotWalletLow, w.cfg.Tenant, HotWalletLowPayload{
		ChainID: w.cfg.ChainID, Address: row.Address, Asset: w.cfg.NativeAsset,
		Balance: balance, Threshold: w.cfg.HotWalletMin,
	}, time.Now().UTC())
	if err != nil {
		return err
	}
	if _, err := (eventbus.Outbox{}).Append(ctx, tx, env); err != nil {
		return fmt.Errorf("reconcile: outbox: %w", err)
	}
	return nil
}

// gasCost is what a receipt says the transaction burned, in the native coin.
// The same computation the workers book with, so the correction and the entry
// it corrects for cannot disagree.
func gasCost(r *types.Receipt) money.Amount {
	if r == nil || r.EffectiveGasPrice == nil {
		return money.Zero
	}
	wei := new(big.Int).Mul(new(big.Int).SetUint64(r.GasUsed), r.EffectiveGasPrice)
	amount, err := evm.FromWei(wei, evm.MaxScale)
	if err != nil {
		return money.Zero
	}
	return amount
}
