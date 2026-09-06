package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/ledger/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// Service posts entries and reads balances for one tenant.
type Service struct {
	pool    *pgxpool.Pool
	tenant  string
	metrics *Metrics

	mu     sync.RWMutex
	houses map[HouseCode]string
}

// New creates a Service; call LoadHouseAccounts (or EnsureHouseAccounts)
// before posting entries that touch house accounts.
func New(pool *pgxpool.Pool, tenantID string) *Service {
	return &Service{pool: pool, tenant: tenantID, houses: map[HouseCode]string{}}
}

// WithMetrics attaches Prometheus metrics (optional).
func (s *Service) WithMetrics(m *Metrics) *Service {
	s.metrics = m
	return s
}

// Tenant returns the tenant id the service is bound to.
func (s *Service) Tenant() string { return s.tenant }

// LoadHouseAccounts reads the tenant's house accounts (seeded by migration
// 0003 for the default tenant).
func (s *Service) LoadHouseAccounts(ctx context.Context) error {
	rows, err := sqlcgen.New(s.pool).ListHouseAccounts(ctx, s.tenant)
	if err != nil {
		return fmt.Errorf("ledger: load house accounts: %w", err)
	}
	m := map[HouseCode]string{}
	for _, r := range rows {
		if r.HouseCode != nil {
			m[HouseCode(*r.HouseCode)] = r.ID
		}
	}
	for _, code := range AllHouseCodes {
		if _, ok := m[code]; !ok {
			return fmt.Errorf("%w: %s (tenant %s)", ErrHouseAccount, code, s.tenant)
		}
	}
	s.mu.Lock()
	s.houses = m
	s.mu.Unlock()
	return nil
}

// EnsureHouseAccounts creates any missing house account (needs INSERT on
// ledger.accounts: admin or all roles) and loads them.
func (s *Service) EnsureHouseAccounts(ctx context.Context) error {
	q := sqlcgen.New(s.pool)
	for _, code := range AllHouseCodes {
		c := string(code)
		if _, err := q.EnsureHouseAccount(ctx, sqlcgen.EnsureHouseAccountParams{TenantID: s.tenant, HouseCode: &c}); err != nil {
			return fmt.Errorf("ledger: ensure house account %s: %w", code, err)
		}
	}
	return s.LoadHouseAccounts(ctx)
}

// HouseAccount returns the account id of a house code.
func (s *Service) HouseAccount(code HouseCode) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.houses[code]
	if !ok {
		return "", fmt.Errorf("%w: %s (call LoadHouseAccounts)", ErrHouseAccount, code)
	}
	return id, nil
}

// --- accounts ---

// CreateSpotAccount opens a spot account (optionally owned by a user).
func (s *Service) CreateSpotAccount(ctx context.Context, db sqlcgen.DBTX, ownerUserID *string) (Account, error) {
	row, err := sqlcgen.New(db).CreateSpotAccount(ctx, sqlcgen.CreateSpotAccountParams{TenantID: s.tenant, OwnerUserID: ownerUserID})
	if err != nil {
		return Account{}, fmt.Errorf("ledger: create spot account: %w", err)
	}
	return accountFromRow(row), nil
}

// Account fetches one account of the tenant.
func (s *Service) Account(ctx context.Context, id string) (Account, error) {
	row, err := sqlcgen.New(s.pool).GetAccount(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Account{}, ErrAccountNotFound
		}
		return Account{}, fmt.Errorf("ledger: get account: %w", err)
	}
	if row.TenantID != s.tenant {
		return Account{}, ErrAccountNotFound
	}
	return accountFromRow(row), nil
}

// ListAccounts pages through the tenant's accounts (kind "" = all).
func (s *Service) ListAccounts(ctx context.Context, kind AccountKind, limit, offset int32) ([]Account, error) {
	rows, err := sqlcgen.New(s.pool).ListAccounts(ctx, sqlcgen.ListAccountsParams{TenantID: s.tenant, Kind: string(kind), Limit: limit, Offset: offset})
	if err != nil {
		return nil, fmt.Errorf("ledger: list accounts: %w", err)
	}
	out := make([]Account, 0, len(rows))
	for _, r := range rows {
		out = append(out, accountFromRow(r))
	}
	return out, nil
}

// SetAccountStatus freezes or reactivates an account.
func (s *Service) SetAccountStatus(ctx context.Context, db sqlcgen.DBTX, id string, status AccountStatus) (Account, error) {
	if status != StatusActive && status != StatusFrozen {
		return Account{}, fmt.Errorf("%w: status %q", ErrInvalidEntry, status)
	}
	if _, err := s.Account(ctx, id); err != nil {
		return Account{}, err
	}
	row, err := sqlcgen.New(db).UpdateAccountStatus(ctx, sqlcgen.UpdateAccountStatusParams{ID: id, Status: string(status)})
	if err != nil {
		return Account{}, fmt.Errorf("ledger: update account status: %w", err)
	}
	return accountFromRow(row), nil
}

func accountFromRow(r sqlcgen.LedgerAccount) Account {
	a := Account{
		ID: r.ID, TenantID: r.TenantID, Kind: AccountKind(r.Kind), OwnerUserID: r.OwnerUserID,
		Status: AccountStatus(r.Status), NextSeq: r.NextSeq, Version: r.Version, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.HouseCode != nil {
		a.HouseCode = HouseCode(*r.HouseCode)
	}
	return a
}

// --- posting ---

type balanceKey struct{ account, asset string }

type balanceDelta struct{ available, hold money.Amount }

// Post writes one journal entry inside the caller's transaction. It returns
// the persisted entry and replayed=true when the idempotency key already
// existed (nothing is written in that case). On ErrInsufficient the caller
// must roll back the transaction.
func (s *Service) Post(ctx context.Context, tx pgx.Tx, e Entry) (JournalEntry, bool, error) {
	if err := e.Validate(); err != nil {
		return JournalEntry{}, false, err
	}
	q := sqlcgen.New(tx)

	// 1. the entry row; a conflict on the key means a replay
	row, err := q.InsertJournalEntry(ctx, sqlcgen.InsertJournalEntryParams{
		TenantID: s.tenant, IdempotencyKey: e.IdempotencyKey, Kind: e.Kind,
		RefType: optString(e.RefType), RefID: optString(e.RefID), Reason: optString(e.Reason), CorrelationID: optString(e.CorrelationID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			existing, err := s.entryByKey(ctx, tx, e.IdempotencyKey)
			if err != nil {
				return JournalEntry{}, false, err
			}
			return existing, true, nil
		}
		return JournalEntry{}, false, fmt.Errorf("ledger: insert entry: %w", err)
	}

	// 2. accounts exist and buckets match their kind
	if err := s.checkAccounts(ctx, q, e.Postings); err != nil {
		return JournalEntry{}, false, err
	}

	// 3. lock the affected balance rows in a fixed order and check funds
	deltas, keys := aggregateDeltas(e.Postings)
	for _, k := range keys {
		cur, err := q.LockBalance(ctx, sqlcgen.LockBalanceParams{AccountID: k.account, Asset: k.asset})
		if err != nil {
			return JournalEntry{}, false, fmt.Errorf("ledger: lock balance: %w", err)
		}
		available, err := pg.AmountFromNumeric(cur.Available)
		if err != nil {
			return JournalEntry{}, false, err
		}
		hold, err := pg.AmountFromNumeric(cur.Hold)
		if err != nil {
			return JournalEntry{}, false, err
		}
		d := deltas[k]
		if available.Add(d.available).IsNegative() {
			return JournalEntry{}, false, fmt.Errorf("%w: account %s %s available %s, need %s", ErrInsufficient, k.account, k.asset, available, d.available.Neg())
		}
		if hold.Add(d.hold).IsNegative() {
			return JournalEntry{}, false, fmt.Errorf("%w: account %s %s hold %s, need %s", ErrInsufficient, k.account, k.asset, hold, d.hold.Neg())
		}
	}

	// 4. postings
	for _, p := range e.Postings {
		if err := q.InsertPosting(ctx, sqlcgen.InsertPostingParams{
			EntryID: row.ID, AccountID: p.AccountID, Asset: p.Asset, Bucket: string(p.Bucket), Direction: string(p.Direction),
			Amount: pg.NumericFromAmount(p.Amount),
		}); err != nil {
			return JournalEntry{}, false, fmt.Errorf("ledger: insert posting: %w", err)
		}
	}

	// 5. balances cache (the CHECK constraints are the backstop for step 3)
	balances := make([]Balance, 0, len(keys))
	for _, k := range keys {
		d := deltas[k]
		updated, err := q.ApplyBalanceDelta(ctx, sqlcgen.ApplyBalanceDeltaParams{
			AccountID: k.account, Asset: k.asset,
			Available: pg.NumericFromAmount(d.available), Hold: pg.NumericFromAmount(d.hold),
		})
		if err != nil {
			if pgErr := pgErrorCode(err); pgErr == "23514" {
				return JournalEntry{}, false, fmt.Errorf("%w: %s %s", ErrInsufficient, k.account, k.asset)
			}
			return JournalEntry{}, false, fmt.Errorf("ledger: apply balance delta: %w", err)
		}
		available, err := pg.AmountFromNumeric(updated.Available)
		if err != nil {
			return JournalEntry{}, false, err
		}
		hold, err := pg.AmountFromNumeric(updated.Hold)
		if err != nil {
			return JournalEntry{}, false, err
		}
		balances = append(balances, Balance{AccountID: updated.AccountID, Asset: updated.Asset, Available: available, Hold: hold, Version: updated.Version})
	}
	if s.metrics != nil {
		s.metrics.entries.WithLabelValues(e.Kind).Inc()
	}
	je := journalFromRow(row)
	je.Postings = append([]Posting(nil), e.Postings...)
	je.Balances = balances
	return je, false, nil
}

// NextAccountSeq increments and returns the account's private event
// sequence (docs/plan-v1.0.md §7.1). Call it in the transaction that writes
// the outbox rows so account_seq and commit order agree.
func (s *Service) NextAccountSeq(ctx context.Context, tx pgx.Tx, accountID string) (int64, error) {
	seq, err := sqlcgen.New(tx).BumpAccountSeq(ctx, accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
		}
		return 0, fmt.Errorf("ledger: bump account seq: %w", err)
	}
	return seq, nil
}

// aggregateDeltas turns spot postings into per-(account, asset) liability
// deltas: credits increase, debits decrease. Returns the deltas and the
// keys in sorted order (the lock order).
func aggregateDeltas(postings []Posting) (map[balanceKey]*balanceDelta, []balanceKey) {
	deltas := map[balanceKey]*balanceDelta{}
	for _, p := range postings {
		if p.Bucket == BucketHouse {
			continue
		}
		k := balanceKey{p.AccountID, p.Asset}
		d, ok := deltas[k]
		if !ok {
			d = &balanceDelta{available: money.Zero, hold: money.Zero}
			deltas[k] = d
		}
		amt := p.Amount
		if p.Direction == Debit {
			amt = amt.Neg()
		}
		if p.Bucket == BucketAvailable {
			d.available = d.available.Add(amt)
		} else {
			d.hold = d.hold.Add(amt)
		}
	}
	keys := make([]balanceKey, 0, len(deltas))
	for k := range deltas {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].account != keys[j].account {
			return keys[i].account < keys[j].account
		}
		return keys[i].asset < keys[j].asset
	})
	return deltas, keys
}

func (s *Service) checkAccounts(ctx context.Context, q *sqlcgen.Queries, postings []Posting) error {
	ids := map[string]struct{}{}
	for _, p := range postings {
		ids[p.AccountID] = struct{}{}
	}
	list := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	sort.Strings(list)
	rows, err := q.GetAccounts(ctx, list)
	if err != nil {
		return fmt.Errorf("ledger: get accounts: %w", err)
	}
	kinds := map[string]AccountKind{}
	for _, r := range rows {
		if r.TenantID == s.tenant {
			kinds[r.ID] = AccountKind(r.Kind)
		}
	}
	for _, p := range postings {
		kind, ok := kinds[p.AccountID]
		if !ok {
			return fmt.Errorf("%w: %s", ErrAccountNotFound, p.AccountID)
		}
		if (kind == KindHouse) != (p.Bucket == BucketHouse) {
			return fmt.Errorf("%w: account %s is %s, bucket %s", ErrAccountKind, p.AccountID, kind, p.Bucket)
		}
	}
	return nil
}

func (s *Service) entryByKey(ctx context.Context, db sqlcgen.DBTX, key string) (JournalEntry, error) {
	q := sqlcgen.New(db)
	row, err := q.GetJournalEntryByKey(ctx, sqlcgen.GetJournalEntryByKeyParams{TenantID: s.tenant, IdempotencyKey: key})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return JournalEntry{}, ErrNotFound
		}
		return JournalEntry{}, fmt.Errorf("ledger: get entry: %w", err)
	}
	return s.withPostings(ctx, q, row)
}

func (s *Service) withPostings(ctx context.Context, q *sqlcgen.Queries, row sqlcgen.LedgerJournalEntry) (JournalEntry, error) {
	je := journalFromRow(row)
	rows, err := q.ListPostingsByEntry(ctx, row.ID)
	if err != nil {
		return JournalEntry{}, fmt.Errorf("ledger: list postings: %w", err)
	}
	je.Postings = make([]Posting, 0, len(rows))
	for _, p := range rows {
		amt, err := pg.AmountFromNumeric(p.Amount)
		if err != nil {
			return JournalEntry{}, err
		}
		je.Postings = append(je.Postings, Posting{AccountID: p.AccountID, Asset: p.Asset, Bucket: Bucket(p.Bucket), Direction: Direction(p.Direction), Amount: amt})
	}
	return je, nil
}

func journalFromRow(r sqlcgen.LedgerJournalEntry) JournalEntry {
	return JournalEntry{
		ID: r.ID, TenantID: r.TenantID, IdempotencyKey: r.IdempotencyKey, Kind: r.Kind,
		RefType: deref(r.RefType), RefID: deref(r.RefID), Reason: deref(r.Reason), CorrelationID: deref(r.CorrelationID),
		CreatedAt: r.CreatedAt,
	}
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// pgErrorCode extracts the SQLSTATE of a Postgres error, or "".
func pgErrorCode(err error) string {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState()
	}
	return ""
}
