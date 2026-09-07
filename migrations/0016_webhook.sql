-- +goose Up
-- Outbound webhooks (docs/plan-v1.0.md §2, §5.2, §12 Phase 5).
--
-- The `webhook` schema has been reserved and granted USAGE since 0001; this is
-- the first thing to live in it. Events already reach JetStream through the
-- outbox and relay, so what is missing is only the last hop: which customer
-- URLs want which events, and what happened each time we tried.

CREATE TABLE webhook.endpoints (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  text        NOT NULL DEFAULT 'default',
    url        text        NOT NULL CHECK (url ~ '^https?://'),
    -- AES-256-GCM(nonce || ciphertext) of the signing secret, under
    -- WEBHOOK_SIGNING_KEY (docs/plan-v1.0.md §16 names it, and compose has
    -- fed it to worker and admin since before anything read it). Same
    -- envelope as auth.api_keys.secret_enc, different key: one master key per
    -- secret domain, so rotating webhook secrets never touches API keys.
    secret_enc bytea       NOT NULL,
    -- Event types this endpoint wants, matched against the envelope's
    -- event_type. Empty would mean "an endpoint that receives nothing", which
    -- is a disabled endpoint spelled confusingly, so it is refused.
    events     text[]      NOT NULL CHECK (cardinality(events) > 0),
    label      text        NOT NULL DEFAULT '',
    -- disabled is reversible; a customer pausing an integration is not the
    -- same as deleting it, and their delivery history has to survive it.
    status     text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The dispatcher's only read: every active endpoint for a tenant, once per
-- event. Filtering by event_type happens in Go against the events array
-- rather than in SQL, because the array is small and the alternative is a
-- GIN index maintained for a predicate that changes on every event.
CREATE INDEX endpoints_active_idx ON webhook.endpoints (tenant_id, status);

CREATE TABLE webhook.deliveries (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   text        NOT NULL DEFAULT 'default',
    endpoint_id uuid        NOT NULL REFERENCES webhook.endpoints (id),
    -- The envelope's event_id, not a foreign key: the outbox row it came from
    -- is cleaned up on its own schedule, and a delivery record has to outlive
    -- it to be evidence.
    event_id    text        NOT NULL,
    event_type  text        NOT NULL,
    -- One row per attempt, not one row per event. §12's DoD says so in as
    -- many words -- "500 twice then success, deliveries has 3 rows" -- and it
    -- is the right shape: what an operator asks is "what happened", and an
    -- UPDATE-in-place row can only answer "what happened last".
    attempt     int         NOT NULL CHECK (attempt >= 0),
    status      text        NOT NULL CHECK (status IN ('pending', 'delivered', 'failed', 'dead')),
    -- Null when the attempt never got an answer: a timeout, a refused
    -- connection, a DNS failure. That is different from a 500, and telling a
    -- customer which one it was is most of the support conversation.
    response_status int,
    error       text        NOT NULL DEFAULT '',
    -- How long the attempt took. §7.6 asks for it, and it is the number that
    -- separates "they rejected us" from "they never answered" when both show
    -- up as a failure.
    duration_ms int         NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    created_at  timestamptz NOT NULL DEFAULT now(),
    delivered_at timestamptz,
    -- delivered means a 2xx came back, and only then.
    CHECK ((status = 'delivered') = (delivered_at IS NOT NULL))
);

-- A retry must reuse its attempt number rather than race a second row for the
-- same try, so a crash between sending and recording cannot double-count.
CREATE UNIQUE INDEX deliveries_attempt_uniq
    ON webhook.deliveries (tenant_id, endpoint_id, event_id, attempt);

-- At most one success per endpoint per event, whatever the redelivery. NATS
-- is at-least-once, so the same event_id will arrive again -- on a consumer
-- restart, or after a nak -- and without this the endpoint would be sent a
-- duplicate and the table would claim both were the first.
--
-- Partial, so the failed attempts that preceded the success stay: the history
-- is the point (0012/0015 use the same shape for sweeps in flight).
CREATE UNIQUE INDEX deliveries_delivered_uniq
    ON webhook.deliveries (tenant_id, endpoint_id, event_id)
    WHERE status = 'delivered';

-- "What has been failing lately", which is the question a webhook page or an
-- alert is built on.
CREATE INDEX deliveries_recent_idx
    ON webhook.deliveries (tenant_id, endpoint_id, created_at DESC);

-- The work list.
--
-- deliveries records what happened; this records what is still owed, and the
-- two have opposite natures -- one is append-only history, the other is
-- mutable state -- so they are separate tables rather than one table doing
-- both badly.
--
-- It exists because §7.6's retry schedule (1m, 5m, 30m, 2h, 12h, 24h) cannot
-- live in JetStream. A nak delay is a single flat value with a 30s AckWait
-- and a MaxDeliver of 10, so holding the message until the next attempt is
-- not expressible past the first minute. The dispatcher acks as soon as it
-- has enqueued, and owns the timeline from here -- which means it has to keep
-- the bytes, because after the ack JetStream will not hand them over again.
CREATE TABLE webhook.queue (
    tenant_id   text        NOT NULL DEFAULT 'default',
    endpoint_id uuid        NOT NULL REFERENCES webhook.endpoints (id),
    event_id    text        NOT NULL,
    event_type  text        NOT NULL,
    -- bytea, not jsonb: what is signed and what is sent must be the same
    -- bytes, and jsonb normalises key order and whitespace. A body that came
    -- back from jsonb would no longer match the signature computed over it.
    body        bytea       NOT NULL,
    attempts    int         NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    created_at  timestamptz NOT NULL DEFAULT now(),
    -- One row per endpoint per event, so the enqueue is idempotent under
    -- JetStream's at-least-once redelivery: ON CONFLICT DO NOTHING and the
    -- second copy of an event is a no-op instead of a second POST.
    PRIMARY KEY (tenant_id, endpoint_id, event_id)
);

-- The dispatcher's only read: what is due now.
CREATE INDEX queue_due_idx ON webhook.queue (next_attempt_at);

-- Privileges (docs/plan-v1.0.md §14).
--
-- The worker delivers, so it reads endpoints and writes its own attempts. It
-- never edits an endpoint: a dispatcher that could disable the endpoint it is
-- failing to reach would be deciding a customer's integration is over.
GRANT SELECT ON webhook.endpoints TO ex_worker, ex_admin, ex_all;
GRANT INSERT ON webhook.deliveries TO ex_worker, ex_all;
GRANT SELECT ON webhook.deliveries TO ex_worker, ex_admin, ex_all;

-- Admin owns the endpoints themselves (§5.2 gives it `webhook` (設定)).
GRANT INSERT, UPDATE ON webhook.endpoints TO ex_admin, ex_all;

-- Nobody gets UPDATE or DELETE on deliveries. An attempt is what happened,
-- and a replay is a new attempt with a new row -- not an edit of the one that
-- failed. Same reasoning as admin.reconciliation_reports in 0013.

-- The queue is the exception, and only because it is the opposite kind of
-- table: it is the worker's own scratch space for work in progress, so the
-- worker owns every operation on it. A row leaves by being delivered or by
-- exhausting the schedule, and in both cases what survives is the delivery
-- rows -- deleting the queue entry loses nothing that mattered.
GRANT SELECT, INSERT, UPDATE, DELETE ON webhook.queue TO ex_worker, ex_all;
GRANT SELECT ON webhook.queue TO ex_admin;
