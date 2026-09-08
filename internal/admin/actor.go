package admin

import (
	"context"

	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/auth"
)

// apiKeyActorID is recorded on audit events for the static admin API key,
// which has no user behind it.
const apiKeyActorID = "admin-api-key"

// actor is who is doing this, for the audit trail and for the columns that
// name a reviewer. It is derived from the request rather than passed around:
// the same write is reachable from the REST API (a machine holding the static
// key) and from the back office (a person with a session), and the record
// has to say which.
type actor struct {
	Type audit.ActorType
	ID   string
	IP   string
}

// actorFrom reads the caller off the context. A back-office session names
// the administrator; anything else is the API key.
func actorFrom(ctx context.Context) actor {
	a := actor{Type: audit.ActorAPIKey, ID: apiKeyActorID, IP: clientIPFrom(ctx)}
	if p, ok := auth.PrincipalFrom(ctx); ok && p.Method == auth.MethodAdminSession {
		a.Type, a.ID = audit.ActorAdmin, p.UserID
	}
	return a
}

// UserID is the administrator's user id when a person is acting, and empty
// for the API key -- the value reviewed_by and friends want.
func (a actor) UserID() string {
	if a.Type == audit.ActorAdmin {
		return a.ID
	}
	return ""
}

type ctxKey int

const (
	clientIPKey ctxKey = iota
	sessionKey
	langKey
)

// clientIPFrom returns the address WithClientIP recorded, or "".
func clientIPFrom(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey).(string)
	return ip
}
