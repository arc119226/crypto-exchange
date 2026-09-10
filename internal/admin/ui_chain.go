package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/chain/deposit"
	"github.com/arc119226/crypto-exchange/internal/chain/hotwallet"
	"github.com/arc119226/crypto-exchange/internal/chain/sweep"
)

// The chain page: the hot wallet, deposits as they are seen and credited,
// and sweeps. Read-only but for one thing -- confirming that a deposit a
// reorg took away should be reversed (§6.4.1). Even that only records the
// decision; the chain role is the one that acts.

var depositStatuses = []string{"detected", "confirming", "credited", "orphaned", "dropped", "reversed"}

type chainData struct {
	HotWallet *gen.HotWallet
	Deposits  []deposit.Record
	Status    string
	Asset     string
	Statuses  []string
	Pager     pager
	Sweeps    []sweep.Record
	// AwaitingReversal is credited deposits the chain no longer shows. Empty
	// in a healthy exchange, and the section is hidden when it is: this is
	// not something an operator should get used to seeing.
	AwaitingReversal []deposit.Record
}

func (u *UI) chain(w http.ResponseWriter, r *http.Request) {
	ctx, q := r.Context(), r.URL.Query()
	d := chainData{Statuses: depositStatuses, Status: q.Get("status"), Asset: strings.ToUpper(strings.TrimSpace(q.Get("asset")))}
	switch hw, err := u.h.hotWallet(ctx); {
	case errors.Is(err, hotwallet.ErrNoHotWallet):
	case err != nil:
		u.fail(w, r, "chain", "hot wallet", err)
		return
	default:
		d.HotWallet = &hw
	}
	limit, offset := pageQuery(q)
	if u.h.deposits != nil {
		var err error
		if d.Deposits, err = u.h.deposits.List(ctx, d.Status, d.Asset, limit, offset); err != nil {
			u.fail(w, r, "chain", "deposits", err)
			return
		}
	}
	d.Pager = newPager("/admin/chain", keepQuery(q, "status", "asset"), limit, offset, len(d.Deposits))
	var err error
	if d.Sweeps, err = sweep.List(ctx, u.h.pool, u.h.tenant, 50); err != nil {
		u.fail(w, r, "chain", "sweeps", err)
		return
	}
	if u.h.depositReviewer != nil {
		if d.AwaitingReversal, err = u.h.depositReviewer.AwaitingReversal(ctx, 50, 0); err != nil {
			u.fail(w, r, "chain", "deposits awaiting reversal", err)
			return
		}
	}
	u.tpl.render(w, r, http.StatusOK, "chain", view{Title: langFrom(ctx).T("page.chain"), Data: d})
}

// reverseDepositPage is the one write on this page: an operator confirming
// that a deposit the chain no longer shows should be undone. It records the
// decision and nothing more -- the chain role posts the reversing entry on
// its next tick, and may still refuse if the account has spent the money.
func (u *UI) reverseDepositPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if u.h.depositReviewer == nil {
		u.fail(w, r, "chain", "deposit reversal", errors.New("admin: deposits are not enabled on this deployment"))
		return
	}
	f := newForm(r)
	id := chi.URLParam(r, "id")
	if f.problem != "" {
		u.bounce(w, r, "/admin/chain", f.problem)
		return
	}
	if f.str("reason") == "" {
		u.bounce(w, r, "/admin/chain", f.l.T("flash.reversal_reason"))
		return
	}
	actor := actorFrom(ctx)
	_, err := u.h.depositReviewer.RequestReversal(ctx, deposit.ReverseParams{
		ID: id, Note: f.str("reason"),
		ActorType: actor.Type, ActorID: actor.ID, IP: actor.IP,
	})
	u.done(w, r, "/admin/chain", err, f.l.T("flash.reversal_requested", id))
}
