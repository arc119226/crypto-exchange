-- +goose Up
-- On-chain reconciliation (docs/plan-v1.0.md §6.4.4).
--
-- 4c-1 deliberately left something behind: a sweep is capped at what the
-- ledger was actually credited, so an on-chain balance that is legitimately
-- higher -- a contract-internal transfer the scanner cannot see -- stays on
-- chain rather than being swept into custody the exchange never received.
-- This is where that surplus becomes visible.
--
-- Per asset the identity is
--
--   custody_deposit_addresses + custody_hot   (what the ledger says we hold)
--   vs
--   Σ balance of every deposit address + the hot wallet, read at one block
--
-- with two correction terms for the two ways the two sides can be looking at
-- different moments: deposits the chain shows but the ledger has not credited
-- yet, and movements the ledger booked above the block the balances were read
-- at. Both are computable exactly, so the tolerance is zero.

CREATE TABLE admin.reconciliation_reports (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   text        NOT NULL DEFAULT 'default',
    chain_id    bigint      NOT NULL,
    started_at  timestamptz NOT NULL,
    finished_at timestamptz NOT NULL,
    -- true when every asset's diff is exactly zero
    balanced    boolean     NOT NULL,
    -- One object per asset, carrying every number that went into its verdict,
    -- including the assets that balanced. An operator opening this table wants
    -- to see the figures, not a row that says "nothing to see"; and a break
    -- can only be argued about against the terms it was derived from.
    --
    -- jsonb rather than a third table because nothing queries inside it: the
    -- assets that matter are lifted into reconciliation_breaks below.
    lines       jsonb       NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CHECK (finished_at >= started_at),
    CHECK (jsonb_typeof(lines) = 'array')
);

-- The only access pattern: the newest report for a chain.
CREATE INDEX reconciliation_reports_recent_idx
    ON admin.reconciliation_reports (tenant_id, chain_id, finished_at DESC);

CREATE TABLE admin.reconciliation_breaks (
    id           uuid   PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    text   NOT NULL DEFAULT 'default',
    report_id    uuid   NOT NULL REFERENCES admin.reconciliation_reports (id),
    chain_id     bigint NOT NULL,
    asset        text   NOT NULL,
    -- the height every balance in this line was read at
    block_height bigint NOT NULL CHECK (block_height >= 0),

    -- The terms, kept so the number can be argued with later. All of them can
    -- be negative: custody_hot is routinely negative between a withdrawal and
    -- the sweep that refills it, and the corrections are signed by direction.
    ledger_total    numeric(36,18) NOT NULL,
    chain_total     numeric(36,18) NOT NULL,
    uncredited      numeric(36,18) NOT NULL,
    above_frontier  numeric(36,18) NOT NULL,
    in_flight       numeric(36,18) NOT NULL,

    -- chain_total − uncredited − above_frontier + in_flight − ledger_total.
    -- A row exists only when this is non-zero, which is what makes it a break
    -- rather than a line.
    diff         numeric(36,18) NOT NULL CHECK (diff <> 0),
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- one row per asset per report: a report is one look at one moment
    UNIQUE (report_id, asset)
);

-- "What has been breaking, and for how long" -- the first question an operator
-- asks, and the one the Phase 5 page will be built on.
CREATE INDEX reconciliation_breaks_recent_idx
    ON admin.reconciliation_breaks (tenant_id, asset, created_at DESC);

-- Edge-triggering for alert.hot_wallet_low (§6.4.3). Without somewhere to
-- remember that the alert already went out, every tick below the threshold
-- would send another one, and an alert that repeats every few seconds is an
-- alert nobody reads. Cleared when the balance comes back above the line.
ALTER TABLE chain.hot_wallets ADD COLUMN low_alerted_at timestamptz;

-- A nonce gap is filled with a 0-value self-transfer (§6.4.2). That costs gas
-- out of the hot wallet, and until now nothing booked it: internal/chain/
-- hotwallet imports no ledger at all, so every fill made custody_hot claim
-- ether the chain no longer had. These two columns are what lets the fill be
-- followed to its receipt and the gas posted, the same way every other
-- transaction this system sends is.
ALTER TABLE chain.nonce_fills ADD COLUMN block_number bigint CHECK (block_number >= 0);
ALTER TABLE chain.nonce_fills ADD COLUMN gas_cost numeric(36,18) CHECK (gas_cost >= 0);

-- The gas-funding leg records what it cost but not where it landed, so
-- reconciliation could not tell whether the ledger entry it produced was
-- above or below the block the balances were read at. One column closes it.
ALTER TABLE chain.sweeps ADD COLUMN gas_funding_block bigint CHECK (gas_funding_block >= 0);

-- Privileges (docs/plan-v1.0.md §14).
--
-- A shorter list than chain.sweeps got, on purpose. Sweeps are read by the
-- signer to check what it is signing; a reconciliation report is read by the
-- role that displays it and written by the role that has the node. Nothing
-- else has a reason, and §14's rule is to grant only what a role needs.
GRANT SELECT ON admin.reconciliation_reports, admin.reconciliation_breaks
  TO ex_chain, ex_admin, ex_all;

-- The chain role writes them because it is the only role that dials the node
-- (docs/plan-v1.0.md §6.4.4 says worker; worker has neither an RPC endpoint
-- nor a dependency on the chain being up, and giving it one to match a word
-- would mean a second node connection for no gain). It reads them back for
-- the previous report, which is how a break event knows not to repeat itself.
--
-- Nobody gets UPDATE or DELETE: a report is what was true at one moment, and
-- a moment cannot be revised. A break is answered by a ledger adjustment that
-- makes the next report balance, not by editing the one that found it.
GRANT INSERT ON admin.reconciliation_reports, admin.reconciliation_breaks
  TO ex_chain, ex_all;

-- The four columns added above need no grants of their own: chain.hot_wallets,
-- chain.nonce_fills and chain.sweeps all carry table-level INSERT, UPDATE for
-- ex_chain and ex_all (0011, 0012), so a new column is already covered. Only
-- chain.withdrawals is column-scoped, and it is untouched here.
