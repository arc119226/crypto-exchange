package admin

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/ratelimit"
)

// Sessions is the part of auth.Service the pages use.
type Sessions interface {
	SessionResolver
	AdminLogin(ctx context.Context, email, password, ip string) (auth.AdminSession, error)
	TOTPConfirm(ctx context.Context, token, code, ip string) (auth.AdminSession, error)
	TOTPVerify(ctx context.Context, token, code, ip string) (auth.AdminSession, error)
	AdminLogout(ctx context.Context, token, ip string) error
}

// UIConfig is what a deployment decides about the pages.
type UIConfig struct {
	Cookies Cookies
	// LoginLimit throttles the password step per source address. The TOTP
	// step has its own per-user lock; this keeps a password guesser from
	// spending argon2 on this process at full speed.
	LoginLimit ratelimit.Limit
	Limiter    ratelimit.Limiter
	Registerer prometheus.Registerer
}

// UI is the back office: the pages under /admin.
type UI struct {
	h        *Handler
	sessions Sessions
	cfg      UIConfig
	tpl      *templates
	metrics  *uiMetrics
	reveals  revealStash
}

// NewUI parses the templates and wires the pages to the handler whose writes
// they share.
func NewUI(h *Handler, sessions Sessions, cfg UIConfig) (*UI, error) {
	tpl, err := loadTemplates()
	if err != nil {
		return nil, err
	}
	if cfg.Limiter == nil {
		cfg.Limiter = ratelimit.NewMemory()
	}
	if cfg.LoginLimit.N == 0 {
		cfg.LoginLimit = ratelimit.Limit{N: 10, Window: 60e9}
	}
	return &UI{h: h, sessions: sessions, cfg: cfg, tpl: tpl, metrics: newUIMetrics(cfg.Registerer)}, nil
}

// Routes registers everything the admin listener serves on r: the OpenAPI
// routes behind the API key, and -- when ui is set -- the pages behind a
// session. Both groups sit behind CrossOriginProtection (see admin_role.go).
// Call it on a router that has its base middleware and nothing else.
func Routes(r chi.Router, h *Handler, ui *UI, apiKey string) {
	r.Use(WithClientIP)
	// Cross-site request forgery, for the whole listener. Browsers announce
	// where an unsafe request came from (Sec-Fetch-Site, Origin) and this
	// refuses anything but this origin; a client that sends neither header --
	// exchangectl, curl -- is not a browser and passes. The session cookie is
	// SameSite=Lax as well, so this is the second line.
	r.Use(http.NewCrossOriginProtection().Handler)

	r.Group(func(api chi.Router) {
		api.Use(RequireAPIKey(apiKey))
		Mount(api, h)
	})
	if ui != nil {
		ui.mount(r)
	}
}

func (u *UI) mount(r chi.Router) {
	r.Get("/", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, HomePath, http.StatusSeeOther) })
	r.Get("/admin", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, HomePath, http.StatusSeeOther) })
	r.Handle("/admin/static/*", http.StripPrefix("/admin/static/", staticHandler()))

	r.Group(func(pages chi.Router) {
		pages.Use(secureHeaders)
		pages.Get(LoginPath, u.loginForm)
		pages.With(u.throttleLogin).Post(LoginPath, u.login)

		pages.Group(func(pending chi.Router) {
			pending.Use(RequirePending(u.sessions, u.cfg.Cookies))
			pending.Get(TOTPPath, u.totpForm)
			pending.Post(TOTPPath, u.totp)
			pending.Post(LogoutPath, u.logout)
		})
		pages.Group(func(admin chi.Router) {
			admin.Use(RequireAdmin(u.sessions, u.cfg.Cookies))
			admin.Get(HomePath, u.dashboard)
			admin.Get("/admin/users", u.users)
			admin.Get("/admin/users/{id}", u.user)
			admin.Post("/admin/users/{id}/kyc-level", u.setUserKYC)
			admin.Post("/admin/users/{id}/status", u.setUserStatus)
			admin.Get("/admin/assets", u.assets)
			admin.Post("/admin/assets", u.createAsset)
			admin.Post("/admin/assets/{symbol}", u.updateAsset)
			admin.Get("/admin/markets", u.markets)
			admin.Post("/admin/markets", u.createMarket)
			admin.Post("/admin/markets/{symbol}", u.updateMarket)
			admin.Post("/admin/markets/{symbol}/status", u.setMarketStatusPage)
			admin.Post("/admin/engine/reload", u.reload)
			admin.Get("/admin/fee-schedules", u.feeSchedules)
			admin.Post("/admin/fee-schedules", u.createFeeSchedule)
			admin.Post("/admin/fee-schedules/{name}", u.updateFeeSchedule)
			admin.Get("/admin/withdrawal-limits", u.withdrawalLimits)
			admin.Post("/admin/withdrawal-limits", u.setWithdrawalLimitPage)
			admin.Post("/admin/withdrawal-limits/{asset}", u.setWithdrawalLimitPage)
			admin.Get("/admin/ledger", u.ledger)
			admin.Post("/admin/ledger/adjustments", u.createAdjustment)
			admin.Post("/admin/ledger/house-adjustments", u.createHouseAdjustment)
			admin.Get("/admin/withdrawals", u.withdrawals)
			admin.Post("/admin/withdrawals/{id}/review", u.reviewWithdrawalPage)
			admin.Post("/admin/withdrawals/{id}/resolve", u.resolveWithdrawalPage)
			admin.Get("/admin/chain", u.chain)
			admin.Get("/admin/reconciliation", u.reconciliation)
			admin.Get("/admin/audit", u.audit)
			admin.Get("/admin/webhooks", u.webhooks)
			admin.Post("/admin/webhooks", u.createWebhook)
			admin.Post("/admin/webhooks/{id}", u.updateWebhook)
			admin.Post("/admin/webhooks/{id}/status", u.setWebhookStatus)
			admin.Post("/admin/webhooks/{id}/rotate-secret", u.rotateWebhookSecretPage)
			admin.Post("/admin/webhooks/{id}/deliveries/{delivery_id}/replay", u.replayWebhookPage)
		})
	})
}

// secureHeaders is the browser-side hardening every page carries. The CSP
// allows only this origin: htmx is served from here, and there is no inline
// script anywhere in the templates.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// throttleLogin bounds password attempts per source address.
func (u *UI) throttleLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, err := u.cfg.Limiter.Allow(r.Context(), "admin-login:"+clientIPFrom(r.Context()), u.cfg.LoginLimit)
		if err != nil {
			WriteProblem(w, r, http.StatusInternalServerError, "Internal Server Error", "")
			return
		}
		if !d.Allowed {
			u.metrics.logins.WithLabelValues("throttled").Inc()
			w.Header().Set("Retry-After", strconv.Itoa(int(d.RetryAfter.Seconds())+1))
			u.tpl.render(w, r, http.StatusTooManyRequests, "login", view{
				Title: "Sign in", Flash: &flash{Kind: "err", Text: fmt.Sprintf("Too many attempts; try again in %s.", d.RetryAfter.Round(1e9))},
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// uiMetrics is what the pages export.
type uiMetrics struct {
	logins *prometheus.CounterVec
}

func newUIMetrics(reg prometheus.Registerer) *uiMetrics {
	m := &uiMetrics{
		logins: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "admin_login_attempts_total",
			Help: "Back-office login attempts by outcome: password_ok, password_failed, totp_ok, totp_failed, locked, throttled.",
		}, []string{"outcome"}),
	}
	if reg != nil {
		reg.MustRegister(m.logins)
	}
	return m
}
