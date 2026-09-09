package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// bookNonceFills follows every gap fill to its receipt and books the gas it
// burned (docs/plan-v1.0.md §6.4.2).
//
// A nonce gap is filled with a 0-value self-transfer so that later withdrawals
// are not stuck behind a hole. It moves nothing, but it costs gas out of the
// hot wallet -- and until this existed, nothing booked that. internal/chain/
// hotwallet imports no ledger at all: it signs, records the row, sends, and
// forgets. Every fill therefore left custody_hot claiming ether the chain no
// longer had, permanently and without limit.
//
// It runs here, at the top of a reconciliation pass, for two reasons. This is
// the code that would otherwise have to report the gap as a break every few
// minutes with no way to resolve it -- and a break the system itself caused is
// not a finding, it is a bug. And it has to happen before the balances are
// read, so the pass sees a ledger that has already accounted for it.
//
// Skipped silently when the chain role runs without reconciliation enabled;
// the fills stay in 'broadcast' and are booked whenever it is turned on.
func (w *Worker) bookNonceFills(ctx context.Context, head uint64, required int32) error {
	fills, err := sqlcgen.New(w.db).ListUnconfirmedNonceFills(ctx, sqlcgen.ListUnconfirmedNonceFillsParams{
		TenantID: w.cfg.Tenant, ChainID: w.cfg.ChainID,
	})
	if err != nil {
		return fmt.Errorf("reconcile: list nonce fills: %w", err)
	}
	for _, fill := range fills {
		if err := w.bookOneFill(ctx, fill, head, required); err != nil {
			return err
		}
	}
	return nil
}

func (w *Worker) bookOneFill(ctx context.Context, fill sqlcgen.ChainNonceFill, head uint64, required int32) error {
	receipt, err := w.chain.Receipt(ctx, fill.TxHash)
	if errors.Is(err, evm.ErrNotFound) {
		return nil // still in flight
	}
	if err != nil {
		return fmt.Errorf("reconcile: receipt of fill %d: %w", fill.Nonce, err)
	}
	block := receipt.BlockNumber.Uint64()
	if head < block || head-block+1 < uint64(required) { //nolint:gosec // required is positive
		return nil
	}
	gas := gasCost(receipt)
	// A fill that reverted still consumed the nonce and still burned gas, so
	// the only thing its status changes is what an operator reading the table
	// sees. Both are terminal.
	status := "confirmed"
	if receipt.Status != 1 {
		status = "failed"
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
		if gas.IsPositive() {
			if _, _, err := w.ledger.Post(ctx, tx, ledger.Entry{
				IdempotencyKey: fmt.Sprintf("nonce_fill:gas:%d:%d", fill.ChainID, fill.Nonce),
				Kind:           ledger.KindGas, RefType: "nonce_fill", RefID: fmt.Sprint(fill.ID),
				Reason: "gas for a nonce gap fill",
				Postings: []ledger.Posting{
					{AccountID: gasAccount, Asset: w.cfg.NativeAsset, Bucket: ledger.BucketHouse, Direction: ledger.Debit, Amount: gas},
					{AccountID: hot, Asset: w.cfg.NativeAsset, Bucket: ledger.BucketHouse, Direction: ledger.Credit, Amount: gas},
				},
			}); err != nil {
				return fmt.Errorf("reconcile: post fill gas %d: %w", fill.Nonce, err)
			}
		}
		if _, err := sqlcgen.New(tx).ConfirmNonceFill(ctx, sqlcgen.ConfirmNonceFillParams{
			TenantID: w.cfg.Tenant, ChainID: w.cfg.ChainID, Nonce: fill.Nonce,
			Status: status, BlockNumber: int64Ptr(block), GasCost: pg.NumericFromAmount(gas),
		}); err != nil {
			return fmt.Errorf("reconcile: confirm fill %d: %w", fill.Nonce, err)
		}
		w.log.Info("booked the gas a nonce gap fill burned",
			slog.Int64("nonce", fill.Nonce), slog.String("gas", gas.String()),
			slog.String("reason", fill.Reason), slog.String("status", status))
		return nil
	})
}

func int64Ptr(v uint64) *int64 {
	n := int64(v) //nolint:gosec // block numbers are far below 2^63
	return &n
}
