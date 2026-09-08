package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/arc119226/crypto-exchange/internal/auth"
)

// Back-office sessions over HTTP (docs/plan-v1.0.md §12 Phase 5, §14).
//
// internal/auth decides whether a token is a live administrator; this file
// decides what that means for a browser: which cookie, which redirect, and
// which pages a half-logged-in person may reach. auth stays HTTP-agnostic the
// way its Authenticate middleware does, through a small interface here.

// SessionCookie is the cookie that carries the opaque session token.
const SessionCookie = "admin_session"

// Paths the session middleware sends people to.
const (
	LoginPath  = "/admin/login"
	TOTPPath   = "/admin/totp"
	LogoutPath = "/admin/logout"
	HomePath   = "/admin/"
)

// SessionResolver is the part of auth.Service the middleware needs.
type SessionResolver interface {
	AdminSession(ctx context.Context, token string) (auth.AdminSession, error)
}

// SessionFrom returns the administrator's session, when a session middleware
// let the request through.
func SessionFrom(ctx context.Context) (auth.AdminSession, bool) {
	s, ok := ctx.Value(sessionKey).(auth.AdminSession)
	return s, ok
}

// WithClientIP records the remote address for audit rows. The admin listener
// is reached directly, never through a proxy that sets X-Forwarded-For, so
// the socket's peer is the truth (auth.ClientIP ignores the header on
// purpose).
func WithClientIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey, auth.ClientIP(r))))
	})
}

// RequireAdmin admits only a session that has passed its TOTP code. Anything
// else is sent to log in -- or, for a session that has the password and owes
// the code, to the code page. A pending session must not reach a page that
// does anything; it is a person who is allowed to try a code.
func RequireAdmin(sessions SessionResolver, cookies Cookies) func(http.Handler) http.Handler {
	return requireSession(sessions, cookies, true)
}

// RequirePending admits a session at either stage. It guards the code pages:
// a person there has proved the password and nothing more.
func RequirePending(sessions SessionResolver, cookies Cookies) func(http.Handler) http.Handler {
	return requireSession(sessions, cookies, false)
}

func requireSession(sessions SessionResolver, cookies Cookies, verified bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(SessionCookie)
			if err != nil {
				redirect(w, r, LoginPath)
				return
			}
			sess, err := sessions.AdminSession(r.Context(), c.Value)
			switch {
			case errors.Is(err, auth.ErrInvalidToken):
				// Dead cookie: clear it so the browser stops presenting it.
				cookies.Clear(w)
				redirect(w, r, LoginPath)
				return
			case err != nil:
				WriteProblem(w, r, http.StatusInternalServerError, "Internal Server Error", "")
				return
			}
			if verified && !sess.TOTPVerified {
				redirect(w, r, TOTPPath)
				return
			}
			p := auth.Principal{UserID: sess.UserID, Role: auth.RoleAdmin, Method: auth.MethodAdminSession}
			ctx := auth.WithPrincipal(r.Context(), p)
			ctx = context.WithValue(ctx, sessionKey, sess)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// redirect sends a browser to path. An htmx request gets HX-Redirect
// instead: htmx follows a 302 itself and would swap the login page into
// whatever element asked for a table.
func redirect(w http.ResponseWriter, r *http.Request, path string) {
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", path)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// Cookies is how this deployment sets the session cookie. Secure is a
// decision, not a detection: the admin listener has no TLS of its own, so
// "when the connection is TLS" would never fire, and the flag says what sits
// in front of it (ADMIN_COOKIE_SECURE; off for dev on plain localhost).
type Cookies struct {
	Secure bool
}

// Set writes the session token. HttpOnly keeps scripts away from it;
// SameSite=Lax keeps cross-site POSTs from carrying it (the router's
// CrossOriginProtection is the second line).
func (c Cookies) Set(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is the deployment's call, see Cookies
		Name: SessionCookie, Value: token, Path: "/admin", Expires: expires,
		HttpOnly: true, Secure: c.Secure, SameSite: http.SameSiteLaxMode,
	})
}

// Clear tells the browser to forget the session. Same attributes as Set: a
// plain-http origin cannot overwrite a Secure cookie, so the two must agree.
func (c Cookies) Clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: must match Set, see Cookies
		Name: SessionCookie, Value: "", Path: "/admin", MaxAge: -1,
		HttpOnly: true, Secure: c.Secure, SameSite: http.SameSiteLaxMode,
	})
}
