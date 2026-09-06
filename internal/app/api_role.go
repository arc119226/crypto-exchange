package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/arc119226/crypto-exchange/internal/api"
	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/chain"
	"github.com/arc119226/crypto-exchange/internal/cmdbus"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/ratelimit"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

// newAPIServer builds the public REST listener: correlation ids, metrics,
// panic recovery and authentication first, then the OpenAPI routes from
// internal/api, with RFC 7807 bodies for everything the router itself
// rejects (404/405/500).
//
// engine is the in-process command bus when the engine role runs in the
// same process (role=all); nil leaves the trading endpoints answering 503
// until the NATS request-reply bus arrives (Phase 3c).
func newAPIServer(ctx context.Context, cfg Config, log *slog.Logger, m *telemetry.HTTPMetrics, reg prometheus.Registerer, d *deps, l *ledger.Service, engine *trading.Engine) (*http.Server, *registryRefresher, error) {
	store := registry.NewStore(d.pool)
	authSvc, err := newAuthService(cfg, log, d, l)
	if err != nil {
		return nil, nil, err
	}

	var (
		cache *registry.Cache
		bus   trading.CommandBus
	)
	switch {
	case engine != nil:
		// role=all: the engine is the bus, so commands never leave the process
		cache, bus = engine.Registry(), engine
	default:
		cache = registry.NewCache(cfg.TenantID)
		if err := retryUntil(ctx, log, "registry", func(ctx context.Context) error { return cache.Load(ctx, store) }); err != nil {
			return nil, nil, err
		}
		if d.nc != nil {
			client, err := cmdbus.NewClient(d.nc, cmdbus.Config{
				Tenant: cfg.TenantID, SubjectPrefix: cfg.Engine.CommandSubjectPrefix, Timeout: cfg.Engine.CommandTimeout,
				Token: internalTokenSource(authSvc.Signer(), cfg.TenantID, cfg.Engine.InternalTokenTTL), Metrics: cmdbus.NewMetrics(reg),
			})
			if err != nil {
				return nil, nil, err
			}
			bus = client
			log.Info("trading commands go to the engine over NATS",
				slog.String("subject", client.Subject("<market>")), slog.Duration("timeout", cfg.Engine.CommandTimeout))
		} else {
			log.Warn("api running without an in-process engine and without NATS: trading endpoints answer 503")
		}
	}
	var tradingSvc *trading.Service
	if bus != nil {
		tradingSvc = trading.NewService(d.pool, bus, cache, cfg.TenantID)
	}

	limits, err := parseLimits(cfg.RateLimit)
	if err != nil {
		return nil, nil, err
	}
	var limiter ratelimit.Limiter = ratelimit.NewMemory()
	if d.rdb != nil {
		limiter = ratelimit.Fallback{
			Primary: ratelimit.NewRedis(d.rdb), Secondary: ratelimit.NewMemory(),
			OnError: func(err error) {
				log.Warn("rate limiter: redis unavailable, using in-memory buckets", slog.String("err", err.Error()))
			},
		}
	}

	handler := newAPIRouter(log, m, api.Deps{
		Tenant: cfg.TenantID, Registry: store, Auth: authSvc, Ledger: l, Trading: tradingSvc, Limiter: limiter, Limits: limits,
		// assignment only: the pool is filled by the signer role, which is the
		// only process that holds the seed (docs/plan-v1.0.md §6.4.1)
		Chain: chain.NewAddresses(d.pool, cfg.TenantID, cfg.Chain.ChainID),
	})
	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	var refresh *registryRefresher
	if engine == nil {
		refresh = &registryRefresher{cache: cache, store: store, interval: cfg.Registry.RefreshInterval, log: log}
	}
	return srv, refresh, nil
}

// newAPIRouter assembles the middleware chain and the OpenAPI routes. A nil
// Registry (tests) leaves only the fallbacks mounted; a nil Auth skips the
// authentication middleware.
func newAPIRouter(log *slog.Logger, m *telemetry.HTTPMetrics, d api.Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(telemetry.CorrelationMiddleware(log))
	r.Use(m.Middleware(func(req *http.Request) string {
		if rc := chi.RouteContext(req.Context()); rc != nil {
			return rc.RoutePattern()
		}
		return ""
	}))
	r.Use(recoverer())
	if d.Auth != nil {
		r.Use(d.Auth.Authenticate(api.WriteProblem))
	}
	notFound := func(w http.ResponseWriter, req *http.Request) {
		api.WriteProblem(w, req, http.StatusNotFound, "Not Found", "no route for "+req.Method+" "+req.URL.Path)
	}
	r.NotFound(notFound)
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		api.WriteProblem(w, req, http.StatusMethodNotAllowed, "Method Not Allowed", "")
	})
	if d.Registry != nil {
		api.Mount(r, api.NewHandler(d))
	}
	if len(r.Routes()) == 0 {
		// chi only builds its middleware chain once a route exists; without
		// any route this catch-all keeps correlation ids, metrics and the
		// problem+json body for unmatched paths. It must not be registered
		// when real routes exist, or it would swallow 405s as 404s.
		r.HandleFunc("/*", notFound)
	}
	return r
}

// newAuthService loads the JWT signing key and the API key master key. In
// dev both may be absent: ephemeral values are generated (sessions and API
// keys then die with the process) and a warning is logged.
func newAuthService(cfg Config, log *slog.Logger, d *deps, l *ledger.Service) (*auth.Service, error) {
	var (
		priv ed25519.PrivateKey
		err  error
	)
	switch {
	case cfg.JWT.PrivateKeyFile != "":
		if priv, err = auth.LoadPrivateKey(cfg.JWT.PrivateKeyFile); err != nil {
			return nil, err
		}
	case cfg.Env == "dev":
		if _, priv, err = ed25519.GenerateKey(rand.Reader); err != nil {
			return nil, fmt.Errorf("auth: ephemeral key: %w", err)
		}
		log.Warn("JWT_PRIVATE_KEY_FILE is empty: using an ephemeral signing key (dev only; run `exchange keys gen-jwt`)")
	default:
		return nil, fmt.Errorf("config: JWT_PRIVATE_KEY_FILE is required for the api role outside dev")
	}
	signer, err := auth.NewSigner(priv, cfg.Auth.Issuer)
	if err != nil {
		return nil, err
	}
	verifier, err := signer.VerifierFor()
	if err != nil {
		return nil, err
	}
	var master []byte
	switch {
	case cfg.APIKeyMasterKey.IsSet():
		if master, err = auth.ParseMasterKey(cfg.APIKeyMasterKey.Reveal()); err != nil {
			return nil, err
		}
	case cfg.Env == "dev":
		master = make([]byte, auth.MasterKeyLen)
		if _, err := rand.Read(master); err != nil {
			return nil, fmt.Errorf("auth: ephemeral master key: %w", err)
		}
		log.Warn("API_KEY_MASTER_KEY is empty: API keys created now stop working when the process restarts (dev only)")
	default:
		log.Warn("API_KEY_MASTER_KEY is empty: API keys are disabled")
	}
	return auth.New(d.pool, auth.Config{
		Tenant: cfg.TenantID, Issuer: cfg.Auth.Issuer, AccessTTL: cfg.Auth.AccessTTL, RefreshTTL: cfg.Auth.RefreshTTL, MasterKey: master,
	}, signer, verifier, l, audit.NewRecorder(cfg.TenantID))
}

func parseLimits(c RateLimitConfig) (api.Limits, error) {
	var (
		out api.Limits
		err error
	)
	if out.LoginPerIP, err = ratelimit.ParseLimit(c.LoginPerIP); err != nil {
		return out, fmt.Errorf("config: RATELIMIT_LOGIN_PER_IP: %w", err)
	}
	if out.LoginPerAccount, err = ratelimit.ParseLimit(c.LoginPerAccount); err != nil {
		return out, fmt.Errorf("config: RATELIMIT_LOGIN_PER_ACCOUNT: %w", err)
	}
	if out.OrdersPerAccount, err = ratelimit.ParseLimit(c.OrdersPerAccount); err != nil {
		return out, fmt.Errorf("config: RATELIMIT_ORDERS_PER_ACCOUNT: %w", err)
	}
	return out, nil
}

func recoverer() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					telemetry.Logger(r.Context()).Error("panic in handler", slog.Any("panic", rec))
					api.WriteProblem(w, r, http.StatusInternalServerError, "Internal Server Error", "")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// internalTokenSource mints the aud=internal JWT that travels with every
// command on the NATS bus (docs/plan-v1.0.md §14). The engine trusts this
// token, never the account id in the command body, so the api role signs
// exactly what it authenticated: the caller's own principal.
func internalTokenSource(signer *auth.Signer, tenant string, ttl time.Duration) cmdbus.TokenSource {
	return func(ctx context.Context) (string, error) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return "", errors.New("api: no authenticated caller for an engine command")
		}
		if p.TenantID == "" {
			p.TenantID = tenant
		}
		return signer.Issue(auth.Claims{
			UserID: p.UserID, AccountID: p.AccountID, TenantID: p.TenantID, Role: p.Role,
			Scopes: p.Scopes, Method: p.Method, Audience: auth.AudienceInternal,
		}, time.Now().UTC(), ttl)
	}
}

// registryRefresher keeps the registry cache of a role without an engine
// close to the database. The engine reloads on market.updated, but an api
// role in another container has no consumer of its own (a durable one would
// split the stream between replicas), so it re-reads on a timer instead.
type registryRefresher struct {
	cache    *registry.Cache
	store    registry.Reader
	interval time.Duration
	log      *slog.Logger
}

func (r *registryRefresher) run(ctx context.Context) error {
	if r == nil || r.interval <= 0 {
		<-ctx.Done()
		return nil
	}
	tick := time.NewTicker(r.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := r.cache.Load(ctx, r.store); err != nil && ctx.Err() == nil {
				r.log.Warn("registry refresh failed", slog.String("err", err.Error()))
			}
		}
	}
}
