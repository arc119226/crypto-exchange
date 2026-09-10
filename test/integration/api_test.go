//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/api"
	"github.com/arc119226/crypto-exchange/internal/api/gen"
	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/chain"
	"github.com/arc119226/crypto-exchange/internal/marketdata"
	"github.com/arc119226/crypto-exchange/internal/ratelimit"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// apiHarness is a trading harness plus the public REST API (auth middleware,
// OpenAPI routes, in-memory rate limiter) on an httptest server. It mirrors
// internal/app.newAPIRouter minus metrics and panic recovery.
type apiHarness struct {
	*tradingHarness
	auth *auth.Service
	srv  *httptest.Server
}

const (
	loginPerAccount = 5 // docs/plan-v1.0.md §14: the 6th attempt in a minute is a 429
	password        = "correct horse battery"
)

func setupAPI(t *testing.T) *apiHarness {
	t.Helper()
	th := setupTrading(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := auth.NewSigner(priv, "exchange")
	require.NoError(t, err)
	verifier, err := signer.VerifierFor()
	require.NoError(t, err)
	master := make([]byte, auth.MasterKeyLen)
	_, err = rand.Read(master)
	require.NoError(t, err)
	authSvc, err := auth.New(th.all, auth.Config{Tenant: "default", MasterKey: master, Password: auth.TestPasswordParams},
		signer, verifier, th.svc2(), audit.NewRecorder("default"))
	require.NoError(t, err)

	limits := api.Limits{
		LoginPerIP:       ratelimit.Limit{N: 1000, Window: time.Minute}, // every request here comes from 127.0.0.1
		LoginPerAccount:  ratelimit.Limit{N: loginPerAccount, Window: time.Minute},
		OrdersPerAccount: ratelimit.Limit{N: 1000, Window: time.Second},
	}
	r := chi.NewRouter()
	r.Use(telemetry.CorrelationMiddleware(th.log))
	r.Use(authSvc.Authenticate(api.WriteProblem))
	api.Mount(r, api.NewHandler(api.Deps{
		Tenant: "default", Registry: th.store, Auth: authSvc, Ledger: th.svc2(), Trading: th.svc,
		Limiter: ratelimit.NewMemory(), Limits: limits,
		Chain:      chain.NewAddresses(th.all, "default", testChainID),
		MarketData: marketdata.NewStore(th.all, "default"),
	}))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &apiHarness{tradingHarness: th, auth: authSvc, srv: srv}
}

// cred is how a request authenticates: nothing, a Bearer token, or an API
// key that signs the request (with knobs to produce invalid signatures).
type cred struct {
	token     string
	keyID     string
	secret    string
	timestamp string // "" = now
	badSig    bool
}

func bearer(s gen.Session) cred { return cred{token: s.AccessToken} }

func apiKey(k gen.CreatedAPIKey) cred { return cred{keyID: k.KeyID, secret: k.Secret} }

func (c cred) apply(req *http.Request, body []byte) {
	switch {
	case c.token != "":
		req.Header.Set("Authorization", "Bearer "+c.token)
	case c.keyID != "":
		ts := c.timestamp
		if ts == "" {
			ts = strconv.FormatInt(time.Now().UnixMilli(), 10)
		}
		sig := auth.SignRequest(c.secret, ts, req.Method, req.URL.RequestURI(), body)
		if c.badSig {
			sig = strings.Repeat("0", len(sig))
		}
		req.Header.Set(auth.HeaderAPIKey, c.keyID)
		req.Header.Set(auth.HeaderAPITimestamp, ts)
		req.Header.Set(auth.HeaderAPISignature, sig)
	}
}

func (h *apiHarness) do(t *testing.T, method, path string, c cred, body any) adminResp {
	t.Helper()
	var raw []byte
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		raw = b
	}
	req, err := http.NewRequestWithContext(context.Background(), method, h.srv.URL+path, bytes.NewReader(raw))
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(telemetry.RequestIDHeader, "it-api")
	c.apply(req, raw)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return adminResp{status: resp.StatusCode, body: out, hdr: resp.Header}
}

func (h *apiHarness) register(t *testing.T, email string) gen.Session {
	t.Helper()
	r := h.do(t, http.MethodPost, "/v1/auth/register", cred{}, map[string]any{"email": email, "password": password})
	require.Equal(t, http.StatusCreated, r.status, string(r.body))
	return decode[gen.Session](t, r)
}

func (h *apiHarness) login(t *testing.T, email, pw string) adminResp {
	t.Helper()
	return h.do(t, http.MethodPost, "/v1/auth/login", cred{}, map[string]any{"email": email, "password": pw})
}

func limitOrder(cid, side, price, qty string) map[string]any {
	return map[string]any{"client_order_id": cid, "market": market, "side": side, "type": "limit", "price": price, "qty": qty}
}

func (h *apiHarness) place(t *testing.T, c cred, body map[string]any, wantStatus int) gen.OrderResult {
	t.Helper()
	r := h.do(t, http.MethodPost, "/v1/orders", c, body)
	require.Equal(t, wantStatus, r.status, string(r.body))
	return decode[gen.OrderResult](t, r)
}

func (h *apiHarness) balances(t *testing.T, c cred) map[string]gen.Balance {
	t.Helper()
	r := h.do(t, http.MethodGet, "/v1/balances", c, nil)
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	out := map[string]gen.Balance{}
	for _, b := range decode[gen.BalanceList](t, r).Balances {
		out[b.Asset] = b
	}
	return out
}

func problemOf(t *testing.T, r adminResp) gen.Problem {
	t.Helper()
	assert.Equal(t, api.ProblemContentType, r.hdr.Get("Content-Type"), string(r.body))
	p := decode[gen.Problem](t, r)
	assert.Equal(t, r.status, p.Status)
	assert.Equal(t, "it-api", p.CorrelationID)
	return p
}

func TestPublicAPI(t *testing.T) {
	h := setupAPI(t)
	ctx := context.Background()

	t.Run("public routes and unauthenticated access", func(t *testing.T) {
		r := h.do(t, http.MethodGet, "/v1/markets", cred{}, nil)
		assert.Equal(t, http.StatusOK, r.status)
		r = h.do(t, http.MethodGet, "/.well-known/jwks.json", cred{}, nil)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		jwks := decode[gen.JWKS](t, r)
		require.Len(t, jwks.Keys, 1)
		assert.Equal(t, "OKP", jwks.Keys[0]["kty"])
		assert.Equal(t, "Ed25519", jwks.Keys[0]["crv"])
		assert.NotEmpty(t, jwks.Keys[0]["kid"])
		assert.NotContains(t, jwks.Keys[0], "d", "no private material in the JWKS")

		for _, ep := range []struct{ method, path string }{
			{http.MethodGet, "/v1/balances"}, {http.MethodGet, "/v1/account"}, {http.MethodGet, "/v1/orders"},
			{http.MethodGet, "/v1/fills"}, {http.MethodGet, "/v1/api-keys"}, {http.MethodGet, "/v1/ledger/entries"},
		} {
			r := h.do(t, ep.method, ep.path, cred{}, nil)
			assert.Equal(t, http.StatusUnauthorized, r.status, ep.path)
			problemOf(t, r)
		}
		r = h.do(t, http.MethodPost, "/v1/orders", cred{}, limitOrder("anon", "buy", "2000", "1"))
		assert.Equal(t, http.StatusUnauthorized, r.status)
		r = h.do(t, http.MethodGet, "/v1/balances", cred{token: "not-a-jwt"}, nil)
		assert.Equal(t, http.StatusUnauthorized, r.status)
		assert.Contains(t, r.hdr.Get("WWW-Authenticate"), "invalid_token")
		problemOf(t, r)
	})

	var buyer, seller gen.Session
	t.Run("register, login, refresh, logout", func(t *testing.T) {
		buyer = h.register(t, "Buyer@Example.com")
		assert.Equal(t, gen.SessionRoleUser, buyer.Role)
		assert.Equal(t, "Bearer", buyer.TokenType)
		assert.NotEmpty(t, buyer.AccessToken)
		assert.NotEmpty(t, buyer.RefreshToken)
		assert.NotEmpty(t, buyer.AccountID)
		assert.InDelta(t, 15*time.Minute.Seconds(), float64(buyer.ExpiresIn), 1)

		r := h.do(t, http.MethodPost, "/v1/auth/register", cred{}, map[string]any{"email": "buyer@example.com", "password": password})
		assert.Equal(t, http.StatusConflict, r.status, "emails are case-insensitive")
		problemOf(t, r)
		r = h.do(t, http.MethodPost, "/v1/auth/register", cred{}, map[string]any{"email": "weak@example.com", "password": "short"})
		assert.Equal(t, http.StatusUnprocessableEntity, r.status, string(r.body))
		r = h.do(t, http.MethodPost, "/v1/auth/register", cred{}, map[string]any{"email": "not-an-email", "password": password})
		assert.Equal(t, http.StatusBadRequest, r.status, string(r.body))

		r = h.login(t, "buyer@example.com", "wrong password")
		assert.Equal(t, http.StatusUnauthorized, r.status)
		problemOf(t, r)
		r = h.login(t, "nobody@example.com", password)
		assert.Equal(t, http.StatusUnauthorized, r.status, "unknown emails look exactly like wrong passwords")
		r = h.login(t, "buyer@example.com", password)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		first := decode[gen.Session](t, r)
		assert.Equal(t, buyer.AccountID, first.AccountID)

		// rotation: the old refresh token dies, the new one lives
		r = h.do(t, http.MethodPost, "/v1/auth/refresh", cred{}, map[string]any{"refresh_token": first.RefreshToken})
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		second := decode[gen.Session](t, r)
		assert.NotEqual(t, first.RefreshToken, second.RefreshToken)
		// reuse of a rotated token is theft: the whole family is revoked
		r = h.do(t, http.MethodPost, "/v1/auth/refresh", cred{}, map[string]any{"refresh_token": first.RefreshToken})
		assert.Equal(t, http.StatusUnauthorized, r.status, "reused refresh token")
		r = h.do(t, http.MethodPost, "/v1/auth/refresh", cred{}, map[string]any{"refresh_token": second.RefreshToken})
		assert.Equal(t, http.StatusUnauthorized, r.status, "the descendant is revoked too")
		// access tokens are stateless and keep working until they expire
		r = h.do(t, http.MethodGet, "/v1/account", bearer(second), nil)
		assert.Equal(t, http.StatusOK, r.status)

		r = h.do(t, http.MethodPost, "/v1/auth/logout", cred{}, map[string]any{"refresh_token": buyer.RefreshToken})
		assert.Equal(t, http.StatusNoContent, r.status)
		r = h.do(t, http.MethodPost, "/v1/auth/refresh", cred{}, map[string]any{"refresh_token": buyer.RefreshToken})
		assert.Equal(t, http.StatusUnauthorized, r.status, "logged-out refresh token")
		r = h.do(t, http.MethodPost, "/v1/auth/logout", cred{}, map[string]any{"refresh_token": buyer.RefreshToken})
		assert.Equal(t, http.StatusNoContent, r.status, "logout is idempotent")
		r = h.do(t, http.MethodPost, "/v1/auth/refresh", cred{}, map[string]any{})
		assert.Equal(t, http.StatusBadRequest, r.status)

		seller = h.register(t, "seller@example.com")
	})

	t.Run("account", func(t *testing.T) {
		r := h.do(t, http.MethodGet, "/v1/account", bearer(buyer), nil)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		a := decode[gen.Account](t, r)
		assert.Equal(t, buyer.UserID, a.UserID)
		assert.Equal(t, buyer.AccountID, a.AccountID)
		assert.Equal(t, "buyer@example.com", a.Email)
		assert.Equal(t, gen.AccountRoleUser, a.Role)
		assert.Equal(t, gen.AccountStatusActive, a.Status)
		assert.EqualValues(t, 0, a.KycLevel)
		assert.Empty(t, h.balances(t, bearer(buyer)), "no balance rows before funding")
	})

	// the dev faucet (docs/plan-v1.0.md §6.1.4 g) funds both accounts
	h.fund(t, ctx, buyer.AccountID, "USDC", "10000", "faucet:api:buyer")
	h.fund(t, ctx, seller.AccountID, "ETH", "1", "faucet:api:seller")

	var buy gen.OrderResult
	t.Run("orders: plan §6.1.4 flow over HTTP", func(t *testing.T) {
		sell := h.place(t, bearer(seller), limitOrder("s1", "sell", "1990", "0.4"), http.StatusCreated)
		assert.Equal(t, gen.OrderStatusOpen, sell.Order.Status)
		assert.Empty(t, sell.Trades)
		eq(t, "0.4", sell.Order.HoldRemaining)
		assert.Equal(t, "ETH", sell.Order.HoldAsset)

		buy = h.place(t, bearer(buyer), limitOrder("b1", "buy", "2000", "1"), http.StatusCreated)
		assert.Equal(t, gen.OrderStatusPartiallyFilled, buy.Order.Status)
		eq(t, "0.4", buy.Order.FilledQty)
		eq(t, "796", buy.Order.FilledQuote)
		eq(t, "0.6", buy.Order.RemainingQty)
		eq(t, "1200", buy.Order.HoldRemaining, "2000 − 796 − 4 price improvement")
		require.Len(t, buy.Trades, 1)
		f := buy.Trades[0]
		eq(t, "1990", f.Price)
		eq(t, "0.4", f.Qty)
		eq(t, "796", f.QuoteQty)
		assert.Equal(t, gen.SideBuy, f.Side)
		assert.Equal(t, gen.SideBuy, f.TakerSide)
		assert.False(t, f.IsMaker)
		eq(t, "0.0008", f.Fee)
		assert.Equal(t, "ETH", f.FeeAsset)
		assert.Equal(t, buy.Order.ID, f.OrderID)
		require.NotNil(t, buy.Order.Seq)

		bb := h.balances(t, bearer(buyer))
		eq(t, "8004", bb["USDC"].Available)
		eq(t, "1200", bb["USDC"].Hold)
		eq(t, "9204", bb["USDC"].Total)
		eq(t, "0.3992", bb["ETH"].Available)
		sb := h.balances(t, bearer(seller))
		eq(t, "795.204", sb["USDC"].Available)
		eq(t, "0.6", sb["ETH"].Available)
		eq(t, "0", sb["ETH"].Hold)

		// idempotent replay → 200 and the same order; a different body → 422
		r := h.do(t, http.MethodPost, "/v1/orders", bearer(buyer), limitOrder("b1", "buy", "2000", "1"))
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		assert.Equal(t, buy.Order.ID, decode[gen.OrderResult](t, r).Order.ID)
		r = h.do(t, http.MethodPost, "/v1/orders", bearer(buyer), limitOrder("b1", "buy", "2001", "1"))
		assert.Equal(t, http.StatusUnprocessableEntity, r.status, string(r.body))
		problemOf(t, r)
		// the same client_order_id belongs to another account: independent
		r = h.do(t, http.MethodPost, "/v1/orders", bearer(seller), limitOrder("b1", "sell", "2500", "0.1"))
		assert.Equal(t, http.StatusCreated, r.status, string(r.body))
		otherB1 := decode[gen.OrderResult](t, r)
		assert.NotEqual(t, buy.Order.ID, otherB1.Order.ID)

		// business rejections are orders in status rejected (201), not errors
		rej := h.place(t, bearer(buyer), limitOrder("b-too-big", "buy", "2000", "100"), http.StatusCreated)
		assert.Equal(t, gen.OrderStatusRejected, rej.Order.Status)
		assert.Equal(t, "insufficient_balance", rej.Order.RejectReason)
		rej = h.place(t, bearer(buyer), limitOrder("b-tick", "buy", "2000.001", "0.1"), http.StatusCreated)
		assert.Equal(t, gen.OrderStatusRejected, rej.Order.Status)
		assert.Equal(t, "invalid_price_tick", rej.Order.RejectReason)
		// malformed input is a 400 problem
		r = h.do(t, http.MethodPost, "/v1/orders", bearer(buyer), map[string]any{"client_order_id": "x", "market": market, "side": "sideways", "type": "limit", "price": "1", "qty": "1"})
		assert.Equal(t, http.StatusBadRequest, r.status, string(r.body))
		r = h.do(t, http.MethodPost, "/v1/orders", bearer(buyer), map[string]any{"client_order_id": "x", "market": market, "side": "buy", "type": "limit", "price": 2000, "qty": "1"})
		assert.Equal(t, http.StatusBadRequest, r.status, "JSON numbers are never accepted for amounts")
		r = h.do(t, http.MethodPost, "/v1/orders", bearer(buyer), limitOrder("", "buy", "2000", "0.1"))
		if assert.Equal(t, http.StatusBadRequest, r.status, string(r.body)) {
			assert.Contains(t, problemOf(t, r).Detail, "client_order_id")
		}
		body := limitOrder("nomarket", "buy", "1", "1")
		body["market"] = "NOPE-USDC"
		r = h.do(t, http.MethodPost, "/v1/orders", bearer(buyer), body)
		assert.Equal(t, http.StatusNotFound, r.status, string(r.body))

		// reads: get, list, fills — always scoped to the caller
		r = h.do(t, http.MethodGet, "/v1/orders/"+buy.Order.ID, bearer(buyer), nil)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		got := decode[gen.OrderResult](t, r)
		assert.Equal(t, gen.OrderStatusPartiallyFilled, got.Order.Status)
		require.Len(t, got.Trades, 1)
		r = h.do(t, http.MethodGet, "/v1/orders/"+buy.Order.ID, bearer(seller), nil)
		assert.Equal(t, http.StatusNotFound, r.status, "another account's order is invisible")
		r = h.do(t, http.MethodGet, "/v1/orders/01J00000000000000000000000", bearer(buyer), nil)
		assert.Equal(t, http.StatusNotFound, r.status)

		r = h.do(t, http.MethodGet, "/v1/orders?open_only=true", bearer(buyer), nil)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		open := decode[gen.OrderList](t, r)
		require.Len(t, open.Orders, 1)
		assert.Equal(t, buy.Order.ID, open.Orders[0].ID)
		r = h.do(t, http.MethodGet, "/v1/orders?status=rejected&market="+market, bearer(buyer), nil)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		assert.Len(t, decode[gen.OrderList](t, r).Orders, 2)
		r = h.do(t, http.MethodGet, "/v1/orders?status=bogus", bearer(buyer), nil)
		assert.Equal(t, http.StatusBadRequest, r.status)

		r = h.do(t, http.MethodGet, "/v1/fills", bearer(seller), nil)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		sf := decode[gen.FillList](t, r)
		require.Len(t, sf.Fills, 1)
		assert.True(t, sf.Fills[0].IsMaker)
		assert.Equal(t, gen.SideSell, sf.Fills[0].Side)
		assert.Equal(t, gen.SideBuy, sf.Fills[0].TakerSide)
		eq(t, "0.796", sf.Fills[0].Fee)
		assert.Equal(t, "USDC", sf.Fills[0].FeeAsset)
		assert.Equal(t, sell.Order.ID, sf.Fills[0].OrderID)
		assert.Equal(t, f.TradeID, sf.Fills[0].TradeID, "both sides see the same trade id")
		r = h.do(t, http.MethodGet, "/v1/fills?order_id="+buy.Order.ID, bearer(buyer), nil)
		require.Equal(t, http.StatusOK, r.status)
		assert.Len(t, decode[gen.FillList](t, r).Fills, 1)
		r = h.do(t, http.MethodGet, "/v1/fills?order_id="+buy.Order.ID, bearer(seller), nil)
		require.Equal(t, http.StatusOK, r.status)
		assert.Empty(t, decode[gen.FillList](t, r).Fills, "filtering by someone else's order yields nothing")

		// ledger entries: the caller's postings only
		r = h.do(t, http.MethodGet, "/v1/ledger/entries", bearer(buyer), nil)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		entries := decode[gen.LedgerEntryList](t, r).Entries
		kinds := map[string]int{}
		for _, e := range entries {
			kinds[e.Kind]++
			assert.NotEmpty(t, e.Postings, "entry %d", e.ID)
		}
		assert.Equal(t, 1, kinds["adjustment"], kinds)
		assert.Equal(t, 1, kinds["hold"], kinds)
		assert.Equal(t, 1, kinds["settle"], kinds)
		var settle gen.LedgerEntry
		for _, e := range entries {
			if e.Kind == "settle" {
				settle = e
			}
		}
		for _, p := range settle.Postings {
			// the seller's legs and the fee revenue legs are not the buyer's business
			assert.Contains(t, []gen.PostingBucket{gen.PostingBucketAvailable, gen.PostingBucketHold}, p.Bucket)
		}
		assert.Len(t, settle.Postings, 4, "buyer: hold USDC −796, available ETH +0.3992, hold USDC −4, available USDC +4")
	})

	t.Run("market data", func(t *testing.T) {
		r := h.do(t, http.MethodGet, "/v1/markets/"+market+"/depth?limit=5", cred{}, nil)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		d := decode[gen.Depth](t, r)
		assert.Equal(t, market, d.Market)
		require.Len(t, d.Bids, 1)
		eq(t, "2000", d.Bids[0].Price)
		eq(t, "0.6", d.Bids[0].Qty)
		assert.EqualValues(t, 1, d.Bids[0].Orders)
		require.Len(t, d.Asks, 1, "the seller's 0.1 @ 2500")
		eq(t, "2500", d.Asks[0].Price)
		assert.Greater(t, d.LastSeq, *buy.Order.Seq)
		assert.Contains(t, string(r.body), `"price":"2000"`, "amounts are JSON strings")

		r = h.do(t, http.MethodGet, "/v1/markets/NOPE-USDC/depth", cred{}, nil)
		assert.Equal(t, http.StatusNotFound, r.status)
		problemOf(t, r)
		r = h.do(t, http.MethodGet, "/v1/markets/"+market+"/depth?limit=0", cred{}, nil)
		assert.Equal(t, http.StatusOK, r.status, "out-of-range limits fall back to the default")

		r = h.do(t, http.MethodGet, "/v1/markets/"+market+"/trades", cred{}, nil)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		trades := decode[gen.TradeList](t, r).Trades
		require.Len(t, trades, 1)
		eq(t, "1990", trades[0].Price)
		eq(t, "0.4", trades[0].Qty)
		assert.Equal(t, gen.SideBuy, trades[0].TakerSide)
		assert.Equal(t, buy.Trades[0].TradeID, trades[0].TradeID)
	})

	t.Run("cancel", func(t *testing.T) {
		r := h.do(t, http.MethodDelete, "/v1/orders/"+buy.Order.ID, bearer(seller), nil)
		assert.Equal(t, http.StatusNotFound, r.status, "cannot cancel someone else's order")
		r = h.do(t, http.MethodDelete, "/v1/orders/"+buy.Order.ID, bearer(buyer), nil)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		o := decode[gen.Order](t, r)
		assert.Equal(t, gen.OrderStatusCancelled, o.Status)
		assert.Equal(t, "user", o.CancelReason)
		eq(t, "0", o.HoldRemaining)
		bb := h.balances(t, bearer(buyer))
		eq(t, "9204", bb["USDC"].Available)
		eq(t, "0", bb["USDC"].Hold)
		// cancelling a terminal order is idempotent (§6.2)
		r = h.do(t, http.MethodDelete, "/v1/orders/"+buy.Order.ID, bearer(buyer), nil)
		assert.Equal(t, http.StatusOK, r.status)
		assert.Equal(t, gen.OrderStatusCancelled, decode[gen.Order](t, r).Status)
		r = h.do(t, http.MethodDelete, "/v1/orders/01J00000000000000000000000", bearer(buyer), nil)
		assert.Equal(t, http.StatusNotFound, r.status)

		h.assertHoldInvariant(t, ctx)
		h.assertTrialBalanceZero(t, ctx)
	})

	t.Run("api keys and HMAC requests", func(t *testing.T) {
		r := h.do(t, http.MethodPost, "/v1/api-keys", bearer(buyer), map[string]any{"label": "bot", "scopes": []string{"read"}})
		require.Equal(t, http.StatusCreated, r.status, string(r.body))
		readKey := decode[gen.CreatedAPIKey](t, r)
		assert.True(t, strings.HasPrefix(readKey.KeyID, "ak_"), readKey.KeyID)
		assert.Len(t, readKey.Secret, 64)
		assert.Equal(t, []gen.Scope{gen.ScopeRead}, readKey.Scopes)
		assert.Equal(t, "bot", readKey.Label)

		r = h.do(t, http.MethodGet, "/v1/api-keys", bearer(buyer), nil)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		assert.Len(t, decode[gen.APIKeyList](t, r).APIKeys, 1)
		assert.NotContains(t, string(r.body), readKey.Secret, "the secret is shown exactly once")

		// signed GET (empty body) and signed request with a query string
		r = h.do(t, http.MethodGet, "/v1/balances", apiKey(readKey), nil)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		r = h.do(t, http.MethodGet, "/v1/orders?status=cancelled&limit=5", apiKey(readKey), nil)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		assert.Len(t, decode[gen.OrderList](t, r).Orders, 1)
		// scope enforcement
		r = h.do(t, http.MethodPost, "/v1/orders", apiKey(readKey), limitOrder("k1", "buy", "2000", "0.1"))
		assert.Equal(t, http.StatusForbidden, r.status, string(r.body))
		problemOf(t, r)
		// keys cannot mint keys
		r = h.do(t, http.MethodPost, "/v1/api-keys", apiKey(readKey), map[string]any{"scopes": []string{"read"}})
		assert.Equal(t, http.StatusForbidden, r.status, string(r.body))
		// broken signatures and stale timestamps
		c := apiKey(readKey)
		c.badSig = true
		r = h.do(t, http.MethodGet, "/v1/balances", c, nil)
		assert.Equal(t, http.StatusUnauthorized, r.status)
		problemOf(t, r)
		c = apiKey(readKey)
		c.timestamp = strconv.FormatInt(time.Now().Add(-2*auth.MaxTimestampSkew).UnixMilli(), 10)
		r = h.do(t, http.MethodGet, "/v1/balances", c, nil)
		assert.Equal(t, http.StatusUnauthorized, r.status)
		c = apiKey(readKey)
		c.secret = strings.Repeat("ab", 32)
		r = h.do(t, http.MethodGet, "/v1/balances", c, nil)
		assert.Equal(t, http.StatusUnauthorized, r.status, "wrong secret")
		c = apiKey(readKey)
		c.keyID = "ak_000000000000000000000000"
		r = h.do(t, http.MethodGet, "/v1/balances", c, nil)
		assert.Equal(t, http.StatusUnauthorized, r.status, "unknown key")
		r = h.do(t, http.MethodGet, "/v1/balances", cred{keyID: readKey.KeyID, secret: readKey.Secret, timestamp: "yesterday"}, nil)
		assert.Equal(t, http.StatusUnauthorized, r.status, "unparsable timestamp")

		// a trade key signs a body and places an order; the body must match the signature
		r = h.do(t, http.MethodPost, "/v1/api-keys", bearer(seller), map[string]any{"scopes": []string{"read", "trade"}})
		require.Equal(t, http.StatusCreated, r.status, string(r.body))
		tradeKey := decode[gen.CreatedAPIKey](t, r)
		res := h.place(t, apiKey(tradeKey), limitOrder("k-sell", "sell", "2100", "0.1"), http.StatusCreated)
		assert.Equal(t, gen.OrderStatusOpen, res.Order.Status)
		r = h.do(t, http.MethodDelete, "/v1/orders/"+res.Order.ID, apiKey(tradeKey), nil)
		assert.Equal(t, http.StatusOK, r.status, string(r.body))
		// tampering with the body after signing
		raw, _ := json.Marshal(limitOrder("k-tamper", "sell", "2100", "0.1"))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/v1/orders", bytes.NewReader(bytes.Replace(raw, []byte(`"0.1"`), []byte(`"0.5"`), 1)))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		apiKey(tradeKey).apply(req, raw)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "signature covers the body")

		// IP allowlists: the test client is 127.0.0.1
		r = h.do(t, http.MethodPost, "/v1/api-keys", bearer(buyer), map[string]any{"scopes": []string{"read"}, "ip_allowlist": []string{"10.0.0.0/8", "192.0.2.1"}})
		require.Equal(t, http.StatusCreated, r.status, string(r.body))
		farKey := decode[gen.CreatedAPIKey](t, r)
		r = h.do(t, http.MethodGet, "/v1/balances", apiKey(farKey), nil)
		assert.Equal(t, http.StatusForbidden, r.status, string(r.body))
		problemOf(t, r)
		r = h.do(t, http.MethodPost, "/v1/api-keys", bearer(buyer), map[string]any{"scopes": []string{"read"}, "ip_allowlist": []string{"127.0.0.0/8"}})
		require.Equal(t, http.StatusCreated, r.status, string(r.body))
		nearKey := decode[gen.CreatedAPIKey](t, r)
		r = h.do(t, http.MethodGet, "/v1/balances", apiKey(nearKey), nil)
		assert.Equal(t, http.StatusOK, r.status, string(r.body))
		// validation
		r = h.do(t, http.MethodPost, "/v1/api-keys", bearer(buyer), map[string]any{"scopes": []string{"read"}, "ip_allowlist": []string{"not-an-ip"}})
		assert.Equal(t, http.StatusBadRequest, r.status, string(r.body))
		r = h.do(t, http.MethodPost, "/v1/api-keys", bearer(buyer), map[string]any{"scopes": []string{"admin"}})
		assert.Equal(t, http.StatusBadRequest, r.status, string(r.body))
		r = h.do(t, http.MethodPost, "/v1/api-keys", bearer(buyer), map[string]any{"scopes": []string{}})
		assert.Equal(t, http.StatusBadRequest, r.status, "at least one scope")

		// revoke: 204, then the key is dead; someone else's key is a 404
		r = h.do(t, http.MethodDelete, "/v1/api-keys/"+readKey.ID, bearer(seller), nil)
		assert.Equal(t, http.StatusNotFound, r.status)
		r = h.do(t, http.MethodDelete, "/v1/api-keys/"+readKey.ID, bearer(buyer), nil)
		assert.Equal(t, http.StatusNoContent, r.status, string(r.body))
		r = h.do(t, http.MethodGet, "/v1/balances", apiKey(readKey), nil)
		assert.Equal(t, http.StatusUnauthorized, r.status, "revoked key")
		r = h.do(t, http.MethodDelete, "/v1/api-keys/"+readKey.ID, bearer(buyer), nil)
		assert.Equal(t, http.StatusNotFound, r.status, "already revoked")
		r = h.do(t, http.MethodGet, "/v1/api-keys", bearer(buyer), nil)
		require.Equal(t, http.StatusOK, r.status)
		var revoked int
		for _, k := range decode[gen.APIKeyList](t, r).APIKeys {
			if k.RevokedAt != nil {
				revoked++
			}
		}
		assert.Equal(t, 1, revoked)
	})

	t.Run("login rate limit: the 6th attempt for an account is a 429", func(t *testing.T) {
		h.register(t, "locked@example.com")
		for i := 1; i <= loginPerAccount; i++ {
			r := h.login(t, "locked@example.com", "wrong")
			assert.Equal(t, http.StatusUnauthorized, r.status, "attempt %d", i)
		}
		r := h.login(t, "LOCKED@example.com", password)
		assert.Equal(t, http.StatusTooManyRequests, r.status, "even the right password is refused while locked")
		retry, err := strconv.Atoi(r.hdr.Get("Retry-After"))
		require.NoError(t, err, "Retry-After: %q", r.hdr.Get("Retry-After"))
		assert.GreaterOrEqual(t, retry, 1)
		assert.LessOrEqual(t, retry, 12, "one token refills every 12 s at 5/min")
		problemOf(t, r)
		// other accounts are unaffected (separate bucket)
		r = h.login(t, "seller@example.com", password)
		assert.Equal(t, http.StatusOK, r.status, string(r.body))
	})

	t.Run("audit trail", func(t *testing.T) {
		rows, err := h.all.Query(ctx, `SELECT action, count(*) FROM audit.audit_events GROUP BY action`)
		require.NoError(t, err)
		defer rows.Close()
		counts := map[string]int64{}
		for rows.Next() {
			var action string
			var n int64
			require.NoError(t, rows.Scan(&action, &n))
			counts[action] = n
		}
		assert.EqualValues(t, 3, counts["auth.register"], counts)
		assert.GreaterOrEqual(t, counts["auth.login"], int64(2), counts)
		assert.GreaterOrEqual(t, counts["auth.login.failed"], int64(loginPerAccount+1), counts)
		assert.EqualValues(t, 1, counts["auth.refresh.reuse_detected"], counts, "only the rotated token's replay counts as reuse")
		assert.EqualValues(t, 4, counts["auth.api_key.create"], counts)
		assert.EqualValues(t, 1, counts["auth.api_key.revoke"], counts)
	})
}

// TestRefreshReuseThatCannotRevokeIsNotReportedAsAnOrdinaryRejection covers
// the failure mode the reuse path used to hide.
//
// Presenting a rotated refresh token means the old one leaked, and the answer
// is to revoke every token the user has. That revocation is a database write,
// and a database write can fail. The code used to read
//
//	if _, err := q.RevokeUserRefreshTokens(...); err == nil { audit }
//
// which used the error only to decide whether to write the audit row. So a
// failed revocation left the thief's other tokens live, recorded nothing, and
// answered 401 -- identical to an ordinary expired token. The compromise was
// both unhandled and invisible.
//
// The revocation is made to fail by taking UPDATE away from the role the
// service connects as, which is the narrowest way to break exactly that
// statement and nothing else. 401 here would mean the bug is back.
func TestRefreshReuseThatCannotRevokeIsNotReportedAsAnOrdinaryRejection(t *testing.T) {
	h := setupAPI(t)
	ctx := context.Background()

	r := h.do(t, http.MethodPost, "/v1/auth/register", cred{}, map[string]any{"email": "thief@example.com", "password": password})
	require.Equal(t, http.StatusCreated, r.status, string(r.body))
	r = h.do(t, http.MethodPost, "/v1/auth/login", cred{}, map[string]any{"email": "thief@example.com", "password": password})
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	first := decode[gen.Session](t, r)

	// Rotate while the grant is still in place, so the reused token below is
	// genuinely a rotated one rather than merely unknown.
	r = h.do(t, http.MethodPost, "/v1/auth/refresh", cred{}, map[string]any{"refresh_token": first.RefreshToken})
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	second := decode[gen.Session](t, r)

	// ex_migrate owns the database, so it is the one that can take the grant
	// away and put it back.
	owner, err := pgx.Connect(ctx, h.DSN("ex_migrate"))
	require.NoError(t, err)
	defer owner.Close(ctx)
	_, err = owner.Exec(ctx, `REVOKE UPDATE ON auth.refresh_tokens FROM ex_all`)
	require.NoError(t, err)
	restored := false
	restore := func() {
		if restored {
			return
		}
		_, err := owner.Exec(ctx, `GRANT UPDATE ON auth.refresh_tokens TO ex_all`)
		require.NoError(t, err)
		restored = true
	}
	defer restore()

	r = h.do(t, http.MethodPost, "/v1/auth/refresh", cred{}, map[string]any{"refresh_token": first.RefreshToken})
	assert.Equal(t, http.StatusInternalServerError, r.status,
		"a reuse whose revocation failed must not answer like an ordinary invalid token: %s", string(r.body))

	// And with the grant back, the same reuse revokes the family as it should.
	restore()
	r = h.do(t, http.MethodPost, "/v1/auth/refresh", cred{}, map[string]any{"refresh_token": first.RefreshToken})
	assert.Equal(t, http.StatusUnauthorized, r.status, string(r.body))
	r = h.do(t, http.MethodPost, "/v1/auth/refresh", cred{}, map[string]any{"refresh_token": second.RefreshToken})
	assert.Equal(t, http.StatusUnauthorized, r.status, "the descendant is revoked once the revocation succeeds")
}

// TestAdminBootstrap covers `exchange admin bootstrap`: idempotent, and the
// admin can log in through the public API with role admin.
func TestAdminBootstrap(t *testing.T) {
	h := setupAPI(t)
	ctx := context.Background()

	u, created, err := h.auth.BootstrapAdmin(ctx, "Admin@Example.com", password)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, "admin@example.com", u.Email)
	assert.Equal(t, auth.RoleAdmin, u.Role)

	again, created, err := h.auth.BootstrapAdmin(ctx, "admin@example.com", "a different password")
	require.NoError(t, err)
	assert.False(t, created, "second run is a no-op")
	assert.Equal(t, u.ID, again.ID)

	r := h.login(t, "admin@example.com", password)
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	s := decode[gen.Session](t, r)
	assert.Equal(t, gen.SessionRoleAdmin, s.Role)
	r = h.login(t, "admin@example.com", "a different password")
	assert.Equal(t, http.StatusUnauthorized, r.status, "bootstrap never rotates an existing password")

	var n int
	require.NoError(t, h.all.QueryRow(ctx, `SELECT count(*) FROM audit.audit_events WHERE action = 'auth.admin.bootstrap'`).Scan(&n))
	assert.Equal(t, 1, n)
}
