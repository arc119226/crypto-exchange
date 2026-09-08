package admin

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/arc119226/crypto-exchange/internal/admin/sqlcgen"
)

// Backup is the newest successful backup of one kind, as recorded in
// admin.backups by the backup sidecar (scripts/backup.sh): a pg_dump, or a
// batch of WAL segments shipped to the store.
type Backup struct {
	Kind       string // dump | wal
	FinishedAt time.Time
	SizeBytes  int64
	Location   string // the object key inside the backup bucket
}

// The kinds admin.backups records.
const (
	BackupDump = "dump"
	BackupWAL  = "wal"
)

// LatestBackups returns the newest successful backup of each kind; a kind
// that has never succeeded is absent.
func (h *Handler) LatestBackups(ctx context.Context) ([]Backup, error) {
	rows, err := sqlcgen.New(h.pool).LatestBackups(ctx)
	if err != nil {
		return nil, fmt.Errorf("admin: latest backups: %w", err)
	}
	out := make([]Backup, 0, len(rows))
	for _, r := range rows {
		out = append(out, Backup{Kind: r.Kind, FinishedAt: r.FinishedAt, SizeBytes: r.SizeBytes, Location: r.Location})
	}
	return out, nil
}

// BackupMetrics is the gauge the BackupStale and WalArchiveStale alerts
// read (infra/observability/alerts.yml).
type BackupMetrics struct {
	lastSuccess *prometheus.GaugeVec
}

// NewBackupMetrics registers the gauge; a nil registerer keeps it private.
func NewBackupMetrics(reg prometheus.Registerer) *BackupMetrics {
	m := &BackupMetrics{
		lastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "backup_last_success_timestamp_seconds",
			Help: "When the newest successful backup of each kind finished (unix seconds), from admin.backups.",
		}, []string{"kind"}),
	}
	if reg != nil {
		reg.MustRegister(m.lastSuccess)
	}
	return m
}

// WithBackupMetrics attaches the gauge ObserveBackups refreshes.
func (h *Handler) WithBackupMetrics(m *BackupMetrics) *Handler {
	h.backups = m
	return h
}

// ObserveBackups refreshes the gauge from admin.backups. A kind that has
// never succeeded gets no series, which is what the BackupNeverTaken alert
// keys on: absence, not zero.
func (h *Handler) ObserveBackups(ctx context.Context) error {
	if h.backups == nil {
		return nil
	}
	latest, err := h.LatestBackups(ctx)
	if err != nil {
		return err
	}
	for _, b := range latest {
		h.backups.lastSuccess.WithLabelValues(b.Kind).Set(float64(b.FinishedAt.Unix()))
	}
	return nil
}
