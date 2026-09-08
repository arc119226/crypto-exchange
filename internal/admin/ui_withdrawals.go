package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/arc119226/crypto-exchange/internal/chain/withdrawal"
)

// The withdrawal page: the review queue with a decision under every row,
// and one withdrawal's record, with the resolutions, when asked for by id.

var resolveActions = []withdrawal.Action{withdrawal.ActionBump, withdrawal.ActionCancelNonce, withdrawal.ActionRefund, withdrawal.ActionRetry}

type withdrawalsData struct {
	Pending  []withdrawal.Record
	Lookup   string
	Selected *withdrawal.Record
	Actions  []withdrawal.Action
}

func (u *UI) withdrawals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l := langFrom(ctx)
	d := withdrawalsData{Actions: resolveActions, Lookup: strings.TrimSpace(r.URL.Query().Get("id"))}
	if u.h.withdrawals == nil {
		u.tpl.render(w, r, http.StatusOK, "withdrawals", view{Title: l.T("page.withdrawals"), Flash: &flash{Kind: "err", Text: l.T("flash.withdrawals_disabled")}, Data: d})
		return
	}
	var (
		err    error
		notice *flash
	)
	if d.Pending, err = u.h.withdrawals.Pending(ctx, 100); err != nil {
		u.fail(w, r, "withdrawals", "review queue", err)
		return
	}
	if d.Lookup != "" {
		switch rec, err := u.h.withdrawals.Get(ctx, d.Lookup); {
		case errors.Is(err, withdrawal.ErrNotFound):
			notice = &flash{Kind: "err", Text: l.T("flash.no_withdrawal", d.Lookup)}
		case err != nil:
			u.fail(w, r, "withdrawals", "withdrawal", err)
			return
		default:
			d.Selected = &rec
		}
	}
	u.tpl.render(w, r, http.StatusOK, "withdrawals", view{Title: l.T("page.withdrawals"), Flash: notice, Data: d})
}

func (u *UI) reviewWithdrawalPage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	f := newForm(r)
	decision, note := f.str("decision"), f.str("note")
	if f.problem != "" {
		u.bounce(w, r, "/admin/withdrawals", f.problem)
		return
	}
	approve := decision == "approve"
	if !approve && decision != "reject" {
		u.bounce(w, r, "/admin/withdrawals", f.l.T("flash.decision"))
		return
	}
	if !approve && note == "" {
		u.bounce(w, r, "/admin/withdrawals", f.l.T("flash.reject_note"))
		return
	}
	rec, err := u.h.reviewWithdrawal(r.Context(), id, approve, note)
	u.done(w, r, "/admin/withdrawals", err, f.l.T("flash.withdrawal_status", rec.ID, f.l.Status(rec.Status)))
}

func (u *UI) resolveWithdrawalPage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	f := newForm(r)
	action, note := withdrawal.Action(f.str("action")), f.str("note")
	if f.problem != "" {
		u.bounce(w, r, "/admin/withdrawals", f.problem)
		return
	}
	if note == "" {
		u.bounce(w, r, "/admin/withdrawals", f.l.T("flash.resolve_note"))
		return
	}
	rec, err := u.h.resolveWithdrawal(r.Context(), id, action, note)
	back := "/admin/withdrawals"
	if rec.ID != "" {
		back += "?id=" + rec.ID
	}
	u.done(w, r, back, err, f.l.T("flash.resolve_requested", f.l.Status(action), rec.ID))
}
