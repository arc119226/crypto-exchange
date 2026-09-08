package hotwallet

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
)

// Description is the hot wallet as the back office shows it: the row the
// chain role keeps, not the chain. What it holds is the ledger's custody_hot
// balance per asset, which the caller reads from the ledger and shows next
// to this; the chain's own figure is what reconciliation compares it to.
type Description struct {
	ChainID   int64
	Address   string
	NextNonce int64
	// LowAlertedAt is when alert.hot_wallet_low was last raised and not yet
	// cleared; nil when the balance is above the line.
	LowAlertedAt *time.Time
	UpdatedAt    time.Time
}

// ErrNoHotWallet means the chain role has never started against this chain:
// the row is written on its first start.
var ErrNoHotWallet = errors.New("hotwallet: no hot wallet recorded for this chain")

// Describe reads the hot wallet row. A read, not a manager: the admin role
// holds no key and allocates no nonce.
func Describe(ctx context.Context, db *pgxpool.Pool, tenant string, chainID int64) (Description, error) {
	row, err := sqlcgen.New(db).GetHotWallet(ctx, sqlcgen.GetHotWalletParams{TenantID: tenant, ChainID: chainID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Description{}, ErrNoHotWallet
	}
	if err != nil {
		return Description{}, fmt.Errorf("hotwallet: describe: %w", err)
	}
	d := Description{ChainID: row.ChainID, Address: row.Address, NextNonce: row.NextNonce, UpdatedAt: row.UpdatedAt}
	if row.LowAlertedAt.Valid {
		t := row.LowAlertedAt.Time
		d.LowAlertedAt = &t
	}
	return d, nil
}
