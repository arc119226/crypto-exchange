package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"

	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/platform/natsx"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/platform/redisx"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// BuildInfo is injected by the CLI from -ldflags.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

// deps holds the shared infrastructure connections of a process.
type deps struct {
	pool *pgxpool.Pool
	nc   *nats.Conn
	rdb  *redis.Client
}

// Run starts the given roles in one process and blocks until ctx is
// cancelled (SIGTERM) or a component fails. Shutdown order: mark draining →
// wait DrainDelay → stop role listeners → stop ops → drain NATS → close pools.
func Run(ctx context.Context, cfg Config, roles []Role, bi BuildInfo) error {
	level, err := telemetry.ParseLevel(cfg.LogLevel)
	if err != nil {
		return err
	}
	label := RolesLabel(roles)
	log := telemetry.NewLogger(os.Stdout, level, label, bi.Version)
	slog.SetDefault(log)
	reg := telemetry.NewRegistry()
	telemetry.BuildInfo(reg, bi.Version, bi.Commit, label)
	checker := NewChecker()
	log.Info("starting", slog.Any("config", cfg), slog.String("roles", label), slog.String("commit", bi.Commit))

	d, cleanup, err := connectDeps(ctx, cfg, label, log, checker)
	if err != nil {
		return err
	}
	defer cleanup()

	httpMetrics := telemetry.NewHTTPMetrics(reg)
	var (
		servers      []*http.Server
		adminLedger  *ledger.Service
		eng          engineComponents
		sharedLedger *ledger.Service
		apiRefresh   *registryRefresher
	)
	// the ledger service is shared by every role in the process that needs it
	ledgerFor := func() (*ledger.Service, error) {
		if sharedLedger != nil {
			return sharedLedger, nil
		}
		l, err := newLedger(ctx, cfg, log, d.pool, reg)
		if err != nil {
			return nil, err
		}
		sharedLedger = l
		return l, nil
	}
	// the engine is built first so an api role in the same process can use
	// it as the in-process command bus (role=all)
	if hasRole(roles, RoleEngine) {
		l, err := ledgerFor()
		if err != nil {
			return err
		}
		eng.engine = newEngine(cfg, log, d.pool, l, reg)
		if d.nc != nil {
			if err := eng.attachNATS(ctx, cfg, log, d, reg); err != nil {
				return err
			}
			defer eng.close(log)
		} else {
			log.Warn("engine running without NATS: events stay in the outbox, and only an api role in this process can trade")
		}
		checker.Register("engine", true, eng.engine.ReadyCheck)
	}
	for _, role := range roles {
		switch role {
		case RoleAPI:
			l, err := ledgerFor()
			if err != nil {
				return err
			}
			srv, refresh, err := newAPIServer(ctx, cfg, log, httpMetrics, reg, d, l, eng.engine)
			if err != nil {
				return err
			}
			servers = append(servers, srv)
			apiRefresh = refresh
		case RoleEngine:
			// built above
		case RoleAdmin:
			if cfg.Admin.APIKey.Reveal() == "" {
				return fmt.Errorf("config: ADMIN_API_KEY is required for the admin role")
			}
			l, err := ledgerFor()
			if err != nil {
				return err
			}
			adminLedger = l
			servers = append(servers, newAdminServer(cfg, log, httpMetrics, d.pool, l))
		default:
			log.Info("role not implemented yet, serving ops endpoints only", slog.String("role", string(role)))
		}
	}
	ops := &http.Server{Addr: cfg.OpsAddr, Handler: newOpsMux(checker, reg, cfg.Env == "dev"), ReadHeaderTimeout: 5 * time.Second}

	g, gctx := errgroup.WithContext(ctx)
	for _, s := range append(servers, ops) {
		g.Go(listenAndServe(s, log))
	}
	if adminLedger != nil {
		g.Go(func() error { return observeTrialBalance(gctx, log, adminLedger) })
	}
	if apiRefresh != nil {
		g.Go(func() error { return apiRefresh.run(gctx) })
	}
	if eng.engine != nil {
		// Start blocks while another instance holds the lock and while books
		// are rebuilt; the ops server is already up so /readyz reports it.
		g.Go(func() error {
			if err := retryUntil(gctx, log, "engine start", func(ctx context.Context) error {
				return eng.engine.Start(ctx)
			}); err != nil {
				return err
			}
			<-gctx.Done()
			eng.engine.Stop()
			return nil
		})
		if eng.relay != nil {
			g.Go(func() error { return eng.relay.Run(gctx) })
		}
	}
	g.Go(func() error {
		<-gctx.Done()
		return shutdown(cfg, log, checker, servers, ops, d)
	})
	log.Info("ready", slog.String("ops_addr", cfg.OpsAddr))
	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("stopped")
	return nil
}

func connectDeps(ctx context.Context, cfg Config, appName string, log *slog.Logger, checker *Checker) (*deps, func(), error) {
	d := &deps{}
	cleanup := func() {
		if d.rdb != nil {
			_ = d.rdb.Close()
		}
		if d.nc != nil {
			_ = d.nc.Drain()
		}
		if d.pool != nil {
			d.pool.Close()
		}
	}
	err := retryUntil(ctx, log, "postgres", func(ctx context.Context) error {
		pool, err := pg.Open(ctx, pg.PoolConfig{
			DSN: cfg.DB.URL.Reveal(), MaxConns: cfg.DB.MaxConns, ConnectTimeout: cfg.DB.ConnectTimeout, ApplicationName: "exchange-" + appName,
		})
		if err != nil {
			return err
		}
		d.pool = pool
		return nil
	})
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("postgres: %w", err)
	}
	checker.Register("postgres", true, pg.HealthCheck(d.pool))

	if cfg.NATS.URL != "" {
		err := retryUntil(ctx, log, "nats", func(context.Context) error {
			nc, err := natsx.Connect(cfg.NATS.URL, "exchange-"+appName)
			if err != nil {
				return err
			}
			d.nc = nc
			return nil
		})
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("nats: %w", err)
		}
		checker.Register("nats", true, natsx.HealthCheck(d.nc))
	} else {
		log.Warn("NATS_URL is empty; running without the event bus (dev only)")
	}

	if cfg.Redis.Addr != "" {
		d.rdb = redisx.Open(cfg.Redis.Addr, cfg.Redis.Password.Reveal())
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := d.rdb.Ping(pctx).Err(); err != nil {
			log.Warn("redis not reachable at startup; continuing degraded", slog.String("err", err.Error()))
		}
		cancel()
		checker.Register("redis", false, redisx.HealthCheck(d.rdb))
	}
	return d, cleanup, nil
}

func listenAndServe(s *http.Server, log *slog.Logger) func() error {
	return func() error {
		log.Info("listening", slog.String("addr", s.Addr))
		if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("listen %s: %w", s.Addr, err)
		}
		return nil
	}
}

func shutdown(cfg Config, log *slog.Logger, checker *Checker, servers []*http.Server, ops *http.Server, _ *deps) error {
	log.Info("shutting down", slog.Duration("drain_delay", cfg.Shutdown.DrainDelay))
	checker.SetDraining()
	time.Sleep(cfg.Shutdown.DrainDelay)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Shutdown.Timeout)
	defer cancel()
	var firstErr error
	for i := len(servers) - 1; i >= 0; i-- {
		if err := servers[i].Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("shutdown %s: %w", servers[i].Addr, err)
		}
	}
	if err := ops.Shutdown(ctx); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("shutdown ops: %w", err)
	}
	return firstErr
}
