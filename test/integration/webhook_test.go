//go:build integration

package integration

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/arc119226/crypto-exchange/internal/eventbus"
	"github.com/arc119226/crypto-exchange/internal/platform/secretbox"
	"github.com/arc119226/crypto-exchange/internal/webhook"
)

// webhookHarness is the ledger harness plus a dispatcher and a customer.
//
// It needs no NATS: Enqueue takes an envelope directly, and Deliver reads the
// queue table. That is a property of the two-halves design rather than a test
// convenience -- the delivery path can be exercised without a broker because
// nothing in it depends on one.
type webhookHarness struct {
	ledgerHarness
	dispatcher *webhook.Dispatcher
	secret     string
	endpointID string
	received   *receiver
}

// receiver is the customer's server: it records what arrived and answers with
// whatever the test told it to.
type receiver struct {
	mu       sync.Mutex
	requests []recorded
	codes    []int // consumed one per request; the last one repeats
	srv      *httptest.Server
}

type recorded struct {
	body      []byte
	signature string
	timestamp string
	eventID   string
	eventType string
}

func (r *receiver) got() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.requests...)
}

func (r *receiver) nextCode() int {
	if len(r.codes) == 0 {
		return http.StatusOK
	}
	c := r.codes[0]
	if len(r.codes) > 1 {
		r.codes = r.codes[1:]
	}
	return c
}

// newReceiver starts the customer's server.
func newReceiver(t *testing.T, codes ...int) *receiver {
	t.Helper()
	rec := &receiver{codes: codes}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		rec.mu.Lock()
		rec.requests = append(rec.requests, recorded{
			body: body, signature: req.Header.Get("X-Exchange-Signature"),
			timestamp: req.Header.Get("X-Exchange-Timestamp"),
			eventID:   req.Header.Get("X-Exchange-Event-Id"),
			eventType: req.Header.Get("X-Exchange-Event-Type"),
		})
		code := rec.nextCode()
		rec.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func setupWebhook(t *testing.T, backoff []time.Duration, codes ...int) webhookHarness {
	t.Helper()
	ctx := context.Background()
	lh := setupLedger(t)

	rec := newReceiver(t, codes...)

	master := make([]byte, secretbox.KeySize)
	_, err := rand.Read(master)
	require.NoError(t, err)
	secret := "whsec_" + hex.EncodeToString(master[:8])
	sealed, err := secretbox.Seal(master, secret)
	require.NoError(t, err)

	var id string
	require.NoError(t, lh.all.QueryRow(ctx,
		`INSERT INTO webhook.endpoints (url, secret_enc, events, label)
		 VALUES ($1, $2, $3, 'test') RETURNING id`,
		rec.srv.URL, sealed, []string{"trade.executed"}).Scan(&id))

	d := webhook.New(lh.all, webhook.Config{
		Tenant: "default", Backoff: backoff, Timeout: 5 * time.Second,
		Batch: 50, MasterKey: master,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	return webhookHarness{ledgerHarness: lh, dispatcher: d, secret: secret, endpointID: id, received: rec}
}

// event builds an envelope the way the relay would publish one.
func event(id string) eventbus.Envelope {
	return eventbus.Envelope{
		EventID: id, EventType: "trade.executed", SchemaVersion: 1, TenantID: "default",
		OccurredAt: time.Now().UTC().Truncate(time.Millisecond),
		Payload:    json.RawMessage(`{"trade_id":"t-1","price":"2000.00","qty":"0.5"}`),
	}
}

// deliveries reads the audit rows for one event, oldest attempt first.
func (h webhookHarness) deliveries(t *testing.T, ctx context.Context, eventID string) []struct {
	Attempt  int32
	Status   string
	Code     *int32
	Duration int32
} {
	t.Helper()
	rows, err := h.all.Query(ctx,
		`SELECT attempt, status, response_status, duration_ms FROM webhook.deliveries
		 WHERE event_id = $1 ORDER BY attempt`, eventID)
	require.NoError(t, err)
	defer rows.Close()
	var out []struct {
		Attempt  int32
		Status   string
		Code     *int32
		Duration int32
	}
	for rows.Next() {
		var r struct {
			Attempt  int32
			Status   string
			Code     *int32
			Duration int32
		}
		require.NoError(t, rows.Scan(&r.Attempt, &r.Status, &r.Code, &r.Duration))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func (h webhookHarness) queued(t *testing.T, ctx context.Context) int {
	t.Helper()
	var n int
	require.NoError(t, h.all.QueryRow(ctx, `SELECT count(*) FROM webhook.queue`).Scan(&n))
	return n
}

// verifyIndependently re-implements the scheme from docs/plan-v1.0.md §7.6
// rather than calling webhook.Verify. A customer integrating against the spec
// writes this function, not ours, so this is the thing that has to work.
func verifyIndependently(secret string, r recorded) bool {
	var v1 string
	for _, part := range strings.Split(r.signature, ",") {
		if k, v, ok := strings.Cut(part, "="); ok && k == "v1" {
			v1 = v
		}
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(r.timestamp + "." + string(r.body)))
	return v1 != "" && hmac.Equal([]byte(v1), []byte(hex.EncodeToString(mac.Sum(nil))))
}

// The Phase 5 DoD, verbatim: "webhook 端點回 500 兩次後第三次成功,deliveries
// 有 3 筆" (docs/plan-v1.0.md §12).
//
// The backoff is injected short, exactly as §7.6 says integration tests
// should ("整合測試注入 100ms,200ms,400ms").
func TestWebhookRetriesUntilTheEndpointAcceptsIt(t *testing.T) {
	ctx := context.Background()
	h := setupWebhook(t, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond},
		http.StatusInternalServerError, http.StatusInternalServerError, http.StatusOK)

	require.NoError(t, h.dispatcher.Enqueue(ctx, event("ev-1")))
	require.Equal(t, 1, h.queued(t, ctx))

	// Three ticks, each after the step before it has come due.
	for i, wait := range []time.Duration{0, 120 * time.Millisecond, 220 * time.Millisecond} {
		time.Sleep(wait)
		n, err := h.dispatcher.Deliver(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, n, "tick %d had nothing due", i)
	}

	got := h.deliveries(t, ctx, "ev-1")
	require.Len(t, got, 3, "500, 500, then 200 -- one row per attempt")
	assert.Equal(t, "failed", got[0].Status)
	assert.Equal(t, "failed", got[1].Status)
	assert.Equal(t, "delivered", got[2].Status)
	require.NotNil(t, got[0].Code)
	assert.EqualValues(t, 500, *got[0].Code)
	require.NotNil(t, got[2].Code)
	assert.EqualValues(t, 200, *got[2].Code)

	assert.Zero(t, h.queued(t, ctx), "delivered, so nothing is owed")
	assert.Len(t, h.received.got(), 3, "the customer really was called three times")
}

// What the customer receives has to be verifiable with nothing but the spec.
func TestWebhookDeliverySignatureAndHeaders(t *testing.T) {
	ctx := context.Background()
	h := setupWebhook(t, []time.Duration{time.Second})

	require.NoError(t, h.dispatcher.Enqueue(ctx, event("ev-sig")))
	_, err := h.dispatcher.Deliver(ctx)
	require.NoError(t, err)

	got := h.received.got()
	require.Len(t, got, 1)
	r := got[0]

	assert.True(t, verifyIndependently(h.secret, r), "a customer following §7.6 must be able to verify this")
	assert.False(t, verifyIndependently("wrong-secret", r))

	tampered := r
	tampered.body = append([]byte(nil), r.body...)
	tampered.body[len(tampered.body)-1] = ' '
	assert.False(t, verifyIndependently(h.secret, tampered), "a changed body must not verify")

	assert.Equal(t, "ev-sig", r.eventID, "§7.6 requires X-Exchange-Event-Id")
	assert.Equal(t, "trade.executed", r.eventType)
	assert.True(t, strings.HasPrefix(r.signature, "v1="), r.signature)

	// The body is the envelope, and it round-trips.
	var env eventbus.Envelope
	require.NoError(t, json.Unmarshal(r.body, &env))
	assert.Equal(t, "ev-sig", env.EventID)
	assert.JSONEq(t, `{"trade_id":"t-1","price":"2000.00","qty":"0.5"}`, string(env.Payload))
}

// Running off the end of the schedule is what dead means, and dead is final:
// the customer is not called again, and nothing stays owed.
func TestWebhookGivesUpAfterTheScheduleIsExhausted(t *testing.T) {
	ctx := context.Background()
	h := setupWebhook(t, []time.Duration{time.Millisecond, time.Millisecond}, http.StatusInternalServerError)

	require.NoError(t, h.dispatcher.Enqueue(ctx, event("ev-dead")))
	for range 4 {
		time.Sleep(5 * time.Millisecond)
		if _, err := h.dispatcher.Deliver(ctx); err != nil {
			require.NoError(t, err)
		}
	}

	got := h.deliveries(t, ctx, "ev-dead")
	require.Len(t, got, 3, "two waits means three attempts, then it stops")
	assert.Equal(t, "failed", got[0].Status)
	assert.Equal(t, "failed", got[1].Status)
	assert.Equal(t, "dead", got[2].Status, "the last one records why there will not be a fourth")
	assert.Zero(t, h.queued(t, ctx))
	assert.Len(t, h.received.got(), 3, "and the customer is not called a fourth time")
}

// JetStream is at-least-once, so the same event arrives again after a restart
// or a nak. Enqueuing it twice must not send it twice.
func TestWebhookEnqueueIsIdempotent(t *testing.T) {
	ctx := context.Background()
	h := setupWebhook(t, []time.Duration{time.Second})

	require.NoError(t, h.dispatcher.Enqueue(ctx, event("ev-dup")))
	require.NoError(t, h.dispatcher.Enqueue(ctx, event("ev-dup")))
	assert.Equal(t, 1, h.queued(t, ctx), "one row, however many times the event arrives")

	_, err := h.dispatcher.Deliver(ctx)
	require.NoError(t, err)
	assert.Len(t, h.received.got(), 1)

	// Once delivered the queue row is gone, so a redelivery arriving after
	// that does queue again and the customer is POSTed a second time. That is
	// not a defect: §7.6 promises at-least-once and says the customer
	// deduplicates on event_id. The window is narrow -- the ack goes out
	// immediately after enqueuing, long before delivery -- but it is real,
	// and pretending otherwise is how somebody later builds on an
	// exactly-once guarantee that was never there.
	require.NoError(t, h.dispatcher.Enqueue(ctx, event("ev-dup")))
	_, err = h.dispatcher.Deliver(ctx)
	require.NoError(t, err)
	assert.Len(t, h.received.got(), 2, "at-least-once: a late redelivery does reach the customer again")

	// And the table says so. 0016 had a partial unique index here that let
	// only one delivered row exist per endpoint per event, which read as a
	// guarantee about deliveries and was really a guarantee about rows: the
	// second POST happened either way, and the index only threw away the
	// evidence. Two sends, two records, two runs.
	var delivered, runs int
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT count(*), count(DISTINCT run_id) FROM webhook.deliveries
		 WHERE event_id = 'ev-dup' AND status = 'delivered'`).Scan(&delivered, &runs))
	assert.Equal(t, 2, delivered, "both successes are on the record")
	assert.Equal(t, 2, runs, "each redelivery is its own run")
}

// Replay can only re-send what still exists. The queue row is deleted the
// moment a delivery reaches a terminal state, so before 0017 the body went
// with it and there was nothing left to replay -- deliveries records what
// happened, not what was sent.
func TestWebhookKeepsTheBodyAfterDeliverySoItCanBeReplayed(t *testing.T) {
	ctx := context.Background()
	h := setupWebhook(t, []time.Duration{time.Second})

	require.NoError(t, h.dispatcher.Enqueue(ctx, event("ev-keep")))
	_, err := h.dispatcher.Deliver(ctx)
	require.NoError(t, err)
	require.Zero(t, h.queued(t, ctx), "delivered, so the queue row is gone")

	var body []byte
	var eventType string
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT body, event_type FROM webhook.events WHERE event_id = $1`, "ev-keep").
		Scan(&body, &eventType))
	assert.Equal(t, "trade.executed", eventType)

	// And it is the same bytes, not an equivalent re-encoding: a replay signs
	// what it sends, so anything that round-trips through a normaliser would
	// produce a signature the customer's first delivery did not have.
	assert.Equal(t, h.received.got()[0].body, body)
}

// One row per event, however many endpoints want it. Storing the body per
// subscriber would mean three copies of one JSON document and a replay having
// to choose between them.
func TestWebhookStoresTheBodyOncePerEvent(t *testing.T) {
	ctx := context.Background()
	h := setupWebhook(t, []time.Duration{time.Second})

	// A second endpoint on the same event.
	sealed, err := secretbox.Seal(make([]byte, secretbox.KeySize), "other")
	require.NoError(t, err)
	_, err = h.all.Exec(ctx,
		`INSERT INTO webhook.endpoints (url, secret_enc, events) VALUES ($1, $2, $3)`,
		h.received.srv.URL, sealed, []string{"trade.executed"})
	require.NoError(t, err)

	require.NoError(t, h.dispatcher.Enqueue(ctx, event("ev-two")))
	assert.Equal(t, 2, h.queued(t, ctx), "one queue row per endpoint")

	var events int
	require.NoError(t, h.all.QueryRow(ctx,
		`SELECT count(*) FROM webhook.events WHERE event_id = $1`, "ev-two").Scan(&events))
	assert.Equal(t, 1, events, "but one body")
}

// An event nobody subscribes to is acked rather than queued: delivered, in
// the only sense that applies.
func TestWebhookIgnoresEventsNobodyWants(t *testing.T) {
	ctx := context.Background()
	h := setupWebhook(t, []time.Duration{time.Second})

	e := event("ev-other")
	e.EventType = "withdrawal.state_changed"
	require.NoError(t, h.dispatcher.Enqueue(ctx, e))
	assert.Zero(t, h.queued(t, ctx))
}
