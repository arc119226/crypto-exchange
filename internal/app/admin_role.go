package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/arc119226/crypto-exchange/internal/admin"
	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// trialBalanceInterval is how often the admin role refreshes
// ledger_trial_balance_diff (docs/plan-v1.0.md §15).
const trialBalanceInterval = 30 * time.Second

// newLedger builds the ledger service for a role and loads the tenant's
// house accounts (seeded by migration 0003), retrying while the database
// is still being migrated.
func newLedger(ctx context.Context, cfg Config, log *slog.Logger, pool *pgxpool.Pool, reg prometheus.Registerer) (*ledger.Service, error) {
	svc := ledger.New(pool, cfg.TenantID).WithMetrics(ledger.NewMetrics(reg))
	err := retryUntil(ctx, log, "ledger house accounts", func(ctx context.Context) error {
		return svc.LoadHouseAccounts(ctx)
	})
	if err != nil {
		return nil, fmt.Errorf("ledger: %w", err)
	}
	return svc, nil
}

// newAdminServer builds the operator API listener: correlation ids,
// metrics, panic recovery, the static admin API key, then the OpenAPI routes.
func newAdminServer(cfg Config, log *slog.Logger, m *telemetry.HTTPMetrics, pool *pgxpool.Pool, l *ledger.Service) *http.Server {
	r := chi.NewRouter()
	r.Use(telemetry.CorrelationMiddleware(log))
	r.Use(m.Middleware(func(req *http.Request) string {
		if rc := chi.RouteContext(req.Context()); rc != nil {
			return rc.RoutePattern()
		}
		return ""
	}))
	r.Use(recoverer())
	r.Use(admin.RequireAPIKey(cfg.Admin.APIKey.Reveal()))
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		admin.WriteProblem(w, req, http.StatusNotFound, "Not Found", "no route for "+req.Method+" "+req.URL.Path)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		admin.WriteProblem(w, req, http.StatusMethodNotAllowed, "Method Not Allowed", "")
	})
	admin.Mount(r, admin.NewHandler(pool, l, registry.NewStore(pool), audit.NewRecorder(cfg.TenantID), cfg.TenantID))
	return &http.Server{Addr: cfg.AdminAddr, Handler: r, ReadHeaderTimeout: 5 * time.Second}
}

// observeTrialBalance refreshes the trial-balance gauge until ctx ends.
func observeTrialBalance(ctx context.Context, log *slog.Logger, l *ledger.Service) error {
	tick := time.NewTicker(trialBalanceInterval)
	defer tick.Stop()
	for {
		if err := l.ObserveTrialBalance(ctx); err != nil && ctx.Err() == nil {
			log.Warn("trial balance refresh failed", slog.String("err", err.Error()))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
