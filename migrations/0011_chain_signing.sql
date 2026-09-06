-- +goose Up
-- Signing, nonces and broadcast (docs/plan-v1.0.md §6.4.2, §6.6). 0010 took a
-- withdrawal as far as funds_locked without touching a key; this migration is
-- what the signer and the broadcaster need to take it the rest of the way.

-- The hot wallet's nonce, owned by the chain role's NonceManager (§6.4.2).
-- One row per chain. next_nonce is the next nonce to hand out, and it only
-- ever moves forward: a nonce that was allocated and not used is filled with a
-- self-transfer rather than reused, because two transactions with the same
-- nonce are a race the chain decides, not us.
CREATE TABLE chain.hot_wallets (
    tenant_id  text        NOT NULL DEFAULT 'default',
    chain_id   bigint      NOT NULL,
    address    text        NOT NULL CHECK (address ~ '^0x[0-9a-f]{40}$'),
    next_nonce bigint      NOT NULL DEFAULT 0 CHECK (next_nonce >= 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, chain_id)
);

-- Every signature the signer role ever produces (§6.6). The UNIQUE key is the
-- point: it is what makes "sign this withdrawal" unrepeatable, so a retry, a
-- replayed NATS message or a second chain instance cannot get two valid
-- transactions for one withdrawal.
--
-- A replacement transaction is a second signature for the same withdrawal, so
-- the key carries the attempt: (withdrawal, 0) is the original and
-- (withdrawal, 1) the first replacement. The plan writes the key as
-- (kind, ref_id); without the attempt a bump could never be signed at all.
CREATE TABLE chain.signing_log (
    id         bigserial   PRIMARY KEY,
    tenant_id  text        NOT NULL DEFAULT 'default',
    kind       text        NOT NULL CHECK (kind IN ('withdrawal', 'sweep', 'gas_fund', 'nonce_fill')),
    ref_id     text        NOT NULL,
    attempt    integer     NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    chain_id   bigint      NOT NULL,
    from_address text      NOT NULL CHECK (from_address ~ '^0x[0-9a-f]{40}$'),
    to_address   text      NOT NULL CHECK (to_address ~ '^0x[0-9a-f]{40}$'),
    nonce      bigint      NOT NULL CHECK (nonce >= 0),
    tx_hash    text        NOT NULL CHECK (tx_hash ~ '^0x[0-9a-f]{64}$'),
    -- The signed transaction itself. Kept so that asking to sign the same
    -- intent twice returns the same bytes instead of failing: a caller that
    -- crashed between signing and recording must be able to recover the
    -- transaction it already caused to exist, and re-deriving it is not an
    -- option once the fee market has moved. These bytes are public -- they are
    -- broadcast to the world -- so storing them reveals nothing.
    raw_tx     bytea       NOT NULL,
    signed_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, kind, ref_id, attempt)
);
CREATE INDEX signing_log_tx_idx ON chain.signing_log (tenant_id, chain_id, tx_hash);

-- Nonces that were allocated but whose transaction never reached the chain,
-- filled with a 0-value self-transfer (§6.4.2). Without this a single failed
-- broadcast leaves a hole and every later withdrawal sits unmined behind it.
CREATE TABLE chain.nonce_fills (
    id         bigserial   PRIMARY KEY,
    tenant_id  text        NOT NULL DEFAULT 'default',
    chain_id   bigint      NOT NULL,
    nonce      bigint      NOT NULL CHECK (nonce >= 0),
    tx_hash    text        NOT NULL CHECK (tx_hash ~ '^0x[0-9a-f]{64}$'),
    -- why the hole existed, so an operator reading this table later can tell a
    -- recycled withdrawal from a startup gap
    reason     text        NOT NULL CHECK (reason IN ('broadcast_failed', 'startup_gap', 'cancel_nonce')),
    status     text        NOT NULL DEFAULT 'broadcast' CHECK (status IN ('broadcast', 'confirmed', 'failed')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, chain_id, nonce)
);

-- What a withdrawal grows once it is signed. All nullable: a withdrawal that
-- never gets past funds_locked has none of them, and 0010's CHECK already
-- forbids the states that would need them without a hold.
ALTER TABLE chain.withdrawals
    ADD COLUMN nonce        bigint  CHECK (nonce >= 0),
    -- the signed transaction itself, so a restart between signing and
    -- broadcasting rebroadcasts the *same* bytes rather than signing again
    ADD COLUMN raw_tx       bytea,
    ADD COLUMN tx_hash      text    CHECK (tx_hash ~ '^0x[0-9a-f]{64}$'),
    ADD COLUMN broadcast_at timestamptz,
    -- how many times it has been re-sent with a higher fee (§6.4.2
    -- MAX_REPLACEMENTS); also the attempt number in chain.signing_log
    ADD COLUMN replacements integer NOT NULL DEFAULT 0 CHECK (replacements >= 0),
    -- the block it was mined in, once a receipt says so
    ADD COLUMN block_number bigint  CHECK (block_number >= 0),
    -- gas actually paid, in the chain's native asset
    ADD COLUMN gas_cost     numeric(36,18) CHECK (gas_cost >= 0),
    -- Set when an operator cancels a stuck withdrawal with a same-nonce
    -- self-transfer (§6.4.2 resolve(cancel_nonce)). The refund waits for
    -- *this* hash to confirm, not the original: until the replacement is
    -- mined the original can still win the race, and refunding early would
    -- credit a user for money that then leaves anyway.
    ADD COLUMN cancel_tx_hash text CHECK (cancel_tx_hash ~ '^0x[0-9a-f]{64}$'),
    -- What an operator asked for on a withdrawal the machine could not finish
    -- (§6.4.2 resolve). The admin role writes these four and nothing else: it
    -- has no node, no key and no grant on the transaction columns, so it can
    -- only ask. The chain role reads the request, applies it and clears the
    -- action -- which is the same separation the review columns already have,
    -- and it means an operator's decision survives a chain-role restart
    -- instead of being lost inside one HTTP request.
    ADD COLUMN resolve_action text
        CHECK (resolve_action IN ('bump', 'cancel_nonce', 'refund', 'retry')),
    ADD COLUMN resolve_note text,
    ADD COLUMN resolve_requested_by text,
    ADD COLUMN resolve_requested_at timestamptz,
    -- Why the last request could not be applied, kept after the action is
    -- cleared so the operator who asked can see what happened.
    ADD COLUMN resolve_error text;

-- A request must say who asked and why. One-directional on purpose (the
-- lesson of 0009): clearing resolve_action leaves the note and the requester
-- in place as the record of what was asked, and must not trip the CHECK.
ALTER TABLE chain.withdrawals
    ADD CONSTRAINT withdrawals_resolve_is_attributed CHECK (
        resolve_action IS NULL
        OR (resolve_note IS NOT NULL AND resolve_requested_by IS NOT NULL
            AND resolve_requested_at IS NOT NULL));

-- Signed means signed: the three things the signer produced must be there
-- together, and stay there afterwards. This is the schema half of "nonce, raw
-- signed tx and state land in one transaction" (§6.4.2).
ALTER TABLE chain.withdrawals
    ADD CONSTRAINT withdrawals_signed_has_tx CHECK (
        status NOT IN ('signed', 'broadcast', 'confirmed')
        OR (nonce IS NOT NULL AND raw_tx IS NOT NULL AND tx_hash IS NOT NULL));

-- The tracker's queue: what has been sent and is waiting for a receipt.
CREATE INDEX withdrawals_broadcast_idx ON chain.withdrawals (tenant_id, broadcast_at)
    WHERE status = 'broadcast';
-- and what has been signed but not yet sent, which a restart must replay
CREATE INDEX withdrawals_signed_idx ON chain.withdrawals (tenant_id, created_at)
    WHERE status = 'signed';
-- The resolve queue: what an operator has asked for and the chain has not yet
-- applied. Oldest first, so a request cannot be starved by newer ones.
CREATE INDEX withdrawals_resolve_idx ON chain.withdrawals (tenant_id, resolve_requested_at)
    WHERE resolve_action IS NOT NULL;
-- NonceManager's startup scan walks the hot wallet's allocated nonces
CREATE INDEX withdrawals_nonce_idx ON chain.withdrawals (tenant_id, chain_id, nonce)
    WHERE nonce IS NOT NULL;

-- Privileges (docs/plan-v1.0.md §14).
GRANT SELECT ON chain.hot_wallets, chain.signing_log, chain.nonce_fills
  TO ex_api, ex_engine, ex_chain, ex_signer, ex_stream, ex_admin, ex_worker, ex_all;

-- The nonce belongs to the chain role: it allocates, fills and advances it.
GRANT INSERT, UPDATE ON chain.hot_wallets TO ex_chain, ex_all;
GRANT INSERT, UPDATE ON chain.nonce_fills TO ex_chain, ex_all;

-- Only the signer writes the signing log, and only ever appends. No role may
-- UPDATE or DELETE it -- a signature that happened cannot be unhappened, and
-- the UNIQUE key is only a defence if nobody can clear the row it collides
-- with.
GRANT INSERT ON chain.signing_log TO ex_signer, ex_all;
GRANT USAGE ON ALL SEQUENCES IN SCHEMA chain TO ex_chain, ex_signer, ex_all;

-- The chain role writes the transaction columns and clears the resolve
-- request it has applied; it still cannot review, and the api still cannot
-- write anything (0010).
GRANT UPDATE (nonce, raw_tx, tx_hash, broadcast_at, replacements, block_number,
              gas_cost, cancel_tx_hash, resolve_action, resolve_error)
  ON chain.withdrawals TO ex_chain;

-- Admin asks for a resolution and nothing more. It is deliberately not given
-- the transaction columns: a role that could write tx_hash could make a
-- withdrawal look sent without anything having been signed, and the whole
-- point of §6.4.2 is that authorising and acting are different roles.
GRANT UPDATE (resolve_action, resolve_note, resolve_requested_by, resolve_requested_at)
  ON chain.withdrawals TO ex_admin;

-- The signer must read the withdrawal it is asked to sign, to check the
-- amount, asset and destination against the request (§6.6). 0010 already
-- granted it SELECT on chain.withdrawals; it gets no write.

-- Broadcasting moves hold -> pending_withdrawal, and confirmation moves
-- pending_withdrawal -> custody_hot with a gas entry. Those are ledger
-- postings the chain role already has the grants for (0003, and the
-- next_seq column 0009 added).
