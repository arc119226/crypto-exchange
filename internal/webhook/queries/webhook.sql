-- name: ListActiveEndpoints :many
-- Every endpoint that could want an event, for one tenant. Filtering by
-- event_type happens in Go against the events array: the array is small, and
-- a GIN index maintained for a predicate that changes with every event buys
-- nothing at this size.
SELECT id, url, secret_enc, events, label
FROM webhook.endpoints
WHERE tenant_id = $1 AND status = 'active'
ORDER BY created_at;

-- name: RecordAttempt :one
-- One row per try. The unique index on (tenant, endpoint, event, attempt)
-- makes a re-run of the same attempt a conflict rather than a second row, so
-- a crash between sending and recording cannot double-count.
INSERT INTO webhook.deliveries (
    tenant_id, endpoint_id, event_id, event_type, attempt, status,
    response_status, error, delivered_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id;

-- name: CountAttempts :one
-- How many times this event has been tried against this endpoint, which is
-- both the next attempt number and the retry budget check.
SELECT count(*) FROM webhook.deliveries
WHERE tenant_id = $1 AND endpoint_id = $2 AND event_id = $3;

-- name: AlreadyDelivered :one
-- JetStream is at-least-once, so the same event arrives again after a
-- restart or a nak. This is what stops the customer getting it twice.
SELECT EXISTS (
    SELECT 1 FROM webhook.deliveries
    WHERE tenant_id = $1 AND endpoint_id = $2 AND event_id = $3 AND status = 'delivered'
);
