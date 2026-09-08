package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// Scope limits what an API key may do. JWT sessions hold every scope.
type Scope string

// Scopes (docs/plan-v1.0.md §7.4).
const (
	ScopeRead     Scope = "read"
	ScopeTrade    Scope = "trade"
	ScopeWithdraw Scope = "withdraw"
)

// AllScopes in canonical order.
var AllScopes = []Scope{ScopeRead, ScopeTrade, ScopeWithdraw}

// ParseScopes validates and normalises a scope list (deduplicated, canonical
// order); an empty list is an error.
func ParseScopes(in []string) ([]Scope, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("%w: at least one scope", ErrInvalidInput)
	}
	set := map[Scope]bool{}
	for _, s := range in {
		sc := Scope(s)
		if !slices.Contains(AllScopes, sc) {
			return nil, fmt.Errorf("%w: unknown scope %q", ErrInvalidInput, s)
		}
		set[sc] = true
	}
	out := make([]Scope, 0, len(set))
	for _, sc := range AllScopes {
		if set[sc] {
			out = append(out, sc)
		}
	}
	return out, nil
}

// Method says how a request was authenticated.
type Method string

// Authentication methods.
const (
	MethodJWT    Method = "jwt"
	MethodAPIKey Method = "api_key"
	// MethodAdminSession is a back-office browser session (password + TOTP).
	// It carries no scopes and no account: it is an operator, not a trader.
	MethodAdminSession Method = "admin_session"
)

// Roles.
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// User statuses (auth.users.status). Frozen means frozen: no login, no
// refresh, no API key, and the spot account is frozen with it.
const (
	StatusActive = "active"
	StatusFrozen = "frozen"
)

// Principal is the authenticated caller as the rest of the API sees it.
// Everything below the API layer keys on AccountID.
type Principal struct {
	UserID    string
	AccountID string
	TenantID  string
	Role      string
	Method    Method
	APIKeyID  string // set for API key requests
	Scopes    []Scope
}

// Has reports whether the principal may perform actions of the scope.
func (p Principal) Has(scope Scope) bool { return slices.Contains(p.Scopes, scope) }

// IsAdmin reports the admin role.
func (p Principal) IsAdmin() bool { return p.Role == RoleAdmin }

type ctxKey int

const principalKey ctxKey = iota

// WithPrincipal stores the principal in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// PrincipalFrom returns the request's principal, if it authenticated.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok
}

// Errors returned by the service. The API maps them to 400/401/403/404/409.
var (
	ErrInvalidInput       = errors.New("auth: invalid input")
	ErrEmailTaken         = errors.New("auth: email already registered")
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrInvalidToken       = errors.New("auth: invalid or expired token")
	ErrForbidden          = errors.New("auth: forbidden")
	ErrNotFound           = errors.New("auth: not found")
	ErrUserFrozen         = errors.New("auth: user is frozen")
)
