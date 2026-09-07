package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/chain/deposit"
	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/hotwallet"
	"github.com/arc119226/crypto-exchange/internal/chain/reconcile"
	"github.com/arc119226/crypto-exchange/internal/chain/signer"
	"github.com/arc119226/crypto-exchange/internal/chain/sweep"
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
	// sweeper collects deposits into the hot wallet. Nil for the same reason
	// sending is, and also when an operator has turned collection off.
	sweeper       *sweep.Worker
	sweepInterval time.Duration
	// reconciler compares ledger custody with on-chain balances (§6.4.4). It
	// needs no signer -- it only reads -- so it keeps running while the signer
	// is down, which is exactly when someone wants to know what is actually
	// there. It does need chain.hot_wallets to have been written once, because
	// the hot wallet's balance is part of the chain total; until then it says
	// so and skips the pass rather than reporting a total it knows is short.
	reconciler        *reconcile.Worker
	reconcileInterval time.Duration
	// lastTick records whether the most recent tick succeeded, so readiness
	// reflects the scanner rather than only the RPC connection.
	lastErr error

	// bringUp is the half of starting that waits on other people's processes.
	// newChain builds it as a closure so the config, pool and registry it
	// needs stay where they were read instead of being copied onto this
	// struct to be used once.
	bringUp func(context.Context) error
	// started is closed when bringUp has succeeded. The tick loops wait on
	// it, which is both an ordering rule and the happens-before edge that
	// makes bringUp's assignments to sending, sweeper and reconciler visible
	// to them.
	started chan struct{}
	// startErr is why the role is not up yet, read by /readyz. A bool could
	// only say "not ready"; the operator needs to know whether it is the node
	// or the signer, and health.CheckResult.Err is a string field waiting for
	// exactly that.
	startMu  sync.Mutex
	startErr error
}

// newChain dials the node and builds the role's parts. It does not talk to
// the chain or the signer beyond the dial: that is start's job, and it is
// deliberately not done here.
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

	store := registry.NewStore(db)
	worker := withdrawal.NewWorker(db, withdrawal.Config{
		Tenant: cfg.TenantID, Batch: cfg.Chain.WithdrawalBatchSize,
		NativeAsset: cfg.Chain.NativeAsset,
	}, store, store, l, withdrawal.NewPostgresKYC(db), policy.Basic{},
		audit.NewRecorder(cfg.TenantID), log).WithMetrics(withdrawal.NewMetrics(reg))

	c := &chainComponents{
		client: client, scanner: scanner, interval: cfg.Chain.ScanInterval,
		withdrawals: worker, withdrawalInterval: cfg.Chain.WithdrawalInterval,
		started:  make(chan struct{}),
		startErr: errors.New("the chain role has not finished starting"),
	}
	c.bringUp = func(ctx context.Context) error {
		return c.up(ctx, cfg, log, db, store, l, reg, nc, local, worker)
	}
	return c, nil
}

// up is everything that waits on somebody else: verifying the chain against
// the recorded anchor, and asking the signer which address it signs from.
// Both can wait indefinitely, so both run after the ops server is listening.
func (c *chainComponents) up(ctx context.Context, cfg Config, log *slog.Logger, db *pgxpool.Pool, store *registry.Store, l *ledger.Service, reg prometheus.Registerer, nc *nats.Conn, local signer.Signer, worker *withdrawal.Worker) error {
	client := c.client
	c.setStartErr(errors.New("connecting to the node and verifying the chain"))
	if err := retryUntil(ctx, log, "chain rpc", func(ctx context.Context) error {
		err := c.scanner.Start(ctx)
		if errors.Is(err, deposit.ErrChainChanged) {
			// Retrying will not fix a different chain; fail out of the loop.
			return retryStop{err}
		}
		if err != nil {
			c.setStartErr(err)
		}
		return err
	}); err != nil {
		var stop retryStop
		if errors.As(err, &stop) {
			return stop.err
		}
		return err
	}
	c.setStartErr(errors.New("waiting for the signer to name its hot wallet"))
	signing, err := attachSigning(ctx, cfg, log, db, worker, client, nc, local)
	if err != nil {
		return err
	}
	if signing == nil {
		log.Warn("no signer reachable: withdrawals will stop at funds_locked and nothing will be collected")
	} else {
		c.sending = worker
		// The sweeper shares the signer, the node and the hot wallet's nonce
		// allocator with the withdrawal worker, because it is the same key
		// paying for the same kind of transaction.
		if cfg.Chain.SweepEnabled {
			maxFee, err := cfg.Chain.MaxFee()
			if err != nil {
				return err
			}
			c.sweeper = sweep.New(db, sweep.Config{
				Tenant: cfg.TenantID, ChainID: cfg.Chain.ChainID, Batch: cfg.Chain.SweepBatchSize,
				NativeAsset: cfg.Chain.NativeAsset, DefaultConfirmations: cfg.Chain.RequiredConfirmations,
				MaxFeePerGas: maxFee,
			}, store, l, client, signing.signer, signing.nonces,
				audit.NewRecorder(cfg.TenantID), log).WithMetrics(sweep.NewMetrics(reg))
			c.sweepInterval = cfg.Chain.SweepInterval
		} else {
			log.Warn("sweeping is off: deposits stay on the addresses they landed on")
		}
	}

	if cfg.Chain.ReconcileEnabled {
		minHot, err := cfg.Chain.MinHotWallet()
		if err != nil {
			return err
		}
		c.reconciler = reconcile.New(db, reconcile.Config{
			Tenant: cfg.TenantID, ChainID: cfg.Chain.ChainID, NativeAsset: cfg.Chain.NativeAsset,
			DefaultConfirmations: cfg.Chain.RequiredConfirmations, HotWalletMin: minHot,
		}, store, l, client, log).WithMetrics(reconcile.NewMetrics(reg))
		c.reconcileInterval = cfg.Chain.ReconcileInterval
	} else {
		log.Warn("reconciliation is off: nothing will compare the ledger with the chain")
	}

	log.Info("scanning for deposits",
		slog.String("rpc", client.LogValue()), slog.Int64("chain_id", cfg.Chain.ChainID),
		slog.Duration("interval", cfg.Chain.ScanInterval))
	if c.sweeper != nil {
		log.Info("collecting deposits into the hot wallet",
			slog.Duration("interval", cfg.Chain.SweepInterval))
	}
	if c.reconciler != nil {
		log.Info("reconciling ledger custody against on-chain balances",
			slog.Duration("interval", cfg.Chain.ReconcileInterval))
	}
	log.Info("driving withdrawals to funds_locked",
		slog.Duration("interval", cfg.Chain.WithdrawalInterval),
		slog.Int("batch", int(cfg.Chain.WithdrawalBatchSize)))
	return nil
}

// start brings the role up and then lets the tick loops run.
//
// It is separate from newChain, and the caller runs it after the ops server
// is listening, because up waits on other people's processes: retryUntil
// backs off to 30s and ends only on success, a retryStop, or ctx. Done inside
// newChain -- as it was until 4d-2b -- that wait happened before anything
// bound a port, so an operator watching a slow RPC got a refused connection
// from /readyz instead of a reason. The engine was moved out of the same spot
// for the same reason (run.go).
//
// Failing is still fatal, as §6.4.1 step 5 requires: a scanner pointed at the
// wrong chain, or one whose cursor is ahead of the head, would silently skip
// every deposit in between. Refusing to start is the only honest answer, and
// returning the error from the errgroup is how that happens now. That only
// became true when retryStop started stopping (4d-2a); before it, this move
// would have turned "hangs forever" into "503s forever", which is not an
// improvement.
func (c *chainComponents) start(ctx context.Context) error {
	if err := c.bringUp(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		c.setStartErr(err)
		return err
	}
	c.setStartErr(nil)
	close(c.started)
	return nil
}

func (c *chainComponents) setStartErr(err error) {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	c.startErr = err
}

// starting reports why the role is not up yet, or nil once it is.
func (c *chainComponents) starting() error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.startErr == nil {
		return nil
	}
	return fmt.Errorf("still starting: %w", c.startErr)
}

// awaitStart blocks a tick loop until the role is up, reporting false when
// ctx ended first. Ticking before then would run against a scanner that has
// not verified the chain, and would read sending, sweeper and reconciler
// while start is still assigning them -- so a deployment that does collect
// could be read as one that does not, once, at the moment it mattered.
func (c *chainComponents) awaitStart(ctx context.Context) bool {
	select {
	case <-c.started:
		return true
	case <-ctx.Done():
		return false
	}
}

// attachSigning gives the worker its signer, nonce manager and send settings.
//
// The signer is whichever exists: the one in this process when a signer role
// runs here, otherwise one reached over NATS. With neither, withdrawals stop
// at funds_locked — which is the correct behaviour for a deployment that was
// never given a way to sign, not a failure to start.
func attachSigning(ctx context.Context, cfg Config, log *slog.Logger, db *pgxpool.Pool, worker *withdrawal.Worker, client *evm.Client, nc *nats.Conn, local signer.Signer) (*signing, error) {
	s := local
	switch {
	case s != nil:
		log.Info("signing in this process")
	case nc != nil:
		c, err := signerbus.NewClient(nc, signerbus.ClientConfig{
			Tenant: cfg.TenantID, SubjectPrefix: cfg.Wallet.SignerSubjectPrefix, Timeout: cfg.Wallet.SignerTimeout,
		})
		if err != nil {
			return nil, err
		}
		s = c
		log.Info("signing requests go to the signer over NATS", slog.String("subject", c.Subject()))
	default:
		return nil, nil //nolint:nilnil // "this deployment cannot sign" is a value, not an error
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
		return nil, fmt.Errorf("chain: the signer would not name its hot wallet: %w", err)
	}
	maxFee, err := cfg.Chain.MaxFee()
	if err != nil {
		return nil, err
	}
	nonces := hotwallet.New(db, cfg.TenantID, cfg.Chain.ChainID, hot, client, s, log).WithMaxFee(maxFee)
	// Fatal by design (§6.4.2): a nonce manager that cannot reconcile with the
	// chain would allocate nonces that collide with transactions already in
	// flight, and ErrForeignTransaction means someone else holds the key. It
	// is the one startup error that must not be retried into submission.
	if err := nonces.Start(ctx); err != nil {
		return nil, err
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
	return &signing{signer: s, nonces: nonces}, nil
}

// signing is what a deployment that can sign has: the signer itself, and the
// hot wallet's nonce allocator. Both the withdrawal worker and the sweeper
// need them, because it is the same key paying for the same kind of
// transaction.
type signing struct {
	signer signer.Signer
	nonces *hotwallet.Manager
}

// run ticks the scanner until ctx ends.
//
// A tick failure is logged and retried rather than fatal: an RPC blip must not
// take the role down, and the next tick resumes from the same cursor because
// every block is committed with it.
func (c *chainComponents) run(ctx context.Context, log *slog.Logger) error {
	if !c.awaitStart(ctx) {
		return nil
	}
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
	if !c.awaitStart(ctx) {
		return nil
	}
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
// runSweeping collects deposits into the hot wallet until ctx ends.
//
// Its own clock, much slower than the other two: nobody is waiting for a
// sweep, and every scan costs one balance call per address per asset.
func (c *chainComponents) runSweeping(ctx context.Context, log *slog.Logger) error {
	// After awaitStart, not before: start is what decides whether there is a
	// sweeper at all.
	if !c.awaitStart(ctx) || c.sweeper == nil {
		return nil
	}
	tick := time.NewTicker(c.sweepInterval)
	defer tick.Stop()
	for {
		if err := c.sweeper.Tick(ctx); err != nil && ctx.Err() == nil {
			log.Error("sweep tick failed", slog.String("err", err.Error()))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// runReconciling compares the ledger with the chain until ctx ends.
//
// The slowest clock in the role, and the only one nobody is waiting on: a pass
// costs a balance call per address per asset, and a difference that appears
// between two of them is not one anybody can act on faster than minutes. A
// failed pass is logged and the next one tries again -- a reconciler that
// stopped the role would turn "we could not check" into "we stopped running",
// which is strictly worse.
func (c *chainComponents) runReconciling(ctx context.Context, log *slog.Logger) error {
	if !c.awaitStart(ctx) || c.reconciler == nil {
		return nil
	}
	tick := time.NewTicker(c.reconcileInterval)
	defer tick.Stop()
	for {
		if err := c.reconciler.Tick(ctx); err != nil && ctx.Err() == nil {
			log.Error("reconciliation pass failed", slog.String("err", err.Error()))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (c *chainComponents) runSending(ctx context.Context, log *slog.Logger) error {
	if !c.awaitStart(ctx) || c.sending == nil {
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
//
// The start check comes first, and not only for ordering: while the role is
// starting there is nothing useful to say about the last scan, and the reason
// it is still starting is the one thing an operator wants. It is also what
// lets /readyz answer at all during startup, which is the whole point of
// running start after the ops server.
func (c *chainComponents) ready(ctx context.Context) error {
	if err := c.starting(); err != nil {
		return err
	}
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
