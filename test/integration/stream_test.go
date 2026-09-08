//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/marketdata"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/stream"
)

// streamHarness is the api harness plus the stream role: NATS, the outbox
// relay, the ordered consumers, the feed and the WebSocket server on a
// listener with a small send buffer (the slow-client test needs the kernel
// to push back early).
type streamHarness struct {
	*apiHarness
	ctx     context.Context
	cancel  context.CancelFunc
	js      jetstream.JetStream
	feed    *stream.Feed
	server  *stream.Server
	ws      *httptest.Server
	reg     *prometheus.Registry
	m       *stream.Metrics
	md      *marketdata.Metrics
	cfg     stream.Config
	streams []*eventbus.OrderedSubscription
}

func setupStream(t *testing.T, cfg stream.Config) *streamHarness {
	t.Helper()
	h := setupAPI(t)
	url := startNATS(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	nc, err := nats.Connect(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = nc.Drain() })
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	require.NoError(t, eventbus.EnsureStreams(ctx, js))
	for _, name := range []string{eventbus.StreamTrading, eventbus.StreamChain} {
		st, err := js.Stream(ctx, name)
		require.NoError(t, err)
		require.NoError(t, st.Purge(ctx))
	}
	reg := prometheus.NewRegistry()
	relay := eventbus.NewRelay(h.all, eventbus.NewJetStreamPublisher(js), eventbus.RelayConfig{PollInterval: 20 * time.Millisecond}, h.log)
	go func() { _ = relay.Run(ctx) }()

	sh := &streamHarness{apiHarness: h, ctx: ctx, cancel: cancel, js: js, reg: reg, cfg: cfg, m: stream.NewMetrics(reg), md: marketdata.NewMetrics(reg)}
	sh.startStream(t)
	return sh
}

// startStream builds the feed and the server; stopStream + startStream is
// a stream role restart.
func (h *streamHarness) startStream(t *testing.T) {
	t.Helper()
	streamPool, err := pg.Open(h.ctx, pg.PoolConfig{DSN: h.DSN("ex_stream"), MaxConns: 4})
	require.NoError(t, err)
	t.Cleanup(streamPool.Close)
	m, md := h.m, h.md
	hub := stream.NewHub(m)
	store := marketdata.NewStore(streamPool, "default")
	h.feed = stream.NewFeed(h.cfg, store, h.cache, hub, nil, m, md, 1000, h.log)
	verifier, err := h.auth.Signer().VerifierFor()
	require.NoError(t, err)
	h.server = stream.New(h.ctx, h.cfg, stream.Deps{
		Tenant: "default", Feed: h.feed, Hub: hub, Verifier: verifier,
		Outbox: eventbus.NewOutboxReader(streamPool), Accounts: store, Metrics: m, Log: h.log,
	})
	for _, sub := range []struct {
		name, stream string
		domains      []string
	}{{"stream-trading", eventbus.StreamTrading, []string{"order", "trade", "balance"}}, {"stream-chain", eventbus.StreamChain, []string{"deposit", "withdrawal"}}} {
		var filters []string
		for _, d := range sub.domains {
			filters = append(filters, eventbus.SubjectPrefix+"."+d+".*.default.*")
		}
		os, err := eventbus.SubscribeOrdered(h.ctx, h.js, eventbus.OrderedConfig{Name: sub.name, Stream: sub.stream, FilterSubjects: filters}, h.log, h.feed.Handle)
		require.NoError(t, err)
		h.streams = append(h.streams, os)
	}
	h.feed.Start(h.ctx)
	go func() { _ = h.feed.Run(h.ctx) }()

	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) { serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, 4096) })
		if err != nil {
			return err
		}
		return serr
	}}
	ln, err := lc.Listen(h.ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	h.ws = &httptest.Server{Listener: ln, Config: &http.Server{Handler: h.server.Handler(), ReadHeaderTimeout: 5 * time.Second}}
	h.ws.Start()
	t.Cleanup(h.ws.Close)
	require.Eventually(t, func() bool { return h.feed.Ready() == nil }, 10*time.Second, 20*time.Millisecond, "feed ready")
}

func (h *streamHarness) stopStream(t *testing.T) {
	t.Helper()
	for _, s := range h.streams {
		s.Stop()
	}
	h.streams = nil
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.server.CloseAll(ctx)
	h.ws.Close()
}

// wsClient is one WebSocket client; readSlow keeps the kernel receive
// buffer tiny so an unread client fills the server's side quickly.
type wsClient struct {
	c *websocket.Conn
}

func (h *streamHarness) dial(t *testing.T, path string, smallRecvBuffer bool) *wsClient {
	t.Helper()
	dialer := &net.Dialer{}
	if smallRecvBuffer {
		dialer.Control = func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) { _ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096) })
		}
	}
	client := &http.Client{Transport: &http.Transport{DialContext: dialer.DialContext}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.ws.URL, "http")+path, &websocket.DialOptions{HTTPClient: client})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.CloseNow() })
	c.SetReadLimit(1 << 20)
	return &wsClient{c: c}
}

func (w *wsClient) send(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, w.c.Write(ctx, websocket.MessageText, b))
}

func (w *wsClient) recv(t *testing.T, timeout time.Duration) (map[string]any, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, b, err := w.c.Read(ctx)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m), string(b))
	return m, b
}

// recvUntil reads messages until pred says stop, returning all of them.
func (w *wsClient) recvUntil(t *testing.T, timeout time.Duration, pred func(m map[string]any) bool) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var out []map[string]any
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		_, b, err := w.c.Read(ctx)
		cancel()
		require.NoError(t, err, "after %d messages", len(out))
		var m map[string]any
		require.NoError(t, json.Unmarshal(b, &m))
		out = append(out, m)
		if pred(m) {
			return out
		}
	}
	t.Fatalf("timed out after %d messages", len(out))
	return nil
}

// closeStatus drains until the connection ends and returns the close
// frame's code and reason, or -1 and the error when the peer went away
// without one.
func (w *wsClient) closeStatus(t *testing.T, timeout time.Duration) (websocket.StatusCode, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		_, _, err := w.c.Read(ctx)
		if err == nil {
			continue
		}
		var ce websocket.CloseError
		if errors.As(err, &ce) {
			return ce.Code, ce.Reason
		}
		require.NoError(t, ctx.Err(), "timed out waiting for the close")
		return -1, err.Error()
	}
}

func levelsOf(v any) map[string]money.Amount {
	out := map[string]money.Amount{}
	for _, l := range v.([]any) {
		pair := l.([]any)
		out[pair[0].(string)] = money.MustParse(pair[1].(string))
	}
	return out
}

func applyDelta(book map[string]money.Amount, v any) {
	for p, q := range levelsOf(v) {
		if q.IsZero() {
			delete(book, p)
		} else {
			book[p] = q
		}
	}
}

func defaultStreamConfig() stream.Config {
	return stream.Config{AuthTimeout: time.Second, ResumeWindow: 500 * time.Millisecond, WriteTimeout: 2 * time.Second}
}

// TestStreamDepthSnapshotAndDeltas: a subscriber gets a snapshot at the
// engine's seq and then one contiguous delta per command, and applying
// them reproduces GET /depth (docs/plan-v1.0.md §7.5 client rule).
func TestStreamDepthSnapshotAndDeltas(t *testing.T) {
	h := setupStream(t, defaultStreamConfig())
	market := h.cache.Markets()[0].Symbol
	seller := h.register(t, "st-seller@example.com")
	buyer := h.register(t, "st-buyer@example.com")
	h.fund(t, h.ctx, seller.AccountID, "ETH", "10", "faucet:st:eth")
	h.fund(t, h.ctx, buyer.AccountID, "USDC", "100000", "faucet:st:usdc")
	s1 := h.place(t, bearer(seller), limitOrder("s1", "sell", "2000", "1"), http.StatusCreated)
	h.waitBookSeq(t, market, 1) // the snapshot below is compared with the engine's seq

	ws := h.dial(t, "/ws/v1/public", false)
	ws.send(t, map[string]any{"op": "subscribe", "channel": "depth", "market": market})
	snap, _ := ws.recv(t, 5*time.Second)
	require.Equal(t, "snapshot", snap["type"])
	r := h.do(t, http.MethodGet, "/v1/markets/"+market+"/depth", cred{}, nil)
	require.Equal(t, http.StatusOK, r.status)
	rest := decode[gen.Depth](t, r)
	assert.EqualValues(t, rest.LastSeq, snap["seq"], "snapshot at the engine's seq")
	bids, asks := levelsOf(snap["bids"]), levelsOf(snap["asks"])
	assert.Len(t, asks, 1)
	ack, _ := ws.recv(t, time.Second)
	assert.Equal(t, "subscribed", ack["type"])

	// a mix: rest, cross with a partial fill, rest, user cancel, a market
	// order into an emptied side (post-book rejection), and an IOC
	h.place(t, bearer(buyer), limitOrder("b1", "buy", "1990", "1"), http.StatusCreated)
	h.place(t, bearer(buyer), limitOrder("b2", "buy", "2000", "1.5"), http.StatusCreated) // takes s1, rests 0.5
	h.place(t, bearer(seller), limitOrder("s2", "sell", "2010", "2"), http.StatusCreated)
	rc := h.do(t, http.MethodDelete, "/v1/orders/"+s1.Order.ID, bearer(seller), nil)
	require.Equal(t, http.StatusOK, rc.status, "cancelling a filled order is idempotent: no seq")
	b1 := h.do(t, http.MethodGet, "/v1/orders?open_only=true", bearer(buyer), nil)
	require.Equal(t, http.StatusOK, b1.status)
	var open gen.OrderList
	require.NoError(t, json.Unmarshal(b1.body, &open))
	for _, o := range open.Orders {
		if o.ClientOrderID == "b1" {
			rc = h.do(t, http.MethodDelete, "/v1/orders/"+o.ID, bearer(buyer), nil)
			require.Equal(t, http.StatusOK, rc.status)
		}
	}
	h.place(t, bearer(buyer), map[string]any{"market": market, "client_order_id": "m1", "side": "buy", "type": "market", "quote_qty": "100"}, http.StatusCreated)                                    // takes 100 USDC of s2
	h.place(t, bearer(seller), map[string]any{"market": market, "client_order_id": "i1", "side": "sell", "type": "limit", "time_in_force": "ioc", "price": "1000", "qty": "10"}, http.StatusCreated) // sweeps b2's rest, remainder cancelled

	r = h.do(t, http.MethodGet, "/v1/markets/"+market+"/depth", cred{}, nil)
	final := decode[gen.Depth](t, r)
	last := uint64(rest.LastSeq)
	msgs := ws.recvUntil(t, 10*time.Second, func(m map[string]any) bool {
		return m["type"] == "delta" && uint64(m["seq"].(float64)) == uint64(final.LastSeq)
	})
	for _, m := range msgs {
		require.Equal(t, "delta", m["type"], "%v", m)
		seq := uint64(m["seq"].(float64))
		require.Equal(t, last+1, seq, "contiguous seq")
		last = seq
		applyDelta(bids, m["bids"])
		applyDelta(asks, m["asks"])
	}
	want := map[string]money.Amount{}
	for _, l := range final.Bids {
		want[l.Price.String()] = l.Qty
	}
	require.Len(t, bids, len(want))
	for p, q := range want {
		assert.True(t, q.Equal(bids[p]), "bid %s: want %s got %s", p, q, bids[p])
	}
	want = map[string]money.Amount{}
	for _, l := range final.Asks {
		want[l.Price.String()] = l.Qty
	}
	require.Len(t, asks, len(want))
	for p, q := range want {
		assert.True(t, q.Equal(asks[p]), "ask %s: want %s got %s", p, q, asks[p])
	}
	assert.Equal(t, 0.0, testutil.ToFloat64(rebuildsOf(t, h.reg, "gap")), "no gap in normal operation")
}

// waitBookSeq blocks until the feed's book for market has applied seq: the
// engine replies at COMMIT, the feed learns of the command through the
// relay and JetStream, and a test that subscribes in between sees a
// snapshot from before the command.
func (h *streamHarness) waitBookSeq(t *testing.T, market string, seq uint64) {
	t.Helper()
	require.Eventually(t, func() bool {
		d, ok := h.feed.Depth(market, 1)
		return ok && d.Seq >= seq
	}, 10*time.Second, 20*time.Millisecond, "the feed has not applied seq %d", seq)
}

func rebuildsOf(t *testing.T, reg *prometheus.Registry, reason string) prometheus.Counter {
	t.Helper()
	// a fresh counter would be registered in place of the real one; read
	// through Gather instead
	return &gatheredCounter{reg: reg, name: "marketdata_book_rebuilds_total", labels: map[string]string{"reason": reason}}
}

// gatheredCounter reads one series out of a registry for testutil.ToFloat64.
type gatheredCounter struct {
	prometheus.Counter
	reg    *prometheus.Registry
	name   string
	labels map[string]string
}

func (g *gatheredCounter) Collect(ch chan<- prometheus.Metric) {
	mfs, err := g.reg.Gather()
	if err != nil {
		return
	}
	for _, mf := range mfs {
		if mf.GetName() != g.name {
			continue
		}
		for _, m := range mf.GetMetric() {
			match := true
			for _, lp := range m.GetLabel() {
				if want, ok := g.labels[lp.GetName()]; ok && want != lp.GetValue() {
					match = false
				}
			}
			if match {
				ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(g.name, "", nil, nil), prometheus.CounterValue, m.GetCounter().GetValue())
				return
			}
		}
	}
	ch <- prometheus.MustNewConstMetric(prometheus.NewDesc(g.name, "", nil, nil), prometheus.CounterValue, 0)
}

func (g *gatheredCounter) Describe(chan<- *prometheus.Desc) {}

// TestStreamGapTriggersResnapshot: an event the book cannot continue from
// makes the feed re-read trading.orders and re-snapshot every subscriber;
// the next real command continues contiguously.
func TestStreamGapTriggersResnapshot(t *testing.T) {
	h := setupStream(t, defaultStreamConfig())
	market := h.cache.Markets()[0].Symbol
	seller := h.register(t, "gap-seller@example.com")
	h.fund(t, h.ctx, seller.AccountID, "ETH", "10", "faucet:gap:eth")
	h.place(t, bearer(seller), limitOrder("s1", "sell", "2000", "1"), http.StatusCreated)
	// The engine has committed s1 when place returns; the feed sees it a
	// relay hop and a JetStream delivery later. Subscribing before that
	// gets a seq-0 snapshot followed by s1's delta, and the assertions
	// below are about the message after the forged event.
	h.waitBookSeq(t, market, 1)

	ws := h.dial(t, "/ws/v1/public", false)
	ws.send(t, map[string]any{"op": "subscribe", "channel": "depth", "market": market})
	snap, _ := ws.recv(t, 5*time.Second)
	require.Equal(t, "snapshot", snap["type"])
	ws.recv(t, time.Second) // subscribed
	seq := uint64(snap["seq"].(float64))

	// a forged event five commands ahead, straight into the stream
	m := market
	fake := eventbus.Envelope{
		EventID: eventbus.NewID(time.Now()), EventType: "order.accepted", SchemaVersion: 1, TenantID: "default", MarketID: &m,
		Seq: eventbus.U64(seq + 5), OccurredAt: time.Now().UTC(),
		Payload: json.RawMessage(fmt.Sprintf(`{"order_id":"ghost","client_order_id":"ghost","account_id":"x","market":%q,"side":"buy","type":"limit","time_in_force":"gtc","price":"1","qty":"1","quote_qty":null,"seq":%d}`, m, seq+5)),
	}
	body, err := fake.Marshal()
	require.NoError(t, err)
	_, err = h.js.Publish(h.ctx, fake.Subject(), body)
	require.NoError(t, err)

	resnap, _ := ws.recv(t, 10*time.Second)
	assert.Equal(t, "snapshot", resnap["type"], "%v", resnap)
	assert.EqualValues(t, seq, resnap["seq"], "the database is still at the real seq")
	require.Eventually(t, func() bool { return h.feed.Ready() == nil }, 10*time.Second, 20*time.Millisecond)
	assert.Equal(t, 1.0, testutil.ToFloat64(rebuildsOf(t, h.reg, "gap")))

	h.place(t, bearer(seller), limitOrder("s2", "sell", "2010", "1"), http.StatusCreated)
	delta, _ := ws.recv(t, 10*time.Second)
	assert.Equal(t, "delta", delta["type"])
	assert.EqualValues(t, seq+1, delta["seq"], "and the real stream continues from the snapshot")
}

// TestStreamSlowClientIsDisconnectedOthersUnaffected (docs/plan-v1.0.md §7.5,
// §12 DoD): a client that stops reading is closed with 1008 slow_consumer
// while a client that reads sees every command with no extra delay.
func TestStreamSlowClientIsDisconnectedOthersUnaffected(t *testing.T) {
	cfg := defaultStreamConfig()
	cfg.WriteBuffer = 32 // a small queue so the stuck client is found within a few hundred deltas
	h := setupStream(t, cfg)
	market := h.cache.Markets()[0].Symbol
	seller := h.register(t, "slow-seller@example.com")
	h.fund(t, h.ctx, seller.AccountID, "ETH", "1000", "faucet:slow:eth")

	slow := h.dial(t, "/ws/v1/public", true)
	fast := h.dial(t, "/ws/v1/public", false)
	for _, c := range []*wsClient{slow, fast} {
		c.send(t, map[string]any{"op": "subscribe", "channel": "depth", "market": market})
	}
	snap, _ := fast.recv(t, 5*time.Second)
	require.Equal(t, "snapshot", snap["type"])
	fast.recv(t, time.Second)
	first := uint64(snap["seq"].(float64))

	// 300 place + cancel pairs, as fast as the engine takes them
	const rounds = 300
	go func() {
		for i := 0; i < rounds; i++ {
			res := h.place(t, bearer(seller), limitOrder(fmt.Sprintf("p%d", i), "sell", fmt.Sprintf("%d", 3000+i%50), "0.01"), http.StatusCreated)
			h.do(t, http.MethodDelete, "/v1/orders/"+res.Order.ID, bearer(seller), nil)
		}
	}()

	var delays []time.Duration
	last := first
	fast.recvUntil(t, 60*time.Second, func(m map[string]any) bool {
		require.Equal(t, "delta", m["type"])
		seq := uint64(m["seq"].(float64))
		require.Equal(t, last+1, seq, "the reading client misses nothing")
		last = seq
		at, err := time.Parse(time.RFC3339Nano, m["at"].(string))
		require.NoError(t, err)
		delays = append(delays, time.Since(at))
		return seq == first+2*rounds
	})
	sort.Slice(delays, func(i, j int) bool { return delays[i] < delays[j] })
	p99 := delays[len(delays)*99/100]
	t.Logf("fast client: %d deltas, p50 %s p99 %s", len(delays), delays[len(delays)/2], p99)
	assert.Less(t, p99, time.Second, "the slow client did not hold the fast one back")

	// The server decided to close it with 1008 slow_consumer. Whether that
	// frame reaches a client that stopped reading depends on whether the
	// kernel buffers drain before the write timeout tears the socket down;
	// the server's own count is the reliable witness.
	code, reason := slow.closeStatus(t, 30*time.Second)
	t.Logf("slow client saw close %d %q", code, reason)
	if code != -1 {
		assert.Equal(t, websocket.StatusPolicyViolation, code)
		assert.Equal(t, "slow_consumer", reason)
	}
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(&gatheredCounter{reg: h.reg, name: "ws_slow_client_disconnects_total"}) == 1
	}, 5*time.Second, 50*time.Millisecond)
	assert.Equal(t, 1, h.server.Connections(), "only the reading client is left")
}

// TestStreamTradesTickerKline: a trade reaches the trades channel at once
// and the ticker and candle channels within their coalescing intervals;
// after a stream restart the ring is seeded from Postgres, so the ticker
// still knows the day's trades.
func TestStreamTradesTickerKline(t *testing.T) {
	h := setupStream(t, defaultStreamConfig())
	market := h.cache.Markets()[0].Symbol
	seller := h.register(t, "tk-seller@example.com")
	buyer := h.register(t, "tk-buyer@example.com")
	h.fund(t, h.ctx, seller.AccountID, "ETH", "10", "faucet:tk:eth")
	h.fund(t, h.ctx, buyer.AccountID, "USDC", "100000", "faucet:tk:usdc")

	ws := h.dial(t, "/ws/v1/public", false)
	for _, ch := range []string{"trades", "ticker", "kline.1m", "kline.1d"} {
		ws.send(t, map[string]any{"op": "subscribe", "channel": ch, "market": market})
		ack, _ := ws.recv(t, time.Second)
		require.Equal(t, "subscribed", ack["type"], "%s: %v", ch, ack)
	}
	h.place(t, bearer(seller), limitOrder("s1", "sell", "2000", "1"), http.StatusCreated)
	h.place(t, bearer(buyer), limitOrder("b1", "buy", "2000", "0.4"), http.StatusCreated)

	seen := map[string]map[string]any{}
	ws.recvUntil(t, 10*time.Second, func(m map[string]any) bool {
		seen[m["channel"].(string)] = m
		return len(seen) == 4
	})
	tr := seen["trades"]["trade"].(map[string]any)
	assert.Equal(t, "2000", tr["price"])
	assert.Equal(t, "0.4", tr["qty"])
	assert.Equal(t, "buy", tr["taker_side"])
	tk := seen["ticker"]["ticker"].(map[string]any)
	assert.Equal(t, "2000", tk["last_price"])
	assert.Equal(t, "0.4", tk["volume"])
	c1 := seen["kline.1m"]["candle"].(map[string]any)
	assert.Equal(t, "2000", c1["open"])
	assert.EqualValues(t, 1, c1["trades"])
	assert.Equal(t, "1d", seen["kline.1d"]["candle"].(map[string]any)["interval"])

	// restart: the new feed seeds its ring from trading.trades (the worker
	// has folded nothing yet) and the ticker still carries the trade
	h.stopStream(t)
	h.startStream(t)
	ws = h.dial(t, "/ws/v1/public", false)
	ws.send(t, map[string]any{"op": "subscribe", "channel": "ticker", "market": market})
	first, _ := ws.recv(t, 5*time.Second)
	require.Equal(t, "ticker", first["channel"], "a subscriber gets the current ticker first: %v", first)
	assert.Equal(t, "0.4", first["ticker"].(map[string]any)["volume"])
	h.place(t, bearer(buyer), limitOrder("b2", "buy", "2000", "0.1"), http.StatusCreated)
	ws.recvUntil(t, 10*time.Second, func(m map[string]any) bool {
		return m["type"] == "update" && m["channel"] == "ticker" && m["ticker"].(map[string]any)["volume"] == "0.5"
	})
}

// --- private channels (docs/plan-v1.0.md §7.5, §12 DoD "重連補齊不漏不重") ---

type privateFrame struct {
	channel    string
	typ        string
	accountSeq *int64
	role       string
	raw        map[string]any
}

func frameOf(m map[string]any) privateFrame {
	f := privateFrame{raw: m}
	f.channel, _ = m["channel"].(string)
	f.typ, _ = m["type"].(string)
	f.role, _ = m["role"].(string)
	if s, ok := m["account_seq"].(float64); ok {
		v := int64(s)
		f.accountSeq = &v
	}
	return f
}

func (h *streamHarness) authed(t *testing.T, s gen.Session) (*wsClient, int64) {
	t.Helper()
	ws := h.dial(t, "/ws/v1/private", false)
	ws.send(t, map[string]any{"op": "auth", "token": s.AccessToken})
	ack, _ := ws.recv(t, 5*time.Second)
	require.Equal(t, "auth", ack["type"], "%v", ack)
	require.Equal(t, s.AccountID, ack["account_id"])
	return ws, int64(ack["account_seq"].(float64))
}

func TestStreamPrivateAuthAndChannels(t *testing.T) {
	h := setupStream(t, defaultStreamConfig())
	seller := h.register(t, "pv-seller@example.com")
	buyer := h.register(t, "pv-buyer@example.com")
	h.fund(t, h.ctx, seller.AccountID, "ETH", "10", "faucet:pv:eth")
	h.fund(t, h.ctx, buyer.AccountID, "USDC", "100000", "faucet:pv:usdc")

	t.Run("no auth", func(t *testing.T) {
		ws := h.dial(t, "/ws/v1/private", false)
		code, reason := ws.closeStatus(t, 5*time.Second)
		assert.Equal(t, websocket.StatusPolicyViolation, code)
		assert.Equal(t, "auth_required", reason)
	})
	t.Run("bad token", func(t *testing.T) {
		ws := h.dial(t, "/ws/v1/private", false)
		ws.send(t, map[string]any{"op": "auth", "token": "garbage"})
		m, _ := ws.recv(t, 5*time.Second)
		assert.Equal(t, "auth_failed", m["code"])
		_, reason := ws.closeStatus(t, 5*time.Second)
		assert.Equal(t, "auth_failed", reason)
	})
	t.Run("orders, balances, fills", func(t *testing.T) {
		sws, sseq := h.authed(t, seller)
		bws, _ := h.authed(t, buyer)
		// (funding through the ledger produces no account events, so both
		// sequences start at zero here)
		// past the resume window: live
		time.Sleep(h.cfg.ResumeWindow + 100*time.Millisecond)

		h.place(t, bearer(seller), limitOrder("s1", "sell", "2000", "1"), http.StatusCreated)
		got := sws.recvUntil(t, 10*time.Second, func(m map[string]any) bool { return m["channel"] == "balances" })
		require.Len(t, got, 2)
		o := frameOf(got[0])
		assert.Equal(t, "orders", o.channel)
		assert.Equal(t, "order.accepted", o.typ)
		require.NotNil(t, o.accountSeq)
		assert.Equal(t, sseq+1, *o.accountSeq, "account_seq continues from the acknowledgement")
		b := frameOf(got[1])
		assert.Equal(t, "balance.updated", b.typ)
		assert.Equal(t, sseq+2, *b.accountSeq)
		assert.Contains(t, got[0]["data"].(map[string]any), "client_order_id")

		h.place(t, bearer(buyer), limitOrder("b1", "buy", "2000", "0.4"), http.StatusCreated)
		var sellerFrames, buyerFrames []privateFrame
		for _, m := range sws.recvUntil(t, 10*time.Second, func(m map[string]any) bool { return m["channel"] == "balances" }) {
			sellerFrames = append(sellerFrames, frameOf(m))
		}
		balances := 0
		for _, m := range bws.recvUntil(t, 10*time.Second, func(m map[string]any) bool {
			if frameOf(m).typ == "balance.updated" {
				balances++
			}
			return balances == 2 // USDC and ETH
		}) {
			buyerFrames = append(buyerFrames, frameOf(m))
		}
		types := func(fs []privateFrame) []string {
			var out []string
			for _, f := range fs {
				out = append(out, f.channel+":"+f.typ+":"+f.role)
			}
			return out
		}
		assert.Contains(t, types(sellerFrames), "fills:trade.executed:maker")
		assert.Contains(t, types(sellerFrames), "orders:order.updated:")
		assert.Contains(t, types(buyerFrames), "orders:order.accepted:")
		assert.Contains(t, types(buyerFrames), "fills:trade.executed:taker")
		assert.Contains(t, types(buyerFrames), "orders:order.filled:")
		for _, fs := range [][]privateFrame{sellerFrames, buyerFrames} {
			var last int64
			for _, f := range fs {
				if f.channel == "fills" {
					assert.Nil(t, f.accountSeq, "fills carry no account_seq")
					continue
				}
				require.NotNil(t, f.accountSeq, "%v", f.raw)
				assert.Greater(t, *f.accountSeq, last, "strictly increasing")
				last = *f.accountSeq
			}
		}
	})
}

// TestStreamResumeNoLossNoDup: after a disconnect, resume from the last
// account_seq seen replays exactly the missed events, merges with what
// arrives during the replay, and continues live -- every account_seq once,
// in order.
func TestStreamResumeNoLossNoDup(t *testing.T) {
	h := setupStream(t, defaultStreamConfig())
	seller := h.register(t, "rs-seller@example.com")
	buyer := h.register(t, "rs-buyer@example.com")
	h.fund(t, h.ctx, seller.AccountID, "ETH", "100", "faucet:rs:eth")
	h.fund(t, h.ctx, buyer.AccountID, "USDC", "1000000", "faucet:rs:usdc")

	ws, seq := h.authed(t, buyer)
	time.Sleep(h.cfg.ResumeWindow + 100*time.Millisecond)
	h.place(t, bearer(seller), limitOrder("s0", "sell", "2000", "50"), http.StatusCreated)
	h.place(t, bearer(buyer), limitOrder("b0", "buy", "2000", "0.5"), http.StatusCreated)
	lastSeen := seq
	ws.recvUntil(t, 10*time.Second, func(m map[string]any) bool {
		f := frameOf(m)
		if f.accountSeq != nil {
			require.Equal(t, lastSeen+1, *f.accountSeq)
			lastSeen = *f.accountSeq
		}
		return f.typ == "balance.updated"
	})
	require.NoError(t, ws.c.Close(websocket.StatusNormalClosure, "bye"))

	// offline: thirty orders, some filling
	for i := 0; i < 30; i++ {
		side, price := "buy", "1990"
		if i%3 == 0 {
			price = "2000" // fills against s0
		}
		h.place(t, bearer(buyer), limitOrder(fmt.Sprintf("off%d", i), side, price, "0.1"), http.StatusCreated)
	}
	var offlineRows int
	require.NoError(t, h.all.QueryRow(h.ctx, `SELECT count(*) FROM eventbus.outbox WHERE account_id = $1 AND account_seq > $2`, buyer.AccountID, lastSeen).Scan(&offlineRows))
	require.Positive(t, offlineRows)

	// reconnect and resume while more orders keep coming
	ws2, ackSeq := h.authed(t, buyer)
	assert.Greater(t, ackSeq, lastSeen)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			// crossing s0, so fills happen while the replay runs
			h.place(t, bearer(buyer), limitOrder(fmt.Sprintf("dur%d", i), "buy", "2000", "0.1"), http.StatusCreated)
		}
	}()
	ws2.send(t, map[string]any{"op": "resume", "since_seq": lastSeen})
	var (
		seqs     []int64
		replayed int
		fills    int
	)
	frames := ws2.recvUntil(t, 20*time.Second, func(m map[string]any) bool { return m["type"] == "resumed" })
	for _, m := range frames {
		f := frameOf(m)
		if f.typ == "resumed" {
			replayed = int(m["replayed"].(float64))
			assert.EqualValues(t, lastSeen, m["since_seq"])
			continue
		}
		if f.channel == "fills" {
			fills++
			continue
		}
		require.NotNil(t, f.accountSeq, "%v", m)
		seqs = append(seqs, *f.accountSeq)
	}
	assert.Equal(t, offlineRows, replayed-fillsIn(frames[:len(frames)-1]), "every offline event replayed once")
	wg.Wait()
	// everything is committed now: read until the ledger's sequence shows up
	var finalSeq int64
	require.NoError(t, h.all.QueryRow(h.ctx, `SELECT next_seq FROM ledger.accounts WHERE id = $1`, buyer.AccountID).Scan(&finalSeq))
	require.NotEmpty(t, seqs)
	if seqs[len(seqs)-1] < finalSeq {
		ws2.recvUntil(t, 20*time.Second, func(m map[string]any) bool {
			f := frameOf(m)
			if f.channel == "fills" {
				fills++
			}
			if f.accountSeq == nil {
				return false
			}
			seqs = append(seqs, *f.accountSeq)
			return *f.accountSeq == finalSeq
		})
	}
	assert.Equal(t, lastSeen+1, seqs[0], "the first frame after resume is the first one missed")
	for i := 1; i < len(seqs); i++ {
		assert.Equal(t, seqs[i-1]+1, seqs[i], "no gap, no duplicate at %d", i)
	}
	assert.Equal(t, finalSeq, seqs[len(seqs)-1], "and it caught up with the ledger")
	assert.Positive(t, fills, "fills are delivered live during the replay")

	ws2.send(t, map[string]any{"op": "resume", "since_seq": lastSeen})
	m, _ := ws2.recv(t, 5*time.Second)
	assert.Equal(t, "resume_too_late", m["code"])
}

func fillsIn(frames []map[string]any) int {
	n := 0
	for _, m := range frames {
		if m["channel"] == "fills" {
			n++
		}
	}
	return n
}

// TestStreamDepositsWithdrawalsChannels: account-scoped chain events reach
// the private stream on their own channels and are resumable like orders.
func TestStreamDepositsWithdrawalsChannels(t *testing.T) {
	h := setupStream(t, defaultStreamConfig())
	user := h.register(t, "chain-user@example.com")
	ws, seq := h.authed(t, user)
	time.Sleep(h.cfg.ResumeWindow + 100*time.Millisecond)

	// what the chain role appends when it credits a deposit: an
	// account-scoped envelope with the next account_seq
	tx, err := h.all.Begin(h.ctx)
	require.NoError(t, err)
	next, err := h.svc2().NextAccountSeq(h.ctx, tx, user.AccountID)
	require.NoError(t, err)
	acct := user.AccountID
	env := eventbus.Envelope{
		EventID: eventbus.NewID(time.Now()), EventType: "deposit.credited", SchemaVersion: 1, TenantID: "default",
		AccountID: &acct, AccountSeq: eventbus.I64(next), OccurredAt: time.Now().UTC(),
		Payload: json.RawMessage(`{"deposit_id":"d1","asset":"ETH","amount":"1"}`),
	}
	_, err = (eventbus.Outbox{}).Append(h.ctx, tx, env)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(h.ctx))

	m, _ := ws.recv(t, 10*time.Second)
	f := frameOf(m)
	assert.Equal(t, "deposits", f.channel)
	assert.Equal(t, "deposit.credited", f.typ)
	require.NotNil(t, f.accountSeq)
	assert.Equal(t, seq+1, *f.accountSeq)

	ws2, _ := h.authed(t, user)
	ws2.send(t, map[string]any{"op": "resume", "since_seq": seq})
	r, _ := ws2.recv(t, 10*time.Second)
	assert.Equal(t, "deposits", r["channel"], "resumable")
	done, _ := ws2.recv(t, 10*time.Second)
	assert.Equal(t, "resumed", done["type"])
	assert.EqualValues(t, 1, done["replayed"])
}

// TestStreamResumeAheadOfAccountFails: a client whose since_seq is beyond
// the account's sequence is a client that outlived a database restore
// (docs/runbooks/backup-restore.md). It gets resume_failed and a live
// connection, not a silent replay of nothing.
func TestStreamResumeAheadOfAccountFails(t *testing.T) {
	h := setupStream(t, defaultStreamConfig())
	user := h.register(t, "ahead@example.com")
	h.fund(t, h.ctx, user.AccountID, "USDC", "1000", "faucet:ahead")
	ws, seq := h.authed(t, user)
	ws.send(t, map[string]any{"op": "resume", "since_seq": seq + 1000})
	m, _ := ws.recv(t, 5*time.Second)
	assert.Equal(t, "error", m["type"])
	assert.Equal(t, "resume_failed", m["code"])

	// live from here: the next event arrives with the real sequence
	h.place(t, bearer(user), limitOrder("ahead-1", "buy", "1000", "0.1"), http.StatusCreated)
	frames := ws.recvUntil(t, 10*time.Second, func(m map[string]any) bool { return frameOf(m).typ == "order.accepted" })
	f := frameOf(frames[len(frames)-1])
	require.NotNil(t, f.accountSeq)
	assert.Equal(t, seq+1, *f.accountSeq)
}
