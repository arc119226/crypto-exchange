package admin

import (
	"net/http"

	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

func (u *UI) dashboard(w http.ResponseWriter, r *http.Request) {
	l := langFrom(r.Context())
	st, err := u.h.systemStatus(r.Context())
	if err != nil {
		telemetry.Logger(r.Context()).Error("admin: dashboard", "err", err.Error())
		u.tpl.render(w, r, http.StatusInternalServerError, "dashboard", view{Title: l.T("page.dashboard"), Flash: &flash{Kind: "err", Text: l.T("flash.system_status")}})
		return
	}
	u.tpl.render(w, r, http.StatusOK, "dashboard", view{Title: l.T("page.dashboard"), Data: st})
}
