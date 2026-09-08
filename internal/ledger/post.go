package ledger

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/arc119226/crypto-exchange/internal/ledger/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// Post writes one journal entry inside the caller's transaction in two
// round trips (see PendingEntry). It returns the persisted entry and
// replayed=true when the idempotency key already existed (nothing is
// written in that case). On ErrInsufficient the caller must roll back the
// transaction or its savepoint.
func (s *Service) Post(ctx context.Context, tx pgx.Tx, e Entry) (JournalEntry, bool, error) {
	pe, err := s.Begin(e)
	if err != nil {
		return JournalEntry{}, false, err
	}
	var b1 pg.Batch
	pe.QueueLocks(&b1)
	if err := b1.Send(ctx, tx); err != nil {
		return JournalEntry{}, false, fmt.Errorf("ledger: post %s: %w", e.Kind, err)
	}
	replayed, err := pe.Check(ctx, tx)
	if err != nil || replayed {
		return pe.Entry(), replayed, err
	}
	var b2 pg.Batch
	pe.QueueApply(&b2)
	if err := b2.Send(ctx, tx); err != nil {
		return JournalEntry{}, false, pe.applyErr(err)
	}
	if err := pe.Finish(); err != nil {
		return JournalEntry{}, false, err
	}
	return pe.Entry(), false, nil
}

// PendingEntry is an entry on its way into the journal, split into the two
// round trips it costs so a caller with statements of its own (the trading
// runner) can share them:
//
//	pe, _ := s.Begin(entry)
//	pe.QueueLocks(&b1)          // entry row, accounts, balance row locks
//	b1.Send(ctx, tx)
//	replayed, err := pe.Check(ctx, tx) // replay, account kinds, sufficiency
//	pe.QueueApply(&b2)          // postings, balance cache
//	b2.Send(ctx, tx)
//	pe.Finish()                 // read the balances back
//	pe.Entry()
//
// Postgres decides what can share a batch: the first failing statement
// aborts the transaction, so QueueLocks holds only statements a business
// rejection never needs undone (a replay inserts nothing; a lock is what
// the caller wanted anyway), and the writes wait for QueueApply, after the
// Go-side checks have passed.
type PendingEntry struct {
	s      *Service
	e      Entry
	deltas map[balanceKey]*balanceDelta
	keys   []balanceKey

	entry    *pg.Result[sqlcgen.LedgerJournalEntry]
	accounts *pg.Result[[]sqlcgen.LedgerAccount]
	locks    *pg.Result[[]sqlcgen.LockBalancesRow]
	applied  *pg.Result[[]sqlcgen.ApplyBalanceDeltasRow]

	result JournalEntry
}

// Begin validates the entry and prepares its posting.
func (s *Service) Begin(e Entry) (*PendingEntry, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	deltas, keys := aggregateDeltas(e.Postings)
	return &PendingEntry{s: s, e: e, deltas: deltas, keys: keys}, nil
}

// QueueLocks queues the first round trip: the entry row, the accounts of
// every posting, and the balance rows (created if missing) locked in
// (account, asset) order.
func (p *PendingEntry) QueueLocks(b *pg.Batch) {
	e := p.e
	// positional arguments in the order of sqlcgen.InsertJournalEntryParams
	p.entry = pg.QueueOne[sqlcgen.LedgerJournalEntry](b, sqlcgen.InsertJournalEntry,
		p.s.tenant, e.IdempotencyKey, e.Kind, optString(e.RefType), optString(e.RefID), optString(e.Reason), optString(e.CorrelationID))
	p.accounts = pg.QueueMany[sqlcgen.LedgerAccount](b, sqlcgen.GetAccounts, accountIDs(e.Postings))
	if len(p.keys) > 0 {
		accounts, assets := splitKeys(p.keys)
		p.locks = pg.QueueMany[sqlcgen.LockBalancesRow](b, sqlcgen.LockBalances, accounts, assets)
	}
}

// Check reads the first round trip: a replayed key returns (true, nil) and
// Entry() is the existing entry; otherwise the accounts and funds are
// checked (ErrAccountNotFound, ErrAccountKind, ErrInsufficient).
func (p *PendingEntry) Check(ctx context.Context, tx pgx.Tx) (bool, error) {
	if p.entry.Err != nil { // ON CONFLICT DO NOTHING returned no row
		existing, err := p.s.entryByKey(ctx, tx, p.e.IdempotencyKey)
		if err != nil {
			return false, err
		}
		p.result = existing
		return true, nil
	}
	if err := p.s.checkAccountKinds(p.accounts.Val, p.e.Postings); err != nil {
		return false, err
	}
	if p.locks == nil {
		return false, nil
	}
	current := make(map[balanceKey]sqlcgen.LockBalancesRow, len(p.locks.Val))
	for _, row := range p.locks.Val {
		current[balanceKey{row.AccountID, row.Asset}] = row
	}
	for _, k := range p.keys {
		cur, ok := current[k]
		if !ok {
			return false, fmt.Errorf("ledger: lock balance: no row for %s %s", k.account, k.asset)
		}
		available, err := pg.AmountFromNumeric(cur.Available)
		if err != nil {
			return false, err
		}
		hold, err := pg.AmountFromNumeric(cur.Hold)
		if err != nil {
			return false, err
		}
		d := p.deltas[k]
		if available.Add(d.available).IsNegative() {
			return false, fmt.Errorf("%w: account %s %s available %s, need %s", ErrInsufficient, k.account, k.asset, available, d.available.Neg())
		}
		if hold.Add(d.hold).IsNegative() {
			return false, fmt.Errorf("%w: account %s %s hold %s, need %s", ErrInsufficient, k.account, k.asset, hold, d.hold.Neg())
		}
	}
	return false, nil
}

// QueueApply queues the second round trip: the postings and the balance
// cache deltas. Only valid after Check returned (false, nil).
func (p *PendingEntry) QueueApply(b *pg.Batch) {
	e := p.e
	n := len(e.Postings)
	accounts, assets, buckets, directions := make([]string, 0, n), make([]string, 0, n), make([]string, 0, n), make([]string, 0, n)
	amounts := make([]pgtype.Numeric, 0, n)
	for _, po := range e.Postings {
		accounts, assets = append(accounts, po.AccountID), append(assets, po.Asset)
		buckets, directions = append(buckets, string(po.Bucket)), append(directions, string(po.Direction))
		amounts = append(amounts, pg.NumericFromAmount(po.Amount))
	}
	b.QueueExec(sqlcgen.InsertPostings, p.entry.Val.ID, accounts, assets, buckets, directions, amounts)
	if len(p.keys) > 0 {
		keyAccounts, keyAssets := splitKeys(p.keys)
		avail, hold := make([]pgtype.Numeric, 0, len(p.keys)), make([]pgtype.Numeric, 0, len(p.keys))
		for _, k := range p.keys {
			d := p.deltas[k]
			avail, hold = append(avail, pg.NumericFromAmount(d.available)), append(hold, pg.NumericFromAmount(d.hold))
		}
		p.applied = pg.QueueMany[sqlcgen.ApplyBalanceDeltasRow](b, sqlcgen.ApplyBalanceDeltas, keyAccounts, keyAssets, avail, hold)
	}
}

// applyErr maps a CHECK violation on the balance cache to ErrInsufficient
// (the Go check in Check is the real gate; the constraint is the backstop).
func (p *PendingEntry) applyErr(err error) error {
	if pgErrorCode(err) == "23514" {
		return fmt.Errorf("%w: %s", ErrInsufficient, p.e.IdempotencyKey)
	}
	return fmt.Errorf("ledger: apply %s: %w", p.e.Kind, err)
}

// Finish reads the second round trip and builds the JournalEntry with the
// balances as they now stand, in (account, asset) order.
func (p *PendingEntry) Finish() error {
	je := journalFromRow(p.entry.Val)
	je.Postings = append([]Posting(nil), p.e.Postings...)
	if p.applied != nil {
		updated := make(map[balanceKey]sqlcgen.ApplyBalanceDeltasRow, len(p.applied.Val))
		for _, row := range p.applied.Val {
			updated[balanceKey{row.AccountID, row.Asset}] = row
		}
		je.Balances = make([]Balance, 0, len(p.keys))
		for _, k := range p.keys {
			row, ok := updated[k]
			if !ok {
				return fmt.Errorf("ledger: apply balance delta: no row for %s %s", k.account, k.asset)
			}
			available, err := pg.AmountFromNumeric(row.Available)
			if err != nil {
				return err
			}
			hold, err := pg.AmountFromNumeric(row.Hold)
			if err != nil {
				return err
			}
			je.Balances = append(je.Balances, Balance{AccountID: row.AccountID, Asset: row.Asset, Available: available, Hold: hold, Version: row.Version})
		}
	}
	if p.s.metrics != nil {
		p.s.metrics.entries.WithLabelValues(p.e.Kind).Inc()
	}
	p.result = je
	return nil
}

// ApplyErr is applyErr for callers driving the phases themselves.
func (p *PendingEntry) ApplyErr(err error) error { return p.applyErr(err) }

// Entry is the posted (or replayed) entry; valid after Check returned a
// replay or after Finish.
func (p *PendingEntry) Entry() JournalEntry { return p.result }

// Kind is the entry kind, for error messages.
func (p *PendingEntry) Kind() string { return p.e.Kind }

func splitKeys(keys []balanceKey) (accounts, assets []string) {
	accounts, assets = make([]string, 0, len(keys)), make([]string, 0, len(keys))
	for _, k := range keys {
		accounts, assets = append(accounts, k.account), append(assets, k.asset)
	}
	return accounts, assets
}
