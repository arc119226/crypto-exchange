-- +goose Up
-- The first of the three reconciliations (docs/plan-v1.0.md §12 Phase 5:
-- "three-way: 帳本內部、帳本 vs 鏈上、在途"). 4c-2 built the second and third
-- in admin.reconciliation_reports / reconciliation_breaks; this is the ledger
-- checking itself.
--
-- The invariant is double entry: for every asset, debits equal credits across
-- all postings. Until now a violation was only a gauge
-- (ledger_trial_balance_diff), refreshed by the admin role every thirty
-- seconds, which is something a person has to be looking at. This makes it a
-- recorded break and an event, so it reaches whoever is on call.
--
-- A separate table from reconciliation_breaks, on purpose. That table is
-- chain-shaped: five term columns and a block height, all NOT NULL, and the
-- break_detected event lists them all as required -- a ledger break has none
-- of them. It is also computed by the chain role, whose reconciler only
-- reaches the point of recording anything once the node answers and the scan
-- cursor is in place; an internal check is worth the most exactly when the
-- node is down, so it must not depend on one.

CREATE TABLE admin.ledger_breaks (
    id          uuid           PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   text           NOT NULL DEFAULT 'default',
    asset       text           NOT NULL,
    debits      numeric(36,18) NOT NULL,
    credits     numeric(36,18) NOT NULL,
    -- debits − credits. A row exists only when this is non-zero, which is what
    -- makes it a break rather than a line.
    diff        numeric(36,18) NOT NULL CHECK (diff <> 0),
    detected_at timestamptz    NOT NULL DEFAULT now(),
    -- Set when the asset balances again. The row stays: what was found, and
    -- for how long, is the record.
    resolved_at timestamptz,
    CHECK (diff = debits - credits),
    CHECK (resolved_at IS NULL OR resolved_at >= detected_at)
);

-- One open break per asset. The watcher is edge-triggered (0013's reasoning):
-- an unchanged break stays quiet, a changed one is resolved and re-opened with
-- the new figures, and the event fires only on those edges.
CREATE UNIQUE INDEX ledger_breaks_open_uniq
    ON admin.ledger_breaks (tenant_id, asset) WHERE resolved_at IS NULL;

CREATE INDEX ledger_breaks_recent_idx ON admin.ledger_breaks (tenant_id, detected_at DESC);

-- Privileges (docs/plan-v1.0.md §14). The admin role runs the check -- it is
-- the role that already computes the trial balance for the gauge -- and
-- shows the result. Nobody deletes; resolving is a timestamp.
GRANT SELECT, INSERT ON admin.ledger_breaks TO ex_admin, ex_all;
GRANT UPDATE (resolved_at) ON admin.ledger_breaks TO ex_admin, ex_all;
