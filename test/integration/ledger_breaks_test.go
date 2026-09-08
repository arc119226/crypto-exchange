//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/admin"
	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// docs/plan-v1.0.md §12 DoD: plant a wrong entry and a break is recorded.
//
// The ledger's own constraint trigger refuses an unbalanced entry, which is
// the point of it, so the planting is done as the superuser with triggers
// off -- the one way a bad row can exist, and the situation the watcher is
// for: something outside the code path having written to the books.
func TestPlantedPostingOpensALedgerBreak(t *testing.T) {
	ctx := context.Background()
	h := setupLedger(t)
	handler := admin.NewHandler(h.all, h.svc, registry.NewStore(h.all), audit.NewRecorder("default"), "default")
	external, err := h.svc.HouseAccount(ledger.HouseExternal)
	require.NoError(t, err)

	super, err := pgx.Connect(ctx, h.DSN("exchange"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = super.Close(ctx) })
	_, err = super.Exec(ctx, `SET session_replication_role = replica`)
	require.NoError(t, err, "triggers off: the balance check must not stop the plant")
	plant := func(key, direction, amount string) {
		t.Helper()
		var id int64
		require.NoError(t, super.QueryRow(ctx,
			`INSERT INTO ledger.journal_entries (tenant_id, idempotency_key, kind, reason) VALUES ('default', $1, 'adjustment', 'planted by the test') RETURNING id`, key).Scan(&id))
		_, err := super.Exec(ctx, `INSERT INTO ledger.postings (entry_id, account_id, asset, bucket, direction, amount) VALUES ($1, $2, 'ETH', 'house', $3, $4)`,
			id, external, direction, amount)
		require.NoError(t, err)
	}
	status := func() gen.SystemStatus {
		t.Helper()
		resp, err := handler.GetSystemStatus(ctx, gen.GetSystemStatusRequestObject{})
		require.NoError(t, err)
		return gen.SystemStatus(resp.(gen.GetSystemStatus200JSONResponse))
	}
	breaks := func() []gen.LedgerBreak {
		t.Helper()
		resp, err := handler.ListReconciliationBreaks(ctx, gen.ListReconciliationBreaksRequestObject{})
		require.NoError(t, err)
		return gen.ReconciliationBreaks(resp.(gen.ListReconciliationBreaks200JSONResponse)).LedgerBreaks
	}

	opened, resolved, err := handler.WatchLedgerBreaks(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, opened+resolved, "a fresh ledger balances")
	assert.True(t, status().LedgerBalanced)

	plant("plant-1", "debit", "1.5")
	opened, resolved, err = handler.WatchLedgerBreaks(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, opened)
	assert.Equal(t, 0, resolved)
	st := status()
	assert.False(t, st.LedgerBalanced)
	assert.Equal(t, 1, st.OpenLedgerBreaks)
	got := breaks()
	require.Len(t, got, 1)
	assert.Equal(t, "ETH", got[0].Asset)
	assert.Equal(t, "1.5", got[0].Diff.String())
	assert.Nil(t, got[0].ResolvedAt)
	assert.Equal(t, []string{"reconciliation.ledger_break_detected"}, outboxTypes(t, h.all, "reconciliation.ledger"))
	var diff, breakID string
	require.NoError(t, h.all.QueryRow(ctx, `SELECT payload->>'diff', payload->>'break_id' FROM eventbus.outbox WHERE event_type = 'reconciliation.ledger_break_detected'`).Scan(&diff, &breakID))
	assert.Equal(t, "1.5", diff)
	assert.Equal(t, got[0].ID, breakID, "the event names the row")

	opened, resolved, err = handler.WatchLedgerBreaks(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, opened+resolved, "still out by the same amount: nothing new")
	assert.Len(t, outboxTypes(t, h.all, "reconciliation.ledger"), 1, "edge-triggered: no second announcement")

	plant("plant-2", "debit", "1")
	opened, resolved, err = handler.WatchLedgerBreaks(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, opened, "a different size is a new break")
	assert.Equal(t, 1, resolved, "and the old size is history")
	assert.Equal(t, 1, status().OpenLedgerBreaks)
	got = breaks()
	require.Len(t, got, 2)
	assert.Equal(t, "2.5", got[0].Diff.String(), "newest first")
	assert.Nil(t, got[0].ResolvedAt)
	assert.NotNil(t, got[1].ResolvedAt)
	assert.Len(t, outboxTypes(t, h.all, "reconciliation.ledger"), 2)

	plant("plant-3", "credit", "2.5")
	opened, resolved, err = handler.WatchLedgerBreaks(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, opened)
	assert.Equal(t, 1, resolved, "the books agree again")
	st = status()
	assert.True(t, st.LedgerBalanced)
	assert.Equal(t, 0, st.OpenLedgerBreaks)
	for _, b := range breaks() {
		assert.NotNil(t, b.ResolvedAt, "how long each stayed open is part of the record")
	}
	assert.Len(t, outboxTypes(t, h.all, "reconciliation.ledger"), 2, "closing is not announced; the row says when")
}
