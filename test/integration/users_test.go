//go:build integration

package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/admin"
	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/platform/secretbox"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// usersAuth is auth.Service with everything switched on -- a signer so
// Register works, a TOTP key so an administrator can hold a session -- on
// the given pool. The users tests need both sides: people to edit, and the
// login the edits must end.
func usersAuth(t *testing.T, pool *pgxpool.Pool, l *ledger.Service) *auth.Service {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := auth.NewSigner(priv, "exchange")
	require.NoError(t, err)
	verifier, err := signer.VerifierFor()
	require.NoError(t, err)
	master := make([]byte, auth.MasterKeyLen)
	_, err = rand.Read(master)
	require.NoError(t, err)
	totp := make([]byte, secretbox.KeySize)
	_, err = rand.Read(totp)
	require.NoError(t, err)
	svc, err := auth.New(pool, auth.Config{
		Tenant: "default", MasterKey: master, TOTPKey: totp, Password: auth.TestPasswordParams,
	}, signer, verifier, l, audit.NewRecorder("default"))
	require.NoError(t, err)
	return svc
}

// usersServer is the admin API with the user directory wired, on ex_all.
func usersServer(t *testing.T, h ledgerHarness, users *auth.Service) (*httptest.Server, *admin.Handler) {
	t.Helper()
	handler := admin.NewHandler(h.all, h.svc, registry.NewStore(h.all), audit.NewRecorder("default"), "default").WithUsers(users)
	r := chi.NewRouter()
	r.Use(telemetry.CorrelationMiddleware(slog.New(slog.NewTextHandler(io.Discard, nil))))
	admin.Routes(r, handler, nil, adminKey)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, handler
}

func outboxTypes(t *testing.T, pool *pgxpool.Pool, prefix string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT event_type FROM eventbus.outbox WHERE event_type LIKE $1 || '%' ORDER BY id`, prefix)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

func auditActions(t *testing.T, pool *pgxpool.Pool, prefix string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT action FROM audit.audit_events WHERE action LIKE $1 || '%' ORDER BY id`, prefix)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

// The directory and the two edits, over HTTP, with what each leaves behind.
func TestAdminUsers(t *testing.T) {
	ctx := context.Background()
	h := setupLedger(t)
	users := usersAuth(t, h.all, h.svc)
	srv, _ := usersServer(t, h, users)

	admin1, _, err := users.BootstrapAdmin(ctx, "ops@example.com", adminPassword)
	require.NoError(t, err)
	alice, err := users.Register(ctx, "alice@example.com", "a long enough password", "203.0.113.1")
	require.NoError(t, err)
	bob, err := users.Register(ctx, "bob@example.net", "a long enough password", "203.0.113.2")
	require.NoError(t, err)

	t.Run("the list, filtered", func(t *testing.T) {
		r := call(t, srv, http.MethodGet, "/admin/v1/users", adminKey, nil)
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		all := decode[gen.UserList](t, r)
		require.Len(t, all.Users, 3)
		assert.Equal(t, "bob@example.net", all.Users[0].Email, "newest first")

		r = call(t, srv, http.MethodGet, "/admin/v1/users?email=example.com", adminKey, nil)
		got := decode[gen.UserList](t, r)
		require.Len(t, got.Users, 2)
		r = call(t, srv, http.MethodGet, "/admin/v1/users?email=%25", adminKey, nil)
		assert.Empty(t, decode[gen.UserList](t, r).Users, "a fragment carries no wildcards")
		r = call(t, srv, http.MethodGet, "/admin/v1/users?role=admin", adminKey, nil)
		got = decode[gen.UserList](t, r)
		require.Len(t, got.Users, 1)
		assert.Equal(t, admin1.ID, got.Users[0].ID)
		assert.False(t, got.Users[0].TotpEnabled)
	})

	t.Run("one user, or not", func(t *testing.T) {
		r := call(t, srv, http.MethodGet, "/admin/v1/users/"+alice.UserID, adminKey, nil)
		require.Equal(t, http.StatusOK, r.status)
		u := decode[gen.User](t, r)
		assert.Equal(t, "alice@example.com", u.Email)
		assert.Equal(t, 0, u.KycLevel)
		assert.Equal(t, gen.UserStatusActive, u.Status)
		assert.Equal(t, gen.UserRoleUser, u.Role)
		r = call(t, srv, http.MethodGet, "/admin/v1/users/not-a-uuid", adminKey, nil)
		assert.Equal(t, http.StatusNotFound, r.status)
		r = call(t, srv, http.MethodGet, "/admin/v1/users/00000000-0000-0000-0000-000000000000", adminKey, nil)
		assert.Equal(t, http.StatusNotFound, r.status)
	})

	t.Run("kyc level", func(t *testing.T) {
		r := call(t, srv, http.MethodPut, "/admin/v1/users/"+alice.UserID+"/kyc-level", adminKey, map[string]any{"kyc_level": 2, "reason": "documents verified"})
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		u := decode[gen.User](t, r)
		assert.Equal(t, 2, u.KycLevel)
		assert.Equal(t, int32(2), u.Version, "registration was version 1")

		r = call(t, srv, http.MethodPut, "/admin/v1/users/"+alice.UserID+"/kyc-level", adminKey, map[string]any{"kyc_level": 2, "reason": "again"})
		require.Equal(t, http.StatusOK, r.status)
		assert.Equal(t, int32(2), decode[gen.User](t, r).Version, "the same level again changes nothing")

		r = call(t, srv, http.MethodPut, "/admin/v1/users/"+alice.UserID+"/kyc-level", adminKey, map[string]any{"kyc_level": 3, "reason": "x"})
		assert.Equal(t, http.StatusBadRequest, r.status, "0007 allows 0..2")
		r = call(t, srv, http.MethodPut, "/admin/v1/users/"+alice.UserID+"/kyc-level", adminKey, map[string]any{"kyc_level": 1})
		assert.Equal(t, http.StatusBadRequest, r.status, "a reason is required")
		r = call(t, srv, http.MethodPut, "/admin/v1/users/"+bob.UserID+"/kyc-level", adminKey, map[string]any{"kyc_level": 1, "reason": "x"})
		require.Equal(t, http.StatusOK, r.status)

		assert.Equal(t, []string{"user.kyc_level.update", "user.kyc_level.update"}, auditActions(t, h.all, "user.kyc"), "one row per real change")
		assert.Equal(t, []string{"user.kyc_level_updated", "user.kyc_level_updated"}, outboxTypes(t, h.all, "user.kyc"))
	})

	t.Run("freezing a user freezes their account and their logins", func(t *testing.T) {
		acct, err := h.svc.Account(ctx, alice.AccountID)
		require.NoError(t, err)
		require.Equal(t, ledger.StatusActive, acct.Status)

		r := call(t, srv, http.MethodPut, "/admin/v1/users/"+alice.UserID+"/status", adminKey, map[string]any{"status": "frozen", "reason": "chargeback investigation"})
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		assert.Equal(t, gen.UserStatusFrozen, decode[gen.User](t, r).Status)

		acct, err = h.svc.Account(ctx, alice.AccountID)
		require.NoError(t, err)
		assert.Equal(t, ledger.StatusFrozen, acct.Status, "the same transaction froze the spot account")

		_, err = users.Login(ctx, "alice@example.com", "a long enough password", "203.0.113.1")
		assert.ErrorIs(t, err, auth.ErrUserFrozen)
		_, err = users.Refresh(ctx, alice.RefreshToken)
		assert.ErrorIs(t, err, auth.ErrUserFrozen)

		var accountID string
		require.NoError(t, h.all.QueryRow(ctx,
			`SELECT after->>'account_id' FROM audit.audit_events WHERE action = 'user.status.update' AND target_id = $1`, alice.UserID).Scan(&accountID))
		assert.Equal(t, alice.AccountID, accountID, "the audit row names the account it froze")
		assert.Equal(t, []string{"user.status_updated"}, outboxTypes(t, h.all, "user.status"))

		r = call(t, srv, http.MethodPut, "/admin/v1/users/"+alice.UserID+"/status", adminKey, map[string]any{"status": "frozen", "reason": "again"})
		require.Equal(t, http.StatusOK, r.status)
		assert.Equal(t, []string{"user.status_updated"}, outboxTypes(t, h.all, "user.status"), "frozen again is not a change")

		r = call(t, srv, http.MethodPut, "/admin/v1/users/"+alice.UserID+"/status", adminKey, map[string]any{"status": "active", "reason": "cleared"})
		require.Equal(t, http.StatusOK, r.status)
		acct, err = h.svc.Account(ctx, alice.AccountID)
		require.NoError(t, err)
		assert.Equal(t, ledger.StatusActive, acct.Status, "release reverses the cascade")
		_, err = users.Login(ctx, "alice@example.com", "a long enough password", "203.0.113.1")
		assert.NoError(t, err)

		r = call(t, srv, http.MethodPut, "/admin/v1/users/"+alice.UserID+"/status", adminKey, map[string]any{"status": "closed", "reason": "x"})
		assert.Equal(t, http.StatusBadRequest, r.status)
	})

	t.Run("the last administrator cannot be frozen; another can", func(t *testing.T) {
		r := call(t, srv, http.MethodPut, "/admin/v1/users/"+admin1.ID+"/status", adminKey, map[string]any{"status": "frozen", "reason": "leaving"})
		assert.Equal(t, http.StatusConflict, r.status, string(r.body))
		assert.Contains(t, string(r.body), "last active administrator")

		// A second administrator, and a live back-office session for the first.
		_, err := h.all.Exec(ctx, `INSERT INTO auth.users (tenant_id, email, password_hash, role) VALUES ('default', 'second@example.com', 'x', 'admin')`)
		require.NoError(t, err)
		pending, err := users.AdminLogin(ctx, "ops@example.com", adminPassword, "127.0.0.1")
		require.NoError(t, err)

		r = call(t, srv, http.MethodPut, "/admin/v1/users/"+admin1.ID+"/status", adminKey, map[string]any{"status": "frozen", "reason": "leaving"})
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		_, err = users.AdminSession(ctx, pending.Token)
		assert.ErrorIs(t, err, auth.ErrInvalidToken, "the frozen administrator's session ended with them")
		_, err = users.AdminLogin(ctx, "ops@example.com", adminPassword, "127.0.0.1")
		assert.ErrorIs(t, err, auth.ErrInvalidCredentials, "and they cannot start another")
	})
}

// docs/plan-v1.0.md §12: a KYC change takes effect on the next withdrawal.
// The first withdrawal is over the level-0 limit and waits for a person;
// after the level changes, the same amount is approved on its own, and the
// one already waiting is left to the person.
func TestKYCLevelChangeTakesEffectOnTheNextWithdrawal(t *testing.T) {
	ctx := context.Background()
	h := setupWithdrawal(t)
	users := usersAuth(t, h.all, h.ledgerHarness.svc)
	_, handler := usersServer(t, h.ledgerHarness, users)

	session, err := users.Register(ctx, "kyc@example.com", "a long enough password", "203.0.113.5")
	require.NoError(t, err)
	h.fund(t, ctx, session.AccountID, "ETH", "5", "faucet-kyc")

	// Level 0 auto-approves up to 0.1 ETH (registry seed); 0.5 is a review.
	first := h.create(t, ctx, session.AccountID, "ETH", "0.5", "kyc-1")
	require.NoError(t, h.worker.Tick(ctx))
	require.Equal(t, withdrawal.StatusPendingReview, h.status(t, ctx, session.AccountID, first.ID))

	resp, err := handler.SetUserKycLevel(ctx, gen.SetUserKycLevelRequestObject{
		ID: session.UserID, Body: &gen.KycLevelRequest{KycLevel: 2, Reason: "documents verified"},
	})
	require.NoError(t, err)
	require.IsType(t, gen.SetUserKycLevel200JSONResponse{}, resp)

	second := h.create(t, ctx, session.AccountID, "ETH", "0.5", "kyc-2")
	require.NoError(t, h.worker.Tick(ctx))
	assert.Equal(t, withdrawal.StatusAutoApproved, h.status(t, ctx, session.AccountID, second.ID), "level 2 auto-approves up to 10 ETH")
	assert.Equal(t, withdrawal.StatusPendingReview, h.status(t, ctx, session.AccountID, first.ID), "nothing already pending is re-decided")
}

// The same edits through the pages.
func TestAdminUIUsersPages(t *testing.T) {
	ctx := context.Background()
	h := setupAdminAuth(t)
	// People to edit come in through the api side of auth; the pages run on
	// the ex_admin pool like the real thing.
	public := usersAuth(t, h.all, h.ledgerHarness.svc)
	alice, err := public.Register(ctx, "alice@example.com", "a long enough password", "203.0.113.1")
	require.NoError(t, err)
	srv := adminUIServer(t, h, loginLimitForTests)
	b := newBrowser(t, srv)
	signIn(t, b, h)

	resp, body := b.get("/admin/users")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, "alice@example.com")
	assert.Contains(t, body, adminEmail)
	assert.NotContains(t, body, "older ›", "two users fit on one page")

	_, body = b.get("/admin/users?limit=1")
	assert.Contains(t, body, "older ›")
	assert.Contains(t, body, `hx-get="/admin/users?limit=1&amp;offset=1"`)
	_, body = b.get("/admin/users?email=alice&limit=1&offset=1")
	assert.Contains(t, body, "No users match.")
	assert.Contains(t, body, `href="/admin/users?email=alice&amp;limit=1&amp;offset=0"`, "paging keeps the filter")

	resp, body = b.get("/admin/users/" + alice.UserID)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, body, ">Freeze<")
	assert.Contains(t, body, alice.AccountID)

	resp, _ = b.post("/admin/users/"+alice.UserID+"/status", url.Values{"status": {"frozen"}, "reason": {"chargeback"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/admin/users/"+alice.UserID, resp.Header.Get("Location"))
	_, body = b.get("/admin/users/" + alice.UserID)
	assert.Contains(t, body, "User is frozen: no login, no orders, no withdrawals.")
	assert.Contains(t, body, `class="badge bad">frozen<`)
	assert.Contains(t, body, ">Release<")

	resp, _ = b.post("/admin/users/"+alice.UserID+"/kyc-level", url.Values{"kyc_level": {"1"}, "reason": {""}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	_, body = b.get("/admin/users/" + alice.UserID)
	assert.Contains(t, body, "A reason is required")
	resp, _ = b.post("/admin/users/"+alice.UserID+"/kyc-level", url.Values{"kyc_level": {"1"}, "reason": {"documents"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	_, body = b.get("/admin/users/" + alice.UserID)
	assert.Contains(t, body, "KYC level is now 1.")

	resp, _ = b.post("/admin/users/"+h.admin.ID+"/status", url.Values{"status": {"frozen"}, "reason": {"me"}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	_, body = b.get("/admin/users/" + h.admin.ID)
	assert.Contains(t, body, "last active administrator")

	resp, _ = b.get("/admin/users/00000000-0000-0000-0000-000000000000")
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	_, body = b.get("/admin/users")
	assert.Contains(t, body, "No user 00000000-0000-0000-0000-000000000000.")

	// The audit trail attributes the page's writes to the person, not the key.
	var actor, actorID string
	require.NoError(t, h.adminPool.QueryRow(ctx,
		`SELECT actor_type, actor_id FROM audit.audit_events WHERE action = 'user.status.update' ORDER BY id LIMIT 1`).Scan(&actor, &actorID))
	assert.Equal(t, "admin", actor)
	assert.Equal(t, h.admin.ID, actorID)
}
