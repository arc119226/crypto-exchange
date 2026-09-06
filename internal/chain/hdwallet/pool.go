package hdwallet

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
)

// Pool keeps chain.deposit_addresses stocked with unassigned addresses
// (docs/plan-v1.0.md §6.4.1). It lives here, next to the seed, because
// filling the pool is the one operation that needs to derive: the api role
// only ever claims a row that already exists.
type Pool struct {
	db      *pgxpool.Pool
	wallet  *Wallet
	tenant  string
	chainID int64
}

// NewPool binds a refiller to one tenant and chain.
func NewPool(db *pgxpool.Pool, w *Wallet, tenant string, chainID int64) *Pool {
	return &Pool{db: db, wallet: w, tenant: tenant, chainID: chainID}
}

// Ensure derives addresses until at least min are unassigned, and reports how
// many it added. Running it again is harmless: it counts first, and the
// insert is ON CONFLICT DO NOTHING.
//
// It is not safe to run two of these concurrently against one database — they
// would both see the same shortfall and each fill it. The signer role is
// "exactly 1" (§5.1), which is what makes that acceptable; a second instance
// only over-fills the pool, it cannot duplicate an address, because every
// index comes from the sequence.
func (p *Pool) Ensure(ctx context.Context, min int) (int, error) {
	if min <= 0 {
		return 0, nil
	}
	q := sqlcgen.New(p.db)
	free, err := q.CountFreeDepositAddresses(ctx, sqlcgen.CountFreeDepositAddressesParams{
		TenantID: p.tenant, ChainID: p.chainID,
	})
	if err != nil {
		return 0, fmt.Errorf("hdwallet: count free addresses: %w", err)
	}
	created := 0
	for i := int64(free); i < int64(min); i++ {
		index, err := q.NextDepositAddressIndex(ctx)
		if err != nil {
			return created, fmt.Errorf("hdwallet: next derivation index: %w", err)
		}
		if index < 0 || index > int64(MaxDepositIndex) {
			return created, fmt.Errorf("hdwallet: derivation index %d is out of range", index)
		}
		path, err := DepositPath(uint32(index)) //nolint:gosec // bounded just above
		if err != nil {
			return created, err
		}
		addr, err := p.wallet.Address(path)
		if err != nil {
			return created, err
		}
		n, err := q.InsertDepositAddress(ctx, sqlcgen.InsertDepositAddressParams{
			TenantID: p.tenant, ChainID: p.chainID, DerivationIndex: index,
			// lower-case: the column's CHECK requires it, so a case change can
			// never split one address into two rows
			Address: strings.ToLower(addr.Hex()),
		})
		if err != nil {
			return created, fmt.Errorf("hdwallet: insert deposit address: %w", err)
		}
		created += int(n)
	}
	return created, nil
}

// Free reports how many unassigned addresses remain, for the pool-depth
// gauge and the signer's readiness check.
func (p *Pool) Free(ctx context.Context) (int64, error) {
	n, err := sqlcgen.New(p.db).CountFreeDepositAddresses(ctx, sqlcgen.CountFreeDepositAddressesParams{
		TenantID: p.tenant, ChainID: p.chainID,
	})
	if err != nil {
		return 0, fmt.Errorf("hdwallet: count free addresses: %w", err)
	}
	return n, nil
}

// HotWallet returns the address of m/44'/60'/1'/0/0. It is logged on startup
// so an operator can see the signer opened the seed they expected — the
// address is public, the key behind it is not.
func (p *Pool) HotWallet() (string, error) {
	addr, err := p.wallet.Address(HotWalletPath())
	if err != nil {
		return "", err
	}
	return addr.Hex(), nil
}
