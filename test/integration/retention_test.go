//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/app"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// TestRetentionPrunesHistoryOnly: the worker role's retention pass removes
// what is only history (published outbox rows, consumer idempotency
// records, webhook attempts, stored bodies nobody queues any more) once it
// is older than the configured age, and nothing else: unpublished rows,
// young rows, and bodies a queue row still references stay. It runs as
// ex_worker, which is the role that got the DELETE grants in 0022.
func TestRetentionPrunesHistoryOnly(t *testing.T) {
	h := setupLedger(t)
	ctx := context.Background()
	worker, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_worker"), MaxConns: 2})
	require.NoError(t, err)
	t.Cleanup(worker.Close)

	old := time.Now().Add(-40 * 24 * time.Hour)
	older := time.Now().Add(-100 * 24 * time.Hour)
	young := time.Now().Add(-time.Hour)
	// outbox: published old, published young, unpublished old
	for i, row := range []struct {
		id        string
		published *time.Time
	}{{"01RET0000000000000000000A", &old}, {"01RET0000000000000000000B", &young}, {"01RET0000000000000000000C", nil}} {
		_, err := h.all.Exec(ctx, `INSERT INTO eventbus.outbox (event_id, event_type, tenant_id, subject, payload, occurred_at, published_at)
			VALUES ($1, 'order.accepted', 'default', 'ex.v1.order.accepted.default.x', '{}', $2, $3)`, row.id, old.Add(time.Duration(i)*time.Second), row.published)
		require.NoError(t, err)
	}
	_, err = h.all.Exec(ctx, `INSERT INTO eventbus.processed_events (consumer, event_id, processed_at) VALUES ('c', 'e-old', $1), ('c', 'e-young', $2)`, old, young)
	require.NoError(t, err)

	// webhook: an endpoint, three stored events (old unreferenced, old but
	// still queued, young), attempts old and young
	var endpoint string
	require.NoError(t, h.all.QueryRow(ctx, `INSERT INTO webhook.endpoints (url, secret_enc, events) VALUES ('https://example.invalid/hook', '\x00', ARRAY['order.accepted']) RETURNING id`).Scan(&endpoint))
	for _, ev := range []struct {
		id string
		at time.Time
	}{{"w-old", older}, {"w-queued", older}, {"w-young", young}} {
		_, err := h.all.Exec(ctx, `INSERT INTO webhook.events (tenant_id, event_id, event_type, body, created_at) VALUES ('default', $1, 'order.accepted', '{}', $2)`, ev.id, ev.at)
		require.NoError(t, err)
	}
	_, err = h.all.Exec(ctx, `INSERT INTO webhook.queue (tenant_id, endpoint_id, event_id) VALUES ('default', $1, 'w-queued')`, endpoint)
	require.NoError(t, err)
	for _, at := range []time.Time{older, young} {
		_, err := h.all.Exec(ctx, `INSERT INTO webhook.deliveries (endpoint_id, event_id, event_type, attempt, status, created_at, run_id) VALUES ($1, 'w-old', 'order.accepted', 1, 'failed', $2, gen_random_uuid())`, endpoint, at)
		require.NoError(t, err)
	}

	cfg := app.RetentionConfig{Interval: time.Hour, Outbox: 30 * 24 * time.Hour, Webhook: 90 * 24 * time.Hour, BatchSize: 1}
	deleted, err := app.PruneRetention(ctx, worker, cfg, time.Now())
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"eventbus.outbox": 1, "eventbus.processed_events": 1, "webhook.deliveries": 1, "webhook.events": 1}, deleted, "batch size 1 loops until each table is clean")

	var left []string
	rows, err := h.all.Query(ctx, `SELECT event_id FROM eventbus.outbox WHERE event_id LIKE '01RET%' ORDER BY event_id`)
	require.NoError(t, err)
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		left = append(left, id)
	}
	rows.Close()
	assert.Equal(t, []string{"01RET0000000000000000000B", "01RET0000000000000000000C"}, left, "young and unpublished rows stay")
	var n int
	require.NoError(t, h.all.QueryRow(ctx, `SELECT count(*) FROM eventbus.processed_events WHERE event_id = 'e-young'`).Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, h.all.QueryRow(ctx, `SELECT count(*) FROM webhook.events WHERE event_id IN ('w-queued', 'w-young')`).Scan(&n))
	assert.Equal(t, 2, n, "a queued body and a young body stay")
	require.NoError(t, h.all.QueryRow(ctx, `SELECT count(*) FROM webhook.deliveries`).Scan(&n))
	assert.Equal(t, 1, n)

	again, err := app.PruneRetention(ctx, worker, cfg, time.Now())
	require.NoError(t, err)
	assert.Equal(t, int64(0), again["eventbus.outbox"]+again["webhook.events"], "a second pass finds nothing")

	t.Run("the api role cannot prune", func(t *testing.T) {
		api, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_api"), MaxConns: 1})
		require.NoError(t, err)
		defer api.Close()
		_, err = app.PruneRetention(ctx, api, cfg, time.Now())
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "42501", pgErr.Code, "DELETE on the outbox is the worker's alone")
	})
}
