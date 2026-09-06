package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakeAdmin(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Admin-Api-Key") != "secret" {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"type":"about:blank","title":"Unauthorized","status":401,"detail":"missing or invalid X-Admin-Api-Key","correlation_id":"c-401"}`))
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("POST /admin/v1/ledger/adjustments", auth(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		require.NoError(t, json.Unmarshal(body, &req))
		assert.Equal(t, "10000", req["amount"], "amount travels as a string")
		assert.Equal(t, "credit", req["direction"])
		w.Header().Set("Content-Type", "application/json")
		status := http.StatusCreated
		if req["idempotency_key"] == "again" {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"id":7,"idempotency_key":"adjust:x","kind":"adjustment","reason":"dev faucet","created_at":"2026-09-06T00:00:00Z","postings":[{"account_id":"ext","asset":"USDC","bucket":"house","direction":"debit","amount":"10000"},{"account_id":"acc","asset":"USDC","bucket":"available","direction":"credit","amount":"10000"}]}`))
	}))
	mux.HandleFunc("GET /admin/v1/ledger/trial-balance", auth(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"balanced":true,"lines":[{"asset":"USDC","debits":"10000","credits":"10000","diff":"0"}],"house":[{"code":"external","type":"external","asset":"USDC","balance":"-10000"}]}`))
	}))
	mux.HandleFunc("GET /admin/v1/accounts/{id}/balances", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "missing" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"about:blank","title":"Not Found","status":404,"detail":"account missing does not exist","correlation_id":"c-404"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"account_id":"acc","balances":[{"asset":"USDC","available":"9204","hold":"0","total":"9204"}]}`))
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAdminFundAndReplay(t *testing.T) {
	srv := fakeAdmin(t)
	out, err := run(t, "admin", "--admin-url", srv.URL, "--admin-key", "secret", "fund", "--account", "acc", "--asset", "USDC", "--amount", "10000")
	require.NoError(t, err)
	assert.Contains(t, out, "posted entry 7")
	assert.Contains(t, out, "available")
	out, err = run(t, "admin", "--admin-url", srv.URL, "--admin-key", "secret", "fund", "--account", "acc", "--asset", "USDC", "--amount", "10000", "--key", "again")
	require.NoError(t, err)
	assert.Contains(t, out, "already posted (replay)")
	_, err = run(t, "admin", "--admin-url", srv.URL, "--admin-key", "secret", "fund", "--account", "acc", "--asset", "USDC", "--amount", "-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "positive decimal")
}

func TestAdminTrialBalanceAndBalances(t *testing.T) {
	srv := fakeAdmin(t)
	out, err := run(t, "admin", "--admin-url", srv.URL, "--admin-key", "secret", "trial-balance")
	require.NoError(t, err)
	assert.Contains(t, out, "balanced: true")
	assert.Contains(t, out, "external")
	assert.Contains(t, out, "-10000")
	out, err = run(t, "--output", "json", "admin", "--admin-url", srv.URL, "--admin-key", "secret", "balances", "acc")
	require.NoError(t, err)
	assert.Contains(t, out, `"available": "9204"`)
	_, err = run(t, "admin", "--admin-url", srv.URL, "--admin-key", "secret", "balances", "missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Not Found: account missing does not exist")
	assert.Contains(t, err.Error(), "correlation_id=c-404")
}

func TestAdminAuthErrors(t *testing.T) {
	srv := fakeAdmin(t)
	_, err := run(t, "admin", "--admin-url", srv.URL, "--admin-key", "wrong", "trial-balance")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Unauthorized")
	assert.Contains(t, err.Error(), "HTTP 401")
	t.Setenv("EXCHANGE_ADMIN_API_KEY", "")
	_, err = run(t, "admin", "--admin-url", srv.URL, "trial-balance")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "admin API key required")
}
