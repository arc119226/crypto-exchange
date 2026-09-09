package admin

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The back office speaks two languages (ADR-0010). Every visible string in
// the templates goes through T; every flash a handler writes is worded in
// the request's language before it is stored. English is the source
// catalogue and is what the interface said before it had a second language,
// so the English rendering is byte for byte what it was, and the tests that
// look for English words still find them.
//
// The language of a request is the admin_lang cookie when the switcher was
// used, else the browser's Accept-Language, else English. A test request
// without either renders English.

type lang string

const (
	langEN   lang = "en"
	langZhTW lang = "zh-TW"
)

var langs = []lang{langEN, langZhTW}

// langCookie stores the operator's choice; LangPath takes it.
const (
	langCookie = "admin_lang"
	LangPath   = "/admin/lang"
)

var messages = map[lang]map[string]string{langEN: messagesEN, langZhTW: messagesZhTW}

var enums = map[lang]map[string]string{langZhTW: enumsZhTW}

func parseLang(s string) (lang, bool) {
	for _, l := range langs {
		if string(l) == s {
			return l, true
		}
	}
	return "", false
}

// acceptLanguage picks a language from an Accept-Language header: the
// first entry the browser lists with a non-zero weight decides, Chinese of
// any script or region being Traditional Chinese and everything else
// English. Browsers list their entries in order of preference, so there is
// nothing to sort with only two languages on offer.
func acceptLanguage(header string) lang {
	for _, part := range strings.Split(header, ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		tag = strings.TrimSpace(tag)
		if tag == "" || tag == "*" {
			continue
		}
		if refused(params) {
			continue
		}
		primary, _, _ := strings.Cut(strings.ToLower(tag), "-")
		switch primary {
		case "zh":
			return langZhTW
		default:
			return langEN
		}
	}
	return langEN
}

// refused reports whether the entry's parameters carry q=0, which a browser
// uses to say "not this one".
func refused(params string) bool {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
			q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			return err == nil && q == 0
		}
	}
	return false
}

// langFor decides a request's language: cookie, then header, then English.
func langFor(r *http.Request) lang {
	if c, err := r.Cookie(langCookie); err == nil {
		if l, ok := parseLang(c.Value); ok {
			return l
		}
	}
	return acceptLanguage(r.Header.Get("Accept-Language"))
}

// withLang puts the request's language in the context for the handlers.
func withLang(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), langKey, langFor(r))))
	})
}

// langFrom reads the language withLang stored; a context without one (a
// test's bare request) is English.
func langFrom(ctx context.Context) lang {
	if l, ok := ctx.Value(langKey).(lang); ok {
		return l
	}
	return langEN
}

// T is the message for key in this language, with fmt verbs filled from
// args when there are any. A key the language lacks falls back to English,
// and a key English lacks renders as itself, so a typo shows on the page
// instead of hiding; TestMessagesComplete makes sure neither happens.
func (l lang) T(key string, args ...any) string {
	msg, ok := messages[l][key]
	if !ok {
		if msg, ok = messagesEN[key]; !ok {
			msg = key
		}
	}
	if len(args) == 0 {
		return msg
	}
	return fmt.Sprintf(msg, args...)
}

// Tn is T for a count: key.one when n is 1, key.other otherwise, with n as
// the message's %d.
func (l lang) Tn(key string, n any) string {
	count := toInt(n)
	suffix := ".other"
	if count == 1 {
		suffix = ".one"
	}
	return l.T(key+suffix, count)
}

func toInt(n any) int {
	switch v := n.(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case int16:
		return int(v)
	}
	return 0
}

// Status is the word for a status code or other enumerated value: English
// shows the code itself, as the interface always has; Traditional Chinese
// looks it up and falls back to the code, since every status set is open on
// the server side. The badge keeps the code in its title attribute so an
// operator can match a runbook whatever the language.
func (l lang) Status(raw any) string {
	s := fmt.Sprint(raw)
	if word, ok := enums[l][s]; ok {
		return word
	}
	return s
}

// funcs are the language-bound template functions; each language's page set
// is parsed with its own (see loadTemplates), which is what lets a partial
// that rebinds the dot still translate.
func (l lang) funcs() template.FuncMap {
	return template.FuncMap{"T": l.T, "Tn": l.Tn, "status": l.Status}
}

// setLang stores the operator's choice and goes back to the page the
// switcher was on. It sits outside the session middleware so the login
// page can be switched too.
func (u *UI) setLang(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	l, ok := parseLang(r.PostFormValue("lang"))
	if !ok {
		http.Error(w, "unknown language", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: a display preference, nothing sensitive; Secure follows Cookies like the session cookie
		Name: langCookie, Value: string(l), Path: "/admin", MaxAge: 365 * 24 * 3600,
		HttpOnly: true, Secure: u.cfg.Cookies.Secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, safeBack(r.PostFormValue("next")), http.StatusSeeOther) //nolint:gosec // G710: safeBack admits only a path under /admin
}

// safeBack admits a same-origin path under /admin as the place to return
// to after the switch; anything else (another host, a scheme, a
// protocol-relative URL, a path outside the pages) goes home.
func safeBack(next string) string {
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") || !strings.HasPrefix(u.Path, "/admin") {
		return HomePath
	}
	u.Fragment = ""
	return u.String()
}
