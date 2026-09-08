//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/admin"
	"github.com/arc119226/crypto-exchange/internal/admin/gen"
	"github.com/arc119226/crypto-exchange/internal/audit"
	"github.com/arc119226/crypto-exchange/internal/platform/pg"
	"github.com/arc119226/crypto-exchange/internal/platform/secretbox"
	"github.com/arc119226/crypto-exchange/internal/registry"
	"github.com/arc119226/crypto-exchange/internal/telemetry"
	"github.com/arc119226/crypto-exchange/internal/webhook"
)

// webhookAdminHarness is the admin API and the delivery loop over one
// database, which is how they really run: different roles, different
// privileges, the same tables.
type webhookAdminHarness struct {
	ledgerHarness
	srv        *httptest.Server
	dispatcher *webhook.Dispatcher
	received   *receiver
	endpointID string
	secret     string
}

func setupWebhookAdmin(t *testing.T, backoff []time.Duration, codes ...int) webhookAdminHarness {
	t.Helper()
	lh := setupLedger(t)
	rec := newReceiver(t, codes...)

	master := make([]byte, secretbox.KeySize)
	_, err := rand.Read(master)
	require.NoError(t, err)

	r := chi.NewRouter()
	r.Use(telemetry.CorrelationMiddleware(slog.New(slog.NewTextHandler(io.Discard, nil))))
	r.Use(admin.RequireAPIKey(adminKey))
	admin.Mount(r, admin.NewHandler(lh.all, lh.svc, registry.NewStore(lh.all), audit.NewRecorder("default"), "default").
		WithWebhooks(webhook.NewStore(lh.all, "default", master)))
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	d := webhook.New(lh.all, webhook.Config{
		Tenant: "default", Backoff: backoff, Timeout: 5 * time.Second,
		Batch: 50, MasterKey: master,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	h := webhookAdminHarness{ledgerHarness: lh, srv: srv, dispatcher: d, received: rec}
	created := decode[gen.CreatedWebhookEndpoint](t, call(t, srv, http.MethodPost, "/admin/v1/webhooks", adminKey,
		gen.WebhookEndpointRequest{URL: rec.srv.URL, Events: []string{"trade.executed"}, Label: "integration"}))
	h.endpointID, h.secret = created.ID, created.Secret
	return h
}

// deliver runs one tick and insists it had something to do.
func (h webhookAdminHarness) deliver(t *testing.T, ctx context.Context) {
	t.Helper()
	n, err := h.dispatcher.Deliver(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n, "expected exactly one delivery to be due")
}

func (h webhookAdminHarness) listDeliveries(t *testing.T) []gen.WebhookDelivery {
	t.Helper()
	r := call(t, h.srv, http.MethodGet, "/admin/v1/webhooks/"+h.endpointID+"/deliveries", adminKey, nil)
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	return decode[gen.WebhookDeliveryList](t, r).Deliveries
}

func (h webhookAdminHarness) replay(t *testing.T, deliveryID string) adminResp {
	t.Helper()
	return call(t, h.srv, http.MethodPost,
		"/admin/v1/webhooks/"+h.endpointID+"/deliveries/"+deliveryID+"/replay", adminKey, nil)
}

func fast() []time.Duration {
	return []time.Duration{50 * time.Millisecond, 100 * time.Millisecond}
}

// The secret is the reason this endpoint exists, and the reason it is
// dangerous: it has to be shown once and never again. "Never again" is
// structural here -- gen.WebhookEndpoint has no field for it -- so this test
// is about the create response being the only one that carries it.
func TestWebhookEndpointSecretIsShownOnceAndNeverAgain(t *testing.T) {
	h := setupWebhookAdmin(t, fast())

	assert.NotEmpty(t, h.secret)
	assert.Len(t, h.secret, 64, "32 bytes hex, same shape as an API key secret")

	r := call(t, h.srv, http.MethodGet, "/admin/v1/webhooks", adminKey, nil)
	require.Equal(t, http.StatusOK, r.status)
	assert.NotContains(t, string(r.body), h.secret,
		"the list must not be able to produce the secret, in any field")

	list := decode[gen.WebhookEndpointList](t, r).WebhookEndpoints
	require.Len(t, list, 1)
	assert.Equal(t, h.endpointID, list[0].ID)
	assert.Equal(t, gen.WebhookEndpointStatus("active"), list[0].Status)
}

// The §12 DoD from the operator's side: the three rows the delivery test
// asserts in SQL are the three rows a person sees.
func TestWebhookDeliveriesEndpointShowsEveryAttempt(t *testing.T) {
	ctx := context.Background()
	h := setupWebhookAdmin(t, fast(), http.StatusInternalServerError, http.StatusInternalServerError, http.StatusOK)

	require.NoError(t, h.dispatcher.Enqueue(ctx, event("ev-admin-1")))
	for _, wait := range []time.Duration{0, 60 * time.Millisecond, 110 * time.Millisecond} {
		time.Sleep(wait)
		h.deliver(t, ctx)
	}

	got := h.listDeliveries(t)
	require.Len(t, got, 3, "500, 500, then 200 -- one row per attempt")
	// Newest first.
	assert.Equal(t, gen.WebhookDeliveryStatus("delivered"), got[0].Status)
	assert.Equal(t, gen.WebhookDeliveryStatus("failed"), got[1].Status)
	assert.Equal(t, gen.WebhookDeliveryStatus("failed"), got[2].Status)
	require.NotNil(t, got[1].ResponseStatus)
	assert.Equal(t, 500, *got[1].ResponseStatus)
	assert.Equal(t, int32(2), got[0].Attempt, "attempt is the step in the schedule")

	runs := map[string]struct{}{}
	for _, d := range got {
		runs[d.RunID] = struct{}{}
	}
	assert.Len(t, runs, 1, "one pass through the schedule is one run")
}

// Replaying something that already succeeded is the case the schema could not
// record before: the endpoint gets a second POST, and until run_id existed the
// row proving it was silently dropped on a unique index.
func TestWebhookReplayOfADeliveredEventIsSentAgainAndRecorded(t *testing.T) {
	ctx := context.Background()
	h := setupWebhookAdmin(t, fast(), http.StatusOK)

	require.NoError(t, h.dispatcher.Enqueue(ctx, event("ev-admin-2")))
	h.deliver(t, ctx)
	first := h.listDeliveries(t)
	require.Len(t, first, 1)
	require.Equal(t, gen.WebhookDeliveryStatus("delivered"), first[0].Status)
	require.Len(t, h.received.got(), 1)

	r := h.replay(t, first[0].ID)
	require.Equal(t, http.StatusAccepted, r.status, string(r.body))
	replay := decode[gen.WebhookReplay](t, r)
	assert.Equal(t, "ev-admin-2", replay.EventID)
	assert.NotEqual(t, first[0].RunID, replay.RunID, "a replay is a new run")

	h.deliver(t, ctx)

	assert.Len(t, h.received.got(), 2, "the customer receives the event a second time")
	second := h.listDeliveries(t)
	require.Len(t, second, 2, "and both deliveries are on the record")
	assert.Equal(t, replay.RunID, second[0].RunID)
	assert.Equal(t, int32(0), second[0].Attempt, "the replay starts the schedule again")
	assert.Equal(t, gen.WebhookDeliveryStatus("delivered"), second[0].Status)

	// The bodies are identical: a replay re-sends what happened. Nothing in the
	// admin path can edit the payload, and ex_admin has no INSERT on
	// webhook.events to make that possible.
	sent := h.received.got()
	assert.Equal(t, sent[0].body, sent[1].body)
	assert.Equal(t, sent[0].eventID, sent[1].eventID, "same event_id: receivers deduplicate")
}

// The worst form of the same defect. A dead delivery has used every step of
// the schedule, so a replay that restarted from attempt 0 collided at every
// single one and produced no rows at all.
func TestWebhookReplayOfADeadDeliveryRunsTheWholeScheduleAgain(t *testing.T) {
	ctx := context.Background()
	h := setupWebhookAdmin(t, fast(), http.StatusInternalServerError)

	require.NoError(t, h.dispatcher.Enqueue(ctx, event("ev-admin-3")))
	for _, wait := range []time.Duration{0, 60 * time.Millisecond, 110 * time.Millisecond} {
		time.Sleep(wait)
		h.deliver(t, ctx)
	}
	dead := h.listDeliveries(t)
	require.Len(t, dead, 3, "two backoff steps means three attempts, then it gives up")
	require.Equal(t, gen.WebhookDeliveryStatus("dead"), dead[0].Status)
	require.Equal(t, 0, h.queued(t, ctx), "nothing is owed after it dies")

	// The endpoint is fixed and the operator asks for it again.
	h.received.mu.Lock()
	h.received.codes = []int{http.StatusOK}
	h.received.mu.Unlock()

	r := h.replay(t, dead[0].ID)
	require.Equal(t, http.StatusAccepted, r.status, string(r.body))
	h.deliver(t, ctx)

	got := h.listDeliveries(t)
	require.Len(t, got, 4, "the replay's attempt is recorded, not swallowed")
	assert.Equal(t, gen.WebhookDeliveryStatus("delivered"), got[0].Status)
	assert.Equal(t, int32(0), got[0].Attempt, "the schedule starts over rather than continuing at 3")
}

// A replay of something still on the retry schedule is refused, and the
// refusal says when the retry is due -- because the answer to "make it send
// again" is that it already is going to.
func TestWebhookReplayIsRefusedWhileTheEventIsStillQueued(t *testing.T) {
	ctx := context.Background()
	h := setupWebhookAdmin(t, fast(), http.StatusInternalServerError)

	require.NoError(t, h.dispatcher.Enqueue(ctx, event("ev-admin-4")))
	h.deliver(t, ctx)
	failed := h.listDeliveries(t)
	require.Len(t, failed, 1)
	require.Equal(t, gen.WebhookDeliveryStatus("failed"), failed[0].Status)
	require.Equal(t, 1, h.queued(t, ctx), "still owed: the schedule has steps left")

	before := len(h.received.got())
	r := h.replay(t, failed[0].ID)
	require.Equal(t, http.StatusConflict, r.status, string(r.body))
	assert.Contains(t, string(r.body), "next attempt at")
	assert.Equal(t, before, len(h.received.got()), "and nothing was sent")
	assert.Equal(t, 1, h.queued(t, ctx), "the original run is untouched")
}

// A replay to a disabled endpoint would write a queue row nothing can ever
// pick up: ClaimDue only looks at active endpoints, and no delete path outside
// the dispatcher would reach it.
func TestWebhookReplayIsRefusedForADisabledEndpoint(t *testing.T) {
	ctx := context.Background()
	h := setupWebhookAdmin(t, fast(), http.StatusOK)

	require.NoError(t, h.dispatcher.Enqueue(ctx, event("ev-admin-5")))
	h.deliver(t, ctx)
	delivered := h.listDeliveries(t)
	require.Len(t, delivered, 1)

	require.Equal(t, http.StatusOK, call(t, h.srv, http.MethodPut,
		"/admin/v1/webhooks/"+h.endpointID+"/status", adminKey,
		gen.WebhookEndpointStatusRequest{Status: "disabled", Reason: "customer paused the integration"}).status)

	r := h.replay(t, delivered[0].ID)
	require.Equal(t, http.StatusConflict, r.status, string(r.body))
	assert.Contains(t, string(r.body), "disabled")
	assert.Equal(t, 0, h.queued(t, ctx), "and it left nothing stuck behind")
}

// Disabling an endpoint has to drop what is queued for it. Left in place those
// rows are unreachable rather than pending, and they would come back out at
// the customer the day the integration was switched on again.
func TestDisablingAnEndpointDropsWhatWasQueuedForIt(t *testing.T) {
	ctx := context.Background()
	h := setupWebhookAdmin(t, fast(), http.StatusInternalServerError)

	require.NoError(t, h.dispatcher.Enqueue(ctx, event("ev-admin-6")))
	h.deliver(t, ctx)
	require.Equal(t, 1, h.queued(t, ctx))

	require.Equal(t, http.StatusOK, call(t, h.srv, http.MethodPut,
		"/admin/v1/webhooks/"+h.endpointID+"/status", adminKey,
		gen.WebhookEndpointStatusRequest{Status: "disabled", Reason: "endpoint retired"}).status)
	assert.Equal(t, 0, h.queued(t, ctx), "nothing is owed to an endpoint that was turned off")
	assert.Len(t, h.listDeliveries(t), 1, "the history survives; only the work list is cleared")

	// Turning it back on does not resurrect them.
	require.Equal(t, http.StatusOK, call(t, h.srv, http.MethodPut,
		"/admin/v1/webhooks/"+h.endpointID+"/status", adminKey,
		gen.WebhookEndpointStatusRequest{Status: "active", Reason: "back in service"}).status)
	assert.Equal(t, 0, h.queued(t, ctx))
	n, err := h.dispatcher.Deliver(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "no stale events fire on re-enable")

	// The count is in the audit trail, which is the only place that will ever
	// say why the customer did not receive it.
	events := decode[gen.AuditEventList](t, call(t, h.srv, http.MethodGet,
		"/admin/v1/audit-events?action=webhook.endpoint.status.update", adminKey, nil)).Events
	require.NotEmpty(t, events)
	var dropped float64
	for _, e := range events {
		after, ok := e.After.(map[string]any)
		if !ok || after["status"] != "disabled" {
			continue
		}
		dropped, _ = after["queued_dropped"].(float64)
	}
	assert.Equal(t, float64(1), dropped, "the audit record says how many were dropped")
}

// Updating replaces the mutable configuration and records what it was before.
func TestWebhookEndpointUpdateIsRecorded(t *testing.T) {
	h := setupWebhookAdmin(t, fast())

	r := call(t, h.srv, http.MethodPut, "/admin/v1/webhooks/"+h.endpointID, adminKey,
		gen.WebhookEndpointUpdateRequest{
			URL: "https://example.com/hooks/v2", Events: []string{"trade.executed", "withdrawal.state_changed"},
			Label: "integration", Reason: "customer moved the receiver",
		})
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	got := decode[gen.WebhookEndpoint](t, r)
	assert.Equal(t, "https://example.com/hooks/v2", got.URL)
	assert.Len(t, got.Events, 2)

	bad := call(t, h.srv, http.MethodPut, "/admin/v1/webhooks/"+h.endpointID, adminKey,
		gen.WebhookEndpointUpdateRequest{URL: "ftp://example.com", Events: []string{"trade.executed"}, Reason: "no"})
	assert.Equal(t, http.StatusBadRequest, bad.status, "the URL scheme is checked before the database CHECK")

	missing := call(t, h.srv, http.MethodGet, "/admin/v1/webhooks/does-not-exist/deliveries", adminKey, nil)
	assert.Equal(t, http.StatusNotFound, missing.status,
		"an endpoint that does not exist is not an endpoint with no history")
}

// gatedReceiver holds each delivery open until the test lets it answer, which
// is how two overlapping claims can be arranged without a sleep.
type gatedReceiver struct {
	arrived chan struct{}
	release chan struct{}
}

func newGatedReceiver(t *testing.T) (*gatedReceiver, *httptest.Server) {
	t.Helper()
	g := &gatedReceiver{arrived: make(chan struct{}, 4), release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		g.arrived <- struct{}{}
		<-g.release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return g, srv
}

// A settle arriving late from a claim that has already been superseded must
// not delete the run that replaced it.
//
// claim() commits before any HTTP happens and ClaimDue does not advance
// next_attempt_at, so two ticks really can hold the same row and both POST.
// The second one to finish settles against a run that is already over; without
// the run_id fence its Dequeue deletes whatever now holds the key, which after
// a replay is the operator's new run -- no POST, no delivery row, and a 202
// that meant nothing.
func TestAStaleSettleCannotDequeueAnotherRun(t *testing.T) {
	ctx := context.Background()
	h := setupWebhookAdmin(t, fast(), http.StatusOK)

	// Point the endpoint at a receiver the test can hold open.
	gate, gated := newGatedReceiver(t)
	require.Equal(t, http.StatusOK, call(t, h.srv, http.MethodPut, "/admin/v1/webhooks/"+h.endpointID, adminKey,
		gen.WebhookEndpointUpdateRequest{
			URL: gated.URL, Events: []string{"trade.executed"}, Reason: "hold deliveries open",
		}).status)

	require.NoError(t, h.dispatcher.Enqueue(ctx, event("ev-admin-7")))

	// Two ticks claim the same row: the first is already past claim() and
	// waiting on the receiver when the second one claims.
	ticks := make(chan error, 2)
	go func() { _, err := h.dispatcher.Deliver(ctx); ticks <- err }()
	<-gate.arrived
	go func() { _, err := h.dispatcher.Deliver(ctx); ticks <- err }()
	<-gate.arrived

	// Let the first finish. It settles, and its run is over.
	gate.release <- struct{}{}
	require.NoError(t, <-ticks)
	require.Equal(t, 0, h.queued(t, ctx))

	first := h.listDeliveries(t)
	require.Len(t, first, 1)
	r := h.replay(t, first[0].ID)
	require.Equal(t, http.StatusAccepted, r.status, string(r.body))
	replay := decode[gen.WebhookReplay](t, r)
	require.Equal(t, 1, h.queued(t, ctx))

	// Now the straggler settles, against a run that no longer exists.
	gate.release <- struct{}{}
	require.NoError(t, <-ticks)

	assert.Equal(t, 1, h.queued(t, ctx), "the replay survives the stale settle")
	var stillThere string
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT run_id FROM webhook.queue WHERE event_id = 'ev-admin-7'`).Scan(&stillThere))
	assert.Equal(t, replay.RunID, stillThere, "and it is the operator's run, not a resurrected one")
}

// Migration 0017's claim about privileges, checked rather than asserted in a
// comment: a replay re-sends what happened, so admin can queue one but can
// never write the bytes it will send.
func TestAdminRoleCannotRewriteAnEventOrDeleteAnEndpoint(t *testing.T) {
	ctx := context.Background()
	h := setupWebhookAdmin(t, fast(), http.StatusOK)
	require.NoError(t, h.dispatcher.Enqueue(ctx, event("ev-admin-8")))

	adminPool, err := pg.Open(ctx, pg.PoolConfig{DSN: h.DSN("ex_admin"), MaxConns: 2})
	require.NoError(t, err)
	defer adminPool.Close()

	_, err = adminPool.Exec(ctx,
		`INSERT INTO webhook.events (event_id, event_type, body) VALUES ('forged', 'trade.executed', '{}')`)
	require.Error(t, err, "an admin who could write an event could send a customer one the exchange never produced")
	assert.Contains(t, err.Error(), "permission denied")

	_, err = adminPool.Exec(ctx, `DELETE FROM webhook.endpoints WHERE id = $1`, h.endpointID)
	require.Error(t, err, "endpoints are disabled, never deleted: the delivery history has to outlive them")
	assert.Contains(t, err.Error(), "permission denied")

	_, err = adminPool.Exec(ctx, `UPDATE webhook.deliveries SET status = 'delivered' WHERE event_id = 'ev-admin-8'`)
	require.Error(t, err, "an attempt is what happened; a replay is a new row, not an edit of the old one")
	assert.Contains(t, err.Error(), "permission denied")

	// What it can do.
	var n int
	require.NoError(t, adminPool.QueryRow(ctx, `SELECT count(*) FROM webhook.events`).Scan(&n))
	assert.Equal(t, 1, n)
}

func (h webhookAdminHarness) queued(t *testing.T, ctx context.Context) int {
	t.Helper()
	var n int
	require.NoError(t, h.all.QueryRow(ctx, `SELECT count(*) FROM webhook.queue`).Scan(&n))
	return n
}

// 5c: a rotation keeps the old secret signing for a grace period, so the
// receiver switches at its own pace and never drops a delivery.
func TestWebhookSecretRotationSignsWithBothUntilTheGraceEnds(t *testing.T) {
	ctx := context.Background()
	h := setupWebhookAdmin(t, fast(), 200)
	rotate := func(body any) adminResp {
		return call(t, h.srv, http.MethodPost, "/admin/v1/webhooks/"+h.endpointID+"/rotate-secret", adminKey, body)
	}
	now := time.Now().UTC()
	h.dispatcher.WithClock(func() time.Time { return now })

	r := rotate(map[string]any{"grace_hours": 1, "reason": "quarterly rotation"})
	require.Equal(t, http.StatusOK, r.status, string(r.body))
	rotated := decode[gen.RotatedWebhookSecret](t, r)
	assert.NotEqual(t, h.secret, rotated.Secret)
	assert.WithinDuration(t, now.Add(time.Hour), rotated.PreviousSecretUntil, 5*time.Second)

	t.Run("inside the grace period a delivery carries both signatures", func(t *testing.T) {
		require.NoError(t, h.dispatcher.Enqueue(ctx, event("rotate-1")))
		h.deliver(t, ctx)
		got := h.received.got()
		require.Len(t, got, 1)
		assert.Equal(t, 2, strings.Count(got[0].signature, "v1="), got[0].signature)
		assert.NoError(t, webhook.Verify(rotated.Secret, got[0].signature, got[0].timestamp, got[0].body, now, time.Minute), "a receiver that switched")
		assert.NoError(t, webhook.Verify(h.secret, got[0].signature, got[0].timestamp, got[0].body, now, time.Minute), "one that has not")
		assert.True(t, verifyIndependently(h.secret, got[0]), "the doc's own snippet still verifies with the old secret")
	})

	t.Run("after it only the new secret signs", func(t *testing.T) {
		later := now.Add(2 * time.Hour)
		h.dispatcher.WithClock(func() time.Time { return later })
		require.NoError(t, h.dispatcher.Enqueue(ctx, event("rotate-2")))
		h.deliver(t, ctx)
		got := h.received.got()
		require.Len(t, got, 2)
		assert.Equal(t, 1, strings.Count(got[1].signature, "v1="), got[1].signature)
		assert.NoError(t, webhook.Verify(rotated.Secret, got[1].signature, got[1].timestamp, got[1].body, later, time.Minute))
		assert.ErrorIs(t, webhook.Verify(h.secret, got[1].signature, got[1].timestamp, got[1].body, later, time.Minute), webhook.ErrBadSignature)
	})

	t.Run("rotating again replaces the old secret; the newest two sign", func(t *testing.T) {
		h.dispatcher.WithClock(func() time.Time { return now })
		r := rotate(map[string]any{"grace_hours": 2, "reason": "rotated again"})
		require.Equal(t, http.StatusOK, r.status, string(r.body))
		second := decode[gen.RotatedWebhookSecret](t, r)
		require.NoError(t, h.dispatcher.Enqueue(ctx, event("rotate-3")))
		h.deliver(t, ctx)
		got := h.received.got()
		require.Len(t, got, 3)
		assert.Equal(t, 2, strings.Count(got[2].signature, "v1="))
		assert.NoError(t, webhook.Verify(second.Secret, got[2].signature, got[2].timestamp, got[2].body, now, time.Minute))
		assert.NoError(t, webhook.Verify(rotated.Secret, got[2].signature, got[2].timestamp, got[2].body, now, time.Minute))
		assert.ErrorIs(t, webhook.Verify(h.secret, got[2].signature, got[2].timestamp, got[2].body, now, time.Minute), webhook.ErrBadSignature, "the first secret is gone for good")
		rotated = second
	})

	t.Run("the audit trail says when, never what", func(t *testing.T) {
		rows, err := h.all.Query(ctx, `SELECT after::text FROM audit.audit_events WHERE action = 'webhook.endpoint.rotate_secret' ORDER BY id`)
		require.NoError(t, err)
		defer rows.Close()
		var n int
		for rows.Next() {
			var after string
			require.NoError(t, rows.Scan(&after))
			assert.Contains(t, after, "previous_secret_until")
			assert.NotContains(t, after, rotated.Secret)
			assert.NotContains(t, after, h.secret)
			n++
		}
		assert.Equal(t, 2, n)
	})

	t.Run("refusals", func(t *testing.T) {
		assert.Equal(t, http.StatusBadRequest, rotate(map[string]any{"grace_hours": 0, "reason": "x"}).status)
		assert.Equal(t, http.StatusBadRequest, rotate(map[string]any{"grace_hours": 200, "reason": "x"}).status, "a week is the most")
		assert.Equal(t, http.StatusBadRequest, rotate(map[string]any{"grace_hours": 1}).status, "no reason, no rotation")
		assert.Equal(t, http.StatusNotFound, call(t, h.srv, http.MethodPost, "/admin/v1/webhooks/00000000-0000-0000-0000-000000000000/rotate-secret", adminKey, map[string]any{"reason": "x"}).status)
	})
}
