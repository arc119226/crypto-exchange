package deposit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// Deposit statuses (docs/plan-v1.0.md §6.4.1). Only credited and reversed
// touch the ledger.
const (
	StatusDetected   = "detected"
	StatusConfirming = "confirming"
	StatusCredited   = "credited"
	StatusOrphaned   = "orphaned"
	StatusDropped    = "dropped"
)

// NativeLogIndex marks a deposit that arrived as a plain value transfer rather
// than a Transfer log. -1 cannot collide with a real log index, which is what
// makes (chain_id, tx_hash, log_index) a usable idempotency key.
const NativeLogIndex int32 = -1

// ErrChainChanged means the node is not serving the chain this database was
// built against. Continuing would skip every deposit between the old cursor
// and the new chain's head.
var ErrChainChanged = errors.New("deposit: the node is on a different chain than the cursor")

// Config tunes the scanner.
type Config struct {
	Tenant  string
	ChainID int64
	// StartBlock is the last block treated as already scanned on a fresh
	// database, and also the anchor: its hash is recorded on the first start
	// and re-checked on every start after, which is how this database knows
	// it is still looking at the same chain (see verifyAnchor).
	StartBlock uint64
	// BatchSize caps how many blocks one tick scans, so catching up on a long
	// chain cannot hold a transaction open for minutes.
	BatchSize uint64
	// RingDepth is how many recent blocks are remembered, and therefore the
	// deepest reorg that can be resolved without an operator.
	RingDepth uint64
	// OrphanExpiryBlocks is how long an orphaned deposit waits to reappear
	// before it is dropped.
	OrphanExpiryBlocks uint64
	// DefaultConfirmations applies to an asset whose registry row says 0.
	DefaultConfirmations int32
}

func (c Config) withDefaults() Config {
	if c.BatchSize == 0 {
		c.BatchSize = 200
	}
	if c.RingDepth == 0 {
		c.RingDepth = 128
	}
	if c.OrphanExpiryBlocks == 0 {
		c.OrphanExpiryBlocks = 100
	}
	if c.DefaultConfirmations <= 0 {
		c.DefaultConfirmations = 1
	}
	return c
}

// Chain is what the scanner needs from a node. *evm.Client is the real
// implementation; the interface exists so the reorg and confirmation logic —
// the part of this package most likely to be subtly wrong — can be driven
// against a scripted chain without a node.
type Chain interface {
	ChainID(ctx context.Context) (int64, error)
	Head(ctx context.Context) (uint64, error)
	AnchorHash(ctx context.Context, block uint64) (string, error)
	BlockByNumber(ctx context.Context, number uint64) (evm.Block, error)
	TransferLogs(ctx context.Context, from, to uint64, contracts []common.Address) ([]types.Log, error)
	Receipt(ctx context.Context, txHash string) (*types.Receipt, error)
}

var _ Chain = (*evm.Client)(nil)

// Scanner polls an EVM chain and credits deposits to their accounts.
type Scanner struct {
	db       *pgxpool.Pool
	chain    Chain
	ledger   *ledger.Service
	registry registry.Reader
	cfg      Config
	log      *slog.Logger
	metrics  *Metrics

	// watched is the controlled-address set, kept in memory and topped up each
	// tick; addresses are only ever added, never removed.
	watched   map[common.Address]watchedAddress
	watermark string // highest deposit_addresses.id already loaded
	// hot is the wallet the exchange pays out of, nil until the signer has
	// registered one. Transfers from it are the exchange's own money moving,
	// not deposits.
	hot *common.Address
}

type watchedAddress struct {
	id        string
	accountID string
	address   string // lower-case, as stored
}

// New builds a scanner. Call Start before Tick.
func New(db *pgxpool.Pool, chain Chain, l *ledger.Service, r registry.Reader, cfg Config, log *slog.Logger) *Scanner {
	return &Scanner{
		db: db, chain: chain, ledger: l, registry: r, cfg: cfg.withDefaults(), log: log,
		metrics: NewMetrics(nil), watched: map[common.Address]watchedAddress{},
	}
}

// WithMetrics attaches Prometheus collectors.
func (s *Scanner) WithMetrics(m *Metrics) *Scanner {
	if m != nil {
		s.metrics = m
	}
	return s
}

// Start verifies the node is serving the expected chain and prepares the
// cursor. Every failure here is fatal: a scanner that starts against the wrong
// chain silently misses deposits, which is worse than not starting.
func (s *Scanner) Start(ctx context.Context) error {
	id, err := s.chain.ChainID(ctx)
	if err != nil {
		return err
	}
	if id != s.cfg.ChainID {
		return fmt.Errorf("%w: node says chain %d, configured %d", ErrChainChanged, id, s.cfg.ChainID)
	}
	if err := s.verifyAnchor(ctx); err != nil {
		return err
	}

	head, err := s.chain.Head(ctx)
	if err != nil {
		return err
	}
	cursor, err := s.cursor(ctx)
	if err != nil {
		return err
	}
	if cursor > head {
		return fmt.Errorf("%w: cursor is at block %d but the head is %d (run `make reset`)",
			ErrChainChanged, cursor, head)
	}
	return s.refreshAddresses(ctx)
}

// verifyAnchor records, or re-checks, which chain this database belongs to.
//
// The anchor is StartBlock — the block the scanner's view begins at — and its
// hash. Both are stored, and both are compared on every start:
//
//   - a different hash at the same height is a different chain, whatever its
//     chain id says. That is the wiped-anvil case, and refusing to start is
//     the only honest answer: resuming would silently skip every deposit
//     between the old cursor and the new chain's head.
//   - a different height means ETH_SCAN_START_BLOCK moved under a live
//     database. Nothing used to notice, because StartBlock is only read when
//     no cursor exists, so a typo would quietly change what "already scanned"
//     means on the next fresh start. It is refused too, with the recorded
//     value in the message, because the fix is a setting and not a reset.
func (s *Scanner) verifyAnchor(ctx context.Context) error {
	q := sqlcgen.New(s.db)
	state, err := q.GetChainState(ctx, sqlcgen.GetChainStateParams{TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		hash, err := s.chain.AnchorHash(ctx, s.cfg.StartBlock)
		if err != nil {
			return err
		}
		if err := q.InsertChainState(ctx, sqlcgen.InsertChainStateParams{
			TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID,
			AnchorBlock: int64(s.cfg.StartBlock), //nolint:gosec // block numbers are far below 2^63
			AnchorHash:  hash,
		}); err != nil {
			return fmt.Errorf("deposit: record chain state: %w", err)
		}
		s.log.Info("chain recorded", slog.Int64("chain_id", s.cfg.ChainID),
			slog.Uint64("anchor_block", s.cfg.StartBlock), slog.String("anchor_hash", hash))
		return nil
	case err != nil:
		return fmt.Errorf("deposit: read chain state: %w", err)
	}

	recorded := uint64(state.AnchorBlock) //nolint:gosec // CHECKed >= 0
	if recorded != s.cfg.StartBlock {
		return fmt.Errorf("%w: this database is anchored at block %d but ETH_SCAN_START_BLOCK is %d "+
			"(set it back to %d, or clear chain.chain_state if this really is a different chain)",
			ErrChainChanged, recorded, s.cfg.StartBlock, recorded)
	}
	hash, err := s.chain.AnchorHash(ctx, recorded)
	if err != nil {
		return err
	}
	if state.AnchorHash != hash {
		// The usual cause is a wiped anvil volume against a surviving database.
		return fmt.Errorf("%w: block %d hashes to %s, expected %s (run `make reset` to start both over)",
			ErrChainChanged, recorded, hash, state.AnchorHash)
	}
	return nil
}

// Tick advances the scanner by at most one batch. It is safe to call on a
// timer; each call is independent and leaves the cursor consistent.
func (s *Scanner) Tick(ctx context.Context) error {
	head, err := s.chain.Head(ctx)
	if err != nil {
		return err
	}
	s.metrics.observeHead(head)

	if err := s.refreshAddresses(ctx); err != nil {
		return err
	}
	cursor, err := s.cursor(ctx)
	if err != nil {
		return err
	}
	s.metrics.observeScanned(cursor, head-min(cursor, head))

	// The tip is checked on every tick, not only when there is new work: a
	// reorg can replace blocks without changing the height, and one that also
	// shortens the chain would otherwise never be noticed at all.
	if cursor, err = s.reconcileTip(ctx, cursor, head); err != nil {
		return err
	}
	if cursor < head {
		from := cursor + 1
		to := min(from+s.cfg.BatchSize-1, head)
		if err := s.scanRange(ctx, from, to); err != nil {
			return err
		}
	}
	if err := s.advance(ctx, head); err != nil {
		return err
	}
	if err := s.expireOrphans(ctx, head); err != nil {
		return err
	}
	return s.pruneRing(ctx, head)
}

// cursor returns the last scanned block, seeding it on first run.
func (s *Scanner) cursor(ctx context.Context) (uint64, error) {
	row, err := sqlcgen.New(s.db).GetScanCursor(ctx, sqlcgen.GetScanCursorParams{
		TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// StartBlock is the last block treated as already scanned, so scanning
		// begins at StartBlock+1. Zero means "from the beginning".
		return s.cfg.StartBlock, nil
	case err != nil:
		return 0, fmt.Errorf("deposit: read cursor: %w", err)
	}
	return uint64(row.LastScannedBlock), nil //nolint:gosec // the column is CHECKed >= 0
}

// reconcileTip verifies that the block the cursor points at is still on the
// canonical chain, rewinding to the common ancestor when it is not
// (docs/plan-v1.0.md §6.4.1 step 3).
//
// Checking the tip rather than the next block's parent is what makes an
// equal-height reorg visible: replacing block N with a different block N
// leaves the head where it was, so a scanner that only looks when there is
// new work would keep a deposit from the abandoned branch as if it were real.
//
// It returns the block to resume from.
func (s *Scanner) reconcileTip(ctx context.Context, cursor, head uint64) (uint64, error) {
	if cursor == 0 {
		return 0, nil // nothing recorded before the genesis block
	}
	// A chain that got shorter cannot agree with us above its own head.
	tip := min(cursor, head)
	q := sqlcgen.New(s.db)
	stored, err := q.GetBlock(ctx, sqlcgen.GetBlockParams{
		TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID, Number: int64(tip), //nolint:gosec // bounded by the chain head
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return cursor, nil // nothing remembered at that height yet
	case err != nil:
		return 0, fmt.Errorf("deposit: read block %d: %w", tip, err)
	}
	onChain, err := s.chain.BlockByNumber(ctx, tip)
	switch {
	case errors.Is(err, evm.ErrNotFound):
		// the chain does not reach our tip at all; walk back from its head
	case err != nil:
		return 0, err
	case onChain.Hash == stored.Hash && tip == cursor:
		return cursor, nil // still on the same chain
	}

	ancestor, err := s.commonAncestor(ctx, tip)
	if err != nil {
		return 0, err
	}
	s.log.Warn("reorg detected",
		slog.Uint64("cursor", cursor), slog.Uint64("head", head),
		slog.Uint64("common_ancestor", ancestor), slog.String("recorded_hash", stored.Hash))
	s.metrics.reorgs.Inc()

	if err := s.rewind(ctx, ancestor); err != nil {
		return 0, err
	}
	return ancestor, nil
}

// commonAncestor walks back through the ring until a recorded hash matches the
// chain. Falling off the end means the reorg is deeper than RingDepth, which
// is an operator's problem rather than something to guess at.
func (s *Scanner) commonAncestor(ctx context.Context, start uint64) (uint64, error) {
	q := sqlcgen.New(s.db)
	floor := uint64(0)
	if start > s.cfg.RingDepth {
		floor = start - s.cfg.RingDepth
	}
	for n := start; ; n-- {
		row, err := q.GetBlock(ctx, sqlcgen.GetBlockParams{
			TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID, Number: int64(n), //nolint:gosec // bounded by the chain head
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return n, nil // nothing recorded here, so nothing to disagree with
		}
		if err != nil {
			return 0, fmt.Errorf("deposit: read block %d: %w", n, err)
		}
		onChain, err := s.chain.BlockByNumber(ctx, n)
		switch {
		case errors.Is(err, evm.ErrNotFound):
			// the chain is shorter than this height; keep walking back
		case err != nil:
			return 0, err
		case onChain.Hash == row.Hash:
			return n, nil
		}
		if n == floor {
			return 0, fmt.Errorf("deposit: reorg deeper than %d blocks at %d; manual intervention required",
				s.cfg.RingDepth, start)
		}
	}
}

// rewind drops the abandoned branch and orphans the deposits that were on it.
// Credited deposits are left alone: undoing those is the manual reversed path
// (§6.4.1), because the funds may already have been spent.
func (s *Scanner) rewind(ctx context.Context, ancestor uint64) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)
		orphaned, err := q.MarkDepositsOrphaned(ctx, sqlcgen.MarkDepositsOrphanedParams{
			TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID,
			BlockNumber: int64(ancestor + 1), //nolint:gosec // bounded by the chain head
			//nolint:gosec // same
			OrphanedAtBlock: ptrInt64(int64(ancestor)),
		})
		if err != nil {
			return fmt.Errorf("deposit: orphan deposits above %d: %w", ancestor, err)
		}
		for _, d := range orphaned {
			if err := s.emit(ctx, tx, EventOrphaned, d); err != nil {
				return err
			}
		}
		if _, err := q.DeleteBlocksFrom(ctx, sqlcgen.DeleteBlocksFromParams{
			TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID, Number: int64(ancestor + 1), //nolint:gosec // bounded
		}); err != nil {
			return fmt.Errorf("deposit: drop blocks above %d: %w", ancestor, err)
		}
		anc, err := s.chain.BlockByNumber(ctx, ancestor)
		if err != nil {
			return err
		}
		if err := q.UpsertScanCursor(ctx, sqlcgen.UpsertScanCursorParams{
			TenantID: s.cfg.Tenant, ChainID: s.cfg.ChainID,
			LastScannedBlock: int64(ancestor), LastBlockHash: anc.Hash, //nolint:gosec // bounded
		}); err != nil {
			return fmt.Errorf("deposit: rewind cursor: %w", err)
		}
		if len(orphaned) > 0 {
			s.log.Warn("deposits orphaned by a reorg", slog.Int("count", len(orphaned)))
		}
		return nil
	})
}

func ptrInt64(v int64) *int64 { return &v }

func ptrString(s string) *string { return &s }
