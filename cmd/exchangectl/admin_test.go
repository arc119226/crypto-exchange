package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	mux.HandleFunc("GET /admin/v1/reconciliation", auth(func(w http.ResponseWriter, _ *http.Request) {
		// Deliberately a while ago: this endpoint serves the most recent
		// stored pass, so anything it returns is history.
		finished := time.Now().UTC().Add(-4 * time.Minute)
		started := finished.Add(-3 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"r-1","chain_id":31337,"started_at":%q,"finished_at":%q,"balanced":true,`+
			`"lines":[{"asset":"ETH","ledger_total":"2","chain_total":"2","uncredited":"0",`+
			`"above_frontier":"0","in_flight":"0","diff":"0","balanced":true}]}`,
			started.Format(time.RFC3339Nano), finished.Format(time.RFC3339Nano))
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The reconciliation table is a recording, never a live read: the admin role
// has no node, so it serves the most recent stored pass. During the Sepolia
// walkthrough that was read as live twice -- an adjustment was made, the table
// still showed the state before it, and the operator concluded the adjustment
// had failed. The table has to say when it was taken.
func TestAdminReconcileSaysWhenThePassRan(t *testing.T) {
	srv := fakeAdmin(t)
	out, err := run(t, "admin", "--admin-url", srv.URL, "--admin-key", "secret", "reconcile")
	require.NoError(t, err)

	first := strings.SplitN(out, "\n", 2)[0]
	assert.Contains(t, first, "pass ran:", "before the numbers, not after them")
	assert.Contains(t, first, "4m ago", "an absolute time still asks the reader to do arithmetic")
	assert.Regexp(t, `\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`, first, "and the absolute time as well, for the record")
	assert.Contains(t, out, "ASSET", "the table still follows")

	// --output json is what a script reads; it must not gain a header line.
	out, err = run(t, "--output", "json", "admin", "--admin-url", srv.URL, "--admin-key", "secret", "reconcile")
	require.NoError(t, err)
	assert.NotContains(t, out, "pass ran:")
	assert.True(t, strings.HasPrefix(strings.TrimSpace(out), "{"), out)
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
