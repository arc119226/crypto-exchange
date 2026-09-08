package admin

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
	"github.com/arc119226/crypto-exchange/internal/webhook"
)

// The people. A list to find one, a page to read one and change the two
// things an operator changes: KYC level and status. Both writes are the
// same functions the REST endpoints call (writes.go).

type usersData struct {
	Filter auth.UserFilter
	Users  []auth.User
	Pager  pager
}

type userData struct {
	User    auth.User
	Account *ledger.Account // the spot account, when the user has one
	Levels  []int
}

func (u *UI) users(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := auth.UserFilter{Email: strings.TrimSpace(q.Get("email")), Status: q.Get("status"), Role: q.Get("role")}
	limit, offset := pageQuery(q)
	users, err := u.h.users.ListUsers(r.Context(), f, limit, offset)
	if err != nil {
		u.fail(w, r, "users", "list users", err)
		return
	}
	keep := url.Values{}
	for _, k := range []string{"email", "status", "role"} {
		if q.Get(k) != "" {
			keep.Set(k, q.Get(k))
		}
	}
	u.tpl.render(w, r, http.StatusOK, "users", view{Title: "Users", Data: usersData{
		Filter: f, Users: users, Pager: newPager("/admin/users", keep, limit, offset, len(users)),
	}})
}

func (u *UI) user(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	usr, err := u.h.users.User(r.Context(), id)
	if errors.Is(err, auth.ErrNotFound) {
		setFlash(w, "err", "No user "+id+".")
		http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
		return
	}
	if err != nil {
		u.fail(w, r, "user", "get user", err)
		return
	}
	d := userData{User: usr, Levels: make([]int, 0, auth.MaxKYCLevel+1)}
	for i := 0; i <= auth.MaxKYCLevel; i++ {
		d.Levels = append(d.Levels, i)
	}
	switch acct, err := u.h.ledger.SpotAccountOf(r.Context(), u.h.pool, id); {
	case errors.Is(err, ledger.ErrAccountNotFound):
	case err != nil:
		u.fail(w, r, "user", "spot account", err)
		return
	default:
		d.Account = &acct
	}
	u.tpl.render(w, r, http.StatusOK, "user", view{Title: usr.Email, Path: "/admin/users", Data: d})
}

func (u *UI) setUserKYC(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	back := userPath(id)
	if err := r.ParseForm(); err != nil {
		u.bounce(w, r, back, "Bad form.")
		return
	}
	level, err := strconv.Atoi(r.PostFormValue("kyc_level"))
	if err != nil {
		u.bounce(w, r, back, "KYC level must be a number.")
		return
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if reason == "" {
		u.bounce(w, r, back, "A reason is required; it goes in the audit trail.")
		return
	}
	usr, err := u.h.setUserKYCLevel(r.Context(), id, level, reason)
	u.done(w, r, back, err, "KYC level is now "+strconv.Itoa(usr.KYCLevel)+".")
}

func (u *UI) setUserStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	back := userPath(id)
	if err := r.ParseForm(); err != nil {
		u.bounce(w, r, back, "Bad form.")
		return
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if reason == "" {
		u.bounce(w, r, back, "A reason is required; it goes in the audit trail.")
		return
	}
	usr, err := u.h.setUserStatus(r.Context(), id, r.PostFormValue("status"), reason)
	msg := "User is now " + usr.Status + "."
	if usr.Status == auth.StatusFrozen {
		msg = "User is frozen: no login, no orders, no withdrawals."
	}
	u.done(w, r, back, err, msg)
}

// userPath is the page of one user. The id came off the URL, so it is
// checked before it goes back into one; anything that is not an id goes to
// the list, which says the user does not exist.
func userPath(id string) string {
	if !idToken.MatchString(id) {
		return "/admin/users"
	}
	return "/admin/users/" + id
}

var idToken = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// bounce is a form that could not be read: say why and go back.
func (u *UI) bounce(w http.ResponseWriter, r *http.Request, back, text string) {
	setFlash(w, "err", text)
	http.Redirect(w, r, back, http.StatusSeeOther) //nolint:gosec // G710: back came from userPath, which admits only an id
}

// done turns a write's outcome into a flash and a redirect. The domain
// errors are the same ones the REST endpoints map to 400/404/409.
func (u *UI) done(w http.ResponseWriter, r *http.Request, back string, err error, ok string) {
	switch {
	case err == nil:
		if ok != "" {
			setFlash(w, "ok", ok)
		}
	case errors.Is(err, auth.ErrNotFound):
		setFlash(w, "err", "That user no longer exists.")
		back = "/admin/users"
	case errors.Is(err, auth.ErrLastAdmin):
		setFlash(w, "err", "This is the last active administrator; freezing them would lock everyone out.")
	case errors.Is(err, auth.ErrInvalidInput):
		setFlash(w, "err", strings.TrimPrefix(err.Error(), "auth: invalid input: "))
	case errors.Is(err, errAlreadyExists):
		setFlash(w, "err", "That one already exists; edit it below.")
	case errors.Is(err, registry.ErrNotFound):
		setFlash(w, "err", strings.TrimPrefix(err.Error(), "registry: not found: ")+" does not exist.")
	case errors.Is(err, registry.ErrInvalid):
		setFlash(w, "err", strings.TrimPrefix(err.Error(), "registry: invalid input: ")+".")
	case errors.Is(err, ledger.ErrAccountNotFound):
		setFlash(w, "err", "That account does not exist.")
	case errors.Is(err, ledger.ErrInvalidEntry), errors.Is(err, withdrawal.ErrInvalid), errors.Is(err, webhook.ErrInvalid),
		errors.Is(err, withdrawal.ErrNotReviewable), errors.Is(err, withdrawal.ErrNotResolvable),
		errors.Is(err, webhook.ErrDisabled), errors.Is(err, webhook.ErrQueued):
		setFlash(w, "err", sentence(err))
	case errors.Is(err, withdrawal.ErrNotFound):
		setFlash(w, "err", "That withdrawal does not exist.")
	case errors.Is(err, webhook.ErrNotFound):
		setFlash(w, "err", "That webhook endpoint or delivery does not exist.")
	default:
		telemetry.Logger(r.Context()).Error("admin: write failed", "path", r.URL.Path, "err", err.Error())
		setFlash(w, "err", "Something went wrong; nothing was changed.")
	}
	http.Redirect(w, r, back, http.StatusSeeOther) //nolint:gosec // G710: back came from userPath, which admits only an id
}

// fail is a read that could not be served: log it, show the page with an
// error and no data.
func (u *UI) fail(w http.ResponseWriter, r *http.Request, page, what string, err error) {
	telemetry.Logger(r.Context()).Error("admin: "+what, "err", err.Error())
	u.tpl.render(w, r, http.StatusInternalServerError, page, view{Title: "Error", Flash: &flash{Kind: "err", Text: "Could not read " + what + "."}})
}
