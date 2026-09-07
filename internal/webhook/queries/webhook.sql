-- name: ListActiveEndpoints :many
-- Every endpoint that could want an event, for one tenant. Filtering by
-- event_type happens in Go against the events array: the array is small, and
-- a GIN index maintained for a predicate that changes with every event buys
-- nothing at this size.
SELECT id, url, secret_enc, events, label
FROM webhook.endpoints
WHERE tenant_id = $1 AND status = 'active'
ORDER BY created_at;

-- name: Enqueue :execrows
-- Idempotent under JetStream's at-least-once redelivery: the primary key is
-- (tenant, endpoint, event), so the same event arriving twice is a no-op
-- rather than a second POST. Returns 0 when it was already queued.
INSERT INTO webhook.queue (tenant_id, endpoint_id, event_id, event_type, body)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT DO NOTHING;

-- name: ClaimDue :many
-- What is owed now. FOR UPDATE SKIP LOCKED so two workers can drain the same
-- queue without either waiting on the other or sending the same event twice.
SELECT q.tenant_id, q.endpoint_id, q.event_id, q.event_type, q.body, q.attempts,
       e.url, e.secret_enc
FROM webhook.queue q
JOIN webhook.endpoints e ON e.id = q.endpoint_id
WHERE q.tenant_id = $1 AND q.next_attempt_at <= now() AND e.status = 'active'
ORDER BY q.next_attempt_at
LIMIT $2
FOR UPDATE OF q SKIP LOCKED;

-- name: RecordAttempt :exec
-- One row per try, append-only. The unique index on
-- (tenant, endpoint, event, attempt) makes a re-run of the same attempt a
-- conflict rather than a second row, so a crash between sending and recording
-- cannot double-count.
INSERT INTO webhook.deliveries (
    tenant_id, endpoint_id, event_id, event_type, attempt, status,
    response_status, error, duration_ms, delivered_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT DO NOTHING;

-- name: RescheduleQueued :exec
-- The attempt failed and the schedule has more steps left.
UPDATE webhook.queue
SET attempts = attempts + 1, next_attempt_at = $4
WHERE tenant_id = $1 AND endpoint_id = $2 AND event_id = $3;

-- name: Dequeue :exec
-- Delivered, or out of schedule. Either way nothing more is owed, and what
-- survives is the delivery rows -- the queue entry held no history.
DELETE FROM webhook.queue
WHERE tenant_id = $1 AND endpoint_id = $2 AND event_id = $3;
