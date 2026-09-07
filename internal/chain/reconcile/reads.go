package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
)

// Latest is the most recent pass, for the admin API to display.
//
// A read, not a run: the admin role has no node, so it cannot produce a report
// any more than it can sign a withdrawal. What it can do is show the one the
// chain role wrote, which is why there is no POST here (§7.4 lists one; it
// waits for the same intent-record pattern withdrawal resolve uses).
//
// Returns ErrNoReport when no pass has been recorded, which is a real state
// and not an error: a deployment whose chain role has just started, or one
// that has none.
func Latest(ctx context.Context, db *pgxpool.Pool, tenant string, chainID int64) (Report, error) {
	row, err := sqlcgen.New(db).GetLatestReconciliationReport(ctx, sqlcgen.GetLatestReconciliationReportParams{
		TenantID: tenant, ChainID: chainID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Report{}, ErrNoReport
	}
	if err != nil {
		return Report{}, fmt.Errorf("reconcile: latest report: %w", err)
	}
	var lines []Line
	if err := json.Unmarshal(row.Lines, &lines); err != nil {
		return Report{}, fmt.Errorf("reconcile: decode report %s: %w", row.ID, err)
	}
	return Report{
		ID: row.ID, ChainID: row.ChainID, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt,
		Balanced: row.Balanced, Lines: lines,
	}, nil
}
