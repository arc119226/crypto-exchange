package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/marketdata"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/stream"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// streamComponents is the stream role: the WebSocket server and the feed
// behind it (docs/plan-v1.0.md §5.1, §7.5). It follows the start-state
// shape of the worker role: newStream only builds, up waits on JetStream,
// and /readyz reports why the role is not up yet.
type streamComponents struct {
	server  *stream.Server
	feed    *stream.Feed
	refresh *registryRefresher
	subs    []*eventbus.OrderedSubscription
	metrics *eventbus.Metrics

	bringUp   func(context.Context) error
	started   chan struct{}
	startMu   sync.Mutex
	startErr  error
	closeOnce sync.Once
}

// newStream builds the role and its HTTP listener.
func newStream(ctx context.Context, cfg Config, log *slog.Logger, reg prometheus.Registerer, d *deps, ebm *eventbus.Metrics, md *marketdata.Metrics) (*streamComponents, *http.Server, error) {
	if d.nc == nil {
		// The feed is the event stream; without NATS there is nothing to
		// serve, and a WebSocket server that never pushes is worse than none.
		return nil, nil, errors.New("config: NATS_URL is required for the stream role")
	}
	cache := registry.NewCache(cfg.TenantID)
	store := registry.NewStore(d.pool)
	if err := retryUntil(ctx, log, "registry", func(ctx context.Context) error { return cache.Load(ctx, store) }); err != nil {
		return nil, nil, err
	}
	verifier, err := streamVerifier(cfg, log)
	if err != nil {
		return nil, nil, err
	}
	var snapshots stream.SnapshotWriter
	if d.rdb != nil {
		snapshots = marketdata.NewSnapshotCache(d.rdb, cfg.TenantID, cfg.MarketData.SnapshotTTL)
	} else {
		log.Warn("REDIS_ADDR is empty: the api role will ask the engine for every depth snapshot")
	}
	m := stream.NewMetrics(reg)
	hub := stream.NewHub(m)
	mdStore := marketdata.NewStore(d.pool, cfg.TenantID)
	scfg := streamConfig(cfg)
	feed := stream.NewFeed(scfg, mdStore, cache, hub, snapshots, m, md, cfg.MarketData.RebuildBuffer, log)
	server := stream.New(ctx, scfg, stream.Deps{
		Tenant: cfg.TenantID, Feed: feed, Hub: hub, Verifier: verifier,
		Outbox: eventbus.NewOutboxReader(d.pool), Accounts: mdStore, Metrics: m, Log: log,
	})
	s := &streamComponents{
		server: server, feed: feed, metrics: ebm,
		refresh:  &registryRefresher{cache: cache, store: store, interval: cfg.Registry.RefreshInterval, log: log},
		started:  make(chan struct{}),
		startErr: errors.New("the stream role has not finished starting"),
	}
	s.bringUp = func(ctx context.Context) error { return s.up(ctx, cfg, log, d.nc) }

	// The listener carries correlation ids and panic recovery but not the
	// per-request latency histogram: a WebSocket request lasts as long as
	// the connection, which would make http_request_duration_seconds say
	// nothing useful. ws_connections is the instrument here.
	r := chi.NewRouter()
	r.Use(telemetry.CorrelationMiddleware(log))
	r.Use(recoverer())
	r.Mount("/", server.Handler())
	srv := &http.Server{Addr: cfg.WSAddr, Handler: r, ReadHeaderTimeout: 5 * time.Second}
	log.Info("serving websockets", slog.String("addr", cfg.WSAddr), slog.Any("allowed_origins", cfg.Stream.AllowedOrigins),
		slog.Int("write_buffer", cfg.Stream.WriteBuffer), slog.Bool("private_channels", verifier != nil))
	return s, srv, nil
}

// streamConfig maps the environment onto the package's configuration.
func streamConfig(cfg Config) stream.Config {
	return stream.Config{
		WriteBuffer: cfg.Stream.WriteBuffer, PingInterval: cfg.Stream.PingInterval, PongTimeout: cfg.Stream.PongTimeout,
		WriteTimeout: cfg.Stream.WriteTimeout, MaxMessageBytes: cfg.Stream.MaxMessageBytes, AuthTimeout: cfg.Stream.AuthTimeout,
		ResumeWindow: cfg.Stream.ResumeWindow, ResumePageSize: cfg.Stream.ResumePageSize, MaxSubscriptions: cfg.Stream.MaxSubscriptions,
		DepthLevels: cfg.Stream.DepthLevels, FlushInterval: cfg.Stream.FlushInterval, TickerInterval: cfg.Stream.TickerInterval,
		KlineInterval: cfg.Stream.KlineInterval, SnapshotInterval: cfg.Stream.SnapshotInterval, AllowedOrigins: cfg.Stream.AllowedOrigins,
	}
}

// streamVerifier is the token check of the private endpoint: the api
// role's JWKS. The same rule as the engine's command bus: outside dev a
// JWKS URL is mandatory; in dev, none means private channels refuse every
// token, loudly.
func streamVerifier(cfg Config, log *slog.Logger) (stream.TokenVerifier, error) {
	if cfg.JWT.JWKSURL != "" {
		v, err := auth.NewRemoteVerifier(cfg.JWT.JWKSURL, cfg.Auth.Issuer)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
	if cfg.Env != "dev" {
		return nil, fmt.Errorf("config: JWT_JWKS_URL is required for the stream role outside dev")
	}
	log.Warn("JWT_JWKS_URL is empty: private channels refuse every token (dev only)")
	return nil, nil //nolint:nilnil // a nil verifier is the documented dev mode
}

// up connects to JetStream and subscribes the feed. The subscriptions exist
// before the feed reads its snapshots, so nothing committed in between is
// missed (docs/events.md "Consumers").
func (s *streamComponents) up(ctx context.Context, cfg Config, log *slog.Logger, nc *nats.Conn) error {
	s.setStartErr(errors.New("connecting to JetStream"))
	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}
	s.setStartErr(errors.New("waiting for the event streams"))
	if err := retryUntil(ctx, log, "jetstream streams", func(ctx context.Context) error {
		return eventbus.EnsureStreams(ctx, js)
	}); err != nil {
		return err
	}
	s.setStartErr(errors.New("subscribing to the event streams"))
	tenant := cfg.TenantID
	for _, sub := range []struct {
		name, stream string
		domains      []string
	}{
		{"stream-trading", eventbus.StreamTrading, []string{"order", "trade", "balance"}},
		{"stream-chain", eventbus.StreamChain, []string{"deposit", "withdrawal"}},
	} {
		filters := make([]string, 0, len(sub.domains))
		for _, d := range sub.domains {
			filters = append(filters, eventbus.SubjectPrefix+"."+d+".*."+tenant+".*")
		}
		os, err := eventbus.SubscribeOrdered(ctx, js, eventbus.OrderedConfig{
			Name: sub.name, Stream: sub.stream, FilterSubjects: filters, Metrics: s.metrics,
		}, log, s.feed.Handle)
		if err != nil {
			s.close()
			return fmt.Errorf("stream consumers: %w", err)
		}
		s.subs = append(s.subs, os)
	}
	s.setStartErr(errors.New("rebuilding the shadow books"))
	s.feed.Start(ctx)
	return nil
}

func (s *streamComponents) start(ctx context.Context) error {
	if err := s.bringUp(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		s.setStartErr(err)
		return err
	}
	s.setStartErr(nil)
	close(s.started)
	return nil
}

func (s *streamComponents) setStartErr(err error) {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	s.startErr = err
}

// ready reports why the role is not up: still starting, or a book still
// rebuilding.
func (s *streamComponents) ready(context.Context) error {
	s.startMu.Lock()
	err := s.startErr
	s.startMu.Unlock()
	if err != nil {
		return fmt.Errorf("still starting: %w", err)
	}
	return s.feed.Ready()
}

// run drives the feed's clocks and the registry refresh.
func (s *streamComponents) run(ctx context.Context) error {
	select {
	case <-s.started:
	case <-ctx.Done():
		return nil
	}
	errc := make(chan error, 1)
	go func() { errc <- s.refresh.run(ctx) }()
	err := s.feed.Run(ctx)
	<-errc
	return err
}

// close is closeWithin with a budget of its own, for the deferred call
// that catches a Run that never reached its shutdown plan.
func (s *streamComponents) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.closeWithin(ctx)
}

// closeWithin stops the consumers and closes every connection (1001 going
// away) within ctx: http.Server.Shutdown does not touch hijacked sockets.
// Idempotent, so the shutdown plan and the deferred close can both call it.
func (s *streamComponents) closeWithin(ctx context.Context) {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		for _, sub := range s.subs {
			sub.Stop()
		}
		s.subs = nil
		s.server.CloseAll(ctx)
	})
}

var _ = (*pgxpool.Pool)(nil)
