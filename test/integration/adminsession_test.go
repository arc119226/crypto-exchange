//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/platform/secretbox"
)

const (
	adminEmail    = "ops@example.com"
	adminPassword = "correct horse battery staple"
)

// adminAuthHarness is auth.Service as the admin role runs it: no signer, a
// TOTP key, and -- the part that matters -- a pool logged in as ex_admin,
// so a grant the migration forgot fails here rather than in compose.
type adminAuthHarness struct {
	ledgerHarness
	adminPool *pgxpool.Pool
	svc       *auth.Service
	clock     time.Time
	admin     auth.User
}

func setupAdminAuth(t *testing.T) *adminAuthHarness {
	t.Helper()
	ctx := context.Background()
	lh := setupLedger(t)
	pool, err := pg.Open(ctx, pg.PoolConfig{DSN: lh.DSN("ex_admin"), MaxConns: 5})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	key := make([]byte, secretbox.KeySize)
	_, err = rand.Read(key)
	require.NoError(t, err)
	l := ledger.New(pool, "default")
	require.NoError(t, l.LoadHouseAccounts(ctx))
	svc, err := auth.New(pool, auth.Config{
		Tenant: "default", TOTPKey: key, Password: auth.TestPasswordParams, AdminSessionTTL: 8 * time.Hour,
	}, nil, nil, l, audit.NewRecorder("default"))
	require.NoError(t, err)

	h := &adminAuthHarness{ledgerHarness: lh, adminPool: pool, svc: svc, clock: time.Now().UTC().Truncate(time.Second)}
	svc.WithClock(func() time.Time { return h.clock })
	h.admin, _, err = svc.BootstrapAdmin(ctx, adminEmail, adminPassword)
	require.NoError(t, err)
	return h
}

func (h *adminAuthHarness) advance(d time.Duration) { h.clock = h.clock.Add(d) }

// code returns the authenticator's answer for the harness clock.
func (h *adminAuthHarness) code(t *testing.T, secret string) string {
	t.Helper()
	c, err := auth.TOTPCode(secret, h.clock)
	require.NoError(t, err)
	return c
}

// enrolAndLogin is the happy path up to a verified session: CLI-issued
// secret, password, first code.
func (h *adminAuthHarness) enrolAndLogin(t *testing.T) (sess auth.AdminSession, secret string) {
	t.Helper()
	ctx := context.Background()
	enrol, err := h.svc.EnrollTOTP(ctx, adminEmail)
	require.NoError(t, err)
	pending, err := h.svc.AdminLogin(ctx, adminEmail, adminPassword, "127.0.0.1")
	require.NoError(t, err)
	sess, err = h.svc.TOTPConfirm(ctx, pending.Token, h.code(t, enrol.Secret), "127.0.0.1")
	require.NoError(t, err)
	return sess, enrol.Secret
}

// The whole login, in the order an operator meets it. Each stage is a
// different kind of session, and each refusal names the stage.
func TestAdminLoginIsAPasswordThenACode(t *testing.T) {
	ctx := context.Background()
	h := setupAdminAuth(t)

	// Bootstrap gave the administrator a password and nothing else.
	pending, err := h.svc.AdminLogin(ctx, adminEmail, adminPassword, "127.0.0.1")
	require.NoError(t, err)
	assert.NotEmpty(t, pending.Token)
	assert.False(t, pending.TOTPVerified, "a password is not an administrator")
	assert.False(t, pending.TOTPEnabled)
	assert.False(t, pending.TOTPPending, "no secret has been issued yet")
	assert.WithinDuration(t, h.clock.Add(10*time.Minute), pending.ExpiresAt, time.Second, "a password-only session is short")

	_, err = h.svc.TOTPVerify(ctx, pending.Token, "123456", "127.0.0.1")
	assert.ErrorIs(t, err, auth.ErrTOTPNotEnrolled, "nothing to verify against until the CLI has issued a secret")

	// The CLI issues one. That signs out the pending session: a new secret
	// means starting over.
	enrol, err := h.svc.EnrollTOTP(ctx, adminEmail)
	require.NoError(t, err)
	assert.Len(t, enrol.Secret, 32)
	assert.Contains(t, enrol.URL, "otpauth://totp/")
	_, err = h.svc.AdminSession(ctx, pending.Token)
	assert.ErrorIs(t, err, auth.ErrInvalidToken)

	pending, err = h.svc.AdminLogin(ctx, adminEmail, adminPassword, "127.0.0.1")
	require.NoError(t, err)
	assert.True(t, pending.TOTPPending, "the first code is still owed")

	_, err = h.svc.TOTPConfirm(ctx, pending.Token, "000000", "127.0.0.1")
	assert.ErrorIs(t, err, auth.ErrInvalidTOTP)

	verified, err := h.svc.TOTPConfirm(ctx, pending.Token, h.code(t, enrol.Secret), "127.0.0.1")
	require.NoError(t, err)
	assert.True(t, verified.TOTPVerified)
	assert.True(t, verified.TOTPEnabled)
	assert.NotEqual(t, pending.Token, verified.Token, "passing the code rotates the session id")
	assert.WithinDuration(t, h.clock.Add(8*time.Hour), verified.ExpiresAt, time.Second)

	// The cookie that carried the password-only session is dead; the new
	// one resolves to an administrator.
	_, err = h.svc.AdminSession(ctx, pending.Token)
	assert.ErrorIs(t, err, auth.ErrInvalidToken, "session fixation: the pre-code cookie must not survive the code")
	got, err := h.svc.AdminSession(ctx, verified.Token)
	require.NoError(t, err)
	assert.True(t, got.TOTPVerified)
	assert.Equal(t, h.admin.ID, got.UserID)
	assert.Empty(t, got.Token, "lookups never hand the raw token back")

	// Enrolment is once: the browser cannot re-issue a secret.
	_, err = h.svc.TOTPConfirm(ctx, verified.Token, h.code(t, enrol.Secret), "127.0.0.1")
	assert.ErrorIs(t, err, auth.ErrTOTPAlreadyEnabled)

	// Logging out revokes; a second logout is a no-op, not an error.
	require.NoError(t, h.svc.AdminLogout(ctx, verified.Token, "127.0.0.1"))
	_, err = h.svc.AdminSession(ctx, verified.Token)
	assert.ErrorIs(t, err, auth.ErrInvalidToken)
	require.NoError(t, h.svc.AdminLogout(ctx, verified.Token, "127.0.0.1"))

	// Every step is on the audit trail (docs/plan-v1.0.md §14).
	for _, action := range []string{"auth.admin.login", "auth.admin.totp.enroll", "auth.admin.totp.failed", "auth.admin.totp.confirmed", "auth.admin.logout"} {
		var n int
		require.NoError(t, h.all.QueryRow(ctx, `SELECT count(*) FROM audit.audit_events WHERE action = $1`, action).Scan(&n))
		assert.Positive(t, n, "expected an audit row for %s", action)
	}
}

// §12: "TOTP 錯碼鎖定". Five wrong codes lock the account for fifteen minutes,
// during which the right code is refused too -- and the lock is per user, so
// nothing an outsider can reach without the password applies it.
func TestAdminTOTPLocksAfterFiveWrongCodes(t *testing.T) {
	ctx := context.Background()
	h := setupAdminAuth(t)
	_, secret := h.enrolAndLogin(t)

	pending, err := h.svc.AdminLogin(ctx, adminEmail, adminPassword, "127.0.0.1")
	require.NoError(t, err)
	for i := 1; i <= 4; i++ {
		_, err := h.svc.TOTPVerify(ctx, pending.Token, "000000", "127.0.0.1")
		assert.ErrorIs(t, err, auth.ErrInvalidTOTP, "attempt %d is just wrong", i)
	}
	_, err = h.svc.TOTPVerify(ctx, pending.Token, "000000", "127.0.0.1")
	assert.ErrorIs(t, err, auth.ErrTOTPLocked, "the fifth locks")

	h.advance(time.Minute)
	_, err = h.svc.TOTPVerify(ctx, pending.Token, h.code(t, secret), "127.0.0.1")
	assert.ErrorIs(t, err, auth.ErrTOTPLocked, "the right code is refused while locked, and not even compared")

	var n int
	require.NoError(t, h.all.QueryRow(ctx, `SELECT count(*) FROM audit.audit_events WHERE action = 'auth.admin.totp.locked'`).Scan(&n))
	assert.Equal(t, 1, n, "the lock is on the audit trail exactly once")

	// The lock runs out; the pending session has too (10 minutes), so log in
	// again and the right code works.
	h.advance(15 * time.Minute)
	_, err = h.svc.AdminSession(ctx, pending.Token)
	assert.ErrorIs(t, err, auth.ErrInvalidToken, "the pending session expired meanwhile")
	pending, err = h.svc.AdminLogin(ctx, adminEmail, adminPassword, "127.0.0.1")
	require.NoError(t, err)
	verified, err := h.svc.TOTPVerify(ctx, pending.Token, h.code(t, secret), "127.0.0.1")
	require.NoError(t, err)
	assert.True(t, verified.TOTPVerified)

	// A wrong password never touches the counter: the lock is for codes.
	for i := 0; i < 10; i++ {
		_, err := h.svc.AdminLogin(ctx, adminEmail, "not the password", "10.0.0.9")
		assert.ErrorIs(t, err, auth.ErrInvalidCredentials)
	}
	pending, err = h.svc.AdminLogin(ctx, adminEmail, adminPassword, "127.0.0.1")
	require.NoError(t, err)
	h.advance(30 * time.Second)
	_, err = h.svc.TOTPVerify(ctx, pending.Token, h.code(t, secret), "127.0.0.1")
	require.NoError(t, err, "ten wrong passwords did not lock the codes")
}

// A code is spent at the step it matched. The same code again -- what
// somebody who watched the screen would try -- is refused, and counts as a
// wrong one.
func TestAdminTOTPRefusesAReplayedCode(t *testing.T) {
	ctx := context.Background()
	h := setupAdminAuth(t)
	_, secret := h.enrolAndLogin(t)
	// Enrolment spent this window's code. The documented side effect of the
	// rule is that a second login inside the same thirty seconds is refused,
	// so the test moves on like a person would.
	h.advance(30 * time.Second)

	pending, err := h.svc.AdminLogin(ctx, adminEmail, adminPassword, "127.0.0.1")
	require.NoError(t, err)
	code := h.code(t, secret)
	_, err = h.svc.TOTPVerify(ctx, pending.Token, code, "127.0.0.1")
	require.NoError(t, err)

	// Same 30-second window, same code, fresh pending session: replay.
	pending, err = h.svc.AdminLogin(ctx, adminEmail, adminPassword, "127.0.0.1")
	require.NoError(t, err)
	_, err = h.svc.TOTPVerify(ctx, pending.Token, code, "127.0.0.1")
	assert.ErrorIs(t, err, auth.ErrInvalidTOTP, "a code already accepted is spent")

	// The next window's code is fine.
	h.advance(30 * time.Second)
	_, err = h.svc.TOTPVerify(ctx, pending.Token, h.code(t, secret), "127.0.0.1")
	require.NoError(t, err)
}

// Every refusal at the password stage is the same refusal. The audit trail
// knows why; the caller does not, because the caller may be guessing.
func TestAdminLoginRefusesUniformly(t *testing.T) {
	ctx := context.Background()
	h := setupAdminAuth(t)

	// A regular user with a correct password is not an administrator.
	hash, err := auth.HashPassword("user password", auth.TestPasswordParams)
	require.NoError(t, err)
	_, err = h.all.Exec(ctx, `INSERT INTO auth.users (email, password_hash, role) VALUES ('alice@example.com', $1, 'user')`, hash)
	require.NoError(t, err)

	for _, tc := range []struct{ name, email, password string }{
		{"unknown email", "nobody@example.com", adminPassword},
		{"wrong password", adminEmail, "wrong"},
		{"not an administrator", "alice@example.com", "user password"},
		{"malformed email", "not an email", adminPassword},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.svc.AdminLogin(ctx, tc.email, tc.password, "127.0.0.1")
			assert.ErrorIs(t, err, auth.ErrInvalidCredentials)
		})
	}

	// A frozen administrator: the live session dies with the status, and a
	// new login is refused like any other.
	verified, _ := h.enrolAndLogin(t)
	_, err = h.all.Exec(ctx, `UPDATE auth.users SET status = 'frozen' WHERE id = $1`, h.admin.ID)
	require.NoError(t, err)
	_, err = h.svc.AdminSession(ctx, verified.Token)
	assert.ErrorIs(t, err, auth.ErrInvalidToken, "freezing an administrator ends their sessions")
	_, err = h.svc.AdminLogin(ctx, adminEmail, adminPassword, "127.0.0.1")
	assert.ErrorIs(t, err, auth.ErrInvalidCredentials)
}

// Frozen means frozen on the public side too. Before this, auth.users.status
// was read by nothing that gated a login: a frozen user could sign in, refresh
// and sign API requests indefinitely.
func TestPublicLoginRefusesAFrozenUser(t *testing.T) {
	ctx := context.Background()
	h := setupAPI(t)
	sess := h.register(t, "frozen@example.com")
	_, err := h.all.Exec(ctx, `UPDATE auth.users SET status = 'frozen' WHERE id = $1`, sess.UserID)
	require.NoError(t, err)

	r := h.login(t, "frozen@example.com", password)
	assert.Equal(t, http.StatusForbidden, r.status, string(r.body))
	assert.Contains(t, string(r.body), "frozen")

	wrong := h.login(t, "frozen@example.com", "wrong password")
	assert.Equal(t, http.StatusUnauthorized, wrong.status, "the wrong password stays a wrong password: frozen is only said once the password is right")

	_, err = h.auth.Refresh(ctx, sess.RefreshToken)
	assert.ErrorIs(t, err, auth.ErrUserFrozen, "a refresh token issued before the freeze is refused")
}

// The grants 0018 made, checked rather than asserted in a comment.
func TestAdminSessionPrivileges(t *testing.T) {
	ctx := context.Background()
	h := setupAdminAuth(t)
	verified, _ := h.enrolAndLogin(t)
	_ = verified

	for _, role := range []string{"ex_api", "ex_chain", "ex_worker", "ex_engine"} {
		t.Run(role+" cannot touch admin sessions", func(t *testing.T) {
			pool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN(role), MaxConns: 1})
			require.NoError(t, err)
			defer pool.Close()
			_, err = pool.Exec(ctx, `SELECT count(*) FROM auth.admin_sessions`)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "permission denied")
			_, err = pool.Exec(ctx, `INSERT INTO auth.admin_sessions (id_hash, user_id, expires_at) VALUES ('\x00', $1, now() + interval '1 hour')`, h.admin.ID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "permission denied")
		})
	}

	// Nobody deletes: revocation is a timestamp.
	_, err := h.adminPool.Exec(ctx, `DELETE FROM auth.admin_sessions`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
}
