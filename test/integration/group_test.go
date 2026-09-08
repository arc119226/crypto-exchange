//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

// counter reads one counter (summed over its label values) from the
// engine's registry.
func (h *tradingHarness) counter(t require.TestingT, name string) float64 {
	fams, err := h.reg.Gather()
	require.NoError(t, err)
	var total float64
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			switch f.GetType() {
			case dto.MetricType_COUNTER:
				total += m.GetCounter().GetValue()
			case dto.MetricType_HISTOGRAM:
				total += float64(m.GetHistogram().GetSampleCount())
			default:
				total += m.GetGauge().GetValue()
			}
		}
	}
	return total
}

func (h *tradingHarness) lastSeq(t require.TestingT, ctx context.Context) int64 {
	var lastSeq int64
	require.NoError(t, h.all.QueryRow(ctx, `SELECT last_seq FROM trading.market_sequences s JOIN registry.markets m ON m.id = s.market_id WHERE m.symbol = $1`, market).Scan(&lastSeq))
	return lastSeq
}

// TestEngineRoundTripBudget pins what a command costs on the wire. These
// numbers are the reason docs/loadtest.md §5's bottleneck is gone; a change
// that adds a statement to the order flow fails here, not in a load test.
func TestEngineRoundTripBudget(t *testing.T) {
	h := setupTrading(t)
	ctx := context.Background()
	var counter pg.CountingTracer
	pool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_engine"), MaxConns: 4, Tracer: &counter})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	h.enginePool = pool
	h.crash(t) // restart the engine over the counting pool
	buyer := h.account(t, ctx, map[string]string{"USDC": "100000"})
	seller := h.account(t, ctx, map[string]string{"ETH": "10"})

	cost := func(name string, budget int64, run func()) {
		counter.Reset()
		run()
		n := counter.RoundTrips()
		t.Logf("%s: %d round trips", name, n)
		assert.LessOrEqual(t, n, budget, "%s exceeds its round-trip budget", name)
	}
	var resting trading.Order
	cost("resting limit order", 3, func() {
		res := h.place(t, ctx, limit(seller, "s1", matching.Sell, "2000", "1"))
		require.Equal(t, trading.StatusOpen, res.Order.Status)
		resting = res.Order
	})
	cost("order with one fill", 4, func() {
		res := h.place(t, ctx, limit(buyer, "b1", matching.Buy, "2000", "0.4"))
		require.Equal(t, trading.StatusFilled, res.Order.Status)
		require.Len(t, res.Trades, 1)
	})
	cost("rejected after reaching the book", 3, func() {
		res := h.place(t, ctx, marketSell(seller, "s2", "0.1")) // no bids: empty_book
		require.Equal(t, trading.StatusRejected, res.Order.Status)
	})
	cost("rejected for insufficient balance", 3, func() {
		res := h.place(t, ctx, limit(buyer, "b2", matching.Buy, "2000", "1000"))
		require.Equal(t, trading.StatusRejected, res.Order.Status)
		require.Equal(t, trading.RejectInsufficientBalance, res.Order.RejectReason)
	})
	cost("cancel", 3, func() {
		o, err := h.svc.CancelOrder(ctx, trading.CancelRequest{AccountID: seller, OrderID: resting.ID})
		require.NoError(t, err)
		require.Equal(t, trading.StatusCancelled, o.Status)
	})
	cost("replay", 4, func() {
		res := h.place(t, ctx, limit(buyer, "b1", matching.Buy, "2000", "0.4"))
		require.True(t, res.Replayed)
	})
	h.assertHoldInvariant(t, ctx)
	h.assertTrialBalanceZero(t, ctx)
}

// projection is what two databases that ran the same commands must agree
// on, with the identifiers that legitimately differ (order ids, trade ids,
// account ids, timestamps) replaced by the accounts' positions.
type projection struct {
	book       []string
	trades     []string
	postings   []string
	outbox     []string            // market-scoped events, in outbox order
	perAccount map[string][]string // each account's events, in outbox order
	balances   []string
}

func (h *tradingHarness) project(t require.TestingT, ctx context.Context, accounts []string) projection {
	idx := map[string]string{}
	for i, a := range accounts {
		idx[a] = fmt.Sprintf("A%d", i)
	}
	name := func(id string) string {
		if n, ok := idx[id]; ok {
			return n
		}
		return "house"
	}
	p := projection{perAccount: map[string][]string{}}
	snap, err := h.engine.Snapshot(ctx, market)
	require.NoError(t, err)
	p.book = append(p.book, fmt.Sprintf("seq=%d", snap.LastSeq))
	for _, side := range [][]matching.RestingOrder{snap.Bids, snap.Asks} {
		for _, o := range side {
			p.book = append(p.book, fmt.Sprintf("%s %s %s rem=%s filled=%s/%s", name(string(o.AccountID)), o.Side, o.Price, o.Remaining, o.FilledQty, o.FilledQuote))
		}
	}
	rows, err := h.all.Query(ctx, `SELECT seq, idx, maker_account_id, taker_account_id, taker_side, price::text, qty::text, maker_fee::text, taker_fee::text FROM trading.trades ORDER BY seq, idx`)
	require.NoError(t, err)
	for rows.Next() {
		var seq, i int64
		var maker, taker, side, price, qty, mfee, tfee string
		require.NoError(t, rows.Scan(&seq, &i, &maker, &taker, &side, &price, &qty, &mfee, &tfee))
		p.trades = append(p.trades, fmt.Sprintf("%d/%d %s->%s %s %s@%s fees %s/%s", seq, i, name(maker), name(taker), side, qty, price, mfee, tfee))
	}
	rows.Close()
	// the faucet postings that funded the accounts are the harness's, in
	// whatever order it funded them; everything after them is the engine's
	rows, err = h.all.Query(ctx, `SELECT e.kind, p.account_id, p.asset, p.bucket, p.direction, p.amount::text FROM ledger.postings p JOIN ledger.journal_entries e ON e.id = p.entry_id WHERE e.kind <> 'adjustment' ORDER BY p.id`)
	require.NoError(t, err)
	for rows.Next() {
		var kind, account, asset, bucket, dir, amount string
		require.NoError(t, rows.Scan(&kind, &account, &asset, &bucket, &dir, &amount))
		p.postings = append(p.postings, fmt.Sprintf("%s %s %s %s %s %s", kind, name(account), asset, bucket, dir, amount))
	}
	rows.Close()
	rows, err = h.all.Query(ctx, `SELECT event_type, account_id, seq, account_seq FROM eventbus.outbox ORDER BY id`)
	require.NoError(t, err)
	for rows.Next() {
		var typ string
		var account *string
		var seq, aseq *int64
		require.NoError(t, rows.Scan(&typ, &account, &seq, &aseq))
		acct, s, as := "-", "-", "-"
		if account != nil {
			acct = name(*account)
		}
		if seq != nil {
			s = fmt.Sprint(*seq)
		}
		if aseq != nil {
			as = fmt.Sprint(*aseq)
		}
		// the market stream is compared row by row; each account's stream
		// on its own, because the interleaving of two accounts' balance
		// events follows their ids' sort order, which differs per database
		if seq != nil {
			p.outbox = append(p.outbox, fmt.Sprintf("%s %s seq=%s aseq=%s", typ, acct, s, as))
		}
		if account != nil {
			p.perAccount[acct] = append(p.perAccount[acct], fmt.Sprintf("%s seq=%s aseq=%s", typ, s, as))
		}
	}
	rows.Close()
	rows, err = h.all.Query(ctx, `SELECT account_id, asset, available::text, hold::text FROM ledger.balances ORDER BY account_id, asset`)
	require.NoError(t, err)
	var bal []string
	for rows.Next() {
		var account, asset, avail, hold string
		require.NoError(t, rows.Scan(&account, &asset, &avail, &hold))
		if n, ok := idx[account]; ok {
			bal = append(bal, fmt.Sprintf("%s %s %s/%s", n, asset, avail, hold))
		}
	}
	rows.Close()
	// balances are keyed by account uuid, whose order differs per database
	p.balances = sortedCopy(bal)
	return p
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// script is a deterministic pseudo-random sequence of commands over a
// fixed set of accounts, addressed by account index so it can run against
// two databases.
type scriptOp struct {
	place  *trading.PlaceOrderRequest // AccountID = "A<i>"
	cancel int                        // index into the placed orders (when place == nil)
}

func randomScript(seed int64, n int) [][]scriptOp {
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // reproducible test data
	var groups [][]scriptOp
	placed := 0
	for len(groups) < n {
		size := 1 + rng.Intn(50)
		var g []scriptOp
		for i := 0; i < size; i++ {
			switch k := rng.Intn(12); {
			case k < 4:
				g = append(g, scriptOp{place: ptr(limitReq(rng, placed, matching.Buy))})
				placed++
			case k < 8:
				g = append(g, scriptOp{place: ptr(limitReq(rng, placed, matching.Sell))})
				placed++
			case k == 8:
				req := marketBuy(fmt.Sprintf("A%d", rng.Intn(6)), fmt.Sprintf("o%d", placed), fmt.Sprintf("%d", 10+rng.Intn(3000)))
				g = append(g, scriptOp{place: &req})
				placed++
			case k == 9:
				req := marketSell(fmt.Sprintf("A%d", rng.Intn(6)), fmt.Sprintf("o%d", placed), fmt.Sprintf("0.%04d", 1+rng.Intn(9999)))
				g = append(g, scriptOp{place: &req})
				placed++
			case k == 10 && placed > 0:
				// a repeated client id: the same request again, or a changed one
				j := rng.Intn(placed)
				req := limit(fmt.Sprintf("A%d", rng.Intn(6)), fmt.Sprintf("o%d", j), matching.Buy, "2000", "0.1")
				g = append(g, scriptOp{place: &req})
			case placed > 0:
				g = append(g, scriptOp{cancel: rng.Intn(placed)})
			}
		}
		if len(g) > 0 {
			groups = append(groups, g)
		}
	}
	return groups
}

func limitReq(rng *rand.Rand, n int, side matching.Side) trading.PlaceOrderRequest {
	price := fmt.Sprintf("%d.%02d", 1950+rng.Intn(100), rng.Intn(100))
	qty := fmt.Sprintf("0.%04d", 30+rng.Intn(9970))
	if rng.Intn(20) == 0 {
		qty = "500" // insufficient balance
	}
	req := limit(fmt.Sprintf("A%d", rng.Intn(6)), fmt.Sprintf("o%d", n), side, price, qty)
	if rng.Intn(4) == 0 {
		req.TimeInForce = matching.IOC
	}
	return req
}

func ptr[T any](v T) *T { return &v }

// runScript executes the script on one harness and returns its projection.
func runScript(t *testing.T, h *tradingHarness, script [][]scriptOp) projection {
	ctx := context.Background()
	accounts := make([]string, 6)
	for i := range accounts {
		accounts[i] = h.account(t, ctx, map[string]string{"USDC": "100000", "ETH": "50"})
	}
	resolve := func(a string) string {
		var i int
		_, _ = fmt.Sscanf(a, "A%d", &i)
		return accounts[i]
	}
	var placed []trading.Order // by script order index (client id o<n>)
	byCID := map[string]trading.Order{}
	for _, g := range script {
		cmds := make([]trading.Command, 0, len(g))
		for _, op := range g {
			if op.place != nil {
				req := *op.place
				req.AccountID = resolve(req.AccountID)
				cmds = append(cmds, trading.Command{Place: &req})
				continue
			}
			target, ok := byCID[fmt.Sprintf("o%d", op.cancel)]
			if !ok {
				continue // not placed yet (placed in this very group): nothing to cancel
			}
			cmds = append(cmds, trading.Command{Cancel: &trading.CancelRequest{AccountID: target.AccountID, OrderID: target.ID}})
		}
		if len(cmds) == 0 {
			continue
		}
		results := h.engine.ExecuteMany(ctx, market, cmds)
		for i, res := range results {
			if res.Err != nil {
				require.ErrorIs(t, res.Err, trading.ErrClientOrderIDMismatch, "command %d of group", i)
				continue
			}
			if cmds[i].Place != nil && !res.Result.Replayed {
				placed = append(placed, res.Result.Order)
				byCID[cmds[i].Place.ClientOrderID] = res.Result.Order
			}
		}
	}
	_ = placed
	h.assertHoldInvariant(t, ctx)
	h.assertTrialBalanceZero(t, ctx)
	h.assertSequenceConsistent(t, ctx)
	return h.project(t, ctx, accounts)
}

// TestEngineGroupEquivalence runs the same random script through an engine
// that commits every command on its own and one that groups up to fifty:
// the books, trades, postings, balances and event streams must be
// identical. This is the property that makes group commit safe to turn on.
func TestEngineGroupEquivalence(t *testing.T) {
	script := randomScript(20260908, 40)
	single := setupTrading(t)
	single.batchSize = 1
	single.crash(t)
	grouped := setupTrading(t)
	grouped.batchSize = 50
	if os.Getenv("GROUP_DEBUG") != "" {
		grouped.log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	grouped.crash(t)

	a := runScript(t, single, script)
	b := runScript(t, grouped, script)
	assert.Equal(t, a.book, b.book, "order book")
	assert.Equal(t, a.trades, b.trades, "trades")
	assert.Equal(t, a.postings, b.postings, "postings")
	if !assert.Equal(t, a.outbox, b.outbox, "outbox") {
		for i := range a.outbox {
			if i >= len(b.outbox) || a.outbox[i] != b.outbox[i] {
				lo := max(0, i-3)
				t.Logf("first outbox divergence at %d:\n single: %v\n grouped: %v", i, a.outbox[lo:min(len(a.outbox), i+4)], b.outbox[lo:min(len(b.outbox), i+4)])
				break
			}
		}
	}
	assert.Equal(t, a.perAccount, b.perAccount, "per-account event streams")
	assert.Equal(t, a.balances, b.balances, "balances")
	assert.Zero(t, single.counter(t, "trading_batch_fallbacks_total"))
	assert.Zero(t, grouped.counter(t, "trading_batch_fallbacks_total"))
	assert.Greater(t, single.counter(t, "trading_batch_size"), grouped.counter(t, "trading_batch_size"), "the grouped engine committed fewer transactions")
	t.Logf("transactions: single %.0f, grouped %.0f", single.counter(t, "trading_batch_size"), grouped.counter(t, "trading_batch_size"))
}

// TestEngineGroupDuplicateClientOrderID: the second and third copies of an
// order in one stretch of the queue are answered as replay and mismatch,
// exactly as if they had arrived later, and the group commits once.
func TestEngineGroupDuplicateClientOrderID(t *testing.T) {
	h := setupTrading(t)
	ctx := context.Background()
	buyer := h.account(t, ctx, map[string]string{"USDC": "100000"})
	before := h.lastSeq(t, ctx)
	same := limit(buyer, "dup", matching.Buy, "2000", "0.1")
	changed := limit(buyer, "dup", matching.Buy, "2001", "0.1")
	other := limit(buyer, "other", matching.Buy, "1999", "0.1")
	results := h.engine.ExecuteMany(ctx, market, []trading.Command{{Place: &same}, {Place: &same}, {Place: &changed}, {Place: &other}})
	require.NoError(t, results[0].Err)
	assert.Equal(t, trading.StatusOpen, results[0].Result.Order.Status)
	require.NoError(t, results[1].Err)
	assert.True(t, results[1].Result.Replayed)
	assert.Equal(t, results[0].Result.Order.ID, results[1].Result.Order.ID)
	assert.ErrorIs(t, results[2].Err, trading.ErrClientOrderIDMismatch)
	require.NoError(t, results[3].Err)
	assert.Equal(t, before+2, h.lastSeq(t, ctx), "two orders reached the book")
	assert.Zero(t, h.counter(t, "trading_batch_fallbacks_total"))
	h.assertHoldInvariant(t, ctx)
}

// TestEngineGroupCancelAfterFill: a cancel that lands in the same group
// as the order that fills its target is answered with the filled order
// and consumes no sequence, instead of failing the group.
func TestEngineGroupCancelAfterFill(t *testing.T) {
	h := setupTrading(t)
	ctx := context.Background()
	buyer := h.account(t, ctx, map[string]string{"USDC": "100000"})
	seller := h.account(t, ctx, map[string]string{"ETH": "10"})
	resting := h.place(t, ctx, limit(seller, "s1", matching.Sell, "2000", "1"))
	before := h.lastSeq(t, ctx)
	buy := limit(buyer, "b1", matching.Buy, "2000", "1")
	results := h.engine.ExecuteMany(ctx, market, []trading.Command{
		{Place: &buy},
		{Cancel: &trading.CancelRequest{AccountID: seller, OrderID: resting.Order.ID}},
		{Cancel: &trading.CancelRequest{AccountID: seller, OrderID: resting.Order.ID}},
	})
	require.NoError(t, results[0].Err)
	assert.Equal(t, trading.StatusFilled, results[0].Result.Order.Status)
	require.NoError(t, results[1].Err)
	assert.Equal(t, trading.StatusFilled, results[1].Result.Order.Status, "the cancel sees the fill")
	eq(t, "1", results[1].Result.Order.FilledQty)
	require.NoError(t, results[2].Err, "the duplicate cancel waited for the next group")
	assert.Equal(t, trading.StatusFilled, results[2].Result.Order.Status)
	assert.Equal(t, before+1, h.lastSeq(t, ctx), "only the fill consumed a sequence")
	assert.Zero(t, h.counter(t, "trading_batch_fallbacks_total"))
	h.assertHoldInvariant(t, ctx)
	h.assertTrialBalanceZero(t, ctx)
}

// TestEngineGroupFallback: a group that fails as a whole is rolled back,
// the book rebuilt, and its commands re-run one per transaction in order,
// so the good ones commit and only the bad one fails.
func TestEngineGroupFallback(t *testing.T) {
	h := setupTrading(t)
	ctx := context.Background()
	failNext := 0
	h.fault = func(_ string, commands int) error {
		if failNext > 0 && commands > 1 {
			failNext--
			return errors.New("injected")
		}
		return nil
	}
	h.crash(t)
	buyer := h.account(t, ctx, map[string]string{"USDC": "100000"})
	seller := h.account(t, ctx, map[string]string{"ETH": "10"})

	failNext = 1
	sell := limit(seller, "s1", matching.Sell, "2000", "1")
	buy := limit(buyer, "b1", matching.Buy, "2000", "0.4")
	broke := limit(buyer, "b2", matching.Buy, "2000", "1000") // insufficient: a business outcome, not a failure
	results := h.engine.ExecuteMany(ctx, market, []trading.Command{{Place: &sell}, {Place: &buy}, {Place: &broke}})
	for i, res := range results {
		require.NoError(t, res.Err, "command %d", i)
	}
	assert.Equal(t, trading.StatusOpen, results[0].Result.Order.Status)
	assert.Equal(t, trading.StatusFilled, results[1].Result.Order.Status)
	assert.Equal(t, trading.StatusRejected, results[2].Result.Order.Status)
	assert.EqualValues(t, 1, h.counter(t, "trading_batch_fallbacks_total"))
	assert.EqualValues(t, 1, h.counter(t, "engine_rebuilds_total"))

	var orders int
	require.NoError(t, h.all.QueryRow(ctx, `SELECT count(*) FROM trading.orders`).Scan(&orders))
	assert.Equal(t, 3, orders, "each command committed exactly once")
	snap, err := h.engine.Snapshot(ctx, market)
	require.NoError(t, err)
	require.Len(t, snap.Asks, 1)
	eq(t, "0.6", snap.Asks[0].Remaining)
	h.assertHoldInvariant(t, ctx)
	h.assertTrialBalanceZero(t, ctx)
	h.assertSequenceConsistent(t, ctx)

	// the book after the fallback is the database's: a restart agrees
	h.fault = nil
	h.crash(t)
	after, err := h.engine.Snapshot(ctx, market)
	require.NoError(t, err)
	assert.True(t, snap.Equal(after))
	assert.False(t, strings.Contains(fmt.Sprint(results), "injected"))
}

var _ = prometheus.NewRegistry
