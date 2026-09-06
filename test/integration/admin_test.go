//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/admin"
	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

const adminKey = "it-admin-key"

func adminServer(t *testing.T, h ledgerHarness) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Use(telemetry.CorrelationMiddleware(slog.New(slog.NewTextHandler(io.Discard, nil))))
	r.Use(admin.RequireAPIKey(adminKey))
	admin.Mount(r, admin.NewHandler(h.all, h.svc, registry.NewStore(h.all), audit.NewRecorder("default"), "default"))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

type adminResp struct {
	status int
	body   []byte
	hdr    http.Header
}

func call(t *testing.T, srv *httptest.Server, method, path, key string, body any) adminResp {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, rd)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set(admin.APIKeyHeader, key)
	}
	req.Header.Set(telemetry.RequestIDHeader, "it-admin")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return adminResp{status: resp.StatusCode, body: raw, hdr: resp.Header}
}

func decode[T any](t *testing.T, r adminResp) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(r.body, &v), string(r.body))
	return v
}

func TestAdminAPI(t *testing.T) {
	h := setupLedger(t)
	srv := adminServer(t, h)
	ctx := context.Background()

	t.Run("api key is required", func(t *testing.T) {
		r := call(t, srv, http.MethodGet, "/admin/v1/ledger/trial-balance", "", nil)
		assert.Equal(t, http.StatusUnauthorized, r.status)
		assert.Equal(t, admin.ProblemContentType, r.hdr.Get("Content-Type"))
		p := decode[gen.Problem](t, r)
		assert.Equal(t, "it-admin", p.CorrelationID)
		r = call(t, srv, http.MethodGet, "/admin/v1/ledger/trial-balance", "wrong", nil)
		assert.Equal(t, http.StatusUnauthorized, r.status)
	})

	var accountID string
	t.Run("create account", func(t *testing.T) {
		r := call(t, srv, http.MethodPost, "/admin/v1/accounts", adminKey, map[string]any{})
		require.Equal(t, http.StatusCreated, r.status, string(r.body))
		a := decode[gen.Account](t, r)
		assert.Equal(t, gen.AccountKindSpot, a.Kind)
		assert.Equal(t, gen.AccountStatusActive, a.Status)
		assert.Nil(t, a.HouseCode)
		accountID = a.ID

		r = call(t, srv, http.MethodGet, "/admin/v1/accounts/"+accountID, adminKey, nil)
		assert.Equal(t, http.StatusOK, r.status)
		r = call(t, srv, http.MethodGet, "/admin/v1/accounts/00000000-0000-0000-0000-000000000000", adminKey, nil)
		assert.Equal(t, http.StatusNotFound, r.status)
		r = call(t, srv, http.MethodGet, "/admin/v1/accounts?kind=house", adminKey, nil)
		require.Equal(t, http.StatusOK, r.status)
		list := decode[gen.AccountList](t, r)
		assert.Len(t, list.Accounts, 6)
	})

	t.Run("fund, replay, balances", func(t *testing.T) {
		body := map[string]any{"account_id": accountID, "asset": "USDC", "amount": "10000", "direction": "credit", "reason": "dev faucet", "idempotency_key": "adjust:it-1"}
		r := call(t, srv, http.MethodPost, "/admin/v1/ledger/adjustments", adminKey, body)
		require.Equal(t, http.StatusCreated, r.status, string(r.body))
		e := decode[gen.JournalEntry](t, r)
		assert.Equal(t, "adjustment", e.Kind)
		assert.Equal(t, "dev faucet", e.Reason)
		assert.Len(t, e.Postings, 2)
		assert.Contains(t, string(r.body), `"amount":"10000"`)

		r = call(t, srv, http.MethodPost, "/admin/v1/ledger/adjustments", adminKey, body)
		assert.Equal(t, http.StatusOK, r.status, "idempotent replay")
		e2 := decode[gen.JournalEntry](t, r)
		assert.Equal(t, e.ID, e2.ID)

		r = call(t, srv, http.MethodGet, "/admin/v1/accounts/"+accountID+"/balances", adminKey, nil)
		require.Equal(t, http.StatusOK, r.status)
		bl := decode[gen.BalanceList](t, r)
		require.Len(t, bl.Balances, 1)
		assert.Equal(t, "USDC", bl.Balances[0].Asset)
		assert.Equal(t, "10000", bl.Balances[0].Available.String())
		assert.Equal(t, "10000", bl.Balances[0].Total.String())

		// a JSON number for the amount is rejected by money.Amount → 400 problem
		r = call(t, srv, http.MethodPost, "/admin/v1/ledger/adjustments", adminKey, map[string]any{"account_id": accountID, "asset": "USDC", "amount": 5, "direction": "credit", "reason": "x"})
		assert.Equal(t, http.StatusBadRequest, r.status, string(r.body))
		// missing reason
		r = call(t, srv, http.MethodPost, "/admin/v1/ledger/adjustments", adminKey, map[string]any{"account_id": accountID, "asset": "USDC", "amount": "5", "direction": "credit", "reason": ""})
		assert.Equal(t, http.StatusBadRequest, r.status)
		// unknown account
		r = call(t, srv, http.MethodPost, "/admin/v1/ledger/adjustments", adminKey, map[string]any{"account_id": "00000000-0000-0000-0000-000000000000", "asset": "USDC", "amount": "5", "direction": "credit", "reason": "x"})
		assert.Equal(t, http.StatusNotFound, r.status)
		// debit beyond available → 422
		r = call(t, srv, http.MethodPost, "/admin/v1/ledger/adjustments", adminKey, map[string]any{"account_id": accountID, "asset": "USDC", "amount": "10000.000001", "direction": "debit", "reason": "clawback"})
		assert.Equal(t, http.StatusUnprocessableEntity, r.status, string(r.body))
		p := decode[gen.Problem](t, r)
		assert.Equal(t, "it-admin", p.CorrelationID)
	})

	t.Run("trial balance and entries", func(t *testing.T) {
		r := call(t, srv, http.MethodGet, "/admin/v1/ledger/trial-balance", adminKey, nil)
		require.Equal(t, http.StatusOK, r.status)
		tb := decode[gen.TrialBalance](t, r)
		assert.True(t, tb.Balanced)
		require.Len(t, tb.Lines, 1)
		assert.Equal(t, "0", tb.Lines[0].Diff.String())
		require.Len(t, tb.House, 1)
		assert.Equal(t, gen.HouseCodeExternal, tb.House[0].Code)
		assert.Equal(t, "-10000", tb.House[0].Balance.String())

		r = call(t, srv, http.MethodGet, "/admin/v1/ledger/entries?account_id="+accountID, adminKey, nil)
		require.Equal(t, http.StatusOK, r.status)
		el := decode[gen.JournalEntryList](t, r)
		require.Len(t, el.Entries, 1)
		assert.Equal(t, "adjust:it-1", el.Entries[0].IdempotencyKey)
		r = call(t, srv, http.MethodGet, "/admin/v1/ledger/entries?ref_type=adjustment&limit=10", adminKey, nil)
		require.Equal(t, http.StatusOK, r.status)
		r = call(t, srv, http.MethodGet, "/admin/v1/ledger/entries?limit=0", adminKey, nil)
		assert.Equal(t, http.StatusOK, r.status, "out-of-range limit falls back to the default")
	})

	t.Run("status change and audit trail", func(t *testing.T) {
		r := call(t, srv, http.MethodPut, "/admin/v1/accounts/"+accountID+"/status", adminKey, map[string]any{"status": "frozen", "reason": "suspicious"})
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		a := decode[gen.Account](t, r)
		assert.Equal(t, gen.AccountStatusFrozen, a.Status)
		r = call(t, srv, http.MethodPut, "/admin/v1/accounts/"+accountID+"/status", adminKey, map[string]any{"status": "frozen"})
		assert.Equal(t, http.StatusBadRequest, r.status, "reason is required")
		r = call(t, srv, http.MethodPut, "/admin/v1/accounts/00000000-0000-0000-0000-000000000000/status", adminKey, map[string]any{"status": "frozen", "reason": "x"})
		assert.Equal(t, http.StatusNotFound, r.status)

		r = call(t, srv, http.MethodGet, "/admin/v1/audit-events", adminKey, nil)
		require.Equal(t, http.StatusOK, r.status)
		ev := decode[gen.AuditEventList](t, r)
		actions := map[string]int{}
		for _, e := range ev.Events {
			actions[e.Action]++
			assert.Equal(t, gen.AuditEventActorTypeAPIKey, e.ActorType)
			assert.Equal(t, "admin-api-key", e.ActorID)
			assert.Equal(t, "it-admin", e.CorrelationID)
		}
		assert.Equal(t, 1, actions["account.create"])
		assert.Equal(t, 1, actions["ledger.adjustment.create"], "the replay must not write a second audit event")
		assert.Equal(t, 1, actions["account.status.update"])
		r = call(t, srv, http.MethodGet, "/admin/v1/audit-events?action=account.status.update", adminKey, nil)
		ev = decode[gen.AuditEventList](t, r)
		require.Len(t, ev.Events, 1)
		after, ok := ev.Events[0].After.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "frozen", after["status"])
		assert.Equal(t, "suspicious", after["reason"])
	})

	h.assertTrialBalanceZero(t, ctx)
	h.assertCacheMatchesPostings(t, ctx, accountID)
}
