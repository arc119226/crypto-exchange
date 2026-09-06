package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/hdwallet"
	"github.com/arc119226/crypto-exchange/internal/chain/signer"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/signerbus"
)

// signerComponents is the signer role: the one process that holds the HD seed
// (docs/plan-v1.0.md §5.1, ADR-0007). In Phase 4a its only job is to keep the
// deposit address pool stocked; signing arrives with withdrawals in 4b.
type signerComponents struct {
	wallet   *hdwallet.Wallet
	pool     *hdwallet.Pool
	min      int
	interval time.Duration
	metrics  *signerMetrics
	// keystore is the Signer implementation. A chain role in the same process
	// uses it directly; a split deployment reaches it over NATS through bus.
	keystore *signer.KeystoreSigner
	bus      *signerbus.Server
}

type signerMetrics struct {
	poolFree    prometheus.Gauge
	poolDerived prometheus.Counter
}

func newSignerMetrics(reg prometheus.Registerer) *signerMetrics {
	m := &signerMetrics{
		poolFree: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "deposit_address_pool_free",
			Help: "Unassigned deposit addresses left in the pool.",
		}),
		poolDerived: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "deposit_addresses_derived_total",
			Help: "Deposit addresses derived by the signer since start.",
		}),
	}
	reg.MustRegister(m.poolFree, m.poolDerived)
	return m
}

// newSigner opens the keystore and prepares the pool refiller.
//
// Every failure here is fatal. A signer that starts without its seed is worse
// than one that does not start: the pool silently stops refilling, and the
// first user to ask for a deposit address after the pool drains gets a 503
// with nothing in the logs pointing at the cause.
func newSigner(cfg Config, log *slog.Logger, db *pgxpool.Pool, reg prometheus.Registerer, nc *nats.Conn) (*signerComponents, error) {
	if cfg.Wallet.KeystoreDir == "" {
		return nil, errors.New("config: WALLET_KEYSTORE_DIR is required for the signer role")
	}
	if !cfg.Wallet.Passphrase.IsSet() {
		return nil, errors.New("config: WALLET_KEYSTORE_PASSPHRASE is required for the signer role")
	}
	w, err := hdwallet.Load(cfg.Wallet.KeystoreDir, cfg.Wallet.Passphrase.Reveal())
	if err != nil {
		return nil, fmt.Errorf("signer: %w (run `exchange keys import-mnemonic`)", err)
	}
	s := &signerComponents{
		wallet:   w,
		pool:     hdwallet.NewPool(db, w, cfg.TenantID, cfg.Chain.ChainID),
		min:      cfg.Wallet.AddressPoolMin,
		interval: cfg.Wallet.AddressPoolInterval,
		metrics:  newSignerMetrics(reg),
	}
	hot, err := s.pool.HotWallet()
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("signer: %w", err)
	}
	// The address is public and its key is not. Logging it lets an operator
	// confirm the signer opened the seed they meant to install: it must match
	// HOT_WALLET_ADDRESS, which scripts/gen-dev-secrets.sh derives separately
	// with `cast wallet address`.
	ks, err := signer.NewKeystoreSigner(db, cfg.TenantID, cfg.Chain.ChainID, w,
		registry.NewStore(db), audit.NewRecorder(cfg.TenantID), log)
	if err != nil {
		w.Close()
		return nil, err
	}
	s.keystore = ks
	// Without NATS the signer can only serve a chain role in this same
	// process. That is the single-binary case, not a broken one, so it is a
	// warning rather than a refusal.
	if nc != nil {
		bus, err := signerbus.Serve(nc, ks, cfg.TenantID, cfg.Wallet.SignerSubjectPrefix, log)
		if err != nil {
			w.Close()
			return nil, err
		}
		s.bus = bus
		log.Info("signer answering on NATS", slog.String("subject", bus.Subject()))
	} else {
		log.Warn("signer running without NATS: only a chain role in this process can reach it")
	}
	log.Info("signer keystore opened",
		slog.String("hot_wallet", hot),
		slog.Int64("chain_id", cfg.Chain.ChainID),
		slog.Int("address_pool_min", s.min))
	return s, nil
}

// run tops the pool up now and then on every tick, until ctx ends.
func (s *signerComponents) run(ctx context.Context, log *slog.Logger) error {
	tick := time.NewTicker(s.interval)
	defer tick.Stop()
	for {
		s.refill(ctx, log)
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// refill is best-effort: a database blip must not take the signer down, and
// the next tick retries. The gauge is what an operator watches.
func (s *signerComponents) refill(ctx context.Context, log *slog.Logger) {
	created, err := s.pool.Ensure(ctx, s.min)
	if created > 0 {
		s.metrics.poolDerived.Add(float64(created))
		log.Info("derived deposit addresses", slog.Int("count", created))
	}
	if err != nil {
		if ctx.Err() == nil {
			log.Error("deposit address pool refill failed", slog.String("err", err.Error()))
		}
		return
	}
	free, err := s.pool.Free(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("deposit address pool depth unavailable", slog.String("err", err.Error()))
		}
		return
	}
	s.metrics.poolFree.Set(float64(free))
}

// ready reports the keystore is open and the pool is reachable. It does not
// require the pool to be full: refilling is asynchronous, and a signer that
// reports unready while it catches up would fail its container healthcheck
// on every restart.
func (s *signerComponents) ready(ctx context.Context) error {
	if s.wallet == nil {
		return errors.New("keystore not loaded")
	}
	if _, err := s.pool.Free(ctx); err != nil {
		return err
	}
	return nil
}

func (s *signerComponents) close() {
	if s == nil {
		return
	}
	if s.bus != nil {
		_ = s.bus.Close()
	}
	if s.wallet != nil {
		s.wallet.Close()
	}
}

// localSigner is the in-process Signer, or nil when this process runs no
// signer role. A nil receiver is deliberate: the caller passes whatever the
// role loop produced without having to check first.
func (s *signerComponents) localSigner() signer.Signer {
	if s == nil || s.keystore == nil {
		return nil
	}
	return s.keystore
}
