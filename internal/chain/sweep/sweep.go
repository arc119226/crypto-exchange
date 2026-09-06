// Package sweep collects deposits into the hot wallet (docs/plan-v1.0.md
// §6.4.3, §6.1.4 f).
//
// Deposits land on one address per account and withdrawals go out of the hot
// wallet, so without this the two halves never meet: since 4b-2 the hot wallet
// has been paying out money that arrived somewhere else, and custody:hot has
// been going more negative with every withdrawal. Sweeping is the transfer
// that closes that gap.
//
// Nothing here touches a user balance. A sweep moves value between two house
// accounts and books the gas; the user whose deposit is swept sees no change
// at all, which is the invariant the tests assert on every path.
package sweep

import (
	"context"
	"errors"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// Statuses (docs/plan-v1.0.md §6.4.3).
const (
	StatusRequested = "requested"
	// StatusGasFunded is reached only by a token sweep: the address has been
	// sent enough ether to pay for its own transfer.
	StatusGasFunded = "gas_funded"
	StatusBroadcast = "broadcast"
	StatusConfirmed = "confirmed"
	StatusFailed    = "failed"
)

// Failure reasons stored in failure_reason.
const (
	// FailureGasFunding is a funding transaction the chain rejected or
	// reverted. Nothing has left the deposit address, so the sweep is
	// abandoned and the next tick starts a fresh one.
	FailureGasFunding = "gas_funding"
	// FailureBroadcast is a sweep the node refused.
	FailureBroadcast = "broadcast"
	// FailureOnChain is a sweep that was mined and reverted.
	FailureOnChain = "on_chain"
	// FailureBalanceChanged is a sweep whose plan no longer fits what the
	// address holds — the fee market moved, or a deposit arrived between
	// planning and signing. Abandoned rather than adjusted: nothing has been
	// sent, and the next tick sees the current numbers.
	FailureBalanceChanged = "balance_changed"
)

// gasForNativeTransfer is the intrinsic cost of a plain value transfer, which
// is exactly what a native sweep and a gas funding both are.
const gasForNativeTransfer = 21000

// ErrNotFound is an unknown sweep id.
var ErrNotFound = errors.New("sweep: not found")

// errUnreadable marks an asset whose on-chain balance could not be read: a
// registry row naming a contract address that answers nothing, or a node that
// would not say. It is internal because callers do not act on it -- only the
// scan does, by leaving that asset alone for the tick.
var errUnreadable = errors.New("sweep: balance unreadable")

// Chain is what the sweeper needs from a node. An interface for the same
// reason the deposit and withdrawal packages have one: the two-step token
// path and the balance races are the parts most likely to be wrong, and a
// scripted chain can produce each of them on demand.
type Chain interface {
	Head(ctx context.Context) (uint64, error)
	Balance(ctx context.Context, address common.Address) (*big.Int, error)
	TokenBalance(ctx context.Context, token, holder common.Address) (*big.Int, error)
	PendingNonceAt(ctx context.Context, address common.Address) (uint64, error)
	SuggestFees(ctx context.Context) (evm.Fees, error)
	EstimateGas(ctx context.Context, from, to common.Address, value *big.Int, data []byte) (uint64, error)
	SendRawTransaction(ctx context.Context, raw []byte) error
	Receipt(ctx context.Context, txHash string) (*types.Receipt, error)
}

// Nonces is the hot wallet's nonce allocator (internal/chain/hotwallet). Only
// the gas-funding leg uses it: a sweep is sent by the deposit address, whose
// nonce comes straight from the node because the sweeper is that address's
// only user (§6.4.3).
type Nonces interface {
	HotWallet() common.Address
	Allocate(ctx context.Context, tx pgx.Tx) (uint64, error)
	Recycle(ctx context.Context, nonce uint64, reason string) error
}

// Config parameterises the sweeper.
type Config struct {
	Tenant  string
	ChainID int64
	// Batch is how many sweeps one tick advances, and how many addresses one
	// scan looks at.
	Batch int32
	// NativeAsset is the symbol gas is denominated in. Gas is always paid in
	// the chain's own coin, whatever the sweep moved.
	NativeAsset string
	// DefaultConfirmations applies to an asset whose registry row says 0.
	DefaultConfirmations int32
	// MaxFeePerGas is the operator's ceiling on the fee market, shared with
	// withdrawals; zero means no ceiling.
	MaxFeePerGas *big.Int
}

// Record is one sweep as the admin API and the CLI see it.
type Record struct {
	ID          string
	ChainID     int64
	FromAddress string
	Asset       string
	Amount      money.Amount
	Status      string
	// FailureReason is set only for StatusFailed.
	FailureReason string
	TxHash        string
	// GasFundingTxHash is the ether the hot wallet sent so the address could
	// pay for its own transfer; empty for a native sweep.
	GasFundingTxHash string
	// GasCost is what the sweep transaction itself burned, in NativeAsset.
	GasCost     money.Amount
	BlockNumber int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func recordFrom(row sqlcgen.ChainSweep) (Record, error) {
	amount, err := pg.AmountFromNumeric(row.Amount)
	if err != nil {
		return Record{}, err
	}
	rec := Record{
		ID: row.ID, ChainID: row.ChainID, FromAddress: row.FromAddress, Asset: row.Asset,
		Amount: amount, Status: row.Status, FailureReason: deref(row.FailureReason),
		TxHash: deref(row.TxHash), GasFundingTxHash: deref(row.GasFundingTxHash),
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	if row.GasCost.Valid {
		if rec.GasCost, err = pg.AmountFromNumeric(row.GasCost); err != nil {
			return Record{}, err
		}
	}
	if row.BlockNumber != nil {
		rec.BlockNumber = *row.BlockNumber
	}
	return rec, nil
}

func records(rows []sqlcgen.ChainSweep) ([]Record, error) {
	out := make([]Record, 0, len(rows))
	for _, r := range rows {
		rec, err := recordFrom(r)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func int64Ptr(v uint64) *int64 {
	n := int64(v) //nolint:gosec // nonces and block numbers are far below 2^63
	return &n
}
