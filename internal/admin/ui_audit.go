package admin

import (
	"net/http"
	"strings"

	"github.com/arc119226/crypto-exchange/internal/audit"
)

// The audit page: the trail, newest first, narrowed by who, what and to
// whom. Every write the back office makes lands here, so this is also where
// an operator checks their own work.

var actorTypes = []audit.ActorType{audit.ActorAdmin, audit.ActorAPIKey, audit.ActorUser, audit.ActorSystem}

type auditData struct {
	Filter     audit.Filter
	Events     []audit.Record
	Pager      pager
	ActorTypes []audit.ActorType
}

func (u *UI) audit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, offset := pageQuery(q)
	f := audit.Filter{
		Action: strings.TrimSpace(q.Get("action")), TargetType: strings.TrimSpace(q.Get("target_type")), TargetID: strings.TrimSpace(q.Get("target_id")),
		ActorType: q.Get("actor_type"), ActorID: strings.TrimSpace(q.Get("actor_id")), Limit: limit, Offset: offset,
	}
	events, err := u.h.audit.List(r.Context(), u.h.pool, f)
	if err != nil {
		u.fail(w, r, "audit", "audit trail", err)
		return
	}
	keep := keepQuery(q, "action", "target_type", "target_id", "actor_type", "actor_id")
	u.tpl.render(w, r, http.StatusOK, "audit", view{Title: langFrom(r.Context()).T("page.audit"), Data: auditData{
		Filter: f, Events: events, Pager: newPager("/admin/audit", keep, limit, offset, len(events)), ActorTypes: actorTypes,
	}})
}
