package admin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/arc119226/crypto-exchange/internal/ledger"
	"github.com/arc119226/crypto-exchange/internal/registry"
)

// The ledger page: the trial balance and the house accounts at the top, one
// account's balances and entries when asked for, and the two forms that
// post an administrator's entries -- the same writes as the REST endpoints,
// with the idempotency key minted here because a browser has none.

type ledgerData struct {
	Trial    []ledger.TrialBalanceLine
	Balanced bool
	House    []ledger.HouseBalance
	// One account, when ?account_id= names one.
	AccountID string
	Account   *ledger.Account
	Balances  []ledger.Balance
	Entries   []ledger.JournalEntry
	Filter    ledger.EntriesFilter
	Pager     pager
	// For the forms.
	Assets     []registry.Asset
	HouseCodes []ledger.HouseCode
}

func (u *UI) ledger(w http.ResponseWriter, r *http.Request) {
	ctx, q := r.Context(), r.URL.Query()
	d := ledgerData{HouseCodes: ledger.AllHouseCodes, Balanced: true}
	var (
		err    error
		notice *flash
	)
	if d.Trial, err = u.h.ledger.TrialBalance(ctx); err != nil {
		u.fail(w, r, "ledger", "trial balance", err)
		return
	}
	for _, l := range d.Trial {
		if !l.Diff.IsZero() {
			d.Balanced = false
		}
	}
	if d.House, err = u.h.ledger.HouseBalances(ctx); err != nil {
		u.fail(w, r, "ledger", "house balances", err)
		return
	}
	if d.Assets, err = u.h.registry.ListAssets(ctx, u.h.tenant); err != nil {
		u.fail(w, r, "ledger", "assets", err)
		return
	}
	limit, offset := pageQuery(q)
	d.AccountID = strings.TrimSpace(q.Get("account_id"))
	d.Filter = ledger.EntriesFilter{AccountID: d.AccountID, RefType: q.Get("ref_type"), RefID: q.Get("ref_id"), Limit: limit, Offset: offset}
	if d.AccountID != "" {
		switch acct, err := u.h.ledger.Account(ctx, d.AccountID); {
		case errors.Is(err, ledger.ErrAccountNotFound):
			notice = &flash{Kind: "err", Text: langFrom(ctx).T("flash.no_account", d.AccountID)}
			d.AccountID, d.Filter.AccountID = "", ""
		case err != nil:
			u.fail(w, r, "ledger", "account", err)
			return
		default:
			d.Account = &acct
			if d.Balances, err = u.h.ledger.Balances(ctx, acct.ID); err != nil {
				u.fail(w, r, "ledger", "balances", err)
				return
			}
		}
	}
	if d.Entries, err = u.h.ledger.Entries(ctx, d.Filter); err != nil {
		u.fail(w, r, "ledger", "entries", err)
		return
	}
	keep := keepQuery(q, "account_id", "ref_type", "ref_id")
	d.Pager = newPager("/admin/ledger", keep, limit, offset, len(d.Entries))
	u.tpl.render(w, r, http.StatusOK, "ledger", view{Title: langFrom(ctx).T("page.ledger"), Flash: notice, Data: d})
}

func (u *UI) createAdjustment(w http.ResponseWriter, r *http.Request) {
	f := newForm(r)
	p := ledger.AdjustParams{
		AccountID: f.str("account_id"), Asset: f.str("asset"), Amount: f.amount("amount"),
		Direction: ledger.Direction(f.str("direction")), Reason: f.reason(), IdempotencyKey: newIdempotencyKey(),
	}
	back := "/admin/ledger?account_id=" + p.AccountID
	if f.problem != "" {
		u.bounce(w, r, "/admin/ledger", f.problem)
		return
	}
	entry, _, err := u.h.createAdjustment(r.Context(), p)
	u.done(w, r, back, err, f.l.T("flash.adjustment_posted", itoa64(entry.ID)))
}

func (u *UI) createHouseAdjustment(w http.ResponseWriter, r *http.Request) {
	f := newForm(r)
	p := ledger.HouseAdjustParams{
		Code: ledger.HouseCode(f.str("code")), Asset: f.str("asset"), Amount: f.amount("amount"),
		Direction: ledger.Direction(f.str("direction")), Reason: f.reason(), IdempotencyKey: newIdempotencyKey(),
	}
	if f.problem != "" {
		u.bounce(w, r, "/admin/reconciliation", f.problem)
		return
	}
	entry, _, err := u.h.createHouseAdjustment(r.Context(), p)
	u.done(w, r, "/admin/reconciliation", err, f.l.T("flash.house_adjustment_posted", itoa64(entry.ID)))
}

// newIdempotencyKey mints the key a browser cannot supply. A double submit
// of the same form is therefore two entries; the page says so next to the
// button, and the audit trail shows both.
func newIdempotencyKey() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "ui:" + hex.EncodeToString(b[:])
}
