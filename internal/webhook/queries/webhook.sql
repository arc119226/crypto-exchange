-- name: ListActiveEndpoints :many
-- Every endpoint that could want an event, for one tenant. Filtering by
-- event_type happens in Go against the events array: the array is small, and
-- a GIN index maintained for a predicate that changes with every event buys
-- nothing at this size.
SELECT id, url, secret_enc, events, label
FROM webhook.endpoints
WHERE tenant_id = $1 AND status = 'active'
ORDER BY created_at;

-- name: RecordEvent :exec
-- The event body, stored once however many endpoints want it. Must run before
-- Enqueue: the queue's foreign key points here.
INSERT INTO webhook.events (tenant_id, event_id, event_type, body)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;

-- name: Enqueue :execrows
-- Idempotent under JetStream's at-least-once redelivery: the primary key is
-- (tenant, endpoint, event), so the same event arriving twice is a no-op
-- rather than a second POST. Returns 0 when it was already queued.
--
-- run_id is not named here. A queue row is a run, and the default mints one.
INSERT INTO webhook.queue (tenant_id, endpoint_id, event_id)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING;

-- name: ClaimDue :many
-- What is owed now. FOR UPDATE SKIP LOCKED keeps two workers off each other's
-- rows for the length of this transaction -- which ends before any HTTP
-- happens, so it does not stop both from claiming the same row on successive
-- ticks and both POSTing. deliveries_attempt_uniq is what makes the second
-- one's bookkeeping a no-op, and run_id is what fences its queue mutation.
SELECT q.tenant_id, q.endpoint_id, q.event_id, q.run_id, ev.event_type, ev.body, q.attempts,
       e.url, e.secret_enc
FROM webhook.queue q
JOIN webhook.endpoints e ON e.id = q.endpoint_id
JOIN webhook.events ev ON ev.tenant_id = q.tenant_id AND ev.event_id = q.event_id
WHERE q.tenant_id = $1 AND q.next_attempt_at <= now() AND e.status = 'active'
ORDER BY q.next_attempt_at
LIMIT $2
FOR UPDATE OF q SKIP LOCKED;

-- name: RecordAttempt :exec
-- One row per try, append-only. The conflict target is spelled out rather than
-- left bare: the only collision that may pass silently is two workers settling
-- the same claimed attempt, and naming it means any other constraint this
-- table grows will fail loudly instead of losing a row.
INSERT INTO webhook.deliveries (
    tenant_id, endpoint_id, event_id, run_id, event_type, attempt, status,
    response_status, error, duration_ms, delivered_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (tenant_id, endpoint_id, event_id, run_id, attempt) DO NOTHING;

-- name: RescheduleQueued :exec
-- The attempt failed and the schedule has more steps left. Fenced on run_id:
-- a settle arriving late from a claim that has already been superseded must
-- not push a schedule it is no longer part of.
UPDATE webhook.queue
SET attempts = attempts + 1, next_attempt_at = $5
WHERE tenant_id = $1 AND endpoint_id = $2 AND event_id = $3 AND run_id = $4;

-- name: Dequeue :exec
-- Delivered, or out of schedule. Either way nothing more is owed, and what
-- survives is the delivery rows -- the queue entry held no history.
--
-- Fenced on run_id, and this one is not a nicety: without it a straggler
-- settling a finished run deletes whatever row now holds that key, which
-- after a replay is the operator's freshly enqueued run.
DELETE FROM webhook.queue
WHERE tenant_id = $1 AND endpoint_id = $2 AND event_id = $3 AND run_id = $4;

-- Admin side (docs/plan-v1.0.md §7.6 "後台可查、可手動 replay").

-- name: CreateEndpoint :one
INSERT INTO webhook.endpoints (tenant_id, url, secret_enc, events, label)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, url, events, label, status, created_at, updated_at;

-- name: ListEndpoints :many
-- Disabled ones included: an operator managing endpoints has to see the one
-- they turned off, which is the difference between this and the dispatcher's
-- ListActiveEndpoints.
SELECT id, url, events, label, status, created_at, updated_at
FROM webhook.endpoints
WHERE tenant_id = $1
ORDER BY created_at;

-- name: GetEndpoint :one
SELECT id, url, events, label, status, created_at, updated_at
FROM webhook.endpoints
WHERE tenant_id = $1 AND id = $2;

-- name: UpdateEndpoint :one
-- A whole replacement of the mutable configuration, which is what PUT means.
-- The secret is not among the fields: rotating it is a separate operation with
-- its own once-only response.
UPDATE webhook.endpoints
SET url = $3, events = $4, label = $5, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING id, url, events, label, status, created_at, updated_at;

-- name: SetEndpointStatus :one
UPDATE webhook.endpoints
SET status = $3, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING id, url, events, label, status, created_at, updated_at;

-- name: ListDeliveries :many
-- What happened, newest first (deliveries_recent_idx is in this order).
SELECT id, event_id, event_type, run_id, attempt, status, response_status,
       error, duration_ms, created_at, delivered_at
FROM webhook.deliveries
WHERE tenant_id = $1 AND endpoint_id = $2
ORDER BY created_at DESC, attempt DESC
LIMIT $3 OFFSET $4;

-- name: GetDelivery :one
-- Which event an operator is pointing at. They pick a delivery row because
-- that is what the list shows them; what gets replayed is the event behind it.
SELECT id, event_id, run_id, attempt, status
FROM webhook.deliveries
WHERE tenant_id = $1 AND endpoint_id = $2 AND id = $3;

-- name: EnqueueReplay :one
-- A replay is a new run of an event that already happened. No body is written
-- and none is read: webhook.events already holds the exact bytes, and ex_admin
-- has no INSERT on it precisely so a replay cannot become a rewrite.
--
-- No rows back means the primary key is taken -- this event is still queued
-- for this endpoint, so it is going to be sent anyway.
INSERT INTO webhook.queue (tenant_id, endpoint_id, event_id)
VALUES ($1, $2, $3)
ON CONFLICT DO NOTHING
RETURNING run_id, next_attempt_at;

-- name: GetQueued :one
-- Only used to say when the retry that blocked a replay is due.
SELECT run_id, attempts, next_attempt_at
FROM webhook.queue
WHERE tenant_id = $1 AND endpoint_id = $2 AND event_id = $3;

-- name: DeleteQueuedForEndpoint :execrows
-- Disabling an endpoint stops what is owed to it. Leaving the rows would make
-- them unreachable rather than pending: ClaimDue skips inactive endpoints, and
-- every other delete path is inside settle.
DELETE FROM webhook.queue
WHERE tenant_id = $1 AND endpoint_id = $2;
