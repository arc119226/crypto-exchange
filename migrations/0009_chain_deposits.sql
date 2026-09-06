-- +goose Up
-- Deposit detection (docs/plan-v1.0.md §6.4.1). The chain role polls blocks,
-- records what it saw, and credits the ledger once a deposit has enough
-- confirmations. Nothing here is the source of truth for money — the ledger
-- is; these tables are the scanner's own memory of the chain.

-- The idempotency key of a deposit is (chain_id, tx_hash, log_index). Native
-- transfers have no log, so they use log_index = -1, which no Transfer log can
-- collide with. The plan writes the key without tenant_id; it is included here
-- because 0001 requires every business table to carry tenant_id and include it
-- in its unique keys.
CREATE TABLE chain.deposits (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     text        NOT NULL DEFAULT 'default',
    chain_id      bigint      NOT NULL,
    tx_hash       text        NOT NULL CHECK (tx_hash ~ '^0x[0-9a-f]{64}$'),
    log_index     integer     NOT NULL CHECK (log_index >= -1),
    -- the controlled address the funds arrived at, and the account it belongs to
    address       text        NOT NULL CHECK (address ~ '^0x[0-9a-f]{40}$'),
    account_id    uuid        NOT NULL REFERENCES ledger.accounts (id),
    asset         text        NOT NULL,
    amount        numeric(36,18) NOT NULL CHECK (amount > 0),
    block_number  bigint      NOT NULL CHECK (block_number >= 0),
    block_hash    text        NOT NULL CHECK (block_hash ~ '^0x[0-9a-f]{64}$'),
    confirmations integer     NOT NULL DEFAULT 0 CHECK (confirmations >= 0),
    -- §6.4.1: only credited and reversed touch the ledger
    status        text        NOT NULL CHECK (status IN
                      ('detected', 'confirming', 'credited', 'orphaned', 'dropped', 'reversed')),
    -- the block the deposit was orphaned at, so ORPHAN_EXPIRY_BLOCKS can be
    -- measured without a second clock; cleared when it reappears
    orphaned_at_block bigint,
    credited_at   timestamptz,
    correlation_id text,
    version       integer     NOT NULL DEFAULT 1,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CHECK ((status = 'credited') = (credited_at IS NOT NULL)),
    -- An orphan that is later dropped keeps the height it was orphaned at:
    -- that is how the expiry was measured, and losing it would make the row
    -- unexplainable. So the requirement is one-directional — these two states
    -- must know when — rather than a biconditional that would reject the
    -- orphaned -> dropped transition itself.
    CHECK (status NOT IN ('orphaned', 'dropped') OR orphaned_at_block IS NOT NULL)
);
CREATE UNIQUE INDEX deposits_txid_uniq ON chain.deposits (tenant_id, chain_id, tx_hash, log_index);
-- the scanner's two hot queries: what is still maturing, and what a reorg of
-- these blocks would affect
CREATE INDEX deposits_pending_idx ON chain.deposits (tenant_id, chain_id, block_number)
    WHERE status IN ('detected', 'confirming');
CREATE INDEX deposits_orphaned_idx ON chain.deposits (tenant_id, chain_id, orphaned_at_block)
    WHERE status = 'orphaned';
-- the user- and admin-facing listings
CREATE INDEX deposits_account_idx ON chain.deposits (tenant_id, account_id, created_at DESC);

-- One cursor per chain. last_block_hash is what the next batch's parent_hash
-- must match; a mismatch is the reorg signal.
CREATE TABLE chain.scan_cursors (
    tenant_id          text        NOT NULL DEFAULT 'default',
    chain_id           bigint      NOT NULL,
    last_scanned_block bigint      NOT NULL CHECK (last_scanned_block >= 0),
    last_block_hash    text        NOT NULL CHECK (last_block_hash ~ '^0x[0-9a-f]{64}$'),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, chain_id)
);

-- A ring of the most recent blocks (§6.4.1 keeps 128). Walking back through
-- it is how a reorg's common ancestor is found, so it only has to be as deep
-- as the deepest reorg worth surviving automatically.
CREATE TABLE chain.blocks (
    tenant_id   text        NOT NULL DEFAULT 'default',
    chain_id    bigint      NOT NULL,
    number      bigint      NOT NULL CHECK (number >= 0),
    hash        text        NOT NULL CHECK (hash ~ '^0x[0-9a-f]{64}$'),
    parent_hash text        NOT NULL CHECK (parent_hash ~ '^0x[0-9a-f]{64}$'),
    seen_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, chain_id, number)
);

-- Guards against pointing a database with a live cursor at a different chain:
-- anvil's state volume can be wiped while Postgres remembers block 4,000, and
-- resuming there would silently skip every deposit in the new chain.
CREATE TABLE chain.chain_state (
    tenant_id    text        NOT NULL DEFAULT 'default',
    chain_id     bigint      NOT NULL,
    genesis_hash text        NOT NULL CHECK (genesis_hash ~ '^0x[0-9a-f]{64}$'),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, chain_id)
);

-- Privileges (docs/plan-v1.0.md §14). The chain role owns these tables; the
-- admin role reads them for the operator API; everyone else only reads.
GRANT SELECT ON chain.deposits, chain.scan_cursors, chain.blocks, chain.chain_state
  TO ex_api, ex_engine, ex_chain, ex_signer, ex_stream, ex_admin, ex_worker, ex_all;
GRANT INSERT, UPDATE ON chain.deposits, chain.scan_cursors, chain.blocks, chain.chain_state
  TO ex_chain, ex_all;

-- The one DELETE granted anywhere in this schema. chain.blocks is a bounded
-- ring of chain tips, not a business record: it exists only to find a reorg's
-- common ancestor, and without pruning it grows by a row every block forever.
-- Deposits, ledger rows and audit rows remain undeletable by every role.
GRANT DELETE ON chain.blocks TO ex_chain, ex_all;

-- Crediting a deposit stamps account_seq on the events it emits, and
-- NextAccountSeq is an UPDATE ... RETURNING. 0003 granted UPDATE on
-- ledger.accounts to ex_engine, ex_admin and ex_all only, so the chain role
-- could open a spot account's sequence in tests (which connect as ex_all) and
-- fail with 42501 in a split deployment. The grant is column-scoped: the chain
-- role advances the sequence and touches nothing else about the account.
GRANT UPDATE (next_seq) ON ledger.accounts TO ex_chain;
