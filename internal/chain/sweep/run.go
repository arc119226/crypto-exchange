package sweep

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/signer"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// fundGas sends the deposit address enough ether to pay for its own transfer
// (docs/plan-v1.0.md §6.4.3, first leg).
//
// An address that has only ever received tokens holds no ether at all, so it
// cannot pay for anything — that is the whole reason a token sweep takes two
// transactions instead of one. This leg comes out of the hot wallet, so it
// takes a nonce from the managed allocator rather than from the node.
func (w *Worker) fundGas(ctx context.Context, row sqlcgen.ChainSweep, asset registry.Asset) error {
	gas, err := w.estimateTransfer(ctx, row, asset)
	if err != nil {
		return err
	}
	funding, err := w.gasBudget(ctx, gas)
	if err != nil {
		return err
	}
	// Whatever the address already holds counts towards it; funding is only
	// the shortfall. A second sweep of the same address usually needs nothing.
	held, err := w.chain.Balance(ctx, common.HexToAddress(row.FromAddress))
	if err != nil {
		return err
	}
	have, err := evm.FromWei(held, evm.MaxScale)
	if err != nil {
		return err
	}
	if have.Cmp(funding) >= 0 {
		// Already funded. Skip the leg entirely rather than send a zero
		// transfer: it moves nothing and costs 21000 gas to say so.
		//
		// Anything a previous attempt pinned has to go first. FundSweepGas
		// writes the amount and the nonce when it reserves the nonce, before
		// the signer is asked; if signing failed, this row still carries them
		// with no transaction behind them. Marking it funded with those left
		// in place would book ether the hot wallet never sent, and would
		// abandon a nonce nothing will ever spend.
		row, err = w.releasePinnedFunding(ctx, row)
		if err != nil {
			return err
		}
		w.log.Info("deposit address can already pay its own gas",
			slog.String("sweep_id", row.ID), slog.String("held", have.String()))
		return w.markGasFunded(ctx, row, money.Zero, nil)
	}
	funding = funding.Sub(have)

	nonce, raw, hash, err := w.signFunding(ctx, row, funding)
	if err != nil {
		return err
	}
	if err := w.chain.SendRawTransaction(ctx, raw); err != nil && !errors.Is(err, evm.ErrKnownTransaction) {
		// The funding never reached the chain, so the nonce it holds will
		// never be spent. Give it back before later transactions queue behind
		// a hole, then abandon the sweep: nothing has moved, and the next tick
		// plans a fresh one.
		if rerr := w.nonces.Recycle(ctx, nonce, "broadcast_failed"); rerr != nil {
			w.log.Error("could not recycle a nonce after a refused gas funding",
				slog.Uint64("nonce", nonce), slog.String("err", rerr.Error()))
		}
		w.log.Error("gas funding was refused",
			slog.String("sweep_id", row.ID), slog.String("err", err.Error()))
		return w.failFunding(ctx, row, money.Zero, nil)
	}
	w.log.Info("funding a deposit address so it can pay for its own sweep",
		slog.String("sweep_id", row.ID), slog.String("address", row.FromAddress),
		slog.String("amount", funding.String()), slog.String("tx_hash", hash))
	return nil
}

// releasePinnedFunding gives back a funding nonce and amount that were
// reserved but never turned into a transaction, and returns the cleaned row.
//
// A no-op on a row that has a funding hash: those columns describe a
// transaction that exists, and the ether it moved still has to be booked.
func (w *Worker) releasePinnedFunding(ctx context.Context, row sqlcgen.ChainSweep) (sqlcgen.ChainSweep, error) {
	if row.GasFundingTxHash != nil || (row.GasFundingNonce == nil && !row.GasFundingAmount.Valid) {
		return row, nil
	}
	if row.GasFundingNonce != nil {
		if err := w.nonces.Recycle(ctx, uint64(*row.GasFundingNonce), "broadcast_failed"); err != nil { //nolint:gosec // CHECKed >= 0
			return row, fmt.Errorf("sweep: recycle unsigned funding nonce on %s: %w", row.ID, err)
		}
	}
	cleaned, err := sqlcgen.New(w.db).ClearSweepGasFunding(ctx, sqlcgen.ClearSweepGasFundingParams{
		TenantID: w.cfg.Tenant, ID: row.ID,
	})
	if err != nil {
		return row, fmt.Errorf("sweep: clear pinned funding on %s: %w", row.ID, err)
	}
	return cleaned, nil
}

// signFunding pins the funding nonce, signs, and records both before the
// transaction is sent.
//
// The nonce is committed first for the reason 4b-2 established: a crash
// between signing and recording must come back asking for the same intent and
// get the same bytes, not allocate a second nonce for a transaction that may
// already be in flight.
func (w *Worker) signFunding(ctx context.Context, row sqlcgen.ChainSweep, funding money.Amount) (uint64, []byte, string, error) {
	if row.GasFundingRawTx != nil && row.GasFundingTxHash != nil && row.GasFundingNonce != nil {
		return uint64(*row.GasFundingNonce), row.GasFundingRawTx, *row.GasFundingTxHash, nil //nolint:gosec // CHECKed >= 0
	}
	var nonce uint64
	if row.GasFundingNonce != nil {
		nonce = uint64(*row.GasFundingNonce) //nolint:gosec // CHECKed >= 0
	} else {
		if err := inTx(ctx, w.db, func(tx pgx.Tx) error {
			n, err := w.nonces.Allocate(ctx, tx)
			if err != nil {
				return err
			}
			if _, err := sqlcgen.New(tx).FundSweepGas(ctx, sqlcgen.FundSweepGasParams{
				TenantID: w.cfg.Tenant, ID: row.ID, GasFundingNonce: int64Ptr(n),
				GasFundingAmount: pg.NumericFromAmount(funding),
			}); err != nil {
				return fmt.Errorf("sweep: pin funding nonce on %s: %w", row.ID, err)
			}
			nonce = n
			return nil
		}); err != nil {
			return 0, nil, "", err
		}
	}
	fees, err := w.fees(ctx)
	if err != nil {
		return 0, nil, "", err
	}
	res, err := w.signer.Sign(ctx, signer.Request{
		Kind: signer.KindGasFund, RefID: row.ID, ChainID: row.ChainID,
		To: common.HexToAddress(row.FromAddress), Asset: w.cfg.NativeAsset, Value: funding,
		Nonce: nonce, Gas: gasForNativeTransfer, TipCap: fees.TipCap, FeeCap: fees.FeeCap,
	})
	if err != nil {
		return 0, nil, "", fmt.Errorf("sweep: sign gas funding for %s: %w", row.ID, err)
	}
	if _, err := sqlcgen.New(w.db).FundSweepGas(ctx, sqlcgen.FundSweepGasParams{
		TenantID: w.cfg.Tenant, ID: row.ID, GasFundingNonce: int64Ptr(nonce),
		GasFundingRawTx: res.RawTx, GasFundingTxHash: &res.TxHash,
		GasFundingAmount: pg.NumericFromAmount(funding),
	}); err != nil {
		return 0, nil, "", fmt.Errorf("sweep: record gas funding for %s: %w", row.ID, err)
	}
	return nonce, res.RawTx, res.TxHash, nil
}

// trackGasFunding waits for the funding transaction and books the ether that
// moved (§6.1.4 f, first row of the ERC-20 pair).
func (w *Worker) trackGasFunding(ctx context.Context, row sqlcgen.ChainSweep) error {
	receipt, err := w.chain.Receipt(ctx, *row.GasFundingTxHash)
	if errors.Is(err, evm.ErrNotFound) {
		// Either still in flight, or recorded and never sent because the
		// process died in between. Re-send the stored bytes rather than try to
		// tell those apart: the transaction is identical, so a node that
		// already has it says so and one that never saw it now does.
		if err := w.chain.SendRawTransaction(ctx, row.GasFundingRawTx); err != nil && !errors.Is(err, evm.ErrKnownTransaction) {
			return fmt.Errorf("sweep: re-send funding for %s: %w", row.ID, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("sweep: receipt of funding %s: %w", *row.GasFundingTxHash, err)
	}
	confirmations, err := w.confirmations(ctx, receipt.BlockNumber.Uint64())
	if err != nil {
		return err
	}
	// The native asset's own threshold governs the funding leg, because that
	// is the asset that moved.
	native, err := w.reg.GetAsset(ctx, w.cfg.Tenant, w.cfg.NativeAsset)
	if err != nil {
		return fmt.Errorf("sweep: native asset: %w", err)
	}
	if confirmations < w.requiredConfirmations(native) {
		return nil
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		// The ether never arrived, so the address still cannot pay. Nothing of
		// the sweep itself has happened.
		w.log.Error("gas funding reverted on chain",
			slog.String("sweep_id", row.ID), slog.String("tx_hash", *row.GasFundingTxHash))
		return w.failFunding(ctx, row, gasCost(receipt), int64Ptr(receipt.BlockNumber.Uint64()))
	}
	return w.markGasFunded(ctx, row, gasCost(receipt), int64Ptr(receipt.BlockNumber.Uint64()))
}

// markGasFunded books the funded ether and the funding's own gas, and moves
// the sweep on.
//
// Two things moved and both are house-to-house: the funding itself went from
// custody:hot to custody:deposit_addresses (it is sitting on one of our
// addresses now), and the gas it burned left custody:hot for gas_expense.
// block is where the funding transaction was mined, or nil when the address
// already held enough and none was sent. Reconciliation reads it with
// gas_funding_cost to place the entry above or below the height it read
// balances at.
func (w *Worker) markGasFunded(ctx context.Context, row sqlcgen.ChainSweep, gas money.Amount, block *int64) error {
	hot, err := w.ledger.HouseAccount(ledger.HouseCustodyHot)
	if err != nil {
		return err
	}
	addresses, err := w.ledger.HouseAccount(ledger.HouseCustodyDepositAddresses)
	if err != nil {
		return err
	}
	gasAccount, err := w.ledger.HouseAccount(ledger.HouseGasExpense)
	if err != nil {
		return err
	}
	funding := money.Zero
	if row.GasFundingAmount.Valid {
		if funding, err = pg.AmountFromNumeric(row.GasFundingAmount); err != nil {
			return err
		}
	}
	return inTx(ctx, w.db, func(tx pgx.Tx) error {
		if funding.IsPositive() {
			if _, _, err := w.ledger.Post(ctx, tx, ledger.Entry{
				IdempotencyKey: "sweep:gas_funding:" + row.ID, Kind: ledger.KindSweep,
				RefType: "sweep", RefID: row.ID, Reason: "funding a deposit address for its own sweep",
				CorrelationID: deref(row.CorrelationID),
				Postings: []ledger.Posting{
					{AccountID: addresses, Asset: w.cfg.NativeAsset, Bucket: ledger.BucketHouse, Direction: ledger.Debit, Amount: funding},
					{AccountID: hot, Asset: w.cfg.NativeAsset, Bucket: ledger.BucketHouse, Direction: ledger.Credit, Amount: funding},
				},
			}); err != nil {
				return fmt.Errorf("sweep: post gas funding %s: %w", row.ID, err)
			}
		}
		if err := w.postGas(ctx, tx, row, "sweep:gas_funding_cost:"+row.ID, gasAccount, hot, gas); err != nil {
			return err
		}
		if _, err := sqlcgen.New(tx).MarkSweepGasFunded(ctx, sqlcgen.MarkSweepGasFundedParams{
			TenantID: w.cfg.Tenant, ID: row.ID, GasFundingCost: pg.NumericFromAmount(gas),
			GasFundingBlock: block,
		}); err != nil {
			return fmt.Errorf("sweep: mark gas funded %s: %w", row.ID, err)
		}
		w.log.Info("deposit address funded",
			slog.String("sweep_id", row.ID), slog.String("funding", funding.String()),
			slog.String("gas", gas.String()))
		return w.record(ctx, tx, row, "sweep.gas_funded", map[string]any{
			"funding": funding.String(), "gas": gas.String(),
		})
	})
}

// send signs the sweep with the deposit address's own key and broadcasts it.
//
// The nonce comes straight from the node: a deposit address is used by nothing
// but the sweeper, so there is no allocator to coordinate with and no gap to
// fill (§6.4.3). It is still pinned to the row before signing, because a crash
// between signing and recording must not produce a second transaction.
func (w *Worker) send(ctx context.Context, row sqlcgen.ChainSweep, asset registry.Asset) error {
	from := common.HexToAddress(row.FromAddress)
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return err
	}
	gas, err := w.estimateTransfer(ctx, row, asset)
	if err != nil {
		return err
	}
	// The plan was made a tick ago. Re-check it against what the address holds
	// now: a native sweep whose fee budget no longer fits cannot be bumped
	// later, because bumping needs balance the address does not have.
	if ok, err := w.stillAffordable(ctx, row, asset, amount, gas); err != nil {
		return err
	} else if !ok {
		w.log.Warn("abandoning a sweep whose balance no longer covers it",
			slog.String("sweep_id", row.ID), slog.String("address", row.FromAddress))
		return w.fail(ctx, row, FailureBalanceChanged, money.Zero, nil)
	}

	nonce, raw, hash, err := w.signSweep(ctx, row, amount, gas, from)
	if err != nil {
		return err
	}
	if err := w.chain.SendRawTransaction(ctx, raw); err != nil && !errors.Is(err, evm.ErrKnownTransaction) {
		w.log.Error("sweep broadcast was refused",
			slog.String("sweep_id", row.ID), slog.String("err", err.Error()))
		return w.fail(ctx, row, FailureBroadcast, money.Zero, nil)
	}
	return inTx(ctx, w.db, func(tx pgx.Tx) error {
		if _, err := sqlcgen.New(tx).MarkSweepBroadcast(ctx, sqlcgen.MarkSweepBroadcastParams{
			TenantID: w.cfg.Tenant, ID: row.ID,
		}); err != nil {
			return fmt.Errorf("sweep: mark broadcast %s: %w", row.ID, err)
		}
		w.log.Info("sweep broadcast",
			slog.String("sweep_id", row.ID), slog.String("address", row.FromAddress),
			slog.String("asset", row.Asset), slog.String("amount", amount.String()),
			slog.Uint64("nonce", nonce), slog.String("tx_hash", hash))
		return w.record(ctx, tx, row, "sweep.broadcast", map[string]any{
			"tx_hash": hash, "nonce": nonce, "amount": amount.String(),
		})
	})
}

// signSweep pins the nonce, signs, and stores the bytes.
func (w *Worker) signSweep(ctx context.Context, row sqlcgen.ChainSweep, amount money.Amount, gas uint64, from common.Address) (uint64, []byte, string, error) {
	if row.RawTx != nil && row.TxHash != nil && row.Nonce != nil {
		return uint64(*row.Nonce), row.RawTx, *row.TxHash, nil //nolint:gosec // CHECKed >= 0
	}
	var nonce uint64
	if row.Nonce != nil {
		nonce = uint64(*row.Nonce) //nolint:gosec // CHECKed >= 0
	} else {
		n, err := w.chain.PendingNonceAt(ctx, from)
		if err != nil {
			return 0, nil, "", err
		}
		if _, err := sqlcgen.New(w.db).AllocateSweepNonce(ctx, sqlcgen.AllocateSweepNonceParams{
			TenantID: w.cfg.Tenant, ID: row.ID, Nonce: int64Ptr(n),
		}); err != nil {
			return 0, nil, "", fmt.Errorf("sweep: pin nonce on %s: %w", row.ID, err)
		}
		nonce = n
	}
	fees, err := w.fees(ctx)
	if err != nil {
		return 0, nil, "", err
	}
	res, err := w.signer.Sign(ctx, signer.Request{
		Kind: signer.KindSweep, RefID: row.ID, ChainID: row.ChainID,
		To: w.nonces.HotWallet(), Asset: row.Asset, Value: amount,
		Nonce: nonce, Gas: gas, TipCap: fees.TipCap, FeeCap: fees.FeeCap,
	})
	if err != nil {
		return 0, nil, "", fmt.Errorf("sweep: sign %s: %w", row.ID, err)
	}
	if _, err := sqlcgen.New(w.db).SignSweep(ctx, sqlcgen.SignSweepParams{
		TenantID: w.cfg.Tenant, ID: row.ID, RawTx: res.RawTx, TxHash: &res.TxHash,
	}); err != nil {
		return 0, nil, "", fmt.Errorf("sweep: record signature %s: %w", row.ID, err)
	}
	return nonce, res.RawTx, res.TxHash, nil
}

// stillAffordable re-checks the plan against the address's current balance.
func (w *Worker) stillAffordable(ctx context.Context, row sqlcgen.ChainSweep, asset registry.Asset, amount money.Amount, gas uint64) (bool, error) {
	// A signed sweep is past the point of re-planning: the transaction exists
	// and re-sending the same bytes is always the right answer.
	if row.RawTx != nil {
		return true, nil
	}
	budget, err := w.gasBudget(ctx, gas)
	if err != nil {
		return false, err
	}
	held, err := w.chain.Balance(ctx, common.HexToAddress(row.FromAddress))
	if err != nil {
		return false, err
	}
	ether, err := evm.FromWei(held, evm.MaxScale)
	if err != nil {
		return false, err
	}
	if asset.IsNative {
		// The address must cover both the amount and the fee it may cost.
		return ether.Cmp(amount.Add(budget)) >= 0, nil
	}
	// A token sweep pays its gas in ether and moves the token, so both have to
	// be there.
	if ether.Cmp(budget) < 0 {
		return false, nil
	}
	tokens, err := w.heldBy(ctx, row.FromAddress, asset)
	if err != nil {
		return false, err
	}
	return tokens.Cmp(amount) >= 0, nil
}

// track waits for the sweep's receipt and settles it (§6.1.4 f).
func (w *Worker) track(ctx context.Context, row sqlcgen.ChainSweep, asset registry.Asset) error {
	if row.TxHash == nil {
		return fmt.Errorf("sweep: %s is broadcast without a hash", row.ID)
	}
	receipt, err := w.chain.Receipt(ctx, *row.TxHash)
	if errors.Is(err, evm.ErrNotFound) {
		// Same reasoning as trackGasFunding, and it belongs here more than
		// there: a token sweep is the leg that can be dropped from the mempool
		// through no fault of its own. Its gas comes from the funding transfer
		// that runs first, and if that transfer is still in flight when this
		// one is priced, the ether can arrive after this transaction has
		// already been rejected as underfunded.
		//
		// This used to `return nil`, which reads as "not mined yet, look
		// again next tick" -- but the next tick asks the same node the same
		// question and gets the same answer, forever. Meanwhile the partial
		// index allows no second sweep for the address and admin has no write
		// path to these rows, so the tokens sit there until somebody edits the
		// database by hand. Re-sending the stored bytes costs nothing when the
		// transaction really is in flight (the node already has it and says
		// so) and is the whole fix when it is not.
		if err := w.chain.SendRawTransaction(ctx, row.RawTx); err != nil && !errors.Is(err, evm.ErrKnownTransaction) {
			return fmt.Errorf("sweep: re-send %s: %w", row.ID, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("sweep: receipt of %s: %w", row.ID, err)
	}
	block := receipt.BlockNumber.Uint64()
	confirmations, err := w.confirmations(ctx, block)
	if err != nil {
		return err
	}
	if confirmations < w.requiredConfirmations(asset) {
		return nil
	}
	gas := gasCost(receipt)
	if receipt.Status != types.ReceiptStatusSuccessful {
		// The gas was spent, so it is booked; the asset never moved, so it is
		// not. The next tick sees the balance still sitting there and plans a
		// fresh sweep.
		w.log.Error("sweep reverted on chain",
			slog.String("sweep_id", row.ID), slog.String("tx_hash", *row.TxHash),
			slog.Uint64("block", block))
		return w.fail(ctx, row, FailureOnChain, gas, int64Ptr(block))
	}
	return w.confirm(ctx, row, block, gas)
}

// confirm books the collected asset and the gas (§6.1.4 f).
//
// The asset moved from custody:deposit_addresses to custody:hot, and the gas
// was paid by whichever address sent the transaction — the deposit address, so
// it comes out of custody:deposit_addresses even for a token sweep whose asset
// entry is in USDC. That asymmetry is the reason these are two entries.
func (w *Worker) confirm(ctx context.Context, row sqlcgen.ChainSweep, block uint64, gas money.Amount) error {
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return err
	}
	hot, err := w.ledger.HouseAccount(ledger.HouseCustodyHot)
	if err != nil {
		return err
	}
	addresses, err := w.ledger.HouseAccount(ledger.HouseCustodyDepositAddresses)
	if err != nil {
		return err
	}
	gasAccount, err := w.ledger.HouseAccount(ledger.HouseGasExpense)
	if err != nil {
		return err
	}
	return inTx(ctx, w.db, func(tx pgx.Tx) error {
		if _, _, err := w.ledger.Post(ctx, tx, ledger.Entry{
			IdempotencyKey: "sweep:confirm:" + row.ID, Kind: ledger.KindSweep,
			RefType: "sweep", RefID: row.ID, Reason: "swept into the hot wallet",
			CorrelationID: deref(row.CorrelationID),
			Postings: []ledger.Posting{
				{AccountID: hot, Asset: row.Asset, Bucket: ledger.BucketHouse, Direction: ledger.Debit, Amount: amount},
				{AccountID: addresses, Asset: row.Asset, Bucket: ledger.BucketHouse, Direction: ledger.Credit, Amount: amount},
			},
		}); err != nil {
			return fmt.Errorf("sweep: post confirm %s: %w", row.ID, err)
		}
		// The deposit address paid, so the gas leaves custody:deposit_addresses
		// rather than custody:hot.
		if err := w.postGas(ctx, tx, row, "sweep:gas:"+row.ID, gasAccount, addresses, gas); err != nil {
			return err
		}
		if _, err := sqlcgen.New(tx).MarkSweepConfirmed(ctx, sqlcgen.MarkSweepConfirmedParams{
			TenantID: w.cfg.Tenant, ID: row.ID, BlockNumber: int64Ptr(block),
			GasCost: pg.NumericFromAmount(gas),
		}); err != nil {
			return fmt.Errorf("sweep: mark confirmed %s: %w", row.ID, err)
		}
		w.log.Info("sweep confirmed",
			slog.String("sweep_id", row.ID), slog.String("address", row.FromAddress),
			slog.String("asset", row.Asset), slog.String("amount", amount.String()),
			slog.String("gas", gas.String()), slog.Uint64("block", block))
		w.metrics.confirmed.WithLabelValues(row.Asset).Inc()
		return w.emit(ctx, tx, row, EventCompleted, StatusConfirmed, "", amount, gas, block)
	})
}

// fail records a terminal failure of the sweep transaction, booking any gas
// the deposit address really spent and the block it spent it in.
func (w *Worker) fail(ctx context.Context, row sqlcgen.ChainSweep, reason string, gas money.Amount, block *int64) error {
	addresses, err := w.ledger.HouseAccount(ledger.HouseCustodyDepositAddresses)
	if err != nil {
		return err
	}
	return w.failWith(ctx, row, reason, gas, addresses, "sweep:gas:"+row.ID, func(tx pgx.Tx) error {
		_, err := sqlcgen.New(tx).FailSweep(ctx, sqlcgen.FailSweepParams{
			TenantID: w.cfg.Tenant, ID: row.ID, FailureReason: optString(reason),
			GasCost: pg.NumericFromAmount(gas), BlockNumber: block,
		})
		return err
	})
}

// failFunding is the same for the gas-funding leg, where the hot wallet paid
// and the cost belongs to the funding columns.
//
// The two are separate because the cost and the block have to stay paired:
// writing a funding cost next to a sweep block would tell reconciliation the
// gas was burned by a transaction that never ran.
func (w *Worker) failFunding(ctx context.Context, row sqlcgen.ChainSweep, gas money.Amount, block *int64) error {
	hot, err := w.ledger.HouseAccount(ledger.HouseCustodyHot)
	if err != nil {
		return err
	}
	return w.failWith(ctx, row, FailureGasFunding, gas, hot, "sweep:gas_funding_cost:"+row.ID, func(tx pgx.Tx) error {
		_, err := sqlcgen.New(tx).FailSweepFunding(ctx, sqlcgen.FailSweepFundingParams{
			TenantID: w.cfg.Tenant, ID: row.ID, FailureReason: optString(FailureGasFunding),
			GasFundingCost: pg.NumericFromAmount(gas), GasFundingBlock: block,
		})
		return err
	})
}

func (w *Worker) failWith(ctx context.Context, row sqlcgen.ChainSweep, reason string, gas money.Amount,
	payer, key string, mark func(pgx.Tx) error,
) error {
	gasAccount, err := w.ledger.HouseAccount(ledger.HouseGasExpense)
	if err != nil {
		return err
	}
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return err
	}
	return inTx(ctx, w.db, func(tx pgx.Tx) error {
		if err := w.postGas(ctx, tx, row, key, gasAccount, payer, gas); err != nil {
			return err
		}
		if err := mark(tx); err != nil {
			return fmt.Errorf("sweep: fail %s: %w", row.ID, err)
		}
		w.log.Warn("sweep failed",
			slog.String("sweep_id", row.ID), slog.String("address", row.FromAddress),
			slog.String("asset", row.Asset), slog.String("reason", reason))
		w.metrics.failed.WithLabelValues(row.Asset, reason).Inc()
		return w.emit(ctx, tx, row, EventFailed, StatusFailed, reason, amount, gas, 0)
	})
}

// postGas books what a transaction actually cost, out of whichever custody
// account sent it. Gas is always in the chain's native coin.
func (w *Worker) postGas(ctx context.Context, tx pgx.Tx, row sqlcgen.ChainSweep, key, gasAccount, payer string, gas money.Amount) error {
	if !gas.IsPositive() {
		return nil // a scripted chain, or a receipt with no effective price
	}
	if _, _, err := w.ledger.Post(ctx, tx, ledger.Entry{
		IdempotencyKey: key, Kind: ledger.KindGas,
		RefType: "sweep", RefID: row.ID, Reason: "sweep gas",
		CorrelationID: deref(row.CorrelationID),
		Postings: []ledger.Posting{
			{AccountID: gasAccount, Asset: w.cfg.NativeAsset, Bucket: ledger.BucketHouse, Direction: ledger.Debit, Amount: gas},
			{AccountID: payer, Asset: w.cfg.NativeAsset, Bucket: ledger.BucketHouse, Direction: ledger.Credit, Amount: gas},
		},
	}); err != nil {
		return fmt.Errorf("sweep: post gas %s: %w", row.ID, err)
	}
	return nil
}

// record writes an audit row for a step that has no event of its own.
func (w *Worker) record(ctx context.Context, tx pgx.Tx, row sqlcgen.ChainSweep, action string, after map[string]any) error {
	if err := w.audit.Record(ctx, tx, audit.Event{
		ActorType: audit.ActorSystem, ActorID: "chain", Action: action,
		TargetType: "sweep", TargetID: row.ID,
		Before:        map[string]any{"status": row.Status},
		After:         after,
		CorrelationID: deref(row.CorrelationID),
	}); err != nil {
		return fmt.Errorf("sweep: audit: %w", err)
	}
	return nil
}

// estimateTransfer is the gas limit for the sweep transaction itself.
func (w *Worker) estimateTransfer(ctx context.Context, row sqlcgen.ChainSweep, asset registry.Asset) (uint64, error) {
	if asset.IsNative {
		return gasForNativeTransfer, nil
	}
	if asset.ContractAddress == nil {
		return 0, fmt.Errorf("sweep: %s has no contract address", asset.Symbol)
	}
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return 0, err
	}
	units, err := evm.ToWei(amount, asset.Scale)
	if err != nil {
		return 0, err
	}
	data, err := evm.TransferCalldata(w.nonces.HotWallet(), units)
	if err != nil {
		return 0, err
	}
	gas, err := w.chain.EstimateGas(ctx, common.HexToAddress(row.FromAddress),
		common.HexToAddress(*asset.ContractAddress), new(big.Int), data)
	if err != nil {
		return 0, err
	}
	// A fifth of headroom, for the same reason the withdrawal path adds it: an
	// estimate that is exactly right fails when the recipient's storage slot
	// changes from zero to non-zero between the estimate and the mine.
	return gas + gas/5, nil
}

// gasCost is gas used × the price actually paid, in the native coin.
func gasCost(r *types.Receipt) money.Amount {
	if r == nil || r.EffectiveGasPrice == nil || r.GasUsed == 0 {
		return money.Zero
	}
	wei := new(big.Int).Mul(new(big.Int).SetUint64(r.GasUsed), r.EffectiveGasPrice)
	amount, err := evm.FromWei(wei, evm.MaxScale)
	if err != nil {
		return money.Zero
	}
	return amount
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
