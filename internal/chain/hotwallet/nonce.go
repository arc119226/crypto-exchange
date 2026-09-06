// Package hotwallet owns the exchange's outgoing nonce (docs/plan-v1.0.md
// §6.4.2). Every transaction the hot wallet sends — a withdrawal, later a
// sweep's gas funding — takes its nonce from here.
//
// A nonce is not a counter that can be retried. Two transactions with the same
// nonce are a race the chain decides, and a nonce allocated but never spent is
// a hole every later transaction queues behind. So the manager only ever moves
// forward, and closes a hole with a transaction that does nothing rather than
// reusing the number.
package hotwallet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/chain/evm"
	"github.com/arc119226/crypto-exchange/internal/chain/signer"
	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/money"
)

// gasForSelfTransfer is the intrinsic cost of a value transfer with no data.
// A nonce fill is exactly that, so the estimate is exact and asking the node
// for it would only add a way to fail.
const gasForSelfTransfer = 21000

// ErrForeignTransaction is the refusal of §6.4.2: the chain has seen more
// transactions from the hot wallet than this database ever allocated, so
// something else holds the key. Starting anyway would mean allocating nonces
// that the unknown sender is also using.
var ErrForeignTransaction = errors.New("hotwallet: the chain shows transactions this exchange did not send")

// Chain is the part of an EVM node the nonce manager needs. It is an
// interface so the startup rules — which are the easiest thing here to get
// wrong and the hardest to reach with a real node — can be driven from a test
// without Docker.
type Chain interface {
	PendingNonceAt(ctx context.Context, address common.Address) (uint64, error)
	NonceAt(ctx context.Context, address common.Address) (uint64, error)
	SendRawTransaction(ctx context.Context, raw []byte) error
	SuggestFees(ctx context.Context) (evm.Fees, error)
	Receipt(ctx context.Context, txHash string) (*types.Receipt, error)
}

// Manager hands out nonces for one hot wallet on one chain.
type Manager struct {
	db      *pgxpool.Pool
	tenant  string
	chainID int64
	hot     common.Address
	chain   Chain
	signer  signer.Signer
	log     *slog.Logger

	// mu is §6.4.2's "single goroutine", held as a lock: allocation and
	// recycling must not interleave, or a recycled nonce could be handed out
	// twice. The database row lock serialises across processes; this
	// serialises within one.
	mu sync.Mutex
}

// New builds a manager. Start must be called before Allocate.
func New(db *pgxpool.Pool, tenant string, chainID int64, hot common.Address, chain Chain, s signer.Signer, log *slog.Logger) *Manager {
	return &Manager{db: db, tenant: tenant, chainID: chainID, hot: hot, chain: chain, signer: s, log: log}
}

// HotWallet is the address nonces are allocated for.
func (m *Manager) HotWallet() common.Address { return m.hot }

// Start reconciles the stored nonce with the chain (docs/plan-v1.0.md §6.4.2).
//
// Three cases, and only one of them is ordinary:
//
//   - pending == dbNext: everything this exchange sent has been seen. Ready.
//   - pending < dbNext: nonces were allocated that the chain has not seen.
//     Each one either belongs to a withdrawal that is signed or broadcast — the
//     worker will re-send it — or to nothing at all, which is a hole that must
//     be filled before anything later can be mined.
//   - pending > dbNext: the chain has seen transactions from this address that
//     this database never allocated. That means the hot wallet key is in use
//     somewhere else, and continuing would allocate nonces the other sender is
//     also using. Refuse to start and alert.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	pending, err := m.chain.PendingNonceAt(ctx, m.hot)
	if err != nil {
		return err
	}
	row, err := sqlcgen.New(m.db).UpsertHotWallet(ctx, sqlcgen.UpsertHotWalletParams{
		TenantID: m.tenant, ChainID: m.chainID, Address: address(m.hot),
		// A fresh database adopts the chain's count: an exchange pointed at a
		// wallet that has already been used starts from where it actually is,
		// not from zero.
		NextNonce: int64(pending), //nolint:gosec // node nonces are far below 2^63
	})
	if err != nil {
		return fmt.Errorf("hotwallet: load: %w", err)
	}
	if !equalAddress(row.Address, m.hot) {
		// The stored address is another wallet: a different seed, or a
		// different derivation path. Its nonce means nothing here.
		return fmt.Errorf("%w: database holds %s, this signer derives %s",
			ErrForeignTransaction, row.Address, address(m.hot))
	}
	dbNext := uint64(row.NextNonce) //nolint:gosec // CHECKed >= 0

	switch {
	case pending == dbNext:
		m.log.Info("hot wallet nonce reconciled",
			slog.String("address", address(m.hot)), slog.Uint64("next_nonce", dbNext))
		return nil
	case pending > dbNext:
		return fmt.Errorf("%w: the node reports nonce %d, this database allocated up to %d (address %s)",
			ErrForeignTransaction, pending, dbNext, address(m.hot))
	}

	// pending < dbNext: find which of the allocated nonces nothing is holding.
	holders, err := m.holders(ctx)
	if err != nil {
		return err
	}
	for n := pending; n < dbNext; n++ {
		if who, ok := holders[n]; ok {
			m.log.Info("nonce is held by a transaction that will be re-sent",
				slog.Uint64("nonce", n), slog.String("holder", who))
			continue
		}
		if err := m.fill(ctx, n, "startup_gap"); err != nil {
			return fmt.Errorf("hotwallet: fill nonce %d: %w", n, err)
		}
	}
	m.log.Info("hot wallet nonce reconciled after filling gaps",
		slog.Uint64("chain_pending", pending), slog.Uint64("next_nonce", dbNext))
	return nil
}

// holders maps every allocated nonce to what is holding it: a withdrawal that
// has been signed or broadcast, or a fill already sent.
func (m *Manager) holders(ctx context.Context) (map[uint64]string, error) {
	q := sqlcgen.New(m.db)
	out := map[uint64]string{}
	rows, err := q.ListAllocatedNonces(ctx, sqlcgen.ListAllocatedNoncesParams{TenantID: m.tenant, ChainID: m.chainID})
	if err != nil {
		return nil, fmt.Errorf("hotwallet: list allocated nonces: %w", err)
	}
	for _, r := range rows {
		// A withdrawal holds its nonce from the moment the nonce is pinned to
		// it, which happens while it is still funds_locked and before anything
		// is signed. Counting only the signed states would treat that window
		// as a gap and fill a nonce a signature is about to use. One that
		// failed before broadcasting released its nonce, and the hole it left
		// is exactly what this scan is looking for.
		if r.Nonce == nil {
			continue
		}
		switch r.Status {
		case "funds_locked", "signed", "broadcast", "confirmed":
		default:
			continue
		}
		out[uint64(*r.Nonce)] = "withdrawal " + r.ID //nolint:gosec // CHECKed >= 0
	}
	fills, err := q.ListUnconfirmedNonceFills(ctx, sqlcgen.ListUnconfirmedNonceFillsParams{TenantID: m.tenant, ChainID: m.chainID})
	if err != nil {
		return nil, fmt.Errorf("hotwallet: list nonce fills: %w", err)
	}
	for _, f := range fills {
		out[uint64(f.Nonce)] = "nonce fill " + f.TxHash //nolint:gosec // CHECKed >= 0
	}
	return out, nil
}

// Allocate hands out the next nonce inside the caller's transaction, so a
// nonce and the row that will use it are committed together.
func (m *Manager) Allocate(ctx context.Context, tx pgx.Tx) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := sqlcgen.New(tx).AllocateNonce(ctx, sqlcgen.AllocateNonceParams{TenantID: m.tenant, ChainID: m.chainID})
	if err != nil {
		return 0, fmt.Errorf("hotwallet: allocate nonce: %w", err)
	}
	if n < 0 {
		return 0, fmt.Errorf("hotwallet: allocated a negative nonce %d", n)
	}
	return uint64(n), nil
}

// Recycle gives back a nonce whose transaction will never be sent
// (docs/plan-v1.0.md §6.4.2).
//
// If it is the last one allocated, the counter simply steps back — nothing was
// lost. Otherwise later nonces are already in flight and stepping back would
// hand this number out twice, so the hole is filled with a transaction that
// moves nothing instead. That fill costs gas, which is the price of the
// alternative being "every later withdrawal is stuck".
func (m *Manager) Recycle(ctx context.Context, nonce uint64, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, err := sqlcgen.New(m.db).GetHotWallet(ctx, sqlcgen.GetHotWalletParams{TenantID: m.tenant, ChainID: m.chainID})
	if err != nil {
		return fmt.Errorf("hotwallet: load: %w", err)
	}
	dbNext := uint64(row.NextNonce) //nolint:gosec // CHECKed >= 0
	if dbNext == nonce+1 {
		if err := sqlcgen.New(m.db).SetNextNonce(ctx, sqlcgen.SetNextNonceParams{
			TenantID: m.tenant, ChainID: m.chainID, NextNonce: int64(nonce), //nolint:gosec // bounded by dbNext
		}); err != nil {
			return fmt.Errorf("hotwallet: step back to %d: %w", nonce, err)
		}
		m.log.Info("nonce recycled", slog.Uint64("nonce", nonce), slog.String("reason", reason))
		return nil
	}
	return m.fill(ctx, nonce, reason)
}

// fill sends a 0-value self-transfer to consume one nonce.
func (m *Manager) fill(ctx context.Context, nonce uint64, reason string) error {
	fees, err := m.chain.SuggestFees(ctx)
	if err != nil {
		return err
	}
	res, err := m.signer.Sign(ctx, signer.Request{
		Kind: signer.KindNonceFill, RefID: fmt.Sprintf("%d:%d", m.chainID, nonce),
		ChainID: m.chainID, To: m.hot, Asset: "", Value: money.Zero,
		Nonce: nonce, Gas: gasForSelfTransfer, TipCap: fees.TipCap, FeeCap: fees.FeeCap,
	})
	if err != nil {
		return err
	}
	if _, err := sqlcgen.New(m.db).InsertNonceFill(ctx, sqlcgen.InsertNonceFillParams{
		TenantID: m.tenant, ChainID: m.chainID, Nonce: int64(nonce), //nolint:gosec // node nonces are far below 2^63
		TxHash: res.TxHash, Reason: reason,
	}); err != nil {
		return fmt.Errorf("hotwallet: record fill: %w", err)
	}
	// Recorded before sending: a fill that reached the chain but is not in the
	// table would be treated as a gap again on the next start, and filled a
	// second time with a nonce the chain has already consumed.
	if err := m.chain.SendRawTransaction(ctx, res.RawTx); err != nil && !errors.Is(err, evm.ErrKnownTransaction) {
		return fmt.Errorf("hotwallet: broadcast fill for nonce %d: %w", nonce, err)
	}
	m.log.Warn("filled a nonce gap with a self-transfer",
		slog.Uint64("nonce", nonce), slog.String("reason", reason), slog.String("tx_hash", res.TxHash))
	return nil
}

func address(a common.Address) string { return lower(a.Hex()) }

func equalAddress(stored string, a common.Address) bool {
	return common.HexToAddress(stored) == a
}

func lower(s string) string { return strings.ToLower(s) }
