package app

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/cmdbus"
	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/trading"
)

// engineComponents is what the engine role runs: the per-market runners, the
// outbox relay, the command bus that serves a separate api role, and the
// consumer that reloads the registry (docs/plan-v1.0.md §5.1, §6.6).
// Everything but the engine itself needs NATS and stays nil without it.
type engineComponents struct {
	engine *trading.Engine
	relay  *eventbus.Relay
	bus    *cmdbus.Server
	reload *eventbus.Subscription
}

// close stops the NATS-attached parts. The engine itself is stopped by Run.
func (e *engineComponents) close(log *slog.Logger) {
	e.reload.Stop()
	if e.bus != nil {
		if err := e.bus.Close(); err != nil {
			log.Warn("command bus close failed", slog.String("err", err.Error()))
		}
	}
}

// attachNATS wires the three things the engine role gets from NATS: the
// outbox relay, the command-bus server that lets an api role in another
// container trade, and the registry reload consumer.
func (e *engineComponents) attachNATS(ctx context.Context, cfg Config, log *slog.Logger, d *deps, reg prometheus.Registerer, ebm *eventbus.Metrics) error {
	js, err := jetstream.New(d.nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}
	if err := retryUntil(ctx, log, "jetstream streams", func(ctx context.Context) error {
		return eventbus.EnsureStreams(ctx, js)
	}); err != nil {
		return err
	}
	e.relay = eventbus.NewRelay(d.pool, eventbus.NewJetStreamPublisher(js), eventbus.RelayConfig{
		PollInterval: cfg.Outbox.PollInterval, BatchSize: cfg.Outbox.BatchSize,
	}, log).WithMetrics(ebm)

	verifier, err := engineVerifier(cfg, log)
	if err != nil {
		return err
	}
	if e.bus, err = cmdbus.Serve(d.nc, e.engine, cmdbus.ServerConfig{
		Tenant: cfg.TenantID, SubjectPrefix: cfg.Engine.CommandSubjectPrefix, Timeout: cfg.Engine.CommandTimeout,
		MaxInFlight: cmp.Or(cfg.Engine.CommandMaxInFlight, cfg.Engine.QueueSize),
		Verifier:    verifier, Metrics: cmdbus.NewMetrics(reg), Logger: log,
	}); err != nil {
		return err
	}
	e.reload, err = newReloadConsumer(ctx, cfg, log, js, e.engine, ebm)
	return err
}

// engineVerifier builds the verifier for the api role's internal tokens.
// Outside dev a JWKS URL is mandatory: an engine that cannot check who sent
// a command must not accept commands from the network at all.
func engineVerifier(cfg Config, log *slog.Logger) (cmdbus.Verifier, error) {
	if cfg.JWT.JWKSURL != "" {
		v, err := auth.NewRemoteVerifier(cfg.JWT.JWKSURL, cfg.Auth.Issuer)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
	if cfg.Env != "dev" {
		return nil, fmt.Errorf("config: JWT_JWKS_URL is required for the engine role outside dev")
	}
	log.Warn("JWT_JWKS_URL is empty: the command bus accepts unsigned commands (dev only)")
	return nil, nil //nolint:nilnil // a nil verifier is the documented dev mode
}

// newEngine builds the trading engine: ledger, registry cache, metrics. It
// does not start it; Run does so it can abort on lock/restore errors.
func newEngine(cfg Config, log *slog.Logger, pool *pgxpool.Pool, l *ledger.Service, reg prometheus.Registerer) *trading.Engine {
	store := registry.NewStore(pool)
	cache := registry.NewCache(cfg.TenantID)
	return trading.NewEngine(pool, l, cache, store, cfg.TenantID, log).
		WithMetrics(trading.NewMetrics(reg)).
		WithQueueSize(cfg.Engine.QueueSize)
}
