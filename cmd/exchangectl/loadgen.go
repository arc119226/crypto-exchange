package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/spf13/cobra"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/adminclient"
	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/apiclient"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// loadgenOptions are the knobs of one run.
type loadgenOptions struct {
	market      string
	rate        int
	duration    time.Duration
	accounts    int
	wsClients   int
	privateWS   int
	idleConns   int
	wsURL       string
	adminURL    string
	adminKey    string
	mid         string
	spread      string
	qty         string
	crossEvery  int
	cancelEvery int
	verbose     bool
}

// newLoadgenCmd is the load generator of docs/plan-v1.0.md §12 Phase 6 and
// the measurement of §3.3: orders per second sustained, POST /v1/orders
// latency percentiles, trades per second, and how long a trade takes to
// reach a public depth subscriber and a private one.
func newLoadgenCmd() *cobra.Command {
	var o loadgenOptions
	c := &cobra.Command{
		Use:   "loadgen",
		Short: "Generate order flow against a running stack and report latency, throughput and push delay",
		Long: `Registers N accounts, funds them through the admin faucet, and places limit
orders around a mid price at the requested rate from every account, cancelling
resting ones as it goes so the book stays bounded. Every --cross-every-th order
crosses the spread to produce trades. WebSocket subscribers time each depth
delta, trade and private order frame from the event's occurred_at to receipt
(the clocks must agree: run it on the same host as the stream role).

Per-account order rate is capped by RATELIMIT_ORDERS_PER_ACCOUNT (20/s by
default), so --rate / --accounts must stay under it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			base, err := cmd.Flags().GetString("base-url")
			if err != nil {
				return err
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			return runLoadgen(cmd.Context(), cmd.OutOrStdout(), base, format, o)
		},
	}
	f := c.Flags()
	f.StringVar(&o.market, "market", "ETH-USDC", "market to trade")
	f.IntVar(&o.rate, "rate", 100, "orders per second across all accounts")
	f.DurationVar(&o.duration, "duration", 30*time.Second, "how long to generate load")
	f.IntVar(&o.accounts, "accounts", 10, "accounts to register and trade from")
	f.IntVar(&o.wsClients, "ws-clients", 5, "public WebSocket subscribers (depth + trades) timing the pushes")
	f.IntVar(&o.privateWS, "private-clients", 3, "accounts that also keep a private WebSocket open, timing their order frames")
	f.IntVar(&o.idleConns, "idle-connections", 0, "extra idle public connections to hold open (the §3.3 1,000-connection target)")
	f.StringVar(&o.wsURL, "ws-url", envOr("EXCHANGE_WS_URL", "ws://localhost:8081"), "stream role base URL; env EXCHANGE_WS_URL")
	f.StringVar(&o.adminURL, "admin-url", envOr("EXCHANGE_ADMIN_URL", "http://localhost:8082"), "admin API base URL (dev faucet)")
	f.StringVar(&o.adminKey, "admin-key", envOr("EXCHANGE_ADMIN_API_KEY", ""), "admin API key (X-Admin-Api-Key); env EXCHANGE_ADMIN_API_KEY")
	f.StringVar(&o.mid, "mid", "2000", "mid price the orders are placed around")
	f.StringVar(&o.spread, "spread", "10", "how far from mid resting orders go (uniform, in price units)")
	f.StringVar(&o.qty, "qty", "0.01", "order quantity")
	f.IntVar(&o.crossEvery, "cross-every", 5, "every n-th order crosses the spread (0 = never)")
	f.IntVar(&o.cancelEvery, "cancel-every", 3, "every n-th order first cancels the account's previous resting order (0 = never)")
	f.BoolVar(&o.verbose, "verbose", false, "print progress")
	return c
}

// loadReport is what a run measured. It is the JSON shape as well.
type loadReport struct {
	Market    string        `json:"market"`
	Duration  time.Duration `json:"duration"`
	Accounts  int           `json:"accounts"`
	RateAsked int           `json:"rate_asked"`

	OrdersSent     int     `json:"orders_sent"`
	OrdersOK       int     `json:"orders_ok"`
	OrdersRejected int     `json:"orders_rejected"`
	Throttled      int     `json:"throttled_429"`
	Unavailable    int     `json:"unavailable_503"`
	Errors         int     `json:"errors"`
	Cancels        int     `json:"cancels"`
	OrdersPerSec   float64 `json:"orders_per_sec"` //nolint:forbidigo // a rate, not money
	OrderLatency   latency `json:"order_latency"`
	CancelLatency  latency `json:"cancel_latency"`

	TradesSeen   int     `json:"trades_seen"`
	TradesPerSec float64 `json:"trades_per_sec"` //nolint:forbidigo // a rate, not money
	DepthDeltas  int     `json:"depth_deltas_seen"`
	DepthDelay   latency `json:"depth_delta_delay"`
	TradeDelay   latency `json:"trade_delay"`
	PrivateDelay latency `json:"private_push_delay"`
	PrivateSeen  int     `json:"private_frames_seen"`

	WSClients      int `json:"ws_clients"`
	PrivateClients int `json:"private_clients"`
	IdleOpened     int `json:"idle_connections_opened"`
	WSClosed       int `json:"ws_connections_closed_early"`
	SeqGaps        int `json:"depth_seq_gaps"`
}

// latency is a percentile summary in milliseconds.
type latency struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50_ms"` //nolint:forbidigo // milliseconds, not money
	P95   float64 `json:"p95_ms"` //nolint:forbidigo // milliseconds, not money
	P99   float64 `json:"p99_ms"` //nolint:forbidigo // milliseconds, not money
	Max   float64 `json:"max_ms"` //nolint:forbidigo // milliseconds, not money
}

// samples collects durations from many goroutines.
type samples struct {
	mu sync.Mutex
	v  []time.Duration
}

func (s *samples) add(d time.Duration) {
	s.mu.Lock()
	s.v = append(s.v, d)
	s.mu.Unlock()
}

func (s *samples) summary() latency {
	s.mu.Lock()
	v := append([]time.Duration(nil), s.v...)
	s.mu.Unlock()
	return summarize(v)
}

// summarize sorts and picks percentiles by nearest-rank.
func summarize(v []time.Duration) latency {
	if len(v) == 0 {
		return latency{}
	}
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	pick := func(p int) float64 { //nolint:forbidigo // milliseconds, not money
		i := (len(v)*p + 99) / 100
		if i < 1 {
			i = 1
		}
		if i > len(v) {
			i = len(v)
		}
		return float64(v[i-1]) / float64(time.Millisecond) //nolint:forbidigo // milliseconds, not money
	}
	return latency{Count: len(v), P50: pick(50), P95: pick(95), P99: pick(99), Max: float64(v[len(v)-1]) / float64(time.Millisecond)} //nolint:forbidigo // milliseconds, not money
}

type loadAccount struct {
	email   string
	session apiclient.Session
	client  *apiclient.ClientWithResponses
	resting []string
}

func runLoadgen(ctx context.Context, out io.Writer, base, format string, o loadgenOptions) error {
	if o.accounts <= 0 || o.rate <= 0 || o.duration <= 0 {
		return errors.New("--accounts, --rate and --duration must be positive")
	}
	if o.adminKey == "" {
		return fmt.Errorf("admin API key required for the faucet: --admin-key or EXCHANGE_ADMIN_API_KEY")
	}
	mid, err := money.ParseAmount(o.mid)
	if err != nil {
		return fmt.Errorf("--mid: %w", err)
	}
	spread, err := money.ParseAmount(o.spread)
	if err != nil {
		return fmt.Errorf("--spread: %w", err)
	}
	qty, err := money.ParseAmount(o.qty)
	if err != nil {
		return fmt.Errorf("--qty: %w", err)
	}
	perAccount := float64(o.rate) / float64(o.accounts) //nolint:forbidigo // a rate, not money
	if perAccount > 20 && format != "json" {
		_, _ = fmt.Fprintf(out, "warning: %d orders/s over %d accounts is %.0f/s per account; RATELIMIT_ORDERS_PER_ACCOUNT defaults to 20/1s, expect 429s\n", o.rate, o.accounts, perAccount)
	}
	step := func(format string, args ...any) {
		if o.verbose {
			_, _ = fmt.Fprintf(out, "• "+format+"\n", args...)
		}
	}

	admin, err := adminclient.NewClientWithResponses(o.adminURL,
		adminclient.WithHTTPClient(&http.Client{Timeout: requestTimeout}),
		adminclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("X-Admin-Api-Key", o.adminKey)
			req.Header.Set(telemetry.RequestIDHeader, newRequestID())
			return nil
		}))
	if err != nil {
		return err
	}

	// 1. accounts, funded on both sides so every account can buy and sell
	stamp := time.Now().UTC().Format("20060102t150405")
	accounts := make([]*loadAccount, 0, o.accounts)
	httpClient := &http.Client{Timeout: requestTimeout, Transport: &http.Transport{MaxIdleConnsPerHost: o.accounts + 8}}
	for i := 0; i < o.accounts; i++ {
		a := &loadAccount{email: fmt.Sprintf("loadgen-%s-%d@loadgen.local", stamp, i)}
		anon, err := apiclient.NewClientWithResponses(base, apiclient.WithHTTPClient(httpClient))
		if err != nil {
			return err
		}
		resp, err := anon.RegisterWithResponse(ctx, apiclient.RegisterJSONRequestBody{Email: openapi_types.Email(a.email), Password: "loadgen-password-" + stamp})
		if err != nil {
			return transportError(base, err)
		}
		if resp.JSON201 == nil {
			return fmt.Errorf("register %s: %w", a.email, problemError(resp.HTTPResponse, resp.Body))
		}
		a.session = *resp.JSON201
		creds := credentials{token: a.session.AccessToken}
		if a.client, err = apiclient.NewClientWithResponses(base, apiclient.WithHTTPClient(httpClient), apiclient.WithRequestEditorFn(creds.editor())); err != nil {
			return err
		}
		for asset, amount := range map[string]string{"USDC": "1000000", "ETH": "1000"} {
			amt := money.MustParse(amount)
			fr, err := admin.CreateAdjustmentWithResponse(ctx, adminclient.CreateAdjustmentJSONRequestBody{
				AccountID: a.session.AccountID, Asset: asset, Amount: amt, Direction: adminclient.AdjustmentRequestDirectionCredit,
				Reason: "loadgen faucet", IdempotencyKey: fmt.Sprintf("loadgen:%s:%s:%s", stamp, a.session.AccountID, asset),
			})
			if err != nil {
				return transportError(o.adminURL, err)
			}
			if fr.JSON201 == nil && fr.JSON200 == nil {
				return fmt.Errorf("fund %s: %w", a.email, problemError(fr.HTTPResponse, fr.Body))
			}
		}
		accounts = append(accounts, a)
	}
	step("registered and funded %d accounts", len(accounts))

	// 2. observers
	rep := loadReport{Market: o.market, Duration: o.duration, Accounts: o.accounts, RateAsked: o.rate, WSClients: o.wsClients, PrivateClients: min(o.privateWS, o.accounts)}
	obs := &observers{}
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	var wg sync.WaitGroup
	for i := 0; i < o.wsClients; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); obs.public(wctx, o.wsURL, o.market) }()
	}
	for i := 0; i < rep.PrivateClients; i++ {
		wg.Add(1)
		tok := accounts[i].session.AccessToken
		go func() { defer wg.Done(); obs.private(wctx, o.wsURL, tok) }()
	}
	if o.idleConns > 0 {
		wg.Add(1)
		go func() { defer wg.Done(); obs.idle(wctx, o.wsURL, o.idleConns) }()
	}
	time.Sleep(500 * time.Millisecond) // let the subscriptions settle before the flood
	step("observers: %d public, %d private, %d idle", o.wsClients, rep.PrivateClients, o.idleConns)

	// 3. the flood: every account on its own clock, rate/accounts each
	gen := &generator{o: o, mid: mid, spread: spread, qty: qty, rep: &rep}
	lctx, lcancel := context.WithTimeout(ctx, o.duration)
	defer lcancel()
	start := time.Now()
	var gw sync.WaitGroup
	for i, a := range accounts {
		gw.Add(1)
		go func(i int, a *loadAccount) {
			defer gw.Done()
			gen.run(lctx, a, i)
		}(i, a)
	}
	gw.Wait()
	elapsed := time.Since(start)
	time.Sleep(time.Second) // drain the last pushes
	wcancel()
	wg.Wait()

	// 4. the report
	rep.OrdersSent = int(gen.sent.Load())
	rep.OrdersOK = int(gen.ok.Load())
	rep.OrdersRejected = int(gen.rejected.Load())
	rep.Throttled = int(gen.throttled.Load())
	rep.Unavailable = int(gen.unavailable.Load())
	rep.Errors = int(gen.failed.Load())
	rep.Cancels = int(gen.cancels.Load())
	rep.OrdersPerSec = float64(rep.OrdersSent) / elapsed.Seconds() //nolint:forbidigo // a rate, not money
	rep.OrderLatency = gen.orderLatency.summary()
	rep.CancelLatency = gen.cancelLatency.summary()
	rep.TradesSeen = int(obs.trades.Load())
	if rep.WSClients > 0 {
		rep.TradesSeen /= rep.WSClients // every subscriber counts every trade
	}
	rep.TradesPerSec = float64(rep.TradesSeen) / elapsed.Seconds() //nolint:forbidigo // a rate, not money
	rep.DepthDeltas = int(obs.deltas.Load())
	rep.DepthDelay = obs.depthDelay.summary()
	rep.TradeDelay = obs.tradeDelay.summary()
	rep.PrivateDelay = obs.privateDelay.summary()
	rep.PrivateSeen = int(obs.privateFrames.Load())
	rep.IdleOpened = int(obs.idleOpened.Load())
	rep.WSClosed = int(obs.closedEarly.Load())
	rep.SeqGaps = int(obs.gaps.Load())
	if format == "json" {
		return printJSON(out, rep)
	}
	return printLoadReport(out, rep)
}

// generator places orders from one account at a steady rate.
type generator struct {
	o             loadgenOptions
	mid, spread   money.Amount
	qty           money.Amount
	rep           *loadReport
	sent, ok      atomic.Int64
	rejected      atomic.Int64
	throttled     atomic.Int64
	unavailable   atomic.Int64
	failed        atomic.Int64
	cancels       atomic.Int64
	orderLatency  samples
	cancelLatency samples
}

func (g *generator) run(ctx context.Context, a *loadAccount, idx int) {
	perAccount := float64(g.o.rate) / float64(g.o.accounts)      //nolint:forbidigo // a rate, not money
	interval := time.Duration(float64(time.Second) / perAccount) //nolint:forbidigo // a rate, not money
	tick := time.NewTicker(max(interval, time.Millisecond))
	defer tick.Stop()
	rng := rand.New(rand.NewPCG(uint64(idx), uint64(time.Now().UnixNano()))) //nolint:gosec // load shaping, not security
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		n++
		if g.o.cancelEvery > 0 && n%g.o.cancelEvery == 0 && len(a.resting) > 0 {
			id := a.resting[0]
			a.resting = a.resting[1:]
			t0 := time.Now()
			resp, err := a.client.CancelOrderWithResponse(ctx, id)
			if err == nil && resp.StatusCode() < 300 {
				g.cancelLatency.add(time.Since(t0))
				g.cancels.Add(1)
			}
		}
		side := "buy"
		if n%2 == 0 {
			side = "sell"
		}
		// resting: inside the spread on the own side; crossing: through it
		offset := money.MustParse(fmt.Sprintf("%d", rng.IntN(100)+1)).Mul(g.spread).Truncate(2)
		hundred := money.FromInt64(100)
		off, _ := offset.DivRoundDown(hundred, 2)
		price := g.mid.Sub(off)
		if side == "sell" {
			price = g.mid.Add(off)
		}
		if g.o.crossEvery > 0 && n%g.o.crossEvery == 0 {
			if side == "buy" {
				price = g.mid.Add(g.spread)
			} else {
				price = g.mid.Sub(g.spread)
			}
		}
		if !price.IsPositive() {
			price = money.MustParse("0.01")
		}
		body := apiclient.PlaceOrderJSONRequestBody{
			ClientOrderID: fmt.Sprintf("lg-%d-%d-%d", idx, n, time.Now().UnixNano()), Market: g.o.market, Side: apiclient.Side(side), Type: "limit",
			Price: &price, Qty: &g.qty,
		}
		t0 := time.Now()
		resp, err := a.client.PlaceOrderWithResponse(ctx, body)
		lat := time.Since(t0)
		g.sent.Add(1)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				g.failed.Add(1)
			}
		case resp.JSON201 != nil:
			g.orderLatency.add(lat)
			if resp.JSON201.Order.Status == "rejected" {
				g.rejected.Add(1)
			} else {
				g.ok.Add(1)
				if resp.JSON201.Order.Status == "open" || resp.JSON201.Order.Status == "partially_filled" {
					a.resting = append(a.resting, resp.JSON201.Order.ID)
					if len(a.resting) > 50 {
						a.resting = a.resting[len(a.resting)-50:]
					}
				}
			}
		case resp.StatusCode() == http.StatusTooManyRequests:
			g.throttled.Add(1)
		case resp.StatusCode() == http.StatusServiceUnavailable:
			g.unavailable.Add(1)
		default:
			g.failed.Add(1)
		}
	}
}

// observers are the WebSocket side of the measurement.
type observers struct {
	trades, deltas atomic.Int64
	privateFrames  atomic.Int64
	idleOpened     atomic.Int64
	closedEarly    atomic.Int64
	gaps           atomic.Int64
	depthDelay     samples
	tradeDelay     samples
	privateDelay   samples
}

// pushFrame is the part of any server message the observer times.
type pushFrame struct {
	Type       string    `json:"type"`
	Channel    string    `json:"channel"`
	Seq        uint64    `json:"seq"`
	At         time.Time `json:"at"`
	OccurredAt time.Time `json:"occurred_at"`
	Code       string    `json:"code"`
}

func dialWS(ctx context.Context, base, path string) (*websocket.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(dctx, strings.TrimSuffix(base, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(1 << 20)
	return c, nil
}

func writeJSON(ctx context.Context, c *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, b)
}

func (o *observers) public(ctx context.Context, base, market string) {
	c, err := dialWS(ctx, base, "/ws/v1/public")
	if err != nil {
		o.closedEarly.Add(1)
		return
	}
	defer c.CloseNow() //nolint:errcheck
	for _, ch := range []string{"depth", "trades"} {
		if err := writeJSON(ctx, c, map[string]any{"op": "subscribe", "channel": ch, "market": market}); err != nil {
			o.closedEarly.Add(1)
			return
		}
	}
	var last uint64
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				o.closedEarly.Add(1)
			}
			return
		}
		var f pushFrame
		if json.Unmarshal(b, &f) != nil {
			continue
		}
		now := time.Now()
		switch {
		case f.Channel == "depth" && f.Type == "snapshot":
			last = f.Seq
		case f.Channel == "depth" && f.Type == "delta":
			if last != 0 && f.Seq != last+1 {
				o.gaps.Add(1)
			}
			last = f.Seq
			o.deltas.Add(1)
			o.depthDelay.add(now.Sub(f.At))
		case f.Channel == "trades":
			o.trades.Add(1)
			o.tradeDelay.add(now.Sub(f.At))
		}
	}
}

func (o *observers) private(ctx context.Context, base, token string) {
	c, err := dialWS(ctx, base, "/ws/v1/private")
	if err != nil {
		o.closedEarly.Add(1)
		return
	}
	defer c.CloseNow() //nolint:errcheck
	if err := writeJSON(ctx, c, map[string]any{"op": "auth", "token": token}); err != nil {
		o.closedEarly.Add(1)
		return
	}
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				o.closedEarly.Add(1)
			}
			return
		}
		var f pushFrame
		if json.Unmarshal(b, &f) != nil {
			continue
		}
		if f.Channel == "orders" || f.Channel == "fills" || f.Channel == "balances" {
			o.privateFrames.Add(1)
			o.privateDelay.add(time.Since(f.OccurredAt))
		}
	}
}

// idle holds n connections open, answering nothing but pings, until ctx ends.
func (o *observers) idle(ctx context.Context, base string, n int) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := dialWS(ctx, base, "/ws/v1/public")
			if err != nil {
				return
			}
			defer c.CloseNow() //nolint:errcheck
			o.idleOpened.Add(1)
			for {
				if _, _, err := c.Read(ctx); err != nil {
					if ctx.Err() == nil {
						o.closedEarly.Add(1)
					}
					return
				}
			}
		}()
	}
	wg.Wait()
}

func printLoadReport(out io.Writer, r loadReport) error {
	row := func(name string, l latency) []string {
		return []string{name, fmt.Sprintf("%d", l.Count), fmt.Sprintf("%.1f", l.P50), fmt.Sprintf("%.1f", l.P95), fmt.Sprintf("%.1f", l.P99), fmt.Sprintf("%.1f", l.Max)}
	}
	_, _ = fmt.Fprintf(out, "loadgen %s: %d accounts, %d orders/s asked, %s\n", r.Market, r.Accounts, r.RateAsked, r.Duration)
	_, _ = fmt.Fprintf(out, "orders: sent %d, ok %d, rejected %d, 429 %d, 503 %d, errors %d, cancels %d -> %.1f orders/s\n",
		r.OrdersSent, r.OrdersOK, r.OrdersRejected, r.Throttled, r.Unavailable, r.Errors, r.Cancels, r.OrdersPerSec)
	_, _ = fmt.Fprintf(out, "trades seen: %d (%.1f/s); depth deltas: %d, seq gaps: %d\n", r.TradesSeen, r.TradesPerSec, r.DepthDeltas, r.SeqGaps)
	_, _ = fmt.Fprintf(out, "websockets: %d public, %d private, %d idle held, %d closed early\n", r.WSClients, r.PrivateClients, r.IdleOpened, r.WSClosed)
	return printTable(out, []string{"LATENCY", "N", "P50_MS", "P95_MS", "P99_MS", "MAX_MS"}, [][]string{
		row("POST /v1/orders", r.OrderLatency), row("DELETE /v1/orders/{id}", r.CancelLatency),
		row("depth delta push", r.DepthDelay), row("trade push", r.TradeDelay), row("private push", r.PrivateDelay),
	})
}
