package admin

import (
	"net/http"

	"github.com/arc119226/crypto-exchange/internal/telemetry"
)

func (u *UI) dashboard(w http.ResponseWriter, r *http.Request) {
	st, err := u.h.systemStatus(r.Context())
	if err != nil {
		telemetry.Logger(r.Context()).Error("admin: dashboard", "err", err.Error())
		u.tpl.render(w, r, http.StatusInternalServerError, "dashboard", view{Title: "Dashboard", Flash: &flash{Kind: "err", Text: "Could not read the system status."}})
		return
	}
	u.tpl.render(w, r, http.StatusOK, "dashboard", view{Title: "Dashboard", Data: st})
}
