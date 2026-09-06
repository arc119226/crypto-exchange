package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/cmd/exchangectl/internal/apiclient"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/money"
)

const (
	fakeToken = "tok-access"
	fakeKeyID = "ak_000000000000000000000001"
	goodPass  = "pw12345678"
)

// fakeSecret has the shape of a real secret (32 bytes hex) without looking
// like one to secret scanners.
var fakeSecret = strings.Repeat("0a", 32)

func amt(s string) apiclient.Amount { return money.MustParse(s) }

func amtP(s string) *apiclient.Amount {
	a := money.MustParse(s)
	return &a
}

func fakeSession() apiclient.Session {
	return apiclient.Session{
		UserID: "u1", AccountID: "acct-1", Role: apiclient.SessionRole("user"), TokenType: "Bearer",
		AccessToken: fakeToken, RefreshToken: "rt-1", ExpiresIn: 900, ExpiresAt: time.Now().Add(15 * time.Minute),
	}
}

func fakeOrder(cid string, status string) apiclient.Order {
	seq := int64(2)
	now := time.Now()
	return apiclient.Order{
		ID: "01ORDER", ClientOrderID: cid, Market: "ETH-USDC", Side: apiclient.Side("buy"), Type: apiclient.OrderType("limit"),
		TimeInForce: apiclient.TimeInForce("gtc"), Price: amtP("2000"), Qty: amtP("1"),
		FilledQty: amt("0.4"), FilledQuote: amt("796"), RemainingQty: amt("0.6"),
		HoldAsset: "USDC", HoldAmount: amt("2000"), HoldRemaining: amt("1200"),
		Status: apiclient.OrderStatus(status), Seq: &seq, CreatedAt: now, UpdatedAt: now,
	}
}

func fakeFill() apiclient.Fill {
	return apiclient.Fill{
		TradeID: "01TRADE", OrderID: "01ORDER", Market: "ETH-USDC", Side: apiclient.Side("buy"), TakerSide: apiclient.Side("buy"),
		IsMaker: false, Price: amt("1990"), Qty: amt("0.4"), QuoteQty: amt("796"), Fee: amt("0.0008"), FeeAsset: "ETH", Seq: 2, ExecutedAt: time.Now(),
	}
}

// fakeTradingAPI is the public API as exchangectl sees it: it authenticates
// exactly like the server (Bearer token, or the HMAC trio verified with
// auth.SignRequest over method, request URI and body) and answers with the
// generated types so the JSON shapes match the contract.
func fakeTradingAPI(t *testing.T) *httptest.Server {
	t.Helper()
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	problem := func(w http.ResponseWriter, status int, title, detail string) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(apiclient.Problem{Type: "about:blank", Title: title, Status: status, Detail: detail, CorrelationID: "corr-1"})
	}
	authed := func(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Header.Get("Authorization") == "Bearer "+fakeToken:
			return body, true
		case r.Header.Get(auth.HeaderAPIKey) == fakeKeyID:
			ts := r.Header.Get(auth.HeaderAPITimestamp)
			ms, err := strconv.ParseInt(ts, 10, 64)
			if err != nil || time.Since(time.UnixMilli(ms)).Abs() > auth.MaxTimestampSkew {
				problem(w, http.StatusUnauthorized, "Unauthorized", "bad timestamp")
				return nil, false
			}
			if auth.SignRequest(fakeSecret, ts, r.Method, r.URL.RequestURI(), body) != r.Header.Get(auth.HeaderAPISignature) {
				problem(w, http.StatusUnauthorized, "Unauthorized", "bad signature")
				return nil, false
			}
			return body, true
		}
		problem(w, http.StatusUnauthorized, "Unauthorized", "authentication required")
		return nil, false
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/register", func(w http.ResponseWriter, r *http.Request) {
		var req apiclient.RegisterJSONRequestBody
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		if len(req.Password) < 8 {
			problem(w, http.StatusUnprocessableEntity, "Unprocessable Entity", "password too short")
			return
		}
		writeJSON(w, http.StatusCreated, fakeSession())
	})
	mux.HandleFunc("POST /v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var req apiclient.LoginJSONRequestBody
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		if req.Password != goodPass {
			problem(w, http.StatusUnauthorized, "Unauthorized", "invalid email or password")
			return
		}
		writeJSON(w, http.StatusOK, fakeSession())
	})
	mux.HandleFunc("POST /v1/auth/logout", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /v1/account", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authed(w, r); !ok {
			return
		}
		writeJSON(w, http.StatusOK, apiclient.Account{UserID: "u1", AccountID: "acct-1", Email: "a@example.com", Role: apiclient.AccountRole("user"), Status: apiclient.AccountStatus("active"), CreatedAt: time.Now()})
	})
	mux.HandleFunc("GET /v1/balances", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authed(w, r); !ok {
			return
		}
		writeJSON(w, http.StatusOK, apiclient.BalanceList{Balances: []apiclient.Balance{{Asset: "USDC", Available: amt("8004"), Hold: amt("1200"), Total: amt("9204")}}})
	})
	mux.HandleFunc("POST /v1/orders", func(w http.ResponseWriter, r *http.Request) {
		body, ok := authed(w, r)
		if !ok {
			return
		}
		var req apiclient.PlaceOrderJSONRequestBody
		require.NoError(t, json.Unmarshal(body, &req))
		assert.Equal(t, "ETH-USDC", req.Market)
		assert.Equal(t, apiclient.Side("buy"), req.Side)
		assert.NotEmpty(t, req.ClientOrderID, "the CLI generates a client_order_id when none is given")
		status := http.StatusCreated
		if req.ClientOrderID == "dup" {
			status = http.StatusOK // replay
		}
		writeJSON(w, status, apiclient.OrderResult{Order: fakeOrder(req.ClientOrderID, "partially_filled"), Trades: []apiclient.Fill{fakeFill()}})
	})
	mux.HandleFunc("GET /v1/orders", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authed(w, r); !ok {
			return
		}
		assert.Equal(t, "true", r.URL.Query().Get("open_only"))
		writeJSON(w, http.StatusOK, apiclient.OrderList{Orders: []apiclient.Order{fakeOrder("b1", "partially_filled")}})
	})
	mux.HandleFunc("GET /v1/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authed(w, r); !ok {
			return
		}
		writeJSON(w, http.StatusOK, apiclient.OrderResult{Order: fakeOrder("b1", "partially_filled"), Trades: []apiclient.Fill{fakeFill()}})
	})
	mux.HandleFunc("DELETE /v1/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authed(w, r); !ok {
			return
		}
		o := fakeOrder("b1", "cancelled")
		o.CancelReason = "user"
		o.HoldRemaining = amt("0")
		writeJSON(w, http.StatusOK, o)
	})
	mux.HandleFunc("GET /v1/fills", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authed(w, r); !ok {
			return
		}
		assert.Equal(t, "ETH-USDC", r.URL.Query().Get("market"))
		writeJSON(w, http.StatusOK, apiclient.FillList{Fills: []apiclient.Fill{fakeFill()}})
	})
	mux.HandleFunc("GET /v1/markets/{symbol}/depth", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, apiclient.Depth{Market: r.PathValue("symbol"), LastSeq: 7,
			Bids: []apiclient.Level{{Price: amt("2000"), Qty: amt("0.6"), Orders: 1}},
			Asks: []apiclient.Level{{Price: amt("2500"), Qty: amt("0.1"), Orders: 1}, {Price: amt("2600"), Qty: amt("1"), Orders: 2}}})
	})
	mux.HandleFunc("GET /v1/markets/{symbol}/trades", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, apiclient.TradeList{Trades: []apiclient.Trade{{TradeID: "01TRADE", Market: r.PathValue("symbol"), Price: amt("1990"), Qty: amt("0.4"), QuoteQty: amt("796"), TakerSide: apiclient.Side("buy"), Seq: 2, ExecutedAt: time.Now()}}})
	})
	mux.HandleFunc("POST /v1/api-keys", func(w http.ResponseWriter, r *http.Request) {
		body, ok := authed(w, r)
		if !ok {
			return
		}
		var req apiclient.CreateAPIKeyJSONRequestBody
		require.NoError(t, json.Unmarshal(body, &req))
		assert.Equal(t, []apiclient.Scope{"read", "trade"}, req.Scopes)
		writeJSON(w, http.StatusCreated, apiclient.CreatedAPIKey{ID: "k1", KeyID: fakeKeyID, Label: "bot", Scopes: req.Scopes, IPAllowlist: []string{}, CreatedAt: time.Now(), Secret: fakeSecret})
	})
	mux.HandleFunc("GET /v1/api-keys", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authed(w, r); !ok {
			return
		}
		writeJSON(w, http.StatusOK, apiclient.APIKeyList{APIKeys: []apiclient.APIKey{{ID: "k1", KeyID: fakeKeyID, Label: "bot", Scopes: []apiclient.Scope{"read", "trade"}, IPAllowlist: []string{"10.0.0.0/8"}, CreatedAt: time.Now()}}})
	})
	mux.HandleFunc("DELETE /v1/api-keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := authed(w, r); !ok {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestUserRegisterAndLogin(t *testing.T) {
	srv := fakeTradingAPI(t)
	out, err := run(t, "--base-url", srv.URL, "user", "register", "--email", "a@example.com", "--password", goodPass)
	require.NoError(t, err)
	assert.Contains(t, out, "USER_ID")
	assert.Contains(t, out, "export EXCHANGE_TOKEN="+fakeToken)
	assert.Contains(t, out, "export EXCHANGE_REFRESH_TOKEN=rt-1")

	out, err = run(t, "--base-url", srv.URL, "--output", "json", "user", "login", "--email", "a@example.com", "--password", goodPass)
	require.NoError(t, err)
	assert.Contains(t, out, `"access_token": "tok-access"`)

	_, err = run(t, "--base-url", srv.URL, "user", "login", "--email", "a@example.com", "--password", "wrong")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Unauthorized: invalid email or password")
	assert.Contains(t, err.Error(), "HTTP 401")
	_, err = run(t, "--base-url", srv.URL, "user", "register", "--email", "a@example.com", "--password", "short")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 422")
	_, err = run(t, "--base-url", srv.URL, "user", "register", "--email", "a@example.com")
	require.Error(t, err, "--password is required")

	out, err = run(t, "--base-url", srv.URL, "user", "logout", "--refresh-token", "rt-1")
	require.NoError(t, err)
	assert.Contains(t, out, "logged out")
}

func TestBearerCredential(t *testing.T) {
	srv := fakeTradingAPI(t)
	_, err := run(t, "--base-url", srv.URL, "user", "me")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 401")

	out, err := run(t, "--base-url", srv.URL, "--token", fakeToken, "user", "me")
	require.NoError(t, err)
	assert.Contains(t, out, "a@example.com")

	t.Setenv("EXCHANGE_TOKEN", fakeToken)
	out, err = run(t, "--base-url", srv.URL, "balances")
	require.NoError(t, err)
	assert.Contains(t, out, "USDC")
	assert.Contains(t, out, "9204")
}

func TestAPIKeyCredentialSignsRequests(t *testing.T) {
	srv := fakeTradingAPI(t)
	key := []string{"--base-url", srv.URL, "--api-key", fakeKeyID, "--api-secret", fakeSecret}
	out, err := run(t, append(key, "balances")...)
	require.NoError(t, err)
	assert.Contains(t, out, "8004")
	// the query string is part of the signed request URI
	out, err = run(t, append(key, "orders", "list", "--open", "--limit", "5")...)
	require.NoError(t, err)
	assert.Contains(t, out, "01ORDER")
	// the body is part of the signature
	out, err = run(t, append(key, "orders", "place", "--side", "buy", "--price", "2000", "--qty", "1", "--client-order-id", "b1")...)
	require.NoError(t, err)
	assert.Contains(t, out, "partially_filled")

	_, err = run(t, "--base-url", srv.URL, "--api-key", fakeKeyID, "--api-secret", "ff", "balances")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bad signature")
	_, err = run(t, "--base-url", srv.URL, "--api-key", fakeKeyID, "balances")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be given together")

	t.Setenv("EXCHANGE_API_KEY", fakeKeyID)
	t.Setenv("EXCHANGE_API_SECRET", fakeSecret)
	out, err = run(t, "--base-url", srv.URL, "user", "me")
	require.NoError(t, err)
	assert.Contains(t, out, "acct-1")
}

func TestOrdersCommands(t *testing.T) {
	srv := fakeTradingAPI(t)
	tok := []string{"--base-url", srv.URL, "--token", fakeToken}
	out, err := run(t, append(tok, "orders", "place", "--side", "buy", "--price", "2000", "--qty", "1", "--client-order-id", "b1")...)
	require.NoError(t, err)
	assert.Contains(t, out, "01ORDER")
	assert.Contains(t, out, "partially_filled")
	assert.Contains(t, out, "1200 USDC")
	assert.Contains(t, out, "TRADE_ID", "fills are printed under the order")
	assert.Contains(t, out, "0.0008 ETH")

	out, err = run(t, append(tok, "orders", "place", "--side", "buy", "--price", "2000", "--qty", "1", "--client-order-id", "dup")...)
	require.NoError(t, err, "a 200 replay is a success")
	assert.Contains(t, out, "01ORDER")
	out, err = run(t, append(tok, "--output", "json", "orders", "place", "--side", "buy", "--price", "2000", "--qty", "1")...)
	require.NoError(t, err)
	assert.Contains(t, out, `"client_order_id": "ctl-`, "generated idempotency key")

	_, err = run(t, append(tok, "orders", "place", "--side", "buy", "--price", "abc", "--qty", "1")...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--price")
	_, err = run(t, append(tok, "orders", "place", "--price", "1", "--qty", "1")...)
	require.Error(t, err, "--side is required")

	out, err = run(t, append(tok, "--output", "json", "orders", "cancel", "01ORDER")...)
	require.NoError(t, err)
	assert.Contains(t, out, `"status": "cancelled"`)
	out, err = run(t, append(tok, "orders", "get", "01ORDER")...)
	require.NoError(t, err)
	assert.Contains(t, out, "CLIENT_ID")
	assert.Contains(t, out, "01TRADE")
	out, err = run(t, append(tok, "fills", "--market", "ETH-USDC")...)
	require.NoError(t, err)
	assert.Contains(t, out, "taker")
	assert.Contains(t, out, "1990")
}

func TestBookAndTrades(t *testing.T) {
	srv := fakeTradingAPI(t)
	out, err := run(t, "--base-url", srv.URL, "book", "ETH-USDC", "--limit", "5")
	require.NoError(t, err)
	assert.Contains(t, out, "BID_PRICE")
	assert.Contains(t, out, "2000")
	assert.Contains(t, out, "2600")
	assert.Contains(t, out, "seq")
	out, err = run(t, "--base-url", srv.URL, "trades", "ETH-USDC")
	require.NoError(t, err)
	assert.Contains(t, out, "1990")
	assert.Contains(t, out, "01TRADE")
}

func TestAPIKeysCommands(t *testing.T) {
	srv := fakeTradingAPI(t)
	tok := []string{"--base-url", srv.URL, "--token", fakeToken}
	out, err := run(t, append(tok, "api-keys", "create", "--scopes", "read, trade", "--label", "bot")...)
	require.NoError(t, err)
	assert.Contains(t, out, "export EXCHANGE_API_KEY="+fakeKeyID)
	assert.Contains(t, out, "export EXCHANGE_API_SECRET="+fakeSecret)
	out, err = run(t, append(tok, "api-keys", "list")...)
	require.NoError(t, err)
	assert.Contains(t, out, "read,trade")
	assert.Contains(t, out, "10.0.0.0/8")
	out, err = run(t, append(tok, "api-keys", "revoke", "k1")...)
	require.NoError(t, err)
	assert.Contains(t, out, "revoked k1")
	_, err = run(t, "--base-url", srv.URL, "api-keys", "list")
	require.Error(t, err, "no credential")
}
