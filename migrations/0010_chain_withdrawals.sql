-- +goose Up
-- Withdrawals (docs/plan-v1.0.md §6.4.2). This migration carries the whole
-- state machine, but only the states up to funds_locked have code behind them
-- in this change: signing, broadcasting and tracking arrive with the signer
-- role. Listing the later states now keeps the CHECK stable across that split
-- rather than rewriting it in the next migration.
--
-- The two iron rules of §6.4.2 shape this table: the ledger is locked before
-- anything is signed, and every state is committed before the next step runs,
-- so a restart resumes from the row rather than from memory.

CREATE TABLE chain.withdrawals (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     text        NOT NULL DEFAULT 'default',
    account_id    uuid        NOT NULL REFERENCES ledger.accounts (id),
    asset         text        NOT NULL,
    amount        numeric(36,18) NOT NULL CHECK (amount > 0),
    -- lowercase hex; the API checksums what it accepts and stores the
    -- normalised form, so the same address is one string here
    to_address    text        NOT NULL CHECK (to_address ~ '^0x[0-9a-f]{40}$'),
    chain_id      bigint      NOT NULL,
    -- Idempotency-Key, scoped to the account that sent it (§6.4.2)
    idempotency_key text      NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    -- What the key was a key *for*: a replay with the same key but a different
    -- asset, amount or destination is a client bug, and must be told apart
    -- from an honest retry rather than silently returning someone else's row.
    request_hash  text        NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    status        text        NOT NULL CHECK (status IN (
                      'requested', 'policy_check', 'auto_approved', 'pending_review',
                      'approved', 'rejected', 'funds_locked',
                      -- reached once the signer role exists
                      'signed', 'broadcast', 'confirmed',
                      'failed')),
    -- Which failure: insufficient_balance and policy arrive with this change,
    -- the rest with the signer.
    failure_reason text       CHECK (failure_reason IN
                      ('insufficient_balance', 'policy', 'broadcast', 'on_chain', 'replaced')),
    -- who approved or rejected it in the admin queue; a policy rejection has
    -- no reviewer, so this is not tied to the rejected status
    reviewed_by   uuid        REFERENCES auth.users (id),
    reviewed_at   timestamptz,
    review_note   text,
    -- the ledger entry that locked the funds, so the hold can be traced
    -- without guessing at the idempotency key
    hold_entry_id bigint      REFERENCES ledger.journal_entries (id),
    correlation_id text,
    version       integer     NOT NULL DEFAULT 1,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    -- A failure must say why. One-directional on purpose: admin resolve(retry)
    -- moves a failed withdrawal back to funds_locked (§6.4.2), and the reason
    -- it failed the first time is exactly what nobody should lose on the way.
    -- The biconditional version of this CHECK is what rejected the
    -- orphaned -> dropped transition in 0009; the same shape, avoided here.
    CHECK (status <> 'failed' OR failure_reason IS NOT NULL),
    -- These two are written in the same statement and never cleared.
    CHECK ((reviewed_by IS NULL) = (reviewed_at IS NULL)),
    -- Funds are locked before signing, and stay locked after: every state from
    -- funds_locked onwards must point at the entry that locked them.
    CHECK (status NOT IN ('funds_locked', 'signed', 'broadcast', 'confirmed')
           OR hold_entry_id IS NOT NULL)
);

-- One withdrawal per (account, Idempotency-Key). Scoped to the account rather
-- than the tenant: one client's key must not collide with another's.
CREATE UNIQUE INDEX withdrawals_idempotency_uniq
    ON chain.withdrawals (tenant_id, account_id, idempotency_key);
-- the chain worker's queue: what still needs a decision or a lock
CREATE INDEX withdrawals_pending_idx ON chain.withdrawals (tenant_id, created_at)
    WHERE status IN ('requested', 'auto_approved', 'approved');
-- the admin review queue
CREATE INDEX withdrawals_review_idx ON chain.withdrawals (tenant_id, created_at)
    WHERE status = 'pending_review';
-- the user-facing listing, and the daily-limit sum the policy needs
CREATE INDEX withdrawals_account_idx ON chain.withdrawals (tenant_id, account_id, created_at DESC);

-- Privileges (docs/plan-v1.0.md §14). Three roles touch this table and each
-- gets only its own columns; the split is what stops a compromised api from
-- approving a withdrawal it just created.
GRANT SELECT ON chain.withdrawals
  TO ex_api, ex_engine, ex_chain, ex_signer, ex_stream, ex_admin, ex_worker, ex_all;

-- api creates the request and nothing else. It has no UPDATE at all, so it
-- cannot move a withdrawal towards being signed. (SELECT above is also what
-- makes its INSERT ... RETURNING work: the audit_events failure in 0004 was
-- exactly this, a writer without SELECT.)
GRANT INSERT ON chain.withdrawals TO ex_api, ex_all;

-- The chain worker drives the state machine but never reviews: it may not
-- write reviewed_by, reviewed_at or review_note, so an approval can only come
-- from a person through the admin role.
GRANT UPDATE (status, failure_reason, hold_entry_id, version, updated_at)
  ON chain.withdrawals TO ex_chain;

-- Admin approves and rejects from the review queue, and records who did it.
GRANT UPDATE (status, failure_reason, reviewed_by, reviewed_at, review_note, version, updated_at)
  ON chain.withdrawals TO ex_admin;

-- The all-in-one dev role keeps the union, as everywhere else.
GRANT UPDATE ON chain.withdrawals TO ex_all;

-- No DELETE for anyone: a withdrawal is a money record. The one DELETE in this
-- schema remains chain.blocks, the reorg ring (0009).

-- Recording a withdrawal emits withdrawal.requested in the same transaction as
-- the INSERT -- that is what the outbox is for -- and an account-scoped event
-- carries account_seq, which NextAccountSeq produces with an
-- UPDATE ... RETURNING. 0003 gave UPDATE on ledger.accounts to ex_engine,
-- ex_admin and ex_all only, so the api would fail with 42501 in a split
-- deployment while every ex_all test passed. Same shape as the grant 0009 had
-- to add for ex_chain, and column-scoped for the same reason: the api may
-- advance an account's event sequence and change nothing else about it.
GRANT UPDATE (next_seq) ON ledger.accounts TO ex_api;
