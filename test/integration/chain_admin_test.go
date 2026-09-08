//go:build integration

package integration

import (
	"context"
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
	"github.com/arc119226/crypto-exchange/internal/chain/deposit"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// What the back office reads about the chain and the audit trail, over the
// API. The rows come in by SQL: the chain role that would write them needs
// a node, and these endpoints only show what it wrote.
func TestAdminChainAndAuditReads(t *testing.T) {
	ctx := context.Background()
	h := setupLedger(t)
	handler := admin.NewHandler(h.all, h.svc, registry.NewStore(h.all), audit.NewRecorder("default"), "default").
		WithChainID(anvilChainID).WithDeposits(deposit.NewReader(h.all, "default"))
	r := chi.NewRouter()
	r.Use(telemetry.CorrelationMiddleware(slog.New(slog.NewTextHandler(io.Discard, nil))))
	admin.Routes(r, handler, nil, adminKey)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	get := func(path string) adminResp { return call(t, srv, http.MethodGet, path, adminKey, nil) }

	t.Run("the hot wallet, before and after the chain role records it", func(t *testing.T) {
		assert.Equal(t, http.StatusNotFound, get("/admin/v1/hot-wallet").status)
		_, err := h.all.Exec(ctx, `INSERT INTO chain.hot_wallets (tenant_id, chain_id, address, next_nonce, low_alerted_at)
			VALUES ('default', $1, '0x00000000000000000000000000000000000000aa', 7, now())`, anvilChainID)
		require.NoError(t, err)
		r := get("/admin/v1/hot-wallet")
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		hw := decode[gen.HotWallet](t, r)
		assert.Equal(t, "0x00000000000000000000000000000000000000aa", hw.Address)
		assert.Equal(t, int64(7), hw.NextNonce)
		assert.True(t, hw.Low)
		assert.NotNil(t, hw.LowAlertedAt)
		assert.NotNil(t, hw.Balances, "an array, even when nothing is booked to custody_hot")
	})

	t.Run("deposits, empty and filtered", func(t *testing.T) {
		r := get("/admin/v1/deposits")
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		assert.Empty(t, decode[gen.AdminDepositList](t, r).Deposits)
		r = get("/admin/v1/deposits?status=credited&asset=ETH&limit=5")
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		assert.Empty(t, decode[gen.AdminDepositList](t, r).Deposits)
	})

	t.Run("reports and breaks of both kinds", func(t *testing.T) {
		r := get("/admin/v1/reconciliation/reports")
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		assert.Empty(t, decode[gen.ReconciliationReportList](t, r).Reports)

		var reportID string
		require.NoError(t, h.all.QueryRow(ctx, `INSERT INTO admin.reconciliation_reports (tenant_id, chain_id, started_at, finished_at, balanced, lines)
			VALUES ('default', $1, now() - interval '1 second', now(), false,
			'[{"asset":"ETH","block_height":10,"ledger_total":"1","chain_total":"0.5","uncredited":"0","above_frontier":"0","in_flight":"0","diff":"-0.5"}]')
			RETURNING id`, anvilChainID).Scan(&reportID))
		_, err := h.all.Exec(ctx, `INSERT INTO admin.reconciliation_breaks (tenant_id, report_id, chain_id, asset, block_height, ledger_total, chain_total, uncredited, above_frontier, in_flight, diff)
			VALUES ('default', $1, $2, 'ETH', 10, 1, 0.5, 0, 0, 0, -0.5)`, reportID, anvilChainID)
		require.NoError(t, err)
		_, err = h.all.Exec(ctx, `INSERT INTO admin.ledger_breaks (tenant_id, asset, debits, credits, diff) VALUES ('default', 'USDC', 10, 9, 1)`)
		require.NoError(t, err)

		r = get("/admin/v1/reconciliation/reports?limit=10")
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		reports := decode[gen.ReconciliationReportList](t, r).Reports
		require.Len(t, reports, 1)
		assert.Equal(t, reportID, reports[0].ID)
		assert.False(t, reports[0].Balanced)
		require.Len(t, reports[0].Lines, 1)
		assert.Equal(t, "-0.5", reports[0].Lines[0].Diff.String())
		assert.False(t, reports[0].Lines[0].Balanced)

		r = get("/admin/v1/reconciliation/breaks")
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		breaks := decode[gen.ReconciliationBreaks](t, r)
		require.Len(t, breaks.ChainBreaks, 1)
		assert.Equal(t, reportID, breaks.ChainBreaks[0].ReportID)
		assert.Equal(t, "-0.5", breaks.ChainBreaks[0].Diff.String())
		assert.Equal(t, int64(10), breaks.ChainBreaks[0].BlockHeight)
		require.Len(t, breaks.LedgerBreaks, 1)
		assert.Equal(t, "USDC", breaks.LedgerBreaks[0].Asset)
		assert.Equal(t, "1", breaks.LedgerBreaks[0].Diff.String())
		assert.Nil(t, breaks.LedgerBreaks[0].ResolvedAt)
		r = get("/admin/v1/reconciliation")
		require.Equal(t, http.StatusOK, r.status)
		assert.Equal(t, reportID, decode[gen.ReconciliationReport](t, r).ID, "the latest is the one just written")
	})

	t.Run("the audit trail filtered by who", func(t *testing.T) {
		r := call(t, srv, http.MethodPost, "/admin/v1/accounts", adminKey, map[string]any{})
		require.Equal(t, http.StatusCreated, r.status, string(r.body))
		r = get("/admin/v1/audit-events?actor_type=api_key")
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		byKey := decode[gen.AuditEventList](t, r).Events
		require.NotEmpty(t, byKey)
		for _, e := range byKey {
			assert.Equal(t, gen.AuditEventActorType("api_key"), e.ActorType)
		}
		r = get("/admin/v1/audit-events?actor_type=admin")
		assert.Empty(t, decode[gen.AuditEventList](t, r).Events, "no person has done anything")
		r = get("/admin/v1/audit-events?actor_id=admin-api-key")
		assert.Len(t, decode[gen.AuditEventList](t, r).Events, len(byKey))
		r = get("/admin/v1/audit-events?actor_id=nobody")
		assert.Empty(t, decode[gen.AuditEventList](t, r).Events)
	})
}
