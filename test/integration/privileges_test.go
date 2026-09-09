//go:build integration

package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
)

// TestAPIRolePrivileges runs the api role's own write paths under the api
// role's own grants.
//
// Every other test here connects as ex_all, which holds every privilege of
// every role. That hides a whole class of defect: a query the api role is
// not allowed to run passes in-process and fails only once api, engine and
// admin are separate containers with separate login roles — where the only
// thing that catches it is scripts/e2e.sh, minutes into CI. Registration is
// the widest of those paths (auth.users + ledger.accounts +
// audit.audit_events in one transaction), so it is the one worth pinning.
func TestAPIRolePrivileges(t *testing.T) {
	h := setupLedger(t)
	ctx := context.Background()

	apiPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_api"), MaxConns: 4})
	require.NoError(t, err)
	defer apiPool.Close()

	led := ledger.New(apiPool, "default")
	require.NoError(t, led.LoadHouseAccounts(ctx))
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := auth.NewSigner(priv, "exchange")
	require.NoError(t, err)
	verifier, err := signer.VerifierFor()
	require.NoError(t, err)
	master := make([]byte, auth.MasterKeyLen)
	_, err = rand.Read(master)
	require.NoError(t, err)
	recorder := audit.NewRecorder("default")
	svc, err := auth.New(apiPool, auth.Config{Tenant: "default", MasterKey: master, Password: auth.TestPasswordParams},
		signer, verifier, led, recorder)
	require.NoError(t, err)

	t.Run("the api role can register and log in", func(t *testing.T) {
		// audit.audit_events is the only table whose writers hold INSERT
		// without SELECT, so an `INSERT … RETURNING` here is a 42501 for
		// every role but ex_admin and ex_all.
		session, err := svc.Register(ctx, "privileges@e2e.local", password, "203.0.113.7")
		require.NoError(t, err, "registration writes auth.users, ledger.accounts and audit.audit_events")
		assert.NotEmpty(t, session.AccountID)
		assert.NotEmpty(t, session.AccessToken)

		_, err = svc.Login(ctx, "privileges@e2e.local", password, "203.0.113.7")
		require.NoError(t, err, "login writes a refresh token and an audit record")
	})

	t.Run("the api role cannot read the audit trail", func(t *testing.T) {
		// The other half of migration 0004: a compromised api container must
		// not be able to read back every admin action and every user's login
		// history. Granting SELECT would "fix" the case above and lose this.
		_, err := recorder.List(ctx, apiPool, audit.Filter{Limit: 1})
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "42501", pgErr.Code, "only ex_admin and ex_all may read audit.audit_events")
	})
}

// The fee snapshot is enforced by a grant, not by discipline: the chain role
// may drive the state machine but holds no privilege on chain.withdrawals.fee
// or fee_asset, so a rate change cannot reach a withdrawal already in flight
// even through a bug. Every other test connects as ex_all and would never
// notice; without this one, an implementation that updated the snapshot would
// pass the whole suite and fail only in a split deployment.
func TestChainRoleCannotRewriteTheFeeSnapshot(t *testing.T) {
	h := setupLedger(t)
	ctx := context.Background()

	chainPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_chain"), MaxConns: 2})
	require.NoError(t, err)
	defer chainPool.Close()

	// Column privileges are checked when the statement is planned, so these
	// need no fixture row: a permitted UPDATE succeeds against nothing, and a
	// forbidden one fails before it looks.
	t.Run("the chain role may advance the state machine", func(t *testing.T) {
		_, err := chainPool.Exec(ctx,
			`UPDATE chain.withdrawals SET status = 'signed' WHERE id = 'no-such-withdrawal'`)
		require.NoError(t, err)
	})

	t.Run("but not the fee it was quoted", func(t *testing.T) {
		for _, stmt := range []string{
			`UPDATE chain.withdrawals SET fee = 0 WHERE id = 'no-such-withdrawal'`,
			`UPDATE chain.withdrawals SET fee_asset = 'ETH' WHERE id = 'no-such-withdrawal'`,
		} {
			_, err := chainPool.Exec(ctx, stmt)
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr, stmt)
			assert.Equal(t, "42501", pgErr.Code, stmt)
		}
	})
}
