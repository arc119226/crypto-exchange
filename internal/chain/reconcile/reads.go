package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"

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

// List returns passes newest first, each with its lines.
func List(ctx context.Context, db *pgxpool.Pool, tenant string, chainID int64, limit, offset int32) ([]Report, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := sqlcgen.New(db).ListReconciliationReports(ctx, sqlcgen.ListReconciliationReportsParams{
		TenantID: tenant, ChainID: chainID, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile: list reports: %w", err)
	}
	out := make([]Report, 0, len(rows))
	for _, row := range rows {
		var lines []Line
		if err := json.Unmarshal(row.Lines, &lines); err != nil {
			return nil, fmt.Errorf("reconcile: decode report %s: %w", row.ID, err)
		}
		out = append(out, Report{
			ID: row.ID, ChainID: row.ChainID, StartedAt: row.StartedAt, FinishedAt: row.FinishedAt,
			Balanced: row.Balanced, Lines: lines,
		})
	}
	return out, nil
}

// Break is one asset of one pass that did not balance, as recorded in
// admin.reconciliation_breaks: the line, plus which report it came from and
// when. A break has no "resolved" state of its own -- the next balanced pass
// is the resolution, and the report list shows it.
type Break struct {
	ID         string
	ReportID   string
	ChainID    int64
	Line       Line
	DetectedAt time.Time
}

// Breaks returns every break on record, newest first.
func Breaks(ctx context.Context, db *pgxpool.Pool, tenant string, chainID int64, limit, offset int32) ([]Break, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := sqlcgen.New(db).ListReconciliationBreaksForTenant(ctx, sqlcgen.ListReconciliationBreaksForTenantParams{
		TenantID: tenant, ChainID: chainID, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile: list breaks: %w", err)
	}
	out := make([]Break, 0, len(rows))
	for _, r := range rows {
		b := Break{ID: r.ID, ReportID: r.ReportID, ChainID: r.ChainID, DetectedAt: r.CreatedAt, Line: Line{Asset: r.Asset, BlockHeight: r.BlockHeight}}
		for dst, src := range map[*money.Amount]pgtype.Numeric{
			&b.Line.LedgerTotal: r.LedgerTotal, &b.Line.ChainTotal: r.ChainTotal, &b.Line.Uncredited: r.Uncredited,
			&b.Line.AboveFrontier: r.AboveFrontier, &b.Line.InFlight: r.InFlight, &b.Line.Diff: r.Diff,
		} {
			v, err := pg.AmountFromNumeric(src)
			if err != nil {
				return nil, fmt.Errorf("reconcile: break %s: %w", r.ID, err)
			}
			*dst = v
		}
		out = append(out, b)
	}
	return out, nil
}
