package marketdata_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/marketdata"
	"github.com/arc119226/crypto-exchange/internal/matching"
	"github.com/arc119226/crypto-exchange/internal/money"
)

var update = flag.Bool("update", false, "rewrite the marketdata golden files from the current implementation")

// --- the closing rules, one scenario each ---

func TestProjectorRestingLimitClosesAtAccept(t *testing.T) {
	p := live(t)
	ds := apply(t, p, accepted(t, 1, "b1", "buy", "limit", "gtc", ptr("1990"), ptr("0.5")))
	require.Len(t, ds, 1)
	assert.Equal(t, uint64(1), ds[0].Seq)
	assert.Equal(t, `[["1990","0.5"]]`, mustJSON(t, ds[0].Bids))
	assert.Equal(t, `[]`, mustJSON(t, ds[0].Asks))
	_, open := p.OpenSince()
	assert.False(t, open, "a GTC limit that cannot cross is closed at the accept")
	assert.Equal(t, uint64(1), p.Seq())

	// the opposite side, not crossing either (ask above the bid)
	ds = apply(t, p, accepted(t, 2, "a1", "sell", "limit", "gtc", ptr("2000"), ptr("1")))
	require.Len(t, ds, 1)
	assert.Equal(t, `[["2000","1"]]`, mustJSON(t, ds[0].Asks))
}

func TestProjectorCrossingLimitWaitsForTheTaker(t *testing.T) {
	p := live(t)
	apply(t, p, accepted(t, 1, "a1", "sell", "limit", "gtc", ptr("2000"), ptr("1")))
	apply(t, p, accepted(t, 2, "a2", "sell", "limit", "gtc", ptr("2001"), ptr("1")))

	// a bid at 2001 sweeps a1 and half of a2, then rests? No: it takes a1
	// (1) and 0.5 of a2, is exhausted, and is filled.
	ds := apply(t, p, accepted(t, 3, "b1", "buy", "limit", "gtc", ptr("2001"), ptr("1.5")))
	require.Empty(t, ds, "crossing: the accept alone closes nothing")
	_, open := p.OpenSince()
	assert.True(t, open)
	assert.Equal(t, uint64(2), p.Seq(), "the book stays at the last closed command while one is open")
	snap := p.Depth(0)
	assert.Len(t, snap.Asks, 2, "a snapshot while a command is open shows the pre-command book")

	ds = apply(t, p,
		trade(t, 3, "t1", "a1", "b1", "buy", amt("2000"), amt("1")),
		filled(t, 3, "a1"),
		trade(t, 3, "t2", "a2", "b1", "buy", amt("2001"), amt("0.5")),
		updated(t, 3, "a2", amt("0.5")),
	)
	require.Empty(t, ds, "maker events do not close the command")
	ds = apply(t, p, filled(t, 3, "b1"))
	require.Len(t, ds, 1, "the taker's own terminal event closes it")
	assert.Equal(t, uint64(3), ds[0].Seq)
	assert.Equal(t, `[]`, mustJSON(t, ds[0].Bids), "the taker never rested: its level is untouched")
	assert.Equal(t, `[["2000","0"],["2001","0.5"]]`, mustJSON(t, ds[0].Asks))
	assert.Equal(t, uint64(3), p.Seq())
}

func TestProjectorPartialFillThenRest(t *testing.T) {
	p := live(t)
	apply(t, p, accepted(t, 1, "a1", "sell", "limit", "gtc", ptr("2000"), ptr("0.4")))
	ds := apply(t, p,
		accepted(t, 2, "b1", "buy", "limit", "gtc", ptr("2000"), ptr("1")),
		trade(t, 2, "t1", "a1", "b1", "buy", amt("2000"), amt("0.4")),
		filled(t, 2, "a1"),
		updated(t, 2, "b1", amt("0.6")),
	)
	require.Len(t, ds, 1)
	assert.Equal(t, `[["2000","0.6"]]`, mustJSON(t, ds[0].Bids), "the residual rests at the taker's price")
	assert.Equal(t, `[["2000","0"]]`, mustJSON(t, ds[0].Asks))
	d := p.Depth(0)
	require.Len(t, d.Bids, 1)
	assert.Equal(t, 1, d.Bids[0].Orders)
	assert.Empty(t, d.Asks)
}

func TestProjectorIOCRemainderAndMarketOrder(t *testing.T) {
	p := live(t)
	apply(t, p, accepted(t, 1, "a1", "sell", "limit", "gtc", ptr("2000"), ptr("0.4")))

	// IOC limit: partially filled, remainder cancelled; never rests
	ds := apply(t, p,
		accepted(t, 2, "b1", "buy", "limit", "ioc", ptr("2000"), ptr("1")),
		trade(t, 2, "t1", "a1", "b1", "buy", amt("2000"), amt("0.4")),
		filled(t, 2, "a1"),
		cancelled(t, 2, "b1", "ioc", amt("0.6")),
	)
	require.Len(t, ds, 1)
	assert.Equal(t, `[]`, mustJSON(t, ds[0].Bids))
	assert.Equal(t, `[["2000","0"]]`, mustJSON(t, ds[0].Asks))

	// an IOC limit that does not cross is cancelled at once, and the accept alone does not close
	apply(t, p, accepted(t, 3, "a2", "sell", "limit", "gtc", ptr("2010"), ptr("1")))
	ds = apply(t, p, accepted(t, 4, "b2", "buy", "limit", "ioc", ptr("2000"), ptr("1")))
	require.Empty(t, ds)
	ds = apply(t, p, cancelled(t, 4, "b2", "ioc", amt("1")))
	require.Len(t, ds, 1)
	assert.Equal(t, `[]`, mustJSON(t, ds[0].Bids), "added then removed inside one command: no change")
	assert.Equal(t, `[]`, mustJSON(t, ds[0].Asks))

	// market buy: no price, never in the book, closed by its own filled
	ds = apply(t, p,
		accepted(t, 5, "m1", "buy", "market", "ioc", nil, nil),
		trade(t, 5, "t2", "a2", "m1", "buy", amt("2010"), amt("0.5")),
		updated(t, 5, "a2", amt("0.5")),
	)
	require.Empty(t, ds)
	ds = apply(t, p, filled(t, 5, "m1"))
	require.Len(t, ds, 1)
	assert.Equal(t, `[["2010","0.5"]]`, mustJSON(t, ds[0].Asks))

	// market sell into an emptying book: remainder cancelled
	ds = apply(t, p,
		accepted(t, 6, "m2", "sell", "market", "ioc", nil, ptr("2")),
	)
	require.Empty(t, ds)
	ds = apply(t, p, cancelled(t, 6, "m2", "ioc", amt("2")))
	require.Len(t, ds, 1, "a market remainder that never rested closes with an empty delta")
	assert.Equal(t, `[]`, mustJSON(t, ds[0].Bids))
	assert.Equal(t, `[]`, mustJSON(t, ds[0].Asks))
}

func TestProjectorCancelCommandAndSelfTrade(t *testing.T) {
	p := live(t)
	apply(t, p, accepted(t, 1, "b1", "buy", "limit", "gtc", ptr("1990"), ptr("0.5")))
	apply(t, p, accepted(t, 2, "b2", "buy", "limit", "gtc", ptr("1990"), ptr("0.25")))

	ds := apply(t, p, cancelled(t, 3, "b1", "user", amt("0.5")))
	require.Len(t, ds, 1, "a cancel command is one event, one delta")
	assert.Equal(t, `[["1990","0.25"]]`, mustJSON(t, ds[0].Bids))

	// STP cancel_newest: the taker is cancelled before trading
	ds = apply(t, p, accepted(t, 4, "a1", "sell", "limit", "gtc", ptr("1990"), ptr("1")))
	require.Empty(t, ds)
	ds = apply(t, p, cancelled(t, 4, "a1", "self_trade", amt("1")))
	require.Len(t, ds, 1)
	assert.Equal(t, `[]`, mustJSON(t, ds[0].Asks))
	assert.Equal(t, `[]`, mustJSON(t, ds[0].Bids))
	assert.Equal(t, uint64(4), p.Seq())
}

func TestProjectorRejectedWithSeqIsAnEmptyDelta(t *testing.T) {
	p := live(t)
	ds := apply(t, p, rejected(t, 1, "m1"))
	require.Len(t, ds, 1)
	assert.Equal(t, uint64(1), ds[0].Seq)
	assert.Equal(t, `[]`, mustJSON(t, ds[0].Bids))
	assert.Equal(t, uint64(1), p.Seq(), "the seq is consumed so clients stay contiguous")

	// a pre-book rejection has no seq and is ignored
	e := rejected(t, 2, "m2")
	e.Seq = nil
	ds = apply(t, p, e)
	assert.Empty(t, ds)
	assert.Equal(t, uint64(1), p.Seq())
}

func TestProjectorIgnoresOtherMarketsDuplicatesAndAccountEvents(t *testing.T) {
	p := live(t)
	apply(t, p, accepted(t, 1, "b1", "buy", "limit", "gtc", ptr("1990"), ptr("0.5")))

	other := accepted(t, 2, "x1", "buy", "limit", "gtc", ptr("1"), ptr("1"))
	o := "BTC-USDC"
	other.MarketID = &o
	assert.Empty(t, apply(t, p, other))
	assert.Equal(t, uint64(1), p.Seq())

	assert.Empty(t, apply(t, p, accepted(t, 1, "b1", "buy", "limit", "gtc", ptr("1990"), ptr("0.5"))), "redelivery")
	noSeq := accepted(t, 2, "b9", "buy", "limit", "gtc", ptr("1990"), ptr("0.5"))
	noSeq.Seq, noSeq.MarketID = nil, nil
	assert.Empty(t, apply(t, p, noSeq), "account-only events carry no seq")
}

func TestProjectorGapAndInconsistency(t *testing.T) {
	p := live(t)
	apply(t, p, accepted(t, 1, "b1", "buy", "limit", "gtc", ptr("1990"), ptr("0.5")))
	_, err := p.Apply(accepted(t, 3, "b2", "buy", "limit", "gtc", ptr("1990"), ptr("0.5")))
	require.ErrorIs(t, err, marketdata.ErrGap)

	p = live(t)
	_, err = p.Apply(trade(t, 1, "t1", "nobody", "b1", "buy", amt("1"), amt("1")))
	require.ErrorIs(t, err, marketdata.ErrInconsistent, "a trade outside a command")

	p = live(t)
	apply(t, p, accepted(t, 1, "a1", "sell", "limit", "gtc", ptr("2000"), ptr("1")))
	apply(t, p, accepted(t, 2, "b1", "buy", "limit", "gtc", ptr("2000"), ptr("1")))
	_, err = p.Apply(trade(t, 2, "t1", "ghost", "b1", "buy", amt("2000"), amt("1")))
	require.NoError(t, err, "collected, applied at close")
	_, err = p.Apply(filled(t, 2, "b1"))
	require.ErrorIs(t, err, marketdata.ErrInconsistent, "a trade against an order the book does not hold")

	p = live(t)
	apply(t, p, accepted(t, 1, "b1", "buy", "limit", "gtc", ptr("1990"), ptr("0.5")))
	_, err = p.Apply(cancelled(t, 2, "b1", "user", amt("0.4")))
	require.ErrorIs(t, err, marketdata.ErrInconsistent, "cancel remaining disagrees with the book")

	p = live(t)
	_, err = p.Apply(cancelled(t, 1, "unknown", "user", amt("0.4")))
	require.ErrorIs(t, err, marketdata.ErrInconsistent, "cancel of an order the book never saw")
}

func TestProjectorLateCloseAndFlush(t *testing.T) {
	p := live(t)
	apply(t, p, accepted(t, 1, "a1", "sell", "limit", "gtc", ptr("2000"), ptr("1")))
	// a crossing accept whose terminal event never arrives (a defect this
	// projector guards against rather than one the engine produces)
	require.Empty(t, apply(t, p, accepted(t, 2, "b1", "buy", "limit", "gtc", ptr("2000"), ptr("0.5"))))
	_, open := p.OpenSince()
	require.True(t, open)

	ds := apply(t, p, accepted(t, 3, "b2", "buy", "limit", "gtc", ptr("1990"), ptr("0.5")))
	require.Len(t, ds, 2, "the next command closes the stuck one first")
	assert.Equal(t, uint64(2), ds[0].Seq)
	assert.Equal(t, uint64(3), ds[1].Seq)

	require.Empty(t, apply(t, p, accepted(t, 4, "b3", "buy", "limit", "gtc", ptr("2000"), ptr("0.5"))))
	d, err := p.Flush()
	require.NoError(t, err)
	require.NotNil(t, d)
	assert.Equal(t, uint64(4), d.Seq)
	d, err = p.Flush()
	require.NoError(t, err)
	assert.Nil(t, d, "nothing open")
}

func TestProjectorRebuildBuffersAndReplays(t *testing.T) {
	p := marketdata.NewBookProjector(market, nil)
	assert.Equal(t, marketdata.Cold, p.State())
	assert.Empty(t, apply(t, p, accepted(t, 1, "b1", "buy", "limit", "gtc", ptr("1990"), ptr("0.5"))), "cold: ignored")

	p.MarkRebuilding()
	assert.Equal(t, marketdata.Rebuilding, p.State())
	// events committed while the snapshot is read: seq 5 is inside the
	// snapshot, 6 and 7 are after it
	assert.Empty(t, apply(t, p, accepted(t, 5, "old", "buy", "limit", "gtc", ptr("1980"), ptr("1"))))
	assert.Empty(t, apply(t, p, accepted(t, 6, "b6", "buy", "limit", "gtc", ptr("1985"), ptr("1"))))
	assert.Empty(t, apply(t, p, cancelled(t, 7, "b5", "user", amt("0.5"))))

	ds, err := p.Restore(marketdata.BookSnapshot{Market: market, Seq: 5, Orders: []marketdata.RestingOrder{
		{ID: "b5", Side: marketdata.Buy, Price: amt("1990"), Remaining: amt("0.5")},
		{ID: "old", Side: marketdata.Buy, Price: amt("1980"), Remaining: amt("1")},
	}})
	require.NoError(t, err)
	assert.Equal(t, marketdata.Live, p.State())
	require.Len(t, ds, 2, "seq 5 dropped, 6 and 7 replayed")
	assert.Equal(t, uint64(6), ds[0].Seq)
	assert.Equal(t, uint64(7), ds[1].Seq)
	assert.Equal(t, uint64(7), p.Seq())
	d := p.Depth(0)
	require.Len(t, d.Bids, 2)
	assert.Equal(t, "1985", d.Bids[0].Price.String())
	assert.Equal(t, "1980", d.Bids[1].Price.String())

	// a buffered gap surfaces at Restore and leaves the projector rebuilding
	p.MarkRebuilding()
	apply(t, p, accepted(t, 20, "b20", "buy", "limit", "gtc", ptr("1985"), ptr("1")))
	_, err = p.Restore(marketdata.BookSnapshot{Market: market, Seq: 10})
	require.ErrorIs(t, err, marketdata.ErrGap)
	assert.Equal(t, marketdata.Rebuilding, p.State())
	_, err = p.Restore(marketdata.BookSnapshot{Market: market, Seq: 10})
	require.NoError(t, err, "the buffer was dropped with the error; a fresh snapshot restores")
	assert.Equal(t, uint64(10), p.Seq())

	// the buffer is bounded
	p = marketdata.NewBookProjector(market, nil).WithRebuildBuffer(2)
	p.MarkRebuilding()
	apply(t, p, accepted(t, 1, "x1", "buy", "limit", "gtc", ptr("1"), ptr("1")))
	apply(t, p, accepted(t, 2, "x2", "buy", "limit", "gtc", ptr("1"), ptr("1")))
	_, err = p.Apply(accepted(t, 3, "x3", "buy", "limit", "gtc", ptr("1"), ptr("1")))
	require.ErrorIs(t, err, marketdata.ErrBufferOverflow)
	assert.Equal(t, marketdata.Rebuilding, p.State())

	_, err = live(t).Restore(marketdata.BookSnapshot{Market: market})
	require.ErrorIs(t, err, marketdata.ErrInconsistent, "restore while live")
}

func TestApplyDelta(t *testing.T) {
	bids, asks := map[string]money.Amount{"1990": amt("1")}, map[string]money.Amount{}
	marketdata.ApplyDelta(bids, asks, marketdata.Delta{
		Bids: []marketdata.PriceLevel{{Price: amt("1990"), Qty: amt("0")}, {Price: amt("1985"), Qty: amt("2")}},
		Asks: []marketdata.PriceLevel{{Price: amt("2000"), Qty: amt("0.5")}},
	})
	assert.Equal(t, map[string]money.Amount{"1985": amt("2")}, bids)
	assert.Len(t, asks, 1)
}

// --- the shadow book against the real one ---

// TestPropertyProjectorTracksTheBook runs random command scripts through
// matching.Book, turns the events into the envelopes the runner would
// append, feeds them to the projector, and checks after every command that
// the shadow book equals the engine's aggregated view and that a client
// applying the deltas reaches the same levels. It also proves the closing
// rules never leave a command open: no delta is ever late.
func TestPropertyProjectorTracksTheBook(t *testing.T) {
	for _, cfg := range []struct {
		name string
		cfg  matching.MarketConfig
	}{
		{"cancel_newest", ethUSDC(matching.STPCancelNewest, nil)},
		{"allow_self_trade", ethUSDC(matching.STPAllow, nil)},
		{"with_band", ethUSDC(matching.STPCancelNewest, ptrI32(50))},
	} {
		t.Run(cfg.name, func(t *testing.T) {
			rapid.Check(t, func(rt *rapid.T) {
				runProjectorProperty(rt, t, cfg.cfg)
			})
		})
	}
}

func runProjectorProperty(rt *rapid.T, t *testing.T, cfg matching.MarketConfig) {
	book, err := matching.New(cfg)
	require.NoError(rt, err)
	p := live(t)
	bids, asks := map[string]money.Amount{}, map[string]money.Amount{}
	var live []matching.OrderID
	n := rapid.IntRange(1, 60).Draw(rt, "commands")
	var projSeq uint64
	for i := 1; i <= n; i++ {
		cmd := genCommand(rt, uint64(i), live)
		evs, err := book.Apply(cmd)
		require.NoError(rt, err)
		if _, rejectedCancel := evs[0].(matching.CancelRejected); rejectedCancel {
			continue // the runner rolls back: no outbox rows, no seq consumed
		}
		projSeq++
		var deltas []marketdata.Delta
		for _, e := range envelopesOf(t, cmd, evs, projSeq) {
			ds, err := p.Apply(e)
			require.NoError(rt, err, "seq %d: %v", projSeq, e.EventType)
			deltas = append(deltas, ds...)
		}
		require.Len(rt, deltas, 1, "exactly one delta per command (seq %d, events %v)", projSeq, kinds(evs))
		require.Equal(rt, projSeq, deltas[0].Seq)
		_, open := p.OpenSince()
		require.False(rt, open, "no command left open after seq %d (%v)", projSeq, kinds(evs))
		requireSameDepth(rt, book.Depth(0), p.Depth(0))
		marketdata.ApplyDelta(bids, asks, deltas[0])
		wantBids, wantAsks := depthMaps(p.Depth(0))
		requireSameMaps(rt, wantBids, bids, "bids")
		requireSameMaps(rt, wantAsks, asks, "asks")
		live = liveOrders(book)
	}
}

func kinds(evs []matching.Event) []matching.EventKind {
	out := make([]matching.EventKind, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Kind())
	}
	return out
}

func liveOrders(b *matching.Book) []matching.OrderID {
	snap := b.Snapshot()
	out := make([]matching.OrderID, 0, len(snap.Bids)+len(snap.Asks))
	for _, o := range snap.Bids {
		out = append(out, o.OrderID)
	}
	for _, o := range snap.Asks {
		out = append(out, o.OrderID)
	}
	return out
}

func ethUSDC(stp matching.SelfTradePolicy, band *int32) matching.MarketConfig {
	return matching.MarketConfig{
		Symbol: market, PriceTick: amt("0.01"), QtyStep: amt("0.0001"), MinNotional: amt("5"),
		BaseScale: 18, QuoteScale: 6, SelfTradePolicy: stp, MaxSlippageBps: band,
	}
}

func ptrI32(v int32) *int32 { return &v }

// genCommand mirrors internal/matching's property generator: limit orders
// (some IOC), market buys and sells, an occasional off-tick order, and
// cancels of live orders (sometimes by the wrong owner, which the runner
// would refuse before the book -- here it is a rejected cancel and skipped).
func genCommand(rt *rapid.T, seq uint64, live []matching.OrderID) matching.Command {
	accounts := []string{"A", "B", "C"}
	acct := matching.AccountID(rapid.SampledFrom(accounts).Draw(rt, "account"))
	id := matching.OrderID(fmt.Sprintf("o%d", seq))
	price := func() money.Amount {
		return amt(fmt.Sprintf("%d.%02d", 1990+rapid.IntRange(0, 20).Draw(rt, "p"), rapid.IntRange(0, 99).Draw(rt, "c")))
	}
	qty := func() money.Amount { return amt(fmt.Sprintf("0.%04d", rapid.IntRange(1, 9999).Draw(rt, "q"))) }
	kind := rapid.IntRange(0, 99).Draw(rt, "kind")
	newCmd := func(o matching.NewOrder) matching.Command {
		return matching.Command{Seq: seq, Timestamp: ts(seq), New: &o}
	}
	switch {
	case kind < 50:
		side := matching.Buy
		if rapid.Bool().Draw(rt, "sell") {
			side = matching.Sell
		}
		o := matching.NewOrder{OrderID: id, AccountID: acct, Side: side, Type: matching.Limit, Price: price(), Qty: qty()}
		if rapid.IntRange(0, 4).Draw(rt, "ioc") == 0 {
			o.TimeInForce = matching.IOC
		}
		return newCmd(o)
	case kind < 62:
		return newCmd(matching.NewOrder{OrderID: id, AccountID: acct, Side: matching.Buy, Type: matching.Market,
			QuoteQty: amt(fmt.Sprintf("%d.%02d", rapid.IntRange(5, 3000).Draw(rt, "quote"), rapid.IntRange(0, 99).Draw(rt, "qc")))})
	case kind < 74:
		return newCmd(matching.NewOrder{OrderID: id, AccountID: acct, Side: matching.Sell, Type: matching.Market, Qty: qty()})
	case kind < 80:
		return newCmd(matching.NewOrder{OrderID: id, AccountID: acct, Side: matching.Buy, Type: matching.Limit, Price: amt(price().String() + "5"), Qty: qty()})
	default:
		if len(live) == 0 {
			return matching.Command{Seq: seq, Timestamp: ts(seq), Cancel: &matching.Cancel{OrderID: "missing"}}
		}
		target := rapid.SampledFrom(live).Draw(rt, "cancel")
		c := &matching.Cancel{OrderID: target}
		if rapid.IntRange(0, 3).Draw(rt, "owner") == 0 {
			c.AccountID = acct
		}
		return matching.Command{Seq: seq, Timestamp: ts(seq), Cancel: c}
	}
}

// --- golden: the matching fixtures, as deltas ---

const (
	fixtureDir = "../../test/fixtures/matching"
	goldenDir  = "../../test/golden/marketdata"
)

// TestGoldenDeltas replays every matching fixture script through the real
// book and the projector and compares the deltas with
// test/golden/marketdata/<name>.deltas.jsonl. `-update` rewrites them.
func TestGoldenDeltas(t *testing.T) {
	scripts, err := filepath.Glob(filepath.Join(fixtureDir, "*.jsonl"))
	require.NoError(t, err)
	var found int
	for _, path := range scripts {
		if strings.HasSuffix(path, ".events.jsonl") {
			continue
		}
		found++
		name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			script, err := matching.ReadScript(bytes.NewReader(raw))
			require.NoError(t, err)
			book, err := matching.New(script.Market)
			require.NoError(t, err)
			p := live(t)
			var out bytes.Buffer
			var projSeq uint64
			for _, cmd := range script.Commands {
				evs, err := book.Apply(cmd)
				require.NoError(t, err)
				if _, rejectedCancel := evs[0].(matching.CancelRejected); rejectedCancel {
					continue
				}
				projSeq++
				deltas := apply(t, p, envelopesOf(t, cmd, evs, projSeq)...)
				require.Len(t, deltas, 1)
				requireSameDepth(t, book.Depth(0), p.Depth(0))
				line, err := json.Marshal(deltas[0])
				require.NoError(t, err)
				out.Write(line)
				out.WriteByte('\n')
			}
			goldenPath := filepath.Join(goldenDir, name+".deltas.jsonl")
			if *update {
				require.NoError(t, os.MkdirAll(goldenDir, 0o755))
				require.NoError(t, os.WriteFile(goldenPath, out.Bytes(), 0o644))
				return
			}
			want, err := os.ReadFile(goldenPath)
			require.NoError(t, err, "missing golden; run with -update")
			assert.Equal(t, string(want), out.String(), "deltas differ from golden %s", goldenPath)
			for _, line := range bytes.Split(bytes.TrimSpace(want), []byte{'\n'}) {
				var d marketdata.Delta
				require.NoError(t, json.Unmarshal(line, &d), string(line))
			}
		})
	}
	require.GreaterOrEqual(t, found, 5)
}

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

var _ = eventbus.Envelope{}
