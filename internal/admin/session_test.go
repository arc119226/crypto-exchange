package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/auth"
)

// fakeSessions is auth.Service as the middleware sees it: token -> session.
type fakeSessions map[string]auth.AdminSession

func (f fakeSessions) AdminSession(_ context.Context, token string) (auth.AdminSession, error) {
	if s, ok := f[token]; ok {
		return s, nil
	}
	if token == "boom" {
		return auth.AdminSession{}, errors.New("database is away")
	}
	return auth.AdminSession{}, auth.ErrInvalidToken
}

func serve(t *testing.T, mw func(http.Handler) http.Handler, cookie, hx string) *httptest.ResponseRecorder {
	t.Helper()
	var seen actor
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = actorFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
	}
	if hx != "" {
		req.Header.Set("HX-Request", hx)
	}
	rec := httptest.NewRecorder()
	WithClientIP(mw(inner)).ServeHTTP(rec, req)
	if rec.Code == http.StatusNoContent {
		assert.Equal(t, "admin", string(seen.Type), "a session names the administrator as the actor")
		assert.Equal(t, "u-1", seen.ID)
		assert.NotEmpty(t, seen.IP)
	}
	return rec
}

func TestRequireAdminAdmitsOnlyAVerifiedSession(t *testing.T) {
	sessions := fakeSessions{
		"verified": {UserID: "u-1", TOTPVerified: true},
		"pending":  {UserID: "u-1", TOTPVerified: false},
	}
	mw := RequireAdmin(sessions, Cookies{})

	assert.Equal(t, http.StatusNoContent, serve(t, mw, "verified", "").Code)

	rec := serve(t, mw, "pending", "")
	assert.Equal(t, http.StatusSeeOther, rec.Code, "a password without a code is sent to the code page")
	assert.Equal(t, TOTPPath, rec.Header().Get("Location"))

	rec = serve(t, mw, "", "")
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, LoginPath, rec.Header().Get("Location"))

	rec = serve(t, mw, "expired-or-forged", "")
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, LoginPath, rec.Header().Get("Location"))
	// The dead cookie is cleared, so the browser stops presenting it.
	require.NotEmpty(t, rec.Result().Cookies())
	assert.Equal(t, -1, rec.Result().Cookies()[0].MaxAge)

	rec = serve(t, mw, "boom", "")
	assert.Equal(t, http.StatusInternalServerError, rec.Code, "a database error is not an authentication failure")
}

func TestRequirePendingAdmitsEitherStage(t *testing.T) {
	sessions := fakeSessions{
		"verified": {UserID: "u-1", TOTPVerified: true},
		"pending":  {UserID: "u-1", TOTPVerified: false},
	}
	mw := RequirePending(sessions, Cookies{})
	assert.Equal(t, http.StatusNoContent, serve(t, mw, "verified", "").Code)
	assert.Equal(t, http.StatusNoContent, serve(t, mw, "pending", "").Code)
	assert.Equal(t, http.StatusSeeOther, serve(t, mw, "", "").Code)
}

// htmx swaps whatever comes back into the element that asked. A 302 to the
// login page would put the login form inside a table; HX-Redirect makes the
// browser navigate instead.
func TestRedirectSpeaksHTMX(t *testing.T) {
	mw := RequireAdmin(fakeSessions{}, Cookies{})
	rec := serve(t, mw, "", "true")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, LoginPath, rec.Header().Get("HX-Redirect"))
	assert.Empty(t, rec.Header().Get("Location"))
}

func TestActorDefaultsToTheAPIKey(t *testing.T) {
	a := actorFrom(context.Background())
	assert.Equal(t, "api_key", string(a.Type))
	assert.Equal(t, apiKeyActorID, a.ID)
	assert.Empty(t, a.UserID(), "the API key has no user to put in reviewed_by")

	ctx := auth.WithPrincipal(context.Background(), auth.Principal{UserID: "u-9", Role: auth.RoleAdmin, Method: auth.MethodAdminSession})
	a = actorFrom(ctx)
	assert.Equal(t, "admin", string(a.Type))
	assert.Equal(t, "u-9", a.UserID())

	// A public-API principal that somehow reached here is still not an admin.
	ctx = auth.WithPrincipal(context.Background(), auth.Principal{UserID: "u-9", Role: auth.RoleAdmin, Method: auth.MethodJWT})
	assert.Equal(t, "api_key", string(actorFrom(ctx).Type))
}

func TestSessionCookieFlags(t *testing.T) {
	rec := httptest.NewRecorder()
	Cookies{Secure: true}.Set(rec, "tok", time.Now().Add(time.Hour))
	c := rec.Result().Cookies()[0]
	assert.Equal(t, SessionCookie, c.Name)
	assert.True(t, c.HttpOnly)
	assert.True(t, c.Secure)
	assert.Equal(t, http.SameSiteLaxMode, c.SameSite)
	assert.Equal(t, "/admin", c.Path)
}

func TestClearMatchesSet(t *testing.T) {
	rec := httptest.NewRecorder()
	Cookies{Secure: true}.Clear(rec)
	c := rec.Result().Cookies()[0]
	assert.Equal(t, -1, c.MaxAge)
	assert.True(t, c.Secure, "a plain-http origin cannot overwrite a Secure cookie, so clearing must carry the same flag")
	assert.Equal(t, "/admin", c.Path)
}
