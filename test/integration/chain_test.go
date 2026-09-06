//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"

	"github.com/arc119226/crypto-exchange/internal/chain"
	"github.com/arc119226/crypto-exchange/internal/chain/hdwallet"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// testChainID matches the anvil chain the compose stack runs.
const testChainID = 31337

// A standard BIP-39 vector, and deliberately not anvil's: hdwallet.Encrypt
// refuses that one, and a test seed whose keys are published everywhere is a
// bad habit to build even where it cannot cost anything.
const testMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

type chainHarness struct {
	ledgerHarness
	addresses *chain.Addresses
	pool      *hdwallet.Pool
}

func setupChain(t *testing.T) chainHarness {
	t.Helper()
	lh := setupLedger(t)
	w, err := hdwallet.FromMnemonic(testMnemonic)
	require.NoError(t, err)
	t.Cleanup(w.Close)
	return chainHarness{
		ledgerHarness: lh,
		addresses:     chain.NewAddresses(lh.all, "default", testChainID),
		pool:          hdwallet.NewPool(lh.all, w, "default", testChainID),
	}
}

func TestDepositAddressPool(t *testing.T) {
	h := setupChain(t)
	ctx := context.Background()

	t.Run("Ensure fills the pool and is idempotent", func(t *testing.T) {
		created, err := h.pool.Ensure(ctx, 5)
		require.NoError(t, err)
		assert.Equal(t, 5, created)

		free, err := h.addresses.Free(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(5), free)

		created, err = h.pool.Ensure(ctx, 5)
		require.NoError(t, err)
		assert.Equal(t, 0, created, "the pool is already at the minimum")

		created, err = h.pool.Ensure(ctx, 8)
		require.NoError(t, err)
		assert.Equal(t, 3, created, "only the shortfall is derived")
	})

	t.Run("assignment is idempotent per account", func(t *testing.T) {
		account := h.newSpot(t, ctx)
		first, err := h.addresses.Assign(ctx, account)
		require.NoError(t, err)
		assert.Regexp(t, `^0x[0-9a-fA-F]{40}$`, first)

		for range 3 {
			again, err := h.addresses.Assign(ctx, account)
			require.NoError(t, err)
			assert.Equal(t, first, again, "one address per account per chain")
		}
	})

	t.Run("the API form is EIP-55 while the column is lower-case", func(t *testing.T) {
		account := h.newSpot(t, ctx)
		got, err := h.addresses.Assign(ctx, account)
		require.NoError(t, err)

		var stored string
		require.NoError(t, h.all.QueryRow(ctx,
			`SELECT address FROM chain.deposit_addresses WHERE account_id = $1`, account).Scan(&stored))
		assert.Equal(t, strings.ToLower(got), stored, "stored lower-case, served checksummed")
		assert.NotEqual(t, stored, got, "a real EIP-55 address has upper-case digits")
	})

	t.Run("concurrent accounts get distinct addresses", func(t *testing.T) {
		const n = 8
		_, err := h.pool.Ensure(ctx, n+8)
		require.NoError(t, err)

		accounts := make([]string, n)
		for i := range accounts {
			accounts[i] = h.newSpot(t, ctx)
		}
		out := make([]string, n)
		var g errgroup.Group
		for i, account := range accounts {
			g.Go(func() error {
				addr, err := h.addresses.Assign(ctx, account)
				out[i] = addr
				return err
			})
		}
		require.NoError(t, g.Wait())

		seen := map[string]bool{}
		for i, addr := range out {
			require.NotEmpty(t, addr, "account %d", i)
			assert.False(t, seen[addr], "address %s handed out twice", addr)
			seen[addr] = true
		}
	})

	// The race Assign has to survive: two first-time calls for one account
	// both miss the lookup, both try to claim, and the unique index lets
	// exactly one through. The loser must return the winner's address, not an
	// error and not a second address.
	t.Run("concurrent first calls for one account agree", func(t *testing.T) {
		_, err := h.pool.Ensure(ctx, 12)
		require.NoError(t, err)
		account := h.newSpot(t, ctx)

		const n = 6
		out := make([]string, n)
		var g errgroup.Group
		for i := range n {
			g.Go(func() error {
				addr, err := h.addresses.Assign(ctx, account)
				out[i] = addr
				return err
			})
		}
		require.NoError(t, g.Wait())
		for i := range out {
			assert.Equal(t, out[0], out[i], "every caller must see one address")
		}

		var rows int
		require.NoError(t, h.all.QueryRow(ctx,
			`SELECT count(*) FROM chain.deposit_addresses WHERE account_id = $1`, account).Scan(&rows))
		assert.Equal(t, 1, rows)
	})
}

func TestDepositAddressPoolExhaustion(t *testing.T) {
	h := setupChain(t)
	ctx := context.Background()

	_, err := h.pool.Ensure(ctx, 2)
	require.NoError(t, err)
	for range 2 {
		_, err := h.addresses.Assign(ctx, h.newSpot(t, ctx))
		require.NoError(t, err)
	}
	_, err = h.addresses.Assign(ctx, h.newSpot(t, ctx))
	require.ErrorIs(t, err, chain.ErrPoolEmpty, "an empty pool is a 503, not a 500")

	// and the signer refilling unblocks it without a restart
	_, err = h.pool.Ensure(ctx, 1)
	require.NoError(t, err)
	addr, err := h.addresses.Assign(ctx, h.newSpot(t, ctx))
	require.NoError(t, err)
	assert.NotEmpty(t, addr)
}

// TestDepositAddressPrivileges pins the split the migration encodes: the api
// role may claim a row but may not create one, and it may not rewrite an
// address. Every other integration test connects as ex_all, which holds every
// role's privileges at once and would hide exactly this.
func TestDepositAddressPrivileges(t *testing.T) {
	h := setupChain(t)
	ctx := context.Background()
	_, err := h.pool.Ensure(ctx, 2)
	require.NoError(t, err)

	apiPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_api"), MaxConns: 4})
	require.NoError(t, err)
	defer apiPool.Close()

	t.Run("the api role can claim an address", func(t *testing.T) {
		addr, err := chain.NewAddresses(apiPool, "default", testChainID).Assign(ctx, h.newSpot(t, ctx))
		require.NoError(t, err)
		assert.NotEmpty(t, addr)
	})

	t.Run("the api role cannot create one", func(t *testing.T) {
		_, err := apiPool.Exec(ctx,
			`INSERT INTO chain.deposit_addresses (tenant_id, chain_id, derivation_index, address)
			 VALUES ('default', $1, 999999, '0x000000000000000000000000000000000000dead')`, testChainID)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "42501", pgErr.Code, "only ex_signer derives addresses")
	})

	t.Run("the api role cannot rewrite an address", func(t *testing.T) {
		_, err := apiPool.Exec(ctx, `UPDATE chain.deposit_addresses SET address = '0x000000000000000000000000000000000000beef'`)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "42501", pgErr.Code, "the UPDATE grant covers account_id and assigned_at only")
	})

	t.Run("nobody may delete an assigned address", func(t *testing.T) {
		_, err := h.all.Exec(ctx, `DELETE FROM chain.deposit_addresses`)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "42501", pgErr.Code, "even ex_all has no DELETE")
	})

	t.Run("the signer role can derive", func(t *testing.T) {
		signerPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_signer"), MaxConns: 2})
		require.NoError(t, err)
		defer signerPool.Close()
		w, err := hdwallet.FromMnemonic(testMnemonic)
		require.NoError(t, err)
		defer w.Close()
		created, err := hdwallet.NewPool(signerPool, w, "default", testChainID).Ensure(ctx, 20)
		require.NoError(t, err)
		assert.Positive(t, created)
	})
}
