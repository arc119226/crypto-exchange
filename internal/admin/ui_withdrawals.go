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
	d := withdrawalsData{Actions: resolveActions, Lookup: strings.TrimSpace(r.URL.Query().Get("id"))}
	if u.h.withdrawals == nil {
		u.tpl.render(w, r, http.StatusOK, "withdrawals", view{Title: "Withdrawals", Flash: &flash{Kind: "err", Text: "Withdrawals are not enabled on this deployment."}, Data: d})
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
			notice = &flash{Kind: "err", Text: "No withdrawal " + d.Lookup + "."}
		case err != nil:
			u.fail(w, r, "withdrawals", "withdrawal", err)
			return
		default:
			d.Selected = &rec
		}
	}
	u.tpl.render(w, r, http.StatusOK, "withdrawals", view{Title: "Withdrawals", Flash: notice, Data: d})
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
		u.bounce(w, r, "/admin/withdrawals", "The decision must be approve or reject.")
		return
	}
	if !approve && note == "" {
		u.bounce(w, r, "/admin/withdrawals", "A rejection must carry a note saying why.")
		return
	}
	rec, err := u.h.reviewWithdrawal(r.Context(), id, approve, note)
	u.done(w, r, "/admin/withdrawals", err, "Withdrawal "+rec.ID+" is now "+rec.Status+".")
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
		u.bounce(w, r, "/admin/withdrawals", "A resolution must carry a note saying why.")
		return
	}
	rec, err := u.h.resolveWithdrawal(r.Context(), id, action, note)
	back := "/admin/withdrawals"
	if rec.ID != "" {
		back += "?id=" + rec.ID
	}
	u.done(w, r, back, err, "Requested "+string(action)+" on "+rec.ID+"; the chain role applies it on its next tick.")
}
