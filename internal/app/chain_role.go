package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/deposit"
	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/hotwallet"
	"github.com/arc119226/crypto-exchange/internal/chain/signer"
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/policy"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/signerbus"
)

// chainComponents is the chain role: it watches the chain and credits
// deposits (docs/plan-v1.0.md §5.1). It holds no keys — signing arrives with
// withdrawals in 4b and goes to the signer role.
type chainComponents struct {
	client   *evm.Client
	scanner  *deposit.Scanner
	interval time.Duration
	// withdrawals drives requested -> funds_locked. It runs on its own clock:
	// scanning follows the chain's block time, a withdrawal only waits on a
	// policy decision and a ledger write.
	withdrawals        *withdrawal.Worker
	withdrawalInterval time.Duration
	// sending is nil when this deployment has no signer to reach, which
	// leaves withdrawals stopping at funds_locked.
	sending *withdrawal.Worker
	// lastTick records whether the most recent tick succeeded, so readiness
	// reflects the scanner rather than only the RPC connection.
	lastErr error
}

// newChain dials the node and prepares the scanner.
//
// Start failures are fatal by design (§6.4.1 step 5): a scanner pointed at the
// wrong chain, or one whose cursor is ahead of the head, would silently skip
// every deposit in between. Refusing to start is the only honest answer.
func newChain(ctx context.Context, cfg Config, log *slog.Logger, db *pgxpool.Pool, l *ledger.Service, reg prometheus.Registerer, nc *nats.Conn, local signer.Signer) (*chainComponents, error) {
	if cfg.Chain.RPCURL == "" {
		return nil, errors.New("config: ETH_RPC_URL is required for the chain role")
	}
	client, err := evm.Dial(ctx, cfg.Chain.RPCURL)
	if err != nil {
		return nil, err
	}
	scanner := deposit.New(db, client, l, registry.NewStore(db), deposit.Config{
		Tenant: cfg.TenantID, ChainID: cfg.Chain.ChainID, StartBlock: cfg.Chain.ScanStartBlock,
		BatchSize: cfg.Chain.ScanBatchSize, RingDepth: cfg.Chain.BlockRingDepth,
		OrphanExpiryBlocks:   cfg.Chain.OrphanExpiryBlocks,
		DefaultConfirmations: cfg.Chain.RequiredConfirmations,
	}, log).WithMetrics(deposit.NewMetrics(reg))

	if err := retryUntil(ctx, log, "chain rpc", func(ctx context.Context) error {
		err := scanner.Start(ctx)
		if errors.Is(err, deposit.ErrChainChanged) {
			// Retrying will not fix a different chain; fail out of the loop.
			return retryStop{err}
		}
		return err
	}); err != nil {
		client.Close()
		var stop retryStop
		if errors.As(err, &stop) {
			return nil, stop.err
		}
		return nil, err
	}
	store := registry.NewStore(db)
	worker := withdrawal.NewWorker(db, withdrawal.Config{
		Tenant: cfg.TenantID, Batch: cfg.Chain.WithdrawalBatchSize,
	}, store, store, l, withdrawal.NewPostgresKYC(db), policy.Basic{},
		audit.NewRecorder(cfg.TenantID), log).WithMetrics(withdrawal.NewMetrics(reg))

	c := &chainComponents{
		client: client, scanner: scanner, interval: cfg.Chain.ScanInterval,
		withdrawals: worker, withdrawalInterval: cfg.Chain.WithdrawalInterval,
	}
	if err := attachSigning(ctx, cfg, log, db, worker, client, nc, local); err != nil {
		client.Close()
		return nil, err
	}
	if !worker.Sends() {
		log.Warn("no signer reachable: withdrawals will stop at funds_locked")
	} else {
		c.sending = worker
	}

	log.Info("scanning for deposits",
		slog.String("rpc", client.LogValue()), slog.Int64("chain_id", cfg.Chain.ChainID),
		slog.Duration("interval", cfg.Chain.ScanInterval))
	log.Info("driving withdrawals to funds_locked",
		slog.Duration("interval", cfg.Chain.WithdrawalInterval),
		slog.Int("batch", int(cfg.Chain.WithdrawalBatchSize)))
	return c, nil
}

// attachSigning gives the worker its signer, nonce manager and send settings.
//
// The signer is whichever exists: the one in this process when a signer role
// runs here, otherwise one reached over NATS. With neither, withdrawals stop
// at funds_locked — which is the correct behaviour for a deployment that was
// never given a way to sign, not a failure to start.
func attachSigning(ctx context.Context, cfg Config, log *slog.Logger, db *pgxpool.Pool, worker *withdrawal.Worker, client *evm.Client, nc *nats.Conn, local signer.Signer) error {
	s := local
	switch {
	case s != nil:
		log.Info("signing in this process")
	case nc != nil:
		c, err := signerbus.NewClient(nc, signerbus.ClientConfig{
			Tenant: cfg.TenantID, SubjectPrefix: cfg.Wallet.SignerSubjectPrefix, Timeout: cfg.Wallet.SignerTimeout,
		})
		if err != nil {
			return err
		}
		s = c
		log.Info("signing requests go to the signer over NATS", slog.String("subject", c.Subject()))
	default:
		return nil
	}
	// Ask the signer which address it signs from. Configuring it here instead
	// would let a wrong value track nonces for one address while another sent
	// the transactions.
	//
	// Retried rather than fatal: over NATS this is a request to another
	// process that may still be starting, and a chain role that refuses to
	// come up because the signer was a second late is a worse failure than
	// waiting for it.
	var hot common.Address
	if err := retryUntil(ctx, log, "signer hot wallet", func(ctx context.Context) error {
		var err error
		hot, err = s.HotWallet(ctx)
		return err
	}); err != nil {
		return fmt.Errorf("chain: the signer would not name its hot wallet: %w", err)
	}
	maxFee, err := cfg.Chain.MaxFee()
	if err != nil {
		return err
	}
	nonces := hotwallet.New(db, cfg.TenantID, cfg.Chain.ChainID, hot, client, s, log)
	// Fatal by design (§6.4.2): a nonce manager that cannot reconcile with the
	// chain would allocate nonces that collide with transactions already in
	// flight, and ErrForeignTransaction means someone else holds the key. It
	// is the one startup error that must not be retried into submission.
	if err := nonces.Start(ctx); err != nil {
		return err
	}
	worker.WithSending(client, s, nonces, withdrawal.SendConfig{
		ChainID: cfg.Chain.ChainID, ReplaceAfter: cfg.Chain.ReplaceAfter,
		MaxReplacements: cfg.Chain.MaxReplacements, MaxFeePerGas: maxFee,
		DefaultConfirmations: cfg.Chain.RequiredConfirmations,
	})
	log.Info("withdrawals will be signed and broadcast",
		slog.String("hot_wallet", strings.ToLower(hot.Hex())),
		slog.Duration("replace_after", cfg.Chain.ReplaceAfter),
		slog.Int("max_replacements", int(cfg.Chain.MaxReplacements)))
	return nil
}

// retryStop wraps an error that must end a retry loop rather than be retried.
type retryStop struct{ err error }

func (r retryStop) Error() string { return r.err.Error() }
func (r retryStop) Unwrap() error { return r.err }

// run ticks the scanner until ctx ends.
//
// A tick failure is logged and retried rather than fatal: an RPC blip must not
// take the role down, and the next tick resumes from the same cursor because
// every block is committed with it.
func (c *chainComponents) run(ctx context.Context, log *slog.Logger) error {
	tick := time.NewTicker(c.interval)
	defer tick.Stop()
	for {
		if err := c.scanner.Tick(ctx); err != nil {
			c.lastErr = err
			if ctx.Err() == nil {
				log.Error("deposit scan failed", slog.String("err", err.Error()))
			}
		} else {
			c.lastErr = nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// runWithdrawals ticks the withdrawal worker until ctx ends.
//
// It is a second goroutine rather than a step inside the scan loop because a
// node outage must not stop withdrawals from being decided and their funds
// locked: none of that touches the chain, and a user whose withdrawal is stuck
// in `requested` because an RPC endpoint is down has been failed twice.
func (c *chainComponents) runWithdrawals(ctx context.Context, log *slog.Logger) error {
	tick := time.NewTicker(c.withdrawalInterval)
	defer tick.Stop()
	for {
		if err := c.withdrawals.Tick(ctx); err != nil && ctx.Err() == nil {
			// Already logged per withdrawal by the worker; this is the
			// batch-level failure (claiming the queue at all).
			log.Error("withdrawal tick failed", slog.String("err", err.Error()))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// runSending ticks the send half until ctx ends.
//
// A third loop rather than a step in runWithdrawals: signing and broadcasting
// depend on the signer and the node, while deciding and locking depend on
// neither. Sharing a loop would let an unreachable signer stop withdrawals
// from being decided at all.
func (c *chainComponents) runSending(ctx context.Context, log *slog.Logger) error {
	if c.sending == nil {
		return nil
	}
	tick := time.NewTicker(c.withdrawalInterval)
	defer tick.Stop()
	for {
		if err := c.sending.Send(ctx); err != nil && ctx.Err() == nil {
			log.Error("withdrawal send tick failed", slog.String("err", err.Error()))
		}
		// Operator resolutions run on the same clock. They are recorded by the
		// admin role, which has no node and no key, so this is the only place
		// they can actually happen.
		if err := c.sending.ApplyResolutions(ctx); err != nil && ctx.Err() == nil {
			log.Error("withdrawal resolve tick failed", slog.String("err", err.Error()))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// ready reports the node is reachable and the last scan succeeded.
func (c *chainComponents) ready(ctx context.Context) error {
	if _, err := c.client.Head(ctx); err != nil {
		return err
	}
	if c.lastErr != nil {
		return fmt.Errorf("last scan failed: %w", c.lastErr)
	}
	return nil
}

func (c *chainComponents) close() {
	if c != nil && c.client != nil {
		c.client.Close()
	}
}
