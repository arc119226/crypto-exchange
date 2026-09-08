package ledger

import (
	"context"
	"fmt"
	"time"

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
