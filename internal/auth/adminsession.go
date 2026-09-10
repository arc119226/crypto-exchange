package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/auth/sqlcgen"
)

// Administrator sessions (docs/plan-v1.0.md §12 Phase 5, §14, ADR-0006).
//
// A human administrator authenticates twice: a password, then a TOTP code.
// Between the two there is a *pending* session -- a person who is allowed to
// try a code and reach nothing else. Passing the code does not mark that
// session verified; it retires it and mints a new one, so a cookie captured
// before the code was entered is worth nothing afterwards (session fixation).
//
// The secret is never generated here. `exchange admin totp enroll` issues it
// with database credentials on the operator's terminal, and the browser only
// ever confirms or verifies. Otherwise anyone holding the password of an
// administrator who had not yet enrolled could enrol themselves and finish
// the job the password alone was not supposed to do.

const (
	// adminPendingTTL is how long a password-only session lives. Long enough
	// to find a phone, short enough that a stolen one is not a standing
	// invitation.
	adminPendingTTL = 10 * time.Minute
	// totpMaxFailures wrong codes in a row lock the account for totpLockout.
	// Only codes count: an outsider with the email and no password never
	// reaches this step, so the lock cannot be applied from outside.
	totpMaxFailures = 5
	totpLockout     = 15 * time.Minute
	// adminSessionTouch bounds how often last_seen_at is written: once a
	// minute, not once a request.
	adminSessionTouch = time.Minute
)

// AdminSession is what the back office knows about a logged-in administrator.
type AdminSession struct {
	// Token is the raw cookie value. Set only by the call that minted the
	// session; every later lookup leaves it empty.
	Token string
	// ExpiresAt is when the cookie should die.
	ExpiresAt time.Time
	UserID    string
	Email     string
	// TOTPEnabled: the administrator has completed enrolment.
	TOTPEnabled bool
	// TOTPPending: a secret has been issued by the CLI and awaits its first
	// code. Mutually exclusive with TOTPEnabled.
	TOTPPending bool
	// TOTPVerified: this session has passed a code. Only such a session is an
	// administrator; a pending one may reach the code page and nothing else.
	TOTPVerified bool
}

// Errors of the admin flow, on top of ErrInvalidCredentials / ErrInvalidToken.
var (
	// ErrTOTPNotEnrolled: no secret to check against; run `exchange admin
	// totp enroll`.
	ErrTOTPNotEnrolled = errors.New("auth: totp not enrolled")
	// ErrTOTPAlreadyEnabled: TOTPConfirm on an administrator who is past
	// enrolment. Re-enrolment goes through the CLI, never the browser.
	ErrTOTPAlreadyEnabled = errors.New("auth: totp already enabled")
	ErrInvalidTOTP        = errors.New("auth: invalid code")
	ErrTOTPLocked         = errors.New("auth: too many wrong codes; locked")
	// ErrTOTPDisabled: the service has no ADMIN_TOTP_KEY, so it can neither
	// seal a new secret nor open a stored one.
	ErrTOTPDisabled = errors.New("auth: totp is not configured (ADMIN_TOTP_KEY)")
)

// AdminLogin checks the password and opens a pending session. Every refusal
// is ErrInvalidCredentials -- a wrong password, an unknown email, a user who
// is not an administrator, a frozen one -- and each costs the same argon2
// work, so nothing about the directory leaks through timing or wording. The
// audit record has the real reason.
func (s *Service) AdminLogin(ctx context.Context, email, password, ip string) (AdminSession, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return AdminSession{}, ErrInvalidCredentials
	}
	q := sqlcgen.New(s.pool)
	row, err := q.GetUserByEmail(ctx, sqlcgen.GetUserByEmailParams{TenantID: s.cfg.Tenant, Email: email})
	hash := s.dummy
	found := err == nil
	if found {
		hash = row.PasswordHash
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return AdminSession{}, fmt.Errorf("auth: get user: %w", err)
	}
	ok, err := VerifyPassword(hash, password)
	if err != nil {
		return AdminSession{}, err
	}
	reason := ""
	switch {
	case !found:
		reason = "unknown email"
	case !ok:
		reason = "wrong password"
	case row.Role != RoleAdmin:
		reason = "not an administrator"
	case row.Status != "active":
		reason = "user " + row.Status
	}
	if reason != "" {
		if found {
			// A failed admin login is rejected regardless; the audit row is
			// how somebody later sees it happened. Never fail the rejection
			// over it, never lose it quietly either.
			if err := s.audit.Record(ctx, s.pool, audit.Event{
				ActorType: audit.ActorUser, ActorID: row.ID, Action: "auth.admin.login.failed",
				TargetType: "user", TargetID: row.ID, IP: ip, After: map[string]any{"reason": reason},
			}); err != nil {
				s.log.Warn("audit write failed", "action", "auth.admin.login.failed", "user_id", row.ID, "reason", reason, "error", err)
			}
		}
		return AdminSession{}, ErrInvalidCredentials
	}

	var sess AdminSession
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		if sess, err = s.insertAdminSession(ctx, tx, row, false, ip); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorAdmin, ActorID: row.ID, Action: "auth.admin.login",
			TargetType: "user", TargetID: row.ID, IP: ip,
		})
	})
	if err != nil {
		return AdminSession{}, err
	}
	return sess, nil
}

// AdminSession resolves a cookie. ErrInvalidToken for anything that is not a
// live session of a live administrator: unknown, expired, revoked, or the
// user has since been frozen or demoted.
func (s *Service) AdminSession(ctx context.Context, token string) (AdminSession, error) {
	if token == "" {
		return AdminSession{}, ErrInvalidToken
	}
	q := sqlcgen.New(s.pool)
	row, err := q.GetAdminSessionByHash(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AdminSession{}, ErrInvalidToken
		}
		return AdminSession{}, fmt.Errorf("auth: get admin session: %w", err)
	}
	now := s.now()
	if row.RevokedAt.Valid || !row.ExpiresAt.After(now) || row.TenantID != s.cfg.Tenant {
		return AdminSession{}, ErrInvalidToken
	}
	urow, err := q.GetUser(ctx, row.UserID)
	if err != nil {
		return AdminSession{}, fmt.Errorf("auth: get user: %w", err)
	}
	if urow.Role != RoleAdmin || urow.Status != "active" {
		return AdminSession{}, ErrInvalidToken
	}
	if now.Sub(row.LastSeenAt) > adminSessionTouch {
		_ = q.TouchAdminSession(ctx, row.IDHash)
	}
	return adminSessionFrom(row, urow), nil
}

// TOTPConfirm completes enrolment: the first code proves the authenticator
// holds the secret the CLI issued. On success the pending session is retired
// and a verified one returned.
func (s *Service) TOTPConfirm(ctx context.Context, token, code, ip string) (AdminSession, error) {
	return s.checkCode(ctx, token, code, ip, true)
}

// TOTPVerify is the second half of every login.
func (s *Service) TOTPVerify(ctx context.Context, token, code, ip string) (AdminSession, error) {
	return s.checkCode(ctx, token, code, ip, false)
}

// checkCode is TOTPConfirm and TOTPVerify: the same check, differing only in
// which state the user must be in and what success writes.
func (s *Service) checkCode(ctx context.Context, token, code, ip string, enrolling bool) (AdminSession, error) {
	if len(s.cfg.TOTPKey) == 0 {
		return AdminSession{}, ErrTOTPDisabled
	}
	sess, err := s.AdminSession(ctx, token)
	if err != nil {
		return AdminSession{}, err
	}
	q := sqlcgen.New(s.pool)
	urow, err := q.GetUser(ctx, sess.UserID)
	if err != nil {
		return AdminSession{}, fmt.Errorf("auth: get user: %w", err)
	}
	switch {
	case enrolling && urow.TotpEnabled:
		return AdminSession{}, ErrTOTPAlreadyEnabled
	case !enrolling && !urow.TotpEnabled, len(urow.TotpSecretEnc) == 0:
		return AdminSession{}, ErrTOTPNotEnrolled
	}
	now := s.now()
	if urow.TotpLockedUntil.Valid && urow.TotpLockedUntil.Time.After(now) {
		return AdminSession{}, ErrTOTPLocked
	}
	secret, err := openSecret(s.totp, urow.TotpSecretEnc)
	if err != nil {
		return AdminSession{}, err
	}
	step, ok := totpMatch(secret, code, now)
	// A code for a step already spent is a replay, whatever its arithmetic
	// says, and it counts as a wrong one.
	if ok && urow.TotpLastStep != nil && step <= *urow.TotpLastStep {
		ok = false
	}
	if !ok {
		return AdminSession{}, s.recordTOTPFailure(ctx, urow.ID, ip, now)
	}

	var next AdminSession
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		qt := sqlcgen.New(tx)
		action := "auth.admin.totp.verified"
		if enrolling {
			action = "auth.admin.totp.confirmed"
			if err := qt.EnableTOTP(ctx, sqlcgen.EnableTOTPParams{ID: urow.ID, TotpLastStep: &step}); err != nil {
				return fmt.Errorf("auth: enable totp: %w", err)
			}
		} else if err := qt.RecordTOTPSuccess(ctx, sqlcgen.RecordTOTPSuccessParams{ID: urow.ID, TotpLastStep: &step}); err != nil {
			return fmt.Errorf("auth: record totp: %w", err)
		}
		// Rotate: the cookie that carried the password-only session dies
		// here, and the one that is an administrator was never in a browser
		// before the code was entered.
		if _, err := qt.RevokeAdminSession(ctx, hashToken(token)); err != nil {
			return fmt.Errorf("auth: revoke admin session: %w", err)
		}
		var err error
		if next, err = s.insertAdminSession(ctx, tx, urow, true, ip); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorAdmin, ActorID: urow.ID, Action: action,
			TargetType: "user", TargetID: urow.ID, IP: ip,
		})
	})
	if err != nil {
		return AdminSession{}, err
	}
	next.TOTPEnabled, next.TOTPPending = true, false
	return next, nil
}

// recordTOTPFailure counts the miss, locks at the limit, and returns the
// error the caller should surface.
func (s *Service) recordTOTPFailure(ctx context.Context, userID, ip string, now time.Time) error {
	res, err := sqlcgen.New(s.pool).RecordTOTPFailure(ctx, sqlcgen.RecordTOTPFailureParams{
		ID: userID, MaxFailures: totpMaxFailures, LockUntil: now.Add(totpLockout),
	})
	if err != nil {
		return fmt.Errorf("auth: record totp failure: %w", err)
	}
	locked := res.TotpLockedUntil.Valid && res.TotpLockedUntil.Time.After(now)
	action := "auth.admin.totp.failed"
	if locked {
		action = "auth.admin.totp.locked"
	}
	if err := s.audit.Record(ctx, s.pool, audit.Event{
		ActorType: audit.ActorUser, ActorID: userID, Action: action,
		TargetType: "user", TargetID: userID, IP: ip,
		After: map[string]any{"failures": res.TotpFailures},
	}); err != nil {
		s.log.Warn("audit write failed", "action", action, "user_id", userID, "failures", res.TotpFailures, "error", err)
	}
	if locked {
		return ErrTOTPLocked
	}
	return ErrInvalidTOTP
}

// AdminLogout revokes the session. Unknown tokens are not an error: the
// cookie is gone either way.
func (s *Service) AdminLogout(ctx context.Context, token, ip string) error {
	q := sqlcgen.New(s.pool)
	row, err := q.GetAdminSessionByHash(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("auth: get admin session: %w", err)
	}
	n, err := q.RevokeAdminSession(ctx, row.IDHash)
	if err != nil {
		return fmt.Errorf("auth: revoke admin session: %w", err)
	}
	if n == 1 {
		if err := s.audit.Record(ctx, s.pool, audit.Event{
			ActorType: audit.ActorAdmin, ActorID: row.UserID, Action: "auth.admin.logout",
			TargetType: "user", TargetID: row.UserID, IP: ip,
		}); err != nil {
			s.log.Warn("audit write failed", "action", "auth.admin.logout", "user_id", row.UserID, "error", err)
		}
	}
	return nil
}

// EnrollTOTP issues a fresh secret for an administrator and returns the one
// copy of it that will ever exist in the clear. It is the CLI's, not the
// browser's: the caller has database credentials, which is a higher bar than
// a password. Any existing enrolment is replaced and every session of the
// user revoked -- a new secret means starting over.
func (s *Service) EnrollTOTP(ctx context.Context, email string) (TOTPEnrolment, error) {
	if len(s.cfg.TOTPKey) == 0 {
		return TOTPEnrolment{}, ErrTOTPDisabled
	}
	email, err := NormalizeEmail(email)
	if err != nil {
		return TOTPEnrolment{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	q := sqlcgen.New(s.pool)
	row, err := q.GetUserByEmail(ctx, sqlcgen.GetUserByEmailParams{TenantID: s.cfg.Tenant, Email: email})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TOTPEnrolment{}, ErrNotFound
		}
		return TOTPEnrolment{}, fmt.Errorf("auth: get user: %w", err)
	}
	if row.Role != RoleAdmin {
		return TOTPEnrolment{}, fmt.Errorf("%w: %s is not an administrator", ErrInvalidInput, email)
	}
	key, err := newTOTPKey(s.cfg.Issuer, email)
	if err != nil {
		return TOTPEnrolment{}, err
	}
	sealed, err := sealSecret(s.totp, key.Secret())
	if err != nil {
		return TOTPEnrolment{}, err
	}
	png, err := qrPNG(key)
	if err != nil {
		return TOTPEnrolment{}, err
	}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		qt := sqlcgen.New(tx)
		if err := qt.SetPendingTOTPSecret(ctx, sqlcgen.SetPendingTOTPSecretParams{ID: row.ID, TotpSecretEnc: sealed}); err != nil {
			return fmt.Errorf("auth: set totp secret: %w", err)
		}
		if _, err := qt.RevokeUserAdminSessions(ctx, row.ID); err != nil {
			return fmt.Errorf("auth: revoke admin sessions: %w", err)
		}
		return s.audit.Record(ctx, tx, audit.Event{
			ActorType: audit.ActorSystem, ActorID: "cli", Action: "auth.admin.totp.enroll",
			TargetType: "user", TargetID: row.ID, After: map[string]any{"email": email, "replaced": row.TotpEnabled},
		})
	})
	if err != nil {
		return TOTPEnrolment{}, err
	}
	return TOTPEnrolment{Secret: key.Secret(), URL: key.URL(), PNG: png}, nil
}

// insertAdminSession mints a session row and returns it with the raw token.
func (s *Service) insertAdminSession(ctx context.Context, tx pgx.Tx, u sqlcgen.AuthUser, verified bool, ip string) (AdminSession, error) {
	raw, err := newOpaqueToken()
	if err != nil {
		return AdminSession{}, err
	}
	now := s.now()
	p := sqlcgen.InsertAdminSessionParams{
		IDHash: hashToken(raw), TenantID: u.TenantID, UserID: u.ID, Ip: ip,
		ExpiresAt: now.Add(adminPendingTTL),
	}
	if verified {
		p.ExpiresAt = now.Add(s.cfg.AdminSessionTTL)
		p.TotpVerifiedAt = pgtype.Timestamptz{Time: now, Valid: true}
	}
	row, err := sqlcgen.New(tx).InsertAdminSession(ctx, p)
	if err != nil {
		return AdminSession{}, fmt.Errorf("auth: insert admin session: %w", err)
	}
	sess := adminSessionFrom(row, u)
	sess.Token = raw
	return sess, nil
}

func adminSessionFrom(row sqlcgen.AuthAdminSession, u sqlcgen.AuthUser) AdminSession {
	return AdminSession{
		ExpiresAt: row.ExpiresAt, UserID: u.ID, Email: u.Email,
		TOTPEnabled: u.TotpEnabled, TOTPPending: !u.TotpEnabled && len(u.TotpSecretEnc) > 0,
		TOTPVerified: row.TotpVerifiedAt.Valid,
	}
}
