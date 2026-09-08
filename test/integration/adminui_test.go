//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/admin"
	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/ratelimit"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// browser is an http.Client that keeps cookies and stops at redirects, so a
// test can see each hop of the login the way the middleware meant it.
type browser struct {
	t   *testing.T
	c   *http.Client
	srv *httptest.Server
}

func newBrowser(t *testing.T, srv *httptest.Server) *browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return &browser{t: t, c: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, srv: srv}
}

func (b *browser) do(req *http.Request) (*http.Response, string) {
	b.t.Helper()
	resp, err := b.c.Do(req)
	require.NoError(b.t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(b.t, err)
	return resp, string(body)
}

func (b *browser) get(path string, hdr ...string) (*http.Response, string) {
	b.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, b.srv.URL+path, nil)
	require.NoError(b.t, err)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return b.do(req)
}

func (b *browser) post(path string, form url.Values, hdr ...string) (*http.Response, string) {
	b.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, b.srv.URL+path, strings.NewReader(form.Encode()))
	require.NoError(b.t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return b.do(req)
}

func (b *browser) cookie(name string) *http.Cookie {
	b.t.Helper()
	u, err := url.Parse(b.srv.URL + "/admin/")
	require.NoError(b.t, err)
	for _, c := range b.c.Jar.Cookies(u) {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// adminUIServer is the admin listener as admin_role.go builds it, on the
// ex_admin pool: API key on /admin/v1, session on /admin.
func adminUIServer(t *testing.T, h *adminAuthHarness, limit ratelimit.Limit) *httptest.Server {
	t.Helper()
	l := ledger.New(h.adminPool, "default")
	require.NoError(t, l.LoadHouseAccounts(context.Background()))
	rec := audit.NewRecorder("default")
	handler := admin.NewHandler(h.adminPool, l, registry.NewStore(h.adminPool), rec, "default").WithUsers(h.svc)
	ui, err := admin.NewUI(handler, h.svc, admin.UIConfig{LoginLimit: limit})
	require.NoError(t, err)
	r := chi.NewRouter()
	r.Use(telemetry.CorrelationMiddleware(slog.New(slog.NewTextHandler(io.Discard, nil))))
	admin.Routes(r, handler, ui, adminKey)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// loginLimitForTests is wide enough that no page test trips it.
var loginLimitForTests = ratelimit.Limit{N: 100, Window: time.Minute}

// signIn walks the whole login for a page test: CLI-issued secret, password,
// first code. The browser leaves with a verified cookie.
func signIn(t *testing.T, b *browser, h *adminAuthHarness) {
	t.Helper()
	enrol, err := h.svc.EnrollTOTP(context.Background(), adminEmail)
	require.NoError(t, err)
	resp, _ := b.post("/admin/login", url.Values{"email": {adminEmail}, "password": {adminPassword}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp, _ = b.post("/admin/totp", url.Values{"code": {h.code(t, enrol.Secret)}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, admin.HomePath, resp.Header.Get("Location"))
}

// The login, through a browser: password, pending cookie, code from the
// CLI-issued secret, verified cookie, dashboard. Every hop is checked for
// what it must and must not allow.
func TestAdminUILoginFlow(t *testing.T) {
	h := setupAdminAuth(t)
	srv := adminUIServer(t, h, ratelimit.Limit{N: 100, Window: time.Minute})
	b := newBrowser(t, srv)

	t.Run("nothing but the login page without a session", func(t *testing.T) {
		resp, _ := b.get("/admin/")
		assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, admin.LoginPath, resp.Header.Get("Location"))
		resp, _ = b.get("/admin/totp")
		assert.Equal(t, admin.LoginPath, resp.Header.Get("Location"))
		resp, body := b.get("/admin/login")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, body, `name="password"`)
	})

	t.Run("the API group still wants the key, not a cookie", func(t *testing.T) {
		resp, body := b.get("/admin/v1/system/status")
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Contains(t, resp.Header.Get("Content-Type"), "problem+json")
		assert.Contains(t, body, "X-Admin-Api-Key")
		resp, body = b.get("/admin/v1/system/status", "X-Admin-Api-Key", adminKey)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, body, `"ledger_balanced":true`)
		assert.Contains(t, body, `"open_ledger_breaks":0`)
	})

	t.Run("a wrong password is refused without a cookie", func(t *testing.T) {
		resp, body := b.post("/admin/login", url.Values{"email": {adminEmail}, "password": {"nope"}})
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Contains(t, body, "Wrong email or password.")
		assert.Nil(t, b.cookie(admin.SessionCookie))
	})

	var pendingToken string
	t.Run("the password earns a pending cookie", func(t *testing.T) {
		resp, _ := b.post("/admin/login", url.Values{"email": {adminEmail}, "password": {adminPassword}})
		assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, admin.TOTPPath, resp.Header.Get("Location"))
		c := b.cookie(admin.SessionCookie)
		require.NotNil(t, c)
		pendingToken = c.Value
		assert.Regexp(t, "^[0-9a-f]{64}$", pendingToken, "32 random bytes, hex")
	})

	t.Run("a pending cookie reaches the code page and nothing else", func(t *testing.T) {
		resp, _ := b.get("/admin/")
		assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, admin.TOTPPath, resp.Header.Get("Location"))
		resp, body := b.get("/admin/totp")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, body, "No authenticator yet", "bootstrap issued a password and nothing else")
		assert.Contains(t, body, "totp enroll --email "+adminEmail)
		assert.NotContains(t, body, `name="code"`, "nothing to type until the CLI has issued a secret")
		assert.NotContains(t, body, "/admin/users", "no navigation before the code")
	})

	t.Run("a code without a secret is refused", func(t *testing.T) {
		resp, body := b.post("/admin/totp", url.Values{"code": {"123456"}})
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Contains(t, body, "No authenticator has been set up")
	})

	// The operator runs `exchange admin totp enroll` with database
	// credentials. That revokes every session the administrator had.
	enrol, err := h.svc.EnrollTOTP(context.Background(), adminEmail)
	require.NoError(t, err)

	t.Run("enrolment revoked the pending session", func(t *testing.T) {
		resp, _ := b.get("/admin/totp")
		assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, admin.LoginPath, resp.Header.Get("Location"))
		assert.Nil(t, b.cookie(admin.SessionCookie), "the dead cookie was cleared")
	})

	t.Run("the first code confirms the authenticator", func(t *testing.T) {
		resp, _ := b.post("/admin/login", url.Values{"email": {adminEmail}, "password": {adminPassword}})
		require.Equal(t, http.StatusSeeOther, resp.StatusCode)
		pending := b.cookie(admin.SessionCookie)
		require.NotNil(t, pending)
		pendingToken = pending.Value

		resp, body := b.get("/admin/totp")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, body, "Finish setting up")
		assert.Contains(t, body, `name="code"`)

		resp, body = b.post("/admin/totp", url.Values{"code": {"000000"}})
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Contains(t, body, "That code is not right.")

		resp, _ = b.post("/admin/totp", url.Values{"code": {h.code(t, enrol.Secret)}})
		assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, admin.HomePath, resp.Header.Get("Location"))
		verified := b.cookie(admin.SessionCookie)
		require.NotNil(t, verified)
		assert.NotEqual(t, pendingToken, verified.Value, "the code swaps the session, it does not upgrade it")
	})

	t.Run("the dashboard", func(t *testing.T) {
		resp, body := b.get("/admin/")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, body, "Authenticator confirmed. You are signed in.", "the flash from the redirect")
		assert.Contains(t, body, "<h1>Dashboard</h1>")
		assert.Contains(t, body, ">balanced<")
		assert.Contains(t, body, "no report yet")
		assert.Contains(t, body, adminEmail)
		assert.Contains(t, body, "/admin/webhooks", "navigation, now")
		assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
		assert.NotEmpty(t, resp.Header.Get("Content-Security-Policy"))

		_, body = b.get("/admin/")
		assert.NotContains(t, body, "Authenticator confirmed.", "a flash shows once")
	})

	t.Run("the pending token is dead", func(t *testing.T) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/admin/", nil)
		require.NoError(t, err)
		req.AddCookie(&http.Cookie{Name: admin.SessionCookie, Value: pendingToken})
		resp, err := http.DefaultTransport.RoundTrip(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, admin.LoginPath, resp.Header.Get("Location"))
	})

	t.Run("the session is not an API credential", func(t *testing.T) {
		resp, _ := b.get("/admin/v1/system/status")
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("a cross-site post is refused with the session cookie present", func(t *testing.T) {
		resp, _ := b.post("/admin/logout", nil, "Sec-Fetch-Site", "cross-site")
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		resp, _ = b.get("/admin/")
		assert.Equal(t, http.StatusOK, resp.StatusCode, "still signed in")
		resp, _ = b.post("/admin/logout", nil, "Origin", "https://evil.example")
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("signing out ends the session on both sides", func(t *testing.T) {
		verified := b.cookie(admin.SessionCookie)
		require.NotNil(t, verified)
		resp, _ := b.post("/admin/logout", nil)
		assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, admin.LoginPath, resp.Header.Get("Location"))
		assert.Nil(t, b.cookie(admin.SessionCookie))

		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/admin/", nil)
		require.NoError(t, err)
		req.AddCookie(&http.Cookie{Name: admin.SessionCookie, Value: verified.Value})
		resp, err = http.DefaultTransport.RoundTrip(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusSeeOther, resp.StatusCode, "a copied token is no use after sign-out")
	})

	// Who each row is attributed to is the policy: a person is "admin" only
	// once proved, an attempt that failed is a "user" (whoever they were),
	// and what the CLI did is "system". A code offered before any secret was
	// issued is not a failed code: nothing was checked, so nothing counts
	// toward the lock.
	t.Run("the audit trail names each step", func(t *testing.T) {
		type row struct{ action, actor string }
		rows, err := h.adminPool.Query(context.Background(),
			`SELECT action, actor_type, actor_id FROM audit.audit_events WHERE action LIKE 'auth.admin.%' ORDER BY id`)
		require.NoError(t, err)
		defer rows.Close()
		var got []row
		for rows.Next() {
			var r row
			var actorID string
			require.NoError(t, rows.Scan(&r.action, &r.actor, &actorID))
			if r.actor != "system" {
				assert.Equal(t, h.admin.ID, actorID, "%s: the failures name the administrator too", r.action)
			}
			got = append(got, r)
		}
		assert.Equal(t, []row{
			{"auth.admin.bootstrap", "system"},
			{"auth.admin.login.failed", "user"},
			{"auth.admin.login", "admin"},
			{"auth.admin.totp.enroll", "system"},
			{"auth.admin.login", "admin"},
			{"auth.admin.totp.failed", "user"},
			{"auth.admin.totp.confirmed", "admin"},
			{"auth.admin.logout", "admin"},
		}, got)
	})
}

func TestAdminUILocksAfterFiveWrongCodes(t *testing.T) {
	h := setupAdminAuth(t)
	srv := adminUIServer(t, h, ratelimit.Limit{N: 100, Window: time.Minute})
	b := newBrowser(t, srv)
	_, err := h.svc.EnrollTOTP(context.Background(), adminEmail)
	require.NoError(t, err)
	resp, _ := b.post("/admin/login", url.Values{"email": {adminEmail}, "password": {adminPassword}})
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)

	for i := 0; i < 5; i++ {
		resp, body := b.post("/admin/totp", url.Values{"code": {"000000"}})
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		if i < 4 {
			assert.Contains(t, body, "That code is not right.")
		} else {
			assert.Contains(t, body, "Locked for fifteen minutes.")
		}
	}
	resp, body := b.post("/admin/totp", url.Values{"code": {"000000"}})
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Contains(t, body, "Locked for fifteen minutes.", "locked stays locked, whatever the code")
}

func TestAdminUIThrottlesThePasswordStep(t *testing.T) {
	h := setupAdminAuth(t)
	srv := adminUIServer(t, h, ratelimit.Limit{N: 3, Window: time.Minute})
	b := newBrowser(t, srv)
	form := url.Values{"email": {adminEmail}, "password": {"nope"}}
	for i := 0; i < 3; i++ {
		resp, _ := b.post("/admin/login", form)
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	}
	resp, body := b.post("/admin/login", url.Values{"email": {adminEmail}, "password": {adminPassword}})
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode, "the right password does not get through either")
	assert.Contains(t, body, "Too many attempts")
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))
	assert.Nil(t, b.cookie(admin.SessionCookie))
}
