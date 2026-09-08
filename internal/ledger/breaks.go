package ledger

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/arc119226/crypto-exchange/internal/ledger/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// Break is one asset whose debits and credits do not agree
// (admin.ledger_breaks, migration 0019). It is the ledger reconciling with
// itself: the invariant every posting is supposed to preserve, checked from
// outside the transactions that are supposed to preserve it.
type Break struct {
	ID         string
	Asset      string
	Debits     money.Amount
	Credits    money.Amount
	Diff       money.Amount // Debits − Credits; never zero
	DetectedAt time.Time
	ResolvedAt *time.Time
}

// OpenBreaks counts the assets currently out of balance and recorded as such.
func (s *Service) OpenBreaks(ctx context.Context) (int64, error) {
	n, err := sqlcgen.New(s.pool).CountOpenLedgerBreaks(ctx, s.tenant)
	if err != nil {
		return 0, fmt.Errorf("ledger: count breaks: %w", err)
	}
	return n, nil
}

// Breaks lists breaks newest first, resolved ones included: how long a break
// stayed open is part of the record.
func (s *Service) Breaks(ctx context.Context, limit, offset int32) ([]Break, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := sqlcgen.New(s.pool).ListLedgerBreaks(ctx, sqlcgen.ListLedgerBreaksParams{TenantID: s.tenant, Limit: limit, Offset: offset})
	if err != nil {
		return nil, fmt.Errorf("ledger: list breaks: %w", err)
	}
	out := make([]Break, 0, len(rows))
	for _, r := range rows {
		b, err := breakFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

func breakFromRow(r sqlcgen.AdminLedgerBreak) (Break, error) {
	debits, err := pg.AmountFromNumeric(r.Debits)
	if err != nil {
		return Break{}, fmt.Errorf("ledger: break %s: %w", r.ID, err)
	}
	credits, err := pg.AmountFromNumeric(r.Credits)
	if err != nil {
		return Break{}, fmt.Errorf("ledger: break %s: %w", r.ID, err)
	}
	diff, err := pg.AmountFromNumeric(r.Diff)
	if err != nil {
		return Break{}, fmt.Errorf("ledger: break %s: %w", r.ID, err)
	}
	b := Break{ID: r.ID, Asset: r.Asset, Debits: debits, Credits: credits, Diff: diff, DetectedAt: r.DetectedAt}
	if r.ResolvedAt.Valid {
		t := r.ResolvedAt.Time
		b.ResolvedAt = &t
	}
	return b, nil
}

// ReconcileBreaks is the ledger checking itself: one pass over the trial
// balance against the breaks already open, inside the caller's transaction
// so the events for what it found commit with the rows.
//
// Edge-triggered, like the chain reconciliation (docs/events.md): a break is
// opened when an asset first fails to balance or when the size of its
// difference changes (the old row is resolved and a new one opened, so the
// history keeps every size it had), and resolved when the asset balances
// again. An asset that stays out by the same amount is left alone -- it is
// already open, already visible, and re-announcing it would only teach people
// to ignore it.
func (s *Service) ReconcileBreaks(ctx context.Context, tx pgx.Tx, now time.Time) (opened, resolved []Break, err error) {
	q := sqlcgen.New(tx)
	rows, err := q.TrialBalance(ctx, s.tenant)
	if err != nil {
		return nil, nil, fmt.Errorf("ledger: trial balance: %w", err)
	}
	type totals struct{ debits, credits, diff money.Amount }
	out := map[string]totals{}
	for _, r := range rows {
		debits, err := pg.AmountFromNumeric(r.Debits)
		if err != nil {
			return nil, nil, err
		}
		credits, err := pg.AmountFromNumeric(r.Credits)
		if err != nil {
			return nil, nil, err
		}
		if diff := debits.Sub(credits); !diff.IsZero() {
			out[r.Asset] = totals{debits, credits, diff}
		}
	}
	openRows, err := q.ListOpenLedgerBreaks(ctx, s.tenant)
	if err != nil {
		return nil, nil, fmt.Errorf("ledger: open breaks: %w", err)
	}
	open := map[string]Break{}
	for _, r := range openRows {
		b, err := breakFromRow(r)
		if err != nil {
			return nil, nil, err
		}
		open[b.Asset] = b
	}
	resolve := func(b Break) error {
		if _, err := q.ResolveLedgerBreak(ctx, sqlcgen.ResolveLedgerBreakParams{ID: b.ID, ResolvedAt: pgtype.Timestamptz{Time: now, Valid: true}}); err != nil {
			return fmt.Errorf("ledger: resolve break %s: %w", b.ID, err)
		}
		b.ResolvedAt = &now
		resolved = append(resolved, b)
		return nil
	}
	for asset, t := range out {
		if prev, ok := open[asset]; ok {
			if prev.Diff.Equal(t.diff) {
				continue // still out by the same amount: already announced
			}
			if err := resolve(prev); err != nil {
				return nil, nil, err
			}
		}
		row, err := q.InsertLedgerBreak(ctx, sqlcgen.InsertLedgerBreakParams{
			TenantID: s.tenant, Asset: asset, Debits: pg.NumericFromAmount(t.debits), Credits: pg.NumericFromAmount(t.credits),
			Diff: pg.NumericFromAmount(t.diff), DetectedAt: now,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("ledger: record break %s: %w", asset, err)
		}
		b, err := breakFromRow(row)
		if err != nil {
			return nil, nil, err
		}
		opened = append(opened, b)
	}
	for asset, prev := range open {
		if _, still := out[asset]; !still {
			if err := resolve(prev); err != nil {
				return nil, nil, err
			}
		}
	}
	return opened, resolved, nil
}
