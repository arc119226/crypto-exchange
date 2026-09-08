package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/arc119226/crypto-exchange/internal/auth/sqlcgen"
)

// The operator's view of users (docs/plan-v1.0.md §12 Phase 5). These run
// inside the caller's transaction: the admin write that calls them records
// the audit row and the event in the same one, and a status change also
// freezes the user's ledger account there, so no reader can see one half.

// ErrLastAdmin is returned by SetUserStatus when freezing this administrator
// would leave nobody able to sign in to the back office.
var ErrLastAdmin = errors.New("auth: cannot freeze the last active administrator")

// MaxKYCLevel is the highest level a user can hold; migration 0007 checks
// the same bound, and the withdrawal limits are seeded per level up to it.
const MaxKYCLevel = 2

// UserFilter narrows ListUsers. Empty fields do not filter. Email is a
// fragment matched anywhere in the address, with no wildcards.
type UserFilter struct {
	Email  string
	Status string
	Role   string
}

// ListUsers is the directory: newest first.
func (s *Service) ListUsers(ctx context.Context, f UserFilter, limit, offset int32) ([]User, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := sqlcgen.New(s.pool).ListUsers(ctx, sqlcgen.ListUsersParams{
		TenantID: s.cfg.Tenant, Email: f.Email, Status: f.Status, Role: f.Role, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: list users: %w", err)
	}
	out := make([]User, 0, len(rows))
	for _, r := range rows {
		out = append(out, userFromRow(r))
	}
	return out, nil
}

// SetUserStatus freezes or releases a user. Frozen means: cannot log in,
// cannot refresh a token, cannot use an API key (an access token already
// issued lasts its fifteen minutes). The caller freezes the ledger account in
// the same transaction; this refuses to freeze the last active administrator
// and ends an administrator's back-office sessions.
//
// changed is false when the user was already in that status: no audit row,
// no event, nothing to cascade.
func (s *Service) SetUserStatus(ctx context.Context, tx pgx.Tx, id, status string) (before, after User, changed bool, err error) {
	if status != StatusActive && status != StatusFrozen {
		return User{}, User{}, false, fmt.Errorf("%w: status must be active or frozen", ErrInvalidInput)
	}
	q := sqlcgen.New(tx)
	before, err = s.userInTenant(ctx, q, id)
	if err != nil {
		return User{}, User{}, false, err
	}
	if before.Status == status {
		return before, before, false, nil
	}
	if status == StatusFrozen && before.Role == RoleAdmin {
		n, err := q.CountActiveAdmins(ctx, s.cfg.Tenant)
		if err != nil {
			return User{}, User{}, false, fmt.Errorf("auth: count admins: %w", err)
		}
		if n <= 1 {
			return User{}, User{}, false, ErrLastAdmin
		}
		if _, err := q.RevokeUserAdminSessions(ctx, id); err != nil {
			return User{}, User{}, false, fmt.Errorf("auth: revoke sessions: %w", err)
		}
	}
	row, err := q.UpdateUserStatus(ctx, sqlcgen.UpdateUserStatusParams{ID: id, Status: status})
	if err != nil {
		return User{}, User{}, false, fmt.Errorf("auth: set user status: %w", err)
	}
	return before, userFromRow(row), true, nil
}

// SetUserKYCLevel moves a user between levels. The withdrawal policy reads
// the level on each request, so the next withdrawal sees it; nothing already
// pending is re-decided.
func (s *Service) SetUserKYCLevel(ctx context.Context, tx pgx.Tx, id string, level int) (before, after User, changed bool, err error) {
	if level < 0 || level > MaxKYCLevel {
		return User{}, User{}, false, fmt.Errorf("%w: kyc_level must be between 0 and %d", ErrInvalidInput, MaxKYCLevel)
	}
	q := sqlcgen.New(tx)
	before, err = s.userInTenant(ctx, q, id)
	if err != nil {
		return User{}, User{}, false, err
	}
	if before.KYCLevel == level {
		return before, before, false, nil
	}
	row, err := q.UpdateUserKYCLevel(ctx, sqlcgen.UpdateUserKYCLevelParams{ID: id, KycLevel: int16(level)}) //nolint:gosec // bounded above
	if err != nil {
		return User{}, User{}, false, fmt.Errorf("auth: set kyc level: %w", err)
	}
	return before, userFromRow(row), true, nil
}

// userInTenant reads a user through q and refuses one from another tenant
// with the same answer as one that does not exist.
func (s *Service) userInTenant(ctx context.Context, q *sqlcgen.Queries, id string) (User, error) {
	row, err := q.GetUser(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || pgErrorCode(err) == "22P02" {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("auth: get user: %w", err)
	}
	if row.TenantID != s.cfg.Tenant {
		return User{}, ErrNotFound
	}
	return userFromRow(row), nil
}
