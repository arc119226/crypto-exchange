-- Event bus queries: the outbox (producers INSERT in their own transaction,
-- the relay reads and stamps published_at) and consumer idempotency.

-- name: InsertOutbox :one
INSERT INTO eventbus.outbox (
    event_id, event_type, schema_version, tenant_id, market_id, account_id, seq, account_seq,
    subject, headers, payload, occurred_at, correlation_id, causation_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING id;

-- name: ListUnpublished :many
SELECT * FROM eventbus.outbox
WHERE published_at IS NULL
ORDER BY id
LIMIT $1;

-- name: MarkPublished :exec
UPDATE eventbus.outbox SET published_at = now() WHERE id = ANY($1::bigint[]) AND published_at IS NULL;

-- name: OutboxBacklog :one
SELECT count(*)::bigint AS backlog,
       COALESCE(EXTRACT(EPOCH FROM (now() - min(occurred_at))), 0)::double precision AS oldest_age_seconds
  FROM eventbus.outbox
 WHERE published_at IS NULL;

-- name: ListOutboxByAccountSince :many
-- Private-stream resume: every event of one account after account_seq.
SELECT * FROM eventbus.outbox
WHERE tenant_id = $1 AND account_id = $2 AND account_seq > $3
ORDER BY account_seq
LIMIT $4;

-- name: ListOutboxByMarket :many
SELECT * FROM eventbus.outbox
WHERE tenant_id = $1 AND market_id = $2
ORDER BY id
LIMIT $3 OFFSET $4;

-- name: MarkProcessed :execrows
-- 0 rows affected means the consumer already processed this event.
INSERT INTO eventbus.processed_events (consumer, event_id)
VALUES ($1, $2)
ON CONFLICT (consumer, event_id) DO NOTHING;

-- Retention (docs/plan-v1.0.md §7.3: the outbox is kept 30 days) --------
-- The worker role's retention loop calls these with a cutoff and a batch
-- size until a call deletes fewer rows than the batch.

-- name: PruneOutbox :execrows
-- Published rows older than the cutoff; unpublished rows are never touched
-- (the relay still owes them to JetStream). Old rows have the lowest ids,
-- so walking the primary key finds them without a new index.
DELETE FROM eventbus.outbox o
 WHERE o.id IN (SELECT i.id FROM eventbus.outbox i
                 WHERE i.published_at IS NOT NULL AND i.published_at < $1
                 ORDER BY i.id LIMIT $2);

-- name: PruneProcessedEvents :execrows
DELETE FROM eventbus.processed_events p
 WHERE (p.consumer, p.event_id) IN (SELECT i.consumer, i.event_id FROM eventbus.processed_events i
                                     WHERE i.processed_at < $1 LIMIT $2);
