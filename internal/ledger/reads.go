package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/ledger/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// Balances returns the cached balances of a spot account.
func (s *Service) Balances(ctx context.Context, accountID string) ([]Balance, error) {
	if _, err := s.Account(ctx, accountID); err != nil {
		return nil, err
	}
	rows, err := sqlcgen.New(s.pool).GetBalances(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("ledger: get balances: %w", err)
	}
	out := make([]Balance, 0, len(rows))
	for _, r := range rows {
		b, err := balanceFromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// Balance returns one cached balance (zero when no posting touched the pair).
func (s *Service) Balance(ctx context.Context, accountID, asset string) (Balance, error) {
	row, err := sqlcgen.New(s.pool).GetBalance(ctx, sqlcgen.GetBalanceParams{AccountID: accountID, Asset: asset})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Balance{AccountID: accountID, Asset: asset, Available: money.Zero, Hold: money.Zero}, nil
		}
		return Balance{}, fmt.Errorf("ledger: get balance: %w", err)
	}
	return balanceFromRow(row)
}

func balanceFromRow(r sqlcgen.LedgerBalance) (Balance, error) {
	available, err := pg.AmountFromNumeric(r.Available)
	if err != nil {
		return Balance{}, err
	}
	hold, err := pg.AmountFromNumeric(r.Hold)
	if err != nil {
		return Balance{}, err
	}
	return Balance{AccountID: r.AccountID, Asset: r.Asset, Available: available, Hold: hold, Version: r.Version, UpdatedAt: r.UpdatedAt}, nil
}

// DerivedBalances recomputes a spot account's balances from its postings
// (credit − debit per bucket). Tests and reconciliation compare it with
// Balances (docs/plan-v1.0.md §6.1.5 invariant 2).
func (s *Service) DerivedBalances(ctx context.Context, accountID string) ([]Balance, error) {
	rows, err := sqlcgen.New(s.pool).AccountBucketSums(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("ledger: account bucket sums: %w", err)
	}
	byAsset := map[string]*Balance{}
	order := []string{}
	for _, r := range rows {
		v, err := pg.AmountFromNumeric(r.CreditMinusDebit)
		if err != nil {
			return nil, err
		}
		b, ok := byAsset[r.Asset]
		if !ok {
			b = &Balance{AccountID: accountID, Asset: r.Asset, Available: money.Zero, Hold: money.Zero}
			byAsset[r.Asset] = b
			order = append(order, r.Asset)
		}
		switch Bucket(r.Bucket) {
		case BucketAvailable:
			b.Available = v
		case BucketHold:
			b.Hold = v
		}
	}
	out := make([]Balance, 0, len(order))
	for _, a := range order {
		out = append(out, *byAsset[a])
	}
	return out, nil
}

// Entry returns a journal entry with its postings.
func (s *Service) Entry(ctx context.Context, id int64) (JournalEntry, error) {
	q := sqlcgen.New(s.pool)
	row, err := q.GetJournalEntry(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return JournalEntry{}, ErrNotFound
		}
		return JournalEntry{}, fmt.Errorf("ledger: get entry: %w", err)
	}
	if row.TenantID != s.tenant {
		return JournalEntry{}, ErrNotFound
	}
	return s.withPostings(ctx, q, row)
}

// EntryByKey returns the entry posted under an idempotency key.
func (s *Service) EntryByKey(ctx context.Context, key string) (JournalEntry, error) {
	return s.entryByKey(ctx, s.pool, key)
}

// EntriesFilter selects entries; zero values mean "any".
type EntriesFilter struct {
	AccountID string
	RefType   string
	RefID     string
	Limit     int32
	Offset    int32
}

// Entries lists entries newest first, each with its postings.
func (s *Service) Entries(ctx context.Context, f EntriesFilter) ([]JournalEntry, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q := sqlcgen.New(s.pool)
	var (
		rows []sqlcgen.LedgerJournalEntry
		err  error
	)
	if f.AccountID != "" {
		rows, err = q.ListEntriesByAccount(ctx, sqlcgen.ListEntriesByAccountParams{TenantID: s.tenant, AccountID: f.AccountID, Limit: f.Limit, Offset: f.Offset})
	} else {
		rows, err = q.ListEntries(ctx, sqlcgen.ListEntriesParams{TenantID: s.tenant, RefType: f.RefType, RefID: f.RefID, Limit: f.Limit, Offset: f.Offset})
	}
	if err != nil {
		return nil, fmt.Errorf("ledger: list entries: %w", err)
	}
	out := make([]JournalEntry, 0, len(rows))
	for _, r := range rows {
		je, err := s.withPostings(ctx, q, r)
		if err != nil {
			return nil, err
		}
		out = append(out, je)
	}
	return out, nil
}

// TrialBalance sums debits and credits per asset over the whole tenant;
// every Diff must be zero (docs/plan-v1.0.md §6.1.5 invariant 5).
func (s *Service) TrialBalance(ctx context.Context) ([]TrialBalanceLine, error) {
	rows, err := sqlcgen.New(s.pool).TrialBalance(ctx, s.tenant)
	if err != nil {
		return nil, fmt.Errorf("ledger: trial balance: %w", err)
	}
	out := make([]TrialBalanceLine, 0, len(rows))
	for _, r := range rows {
		debits, err := pg.AmountFromNumeric(r.Debits)
		if err != nil {
			return nil, err
		}
		credits, err := pg.AmountFromNumeric(r.Credits)
		if err != nil {
			return nil, err
		}
		out = append(out, TrialBalanceLine{Asset: r.Asset, Debits: debits, Credits: credits, Diff: debits.Sub(credits)})
	}
	return out, nil
}

// HouseBalances derives every house account balance, signed so that the
// account type's normal balance is positive.
func (s *Service) HouseBalances(ctx context.Context) ([]HouseBalance, error) {
	return s.houseBalances(ctx, sqlcgen.New(s.pool))
}

// HouseBalancesTx is the same read inside a caller's transaction, so it can be
// taken in one snapshot with whatever the caller is comparing it against.
//
// On-chain reconciliation needs that: crediting a deposit moves the amount out
// of the pending set and into custody in one commit, and reading the two sides
// either side of it would show the amount in neither.
func (s *Service) HouseBalancesTx(ctx context.Context, tx pgx.Tx) ([]HouseBalance, error) {
	return s.houseBalances(ctx, sqlcgen.New(tx))
}

func (s *Service) houseBalances(ctx context.Context, q *sqlcgen.Queries) ([]HouseBalance, error) {
	rows, err := q.HouseBalances(ctx, s.tenant)
	if err != nil {
		return nil, fmt.Errorf("ledger: house balances: %w", err)
	}
	out := make([]HouseBalance, 0, len(rows))
	for _, r := range rows {
		if r.HouseCode == nil {
			continue
		}
		dmc, err := pg.AmountFromNumeric(r.DebitMinusCredit)
		if err != nil {
			return nil, err
		}
		code := HouseCode(*r.HouseCode)
		bal := dmc
		if !code.Type().DebitNormal() {
			bal = dmc.Neg()
		}
		out = append(out, HouseBalance{Code: code, Type: code.Type(), Asset: r.Asset, Balance: bal})
	}
	return out, nil
}
