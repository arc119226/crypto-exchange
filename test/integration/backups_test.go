//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/admin"
	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// TestBackupRecordReachesTheOperator: the sidecar writes admin.backups as
// ex_backup (scripts/backup.sh); the admin role turns the newest successful
// row per kind into the system status and into the gauge the BackupStale
// and WalArchiveStale alerts read.
func TestBackupRecordReachesTheOperator(t *testing.T) {
	ctx := context.Background()
	h := setupLedger(t)

	// what the sidecar does, with the sidecar's role
	backupConn, err := pgx.Connect(ctx, h.DSN("ex_backup"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = backupConn.Close(ctx) })
	insert := func(kind, status string, finished time.Time, size int64, location string) {
		_, err := backupConn.Exec(ctx,
			`INSERT INTO admin.backups (kind, started_at, finished_at, status, size_bytes, location, error)
			 VALUES ($1, $3::timestamptz - interval '1 minute', $3::timestamptz, $2, $4, $5, NULLIF($6, ''))`,
			kind, status, finished, size, location, map[string]string{"ok": "", "failed": "upload failed"}[status])
		require.NoError(t, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	insert("dump", "ok", now.Add(-30*time.Hour), 1000, "dumps/old.dump")
	insert("dump", "ok", now.Add(-2*time.Hour), 4096, "dumps/exchange-latest.dump")
	insert("dump", "failed", now.Add(-time.Hour), 0, "dumps/exchange-failed.dump")
	insert("wal", "ok", now.Add(-10*time.Minute), 16<<20, "wal/000000010000000000000042")

	t.Run("the backup role reads everything and writes nothing else", func(t *testing.T) {
		var n int
		require.NoError(t, backupConn.QueryRow(ctx, `SELECT count(*) FROM ledger.accounts`).Scan(&n), "pg_read_all_data")
		_, err := backupConn.Exec(ctx, `INSERT INTO ledger.journal_entries (idempotency_key, kind) VALUES ('backup-role', 'adjustment')`)
		require.Error(t, err, "the ledger is not the backup role's to write")
		_, err = backupConn.Exec(ctx, `DELETE FROM admin.backups`)
		require.Error(t, err, "and neither is the record, once written")
	})

	reg := prometheus.NewRegistry()
	handler := admin.NewHandler(h.all, h.svc, registry.NewStore(h.all), audit.NewRecorder("default"), "default").
		WithBackupMetrics(admin.NewBackupMetrics(reg))

	t.Run("the newest successful row per kind", func(t *testing.T) {
		latest, err := handler.LatestBackups(ctx)
		require.NoError(t, err)
		byKind := map[string]admin.Backup{}
		for _, b := range latest {
			byKind[b.Kind] = b
		}
		require.Len(t, byKind, 2)
		assert.Equal(t, "dumps/exchange-latest.dump", byKind["dump"].Location, "the failed, newer row does not count")
		assert.Equal(t, int64(4096), byKind["dump"].SizeBytes)
		assert.WithinDuration(t, now.Add(-2*time.Hour), byKind["dump"].FinishedAt, time.Second)
		assert.Equal(t, "wal/000000010000000000000042", byKind["wal"].Location)
	})

	t.Run("the gauge the alerts read", func(t *testing.T) {
		require.NoError(t, handler.ObserveBackups(ctx))
		mfs, err := reg.Gather()
		require.NoError(t, err)
		got := map[string]float64{}
		for _, mf := range mfs {
			if mf.GetName() != "backup_last_success_timestamp_seconds" {
				continue
			}
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "kind" {
						got[l.GetValue()] = m.GetGauge().GetValue()
					}
				}
			}
		}
		assert.Equal(t, float64(now.Add(-2*time.Hour).Unix()), got["dump"])
		assert.Equal(t, float64(now.Add(-10*time.Minute).Unix()), got["wal"])
	})

	t.Run("GET /admin/v1/system/status carries the newest dump", func(t *testing.T) {
		srv := adminServer(t, h)
		res := call(t, srv, http.MethodGet, "/admin/v1/system/status", adminKey, nil)
		require.Equal(t, http.StatusOK, res.status, string(res.body))
		var st struct {
			LastBackup *struct {
				Kind       string    `json:"kind"`
				FinishedAt time.Time `json:"finished_at"`
				SizeBytes  int64     `json:"size_bytes"`
				Location   string    `json:"location"`
			} `json:"last_backup"`
		}
		require.NoError(t, json.Unmarshal(res.body, &st))
		require.NotNil(t, st.LastBackup)
		assert.Equal(t, "dump", st.LastBackup.Kind)
		assert.Equal(t, "dumps/exchange-latest.dump", st.LastBackup.Location)
		assert.Equal(t, int64(4096), st.LastBackup.SizeBytes)
	})

	t.Run("no backup yet is null, not an error", func(t *testing.T) {
		fresh := setupLedger(t)
		srv := adminServer(t, fresh)
		res := call(t, srv, http.MethodGet, "/admin/v1/system/status", adminKey, nil)
		require.Equal(t, http.StatusOK, res.status, string(res.body))
		var st map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(res.body, &st))
		v, ok := st["last_backup"]
		if ok {
			assert.Equal(t, "null", string(v))
		}
	})
}
