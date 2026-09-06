// Package chain hands pre-generated deposit addresses to accounts
// (docs/plan-v1.0.md §6.4.1).
//
// It deliberately does not import internal/chain/hdwallet, and .golangci.yml
// enforces that: the api role links this package to serve
// GET /v1/deposit-address, and the api role must never be able to derive a
// key. Filling the pool is the signer's job (hdwallet.Pool).
package chain

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
)

// ErrPoolEmpty means every pre-generated address is already assigned. The
// signer refills on a timer, so the caller should return 503 and alert rather
// than fail permanently (§6.4.1).
var ErrPoolEmpty = errors.New("chain: deposit address pool is empty")

// Addresses assigns deposit addresses for one tenant on one chain.
type Addresses struct {
	db      *pgxpool.Pool
	tenant  string
	chainID int64
}

// NewAddresses binds the assigner to a tenant and chain.
func NewAddresses(db *pgxpool.Pool, tenant string, chainID int64) *Addresses {
	return &Addresses{db: db, tenant: tenant, chainID: chainID}
}

// ChainID reports the chain these addresses belong to.
func (a *Addresses) ChainID() int64 { return a.chainID }

// Assign returns the account's deposit address, claiming a free row from the
// pool the first time it is called. It is idempotent: one address per account
// per chain, shared by the native coin and every ERC-20 on that chain.
//
// The returned address is EIP-55 checksummed; the column holds it lower-case
// so no join or unique index can be defeated by a change of case.
func (a *Addresses) Assign(ctx context.Context, accountID string) (string, error) {
	q := sqlcgen.New(a.db)
	if addr, err := a.existing(ctx, q, accountID); err != nil || addr != "" {
		return addr, err
	}
	row, err := q.ClaimDepositAddress(ctx, sqlcgen.ClaimDepositAddressParams{
		TenantID: a.tenant, ChainID: a.chainID, AccountID: &accountID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", ErrPoolEmpty
	case isUniqueViolation(err):
		// Two concurrent first-time calls for one account: the unique index
		// on (tenant, chain, account_id) let exactly one through and left the
		// pool untouched for the other. Read back the winner's address.
		addr, err := a.existing(ctx, q, accountID)
		if err != nil {
			return "", err
		}
		if addr == "" {
			return "", errors.New("chain: assignment lost a race but no address exists")
		}
		return addr, nil
	case err != nil:
		return "", fmt.Errorf("chain: claim deposit address: %w", err)
	}
	return checksummed(row.Address), nil
}

// Free reports how many unassigned addresses are left, for the signer's
// refill loop and the pool-depth metric.
func (a *Addresses) Free(ctx context.Context) (int64, error) {
	n, err := sqlcgen.New(a.db).CountFreeDepositAddresses(ctx, sqlcgen.CountFreeDepositAddressesParams{
		TenantID: a.tenant, ChainID: a.chainID,
	})
	if err != nil {
		return 0, fmt.Errorf("chain: count free addresses: %w", err)
	}
	return n, nil
}

// existing returns the account's address, or "" when it has none yet.
func (a *Addresses) existing(ctx context.Context, q *sqlcgen.Queries, accountID string) (string, error) {
	row, err := q.GetDepositAddressByAccount(ctx, sqlcgen.GetDepositAddressByAccountParams{
		TenantID: a.tenant, ChainID: a.chainID, AccountID: &accountID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("chain: look up deposit address: %w", err)
	}
	return checksummed(row.Address), nil
}

func checksummed(address string) string { return common.HexToAddress(address).Hex() }

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
