-- +goose Up
-- What replay needs, which 0016 did not provide (docs/plan-v1.0.md §7.6:
-- "後台可查、可手動 replay").
--
-- Four things were missing, and every one of them only became visible when the
-- admin side was designed -- which is the lesson of this file. 0016 built the
-- tables before anything used them, and its two unique indexes were both
-- written for a redelivery scenario that had never been walked from POST to
-- settle. Walking it is what found (3) and (4).
--
--   1. The body did not survive delivery. queue.body was deleted with the
--      queue row the moment a delivery reached delivered or dead, so by the
--      time an operator wants to replay one there is nothing left to send.
--      deliveries records what happened, not what was sent.
--   2. ex_admin has SELECT on webhook.queue and nothing else, so the admin
--      role could not enqueue a replay even if it had the bytes.
--   3. A replayed queue row starts at attempts = 0, so its delivery rows
--      collide with the original run's on deliveries_attempt_uniq and are
--      swallowed by an untargeted ON CONFLICT DO NOTHING. The POST happens,
--      the customer receives it, and the table that exists to record it says
--      nothing. Replaying a dead delivery is the worst case: it re-runs the
--      whole schedule from 0 and collides at every step, so it produces zero
--      rows.
--   4. deliveries_delivered_uniq made a successful replay unrecordable, and
--      never did the job its comment claimed. See below.
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

-- A run: one pass through the §7.6 schedule for one (endpoint, event).
--
-- The first delivery is one run; each replay is another. Without it, `attempt`
-- has to mean two things at once -- which step of the retry schedule this is,
-- and which row in the history -- and (3) above is what happens when one
-- column tries. With it, `attempt` keeps meaning "step in the schedule", so
-- the backoff index, the give-up test and every log line stay exactly as they
-- were, and a replay simply starts a new run at step 0.
--
-- Existing rows get the nil UUID rather than fresh ones, so an in-flight
-- retry chain and the attempts it has already recorded land in the same run.
ALTER TABLE webhook.deliveries
    ADD COLUMN run_id uuid NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000';
ALTER TABLE webhook.queue
    ADD COLUMN run_id uuid NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000';
-- Enqueue does not name it; a queue row is a run.
ALTER TABLE webhook.queue ALTER COLUMN run_id SET DEFAULT gen_random_uuid();
-- The writer always knows which run it is settling, so nothing should default.
ALTER TABLE webhook.deliveries ALTER COLUMN run_id DROP DEFAULT;

-- Still the index that makes a double-claim idempotent: claim() commits before
-- any HTTP happens and ClaimDue does not advance next_attempt_at, so two ticks
-- can hold the same row and both POST. What is new is `run_id`, which stops a
-- replay's step 0 from colliding with the original run's step 0.
DROP INDEX webhook.deliveries_attempt_uniq;
CREATE UNIQUE INDEX deliveries_attempt_uniq
    ON webhook.deliveries (tenant_id, endpoint_id, event_id, run_id, attempt);

-- 0016 created deliveries_delivered_uniq to stop at-least-once redelivery from
-- sending a customer a duplicate. It could never have done that. The POST is
-- finished before settle() opens the transaction that touches this index, so
-- the index sits downstream of the effect it claimed to prevent: it cannot
-- stop a second POST, only destroy the record of one. And that is what it did
-- -- after a success the queue row is gone, so a late redelivery re-enqueues,
-- the worker POSTs again, and the row recording it was silently dropped.
--
-- So "delivered twice" is not a state being made legal here. It was always
-- reachable; this is the table starting to admit it. The per-run attempt index
-- above is the real guard, and one success per run follows from it.
DROP INDEX webhook.deliveries_delivered_uniq;

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
-- Disabling an endpoint has to drop what is still queued for it. ClaimDue
-- filters on status = 'active', so rows left behind are unreachable: nothing
-- delivers them, nothing deletes them (every delete path is inside settle),
-- they block any later replay of the same (endpoint, event) on the primary
-- key, they pin their webhook.events row against the pruner through
-- queue_event_fk, and re-enabling months later fires a batch of stale events.
-- The queue holds what is owed, not history (0016 says so), and after a
-- deliberate disable nothing is owed.
GRANT DELETE ON webhook.queue TO ex_admin;
