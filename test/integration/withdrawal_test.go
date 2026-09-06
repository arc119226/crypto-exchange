//go:build integration

package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/chain/hdwallet"
	"github.com/arc119226/crypto-exchange/internal/chain/signer"
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/policy"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// A destination that is not the zero address and not one of ours.
const payoutAddress = "0x70997970c51812dc3a010c7d01b50e0d17dc79c8"

type withdrawalHarness struct {
	ledgerHarness
	svc      *withdrawal.Service
	worker   *withdrawal.Worker
	reviewer *withdrawal.Reviewer
}

func setupWithdrawal(t *testing.T) withdrawalHarness {
	t.Helper()
	ctx := context.Background()
	lh := setupLedger(t)

	fx, err := registry.LoadFixtures(fixtures)
	require.NoError(t, err)
	_, err = registry.Seed(ctx, lh.all, fx, registry.SeedOptions{TenantID: "default", RequiredConfirmations: 1})
	require.NoError(t, err)

	store := registry.NewStore(lh.all)
	rec := audit.NewRecorder("default")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return withdrawalHarness{
		ledgerHarness: lh,
		svc:           withdrawal.NewService(lh.all, "default", anvilChainID, store, lh.svc, rec),
		worker: withdrawal.NewWorker(lh.all, withdrawal.Config{Tenant: "default"},
			store, store, lh.svc, withdrawal.NewPostgresKYC(lh.all), policy.Basic{}, rec, log),
		reviewer: withdrawal.NewReviewer(lh.all, "default", lh.svc, rec),
	}
}

// create records a withdrawal with a generated key.
func (h withdrawalHarness) create(t *testing.T, ctx context.Context, account, asset, amount, key string) withdrawal.Record {
	t.Helper()
	res, err := h.svc.Create(ctx, withdrawal.CreateParams{
		AccountID: account, Asset: asset, Amount: amt(amount),
		ToAddress: payoutAddress, IdempotencyKey: key,
	})
	require.NoError(t, err)
	return res.Record
}

func (h withdrawalHarness) status(t *testing.T, ctx context.Context, account, id string) string {
	t.Helper()
	rec, err := h.svc.Get(ctx, account, id)
	require.NoError(t, err)
	return rec.Status
}

// The whole automatic path: a withdrawal inside every limit is decided without
// a person and its funds end up on hold.
func TestWithdrawalAutoApprovesAndLocksFunds(t *testing.T) {
	h := setupWithdrawal(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-auto")

	// 0.05 ETH is under the seeded level-0 ceiling of 0.1.
	w := h.create(t, ctx, account, "ETH", "0.05", "auto-1")
	assert.Equal(t, withdrawal.StatusRequested, w.Status)
	assert.Equal(t, payoutAddress, w.ToAddress, "the destination is stored lower-case")

	// One tick decides it, the next locks the funds: every state is committed
	// before the next step runs (§6.4.2), so the machine takes two passes.
	require.NoError(t, h.worker.Tick(ctx))
	assert.Equal(t, withdrawal.StatusAutoApproved, h.status(t, ctx, account, w.ID))
	require.NoError(t, h.worker.Tick(ctx))
	assert.Equal(t, withdrawal.StatusFundsLocked, h.status(t, ctx, account, w.ID))

	b := h.balance(t, ctx, account, "ETH")
	assert.Equal(t, "4.95", b.Available.String(), "the amount left available")
	assert.Equal(t, "0.05", b.Hold.String(), "and is on hold, not gone")
	h.assertCacheMatchesPostings(t, ctx, account)
	h.assertTrialBalanceZero(t, ctx)

	// Nothing further to do: a locked withdrawal waits for the signer.
	require.NoError(t, h.worker.Tick(ctx))
	assert.Equal(t, withdrawal.StatusFundsLocked, h.status(t, ctx, account, w.ID))
}

// Over the per-request ceiling the withdrawal waits for a person, and an
// approval hands it back to the worker rather than moving money itself.
func TestWithdrawalOverTheLimitWaitsForReview(t *testing.T) {
	h := setupWithdrawal(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-review")

	w := h.create(t, ctx, account, "ETH", "0.5", "review-1") // ceiling is 0.1
	require.NoError(t, h.worker.Tick(ctx))
	assert.Equal(t, withdrawal.StatusPendingReview, h.status(t, ctx, account, w.ID))

	// The worker must not touch it while it waits, however often it runs.
	require.NoError(t, h.worker.Tick(ctx))
	assert.Equal(t, withdrawal.StatusPendingReview, h.status(t, ctx, account, w.ID))
	assert.True(t, h.balance(t, ctx, account, "ETH").Hold.IsZero(), "nothing is held before a decision")

	queue, err := h.reviewer.Pending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, queue, 1)
	assert.Equal(t, w.ID, queue[0].ID)

	reviewed, err := h.reviewer.Review(ctx, withdrawal.ReviewParams{
		ID: w.ID, Approve: true, Note: "known counterparty",
		ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	require.NoError(t, err)
	assert.Equal(t, withdrawal.StatusApproved, reviewed.Status)
	assert.NotNil(t, reviewed.ReviewedAt, "when a person decided is recorded even though who is not yet")

	require.NoError(t, h.worker.Tick(ctx))
	assert.Equal(t, withdrawal.StatusFundsLocked, h.status(t, ctx, account, w.ID))
	assert.Equal(t, "0.5", h.balance(t, ctx, account, "ETH").Hold.String())
	h.assertTrialBalanceZero(t, ctx)
}

// A rejection is terminal and leaves the balance exactly as it was: nothing
// was ever held, so there is nothing to release.
func TestWithdrawalRejectionLeavesTheBalanceAlone(t *testing.T) {
	h := setupWithdrawal(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-reject")

	w := h.create(t, ctx, account, "ETH", "0.5", "reject-1")
	require.NoError(t, h.worker.Tick(ctx))
	_, err := h.reviewer.Review(ctx, withdrawal.ReviewParams{
		ID: w.ID, Approve: false, Note: "destination on a sanctions list",
		ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	require.NoError(t, err)

	rec, err := h.svc.Get(ctx, account, w.ID)
	require.NoError(t, err)
	assert.Equal(t, withdrawal.StatusRejected, rec.Status)
	assert.Equal(t, "destination on a sanctions list", rec.ReviewNote)

	require.NoError(t, h.worker.Tick(ctx), "a rejected withdrawal is not in the worker's queue")
	assert.Equal(t, withdrawal.StatusRejected, h.status(t, ctx, account, w.ID))
	b := h.balance(t, ctx, account, "ETH")
	assert.Equal(t, "5", b.Available.String())
	assert.True(t, b.Hold.IsZero())

	// Deciding it a second time must fail rather than resurrect it.
	_, err = h.reviewer.Review(ctx, withdrawal.ReviewParams{
		ID: w.ID, Approve: true, ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
	})
	assert.ErrorIs(t, err, withdrawal.ErrNotReviewable)
}

// The Idempotency-Key is what makes a retry safe, and what makes a *different*
// request under the same key an error rather than a second withdrawal.
func TestWithdrawalIdempotencyKey(t *testing.T) {
	h := setupWithdrawal(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-idem")

	first, err := h.svc.Create(ctx, withdrawal.CreateParams{
		AccountID: account, Asset: "ETH", Amount: amt("0.05"),
		ToAddress: payoutAddress, IdempotencyKey: "same-key",
	})
	require.NoError(t, err)
	assert.False(t, first.Replayed)

	again, err := h.svc.Create(ctx, withdrawal.CreateParams{
		AccountID: account, Asset: "ETH", Amount: amt("0.05"),
		ToAddress: payoutAddress, IdempotencyKey: "same-key",
	})
	require.NoError(t, err)
	assert.True(t, again.Replayed, "the same request under the same key is the same withdrawal")
	assert.Equal(t, first.Record.ID, again.Record.ID)

	// Same key, different amount: a client bug, and the one thing that must
	// never quietly become a second withdrawal.
	_, err = h.svc.Create(ctx, withdrawal.CreateParams{
		AccountID: account, Asset: "ETH", Amount: amt("0.06"),
		ToAddress: payoutAddress, IdempotencyKey: "same-key",
	})
	assert.ErrorIs(t, err, withdrawal.ErrIdempotencyMismatch)

	// Same key, different destination: the same answer, and the reason the
	// hash covers the address rather than only the amount.
	_, err = h.svc.Create(ctx, withdrawal.CreateParams{
		AccountID: account, Asset: "ETH", Amount: amt("0.05"),
		ToAddress: "0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc", IdempotencyKey: "same-key",
	})
	assert.ErrorIs(t, err, withdrawal.ErrIdempotencyMismatch)

	list, err := h.svc.List(ctx, account, 10)
	require.NoError(t, err)
	assert.Len(t, list, 1, "four calls, one withdrawal")

	// The key belongs to the account, so another account may reuse it.
	other := h.newSpot(t, ctx)
	h.fund(t, ctx, other, "ETH", "5", "faucet-idem-other")
	res, err := h.svc.Create(ctx, withdrawal.CreateParams{
		AccountID: other, Asset: "ETH", Amount: amt("0.05"),
		ToAddress: payoutAddress, IdempotencyKey: "same-key",
	})
	require.NoError(t, err)
	assert.NotEqual(t, first.Record.ID, res.Record.ID)
}

// The balance can move between the request and the lock, and the lock is the
// authority: the pre-check is a courtesy.
func TestWithdrawalFailsWhenTheBalanceIsGoneByTheTimeItLocks(t *testing.T) {
	h := setupWithdrawal(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "1", "faucet-race")

	w := h.create(t, ctx, account, "ETH", "0.05", "race-1")
	require.NoError(t, h.worker.Tick(ctx)) // auto_approved

	// Spend it all before the lock — an order, another withdrawal, anything.
	require.NoError(t, inTx(ctx, h.all, func(tx pgx.Tx) error {
		_, _, err := h.ledgerHarness.svc.Adjust(ctx, tx, ledger.AdjustParams{
			AccountID: account, Asset: "ETH", Amount: amt("1"), Direction: ledger.Debit,
			Reason: "spent elsewhere", IdempotencyKey: "race-drain",
		})
		return err
	}))
	require.True(t, h.balance(t, ctx, account, "ETH").Available.IsZero())

	require.NoError(t, h.worker.Tick(ctx))
	rec, err := h.svc.Get(ctx, account, w.ID)
	require.NoError(t, err)
	assert.Equal(t, withdrawal.StatusFailed, rec.Status)
	assert.Equal(t, withdrawal.FailureInsufficientBalance, rec.FailureReason)
	h.assertTrialBalanceZero(t, ctx)
}

// The daily limit counts what is already committed, so a series of individually
// auto-approvable withdrawals cannot cross it.
func TestWithdrawalDailyLimitCountsWhatIsAlreadyCommitted(t *testing.T) {
	h := setupWithdrawal(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "20", "faucet-daily")

	// Level 0: 0.1 per request, 1 a day. Ten at the ceiling reach the limit.
	for i := range 10 {
		w := h.create(t, ctx, account, "ETH", "0.1", "daily-"+string(rune('a'+i)))
		require.NoError(t, h.worker.Tick(ctx))
		assert.Equalf(t, withdrawal.StatusAutoApproved, h.status(t, ctx, account, w.ID),
			"withdrawal %d lands exactly on the daily limit, not over it", i)
	}

	// The eleventh is within the per-request ceiling and still must not pass.
	over := h.create(t, ctx, account, "ETH", "0.1", "daily-over")
	require.NoError(t, h.worker.Tick(ctx))
	assert.Equal(t, withdrawal.StatusPendingReview, h.status(t, ctx, account, over.ID),
		"the daily limit is what stops it, and it queues rather than being refused")
}

// Validation the API performs before anything is recorded.
func TestWithdrawalRefusesRequestsItShouldNeverRecord(t *testing.T) {
	h := setupWithdrawal(t)
	ctx := context.Background()
	account := h.newSpot(t, ctx)
	h.fund(t, ctx, account, "ETH", "5", "faucet-invalid")

	for name, p := range map[string]withdrawal.CreateParams{
		"unknown asset":      {Asset: "DOGE", Amount: amt("1"), ToAddress: payoutAddress, IdempotencyKey: "k1"},
		"no idempotency key": {Asset: "ETH", Amount: amt("1"), ToAddress: payoutAddress},
		"not an address":     {Asset: "ETH", Amount: amt("1"), ToAddress: "0xnope", IdempotencyKey: "k3"},
		"zero address":       {Asset: "ETH", Amount: amt("1"), ToAddress: "0x0000000000000000000000000000000000000000", IdempotencyKey: "k4"},
		"more than held":     {Asset: "ETH", Amount: amt("500"), ToAddress: payoutAddress, IdempotencyKey: "k5"},
		"below the minimum":  {Asset: "ETH", Amount: amt("0.0000000001"), ToAddress: payoutAddress, IdempotencyKey: "k6"},
	} {
		p.AccountID = account
		_, err := h.svc.Create(ctx, p)
		assert.ErrorIsf(t, err, withdrawal.ErrInvalid, "%s must be refused", name)
	}

	// A mixed-case address whose EIP-55 checksum does not match is a typo, and
	// sending money to it is not recoverable.
	_, err := h.svc.Create(ctx, withdrawal.CreateParams{
		AccountID: account, Asset: "ETH", Amount: amt("1"),
		ToAddress: "0x70997970C51812dc3A010C7d01b50e0d17dc79C9", IdempotencyKey: "k7",
	})
	assert.ErrorIs(t, err, withdrawal.ErrInvalid)

	// The correctly checksummed form of the same address is accepted and
	// stored lower-case.
	res, err := h.svc.Create(ctx, withdrawal.CreateParams{
		AccountID: account, Asset: "ETH", Amount: amt("1"),
		ToAddress: "0x70997970C51812dc3A010C7d01b50e0d17dc79C8", IdempotencyKey: "k8",
	})
	require.NoError(t, err)
	assert.Equal(t, payoutAddress, res.Record.ToAddress)

	list, err := h.svc.List(ctx, account, 20)
	require.NoError(t, err)
	assert.Len(t, list, 1, "only the valid request was recorded")
}

// One account must not be able to read or list another's withdrawals.
func TestWithdrawalIsScopedToItsAccount(t *testing.T) {
	h := setupWithdrawal(t)
	ctx := context.Background()
	mine := h.newSpot(t, ctx)
	theirs := h.newSpot(t, ctx)
	h.fund(t, ctx, mine, "ETH", "5", "faucet-scope")

	w := h.create(t, ctx, mine, "ETH", "0.05", "scope-1")

	_, err := h.svc.Get(ctx, theirs, w.ID)
	assert.ErrorIs(t, err, withdrawal.ErrNotFound,
		"not-found rather than forbidden: the existence of the withdrawal is itself private")

	list, err := h.svc.List(ctx, theirs, 10)
	require.NoError(t, err)
	assert.Empty(t, list)
}

// TestWithdrawalRolePrivileges runs the whole path under the login roles a
// split deployment actually uses.
//
// Every test above connects as ex_all, which holds every privilege of every
// role at once. That is exactly how the audit_events failure of Phase 3c and
// the ledger.accounts failure of 4a-2 both reached CI: a query a single role
// may not run passes in-process and fails only once api, chain and admin are
// separate containers. Withdrawals cross more role boundaries than anything
// before them — api inserts, chain decides and holds, admin reviews — so the
// whole machine is worth pinning under real grants.
func TestWithdrawalRolePrivileges(t *testing.T) {
	h := setupWithdrawal(t)
	ctx := context.Background()

	apiPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_api"), MaxConns: 4})
	require.NoError(t, err)
	defer apiPool.Close()
	chainPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_chain"), MaxConns: 4})
	require.NoError(t, err)
	defer chainPool.Close()
	adminPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_admin"), MaxConns: 4})
	require.NoError(t, err)
	defer adminPool.Close()

	// A real user, so the policy has a KYC level to read: the chain role has
	// to reach auth.users across the account, which 0007 never granted it.
	apiLedger := ledger.New(apiPool, "default")
	require.NoError(t, apiLedger.LoadHouseAccounts(ctx))
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	jwtSigner, err := auth.NewSigner(priv, "exchange")
	require.NoError(t, err)
	verifier, err := jwtSigner.VerifierFor()
	require.NoError(t, err)
	master := make([]byte, auth.MasterKeyLen)
	_, err = rand.Read(master)
	require.NoError(t, err)
	rec := audit.NewRecorder("default")
	authSvc, err := auth.New(apiPool, auth.Config{Tenant: "default", MasterKey: master, Password: auth.TestPasswordParams},
		jwtSigner, verifier, apiLedger, rec)
	require.NoError(t, err)
	session, err := authSvc.Register(ctx, "withdraw-privileges@e2e.local", password, "203.0.113.9")
	require.NoError(t, err)
	h.fund(t, ctx, session.AccountID, "ETH", "5", "faucet-privileges")

	store := registry.NewStore(apiPool)
	apiSvc := withdrawal.NewService(apiPool, "default", anvilChainID, store, apiLedger, rec)

	var id string
	t.Run("the api role can record a withdrawal", func(t *testing.T) {
		// INSERT ... RETURNING (needs SELECT), an audit row, and
		// withdrawal.requested in the outbox — whose account_seq comes from an
		// UPDATE ... RETURNING on ledger.accounts that 0003 never granted the
		// api role. All three in one transaction.
		res, err := apiSvc.Create(ctx, withdrawal.CreateParams{
			AccountID: session.AccountID, UserID: session.UserID, Asset: "ETH", Amount: amt("0.5"),
			ToAddress: payoutAddress, IdempotencyKey: "privileges-1", IP: "203.0.113.9",
		})
		require.NoError(t, err)
		id = res.Record.ID
	})

	t.Run("the api role cannot move a withdrawal it created", func(t *testing.T) {
		// The point of the column grants: recording a withdrawal must not
		// carry the ability to walk it towards being signed.
		_, err := apiPool.Exec(ctx, `UPDATE chain.withdrawals SET status = 'approved' WHERE id = $1`, id)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "42501", pgErr.Code)
	})

	t.Run("the chain role can decide it", func(t *testing.T) {
		chainLedger := ledger.New(chainPool, "default")
		require.NoError(t, chainLedger.LoadHouseAccounts(ctx))
		worker := withdrawal.NewWorker(chainPool, withdrawal.Config{Tenant: "default"},
			registry.NewStore(chainPool), registry.NewStore(chainPool), chainLedger,
			withdrawal.NewPostgresKYC(chainPool), policy.Basic{}, rec,
			slog.New(slog.NewTextHandler(io.Discard, nil)))
		// Reads auth.users for the KYC level, writes the status, an audit row
		// and an event.
		require.NoError(t, worker.Tick(ctx))
		assert.Equal(t, withdrawal.StatusPendingReview, h.status(t, ctx, session.AccountID, id))
	})

	t.Run("the chain role cannot review it", func(t *testing.T) {
		// An approval must come from a person through the admin role, which
		// is what makes "no single role both authorises and acts" true rather
		// than merely intended.
		_, err := chainPool.Exec(ctx,
			`UPDATE chain.withdrawals SET reviewed_by = NULL, reviewed_at = now() WHERE id = $1`, id)
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "42501", pgErr.Code)
	})

	t.Run("the admin role can approve it and the chain role can lock the funds", func(t *testing.T) {
		adminLedger := ledger.New(adminPool, "default")
		require.NoError(t, adminLedger.LoadHouseAccounts(ctx))
		reviewer := withdrawal.NewReviewer(adminPool, "default", adminLedger, rec)
		_, err := reviewer.Review(ctx, withdrawal.ReviewParams{
			ID: id, Approve: true, Note: "checked", ActorType: audit.ActorAPIKey, ActorID: "admin-api-key",
		})
		require.NoError(t, err)

		chainLedger := ledger.New(chainPool, "default")
		require.NoError(t, chainLedger.LoadHouseAccounts(ctx))
		worker := withdrawal.NewWorker(chainPool, withdrawal.Config{Tenant: "default"},
			registry.NewStore(chainPool), registry.NewStore(chainPool), chainLedger,
			withdrawal.NewPostgresKYC(chainPool), policy.Basic{}, rec,
			slog.New(slog.NewTextHandler(io.Discard, nil)))
		require.NoError(t, worker.Tick(ctx))

		assert.Equal(t, withdrawal.StatusFundsLocked, h.status(t, ctx, session.AccountID, id))
		assert.Equal(t, "0.5", h.balance(t, ctx, session.AccountID, "ETH").Hold.String())
		h.assertTrialBalanceZero(t, ctx)
	})

	signerPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_signer"), MaxConns: 4})
	require.NoError(t, err)
	defer signerPool.Close()

	t.Run("the signer role can sign it", func(t *testing.T) {
		// Reads the withdrawal to check the request against it, and appends to
		// chain.signing_log — the two grants 0010 and 0011 give this role, and
		// nothing else.
		w, err := hdwallet.FromMnemonic(testMnemonic)
		require.NoError(t, err)
		defer w.Close()
		s, err := signer.NewKeystoreSigner(signerPool, "default", anvilChainID, w,
			registry.NewStore(signerPool), rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
		require.NoError(t, err)
		res, err := s.Sign(ctx, signer.Request{
			Kind: signer.KindWithdrawal, RefID: id, ChainID: anvilChainID,
			To: common.HexToAddress(payoutAddress), Asset: "ETH", Value: amt("0.5"),
			Nonce: 0, Gas: 21000, TipCap: big.NewInt(1), FeeCap: big.NewInt(2),
		})
		require.NoError(t, err)
		assert.NotEmpty(t, res.RawTx)
	})

	t.Run("the signer role cannot move the withdrawal it signed", func(t *testing.T) {
		// Holding the key must not carry the ability to declare the money
		// sent. The chain role writes the transaction columns; the signer only
		// produces bytes (§6.4.2, §6.6).
		for _, stmt := range []string{
			`UPDATE chain.withdrawals SET status = 'broadcast' WHERE id = $1`,
			`UPDATE chain.withdrawals SET tx_hash = '0x` + strings.Repeat("a", 64) + `' WHERE id = $1`,
		} {
			_, err := signerPool.Exec(ctx, stmt, id)
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr, stmt)
			assert.Equal(t, "42501", pgErr.Code, stmt)
		}
	})

	t.Run("nobody may rewrite the signing log", func(t *testing.T) {
		// The UNIQUE key is only a defence if the row it collides with cannot
		// be cleared: a signature that happened cannot be unhappened.
		for _, p := range []struct {
			role string
			pool *pgxpool.Pool
		}{{"ex_chain", chainPool}, {"ex_admin", adminPool}, {"ex_signer", signerPool}} {
			_, err := p.pool.Exec(ctx, `DELETE FROM chain.signing_log WHERE ref_id = $1`, id)
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr, p.role)
			assert.Equal(t, "42501", pgErr.Code, p.role)
		}
		// And the chain role cannot forge one either.
		_, err := chainPool.Exec(ctx,
			`INSERT INTO chain.signing_log (kind, ref_id, attempt, chain_id, from_address, to_address, nonce, tx_hash, raw_tx)
			 VALUES ('withdrawal', $1, 9, $2, $3, $3, 0, $4, '\x00')`,
			id, anvilChainID, payoutAddress, "0x"+strings.Repeat("b", 64))
		var pgErr *pgconn.PgError
		require.ErrorAs(t, err, &pgErr)
		assert.Equal(t, "42501", pgErr.Code)
	})

	t.Run("the admin role can ask for a resolution but not act on one", func(t *testing.T) {
		// resolve is four actions that all need a node and a key. Admin has
		// neither, so it may write the request columns and nothing else — a
		// role that could write tx_hash could make a withdrawal look sent
		// without anything having been signed.
		_, err := adminPool.Exec(ctx,
			`UPDATE chain.withdrawals SET resolve_action = 'bump', resolve_note = 'why',
			   resolve_requested_by = 'admin-api-key', resolve_requested_at = now() WHERE id = $1`, id)
		require.NoError(t, err)

		for _, stmt := range []string{
			`UPDATE chain.withdrawals SET nonce = 3 WHERE id = $1`,
			`UPDATE chain.withdrawals SET tx_hash = '0x` + strings.Repeat("c", 64) + `' WHERE id = $1`,
			`UPDATE chain.withdrawals SET cancel_tx_hash = '0x` + strings.Repeat("d", 64) + `' WHERE id = $1`,
		} {
			_, err := adminPool.Exec(ctx, stmt, id)
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr, stmt)
			assert.Equal(t, "42501", pgErr.Code, stmt)
		}

		// Admin does hold status, because reviewing writes it. What stops it
		// declaring a withdrawal sent is the pair: the states that mean "on
		// the chain" require the transaction columns (0011), and those are the
		// ones it may not write. Neither half is sufficient alone.
		_, err = adminPool.Exec(ctx,
			`UPDATE chain.withdrawals SET status = 'confirmed' WHERE id = $1`, id)
		var checkErr *pgconn.PgError
		require.ErrorAs(t, err, &checkErr)
		assert.Equal(t, "23514", checkErr.Code, "signed means signed")

		// The chain role is the one that clears the request it applied.
		_, err = chainPool.Exec(ctx,
			`UPDATE chain.withdrawals SET resolve_action = NULL, resolve_error = 'done' WHERE id = $1`, id)
		require.NoError(t, err)
	})
}
