package admin

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/arc119226/crypto-exchange/internal/auth"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
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
	back := "/admin/users/" + id
	if err := r.ParseForm(); err != nil {
		setFlash(w, "err", "Bad form.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	level, err := strconv.Atoi(r.PostFormValue("kyc_level"))
	if err != nil {
		setFlash(w, "err", "KYC level must be a number.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if reason == "" {
		setFlash(w, "err", "A reason is required; it goes in the audit trail.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	usr, err := u.h.setUserKYCLevel(r.Context(), id, level, reason)
	u.done(w, r, back, err, "KYC level is now "+strconv.Itoa(usr.KYCLevel)+".")
}

func (u *UI) setUserStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	back := "/admin/users/" + id
	if err := r.ParseForm(); err != nil {
		setFlash(w, "err", "Bad form.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if reason == "" {
		setFlash(w, "err", "A reason is required; it goes in the audit trail.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	usr, err := u.h.setUserStatus(r.Context(), id, r.PostFormValue("status"), reason)
	msg := "User is now " + usr.Status + "."
	if usr.Status == auth.StatusFrozen {
		msg = "User is frozen: no login, no orders, no withdrawals."
	}
	u.done(w, r, back, err, msg)
}

// done turns a write's outcome into a flash and a redirect. The domain
// errors are the same ones the REST endpoints map to 400/404/409.
func (u *UI) done(w http.ResponseWriter, r *http.Request, back string, err error, ok string) {
	switch {
	case err == nil:
		setFlash(w, "ok", ok)
	case errors.Is(err, auth.ErrNotFound):
		setFlash(w, "err", "That user no longer exists.")
		back = "/admin/users"
	case errors.Is(err, auth.ErrLastAdmin):
		setFlash(w, "err", "This is the last active administrator; freezing them would lock everyone out.")
	case errors.Is(err, auth.ErrInvalidInput):
		setFlash(w, "err", strings.TrimPrefix(err.Error(), "auth: invalid input: "))
	default:
		telemetry.Logger(r.Context()).Error("admin: write failed", "path", r.URL.Path, "err", err.Error())
		setFlash(w, "err", "Something went wrong; nothing was changed.")
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// fail is a read that could not be served: log it, show the page with an
// error and no data.
func (u *UI) fail(w http.ResponseWriter, r *http.Request, page, what string, err error) {
	telemetry.Logger(r.Context()).Error("admin: "+what, "err", err.Error())
	u.tpl.render(w, r, http.StatusInternalServerError, page, view{Title: "Error", Flash: &flash{Kind: "err", Text: "Could not read " + what + "."}})
}
