package deposit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// The reversed path of §6.4.1: a deposit the ledger credited, whose block a
// reorg then took away.
//
// It is deliberately in two halves, on the same boundary withdrawal resolve
// uses (§6.4.2). The admin role has the person's decision but no node and no
// keys, so it records a request; the chain role, which watches the chain and
// owns these tables, applies it on its next tick. The privilege split is in
// migration 0024: admin may write three columns and nothing else.
//
// The machine will not start a reversal by itself, and that is the design.
// Between the credit and the reorg the account may have traded the money,
// withdrawn it, or had it withdrawn on their behalf; deciding what to do about
// that is not a decision an unattended process should make.

// applyReversals posts the reversing entry for every deposit an operator has
// confirmed. Called at the end of each tick, after the chain work: a reversal
// touches only the ledger, so nothing in the scan depends on it.
func (s *Scanner) applyReversals(ctx context.Context) error {
	q := sqlcgen.New(s.db)
	rows, err := q.ClaimDepositReversals(ctx, sqlcgen.ClaimDepositReversalsParams{
		TenantID: s.cfg.Tenant, Limit: reversalBatch,
	})
	if err != nil {
		return fmt.Errorf("deposit: claim reversals: %w", err)
	}
	for _, row := range rows {
		if err := s.reverse(ctx, row); err != nil {
			return err
		}
	}
	// The gauge counts everything stamped and not yet reversed, whether or not
	// a person has decided, so it is refreshed after the applying rather than
	// before: an operator watching it should see it fall.
	n, err := q.CountDepositsAwaitingReversal(ctx, s.cfg.Tenant)
	if err != nil {
		return fmt.Errorf("deposit: count awaiting reversal: %w", err)
	}
	s.metrics.observeAwaitingReversal(n)
	return nil
}

// reversalBatch caps one tick's reversals. Small on purpose: this queue is
// only ever a handful of rows, and each one moves real money.
const reversalBatch = 16

// reverse undoes one credited deposit: the exact mirror of the three postings
// credit made (§6.1.4 i), in one transaction with the status and the event.
//
// The idempotency key is derived from the deposit's id, so a crash between the
// post and the commit replays into the same entry rather than a second one. It
// is not the credit's key with a prefix: the credit is keyed by the on-chain
// coordinates it was found at, and those are exactly what a reorg took away.
func (s *Scanner) reverse(ctx context.Context, row sqlcgen.ChainDeposit) error {
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return fmt.Errorf("deposit: amount of %s: %w", row.ID, err)
	}
	// Both columns are NOT NULL for anything the ledger has touched, by the
	// CHECKs 0024 re-tied to that rather than to credited_at -- which is what
	// makes them still readable here, after the reversal clears credited_at. A
	// nil is therefore a broken row, not a missing optional, and it is reported
	// separately from a malformed one because the two mean different things.
	credited, err := pg.NullableAmountFromNumeric(row.CreditedAmount)
	if err != nil {
		return fmt.Errorf("deposit: credited amount of %s: %w", row.ID, err)
	}
	if credited == nil {
		return fmt.Errorf("deposit: %s was credited without recording how much", row.ID)
	}
	fee, err := pg.NullableAmountFromNumeric(row.Fee)
	if err != nil {
		return fmt.Errorf("deposit: fee of %s: %w", row.ID, err)
	}
	if fee == nil {
		return fmt.Errorf("deposit: %s was credited without recording its fee", row.ID)
	}
	custody, err := s.ledger.HouseAccount(ledger.HouseCustodyDepositAddresses)
	if err != nil {
		return err
	}
	feeRevenue, err := s.ledger.HouseAccount(ledger.HouseFeeRevenue)
	if err != nil {
		return err
	}

	// Every direction flipped, every amount the same: custody gives back what
	// the chain never delivered, the account gives back what it was credited,
	// and the exchange gives back the fee it charged for delivering it.
	postings := []ledger.Posting{
		{AccountID: custody, Asset: row.Asset, Bucket: ledger.BucketHouse, Direction: ledger.Credit, Amount: amount},
		{AccountID: row.AccountID, Asset: row.Asset, Bucket: ledger.BucketAvailable, Direction: ledger.Debit, Amount: *credited},
	}
	if fee.IsPositive() {
		postings = append(postings,
			ledger.Posting{AccountID: feeRevenue, Asset: row.Asset, Bucket: ledger.BucketHouse, Direction: ledger.Debit, Amount: *fee})
	}

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := s.ledger.Post(ctx, tx, ledger.Entry{
			IdempotencyKey: "deposit:reversal:" + row.ID, Kind: ledger.KindDeposit,
			RefType: "deposit", RefID: row.ID,
			Reason:        "deposit reversed after a deep reorg: " + deref(row.ReversalNote),
			CorrelationID: deref(row.CorrelationID),
			Postings:      postings,
		}); err != nil {
			return fmt.Errorf("deposit: post reversal %s: %w", row.ID, err)
		}
		reversed, err := sqlcgen.New(tx).MarkDepositReversed(ctx, sqlcgen.MarkDepositReversedParams{
			ID: row.ID, TenantID: s.cfg.Tenant,
		})
		if err != nil {
			return fmt.Errorf("deposit: mark reversed %s: %w", row.ID, err)
		}
		s.log.Warn("deposit reversed",
			slog.String("deposit_id", row.ID), slog.String("account_id", row.AccountID),
			slog.String("asset", row.Asset), slog.String("amount", amount.String()),
			slog.String("debited", credited.String()),
			slog.String("requested_by", deref(row.ReversalRequestedBy)),
			slog.String("note", deref(row.ReversalNote)))
		return s.emit(ctx, tx, EventReversed, reversed)
	})
	if errors.Is(err, ledger.ErrInsufficient) {
		return s.reversalRefused(ctx, row, *credited)
	}
	return err
}

// reversalRefused records a reversal the ledger would not take.
//
// It happens for one reason: the account no longer has the money. Balances may
// not go negative (§6.1.5), and this is the case that rule exists for -- the
// alternative is an account that owes the exchange, which this system has no
// concept of and no way to collect. So the request is cleared, the reason is
// kept for the person who asked, and the deposit stays credited and stays in
// the queue. It is a decision that has to go back to a human, and
// docs/runbooks/reorg-alert.md says what the choices are.
func (s *Scanner) reversalRefused(ctx context.Context, row sqlcgen.ChainDeposit, credited money.Amount) error {
	balance, err := s.ledger.Balance(ctx, row.AccountID, row.Asset)
	if err != nil {
		return fmt.Errorf("deposit: balance of %s: %w", row.AccountID, err)
	}
	reason := fmt.Sprintf("the account has %s %s available and the reversal needs %s: it has already been spent",
		balance.Available, row.Asset, credited)
	if err := sqlcgen.New(s.db).RecordDepositReversalError(ctx, sqlcgen.RecordDepositReversalErrorParams{
		ID: row.ID, TenantID: s.cfg.Tenant, ReversalError: &reason,
	}); err != nil {
		return fmt.Errorf("deposit: record reversal error %s: %w", row.ID, err)
	}
	s.log.Error("a deposit reversal was refused because the money is gone",
		slog.String("deposit_id", row.ID), slog.String("account_id", row.AccountID),
		slog.String("asset", row.Asset), slog.String("needed", credited.String()),
		slog.String("available", balance.Available.String()))
	return nil
}
