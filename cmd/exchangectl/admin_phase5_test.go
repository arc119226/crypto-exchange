package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Phase 5 verbs against a fake admin API: what each sends, and what it
// prints. The real endpoints are covered by the integration tests; this is
// the CLI's half of the contract.
func fakeAdminPhase5(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	jsonOK := func(w http.ResponseWriter, body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
	user := `{"id":"u-1","email":"alice@example.com","role":"user","kyc_level":%d,"status":"%s","totp_enabled":false,"version":2,"created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-08T00:00:00Z"}`
	mux.HandleFunc("GET /admin/v1/users", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "example", r.URL.Query().Get("email"))
		assert.Equal(t, "frozen", r.URL.Query().Get("status"))
		jsonOK(w, `{"users":[`+strings.Replace(strings.Replace(user, "%d", "1", 1), "%s", "frozen", 1)+`]}`)
	})
	mux.HandleFunc("PUT /admin/v1/users/{id}/status", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "frozen", req["status"])
		assert.Equal(t, "chargeback", req["reason"])
		jsonOK(w, strings.Replace(strings.Replace(user, "%d", "1", 1), "%s", "frozen", 1))
	})
	mux.HandleFunc("PUT /admin/v1/users/{id}/kyc-level", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, float64(2), req["kyc_level"])
		jsonOK(w, strings.Replace(strings.Replace(user, "%d", "2", 1), "%s", "active", 1))
	})
	mux.HandleFunc("PUT /admin/v1/withdrawal-limits/{asset}/{level}", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		assert.Contains(t, string(body), `"auto_approve_limit":"2"`, "amounts travel as strings")
		assert.Contains(t, string(body), `"require_manual_review":true`)
		assert.Equal(t, "ETH", r.PathValue("asset"))
		assert.Equal(t, "1", r.PathValue("level"))
		jsonOK(w, `{"asset":"ETH","kyc_level":1,"auto_approve_limit":"2","daily_limit":"20","require_manual_review":true,"version":3,"updated_at":"2026-09-08T00:00:00Z"}`)
	})
	mux.HandleFunc("POST /admin/v1/assets", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "ARB", req["symbol"])
		assert.Equal(t, "listing", req["reason"], "the reason comes from the flag, not the file")
		assert.Equal(t, "50", req["sweep_threshold"])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"a-1","symbol":"ARB","name":"Arbitrum","chain_id":31337,"contract_address":"0x912CE59144191C1204E64559FE8253a0e49E6548","is_native":false,"scale":18,"display_scale":4,"required_confirmations":12,"min_deposit":"0","min_withdrawal":"1","withdrawal_fee":"0.1","sweep_threshold":"50","deposit_enabled":true,"withdraw_enabled":true,"status":"active","version":1,"created_at":"2026-09-08T00:00:00Z","updated_at":"2026-09-08T00:00:00Z"}`))
	})
	mux.HandleFunc("GET /admin/v1/hot-wallet", func(w http.ResponseWriter, _ *http.Request) {
		jsonOK(w, `{"chain_id":31337,"address":"0xhot","next_nonce":9,"low":true,"low_alerted_at":"2026-09-08T03:00:00Z","balances":[{"asset":"ETH","balance":"1.5"}],"updated_at":"2026-09-08T03:00:00Z"}`)
	})
	mux.HandleFunc("GET /admin/v1/reconciliation/breaks", func(w http.ResponseWriter, _ *http.Request) {
		jsonOK(w, `{"chain_breaks":[{"id":"b-1","report_id":"r-1","chain_id":31337,"asset":"ETH","block_height":10,"ledger_total":"1","chain_total":"0.5","uncredited":"0","above_frontier":"0","in_flight":"0","diff":"-0.5","detected_at":"2026-09-08T03:00:00Z"}],`+
			`"ledger_breaks":[{"id":"lb-1","asset":"USDC","debits":"10","credits":"9","diff":"1","detected_at":"2026-09-08T03:00:00Z"}]}`)
	})
	mux.HandleFunc("POST /admin/v1/engine/reload", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "seeded", req["reason"])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"event_id":"01J8Z2K3M4N5P6Q7R8S9T0V1Y5","occurred_at":"2026-09-08T03:00:00Z"}`))
	})
	mux.HandleFunc("POST /admin/v1/webhooks/{id}/rotate-secret", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, float64(48), req["grace_hours"])
		jsonOK(w, `{"id":"`+r.PathValue("id")+`","secret":"deadbeef","previous_secret_until":"2026-09-10T03:00:00Z"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAdminUsersVerbs(t *testing.T) {
	srv := fakeAdminPhase5(t)
	base := []string{"admin", "--admin-url", srv.URL, "--admin-key", "secret"}
	out, err := run(t, append(base, "users", "list", "--email", "example", "--status", "frozen")...)
	require.NoError(t, err)
	assert.Contains(t, out, "alice@example.com")
	assert.Contains(t, out, "frozen")
	out, err = run(t, append(base, "users", "freeze", "u-1", "--reason", "chargeback")...)
	require.NoError(t, err)
	assert.Contains(t, out, "frozen")
	out, err = run(t, append(base, "users", "set-kyc", "u-1", "2", "--reason", "documents")...)
	require.NoError(t, err)
	assert.Regexp(t, `u-1\s+alice@example.com\s+user\s+2\s+active`, out)
	_, err = run(t, append(base, "users", "set-kyc", "u-1", "two", "--reason", "x")...)
	require.Error(t, err)
	_, err = run(t, append(base, "users", "freeze", "u-1")...)
	require.Error(t, err, "a reason is required")
}

func TestAdminRegistryVerbs(t *testing.T) {
	srv := fakeAdminPhase5(t)
	base := []string{"admin", "--admin-url", srv.URL, "--admin-key", "secret"}
	out, err := run(t, append(base, "withdrawal-limits", "set", "ETH", "1", "--auto-approve", "2", "--daily", "20", "--review-all", "--reason", "raise")...)
	require.NoError(t, err)
	assert.Regexp(t, `ETH\s+1\s+2\s+20\s+always`, out)
	_, err = run(t, append(base, "withdrawal-limits", "set", "ETH", "1", "--auto-approve", "lots", "--daily", "20", "--reason", "x")...)
	require.Error(t, err)

	file := filepath.Join(t.TempDir(), "arb.json")
	require.NoError(t, os.WriteFile(file, []byte(`{"symbol":"ARB","name":"Arbitrum","chain_id":31337,"contract_address":"0x912CE59144191C1204E64559FE8253a0e49E6548","is_native":false,"scale":18,"display_scale":4,"required_confirmations":12,"min_deposit":"0","min_withdrawal":"1","withdrawal_fee":"0.1","sweep_threshold":"50","deposit_enabled":true,"withdraw_enabled":true,"status":"active","reason":"ignored"}`), 0o600))
	out, err = run(t, append(base, "assets", "create", "--file", file, "--reason", "listing")...)
	require.NoError(t, err)
	assert.Contains(t, out, "ARB")
	assert.Contains(t, out, "0x912CE59144191C1204E64559FE8253a0e49E6548")
	_, err = run(t, append(base, "assets", "create", "--file", filepath.Join(t.TempDir(), "missing.json"), "--reason", "x")...)
	require.Error(t, err)

	out, err = run(t, append(base, "reload", "--reason", "seeded")...)
	require.NoError(t, err)
	assert.Contains(t, out, "reload requested: event 01J8Z2K3M4N5P6Q7R8S9T0V1Y5")
}

func TestAdminChainVerbs(t *testing.T) {
	srv := fakeAdminPhase5(t)
	base := []string{"admin", "--admin-url", srv.URL, "--admin-key", "secret"}
	out, err := run(t, append(base, "hot-wallet")...)
	require.NoError(t, err)
	assert.Contains(t, out, "address 0xhot")
	assert.Contains(t, out, "LOW since 2026-09-08T03:00:00Z")
	assert.Regexp(t, `ETH\s+1.5`, out)

	out, err = run(t, append(base, "breaks")...)
	require.NoError(t, err)
	assert.Contains(t, out, "chain breaks")
	assert.Regexp(t, `ETH\s+10\s+1\s+0.5\s+-0.5\s+r-1`, out)
	assert.Contains(t, out, "ledger breaks")
	assert.Regexp(t, `USDC\s+10\s+9\s+1\s+open`, out)

	out, err = run(t, append(base, "webhooks", "rotate-secret", "ep-1", "--grace-hours", "48", "--reason", "quarterly")...)
	require.NoError(t, err)
	assert.Contains(t, out, "secret   deadbeef")
	assert.Contains(t, out, "also signs until 2026-09-10T03:00:00Z")
	_, err = run(t, append(base, "webhooks", "rotate-secret", "ep-1")...)
	require.Error(t, err, "a reason is required")
}
