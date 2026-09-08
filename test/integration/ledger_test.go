//go:build integration

package integration

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"pgregory.net/rapid"

	"github.com/arc119226/crypto-exchange/internal/app"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// TestMain: the ledger property test must run >= 500 sequences (Phase 2 DoD)
// unless -rapid.checks was given explicitly.
func TestMain(m *testing.M) {
	flag.Parse()
	if f := flag.Lookup("rapid.checks"); f != nil && f.Value.String() == f.DefValue {
		_ = f.Value.Set("500")
	}
	os.Exit(m.Run())
}

func amt(s string) money.Amount { return money.MustParse(s) }

func eq(t require.TestingT, want string, got money.Amount, msg ...any) {
	require.Truef(t, amt(want).Equal(got), "want %s got %s %v", want, got, msg)
}

var fees = ledger.FeeParams{MakerBps: 10, TakerBps: 20, BaseScale: 18, QuoteScale: 6}

// ledgerHarness migrates a fresh database and opens pools for the roles used.
type ledgerHarness struct {
	pgHarness
	all *pgxpool.Pool
	svc *ledger.Service
}

func setupLedger(t *testing.T) ledgerHarness {
	t.Helper()
	h := startPostgres(t)
	ctx := context.Background()
	require.NoError(t, app.MigrateUp(ctx, h.DSN("ex_migrate"), discard{}))
	pool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_all"), MaxConns: 30})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	svc := ledger.New(pool, "default")
	require.NoError(t, svc.LoadHouseAccounts(ctx), "migration 0003 seeds the house accounts")
	return ledgerHarness{pgHarness: h, all: pool, svc: svc}
}

type discard struct{}

func (discard) Write(b []byte) (int, error) { return len(b), nil }

// inTx runs fn in a transaction, committing on success and rolling back on error.
func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return tx.Commit(ctx)
}

func (h ledgerHarness) newSpot(t require.TestingT, ctx context.Context) string {
	var id string
	require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
		a, err := h.svc.CreateSpotAccount(ctx, tx, nil)
		id = a.ID
		return err
	}))
	return id
}

func (h ledgerHarness) fund(t require.TestingT, ctx context.Context, account, asset, amount, key string) {
	require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
		_, _, err := h.svc.Adjust(ctx, tx, ledger.AdjustParams{
			AccountID: account, Asset: asset, Amount: amt(amount), Direction: ledger.Credit, Reason: "dev faucet", IdempotencyKey: key,
		})
		return err
	}))
}

func (h ledgerHarness) balance(t require.TestingT, ctx context.Context, account, asset string) ledger.Balance {
	b, err := h.svc.Balance(ctx, account, asset)
	require.NoError(t, err)
	return b
}

func (h ledgerHarness) assertCacheMatchesPostings(t require.TestingT, ctx context.Context, account string) {
	cached, err := h.svc.Balances(ctx, account)
	require.NoError(t, err)
	derived, err := h.svc.DerivedBalances(ctx, account)
	require.NoError(t, err)
	require.Equal(t, len(derived), len(cached), "assets in cache vs postings for %s", account)
	for i := range cached {
		require.Equal(t, derived[i].Asset, cached[i].Asset)
		require.Truef(t, cached[i].Available.Equal(derived[i].Available), "%s %s available cache %s != postings %s", account, cached[i].Asset, cached[i].Available, derived[i].Available)
		require.Truef(t, cached[i].Hold.Equal(derived[i].Hold), "%s %s hold cache %s != postings %s", account, cached[i].Asset, cached[i].Hold, derived[i].Hold)
	}
}

func (h ledgerHarness) assertTrialBalanceZero(t require.TestingT, ctx context.Context) []ledger.TrialBalanceLine {
	lines, err := h.svc.TrialBalance(ctx)
	require.NoError(t, err)
	for _, l := range lines {
		require.Truef(t, l.Diff.IsZero(), "trial balance %s: debits %s credits %s", l.Asset, l.Debits, l.Credits)
	}
	return lines
}

// docs/plan-v1.0.md §6.1.4 (a)(b)(c)(g) end to end against Postgres.
func TestLedgerPlanExampleFlow(t *testing.T) {
	h := setupLedger(t)
	ctx := context.Background()
	B := h.newSpot(t, ctx)
	S := h.newSpot(t, ctx)
	h.fund(t, ctx, B, "USDC", "10000", "adjust:faucet-B")
	h.fund(t, ctx, S, "ETH", "1", "adjust:faucet-S")
	eq(t, "10000", h.balance(t, ctx, B, "USDC").Available)
	eq(t, "1", h.balance(t, ctx, S, "ETH").Available)

	// (a) holds
	require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
		_, replayed, err := h.svc.Hold(ctx, tx, ledger.HoldParams{AccountID: S, Asset: "ETH", Amount: amt("0.4"), IdempotencyKey: "hold:order:S1", Ref: ledger.Ref{Type: "order", ID: "S1"}})
		require.False(t, replayed)
		return err
	}))
	require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
		_, _, err := h.svc.Hold(ctx, tx, ledger.HoldParams{AccountID: B, Asset: "USDC", Amount: amt("2000"), IdempotencyKey: "hold:order:B1", Ref: ledger.Ref{Type: "order", ID: "B1"}})
		return err
	}))
	b := h.balance(t, ctx, B, "USDC")
	eq(t, "8000", b.Available)
	eq(t, "2000", b.Hold)
	s := h.balance(t, ctx, S, "ETH")
	eq(t, "0.6", s.Available)
	eq(t, "0.4", s.Hold)

	// (b) settle 0.4 @ 1990, buyer limit 2000
	var res ledger.SettleResult
	require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
		var err error
		res, _, err = h.svc.Settle(ctx, tx, ledger.SettleParams{
			TradeID: "t1", BuyerAccountID: B, SellerAccountID: S, BuyerIsTaker: true,
			BaseAsset: "ETH", QuoteAsset: "USDC", Price: amt("1990"), Qty: amt("0.4"), QuoteQty: amt("796"),
			BuyerLimitPrice: amt("2000"), Fees: fees,
		})
		return err
	}))
	eq(t, "0.0008", res.TakerFee)
	eq(t, "0.796", res.MakerFee)
	eq(t, "4", res.Release)
	assert.Len(t, res.Entry.Postings, 8)
	b = h.balance(t, ctx, B, "USDC")
	eq(t, "8004", b.Available, "8000 + 4 price improvement")
	eq(t, "1200", b.Hold, "2000 − 796 − 4 (docs/domain.md 1.3 b)")
	eq(t, "0.3992", h.balance(t, ctx, B, "ETH").Available)
	eq(t, "795.204", h.balance(t, ctx, S, "USDC").Available)
	s = h.balance(t, ctx, S, "ETH")
	eq(t, "0.6", s.Available)
	eq(t, "0", s.Hold)

	// (c) cancel remainder
	require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
		_, _, err := h.svc.Release(ctx, tx, ledger.HoldParams{AccountID: B, Asset: "USDC", Amount: amt("1200"), IdempotencyKey: "release:order:B1:3", Ref: ledger.Ref{Type: "order", ID: "B1"}})
		return err
	}))
	b = h.balance(t, ctx, B, "USDC")
	eq(t, "9204", b.Available)
	eq(t, "0", b.Hold)

	// invariants
	h.assertCacheMatchesPostings(t, ctx, B)
	h.assertCacheMatchesPostings(t, ctx, S)
	lines := h.assertTrialBalanceZero(t, ctx)
	require.Len(t, lines, 2)

	house, err := h.svc.HouseBalances(ctx)
	require.NoError(t, err)
	got := map[string]money.Amount{}
	for _, hb := range house {
		got[string(hb.Code)+"/"+hb.Asset] = hb.Balance
	}
	eq(t, "0.796", got["fee_revenue/USDC"], "revenue is credit-normal → positive")
	eq(t, "0.0008", got["fee_revenue/ETH"])
	eq(t, "-10000", got["external/USDC"], "faucet debited external (docs/domain.md §1.1 sign convention)")
	eq(t, "-1", got["external/ETH"])

	// entries and lookups
	entries, err := h.svc.Entries(ctx, ledger.EntriesFilter{AccountID: B})
	require.NoError(t, err)
	kinds := map[string]int{}
	for _, e := range entries {
		kinds[e.Kind]++
		require.NotEmpty(t, e.Postings)
	}
	assert.Equal(t, map[string]int{ledger.KindAdjustment: 1, ledger.KindHold: 1, ledger.KindSettle: 1, ledger.KindRelease: 1}, kinds)
	byKey, err := h.svc.EntryByKey(ctx, "settle:trade:t1")
	require.NoError(t, err)
	assert.Equal(t, "trade", byKey.RefType)
	assert.Equal(t, "t1", byKey.RefID)
	byID, err := h.svc.Entry(ctx, byKey.ID)
	require.NoError(t, err)
	assert.Len(t, byID.Postings, 8)
	_, err = h.svc.EntryByKey(ctx, "nope")
	assert.ErrorIs(t, err, ledger.ErrNotFound)
	byRef, err := h.svc.Entries(ctx, ledger.EntriesFilter{RefType: "order", RefID: "B1"})
	require.NoError(t, err)
	assert.Len(t, byRef, 2, "hold + release")

	// idempotency: replaying the settlement and the hold changes nothing
	before := h.balance(t, ctx, B, "USDC")
	require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
		r2, replayed, err := h.svc.Settle(ctx, tx, ledger.SettleParams{
			TradeID: "t1", BuyerAccountID: B, SellerAccountID: S, BuyerIsTaker: true,
			BaseAsset: "ETH", QuoteAsset: "USDC", Price: amt("1990"), Qty: amt("0.4"), QuoteQty: amt("796"),
			BuyerLimitPrice: amt("2000"), Fees: fees,
		})
		require.NoError(t, err)
		require.True(t, replayed, "same idempotency key must replay")
		require.Equal(t, res.Entry.ID, r2.Entry.ID)
		return nil
	}))
	require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
		_, replayed, err := h.svc.Hold(ctx, tx, ledger.HoldParams{AccountID: B, Asset: "USDC", Amount: amt("2000"), IdempotencyKey: "hold:order:B1"})
		require.True(t, replayed)
		return err
	}))
	after := h.balance(t, ctx, B, "USDC")
	assert.True(t, before.Available.Equal(after.Available) && before.Hold.Equal(after.Hold), "replay must not move money")
	assert.Equal(t, before.Version, after.Version)
	h.assertTrialBalanceZero(t, ctx)

	// metrics
	reg := prometheus.NewRegistry()
	h.svc.WithMetrics(ledger.NewMetrics(reg))
	require.NoError(t, h.svc.ObserveTrialBalance(ctx))
	mfs, err := reg.Gather()
	require.NoError(t, err)
	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
		if mf.GetName() == "ledger_trial_balance_diff" {
			for _, m := range mf.GetMetric() {
				assert.Zero(t, m.GetGauge().GetValue())
			}
		}
	}
	assert.True(t, names["ledger_trial_balance_diff"], "DoD: ledger_trial_balance_diff exists")
}

func TestLedgerRejections(t *testing.T) {
	h := setupLedger(t)
	ctx := context.Background()
	A := h.newSpot(t, ctx)
	h.fund(t, ctx, A, "USDC", "100", "adjust:A")

	t.Run("available < 0 is rejected and nothing is written", func(t *testing.T) {
		err := inTx(ctx, h.all, func(tx pgx.Tx) error {
			_, _, err := h.svc.Hold(ctx, tx, ledger.HoldParams{AccountID: A, Asset: "USDC", Amount: amt("100.000001"), IdempotencyKey: "hold:too-much"})
			return err
		})
		require.ErrorIs(t, err, ledger.ErrInsufficient)
		eq(t, "100", h.balance(t, ctx, A, "USDC").Available)
		_, err = h.svc.EntryByKey(ctx, "hold:too-much")
		assert.ErrorIs(t, err, ledger.ErrNotFound, "the rolled-back entry must not exist")
		// exact amount is fine
		require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
			_, _, err := h.svc.Hold(ctx, tx, ledger.HoldParams{AccountID: A, Asset: "USDC", Amount: amt("100"), IdempotencyKey: "hold:all"})
			return err
		}))
		eq(t, "0", h.balance(t, ctx, A, "USDC").Available)
	})
	t.Run("hold < 0 is rejected", func(t *testing.T) {
		err := inTx(ctx, h.all, func(tx pgx.Tx) error {
			_, _, err := h.svc.Release(ctx, tx, ledger.HoldParams{AccountID: A, Asset: "USDC", Amount: amt("100.5"), IdempotencyKey: "release:too-much"})
			return err
		})
		require.ErrorIs(t, err, ledger.ErrInsufficient)
	})
	t.Run("unknown asset balance starts at zero", func(t *testing.T) {
		err := inTx(ctx, h.all, func(tx pgx.Tx) error {
			_, _, err := h.svc.Hold(ctx, tx, ledger.HoldParams{AccountID: A, Asset: "ETH", Amount: amt("0.1"), IdempotencyKey: "hold:eth"})
			return err
		})
		require.ErrorIs(t, err, ledger.ErrInsufficient)
	})
	t.Run("account kind and existence are checked", func(t *testing.T) {
		ext, err := h.svc.HouseAccount(ledger.HouseExternal)
		require.NoError(t, err)
		err = inTx(ctx, h.all, func(tx pgx.Tx) error {
			_, _, err := h.svc.Post(ctx, tx, ledger.Entry{IdempotencyKey: "bad:bucket", Kind: "test", Postings: []ledger.Posting{
				{AccountID: A, Asset: "USDC", Bucket: ledger.BucketHouse, Direction: ledger.Debit, Amount: amt("1")},
				{AccountID: ext, Asset: "USDC", Bucket: ledger.BucketHouse, Direction: ledger.Credit, Amount: amt("1")},
			}})
			return err
		})
		require.ErrorIs(t, err, ledger.ErrAccountKind)
		err = inTx(ctx, h.all, func(tx pgx.Tx) error {
			_, _, err := h.svc.Post(ctx, tx, ledger.Entry{IdempotencyKey: "bad:account", Kind: "test", Postings: []ledger.Posting{
				{AccountID: "00000000-0000-0000-0000-000000000000", Asset: "USDC", Bucket: ledger.BucketAvailable, Direction: ledger.Debit, Amount: amt("1")},
				{AccountID: ext, Asset: "USDC", Bucket: ledger.BucketHouse, Direction: ledger.Credit, Amount: amt("1")},
			}})
			return err
		})
		require.ErrorIs(t, err, ledger.ErrAccountNotFound)
		_, err = h.svc.Account(ctx, "00000000-0000-0000-0000-000000000000")
		assert.ErrorIs(t, err, ledger.ErrAccountNotFound)
	})
	t.Run("adjustments need a reason; debit adjustments respect available", func(t *testing.T) {
		err := inTx(ctx, h.all, func(tx pgx.Tx) error {
			_, _, err := h.svc.Adjust(ctx, tx, ledger.AdjustParams{AccountID: A, Asset: "USDC", Amount: amt("1"), Direction: ledger.Credit, IdempotencyKey: "adjust:noreason"})
			return err
		})
		require.ErrorIs(t, err, ledger.ErrReasonRequired)
		err = inTx(ctx, h.all, func(tx pgx.Tx) error {
			_, _, err := h.svc.Adjust(ctx, tx, ledger.AdjustParams{AccountID: A, Asset: "USDC", Amount: amt("1"), Direction: ledger.Debit, Reason: "clawback", IdempotencyKey: "adjust:debit"})
			return err
		})
		require.ErrorIs(t, err, ledger.ErrInsufficient, "available is 0 (all held)")
	})
	t.Run("the deferred trigger rejects an unbalanced entry written around the Go code", func(t *testing.T) {
		engine, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_engine"), MaxConns: 2})
		require.NoError(t, err)
		defer engine.Close()
		ext, _ := h.svc.HouseAccount(ledger.HouseExternal)
		tx, err := engine.Begin(ctx)
		require.NoError(t, err)
		var entryID int64
		require.NoError(t, tx.QueryRow(ctx, `INSERT INTO ledger.journal_entries (idempotency_key, kind) VALUES ('raw:unbalanced', 'test') RETURNING id`).Scan(&entryID))
		_, err = tx.Exec(ctx, `INSERT INTO ledger.postings (entry_id, account_id, asset, bucket, direction, amount) VALUES ($1, $2, 'USDC', 'house', 'debit', 5)`, entryID, ext)
		require.NoError(t, err)
		err = tx.Commit(ctx)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "23514", pgErr.Code, "check_violation from ledger.check_entry_balanced")
		assert.Contains(t, pgErr.Message, "unbalanced")
		_, err = h.svc.EntryByKey(ctx, "raw:unbalanced")
		assert.ErrorIs(t, err, ledger.ErrNotFound)
	})
	t.Run("postings are append-only and the api role cannot post", func(t *testing.T) {
		_, err := h.all.Exec(ctx, `UPDATE ledger.postings SET amount = amount + 1`)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "42501", pgErr.Code, "even ex_all has no UPDATE on postings")
		_, err = h.all.Exec(ctx, `DELETE FROM ledger.journal_entries`)
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "42501", pgErr.Code)

		apiPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_api"), MaxConns: 2})
		require.NoError(t, err)
		defer apiPool.Close()
		apiSvc := ledger.New(apiPool, "default")
		require.NoError(t, apiSvc.LoadHouseAccounts(ctx), "api may read")
		err = inTx(ctx, apiPool, func(tx pgx.Tx) error {
			_, _, err := apiSvc.Hold(ctx, tx, ledger.HoldParams{AccountID: A, Asset: "USDC", Amount: amt("1"), IdempotencyKey: "hold:api"})
			return err
		})
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "42501", pgErr.Code, "ex_api must not write the ledger")
		// but it may open spot accounts (registration)
		require.NoError(t, inTx(ctx, apiPool, func(tx pgx.Tx) error {
			_, err := apiSvc.CreateSpotAccount(ctx, tx, nil)
			return err
		}))
	})
	t.Run("account status", func(t *testing.T) {
		require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
			a, err := h.svc.SetAccountStatus(ctx, tx, A, ledger.StatusFrozen)
			require.Equal(t, ledger.StatusFrozen, a.Status)
			return err
		}))
		a, err := h.svc.Account(ctx, A)
		require.NoError(t, err)
		assert.Equal(t, ledger.StatusFrozen, a.Status)
		assert.Equal(t, int32(2), a.Version)
		err = inTx(ctx, h.all, func(tx pgx.Tx) error {
			_, err := h.svc.SetAccountStatus(ctx, tx, A, "asleep")
			return err
		})
		assert.ErrorIs(t, err, ledger.ErrInvalidEntry)
		list, err := h.svc.ListAccounts(ctx, ledger.KindHouse, 100, 0)
		require.NoError(t, err)
		assert.Len(t, list, 6)
	})
	h.assertTrialBalanceZero(t, ctx)
	h.assertCacheMatchesPostings(t, ctx, A)
}

// --- property test: random operation sequences against a Go model ---

type modelBalance struct{ available, hold money.Amount }

type ledgerModel struct {
	bal map[string]map[string]*modelBalance // account → asset → balance
}

func (m *ledgerModel) get(acct, asset string) *modelBalance {
	if m.bal[acct] == nil {
		m.bal[acct] = map[string]*modelBalance{}
	}
	b, ok := m.bal[acct][asset]
	if !ok {
		b = &modelBalance{available: money.Zero, hold: money.Zero}
		m.bal[acct][asset] = b
	}
	return b
}

var seqCounter atomic.Int64

func TestLedgerPropertyRandomOperations(t *testing.T) {
	h := setupLedger(t)
	ctx := context.Background()
	assets := []string{"ETH", "USDC"}

	rapid.Check(t, func(rt *rapid.T) {
		run := seqCounter.Add(1)
		key := func(s string) string { return fmt.Sprintf("p%d:%s", run, s) }
		accts := []string{h.newSpot(rt, ctx), h.newSpot(rt, ctx), h.newSpot(rt, ctx)}
		model := &ledgerModel{bal: map[string]map[string]*modelBalance{}}
		for i, a := range accts {
			usdc := fmt.Sprintf("%d", rapid.IntRange(0, 5000).Draw(rt, "usdc"))
			eth := fmt.Sprintf("%d.%04d", rapid.IntRange(0, 3).Draw(rt, "eth"), rapid.IntRange(0, 9999).Draw(rt, "ethf"))
			if usdc != "0" {
				h.fund(rt, ctx, a, "USDC", usdc, key(fmt.Sprintf("fund-usdc-%d", i)))
				model.get(a, "USDC").available = amt(usdc)
			}
			if !amt(eth).IsZero() {
				h.fund(rt, ctx, a, "ETH", eth, key(fmt.Sprintf("fund-eth-%d", i)))
				model.get(a, "ETH").available = amt(eth)
			}
		}
		type replayable struct {
			apply func(tx pgx.Tx) (bool, error)
		}
		var successes []replayable
		n := rapid.IntRange(1, 25).Draw(rt, "n")
		for i := 0; i < n; i++ {
			acct := rapid.SampledFrom(accts).Draw(rt, "acct")
			asset := rapid.SampledFrom(assets).Draw(rt, "asset")
			amount := amt(fmt.Sprintf("%d.%04d", rapid.IntRange(0, 300).Draw(rt, "amt"), rapid.IntRange(1, 9999).Draw(rt, "amtf")))
			k := key(fmt.Sprintf("op-%d", i))
			op := rapid.IntRange(0, 99).Draw(rt, "op")
			var (
				apply   func(tx pgx.Tx) (bool, error)
				expect  error
				onApply func()
			)
			switch {
			case op < 30: // hold
				b := model.get(acct, asset)
				if b.available.Cmp(amount) < 0 {
					expect = ledger.ErrInsufficient
				}
				apply = func(tx pgx.Tx) (bool, error) {
					_, r, err := h.svc.Hold(ctx, tx, ledger.HoldParams{AccountID: acct, Asset: asset, Amount: amount, IdempotencyKey: k})
					return r, err
				}
				onApply = func() { b.available = b.available.Sub(amount); b.hold = b.hold.Add(amount) }
			case op < 50: // release
				b := model.get(acct, asset)
				if b.hold.Cmp(amount) < 0 {
					expect = ledger.ErrInsufficient
				}
				apply = func(tx pgx.Tx) (bool, error) {
					_, r, err := h.svc.Release(ctx, tx, ledger.HoldParams{AccountID: acct, Asset: asset, Amount: amount, IdempotencyKey: k})
					return r, err
				}
				onApply = func() { b.hold = b.hold.Sub(amount); b.available = b.available.Add(amount) }
			case op < 60: // credit (faucet)
				b := model.get(acct, asset)
				apply = func(tx pgx.Tx) (bool, error) {
					_, r, err := h.svc.Adjust(ctx, tx, ledger.AdjustParams{AccountID: acct, Asset: asset, Amount: amount, Direction: ledger.Credit, Reason: "prop", IdempotencyKey: k})
					return r, err
				}
				onApply = func() { b.available = b.available.Add(amount) }
			case op < 85: // settle: buyer pays quote from hold, seller pays base from hold
				buyer := acct
				seller := rapid.SampledFrom(accts).Draw(rt, "seller")
				if seller == buyer {
					seller = accts[(indexOf(accts, buyer)+1)%len(accts)]
				}
				price := amt(fmt.Sprintf("%d.%02d", rapid.IntRange(1000, 3000).Draw(rt, "price"), rapid.IntRange(0, 99).Draw(rt, "pc")))
				qty := amt(fmt.Sprintf("0.%04d", rapid.IntRange(1, 9999).Draw(rt, "qty")))
				quote := price.Mul(qty)
				limit := price.Add(amt(fmt.Sprintf("0.%02d", rapid.IntRange(0, 99).Draw(rt, "improve"))))
				release := limit.Sub(price).Mul(qty)
				buyerIsTaker := rapid.Bool().Draw(rt, "taker")
				bq, sb := model.get(buyer, "USDC"), model.get(seller, "ETH")
				if bq.hold.Cmp(quote.Add(release)) < 0 || sb.hold.Cmp(qty) < 0 {
					expect = ledger.ErrInsufficient
				}
				apply = func(tx pgx.Tx) (bool, error) {
					_, r, err := h.svc.Settle(ctx, tx, ledger.SettleParams{
						TradeID: k, BuyerAccountID: buyer, SellerAccountID: seller, BuyerIsTaker: buyerIsTaker,
						BaseAsset: "ETH", QuoteAsset: "USDC", Price: price, Qty: qty, QuoteQty: quote, BuyerLimitPrice: limit, Fees: fees,
					})
					return r, err
				}
				onApply = func() {
					buyerBps, sellerBps := fees.MakerBps, fees.TakerBps
					if buyerIsTaker {
						buyerBps, sellerBps = fees.TakerBps, fees.MakerBps
					}
					buyerFee, _ := ledger.ComputeFee(qty, buyerBps, 18)
					sellerFee, _ := ledger.ComputeFee(quote, sellerBps, 6)
					bq.hold = bq.hold.Sub(quote).Sub(release)
					bq.available = bq.available.Add(release)
					model.get(seller, "USDC").available = model.get(seller, "USDC").available.Add(quote.Sub(sellerFee))
					sb.hold = sb.hold.Sub(qty)
					model.get(buyer, "ETH").available = model.get(buyer, "ETH").available.Add(qty.Sub(buyerFee))
				}
			default: // replay an earlier successful operation: must be a no-op
				if len(successes) == 0 {
					continue
				}
				prev := rapid.SampledFrom(successes).Draw(rt, "replay")
				require.NoError(rt, inTx(ctx, h.all, func(tx pgx.Tx) error {
					replayed, err := prev.apply(tx)
					require.NoError(rt, err)
					require.True(rt, replayed, "replaying a posted key must report replayed")
					return nil
				}))
				continue
			}

			err := inTx(ctx, h.all, func(tx pgx.Tx) error {
				replayed, err := apply(tx)
				if err == nil {
					require.False(rt, replayed)
				}
				return err
			})
			if expect != nil {
				require.ErrorIs(rt, err, expect, "op %d", i)
			} else {
				require.NoError(rt, err, "op %d", i)
				onApply()
				successes = append(successes, replayable{apply: apply})
			}
			// cache == model after every operation
			for _, a := range accts {
				for _, as := range assets {
					got := h.balance(rt, ctx, a, as)
					want := model.get(a, as)
					require.Truef(rt, got.Available.Equal(want.available), "op %d: %s %s available db %s model %s", i, a, as, got.Available, want.available)
					require.Truef(rt, got.Hold.Equal(want.hold), "op %d: %s %s hold db %s model %s", i, a, as, got.Hold, want.hold)
				}
			}
		}
		for _, a := range accts {
			h.assertCacheMatchesPostings(rt, ctx, a)
		}
		h.assertTrialBalanceZero(rt, ctx)
	})
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

// 100 goroutines move money between two accounts in both directions; sorted
// row locks mean no deadlock, and the totals are conserved.
func TestLedgerConcurrentTransfers(t *testing.T) {
	h := setupLedger(t)
	ctx := context.Background()
	A := h.newSpot(t, ctx)
	B := h.newSpot(t, ctx)
	h.fund(t, ctx, A, "USDC", "1000", "adjust:cA")
	h.fund(t, ctx, B, "USDC", "1000", "adjust:cB")

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(25)
	const workers, perWorker = 100, 10
	for w := 0; w < workers; w++ {
		w := w
		g.Go(func() error {
			for i := 0; i < perWorker; i++ {
				from, to := A, B
				if w%2 == 1 {
					from, to = B, A
				}
				err := inTx(gctx, h.all, func(tx pgx.Tx) error {
					_, _, err := h.svc.Post(gctx, tx, ledger.Entry{
						IdempotencyKey: fmt.Sprintf("transfer:%d:%d", w, i), Kind: "transfer", Postings: []ledger.Posting{
							{AccountID: from, Asset: "USDC", Bucket: ledger.BucketAvailable, Direction: ledger.Debit, Amount: amt("1")},
							{AccountID: to, Asset: "USDC", Bucket: ledger.BucketAvailable, Direction: ledger.Credit, Amount: amt("1")},
						},
					})
					return err
				})
				if err != nil {
					return fmt.Errorf("worker %d op %d: %w", w, i, err)
				}
			}
			return nil
		})
	}
	require.NoError(t, g.Wait())
	a := h.balance(t, ctx, A, "USDC")
	b := h.balance(t, ctx, B, "USDC")
	eq(t, "2000", a.Available.Add(b.Available), "total is conserved")
	eq(t, "1000", a.Available, "50 workers each way × 10 = 500 in, 500 out")
	assert.Equal(t, int64(2+workers*perWorker), a.Version, "row created at version 1, +1 for the faucet, +1 per transfer")
	h.assertCacheMatchesPostings(t, ctx, A)
	h.assertCacheMatchesPostings(t, ctx, B)
	h.assertTrialBalanceZero(t, ctx)
	entries, err := h.svc.Entries(ctx, ledger.EntriesFilter{RefType: "", Limit: 500})
	require.NoError(t, err)
	assert.Len(t, entries, 500, "Entries caps at 500")
}

// TestLedgerPostRoundTrips pins what a posting costs on the wire: two
// round trips (locks, then writes) however many accounts and assets the
// entry touches. The engine's order flow is built on this number; a change
// that adds a statement fails here rather than in a load test.
func TestLedgerPostRoundTrips(t *testing.T) {
	h := setupLedger(t)
	ctx := context.Background()
	var counter pg.CountingTracer
	pool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_all"), MaxConns: 2, Tracer: &counter})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	svc := ledger.New(pool, "default")
	require.NoError(t, svc.LoadHouseAccounts(ctx))
	a := h.newSpot(t, ctx)
	b := h.newSpot(t, ctx)
	fees, err := svc.HouseAccount(ledger.HouseFeeRevenue)
	require.NoError(t, err)

	post := func(e ledger.Entry) (ledger.JournalEntry, bool, int64, error) {
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		counter.Reset()
		je, replayed, perr := svc.Post(ctx, tx, e)
		n := counter.RoundTrips()
		if perr != nil {
			_ = tx.Rollback(ctx)
		} else {
			require.NoError(t, tx.Commit(ctx))
		}
		return je, replayed, n, perr
	}

	// one entry, four balance rows, six postings: still two round trips
	settle := ledger.Entry{IdempotencyKey: "rt:settle", Kind: "test", Postings: []ledger.Posting{
		{AccountID: a, Asset: "ETH", Bucket: ledger.BucketAvailable, Direction: ledger.Credit, Amount: amt("1")},
		{AccountID: a, Asset: "USDC", Bucket: ledger.BucketAvailable, Direction: ledger.Credit, Amount: amt("2000")},
		{AccountID: b, Asset: "ETH", Bucket: ledger.BucketHold, Direction: ledger.Credit, Amount: amt("3")},
		{AccountID: b, Asset: "USDC", Bucket: ledger.BucketHold, Direction: ledger.Credit, Amount: amt("5")},
		{AccountID: fees, Asset: "ETH", Bucket: ledger.BucketHouse, Direction: ledger.Debit, Amount: amt("4")},
		{AccountID: fees, Asset: "USDC", Bucket: ledger.BucketHouse, Direction: ledger.Debit, Amount: amt("2005")},
	}}
	je, replayed, n, err := post(settle)
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.EqualValues(t, 2, n, "locks, then writes")
	require.Len(t, je.Balances, 4, "one balance per (account, asset), in key order")
	assert.Equal(t, []string{a + "/ETH", a + "/USDC", b + "/ETH", b + "/USDC"}, []string{
		je.Balances[0].AccountID + "/" + je.Balances[0].Asset, je.Balances[1].AccountID + "/" + je.Balances[1].Asset,
		je.Balances[2].AccountID + "/" + je.Balances[2].Asset, je.Balances[3].AccountID + "/" + je.Balances[3].Asset,
	})
	eq(t, "3", je.Balances[2].Hold)
	eq(t, "2000", h.balance(t, ctx, a, "USDC").Available)

	// a replay reads the existing entry instead: the batch, then the entry
	// and its postings
	again, replayed, n, err := post(settle)
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, je.ID, again.ID)
	assert.Len(t, again.Postings, 6)
	assert.LessOrEqual(t, n, int64(3))
	eq(t, "2000", h.balance(t, ctx, a, "USDC").Available, "nothing posted twice")

	// insufficient funds is decided after the first round trip; nothing is
	// written and the transaction is still usable for the caller's own
	// rejection path
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	counter.Reset()
	_, _, err = svc.Post(ctx, tx, ledger.Entry{IdempotencyKey: "rt:short", Kind: "test", Postings: []ledger.Posting{
		{AccountID: a, Asset: "USDC", Bucket: ledger.BucketAvailable, Direction: ledger.Debit, Amount: amt("2001")},
		{AccountID: a, Asset: "USDC", Bucket: ledger.BucketHold, Direction: ledger.Credit, Amount: amt("2001")},
	}})
	assert.ErrorIs(t, err, ledger.ErrInsufficient)
	assert.EqualValues(t, 1, counter.RoundTrips())
	_, execErr := tx.Exec(ctx, "SELECT 1")
	assert.NoError(t, execErr, "the transaction was not aborted by the rejection")
	require.NoError(t, tx.Rollback(ctx))
	var entries int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM ledger.journal_entries WHERE idempotency_key = 'rt:short'").Scan(&entries))
	assert.Equal(t, 0, entries)
}
