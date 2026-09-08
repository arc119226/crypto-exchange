package admin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/arc119226/crypto-exchange/internal/webhook"
)

// The webhooks page: endpoints with their controls, one endpoint's
// deliveries with a replay button on each, and the one place a signing
// secret is ever shown.
//
// A secret shown once is the contract (docs/webhooks.md). After a create or
// a rotation the page redirects, as every form does, so the browser cannot
// resubmit on refresh; the secret rides across that redirect in a
// server-side stash keyed by a random token, read once and forgotten. Not
// in the flash cookie: a cookie is written to disk by the browser.

type revealed struct {
	EndpointID string
	Secret     string
	Until      *time.Time // the old secret's end, after a rotation
	expires    time.Time
}

type revealStash struct {
	mu sync.Mutex
	m  map[string]revealed
}

const revealTTL = 5 * time.Minute

func (s *revealStash) put(r revealed) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	r.expires = time.Now().Add(revealTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]revealed{}
	}
	for k, v := range s.m {
		if time.Now().After(v.expires) {
			delete(s.m, k)
		}
	}
	s.m[token] = r
	return token
}

func (s *revealStash) take(token string) (revealed, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[token]
	if ok {
		delete(s.m, token)
	}
	if !ok || time.Now().After(r.expires) {
		return revealed{}, false
	}
	return r, true
}

type webhooksData struct {
	Endpoints  []webhook.Endpoint
	Reveal     *revealed
	Selected   *webhook.Endpoint
	Deliveries []webhook.Delivery
	Pager      pager
}

func (u *UI) webhooks(w http.ResponseWriter, r *http.Request) {
	ctx, q := r.Context(), r.URL.Query()
	var d webhooksData
	if u.h.webhooks == nil {
		u.tpl.render(w, r, http.StatusOK, "webhooks", view{Title: "Webhooks", Flash: &flash{Kind: "err", Text: "Webhooks are disabled: WEBHOOK_SIGNING_KEY is not set."}, Data: d})
		return
	}
	var (
		err    error
		notice *flash
	)
	if d.Endpoints, err = u.h.webhooks.List(ctx); err != nil {
		u.fail(w, r, "webhooks", "endpoints", err)
		return
	}
	if token := q.Get("reveal"); token != "" {
		if rv, ok := u.reveals.take(token); ok {
			d.Reveal = &rv
		}
	}
	if id := q.Get("endpoint"); id != "" {
		switch ep, err := u.h.webhooks.Get(ctx, id); {
		case errors.Is(err, webhook.ErrNotFound):
			notice = &flash{Kind: "err", Text: "No webhook endpoint " + id + "."}
		case err != nil:
			u.fail(w, r, "webhooks", "endpoint", err)
			return
		default:
			d.Selected = &ep
			limit, offset := pageQuery(q)
			if d.Deliveries, err = u.h.webhooks.Deliveries(ctx, ep.ID, limit, offset); err != nil {
				u.fail(w, r, "webhooks", "deliveries", err)
				return
			}
			d.Pager = newPager("/admin/webhooks", keepQuery(q, "endpoint"), limit, offset, len(d.Deliveries))
		}
	}
	u.tpl.render(w, r, http.StatusOK, "webhooks", view{Title: "Webhooks", Flash: notice, Data: d})
}

func eventsField(s string) []string {
	var out []string
	for _, e := range strings.Split(s, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

func (u *UI) createWebhook(w http.ResponseWriter, r *http.Request) {
	f := newForm(r)
	url, events, label := f.str("url"), eventsField(f.str("events")), f.str("label")
	if f.problem != "" {
		u.bounce(w, r, "/admin/webhooks", f.problem)
		return
	}
	ep, secret, err := u.h.createWebhookEndpoint(r.Context(), url, events, label)
	if err != nil {
		u.done(w, r, "/admin/webhooks", err, "")
		return
	}
	token := u.reveals.put(revealed{EndpointID: ep.ID, Secret: secret})
	http.Redirect(w, r, "/admin/webhooks?reveal="+token, http.StatusSeeOther)
}

func (u *UI) updateWebhook(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	f := newForm(r)
	url, events, label, reason := f.str("url"), eventsField(f.str("events")), f.str("label"), f.reason()
	if f.problem != "" {
		u.bounce(w, r, "/admin/webhooks", f.problem)
		return
	}
	ep, err := u.h.updateWebhookEndpoint(r.Context(), id, url, events, label, reason)
	u.done(w, r, "/admin/webhooks", err, "Endpoint "+ep.ID+" saved.")
}

func (u *UI) setWebhookStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	f := newForm(r)
	status, reason := f.str("status"), f.reason()
	if f.problem != "" {
		u.bounce(w, r, "/admin/webhooks", f.problem)
		return
	}
	ep, err := u.h.setWebhookEndpointStatus(r.Context(), id, status, reason)
	msg := "Endpoint " + ep.ID + " is now " + ep.Status + "."
	if ep.Status == webhook.StatusDisabled {
		msg += " Whatever was queued for it was dropped."
	}
	u.done(w, r, "/admin/webhooks", err, msg)
}

func (u *UI) rotateWebhookSecretPage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	f := newForm(r)
	hours, reason := f.i32("grace_hours"), f.reason()
	if f.problem != "" {
		u.bounce(w, r, "/admin/webhooks", f.problem)
		return
	}
	rot, err := u.h.rotateWebhookSecret(r.Context(), id, time.Duration(hours)*time.Hour, reason)
	if err != nil {
		u.done(w, r, "/admin/webhooks", err, "")
		return
	}
	until := rot.PreviousUntil
	token := u.reveals.put(revealed{EndpointID: rot.Endpoint.ID, Secret: rot.Secret, Until: &until})
	http.Redirect(w, r, "/admin/webhooks?reveal="+token, http.StatusSeeOther)
}

func (u *UI) replayWebhookPage(w http.ResponseWriter, r *http.Request) {
	id, deliveryID := chi.URLParam(r, "id"), chi.URLParam(r, "delivery_id")
	back := "/admin/webhooks?endpoint=" + id
	rep, err := u.h.replayWebhookDelivery(r.Context(), id, deliveryID)
	if err != nil {
		u.done(w, r, back, err, "")
		return
	}
	u.done(w, r, back, nil, "Queued event "+rep.EventID+" again; the first attempt is due "+rep.NextAttemptAt.UTC().Format("15:04:05Z")+".")
}
