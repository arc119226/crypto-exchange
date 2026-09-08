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
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/chain/deposit"
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/ratelimit"
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

// newAdminServer builds the operator listener: correlation ids, metrics,
// panic recovery, then two groups on one router -- the OpenAPI routes behind
// the static API key for machines, and the back-office pages behind a
// session cookie for people (docs/plan-v1.0.md §12).
func newAdminServer(cfg Config, log *slog.Logger, m *telemetry.HTTPMetrics, reg prometheus.Registerer, pool *pgxpool.Pool, l *ledger.Service) (*http.Server, *admin.Handler, error) {
	// Only to seal the secrets of endpoints this role creates. Config.Validate
	// has already checked the key, so a failure here is a wiring mistake --
	// but swallowing it would leave admin up with every webhook endpoint
	// returning 500, which is a worse way to find out.
	signing, err := cfg.Webhook.Keys()
	if err != nil {
		return nil, nil, fmt.Errorf("webhook signing key: %w", err)
	}
	// Run has already refused to start without it; this only decodes it.
	totp, err := cfg.Admin.TOTPKeys()
	if err != nil {
		return nil, nil, fmt.Errorf("admin totp key: %w", err)
	}
	loginLimit, err := ratelimit.ParseLimit(cfg.RateLimit.LoginPerIP)
	if err != nil {
		return nil, nil, fmt.Errorf("config: LOGIN_PER_IP: %w", err)
	}
	r := chi.NewRouter()
	r.Use(telemetry.CorrelationMiddleware(log))
	r.Use(m.Middleware(chiRoute))
	r.Use(telemetry.TracingMiddleware(chiRoute))
	r.Use(recoverer())
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		admin.WriteProblem(w, req, http.StatusNotFound, "Not Found", "no route for "+req.Method+" "+req.URL.Path)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		admin.WriteProblem(w, req, http.StatusMethodNotAllowed, "Method Not Allowed", "")
	})
	rec := audit.NewRecorder(cfg.TenantID)
	// A deployment with no signing key cannot have working endpoints at all --
	// the worker could not open their secrets either -- so the endpoints say
	// so instead of creating ones that could never be delivered to.
	var webhooks *webhook.Store
	if !signing.Empty() {
		webhooks = webhook.NewStore(pool, cfg.TenantID, signing.Current).WithPreviousKey(signing.Previous).WithMetrics(webhook.NewAdminMetrics(reg))
	} else {
		log.Warn("WEBHOOK_SIGNING_KEY is not set: the webhook endpoints are disabled")
	}
	// The sessions people log in with, and the user directory they edit. No
	// signer: this role issues no JWTs, and no master key: it opens no
	// API-key secret (docs/plan-v1.0.md §14).
	sessions, err := auth.New(pool, auth.Config{
		Tenant: cfg.TenantID, Issuer: cfg.Auth.Issuer, TOTPKey: totp.Current, PreviousTOTPKey: totp.Previous, AdminSessionTTL: cfg.Admin.SessionTTL,
	}, nil, nil, l, rec)
	if err != nil {
		return nil, nil, fmt.Errorf("admin sessions: %w", err)
	}
	h := admin.NewHandler(pool, l, registry.NewStore(pool), rec, cfg.TenantID).
		// Which chain's reconciliation reports this role shows. It cannot
		// produce one -- it has no node -- so this is only which rows to read.
		WithChainID(cfg.Chain.ChainID).
		// The review queue. Approving marks a withdrawal for the chain worker;
		// this role can neither lock funds nor sign (docs/plan-v1.0.md §6.4.2).
		WithWithdrawals(withdrawal.NewReviewer(pool, cfg.TenantID, l, rec)).
		// Endpoint configuration and replay. This role never delivers -- it has
		// no consumer and no delivery loop -- so the only thing it does to the
		// queue is add a run to it (migration 0016/0017 grant exactly that).
		WithWebhooks(webhooks).
		// Read-only: what the chain role has seen and not yet credited.
		WithDeposits(deposit.NewReader(pool, cfg.TenantID)).
		// People: the directory, KYC levels, freezes.
		WithUsers(sessions)
	// One admin replica (docs/plan-v1.0.md §5.2), so the login throttle can
	// live in memory: a second replica would only double the allowance.
	ui, err := admin.NewUI(h, sessions, admin.UIConfig{
		Cookies: admin.Cookies{Secure: cfg.SecureCookies()}, LoginLimit: loginLimit, Registerer: reg,
	})
	if err != nil {
		return nil, nil, err
	}
	if !cfg.SecureCookies() {
		log.Warn("admin session cookie is not Secure (ADMIN_COOKIE_SECURE / EXCHANGE_ENV=dev): plain http only on a private network")
	}
	admin.Routes(r, h, ui, cfg.Admin.APIKey.Reveal())
	return &http.Server{Addr: cfg.AdminAddr, Handler: r, ReadHeaderTimeout: 5 * time.Second}, h, nil
}

// observeLedger refreshes the trial-balance gauge and runs the ledger's
// self-check until ctx ends: the first is a number for Prometheus, the second
// is a row and an event for people (docs/plan-v1.0.md §12, §15). Both read
// the same sums; the check is what turns a gauge nobody watches at 3am into
// something that pages.
func observeLedger(ctx context.Context, log *slog.Logger, l *ledger.Service, h *admin.Handler) error {
	tick := time.NewTicker(trialBalanceInterval)
	defer tick.Stop()
	for {
		if err := l.ObserveTrialBalance(ctx); err != nil && ctx.Err() == nil {
			log.Warn("trial balance refresh failed", slog.String("err", err.Error()))
		}
		if _, _, err := h.WatchLedgerBreaks(ctx); err != nil && ctx.Err() == nil {
			log.Warn("ledger break check failed", slog.String("err", err.Error()))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
