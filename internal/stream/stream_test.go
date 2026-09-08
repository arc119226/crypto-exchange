package stream

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/marketdata"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeTransport scripts a socket: frames the client sends, what the server
// wrote, and a switch that makes writes hang like a client that stopped
// reading.
type fakeTransport struct {
	in     chan []byte
	mu     sync.Mutex
	out    [][]byte
	block  bool
	closed chan struct{}
	code   int
	reason string
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{in: make(chan []byte, 16), closed: make(chan struct{})}
}

func (f *fakeTransport) Read(ctx context.Context) ([]byte, error) {
	select {
	case b, ok := <-f.in:
		if !ok {
			return nil, io.EOF
		}
		return b, nil
	case <-f.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeTransport) Write(ctx context.Context, b []byte) error {
	f.mu.Lock()
	block := f.block
	f.mu.Unlock()
	if block {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.closed:
			return io.EOF
		}
	}
	f.mu.Lock()
	f.out = append(f.out, b)
	f.mu.Unlock()
	return nil
}

func (f *fakeTransport) Ping(context.Context) error { return nil }

func (f *fakeTransport) Close(code int, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.closed:
		return nil
	default:
	}
	f.code, f.reason = code, reason
	close(f.closed)
	return nil
}

func (f *fakeTransport) written() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.out))
	for _, b := range f.out {
		out = append(out, string(b))
	}
	return out
}

func testConfig() Config {
	return Config{WriteBuffer: 4, PingInterval: time.Hour, PongTimeout: time.Second, WriteTimeout: time.Second, AuthTimeout: 200 * time.Millisecond, ResumeWindow: 200 * time.Millisecond, MaxSubscriptions: 2, AllowedOrigins: []string{"*"}}.withDefaults()
}

func TestSlowClientIsClosedNotWaitedFor(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	ft := newFakeTransport()
	ft.mu.Lock()
	ft.block = true
	ft.mu.Unlock()
	c := newConn(context.Background(), 1, "public", ft, testConfig(), m, quiet)
	done := make(chan struct{})
	go func() { c.serve(func(clientMessage) {}, func() {}); close(done) }()

	// the writer is stuck on the first frame; the buffer holds four more;
	// the sixth has nowhere to go
	start := time.Now()
	for i := 0; i < 6; i++ {
		c.send([]byte(`{"n":1}`))
	}
	code, reason := c.closeStatus()
	assert.Equal(t, closePolicy, code)
	assert.Equal(t, ReasonSlowConsumer, reason)
	assert.Less(t, time.Since(start), 500*time.Millisecond, "the decision does not wait for the stuck write")
	assert.Equal(t, 1.0, testutil.ToFloat64(m.slow))
	select {
	case <-ft.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("slow client not closed")
	}
	assert.Equal(t, closePolicy, ft.code)
	assert.Equal(t, ReasonSlowConsumer, ft.reason)
	assert.False(t, c.send([]byte(`late`)), "nothing is queued after the close")
	<-done
}

func TestBadFramesAreAnsweredThenClosed(t *testing.T) {
	ft := newFakeTransport()
	c := newConn(context.Background(), 1, "public", ft, testConfig(), NewMetrics(nil), quiet)
	done := make(chan struct{})
	go func() { c.serve(func(clientMessage) {}, func() {}); close(done) }()
	for i := 0; i < 5; i++ {
		ft.in <- []byte("not json")
	}
	<-ft.closed
	assert.Equal(t, ReasonBadMessage, ft.reason)
	out := ft.written()
	require.NotEmpty(t, out)
	assert.Contains(t, out[0], `"code":"bad_message"`)
	close(ft.in)
	<-done
}

func TestHubFanOutAndRemoval(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	h := NewHub(m)
	var conns []*conn
	var fts []*fakeTransport
	for i := 0; i < 3; i++ {
		ft := newFakeTransport()
		c := newConn(context.Background(), uint64(i+1), "public", ft, testConfig(), m, quiet)
		h.add(c)
		go c.writeLoop()
		conns, fts = append(conns, c), append(fts, ft)
	}
	require.True(t, h.subscribe(conns[0], "depth|ETH-USDC"))
	require.False(t, h.subscribe(conns[0], "depth|ETH-USDC"), "already subscribed")
	require.True(t, h.subscribe(conns[1], "depth|ETH-USDC"))
	require.True(t, h.subscribe(conns[2], "trades|ETH-USDC"))
	assert.Equal(t, 2, h.Publish("depth|ETH-USDC", "depth", []byte(`{"d":1}`)))
	assert.Equal(t, 0, h.Publish("depth|BTC-USDC", "depth", []byte(`{"d":2}`)))
	require.True(t, h.unsubscribe(conns[1], "depth|ETH-USDC"))
	assert.Equal(t, 1, h.Publish("depth|ETH-USDC", "depth", []byte(`{"d":3}`)))
	h.remove(conns[0])
	assert.Equal(t, 0, h.Publish("depth|ETH-USDC", "depth", []byte(`{"d":4}`)))
	assert.Equal(t, 2, h.Connections())
	require.Eventually(t, func() bool { return len(fts[0].written()) == 2 && len(fts[1].written()) == 1 }, time.Second, 10*time.Millisecond)
	assert.Equal(t, 3.0, testutil.ToFloat64(m.sent.WithLabelValues("depth")))
	for _, c := range conns {
		c.cancel()
	}
}

func TestParseChannel(t *testing.T) {
	for _, name := range []string{"depth", "trades", "ticker", "kline.1m", "kline.5m", "kline.15m", "kline.1h", "kline.1d"} {
		_, ok := parseChannel(name)
		assert.True(t, ok, name)
	}
	for _, name := range []string{"", "kline", "kline.2m", "orders", "depth "} {
		_, ok := parseChannel(name)
		assert.False(t, ok, name)
	}
	assert.Equal(t, ChannelOrders, privateChannel("order.accepted"))
	assert.Equal(t, ChannelBalances, privateChannel("balance.updated"))
	assert.Equal(t, ChannelDeposits, privateChannel("deposit.credited"))
	assert.Equal(t, ChannelWithdrawals, privateChannel("withdrawal.state_changed"))
	assert.Equal(t, "", privateChannel("market.updated"))
}

// --- the server over a real socket, with a fake store ---

type fakeStore struct {
	mu    sync.Mutex
	snap  marketdata.BookSnapshot
	seed  marketdata.RingSeed
	calls int
	err   error
}

func (f *fakeStore) OpenOrders(context.Context, string, string) (marketdata.BookSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.snap, f.err
}

func (f *fakeStore) RingSeed(context.Context, string, string, time.Time) (marketdata.RingSeed, error) {
	return f.seed, nil
}

func (f *fakeStore) AccountSeq(context.Context, string) (int64, error) { return 7, nil }

type fakeMarkets struct{ markets []registry.Market }

func (f fakeMarkets) Markets() []registry.Market { return f.markets }

type fakeOutbox struct {
	pages [][]eventbus.Envelope
	calls []int64
}

func (f *fakeOutbox) ByAccountSince(_ context.Context, _, _ string, since int64, _ int32) ([]eventbus.Envelope, error) {
	f.calls = append(f.calls, since)
	if len(f.pages) == 0 {
		return nil, nil
	}
	p := f.pages[0]
	f.pages = f.pages[1:]
	return p, nil
}

type harness struct {
	srv    *httptest.Server
	server *Server
	feed   *Feed
	store  *fakeStore
	outbox *fakeOutbox
	signer *auth.Signer
	reg    *prometheus.Registry
	md     *marketdata.Metrics
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	md := marketdata.NewMetrics(reg)
	hub := NewHub(m)
	store := &fakeStore{snap: marketdata.BookSnapshot{Market: "ETH-USDC", Seq: 10, Orders: []marketdata.RestingOrder{
		{ID: "b1", Side: marketdata.Buy, Price: money.MustParse("1990"), Remaining: money.MustParse("0.5")},
		{ID: "a1", Side: marketdata.Sell, Price: money.MustParse("2010"), Remaining: money.MustParse("1")},
	}}, seed: marketdata.RingSeed{Seq: 10}}
	markets := fakeMarkets{markets: []registry.Market{{ID: "m1", Symbol: "ETH-USDC"}}}
	feed := NewFeed(cfg, store, markets, hub, nil, m, md, 100, quiet)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := auth.NewSigner(priv, "exchange")
	require.NoError(t, err)
	verifier, err := signer.VerifierFor()
	require.NoError(t, err)
	outbox := &fakeOutbox{}
	server := New(ctx, cfg, Deps{Tenant: "default", Feed: feed, Hub: hub, Verifier: verifier, Outbox: outbox, Accounts: store, Metrics: m, Log: quiet})
	srv := httptest.NewServer(server.Handler())
	t.Cleanup(srv.Close)
	go func() { _ = feed.Run(ctx) }()
	feed.Start(ctx)
	require.Eventually(t, func() bool { return feed.Ready() == nil }, 2*time.Second, 10*time.Millisecond)
	return &harness{srv: srv, server: server, feed: feed, store: store, outbox: outbox, signer: signer, reg: reg, md: md}
}

func (h *harness) dial(t *testing.T, path string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.srv.URL, "http")+path, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.CloseNow() })
	return ws
}

func send(t *testing.T, ws *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, ws.Write(ctx, websocket.MessageText, b))
}

func recv(t *testing.T, ws *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := ws.Read(ctx)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m), string(b))
	return m
}

func recvClose(t *testing.T, ws *websocket.Conn) (websocket.StatusCode, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := ws.Read(ctx)
		if err == nil {
			continue
		}
		var ce websocket.CloseError
		require.True(t, errors.As(err, &ce), "%v", err)
		return ce.Code, ce.Reason
	}
}

func envelope(t *testing.T, eventType string, seq uint64, account string, accountSeq *int64, payload map[string]any) eventbus.Envelope {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	m := "ETH-USDC"
	e := eventbus.Envelope{EventID: eventbus.NewID(time.Now()), EventType: eventType, SchemaVersion: 1, TenantID: "default", OccurredAt: time.Now().UTC(), Payload: body}
	if seq > 0 {
		e.MarketID, e.Seq = &m, eventbus.U64(seq)
	}
	if account != "" {
		e.AccountID, e.AccountSeq = &account, accountSeq
	}
	return e
}

func TestPublicSubscribeSnapshotAndDelta(t *testing.T) {
	h := newHarness(t, testConfig())
	ws := h.dial(t, "/ws/v1/public")

	send(t, ws, map[string]any{"op": "subscribe", "channel": "depth", "market": "ETH-USDC"})
	snap := recv(t, ws)
	assert.Equal(t, "snapshot", snap["type"])
	assert.Equal(t, float64(10), snap["seq"])
	assert.Equal(t, []any{[]any{"1990", "0.5"}}, snap["bids"])
	ack := recv(t, ws)
	assert.Equal(t, "subscribed", ack["type"])

	// a resting order closes at the accept: one delta, seq 11
	h.feed.Handle(context.Background(), envelope(t, marketdata.EventOrderAccepted, 11, "acct", nil, map[string]any{
		"order_id": "b2", "market": "ETH-USDC", "side": "buy", "type": "limit", "time_in_force": "gtc", "price": "1990", "qty": "0.25",
	}))
	delta := recv(t, ws)
	assert.Equal(t, "delta", delta["type"])
	assert.Equal(t, float64(11), delta["seq"])
	assert.Equal(t, []any{[]any{"1990", "0.75"}}, delta["bids"])
	assert.Equal(t, []any{}, delta["asks"])

	// duplicate subscribe repeats nothing, unsubscribe stops the flow
	send(t, ws, map[string]any{"op": "subscribe", "channel": "depth", "market": "ETH-USDC"})
	assert.Equal(t, "subscribed", recv(t, ws)["type"])
	send(t, ws, map[string]any{"op": "unsubscribe", "channel": "depth", "market": "ETH-USDC"})
	assert.Equal(t, "unsubscribed", recv(t, ws)["type"])
	h.feed.Handle(context.Background(), envelope(t, marketdata.EventOrderCancelled, 12, "acct", nil, map[string]any{
		"order_id": "b2", "market": "ETH-USDC", "reason": "user", "remaining_qty": "0.25",
	}))
	send(t, ws, map[string]any{"op": "ping"})
	assert.Equal(t, "pong", recv(t, ws)["type"], "nothing but the pong arrives")

	// errors
	send(t, ws, map[string]any{"op": "subscribe", "channel": "depth", "market": "BTC-USDC"})
	assert.Equal(t, CodeUnknownMarket, recv(t, ws)["code"])
	send(t, ws, map[string]any{"op": "subscribe", "channel": "kline.2m", "market": "ETH-USDC"})
	assert.Equal(t, CodeUnknownChannel, recv(t, ws)["code"])
	send(t, ws, map[string]any{"op": "subscribe", "channel": "trades"})
	assert.Equal(t, CodeBadMessage, recv(t, ws)["code"])
	send(t, ws, map[string]any{"op": "auth", "token": "x"})
	assert.Equal(t, CodeNotAvailable, recv(t, ws)["code"])
	send(t, ws, map[string]any{"op": "dance"})
	assert.Equal(t, CodeBadMessage, recv(t, ws)["code"])
	send(t, ws, map[string]any{"op": "subscribe", "channel": "trades", "market": "ETH-USDC"})
	assert.Equal(t, "subscribed", recv(t, ws)["type"])
	send(t, ws, map[string]any{"op": "subscribe", "channel": "ticker", "market": "ETH-USDC"})
	assert.Equal(t, "subscribed", recv(t, ws)["type"], "no ticker yet: nothing to send first")
	send(t, ws, map[string]any{"op": "subscribe", "channel": "kline.1m", "market": "ETH-USDC"})
	assert.Equal(t, CodeTooManySubscriptions, recv(t, ws)["code"])

	// trades, ticker and candles follow a trade
	h.feed.Handle(context.Background(), envelope(t, marketdata.EventOrderAccepted, 13, "acct", nil, map[string]any{
		"order_id": "t1", "market": "ETH-USDC", "side": "buy", "type": "market", "time_in_force": "ioc", "price": nil, "qty": nil,
	}))
	h.feed.Handle(context.Background(), envelope(t, marketdata.EventTradeExecuted, 13, "", nil, map[string]any{
		"trade_id": "tr1", "market": "ETH-USDC", "maker_order_id": "a1", "taker_order_id": "t1", "maker_account_id": "M", "taker_account_id": "T",
		"taker_side": "buy", "price": "2010", "qty": "0.4", "quote_qty": "804",
	}))
	tr := recv(t, ws)
	assert.Equal(t, "trades", tr["channel"])
	assert.Equal(t, "2010", tr["trade"].(map[string]any)["price"])
	tk := recv(t, ws)
	assert.Equal(t, "ticker", tk["channel"])
	assert.Equal(t, "2010", tk["ticker"].(map[string]any)["last_price"])
	assert.Equal(t, 1, h.store.calls, "one rebuild at start")
}

func TestPublicGapRebuildsFromTheStore(t *testing.T) {
	h := newHarness(t, testConfig())
	ws := h.dial(t, "/ws/v1/public")
	send(t, ws, map[string]any{"op": "subscribe", "channel": "depth", "market": "ETH-USDC"})
	assert.Equal(t, "snapshot", recv(t, ws)["type"])
	recv(t, ws) // subscribed

	// the next snapshot the store hands out is at seq 20
	h.store.mu.Lock()
	h.store.snap = marketdata.BookSnapshot{Market: "ETH-USDC", Seq: 20, Orders: []marketdata.RestingOrder{{ID: "z", Side: marketdata.Sell, Price: money.MustParse("2020"), Remaining: money.MustParse("2")}}}
	h.store.mu.Unlock()
	h.feed.Handle(context.Background(), envelope(t, marketdata.EventOrderAccepted, 15, "acct", nil, map[string]any{
		"order_id": "gap", "market": "ETH-USDC", "side": "buy", "type": "limit", "time_in_force": "gtc", "price": "1000", "qty": "1",
	}))
	snap := recv(t, ws)
	assert.Equal(t, "snapshot", snap["type"], "a gap re-snapshots every subscriber")
	assert.Equal(t, float64(20), snap["seq"])
	assert.Equal(t, []any{[]any{"2020", "2"}}, snap["asks"])
	require.Eventually(t, func() bool { return h.feed.Ready() == nil }, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, 2, h.store.calls)
}

func TestOriginRefused(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedOrigins = []string{"app.example.com"}
	h := newHarness(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/ws/v1/public"
	_, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"http://evil.example.com"}}})
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{"https://app.example.com"}}})
	require.NoError(t, err)
	_ = ws.CloseNow()
}

func (h *harness) token(t *testing.T, account, tenant string, ttl time.Duration) string {
	t.Helper()
	tok, err := h.signer.Issue(auth.Claims{UserID: "u1", AccountID: account, TenantID: tenant, Role: "user", Scopes: []auth.Scope{auth.ScopeRead}, Method: auth.MethodJWT, Audience: auth.AudiencePublic}, time.Now(), ttl)
	require.NoError(t, err)
	return tok
}

func TestPrivateAuthHoldAndResume(t *testing.T) {
	h := newHarness(t, testConfig())

	t.Run("no auth in time", func(t *testing.T) {
		ws := h.dial(t, "/ws/v1/private")
		code, reason := recvClose(t, ws)
		assert.Equal(t, websocket.StatusPolicyViolation, code)
		assert.Equal(t, ReasonAuthRequired, reason)
	})
	t.Run("bad token", func(t *testing.T) {
		ws := h.dial(t, "/ws/v1/private")
		send(t, ws, map[string]any{"op": "auth", "token": "nope"})
		assert.Equal(t, CodeAuthFailed, recv(t, ws)["code"])
		_, reason := recvClose(t, ws)
		assert.Equal(t, ReasonAuthFailed, reason)
	})
	t.Run("wrong tenant", func(t *testing.T) {
		ws := h.dial(t, "/ws/v1/private")
		send(t, ws, map[string]any{"op": "auth", "token": h.token(t, "acct", "other", time.Minute)})
		assert.Equal(t, CodeAuthFailed, recv(t, ws)["code"])
	})
	t.Run("ops before auth", func(t *testing.T) {
		ws := h.dial(t, "/ws/v1/private")
		send(t, ws, map[string]any{"op": "subscribe", "channel": "depth", "market": "ETH-USDC"})
		assert.Equal(t, CodeNotAuthenticated, recv(t, ws)["code"])
		send(t, ws, map[string]any{"op": "ping"})
		assert.Equal(t, "pong", recv(t, ws)["type"])
	})
	t.Run("resume merges the replay and what was held", func(t *testing.T) {
		ws := h.dial(t, "/ws/v1/private")
		send(t, ws, map[string]any{"op": "auth", "token": h.token(t, "acct", "default", time.Minute)})
		ack := recv(t, ws)
		assert.Equal(t, "auth", ack["type"])
		assert.Equal(t, "acct", ack["account_id"])
		assert.Equal(t, float64(7), ack["account_seq"])

		// live frames arrive while the client decides: 5 (already in the
		// replay), 6 (new) and a fill (no sequence)
		order := func(seq int64, id string) eventbus.Envelope {
			return envelope(t, marketdata.EventOrderAccepted, 0, "acct", &seq, map[string]any{"order_id": id})
		}
		h.feed.Handle(context.Background(), order(5, "o5"))
		h.feed.Handle(context.Background(), order(6, "o6"))
		h.feed.Handle(context.Background(), envelope(t, marketdata.EventTradeExecuted, 30, "", nil, map[string]any{
			"trade_id": "f1", "market": "ETH-USDC", "maker_order_id": "x", "taker_order_id": "o6", "maker_account_id": "other", "taker_account_id": "acct", "taker_side": "buy", "price": "1", "qty": "1", "quote_qty": "1",
		}))
		h.outbox.pages = [][]eventbus.Envelope{{order(4, "o4"), order(5, "o5")}}
		send(t, ws, map[string]any{"op": "resume", "since_seq": 3})
		var seqs []float64
		var types []string
		for i := 0; i < 5; i++ {
			m := recv(t, ws)
			types = append(types, m["type"].(string))
			if s, ok := m["account_seq"].(float64); ok {
				seqs = append(seqs, s)
			}
			if m["type"] == "resumed" {
				assert.Equal(t, float64(3), m["since_seq"])
				assert.Equal(t, float64(2), m["replayed"])
			}
		}
		assert.Equal(t, []float64{4, 5, 6}, seqs, "replay, then the held frame after it; 5 is not repeated")
		assert.Equal(t, []string{"order.accepted", "order.accepted", "order.accepted", "trade.executed", "resumed"}, types)
		assert.Equal(t, []int64{3}, h.outbox.calls)

		// live from now on, and a second resume is refused
		h.feed.Handle(context.Background(), order(7, "o7"))
		m := recv(t, ws)
		assert.Equal(t, float64(7), m["account_seq"])
		assert.Equal(t, "orders", m["channel"])
		send(t, ws, map[string]any{"op": "resume", "since_seq": 7})
		assert.Equal(t, CodeResumeTooLate, recv(t, ws)["code"])
		send(t, ws, map[string]any{"op": "auth", "token": "again"})
		assert.Equal(t, CodeBadMessage, recv(t, ws)["code"])

		// public channels work on the private socket too
		send(t, ws, map[string]any{"op": "subscribe", "channel": "depth", "market": "ETH-USDC"})
		assert.Equal(t, "snapshot", recv(t, ws)["type"])
	})
	t.Run("silence after auth goes live", func(t *testing.T) {
		ws := h.dial(t, "/ws/v1/private")
		send(t, ws, map[string]any{"op": "auth", "token": h.token(t, "acct2", "default", time.Minute)})
		assert.Equal(t, "auth", recv(t, ws)["type"])
		seq := int64(1)
		h.feed.Handle(context.Background(), envelope(t, "balance.updated", 0, "acct2", &seq, map[string]any{"asset": "ETH"}))
		m := recv(t, ws)
		assert.Equal(t, "balances", m["channel"], "held, then released when the window closes")
		send(t, ws, map[string]any{"op": "resume", "since_seq": 0})
		assert.Equal(t, CodeResumeTooLate, recv(t, ws)["code"])
	})
	t.Run("expired token", func(t *testing.T) {
		ws := h.dial(t, "/ws/v1/private")
		send(t, ws, map[string]any{"op": "auth", "token": h.token(t, "acct", "default", -time.Minute)})
		assert.Equal(t, CodeAuthFailed, recv(t, ws)["code"])
	})
}

func TestCloseAll(t *testing.T) {
	h := newHarness(t, testConfig())
	ws := h.dial(t, "/ws/v1/public")
	send(t, ws, map[string]any{"op": "ping"})
	recv(t, ws)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.server.CloseAll(ctx)
	code, reason := recvClose(t, ws)
	assert.Equal(t, websocket.StatusGoingAway, code)
	assert.Equal(t, "shutdown", reason)
	assert.Equal(t, 0, h.server.Connections())
}
