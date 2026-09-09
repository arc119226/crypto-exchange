package deposit

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/arc119226/crypto-exchange/internal/chain/sqlcgen"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// Record is a deposit as the APIs present it.
type Record struct {
	ID            string
	AccountID     string
	Asset         string
	Amount        money.Amount
	Address       string
	TxHash        string
	LogIndex      int32
	BlockNumber   int64
	Confirmations int32
	Status        string
	// Fee and Credited exist only once the deposit has been credited: the
	// fee comes out of Amount rather than being added to it, so Amount stays
	// what the chain delivered and Credited is what became the balance.
	Fee        *money.Amount
	Credited   *money.Amount
	CreditedAt *time.Time
	CreatedAt  time.Time
}

// Reader lists deposits. It is separate from the Scanner because the api and
// admin roles read this table but must never drive it.
type Reader struct {
	db     *pgxpool.Pool
	tenant string
}

// NewReader binds a reader to a tenant.
func NewReader(db *pgxpool.Pool, tenant string) *Reader { return &Reader{db: db, tenant: tenant} }

// ByAccount returns one account's deposits, newest first.
func (r *Reader) ByAccount(ctx context.Context, accountID string, limit, offset int32) ([]Record, error) {
	rows, err := sqlcgen.New(r.db).ListDepositsByAccount(ctx, sqlcgen.ListDepositsByAccountParams{
		TenantID: r.tenant, AccountID: accountID, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, fmt.Errorf("deposit: list for %s: %w", accountID, err)
	}
	return records(rows)
}

// List returns every deposit, optionally filtered, for the operator API.
func (r *Reader) List(ctx context.Context, status, asset string, limit, offset int32) ([]Record, error) {
	rows, err := sqlcgen.New(r.db).ListDeposits(ctx, sqlcgen.ListDepositsParams{
		TenantID: r.tenant, Status: status, Asset: asset, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, fmt.Errorf("deposit: list: %w", err)
	}
	return records(rows)
}

func records(rows []sqlcgen.ChainDeposit) ([]Record, error) {
	out := make([]Record, 0, len(rows))
	for _, row := range rows {
		amount, err := pg.AmountFromNumeric(row.Amount)
		if err != nil {
			return nil, fmt.Errorf("deposit: amount of %s: %w", row.ID, err)
		}
		fee, err := pg.NullableAmountFromNumeric(row.Fee)
		if err != nil {
			return nil, fmt.Errorf("deposit: fee of %s: %w", row.ID, err)
		}
		credited, err := pg.NullableAmountFromNumeric(row.CreditedAmount)
		if err != nil {
			return nil, fmt.Errorf("deposit: credited amount of %s: %w", row.ID, err)
		}
		rec := Record{
			ID: row.ID, AccountID: row.AccountID, Asset: row.Asset, Amount: amount,
			Address: row.Address, TxHash: row.TxHash, LogIndex: row.LogIndex,
			BlockNumber: row.BlockNumber, Confirmations: row.Confirmations,
			Status: row.Status, Fee: fee, Credited: credited, CreatedAt: row.CreatedAt,
		}
		if row.CreditedAt.Valid {
			at := row.CreditedAt.Time
			rec.CreditedAt = &at
		}
		out = append(out, rec)
	}
	return out, nil
}

// CountInStatus counts the tenant's deposits in any of the statuses.
func (r *Reader) CountInStatus(ctx context.Context, statuses ...string) (int64, error) {
	n, err := sqlcgen.New(r.db).CountDepositsInStatus(ctx, sqlcgen.CountDepositsInStatusParams{
		TenantID: r.tenant, Statuses: statuses,
	})
	if err != nil {
		return 0, fmt.Errorf("deposit: count: %w", err)
	}
	return n, nil
}
