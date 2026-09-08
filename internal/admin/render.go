package admin

import (
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/money"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

// The back office is server-rendered html/template with htmx for the few
// places a full page reload would be rude (paging a table, pressing a button
// on one row). Everything is embedded: one binary, no build step, no CDN.

//go:embed templates/*.html templates/partials/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// view is what every template gets.
type view struct {
	Title   string
	Path    string             // the request path, for the active nav item
	Session *auth.AdminSession // nil on the login page
	Flash   *flash
	Data    any
}

// flash is a one-shot message carried across a redirect in a cookie.
type flash struct {
	Kind string // ok | err
	Text string
}

const flashCookie = "admin_flash"

// templates is the parsed page set: layout + partials + one page each.
type templates struct {
	pages map[string]*template.Template
}

var funcs = template.FuncMap{
	"when": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04:05Z")
	},
	"whenp": func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format("2006-01-02 15:04:05Z")
	},
	"amount": func(a money.Amount) string { return a.String() },
	"join":   strings.Join,
	"active": func(path, prefix string) string {
		if prefix == HomePath && path == HomePath || prefix != HomePath && strings.HasPrefix(path, prefix) {
			return "active"
		}
		return ""
	},
}

// loadTemplates parses every page against the layout. A template that does
// not parse is a startup failure, not a 500 on first visit.
func loadTemplates() (*templates, error) {
	pageFiles, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	t := &templates{pages: map[string]*template.Template{}}
	for _, f := range pageFiles {
		name := strings.TrimSuffix(strings.TrimPrefix(f, "templates/"), ".html")
		if name == "layout" {
			continue
		}
		tpl, err := template.New("layout").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html", f)
		if err != nil {
			return nil, fmt.Errorf("admin: template %s: %w", name, err)
		}
		t.pages[name] = tpl
	}
	return t, nil
}

// render writes a page. The flash cookie is consumed here: read once, then
// cleared, so a message shows on the page after the redirect and no other.
func (t *templates) render(w http.ResponseWriter, r *http.Request, status int, name string, p view) {
	tpl, ok := t.pages[name]
	if !ok {
		telemetry.Logger(r.Context()).Error("admin: no such template", slog.String("name", name))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if p.Path == "" {
		p.Path = r.URL.Path
	}
	if p.Session == nil {
		if s, ok := SessionFrom(r.Context()); ok {
			p.Session = &s
		}
	}
	if p.Flash == nil {
		p.Flash = takeFlash(w, r)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tpl.ExecuteTemplate(w, "layout", p); err != nil {
		telemetry.Logger(r.Context()).Error("admin: render failed", slog.String("template", name), slog.String("err", err.Error()))
	}
}

// setFlash queues a message for the next page.
func setFlash(w http.ResponseWriter, kind, text string) {
	v := base64.RawURLEncoding.EncodeToString([]byte(kind + "|" + text))
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: a one-shot display message, nothing sensitive; Secure mirrors nothing it would protect
		Name: flashCookie, Value: v, Path: "/admin", MaxAge: 60, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

func takeFlash(w http.ResponseWriter, r *http.Request) *flash {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: clearing the flash cookie
		Name: flashCookie, Value: "", Path: "/admin", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil
	}
	kind, text, ok := strings.Cut(string(raw), "|")
	if !ok || (kind != "ok" && kind != "err") {
		return nil
	}
	return &flash{Kind: kind, Text: text}
}

// staticHandler serves the embedded assets with a day of caching: they only
// change with the binary.
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // the embed directive guarantees the directory
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		files.ServeHTTP(w, r)
	})
}
