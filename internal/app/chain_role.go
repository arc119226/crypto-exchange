package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/arc119226/crypto-exchange/internal/chain/deposit"
	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// chainComponents is the chain role: it watches the chain and credits
// deposits (docs/plan-v1.0.md §5.1). It holds no keys — signing arrives with
// withdrawals in 4b and goes to the signer role.
type chainComponents struct {
	client   *evm.Client
	scanner  *deposit.Scanner
	interval time.Duration
	// lastTick records whether the most recent tick succeeded, so readiness
	// reflects the scanner rather than only the RPC connection.
	lastErr error
}

// newChain dials the node and prepares the scanner.
//
// Start failures are fatal by design (§6.4.1 step 5): a scanner pointed at the
// wrong chain, or one whose cursor is ahead of the head, would silently skip
// every deposit in between. Refusing to start is the only honest answer.
func newChain(ctx context.Context, cfg Config, log *slog.Logger, db *pgxpool.Pool, l *ledger.Service, reg prometheus.Registerer) (*chainComponents, error) {
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
	log.Info("scanning for deposits",
		slog.String("rpc", client.LogValue()), slog.Int64("chain_id", cfg.Chain.ChainID),
		slog.Duration("interval", cfg.Chain.ScanInterval))
	return &chainComponents{client: client, scanner: scanner, interval: cfg.Chain.ScanInterval}, nil
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
