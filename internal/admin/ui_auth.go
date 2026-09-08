package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/arc119226/crypto-exchange/internal/auth"
)

// The login is two pages. The first takes a password and hands out a pending
// session; the second takes a code and swaps it for a real one. Everything
// the session does on the auth side is in internal/auth/adminsession.go;
// this is only the browser's view of it.

type loginData struct {
	Email string
}

func (u *UI) loginForm(w http.ResponseWriter, r *http.Request) {
	// Already in: go home. Half in: go finish.
	if c, err := r.Cookie(SessionCookie); err == nil {
		if s, err := u.sessions.AdminSession(r.Context(), c.Value); err == nil {
			if s.TOTPVerified {
				http.Redirect(w, r, HomePath, http.StatusSeeOther)
			} else {
				http.Redirect(w, r, TOTPPath, http.StatusSeeOther)
			}
			return
		}
	}
	u.tpl.render(w, r, http.StatusOK, "login", view{Title: "Sign in"})
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.tpl.render(w, r, http.StatusBadRequest, "login", view{Title: "Sign in", Flash: &flash{Kind: "err", Text: "Bad form."}})
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))
	sess, err := u.sessions.AdminLogin(r.Context(), email, r.PostFormValue("password"), clientIPFrom(r.Context()))
	if err != nil {
		outcome := "password_failed"
		msg := "Wrong email or password."
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			outcome, msg = "error", "Something went wrong; try again."
		}
		u.metrics.logins.WithLabelValues(outcome).Inc()
		u.tpl.render(w, r, http.StatusUnauthorized, "login", view{Title: "Sign in", Flash: &flash{Kind: "err", Text: msg}, Data: loginData{Email: email}})
		return
	}
	u.metrics.logins.WithLabelValues("password_ok").Inc()
	u.cfg.Cookies.Set(w, sess.Token, sess.ExpiresAt)
	http.Redirect(w, r, TOTPPath, http.StatusSeeOther)
}

// totpData tells the code page which of three situations it is in.
type totpData struct {
	Enrolled bool // enter the code from your app
	Pending  bool // enter the first code to finish enrolment
	// neither: nothing has been issued; run `exchange admin totp enroll`
}

func (u *UI) totpForm(w http.ResponseWriter, r *http.Request) {
	s, _ := SessionFrom(r.Context())
	if s.TOTPVerified {
		http.Redirect(w, r, HomePath, http.StatusSeeOther)
		return
	}
	u.tpl.render(w, r, http.StatusOK, "totp", view{Title: "Verify", Data: totpData{Enrolled: s.TOTPEnabled, Pending: s.TOTPPending}})
}

func (u *UI) totp(w http.ResponseWriter, r *http.Request) {
	s, _ := SessionFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		u.tpl.render(w, r, http.StatusBadRequest, "totp", view{Title: "Verify", Flash: &flash{Kind: "err", Text: "Bad form."}})
		return
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		http.Redirect(w, r, LoginPath, http.StatusSeeOther)
		return
	}
	code := strings.ReplaceAll(strings.TrimSpace(r.PostFormValue("code")), " ", "")
	ip := clientIPFrom(r.Context())
	var next auth.AdminSession
	if s.TOTPEnabled {
		next, err = u.sessions.TOTPVerify(r.Context(), c.Value, code, ip)
	} else {
		next, err = u.sessions.TOTPConfirm(r.Context(), c.Value, code, ip)
	}
	if err != nil {
		outcome, msg, status := "totp_failed", "That code is not right.", http.StatusUnauthorized
		switch {
		case errors.Is(err, auth.ErrTOTPLocked):
			outcome, msg = "locked", "Too many wrong codes. Locked for fifteen minutes."
		case errors.Is(err, auth.ErrTOTPNotEnrolled):
			outcome, msg = "not_enrolled", "No authenticator has been set up for this account. Run `exchange admin totp enroll` first."
		case errors.Is(err, auth.ErrInvalidToken):
			http.Redirect(w, r, LoginPath, http.StatusSeeOther)
			return
		case !errors.Is(err, auth.ErrInvalidTOTP) && !errors.Is(err, auth.ErrTOTPAlreadyEnabled):
			outcome, msg, status = "error", "Something went wrong; try again.", http.StatusInternalServerError
		}
		u.metrics.logins.WithLabelValues(outcome).Inc()
		u.tpl.render(w, r, status, "totp", view{Title: "Verify", Flash: &flash{Kind: "err", Text: msg}, Data: totpData{Enrolled: s.TOTPEnabled, Pending: s.TOTPPending}})
		return
	}
	u.metrics.logins.WithLabelValues("totp_ok").Inc()
	// The pending cookie is dead on the auth side already; this replaces it
	// with the verified one.
	u.cfg.Cookies.Set(w, next.Token, next.ExpiresAt)
	if !s.TOTPEnabled {
		setFlash(w, "ok", "Authenticator confirmed. You are signed in.")
	}
	http.Redirect(w, r, HomePath, http.StatusSeeOther)
}

func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil {
		_ = u.sessions.AdminLogout(r.Context(), c.Value, clientIPFrom(r.Context()))
	}
	u.cfg.Cookies.Clear(w)
	http.Redirect(w, r, LoginPath, http.StatusSeeOther)
}
