package admin

import (
	"errors"
	"net/http"
	"strings"

	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/chain/deposit"
	"github.com/arc119226/crypto-exchange/internal/chain/hotwallet"
	"github.com/arc119226/crypto-exchange/internal/chain/sweep"
)

// The chain page: the hot wallet, deposits as they are seen and credited,
// and sweeps. All of it read-only; the chain role is the one that acts.

var depositStatuses = []string{"detected", "confirming", "credited", "orphaned", "dropped", "reversed"}

type chainData struct {
	HotWallet *gen.HotWallet
	Deposits  []deposit.Record
	Status    string
	Asset     string
	Statuses  []string
	Pager     pager
	Sweeps    []sweep.Record
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
	u.tpl.render(w, r, http.StatusOK, "chain", view{Title: langFrom(ctx).T("page.chain"), Data: d})
}
