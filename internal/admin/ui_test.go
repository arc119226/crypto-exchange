package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/ratelimit"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// fakeAuth is auth.Service as the login pages see it. One password, one
// code; the sessions it hands out are looked up by fakeSessions.
type fakeAuth struct {
	fakeSessions
	logins int
}

func (f *fakeAuth) AdminLogin(_ context.Context, email, password, _ string) (auth.AdminSession, error) {
	f.logins++
	if email == "ops@example.com" && password == "right" {
		return f.fakeSessions["pending"], nil
	}
	return auth.AdminSession{}, auth.ErrInvalidCredentials
}

func (f *fakeAuth) TOTPConfirm(_ context.Context, token, code, _ string) (auth.AdminSession, error) {
	return f.TOTPVerify(context.Background(), token, code, "")
}

func (f *fakeAuth) TOTPVerify(_ context.Context, token, code, _ string) (auth.AdminSession, error) {
	if token != "pending" {
		return auth.AdminSession{}, auth.ErrInvalidToken
	}
	switch code {
	case "123456":
		return f.fakeSessions["verified"], nil
	case "000000":
		return auth.AdminSession{}, auth.ErrTOTPLocked
	}
	return auth.AdminSession{}, auth.ErrInvalidTOTP
}

func (f *fakeAuth) AdminLogout(context.Context, string, string) error { return nil }

func newTestUI(t *testing.T) (*UI, *fakeAuth) {
	t.Helper()
	sessions := &fakeAuth{fakeSessions: fakeSessions{
		"verified": {Token: "verified", UserID: "u-1", Email: "ops@example.com", TOTPEnabled: true, TOTPVerified: true, ExpiresAt: time.Now().Add(time.Hour)},
		"pending":  {Token: "pending", UserID: "u-1", Email: "ops@example.com", TOTPPending: true, ExpiresAt: time.Now().Add(10 * time.Minute)},
	}}
	ui, err := NewUI(nil, sessions, UIConfig{LoginLimit: ratelimit.Limit{N: 2, Window: time.Minute}})
	require.NoError(t, err, "every template parses at startup")
	return ui, sessions
}

func pages(t *testing.T, ui *UI) http.Handler {
	t.Helper()
	r := chiRouter()
	Routes(r, nil, ui, "k")
	return r
}

func get(h http.Handler, path, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func post(h http.Handler, path, cookie string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Every page, rendered with a fixture and checked for the words an operator
// would look for (docs/plan-v1.0.md §12: "template 渲染測試"). The layout is
// checked once here for the two things that must never regress: no inline
// script (the CSP forbids it) and no navigation for a session that has not
// entered its code.
func TestTemplatesRender(t *testing.T) {
	tpl, err := loadTemplates()
	require.NoError(t, err)
	inline := regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]*\S[^<]*</script>|\son[a-z]+="`)
	verified := &auth.AdminSession{Email: "ops@example.com", TOTPEnabled: true, TOTPVerified: true}
	pending := &auth.AdminSession{Email: "ops@example.com", TOTPPending: true}

	cases := []struct {
		name    string
		page    string
		v       view
		want    []string
		wantNot []string
	}{
		{"login", "login", view{Title: "Sign in", Data: loginData{Email: "ops@example.com"}},
			[]string{`action="/admin/login"`, `value="ops@example.com"`, `type="password"`}, []string{"Sign out", "/admin/users"}},
		{"login with a flash", "login", view{Title: "Sign in", Flash: &flash{Kind: "err", Text: "Wrong email or password."}},
			[]string{`class="flash err"`, "Wrong email or password."}, nil},
		{"totp, enrolled", "totp", view{Title: "Verify", Session: verified, Data: totpData{Enrolled: true}},
			[]string{"Enter your code", `name="code"`, `action="/admin/totp"`}, []string{"totp enroll"}},
		{"totp, pending", "totp", view{Title: "Verify", Session: pending, Data: totpData{Pending: true}},
			[]string{"Finish setting up", `name="code"`, "exchange admin totp enroll"}, []string{"Sign out", "/admin/users"}},
		{"totp, nothing issued", "totp", view{Title: "Verify", Session: pending, Data: totpData{}},
			[]string{"No authenticator yet", "--email ops@example.com"}, []string{`name="code"`}},
		{"dashboard", "dashboard", view{Title: "Dashboard", Path: HomePath, Session: verified, Data: gen.SystemStatus{
			Database: gen.SystemStatusDatabaseOk, LedgerBalanced: false, OpenLedgerBreaks: 2, PendingWithdrawals: 3,
			ConfirmingDeposits: 4, DeadWebhookDeliveries24H: 5, Now: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC),
			LastReconciliation: &gen.ReconciliationSummary{ID: "r-1", Balanced: true, FinishedAt: time.Date(2026, 9, 8, 11, 30, 0, 0, time.UTC)},
		}},
			[]string{"OUT OF BALANCE", "2 open breaks", "2026-09-08 11:30:00Z", "Sign out", "ops@example.com",
				`class="active" href="/admin/"`, "/admin/webhooks", "Database: ok", "2026-09-08 12:00:00Z"},
			[]string{"no report yet"}},
		{"dashboard before any reconciliation", "dashboard", view{Title: "Dashboard", Session: verified, Data: gen.SystemStatus{
			Database: gen.SystemStatusDatabaseOk, LedgerBalanced: true, OpenLedgerBreaks: 1, Now: time.Now(),
		}}, []string{"balanced", "1 open break<", "no report yet"}, []string{"OUT OF BALANCE"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/admin/x", nil)
			tpl.render(rec, req, http.StatusOK, tc.page, tc.v)
			body := rec.Body.String()
			assert.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
			assert.Contains(t, body, "<title>"+tc.v.Title+" · exchange admin</title>")
			for _, w := range tc.want {
				assert.Contains(t, body, w)
			}
			for _, w := range tc.wantNot {
				assert.NotContains(t, body, w)
			}
			assert.NotRegexp(t, inline, body, "no inline script or handler: the CSP would block it")
		})
	}

	rec := httptest.NewRecorder()
	tpl.render(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, "no-such-page", view{})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestLoginPages(t *testing.T) {
	ui, sessions := newTestUI(t)
	h := pages(t, ui)

	t.Run("the front door", func(t *testing.T) {
		rec := get(h, "/", "")
		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, HomePath, rec.Header().Get("Location"))
		rec = get(h, HomePath, "")
		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, LoginPath, rec.Header().Get("Location"))
		rec = get(h, LoginPath, "")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), `name="password"`)
		assert.Equal(t, "default-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'", rec.Header().Get("Content-Security-Policy"))
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	})

	t.Run("static assets are served from here, so the CSP can be strict", func(t *testing.T) {
		rec := get(h, "/admin/static/htmx.min.js", "")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Header().Get("Content-Type"), "javascript")
		assert.Equal(t, "public, max-age=86400", rec.Header().Get("Cache-Control"))
		assert.Equal(t, http.StatusOK, get(h, "/admin/static/app.css", "").Code)
		assert.Equal(t, http.StatusNotFound, get(h, "/admin/static/../ui.go", "").Code)
	})

	t.Run("a wrong password stays on the form", func(t *testing.T) {
		rec := post(h, LoginPath, "", url.Values{"email": {"ops@example.com"}, "password": {"wrong"}})
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Body.String(), "Wrong email or password.")
		assert.Contains(t, rec.Body.String(), `value="ops@example.com"`, "the email is kept, the password is not")
		assert.Empty(t, rec.Result().Cookies())
	})

	t.Run("the right password sets a cookie and asks for the code", func(t *testing.T) {
		rec := post(h, LoginPath, "", url.Values{"email": {" ops@example.com "}, "password": {"right"}})
		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, TOTPPath, rec.Header().Get("Location"))
		cookies := rec.Result().Cookies()
		require.Len(t, cookies, 1)
		c := cookies[0]
		assert.Equal(t, SessionCookie, c.Name)
		assert.Equal(t, "pending", c.Value)
		assert.True(t, c.HttpOnly)
		assert.Equal(t, http.SameSiteLaxMode, c.SameSite)
		assert.Equal(t, "/admin", c.Path)
		assert.False(t, c.Secure, "the test config is a dev deployment")
	})

	t.Run("a pending session sees the code page and nothing else", func(t *testing.T) {
		rec := get(h, TOTPPath, "pending")
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), "Finish setting up")
		assert.NotContains(t, rec.Body.String(), "/admin/users", "no navigation before the code")

		rec = get(h, HomePath, "pending")
		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, TOTPPath, rec.Header().Get("Location"))

		rec = get(h, LoginPath, "pending")
		assert.Equal(t, http.StatusSeeOther, rec.Code, "already half in: go finish")
		assert.Equal(t, TOTPPath, rec.Header().Get("Location"))
	})

	t.Run("a wrong code stays on the code page", func(t *testing.T) {
		rec := post(h, TOTPPath, "pending", url.Values{"code": {"999999"}})
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Body.String(), "That code is not right.")
		rec = post(h, TOTPPath, "pending", url.Values{"code": {"000000"}})
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.Contains(t, rec.Body.String(), "Locked for fifteen minutes.")
	})

	t.Run("the right code swaps the cookie and goes home", func(t *testing.T) {
		rec := post(h, TOTPPath, "pending", url.Values{"code": {"123 456"}})
		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, HomePath, rec.Header().Get("Location"))
		var session, flashed bool
		for _, c := range rec.Result().Cookies() {
			switch c.Name {
			case SessionCookie:
				session = true
				assert.Equal(t, "verified", c.Value)
			case flashCookie:
				flashed = true
			}
		}
		assert.True(t, session)
		assert.True(t, flashed, "confirming an authenticator is told on the next page")

		rec = get(h, LoginPath, "verified")
		assert.Equal(t, http.StatusSeeOther, rec.Code, "already in: go home")
		assert.Equal(t, HomePath, rec.Header().Get("Location"))
		rec = get(h, TOTPPath, "verified")
		assert.Equal(t, HomePath, rec.Header().Get("Location"))
	})

	t.Run("an htmx request is told where to go instead of being redirected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, HomePath, nil)
		req.Header.Set("HX-Request", "true")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, LoginPath, rec.Header().Get("HX-Redirect"))
		assert.Empty(t, rec.Body.String())
	})

	t.Run("logging out clears the cookie", func(t *testing.T) {
		rec := post(h, LogoutPath, "verified", nil)
		assert.Equal(t, http.StatusSeeOther, rec.Code)
		assert.Equal(t, LoginPath, rec.Header().Get("Location"))
		require.Len(t, rec.Result().Cookies(), 1)
		assert.Equal(t, -1, rec.Result().Cookies()[0].MaxAge)
		rec = post(h, LogoutPath, "", nil)
		assert.Equal(t, http.StatusSeeOther, rec.Code, "no session to end is still a way out")
	})

	t.Run("a cross-site form post is refused before it reaches the password check", func(t *testing.T) {
		before := sessions.logins
		req := httptest.NewRequest(http.MethodPost, LoginPath, strings.NewReader("email=ops%40example.com&password=right"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Equal(t, before, sessions.logins)
		assert.Empty(t, rec.Result().Cookies())
	})

	t.Run("the password step is throttled per address", func(t *testing.T) {
		ui2, _ := newTestUI(t)
		h2 := pages(t, ui2)
		form := url.Values{"email": {"ops@example.com"}, "password": {"wrong"}}
		assert.Equal(t, http.StatusUnauthorized, post(h2, LoginPath, "", form).Code)
		assert.Equal(t, http.StatusUnauthorized, post(h2, LoginPath, "", form).Code)
		rec := post(h2, LoginPath, "", form)
		assert.Equal(t, http.StatusTooManyRequests, rec.Code)
		assert.NotEmpty(t, rec.Header().Get("Retry-After"))
		assert.Contains(t, rec.Body.String(), "Too many attempts")
		assert.Equal(t, http.StatusOK, get(h2, LoginPath, "").Code, "reading the form is not an attempt")
	})
}

func TestFlashSurvivesExactlyOneRedirect(t *testing.T) {
	rec := httptest.NewRecorder()
	setFlash(rec, "ok", "Saved.")
	require.Len(t, rec.Result().Cookies(), 1)
	c := rec.Result().Cookies()[0]
	assert.Equal(t, flashCookie, c.Name)
	assert.True(t, c.HttpOnly)

	req := httptest.NewRequest(http.MethodGet, HomePath, nil)
	req.AddCookie(c)
	rec = httptest.NewRecorder()
	f := takeFlash(rec, req)
	require.NotNil(t, f)
	assert.Equal(t, flash{Kind: "ok", Text: "Saved."}, *f)
	require.Len(t, rec.Result().Cookies(), 1)
	assert.Equal(t, -1, rec.Result().Cookies()[0].MaxAge, "read once, then cleared")

	req = httptest.NewRequest(http.MethodGet, HomePath, nil)
	req.AddCookie(&http.Cookie{Name: flashCookie, Value: "not base64!"})
	assert.Nil(t, takeFlash(httptest.NewRecorder(), req))
	req = httptest.NewRequest(http.MethodGet, HomePath, nil)
	req.AddCookie(&http.Cookie{Name: flashCookie, Value: "d2FybnxoaQ"}) // warn|hi
	assert.Nil(t, takeFlash(httptest.NewRecorder(), req), "only the two kinds the stylesheet knows")
}

func TestActiveNav(t *testing.T) {
	active := funcs["active"].(func(string, string) string)
	assert.Equal(t, "active", active("/admin/", "/admin/"))
	assert.Equal(t, "", active("/admin/users", "/admin/"), "the home item is exact, not a prefix of everything")
	assert.Equal(t, "active", active("/admin/users/u-1", "/admin/users"))
	assert.Equal(t, "", active("/admin/webhooks", "/admin/users"))
}

// auth.Service is what admin_role.go hands NewUI; the fakes above stand in for it.
var _ Sessions = (*auth.Service)(nil)

func TestUserTemplatesRender(t *testing.T) {
	tpl, err := loadTemplates()
	require.NoError(t, err)
	verified := &auth.AdminSession{Email: "ops@example.com", TOTPEnabled: true, TOTPVerified: true}
	alice := auth.User{ID: "u-alice", Email: "alice@example.com", Role: "user", KYCLevel: 1, Status: "active", Version: 2, CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	frozen := alice
	frozen.Status = "frozen"

	render := func(name string, data any) string {
		rec := httptest.NewRecorder()
		tpl.render(rec, httptest.NewRequest(http.MethodGet, "/admin/users", nil), http.StatusOK, name, view{Title: "Users", Session: verified, Data: data})
		return rec.Body.String()
	}

	body := render("users", usersData{Filter: auth.UserFilter{Email: "ali", Status: "active"}, Users: []auth.User{alice}, Pager: newPager("/admin/users", url.Values{"email": {"ali"}}, 1, 1, 1)})
	assert.Contains(t, body, `href="/admin/users/u-alice"`)
	assert.Contains(t, body, `value="ali"`)
	assert.Contains(t, body, `<option value="active" selected>`)
	assert.Contains(t, body, `class="badge ok">active<`)
	assert.Contains(t, body, `hx-get="/admin/users?email=ali&amp;limit=1&amp;offset=0"`, "newer")
	assert.Contains(t, body, `hx-get="/admin/users?email=ali&amp;limit=1&amp;offset=2"`, "older")
	assert.Contains(t, body, `class="active" href="/admin/users"`, "the nav item")

	body = render("users", usersData{})
	assert.Contains(t, body, "No users match.")
	assert.NotContains(t, body, `class="pager"`)

	acct := &ledger.Account{ID: "acct-alice", Status: ledger.StatusActive}
	body = render("user", userData{User: alice, Account: acct, Levels: []int{0, 1, 2}})
	assert.Contains(t, body, `<option value="1" selected>`)
	assert.Contains(t, body, `<input type="hidden" name="status" value="frozen">`)
	assert.Contains(t, body, ">Freeze<")
	assert.Contains(t, body, `href="/admin/ledger?account_id=acct-alice"`)
	assert.Contains(t, body, "version 2")

	body = render("user", userData{User: frozen, Levels: []int{0, 1, 2}})
	assert.Contains(t, body, `<input type="hidden" name="status" value="active">`)
	assert.Contains(t, body, ">Release<")
	assert.Contains(t, body, `<dd><span class="muted">none</span></dd>`)
}

func TestPager(t *testing.T) {
	limit, offset := pageQuery(url.Values{"limit": {"500"}, "offset": {"-3"}})
	assert.Equal(t, int32(50), limit, "over the cap: the default")
	assert.Equal(t, int32(0), offset)
	limit, offset = pageQuery(url.Values{"limit": {"20"}, "offset": {"40"}})
	assert.Equal(t, int32(20), limit)
	assert.Equal(t, int32(40), offset)

	p := newPager("/admin/x", nil, 20, 0, 19)
	assert.Equal(t, pager{}, p, "first page, not full: nowhere to go")
	p = newPager("/admin/x", url.Values{"status": {"frozen"}}, 20, 30, 20)
	assert.Equal(t, "/admin/x?limit=20&offset=10&status=frozen", p.Prev)
	assert.Equal(t, "/admin/x?limit=20&offset=50&status=frozen", p.Next)
}

func TestRegistryTemplatesRender(t *testing.T) {
	tpl, err := loadTemplates()
	require.NoError(t, err)
	verified := &auth.AdminSession{Email: "ops@example.com", TOTPEnabled: true, TOTPVerified: true}
	render := func(name string, data any) string {
		rec := httptest.NewRecorder()
		tpl.render(rec, httptest.NewRequest(http.MethodGet, "/admin/"+name, nil), http.StatusOK, name, view{Title: name, Session: verified, Data: data})
		body := rec.Body.String()
		require.Contains(t, body, "</html>", "%s rendered to the end", name)
		return body
	}
	addr := "0x1234567890abcdef1234567890abcdef12345678"
	eth := registry.Asset{Symbol: "ETH", Name: "Ether", ChainID: 31337, IsNative: true, Scale: 18, DisplayScale: 6, RequiredConfirmations: 12,
		MinWithdrawal: money.MustParse("0.01"), SweepThreshold: money.MustParse("0.05"), DepositEnabled: true, WithdrawEnabled: true, Status: "active", Version: 1}
	usdc := registry.Asset{Symbol: "USDC", Name: "USD Coin", ChainID: 31337, ContractAddress: &addr, Scale: 6, DisplayScale: 2, RequiredConfirmations: 12,
		MinWithdrawal: money.MustParse("5"), DepositEnabled: true, WithdrawEnabled: false, Status: "disabled", Version: 3}
	fee := registry.FeeSchedule{Name: "default", MakerBps: 10, TakerBps: 20, Version: 1}
	slip := int32(500)
	market := registry.Market{Symbol: "ETH-USDC", BaseSymbol: "ETH", QuoteSymbol: "USDC", PriceTick: money.MustParse("0.01"), QtyStep: money.MustParse("0.0001"),
		MinNotional: money.MustParse("5"), MaxSlippageBps: &slip, FeeScheduleName: "default", MakerBps: 10, TakerBps: 20, SelfTradePolicy: "cancel_newest", Status: "halted", Version: 2}

	body := render("assets", assetsData{Assets: []registry.Asset{eth, usdc}, Statuses: assetStatuses})
	assert.Contains(t, body, `action="/admin/assets/USDC"`)
	assert.Contains(t, body, `value="`+addr+`"`)
	assert.Contains(t, body, `<option value="disabled" selected>`)
	assert.Contains(t, body, `name="is_native" checked`)
	assert.Contains(t, body, `class="badge bad">off<`, "USDC withdrawals are off")
	assert.Contains(t, body, `action="/admin/assets"`, "the new-asset form")
	assert.Contains(t, body, `name="symbol"`)

	body = render("markets", marketsData{Markets: []registry.Market{market}, Assets: []registry.Asset{eth, usdc}, FeeSchedules: []registry.FeeSchedule{fee}, Statuses: marketStatuses, Policies: stpPolicies})
	assert.Contains(t, body, `action="/admin/markets/ETH-USDC/status"`)
	assert.Contains(t, body, `action="/admin/markets/ETH-USDC"`)
	assert.Contains(t, body, `<option value="halted" selected>`)
	assert.Contains(t, body, `name="max_slippage_bps" value="500"`)
	assert.Contains(t, body, `<option value="default" selected>default (10/20 bps)</option>`)
	assert.Contains(t, body, `action="/admin/engine/reload"`)
	assert.Contains(t, body, `<option value="USDC">USDC</option>`, "assets to pick a pair from")

	body = render("fee-schedules", feeSchedulesData{FeeSchedules: []registry.FeeSchedule{fee}})
	assert.Contains(t, body, `action="/admin/fee-schedules/default"`)
	assert.Contains(t, body, `name="taker_bps" value="20"`)
	assert.Contains(t, body, `action="/admin/fee-schedules"`)

	body = render("withdrawal-limits", withdrawalLimitsData{Limits: []registry.WithdrawalLimit{
		{Asset: "ETH", KYCLevel: 0, AutoApproveLimit: money.MustParse("0.1"), DailyLimit: money.MustParse("1"), Version: 1},
		{Asset: "ETH", KYCLevel: 2, AutoApproveLimit: money.MustParse("10"), DailyLimit: money.MustParse("100"), RequireManualReview: true, Version: 4},
	}, Assets: []registry.Asset{eth, usdc}, Levels: kycLevels})
	assert.Contains(t, body, `action="/admin/withdrawal-limits/ETH"`)
	assert.Contains(t, body, `name="kyc_level" value="2"`)
	assert.Contains(t, body, `name="require_manual_review" checked`)
	assert.Contains(t, body, `class="badge warn">always<`)
	assert.Contains(t, body, `<option value="2">2</option>`)

	// empty tables say so instead of rendering nothing
	assert.Contains(t, render("assets", assetsData{Statuses: assetStatuses}), "No assets.")
	assert.Contains(t, render("markets", marketsData{Statuses: marketStatuses, Policies: stpPolicies}), "No markets.")
	assert.Contains(t, render("fee-schedules", feeSchedulesData{}), "No fee schedules.")
	assert.Contains(t, render("withdrawal-limits", withdrawalLimitsData{Levels: kycLevels}), "every withdrawal waits for a person")
}

func TestFormReadsFieldsAndKeepsTheFirstComplaint(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("name=Ether&chain_id=31337&scale=18&min_deposit=0.5&is_native=on&max_qty=&reason=because"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	f := newForm(req)
	assert.Equal(t, "Ether", f.str("name"))
	assert.Equal(t, int64(31337), f.i64("chain_id"))
	assert.Equal(t, int32(18), f.i32("scale"))
	assert.Equal(t, "0.5", f.amount("min_deposit").String())
	assert.True(t, f.boolean("is_native"))
	assert.False(t, f.boolean("deposit_enabled"), "an unchecked box is absent")
	assert.Nil(t, f.optAmount("max_qty"))
	assert.Nil(t, f.optI32("max_slippage_bps"))
	assert.Equal(t, "because", f.reason())
	assert.Empty(t, f.problem)

	f.i32("name")
	f.amount("scale")
	assert.Equal(t, "name must be a whole number.", f.problem, "the first complaint wins")

	f = newForm(httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("")))
	f.r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	f.reason()
	assert.Contains(t, f.problem, "A reason is required")
}
