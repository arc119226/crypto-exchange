package withdrawal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/signer"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// gasForNativeTransfer is the intrinsic cost of a plain value transfer. A
// native withdrawal is exactly that, so it needs no estimate; a token
// withdrawal calls a contract and does.
const gasForNativeTransfer = 21000

// Chain is what the send half of the machine needs from a node. An interface,
// for the same reason the deposit scanner has one: replacement, receipt
// handling and the failure paths are the parts most likely to be wrong, and a
// scripted chain can produce each of them on demand.
type Chain interface {
	Head(ctx context.Context) (uint64, error)
	NonceAt(ctx context.Context, address common.Address) (uint64, error)
	SendRawTransaction(ctx context.Context, raw []byte) error
	SuggestFees(ctx context.Context) (evm.Fees, error)
	EstimateGas(ctx context.Context, from, to common.Address, value *big.Int, data []byte) (uint64, error)
	Receipt(ctx context.Context, txHash string) (*types.Receipt, error)
}

// Nonces is the hot wallet's nonce allocator (internal/chain/hotwallet).
type Nonces interface {
	HotWallet() common.Address
	Allocate(ctx context.Context, tx pgx.Tx) (uint64, error)
	Recycle(ctx context.Context, nonce uint64, reason string) error
}

// SendConfig tunes the send half (docs/plan-v1.0.md §6.4.2).
type SendConfig struct {
	ChainID int64
	// ReplaceAfter is how long a broadcast transaction may sit unmined before
	// it is re-sent with a higher fee. anvil mines instantly, so this is long
	// enough that a healthy chain never reaches it.
	ReplaceAfter time.Duration
	// MaxReplacements caps the fee bumps before a person is asked. Past it the
	// withdrawal stays broadcast and waits for an admin resolve, because
	// bidding against a stuck mempool forever is not a strategy.
	MaxReplacements int32
	// MaxFeePerGas is the operator's stop-loss on the fee market; zero means
	// no ceiling.
	MaxFeePerGas *big.Int
	// DefaultConfirmations applies to an asset whose registry row says 0.
	DefaultConfirmations int32
}

// Send advances every withdrawal in the second half of the machine: signing
// what is locked, broadcasting what is signed, and settling what is mined.
//
// Like Tick, each withdrawal is its own transaction, so one that fails does
// not hold up the rest and a crash leaves the others where they were.
func (w *Worker) Send(ctx context.Context) error {
	if w.chain == nil {
		return nil // no node configured: the policy half still runs
	}
	rows, err := sqlcgen.New(w.db).ClaimWithdrawals(ctx, sqlcgen.ClaimWithdrawalsParams{
		TenantID: w.cfg.Tenant,
		Statuses: []string{StatusFundsLocked, StatusSigned, StatusBroadcast},
		Limit:    w.cfg.Batch,
	})
	if err != nil {
		return fmt.Errorf("withdrawal: claim for send: %w", err)
	}
	var firstErr error
	for _, row := range rows {
		if err := w.advanceSend(ctx, row.ID); err != nil {
			w.log.Error("withdrawal send step failed",
				slog.String("withdrawal_id", row.ID), slog.String("err", err.Error()))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (w *Worker) advanceSend(ctx context.Context, id string) error {
	row, err := sqlcgen.New(w.db).GetWithdrawal(ctx, sqlcgen.GetWithdrawalParams{TenantID: w.cfg.Tenant, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("withdrawal: read %s: %w", id, err)
	}
	switch row.Status {
	case StatusFundsLocked:
		return w.sign(ctx, row)
	case StatusSigned:
		return w.broadcast(ctx, row)
	case StatusBroadcast:
		return w.track(ctx, row)
	default:
		return nil
	}
}

// sign allocates a nonce, asks the signer for the transaction, and stores the
// nonce, the raw transaction and the new state in one database transaction
// (docs/plan-v1.0.md §6.4.2).
//
// Signing itself happens outside that transaction, because it is a network
// call to another process and holding a row lock across it would serialise
// every withdrawal behind the slowest signer. The nonce is allocated inside,
// which is what makes the pairing safe: if the commit fails, the nonce is
// recycled and the signature -- which is recorded in the signer's own log --
// is simply never used.
func (w *Worker) sign(ctx context.Context, row sqlcgen.ChainWithdrawal) error {
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return fmt.Errorf("withdrawal: amount of %s: %w", row.ID, err)
	}
	asset, err := w.reg.GetAsset(ctx, w.cfg.Tenant, row.Asset)
	if err != nil {
		return fmt.Errorf("withdrawal: asset %s: %w", row.Asset, err)
	}
	fees, err := w.fees(ctx)
	if err != nil {
		return err
	}
	to := common.HexToAddress(row.ToAddress)
	gas, err := w.estimate(ctx, asset, to, amount)
	if err != nil {
		return err
	}

	// Allocate the nonce and pin it to this withdrawal in one committed
	// transaction, before anything is signed.
	//
	// The pinning is what makes a crash here survivable. Without it, a process
	// that died between signing and recording would come back, allocate a
	// *different* nonce, and ask the signer for attempt 0 again -- which the
	// signing log refuses, stranding the withdrawal forever. With the nonce on
	// the row, the retry asks for exactly the same intent and gets back the
	// transaction that already exists.
	nonce, err := w.pinNonce(ctx, row)
	if err != nil {
		return err
	}
	res, err := w.signer.Sign(ctx, signer.Request{
		// The attempt is the replacement counter, which resolve(retry)
		// advances: a retry must not collide with the signature that failed.
		Kind: signer.KindWithdrawal, RefID: row.ID, Attempt: row.Replacements, ChainID: row.ChainID,
		To: to, Asset: row.Asset, Value: amount, Nonce: nonce, Gas: gas,
		TipCap: fees.TipCap, FeeCap: fees.FeeCap,
	})
	if err != nil {
		// The signature was refused, so the nonce will never be spent by this
		// withdrawal. Give it back before anything later queues behind it.
		if rerr := w.nonces.Recycle(ctx, nonce, "broadcast_failed"); rerr != nil {
			w.log.Error("could not recycle a nonce after a refused signature",
				slog.Uint64("nonce", nonce), slog.String("err", rerr.Error()))
		}
		return fmt.Errorf("withdrawal: sign %s: %w", row.ID, err)
	}

	return inTx(ctx, w.db, func(tx pgx.Tx) error {
		signed, err := sqlcgen.New(tx).SignWithdrawal(ctx, sqlcgen.SignWithdrawalParams{
			TenantID: w.cfg.Tenant, ID: row.ID, Nonce: int64Ptr(nonce),
			RawTx: res.RawTx, TxHash: &res.TxHash,
		})
		if err != nil {
			return fmt.Errorf("withdrawal: record signature %s: %w", row.ID, err)
		}
		if err := w.audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorSystem, ActorID: "chain", Action: "withdrawal.signed",
			TargetType: "withdrawal", TargetID: row.ID,
			Before:        map[string]any{"status": row.Status},
			After:         map[string]any{"status": StatusSigned, "nonce": nonce, "tx_hash": res.TxHash},
			CorrelationID: deref(row.CorrelationID),
		}); err != nil {
			return fmt.Errorf("withdrawal: audit: %w", err)
		}
		w.log.Info("withdrawal signed",
			slog.String("withdrawal_id", row.ID), slog.Uint64("nonce", nonce),
			slog.String("tx_hash", res.TxHash))
		w.metrics.decided.WithLabelValues(row.Asset, StatusSigned).Inc()
		return emit(ctx, tx, w.ledger, w.cfg.Tenant, EventStateChanged, signed, row.Status, "")
	})
}

// pinNonce returns the nonce this withdrawal will use, allocating one only if
// it does not already have it.
func (w *Worker) pinNonce(ctx context.Context, row sqlcgen.ChainWithdrawal) (uint64, error) {
	if row.Nonce != nil {
		return uint64(*row.Nonce), nil //nolint:gosec // CHECKed >= 0
	}
	var nonce uint64
	err := inTx(ctx, w.db, func(tx pgx.Tx) error {
		n, err := w.nonces.Allocate(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := sqlcgen.New(tx).AllocateWithdrawalNonce(ctx, sqlcgen.AllocateWithdrawalNonceParams{
			TenantID: w.cfg.Tenant, ID: row.ID, Nonce: int64Ptr(n),
		}); err != nil {
			return fmt.Errorf("withdrawal: pin nonce on %s: %w", row.ID, err)
		}
		nonce = n
		return nil
	})
	return nonce, err
}

// broadcast sends the stored bytes and moves hold -> pending_withdrawal
// (§6.1.4 e).
//
// The posting and the state change are one transaction, and the send happens
// first: a transaction on the chain with no posting would be money that left
// without the ledger knowing, while a posting with no transaction is corrected
// by the next tick re-sending the same bytes.
func (w *Worker) broadcast(ctx context.Context, row sqlcgen.ChainWithdrawal) error {
	if row.RawTx == nil || row.TxHash == nil || row.Nonce == nil {
		return fmt.Errorf("withdrawal: %s is signed without a transaction", row.ID)
	}
	err := w.chain.SendRawTransaction(ctx, row.RawTx)
	switch {
	case err == nil, errors.Is(err, evm.ErrKnownTransaction):
		// Known means it is already in the mempool or already mined. Either
		// way the money is moving and this is a broadcast.
	default:
		return w.broadcastFailed(ctx, row, err)
	}
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return fmt.Errorf("withdrawal: amount of %s: %w", row.ID, err)
	}
	pending, err := w.ledger.HouseAccount(ledger.HousePendingWithdrawal)
	if err != nil {
		return err
	}
	return inTx(ctx, w.db, func(tx pgx.Tx) error {
		if _, _, err := w.ledger.Post(ctx, tx, ledger.Entry{
			IdempotencyKey: "withdrawal:broadcast:" + row.ID, Kind: "withdrawal",
			RefType: "withdrawal", RefID: row.ID, Reason: "broadcast to the chain",
			CorrelationID: deref(row.CorrelationID),
			Postings: []ledger.Posting{
				{AccountID: row.AccountID, Asset: row.Asset, Bucket: ledger.BucketHold, Direction: ledger.Debit, Amount: amount},
				{AccountID: pending, Asset: row.Asset, Bucket: ledger.BucketHouse, Direction: ledger.Credit, Amount: amount},
			},
		}); err != nil {
			return fmt.Errorf("withdrawal: post broadcast %s: %w", row.ID, err)
		}
		updated, err := sqlcgen.New(tx).MarkWithdrawalBroadcast(ctx, sqlcgen.MarkWithdrawalBroadcastParams{
			TenantID: w.cfg.Tenant, ID: row.ID,
		})
		if err != nil {
			return fmt.Errorf("withdrawal: mark broadcast %s: %w", row.ID, err)
		}
		if err := w.audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorSystem, ActorID: "chain", Action: "withdrawal.broadcast",
			TargetType: "withdrawal", TargetID: row.ID,
			Before:        map[string]any{"status": row.Status},
			After:         map[string]any{"status": StatusBroadcast, "tx_hash": *row.TxHash},
			CorrelationID: deref(row.CorrelationID),
		}); err != nil {
			return fmt.Errorf("withdrawal: audit: %w", err)
		}
		w.log.Info("withdrawal broadcast",
			slog.String("withdrawal_id", row.ID), slog.String("tx_hash", *row.TxHash),
			slog.Uint64("nonce", uint64(*row.Nonce))) //nolint:gosec // CHECKed >= 0
		w.metrics.decided.WithLabelValues(row.Asset, StatusBroadcast).Inc()
		return emit(ctx, tx, w.ledger, w.cfg.Tenant, EventStateChanged, updated, row.Status, "")
	})
}

// broadcastFailed handles a send the node refused for a reason that is not
// "already known" (§6.4.2 failed(broadcast)).
//
// The funds go back to available and the nonce is recycled, but only once the
// chain confirms the transaction is not mined. Releasing a withdrawal whose
// transaction is actually on its way would credit a user for money that has
// already left.
func (w *Worker) broadcastFailed(ctx context.Context, row sqlcgen.ChainWithdrawal, cause error) error {
	nonce := uint64(*row.Nonce) //nolint:gosec // CHECKed >= 0
	mined, err := w.chain.NonceAt(ctx, w.nonces.HotWallet())
	if err != nil {
		return fmt.Errorf("withdrawal: broadcast %s failed (%v) and the chain could not be checked: %w", row.ID, cause, err)
	}
	if mined > nonce {
		// The nonce is spent, so something with this nonce is mined. Leave the
		// withdrawal alone; the tracker will find the receipt.
		w.log.Warn("broadcast was refused but the nonce is already used; leaving it to the tracker",
			slog.String("withdrawal_id", row.ID), slog.Uint64("nonce", nonce), slog.String("err", cause.Error()))
		return nil
	}
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return err
	}
	if err := inTx(ctx, w.db, func(tx pgx.Tx) error {
		if _, _, err := w.ledger.Release(ctx, tx, ledger.HoldParams{
			AccountID: row.AccountID, Asset: row.Asset, Amount: amount,
			IdempotencyKey: "release:withdrawal:" + row.ID,
			Ref:            ledger.Ref{Type: "withdrawal", ID: row.ID},
			CorrelationID:  deref(row.CorrelationID),
		}); err != nil {
			return fmt.Errorf("withdrawal: release %s: %w", row.ID, err)
		}
		return w.failTx(ctx, tx, row, FailureBroadcast)
	}); err != nil {
		return err
	}
	// Recycled after the release commits: a nonce given back for a withdrawal
	// still holding funds would be reused while the first transaction could
	// still be revived.
	if err := w.nonces.Recycle(ctx, nonce, "broadcast_failed"); err != nil {
		return fmt.Errorf("withdrawal: recycle nonce %d: %w", nonce, err)
	}
	w.log.Warn("withdrawal broadcast failed and the funds were released",
		slog.String("withdrawal_id", row.ID), slog.String("err", cause.Error()))
	return nil
}

// track polls for the receipt of a broadcast withdrawal (§6.4.2).
func (w *Worker) track(ctx context.Context, row sqlcgen.ChainWithdrawal) error {
	if row.TxHash == nil {
		return fmt.Errorf("withdrawal: %s is broadcast without a hash", row.ID)
	}
	// A cancellation in flight changes what "mined" means for this withdrawal:
	// the answer is whichever of the two transactions takes the nonce. Watch
	// the displacement, and fall back to the original when the node says the
	// displacement is not there — the original may have won.
	watch := *row.TxHash
	cancelling := row.CancelTxHash != nil
	if cancelling {
		watch = *row.CancelTxHash
	}
	receipt, err := w.chain.Receipt(ctx, watch)
	if cancelling && errors.Is(err, evm.ErrNotFound) {
		receipt, err = w.chain.Receipt(ctx, *row.TxHash)
		cancelling = false
	}
	if errors.Is(err, evm.ErrNotFound) {
		if row.CancelTxHash != nil {
			return nil // both are still in flight; wait rather than bid again
		}
		return w.maybeReplace(ctx, row)
	}
	if err != nil {
		return fmt.Errorf("withdrawal: receipt of %s: %w", row.ID, err)
	}
	head, err := w.chain.Head(ctx)
	if err != nil {
		return err
	}
	block := receipt.BlockNumber.Uint64()
	if head < block {
		return nil // the node's head is behind its own receipt; try again
	}
	confirmations := int32(head - block + 1) //nolint:gosec // bounded by the head
	required := w.requiredConfirmations(ctx, row.Asset)
	if confirmations < required {
		return nil
	}
	gas := gasCost(receipt)
	if cancelling {
		// The displacement won the nonce, so the withdrawal never happened.
		return w.settleCancelled(ctx, row, gas)
	}
	if receipt.Status == types.ReceiptStatusSuccessful {
		return w.confirm(ctx, row, block, gas)
	}
	return w.failedOnChain(ctx, row, block, gas)
}

// confirm settles a mined withdrawal: the money leaves pending_withdrawal for
// the hot wallet's custody, and the gas is booked as an expense (§6.1.4 e).
//
// Two entries, not one. The asset entry is in the withdrawal's own asset; the
// gas entry is always in the chain's native coin, so a USDC withdrawal
// produces one USDC entry and one ETH entry. Merging them would make a single
// entry that does not balance per asset.
func (w *Worker) confirm(ctx context.Context, row sqlcgen.ChainWithdrawal, block uint64, gas money.Amount) error {
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return err
	}
	pending, err := w.ledger.HouseAccount(ledger.HousePendingWithdrawal)
	if err != nil {
		return err
	}
	hot, err := w.ledger.HouseAccount(ledger.HouseCustodyHot)
	if err != nil {
		return err
	}
	gasAccount, err := w.ledger.HouseAccount(ledger.HouseGasExpense)
	if err != nil {
		return err
	}
	return inTx(ctx, w.db, func(tx pgx.Tx) error {
		if _, _, err := w.ledger.Post(ctx, tx, ledger.Entry{
			IdempotencyKey: "withdrawal:confirm:" + row.ID, Kind: "withdrawal",
			RefType: "withdrawal", RefID: row.ID, Reason: "confirmed on chain",
			CorrelationID: deref(row.CorrelationID),
			Postings: []ledger.Posting{
				{AccountID: pending, Asset: row.Asset, Bucket: ledger.BucketHouse, Direction: ledger.Debit, Amount: amount},
				{AccountID: hot, Asset: row.Asset, Bucket: ledger.BucketHouse, Direction: ledger.Credit, Amount: amount},
			},
		}); err != nil {
			return fmt.Errorf("withdrawal: post confirm %s: %w", row.ID, err)
		}
		if err := w.postGas(ctx, tx, row, gasAccount, hot, gas); err != nil {
			return err
		}
		updated, err := sqlcgen.New(tx).MarkWithdrawalConfirmed(ctx, sqlcgen.MarkWithdrawalConfirmedParams{
			TenantID: w.cfg.Tenant, ID: row.ID, BlockNumber: int64Ptr(block),
			GasCost: pg.NumericFromAmount(gas),
		})
		if err != nil {
			return fmt.Errorf("withdrawal: mark confirmed %s: %w", row.ID, err)
		}
		if err := w.audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorSystem, ActorID: "chain", Action: "withdrawal.confirmed",
			TargetType: "withdrawal", TargetID: row.ID,
			Before:        map[string]any{"status": row.Status},
			After:         map[string]any{"status": StatusConfirmed, "block_number": block, "gas_cost": gas.String()},
			CorrelationID: deref(row.CorrelationID),
		}); err != nil {
			return fmt.Errorf("withdrawal: audit: %w", err)
		}
		w.log.Info("withdrawal confirmed",
			slog.String("withdrawal_id", row.ID), slog.String("tx_hash", deref(row.TxHash)),
			slog.Uint64("block", block), slog.String("gas", gas.String()))
		w.metrics.decided.WithLabelValues(row.Asset, StatusConfirmed).Inc()
		return emit(ctx, tx, w.ledger, w.cfg.Tenant, EventStateChanged, updated, row.Status, "")
	})
}

// failedOnChain records a transaction that was mined and reverted (§6.4.2).
//
// The gas is booked because it was really spent. The withdrawal amount stays
// in pending_withdrawal on purpose: the exchange no longer owes it as an
// available balance and has not paid it out either, and which of those it
// becomes is a decision for a person through admin resolve.
func (w *Worker) failedOnChain(ctx context.Context, row sqlcgen.ChainWithdrawal, block uint64, gas money.Amount) error {
	hot, err := w.ledger.HouseAccount(ledger.HouseCustodyHot)
	if err != nil {
		return err
	}
	gasAccount, err := w.ledger.HouseAccount(ledger.HouseGasExpense)
	if err != nil {
		return err
	}
	return inTx(ctx, w.db, func(tx pgx.Tx) error {
		if err := w.postGas(ctx, tx, row, gasAccount, hot, gas); err != nil {
			return err
		}
		if _, err := sqlcgen.New(tx).RecordWithdrawalGas(ctx, sqlcgen.RecordWithdrawalGasParams{
			TenantID: w.cfg.Tenant, ID: row.ID, BlockNumber: int64Ptr(block),
			GasCost: pg.NumericFromAmount(gas),
		}); err != nil {
			return fmt.Errorf("withdrawal: record gas %s: %w", row.ID, err)
		}
		w.log.Error("withdrawal reverted on chain and needs a decision",
			slog.String("withdrawal_id", row.ID), slog.String("tx_hash", deref(row.TxHash)),
			slog.Uint64("block", block))
		return w.failTx(ctx, tx, row, FailureOnChain)
	})
}

// postGas books what the transaction actually cost. Gas is always paid in the
// chain's native coin, whatever the withdrawal moved.
func (w *Worker) postGas(ctx context.Context, tx pgx.Tx, row sqlcgen.ChainWithdrawal, gasAccount, hot string, gas money.Amount) error {
	if !gas.IsPositive() {
		return nil // a scripted chain, or a receipt with no effective price
	}
	if _, _, err := w.ledger.Post(ctx, tx, ledger.Entry{
		IdempotencyKey: "withdrawal:gas:" + row.ID, Kind: "gas",
		RefType: "withdrawal", RefID: row.ID, Reason: "withdrawal gas",
		CorrelationID: deref(row.CorrelationID),
		Postings: []ledger.Posting{
			{AccountID: gasAccount, Asset: w.cfg.NativeAsset, Bucket: ledger.BucketHouse, Direction: ledger.Debit, Amount: gas},
			{AccountID: hot, Asset: w.cfg.NativeAsset, Bucket: ledger.BucketHouse, Direction: ledger.Credit, Amount: gas},
		},
	}); err != nil {
		return fmt.Errorf("withdrawal: post gas %s: %w", row.ID, err)
	}
	return nil
}

// maybeReplace re-sends a transaction that has sat unmined too long (§6.4.2).
func (w *Worker) maybeReplace(ctx context.Context, row sqlcgen.ChainWithdrawal) error {
	if row.BroadcastAt.Valid && time.Since(row.BroadcastAt.Time) < w.send.ReplaceAfter {
		return nil
	}
	if row.Replacements >= w.send.MaxReplacements {
		// Bidding against a stuck mempool forever is not a strategy. The
		// withdrawal stays broadcast and waits for a person to decide between
		// another bump and cancelling the nonce.
		w.log.Error("withdrawal has exhausted its replacements and needs a decision",
			slog.String("withdrawal_id", row.ID), slog.Int("replacements", int(row.Replacements)),
			slog.String("tx_hash", deref(row.TxHash)))
		w.metrics.stuck.WithLabelValues(row.Asset).Inc()
		return nil
	}
	return w.replace(ctx, row, "unmined for longer than the replacement window")
}

// replace signs the same withdrawal again with a higher fee and the same
// nonce. Attempt is the replacement count, which is what the signing log's
// unique key uses to tell one bump from the next.
func (w *Worker) replace(ctx context.Context, row sqlcgen.ChainWithdrawal, reason string) error {
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return err
	}
	asset, err := w.reg.GetAsset(ctx, w.cfg.Tenant, row.Asset)
	if err != nil {
		return err
	}
	fees, err := w.fees(ctx)
	if err != nil {
		return err
	}
	// Bump from the current suggestion rather than from the original fees: a
	// market that has moved makes the old numbers irrelevant, and the protocol
	// only requires beating what was actually sent.
	fees = fees.Bump(int64(10 + 10*row.Replacements)).CapAt(w.send.MaxFeePerGas)
	to := common.HexToAddress(row.ToAddress)
	gas, err := w.estimate(ctx, asset, to, amount)
	if err != nil {
		return err
	}
	res, err := w.signer.Sign(ctx, signer.Request{
		Kind: signer.KindWithdrawal, RefID: row.ID, Attempt: row.Replacements + 1, ChainID: row.ChainID,
		To: to, Asset: row.Asset, Value: amount, Nonce: uint64(*row.Nonce), //nolint:gosec // CHECKed >= 0
		Gas: gas, TipCap: fees.TipCap, FeeCap: fees.FeeCap,
	})
	if err != nil {
		return fmt.Errorf("withdrawal: sign replacement for %s: %w", row.ID, err)
	}
	if err := w.chain.SendRawTransaction(ctx, res.RawTx); err != nil && !errors.Is(err, evm.ErrKnownTransaction) {
		return fmt.Errorf("withdrawal: send replacement for %s: %w", row.ID, err)
	}
	return inTx(ctx, w.db, func(tx pgx.Tx) error {
		updated, err := sqlcgen.New(tx).ReplaceWithdrawalTx(ctx, sqlcgen.ReplaceWithdrawalTxParams{
			TenantID: w.cfg.Tenant, ID: row.ID, RawTx: res.RawTx, TxHash: &res.TxHash,
		})
		if err != nil {
			return fmt.Errorf("withdrawal: record replacement %s: %w", row.ID, err)
		}
		if err := w.audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorSystem, ActorID: "chain", Action: "withdrawal.replaced",
			TargetType: "withdrawal", TargetID: row.ID,
			Before: map[string]any{"tx_hash": deref(row.TxHash), "replacements": row.Replacements},
			After: map[string]any{
				"tx_hash": res.TxHash, "replacements": updated.Replacements,
				"reason": reason, "fee_cap": fees.FeeCap.String(),
			},
			CorrelationID: deref(row.CorrelationID),
		}); err != nil {
			return fmt.Errorf("withdrawal: audit: %w", err)
		}
		w.log.Warn("withdrawal re-sent with a higher fee",
			slog.String("withdrawal_id", row.ID), slog.String("tx_hash", res.TxHash),
			slog.Int("replacement", int(updated.Replacements)), slog.String("reason", reason))
		w.metrics.replacements.WithLabelValues(row.Asset).Inc()
		return nil
	})
}

// failTx marks a withdrawal failed inside the caller's transaction.
func (w *Worker) failTx(ctx context.Context, tx pgx.Tx, row sqlcgen.ChainWithdrawal, reason string) error {
	updated, err := sqlcgen.New(tx).UpdateWithdrawalStatus(ctx, sqlcgen.UpdateWithdrawalStatusParams{
		TenantID: w.cfg.Tenant, ID: row.ID, Status: StatusFailed, FailureReason: &reason,
	})
	if err != nil {
		return fmt.Errorf("withdrawal: fail %s: %w", row.ID, err)
	}
	if err := w.audit.Record(ctx, tx, audit.Event{
		ActorType: audit.ActorSystem, ActorID: "chain", Action: "withdrawal.failed",
		TargetType: "withdrawal", TargetID: row.ID,
		Before:        map[string]any{"status": row.Status},
		After:         map[string]any{"status": StatusFailed, "failure_reason": reason},
		CorrelationID: deref(row.CorrelationID),
	}); err != nil {
		return fmt.Errorf("withdrawal: audit: %w", err)
	}
	w.metrics.decided.WithLabelValues(row.Asset, StatusFailed).Inc()
	return emit(ctx, tx, w.ledger, w.cfg.Tenant, EventStateChanged, updated, row.Status, reason)
}

// fees reads the current suggestion and applies the operator's ceiling.
func (w *Worker) fees(ctx context.Context) (evm.Fees, error) {
	f, err := w.chain.SuggestFees(ctx)
	if err != nil {
		return evm.Fees{}, fmt.Errorf("withdrawal: fees: %w", err)
	}
	return f.CapAt(w.send.MaxFeePerGas), nil
}

// estimate returns the gas limit. A native transfer is exactly the intrinsic
// cost, so it needs no round trip; a token call is asked of the node and given
// a fifth of headroom, because an estimate that is exactly right fails when
// the recipient's storage slot changes from zero to non-zero between the
// estimate and the mine.
func (w *Worker) estimate(ctx context.Context, asset registry.Asset, to common.Address, amount money.Amount) (uint64, error) {
	if asset.IsNative {
		return gasForNativeTransfer, nil
	}
	if asset.ContractAddress == nil {
		return 0, fmt.Errorf("withdrawal: %s has no contract address", asset.Symbol)
	}
	units, err := evm.ToWei(amount, asset.Scale)
	if err != nil {
		return 0, err
	}
	data, err := evm.TransferCalldata(to, units)
	if err != nil {
		return 0, err
	}
	gas, err := w.chain.EstimateGas(ctx, w.nonces.HotWallet(), common.HexToAddress(*asset.ContractAddress), new(big.Int), data)
	if err != nil {
		return 0, err
	}
	return gas + gas/5, nil
}

// requiredConfirmations is the asset's threshold, or the configured default
// when the registry says nothing.
func (w *Worker) requiredConfirmations(ctx context.Context, symbol string) int32 {
	asset, err := w.reg.GetAsset(ctx, w.cfg.Tenant, symbol)
	if err != nil || asset.RequiredConfirmations <= 0 {
		if w.send.DefaultConfirmations > 0 {
			return w.send.DefaultConfirmations
		}
		return 1
	}
	return asset.RequiredConfirmations
}

// gasCost is gas used × the price actually paid, in the native coin's own
// units. A receipt without an effective price (some scripted and older nodes)
// yields zero, which books no gas rather than an invented number.
func gasCost(r *types.Receipt) money.Amount {
	if r == nil || r.EffectiveGasPrice == nil || r.GasUsed == 0 {
		return money.Zero
	}
	wei := new(big.Int).Mul(new(big.Int).SetUint64(r.GasUsed), r.EffectiveGasPrice)
	amount, err := evm.FromWei(wei, 18)
	if err != nil {
		return money.Zero
	}
	return amount
}

func int64Ptr(v uint64) *int64 {
	n := int64(v) //nolint:gosec // nonces and block numbers are far below 2^63
	return &n
}
