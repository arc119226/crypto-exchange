package admin

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/auth"
)

// The two catalogues have the same keys, the same fmt verbs per key, every
// key a template or a handler asks for, and no key nobody asks for. A
// missing translation would otherwise show as English on the Chinese page,
// a mismatched verb as "%!d(string=...)", a dead key as a wrong page later.
func TestMessagesComplete(t *testing.T) {
	keys := func(m map[string]string) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	assert.Equal(t, keys(messagesEN), keys(messagesZhTW), "every English key has a Traditional Chinese one and vice versa")

	verb := regexp.MustCompile(`%(\[\d+\])?[sdv]`)
	verbs := func(s string) []string {
		var out []string
		for _, v := range verb.FindAllString(s, -1) {
			out = append(out, v[len(v)-1:]) // drop an explicit index: %[2]s is an %s
		}
		sort.Strings(out)
		return out
	}
	for k, en := range messagesEN {
		assert.Equal(t, verbs(en), verbs(messagesZhTW[k]), "fmt verbs of %s", k)
		assert.NotEmpty(t, en, "%s", k)
		assert.NotEmpty(t, messagesZhTW[k], "%s", k)
	}

	// every key a template asks for exists; every key exists for a reason
	used := map[string]bool{}
	call := regexp.MustCompile(`[{(]\s*(T|Tn)\s+"([^"]+)"`)
	require.NoError(t, fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(templateFS, path)
		if err != nil {
			return err
		}
		for _, m := range call.FindAllStringSubmatch(string(b), -1) {
			wanted := []string{m[2]}
			if m[1] == "Tn" {
				wanted = []string{m[2] + ".one", m[2] + ".other"}
			}
			for _, k := range wanted {
				used[k] = true
				_, ok := messagesEN[k]
				assert.True(t, ok, "%s asks for %q, which the catalogue lacks", path, k)
			}
		}
		return nil
	}))
	var goSrc strings.Builder
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") || strings.HasPrefix(e.Name(), "i18n_") {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(e.Name()))
		require.NoError(t, err)
		goSrc.Write(b)
	}
	for k := range messagesEN {
		if used[k] || strings.Contains(goSrc.String(), `"`+k+`"`) {
			continue
		}
		assert.Fail(t, "dead key", "%s is in the catalogue but no template or handler asks for it", k)
	}
}

func TestAcceptLanguage(t *testing.T) {
	cases := map[string]lang{
		"zh-TW,zh;q=0.9,en;q=0.8":    langZhTW,
		"zh-Hant-HK":                 langZhTW,
		"ZH":                         langZhTW,
		"en-US,en;q=0.9,zh-TW;q=0.8": langEN,
		"zh;q=0,en":                  langEN,
		"zh; q=0.0, en":              langEN,
		"*":                          langEN,
		"":                           langEN,
		"fr-FR,fr;q=0.9":             langEN,
		"garbage;;;,,,":              langEN,
		"*,zh-TW;q=0.5":              langZhTW,
		"en;q=0.5,zh-TW;q=0.9":       langEN, // the browser's order is trusted, not sorted
		"zh-TW ; q=0.7 , en ; q=0.3": langZhTW,
	}
	for header, want := range cases {
		assert.Equal(t, want, acceptLanguage(header), "%q", header)
	}
}

func TestLangCookieWins(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, HomePath, nil)
	req.Header.Set("Accept-Language", "zh-TW")
	assert.Equal(t, langZhTW, langFor(req))
	req.AddCookie(&http.Cookie{Name: langCookie, Value: "en"})
	assert.Equal(t, langEN, langFor(req), "the switcher's choice beats the browser's")
	req = httptest.NewRequest(http.MethodGet, HomePath, nil)
	req.AddCookie(&http.Cookie{Name: langCookie, Value: "fr"})
	assert.Equal(t, langEN, langFor(req), "an unknown cookie value is ignored")
	assert.Equal(t, langEN, langFrom(req.Context()), "a request that skipped the middleware is English")
}

func TestSetLang(t *testing.T) {
	ui, _ := newTestUI(t)
	h := pages(t, ui)

	rec := post(h, LangPath, "", url.Values{"lang": {"zh-TW"}, "next": {"/admin/users?status=frozen"}})
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "/admin/users?status=frozen", rec.Header().Get("Location"))
	require.Len(t, rec.Result().Cookies(), 1)
	c := rec.Result().Cookies()[0]
	assert.Equal(t, langCookie, c.Name)
	assert.Equal(t, "zh-TW", c.Value)
	assert.Equal(t, "/admin", c.Path)
	assert.True(t, c.HttpOnly)
	assert.Equal(t, http.SameSiteLaxMode, c.SameSite)
	assert.Equal(t, 365*24*3600, c.MaxAge)
	assert.False(t, c.Secure, "the test config is a dev deployment, as for the session cookie")

	for _, next := range []string{"https://evil.example/", "//evil.example/x", "/other", "", "/admin\\evil.example", "javascript:alert(1)"} {
		rec = post(h, LangPath, "", url.Values{"lang": {"en"}, "next": {next}})
		assert.Equal(t, http.StatusSeeOther, rec.Code, "%q", next)
		assert.Equal(t, HomePath, rec.Header().Get("Location"), "%q goes home", next)
	}
	rec = post(h, LangPath, "", url.Values{"lang": {"en"}, "next": {"/admin/ledger?account_id=a#frag"}})
	assert.Equal(t, "/admin/ledger?account_id=a", rec.Header().Get("Location"), "the fragment is dropped")

	rec = post(h, LangPath, "", url.Values{"lang": {"fr"}, "next": {HomePath}})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, rec.Result().Cookies())

	// the switch works before a session exists: the login page is bilingual
	req := httptest.NewRequest(http.MethodGet, LoginPath, nil)
	req.AddCookie(&http.Cookie{Name: langCookie, Value: "zh-TW"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `<html lang="zh-TW">`)
	assert.Contains(t, rec.Body.String(), "<h1>登入</h1>")
	assert.Contains(t, rec.Body.String(), `<title>登入 · exchange admin</title>`)
	assert.Contains(t, rec.Body.String(), `<input type="hidden" name="next" value="/admin/login">`, "the switcher returns here")

	// a wrong password is told in the chosen language
	req = httptest.NewRequest(http.MethodPost, LoginPath, strings.NewReader("email=ops%40example.com&password=wrong"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: langCookie, Value: "zh-TW"})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "電子郵件或密碼錯誤。")
}

// The Traditional Chinese page set renders: the words an operator looks
// for are Chinese, the codes a runbook names are still there (badge title,
// option value), amounts and times are untouched, and the CSP rule holds.
func TestTemplatesRenderInZhTW(t *testing.T) {
	tpl, err := loadTemplates()
	require.NoError(t, err)
	inline := regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]*\S[^<]*</script>|\son[a-z]+="`)
	verified := &auth.AdminSession{Email: "ops@example.com", TOTPEnabled: true, TOTPVerified: true}
	render := func(name string, v view) string {
		req := httptest.NewRequest(http.MethodGet, "/admin/"+name, nil)
		req.AddCookie(&http.Cookie{Name: langCookie, Value: "zh-TW"})
		rec := httptest.NewRecorder()
		withLang(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tpl.render(w, r, http.StatusOK, name, v)
		})).ServeHTTP(rec, req)
		body := rec.Body.String()
		require.Contains(t, body, "</html>", "%s rendered to the end", name)
		assert.Contains(t, body, `<html lang="zh-TW">`)
		assert.NotRegexp(t, inline, body, "no inline script or handler: the CSP would block it")
		return body
	}

	body := render("login", view{Title: "登入"})
	assert.Contains(t, body, ">中文</button>")
	assert.Contains(t, body, `value="zh-TW" class="link active"`)
	assert.Contains(t, body, "下一步會要求輸入驗證器 App 的驗證碼。")
	assert.NotContains(t, body, "Sign in")

	body = render("dashboard", view{Title: "儀表板", Path: HomePath, Session: verified, Data: gen.SystemStatus{
		Database: gen.SystemStatusDatabaseOk, LedgerBalanced: false, OpenLedgerBreaks: 2, Now: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC),
	}})
	assert.Contains(t, body, ">儀表板</a>", "the nav")
	assert.Contains(t, body, ">登出</button>")
	assert.Contains(t, body, ">不平衡<")
	assert.Contains(t, body, "2 筆未結差異")
	assert.Contains(t, body, "資料庫: ok.")
	assert.Contains(t, body, "2026-09-08 12:00:00Z", "times are not localised")
	body = render("dashboard", view{Title: "儀表板", Session: verified, Data: gen.SystemStatus{LedgerBalanced: true, OpenLedgerBreaks: 1, Now: time.Now()}})
	assert.Contains(t, body, "1 筆未結差異")
	assert.Contains(t, body, "尚無報告")

	alice := auth.User{ID: "u-alice", Email: "alice@example.com", Role: "user", KYCLevel: 1, Status: "frozen", CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	body = render("users", view{Title: "用戶", Session: verified, Data: usersData{Filter: auth.UserFilter{Status: "frozen"}, Users: []auth.User{alice}}})
	assert.Contains(t, body, `title="frozen" class="badge bad">凍結<`, "the badge keeps the code in its title")
	assert.Contains(t, body, `<option value="frozen" selected>凍結</option>`, "the option keeps the code in its value")
	assert.Contains(t, body, ">一般用戶")
	assert.Contains(t, body, "<th>電子郵件</th>")
	body = render("users", view{Title: "用戶", Session: verified, Data: usersData{}})
	assert.Contains(t, body, "沒有符合的用戶。")

	body = render("totp", view{Title: "驗證", Session: &auth.AdminSession{Email: "ops@example.com", TOTPPending: true}, Data: totpData{}})
	assert.Contains(t, body, "<pre>exchange admin totp enroll --email ops@example.com</pre>", "the command an operator copies is untouched")
	assert.Contains(t, body, "尚未設定驗證器")
}

// English is the page set that was there before the second language, word
// for word: the switch adds only the lang attribute, the switcher and the
// badge titles. (The rest of ui_test.go asserts the English words page by
// page; this pins the parts the switch touched.)
func TestEnglishIsTheDefault(t *testing.T) {
	ui, _ := newTestUI(t)
	h := pages(t, ui)
	rec := get(h, LoginPath, "")
	body := rec.Body.String()
	assert.Contains(t, body, `<html lang="en">`)
	assert.Contains(t, body, "<h1>Sign in</h1>")
	assert.Contains(t, body, `value="en" class="link active"`)
	assert.Contains(t, body, `action="/admin/lang"`)

	req := httptest.NewRequest(http.MethodGet, LoginPath, nil)
	req.Header.Set("Accept-Language", "zh-TW,zh;q=0.9")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Contains(t, rec.Body.String(), "<h1>登入</h1>", "a Chinese browser gets Chinese without touching the switcher")
}
