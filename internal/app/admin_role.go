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
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
	"github.com/arc119226/crypto-exchange/internal/webhook"
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
func newAdminServer(cfg Config, log *slog.Logger, m *telemetry.HTTPMetrics, reg prometheus.Registerer, pool *pgxpool.Pool, l *ledger.Service) (*http.Server, error) {
	// Only to seal the secrets of endpoints this role creates. Config.Validate
	// has already checked the key, so a failure here is a wiring mistake --
	// but swallowing it would leave admin up with every webhook endpoint
	// returning 500, which is a worse way to find out.
	master, err := cfg.Webhook.Master()
	if err != nil {
		return nil, fmt.Errorf("webhook signing key: %w", err)
	}
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
	rec := audit.NewRecorder(cfg.TenantID)
	admin.Mount(r, admin.NewHandler(pool, l, registry.NewStore(pool), rec, cfg.TenantID).
		// Which chain's reconciliation reports this role shows. It cannot
		// produce one -- it has no node -- so this is only which rows to read.
		WithChainID(cfg.Chain.ChainID).
		// The review queue. Approving marks a withdrawal for the chain worker;
		// this role can neither lock funds nor sign (docs/plan-v1.0.md §6.4.2).
		WithWithdrawals(withdrawal.NewReviewer(pool, cfg.TenantID, l, rec)).
		// Endpoint configuration and replay. This role never delivers -- it has
		// no consumer and no delivery loop -- so the only thing it does to the
		// queue is add a run to it (migration 0016/0017 grant exactly that).
		WithWebhooks(webhook.NewStore(pool, cfg.TenantID, master).
			WithMetrics(webhook.NewAdminMetrics(reg))))
	return &http.Server{Addr: cfg.AdminAddr, Handler: r, ReadHeaderTimeout: 5 * time.Second}, nil
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
