package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/adminclient"
	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/apiclient"
)

func TestSummarize(t *testing.T) {
	assert.Equal(t, latency{}, summarize(nil))
	var v []time.Duration
	for i := 1; i <= 100; i++ {
		v = append(v, time.Duration(i)*time.Millisecond)
	}
	s := summarize(v)
	assert.Equal(t, 100, s.Count)
	assert.InDelta(t, 50, s.P50, 0.001)
	assert.InDelta(t, 95, s.P95, 0.001)
	assert.InDelta(t, 99, s.P99, 0.001)
	assert.InDelta(t, 100, s.Max, 0.001)
	one := summarize([]time.Duration{7 * time.Millisecond})
	assert.InDelta(t, 7, one.P99, 0.001)
}

// fakeLoadStack is an api + admin + stream role that answers just enough
// for a short run: registration, the faucet, orders (every third rejected,
// every seventh throttled), cancels, and a WebSocket that acknowledges
// subscriptions and pushes a delta and a trade every 20 ms.
func fakeLoadStack(t *testing.T) (api, admin, ws *httptest.Server, orders *atomic.Int64) {
	t.Helper()
	orders = &atomic.Int64{}
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/register", func(w http.ResponseWriter, r *http.Request) {
		var req apiclient.RegisterJSONRequestBody
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		s := fakeSession()
		s.AccountID = "acct-" + strings.SplitN(string(req.Email), "@", 2)[0]
		writeJSON(w, http.StatusCreated, s)
	})
	mux.HandleFunc("POST /v1/orders", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer "+fakeToken, r.Header.Get("Authorization"))
		var req apiclient.PlaceOrderJSONRequestBody
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		n := orders.Add(1)
		switch {
		case n%7 == 0:
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(apiclient.Problem{Type: "about:blank", Title: "Too Many Requests", Status: 429})
			return
		case n%3 == 0:
			o := fakeOrder(req.ClientOrderID, "rejected")
			writeJSON(w, http.StatusCreated, apiclient.OrderResult{Order: o, Trades: []apiclient.Fill{}})
			return
		}
		o := fakeOrder(req.ClientOrderID, "open")
		o.ID = "ord-" + req.ClientOrderID
		writeJSON(w, http.StatusCreated, apiclient.OrderResult{Order: o, Trades: []apiclient.Fill{}})
	})
	mux.HandleFunc("DELETE /v1/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, fakeOrder("x", "cancelled"))
	})
	api = httptest.NewServer(mux)
	t.Cleanup(api.Close)

	amux := http.NewServeMux()
	amux.HandleFunc("POST /admin/v1/ledger/adjustments", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "k", r.Header.Get("X-Admin-Api-Key"))
		writeJSON(w, http.StatusCreated, adminclient.JournalEntry{ID: 1})
	})
	admin = httptest.NewServer(amux)
	t.Cleanup(admin.Close)

	wmux := http.NewServeMux()
	wmux.HandleFunc("GET /ws/v1/public", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow() //nolint:errcheck
		ctx := r.Context()
		go func() {
			for {
				_, b, err := c.Read(ctx)
				if err != nil {
					return
				}
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"subscribed","channel":"`+m["channel"].(string)+`"}`))
			}
		}()
		seq := 10
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"channel":"depth","type":"snapshot","market":"ETH-USDC","seq":10,"bids":[],"asks":[]}`))
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			seq++
			at := time.Now().Add(-3 * time.Millisecond).UTC().Format(time.RFC3339Nano)
			if err := c.Write(ctx, websocket.MessageText, []byte(`{"channel":"depth","type":"delta","market":"ETH-USDC","seq":`+itoa(seq)+`,"at":"`+at+`","bids":[["1990","1"]],"asks":[]}`)); err != nil {
				return
			}
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"channel":"trades","type":"update","market":"ETH-USDC","seq":`+itoa(seq)+`,"at":"`+at+`","trade":{"price":"1990"}}`))
		}
	})
	wmux.HandleFunc("GET /ws/v1/private", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow() //nolint:errcheck
		ctx := r.Context()
		_, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		require.Contains(t, string(b), `"op":"auth"`)
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"auth","account_id":"a","account_seq":1}`))
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Millisecond):
			}
			at := time.Now().Add(-2 * time.Millisecond).UTC().Format(time.RFC3339Nano)
			if err := c.Write(ctx, websocket.MessageText, []byte(`{"channel":"orders","type":"order.accepted","account_seq":2,"occurred_at":"`+at+`","data":{}}`)); err != nil {
				return
			}
		}
	})
	ws = httptest.NewServer(wmux)
	t.Cleanup(ws.Close)
	return api, admin, ws, orders
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestLoadgenRunsAndReports(t *testing.T) {
	api, admin, ws, orders := fakeLoadStack(t)
	out, err := run(t, "--base-url", api.URL, "--output", "json", "loadgen",
		"--admin-url", admin.URL, "--admin-key", "k", "--ws-url", "ws"+strings.TrimPrefix(ws.URL, "http"),
		"--accounts", "2", "--rate", "40", "--duration", "600ms", "--ws-clients", "2", "--private-clients", "1", "--idle-connections", "3")
	require.NoError(t, err, out)
	var rep loadReport
	require.NoError(t, json.Unmarshal([]byte(out), &rep))
	assert.Equal(t, 2, rep.Accounts)
	// requests cut off by the deadline are counted as sent (and as errors)
	// before the fake could count them
	assert.InDelta(t, float64(orders.Load()), float64(rep.OrdersSent), 2) //nolint:forbidigo // counts, not money
	assert.Greater(t, rep.OrdersSent, 10)
	assert.Positive(t, rep.OrdersOK)
	assert.Positive(t, rep.OrdersRejected, "every third order is rejected by the fake")
	assert.Positive(t, rep.Throttled, "every seventh is a 429")
	assert.Positive(t, rep.Cancels)
	assert.Positive(t, rep.OrderLatency.Count)
	assert.Positive(t, rep.DepthDeltas)
	assert.Positive(t, rep.TradesSeen)
	assert.Positive(t, rep.DepthDelay.Count)
	assert.Greater(t, rep.DepthDelay.P50, 0.0)
	assert.Positive(t, rep.PrivateSeen)
	assert.Equal(t, 3, rep.IdleOpened)
	assert.Equal(t, 0, rep.SeqGaps)
	assert.Equal(t, 0, rep.WSClosed)

	text, err := run(t, "--base-url", api.URL, "loadgen",
		"--admin-url", admin.URL, "--admin-key", "k", "--ws-url", "ws"+strings.TrimPrefix(ws.URL, "http"),
		"--accounts", "1", "--rate", "10", "--duration", "200ms", "--ws-clients", "0", "--private-clients", "0")
	require.NoError(t, err, text)
	assert.Contains(t, text, "POST /v1/orders")
	assert.Contains(t, text, "orders/s")

	_, err = run(t, "--base-url", api.URL, "loadgen", "--admin-key", "", "--duration", "1s")
	require.Error(t, err, "the faucet needs the admin key")
}

var _ = context.Background
