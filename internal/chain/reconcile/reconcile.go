// Package reconcile compares what the ledger says the exchange holds on chain
// against what the chain says (docs/plan-v1.0.md §6.4.4).
//
// Per asset the claim is
//
//	custody_deposit_addresses + custody_hot
//	    ==
//	Σ balance of every deposit address + the hot wallet
//
// and the whole difficulty is that the two sides are never looking at the same
// moment. The chain side is pinned to one block; the ledger side is corrected
// for the two ways it can be ahead or behind that block. Both corrections are
// computed exactly -- from rows this system wrote, and from receipts -- so the
// tolerance is zero rather than a fudge factor.
//
// It runs in the chain role. §6.4.4 says worker, but only the chain role dials
// a node, and a reconciler that cannot read a balance is not one. The role that
// displays the result still cannot produce one, which is the same split
// withdrawals already have: admin records intent, chain acts.
//
// The tenant's house balances are summed across every chain, which is correct
// while there is one. A second chain would need custody split per chain before
// this comparison means anything.
package reconcile

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// Chain is what reconciliation needs from a node.
//
// An interface for the same reason deposit, withdrawal and sweep each have
// one: the confirmation arithmetic and the balance races are the parts most
// likely to be subtly wrong, and a scripted chain can produce each of them on
// demand. Every bug this package exists to catch is of that kind.
type Chain interface {
	Head(ctx context.Context) (uint64, error)
	BlockByNumber(ctx context.Context, number uint64) (evm.Block, error)
	BalanceAt(ctx context.Context, address common.Address, block *big.Int) (*big.Int, error)
	TokenBalanceAt(ctx context.Context, token, holder common.Address, block *big.Int) (*big.Int, error)
	Receipt(ctx context.Context, txHash string) (*types.Receipt, error)
}

// Config parameterises the reconciler.
type Config struct {
	Tenant  string
	ChainID int64
	// NativeAsset is the symbol gas is denominated in. Every gas correction
	// lands on this asset's line, whatever the transaction moved.
	NativeAsset string
	// DefaultConfirmations applies to an asset whose registry row says 0. It
	// must be the same value the scanner and the withdrawal worker use, or the
	// frontier this package reads at is not the one they book at.
	DefaultConfirmations int32
	// HotWalletMin is the balance below which alert.hot_wallet_low fires
	// (§6.4.3, spelled HOT_WALLET_MIN_ETH there). Zero disables the alert.
	HotWalletMin money.Amount
}

// Line is one asset's verdict, and every number it was derived from.
//
// The terms are kept because a break is only arguable against them: "custody
// is 3.2 ETH short" is a fact nobody can act on, while "3.2 short, of which
// none is uncredited and none is in flight" says where to look.
type Line struct {
	Asset string `json:"asset"`
	// BlockHeight is where this asset's balances were read.
	BlockHeight int64 `json:"block_height"`
	// LedgerTotal is custody_deposit_addresses + custody_hot for the asset.
	// It can be negative: custody_hot goes negative between a withdrawal and
	// the sweep that refills it, which is the gap 4c-1 exists to close.
	LedgerTotal money.Amount `json:"ledger_total"`
	// ChainTotal is every controlled address plus the hot wallet, at
	// BlockHeight.
	ChainTotal money.Amount `json:"chain_total"`
	// Uncredited is money the chain shows at or below the frontier that the
	// ledger has not credited yet. It makes the chain legitimately higher.
	Uncredited money.Amount `json:"uncredited"`
	// AboveFrontier is the net effect the ledger booked for transactions mined
	// above BlockHeight -- movements the chain read cannot see.
	AboveFrontier money.Amount `json:"above_frontier"`
	// InFlight is what transactions mined at or below the frontier have spent
	// without the ledger having booked them yet.
	InFlight money.Amount `json:"in_flight"`
	// Diff is ChainTotal − LedgerTotal + AboveFrontier − Uncredited + InFlight,
	// and is zero when nothing is wrong.
	Diff money.Amount `json:"diff"`
}

// Broken reports whether this line is a break rather than a balanced line.
func (l Line) Broken() bool { return !l.Diff.IsZero() }

// Report is one pass over every asset.
type Report struct {
	ID         string
	ChainID    int64
	StartedAt  time.Time
	FinishedAt time.Time
	Balanced   bool
	Lines      []Line
}

// ErrNoReport means no pass has been recorded yet: the chain role has not run
// one, or is not deployed.
var ErrNoReport = errors.New("reconcile: no report yet")

// errChainMoved aborts a pass whose blocks changed underneath it. BalanceAt
// takes a number, not a hash, so a reorg part-way through a set of reads would
// silently mix two chains; the pass is dropped rather than reported, because a
// report nobody can trust is worse than no report.
var errChainMoved = errors.New("reconcile: the chain moved while balances were being read")

// errNoCursor and errNoHotWallet are the two rows a pass needs that a
// deployment can legitimately not have yet. Neither is a failure, and both
// used to be reported as one: they were wrapped as ordinary errors, and the
// caller logs any error from Tick at ERROR, every interval, forever.
//
// errNoCursor is the fresh database: the scanner records its cursor on the
// first pass and reconciliation runs on its own clock, so on a new deployment
// it can easily ask first. One tick later there is a cursor.
//
// errNoHotWallet does not go away on its own. chain.hot_wallets is written by
// the nonce manager when the signer first starts, so a deployment that has
// never had a signer never has that row. A pass cannot be completed without
// it -- the hot wallet's balance is part of the chain total, and reporting a
// total with it missing would invent a break out of money that is exactly
// where it should be. So the pass is skipped, and skipping it is said once
// per pass at INFO with what would fix it, not shouted as a failure.
var (
	errNoCursor    = errors.New("reconcile: the scanner has not recorded a cursor yet")
	errNoHotWallet = errors.New("reconcile: no hot wallet is recorded for this chain")
)
