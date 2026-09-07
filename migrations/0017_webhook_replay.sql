-- +goose Up
-- What replay needs, which 0016 did not provide (docs/plan-v1.0.md §7.6:
-- "後台可查、可手動 replay").
--
-- Two things were missing, and both only became visible when the admin side
-- was designed:
--
--   1. The body did not survive delivery. queue.body was deleted with the
--      queue row the moment a delivery reached delivered or dead, so by the
--      time an operator wants to replay one there is nothing left to send.
--      deliveries records what happened, not what was sent.
--   2. ex_admin has SELECT on webhook.queue and nothing else, so the admin
--      role could not enqueue a replay even if it had the bytes.
--
-- Storing the body per attempt or per endpoint would have fixed (1) by
-- duplicating it -- three subscribers meant three copies of the same JSON,
-- and a replay would have had to pick one. An event is one thing that
-- happened, so it is stored once.

CREATE TABLE webhook.events (
    tenant_id  text        NOT NULL DEFAULT 'default',
    event_id   text        NOT NULL,
    event_type text        NOT NULL,
    -- The exact bytes published, kept as bytea for the reason queue.body was:
    -- what is signed and what is sent must be identical, and jsonb would
    -- normalise key order and whitespace out from under the signature.
    body       bytea       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, event_id)
);

-- Nothing prunes this yet. §5.2 lists outbox cleanup as a worker job and this
-- belongs with it; the index is here so that job can find old rows by age
-- without a sequential scan, and so this comment is not the only trace of the
-- decision. Until then the table grows with the event stream.
CREATE INDEX events_age_idx ON webhook.events (created_at);

-- The queue now points at the event instead of carrying a copy of it. Dropping
-- these two columns is safe in a way it will not be later: 0016 shipped an
-- hour ago and no deployment has run it with traffic.
ALTER TABLE webhook.queue DROP COLUMN body;
ALTER TABLE webhook.queue DROP COLUMN event_type;
ALTER TABLE webhook.queue
    ADD CONSTRAINT queue_event_fk FOREIGN KEY (tenant_id, event_id)
    REFERENCES webhook.events (tenant_id, event_id);

-- Privileges (docs/plan-v1.0.md §14).
--
-- The worker writes events as it enqueues them and reads them back to deliver.
GRANT SELECT, INSERT ON webhook.events TO ex_worker, ex_all;
-- Admin reads a body to show what was sent, and inserts a queue row to replay
-- it. It still never writes webhook.events: a replay re-sends what happened,
-- and an admin able to edit the payload could send a customer something the
-- exchange never produced.
GRANT SELECT ON webhook.events TO ex_admin;
GRANT INSERT ON webhook.queue TO ex_admin;
