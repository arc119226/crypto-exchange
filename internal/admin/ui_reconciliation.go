package admin

import (
	"errors"
	"net/http"

	"github.com/arc119226/crypto-exchange/internal/chain/reconcile"
	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// The reconciliation page: the latest pass, the passes before it, every
// break of both kinds, and the house adjustments that answered them --
// with the form to post the next one (docs/domain.md, open question 8).

type reconciliationData struct {
	Latest           *reconcile.Report
	Reports          []reconcile.Report
	ChainBreaks      []reconcile.Break
	LedgerBreaks     []ledger.Break
	HouseAdjustments []ledger.JournalEntry
	Assets           []registry.Asset
	HouseCodes       []ledger.HouseCode
}

func (u *UI) reconciliation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := reconciliationData{HouseCodes: ledger.AllHouseCodes}
	switch report, err := reconcile.Latest(ctx, u.h.pool, u.h.tenant, u.h.chainID); {
	case errors.Is(err, reconcile.ErrNoReport):
	case err != nil:
		u.fail(w, r, "reconciliation", "latest report", err)
		return
	default:
		d.Latest = &report
	}
	var err error
	if d.Reports, err = reconcile.List(ctx, u.h.pool, u.h.tenant, u.h.chainID, 20, 0); err != nil {
		u.fail(w, r, "reconciliation", "reports", err)
		return
	}
	if d.ChainBreaks, err = reconcile.Breaks(ctx, u.h.pool, u.h.tenant, u.h.chainID, 50, 0); err != nil {
		u.fail(w, r, "reconciliation", "chain breaks", err)
		return
	}
	if d.LedgerBreaks, err = u.h.ledger.Breaks(ctx, 50, 0); err != nil {
		u.fail(w, r, "reconciliation", "ledger breaks", err)
		return
	}
	if d.HouseAdjustments, err = u.h.ledger.Entries(ctx, ledger.EntriesFilter{RefType: "house_adjustment", Limit: 20}); err != nil {
		u.fail(w, r, "reconciliation", "house adjustments", err)
		return
	}
	if d.Assets, err = u.h.registry.ListAssets(ctx, u.h.tenant); err != nil {
		u.fail(w, r, "reconciliation", "assets", err)
		return
	}
	u.tpl.render(w, r, http.StatusOK, "reconciliation", view{Title: "Reconciliation", Data: d})
}
