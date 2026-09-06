-- +goose Up
-- Sweeping (docs/plan-v1.0.md §6.4.3, §6.1.4 f). Deposits land on one address
-- per account; withdrawals go out of the hot wallet. Nothing has moved money
-- between the two until now, which is why custody:hot has been going negative
-- since 4b-2. This is the transfer that closes that gap.
--
-- Sweeping never touches a user balance. It moves value between two house
-- accounts -- custody:deposit_addresses and custody:hot -- and books the gas.
-- A user whose deposit is swept sees nothing change, which is the invariant
-- the tests assert.

CREATE TABLE chain.sweeps (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     text        NOT NULL DEFAULT 'default',
    chain_id      bigint      NOT NULL,
    -- the address being emptied, and the pool row it came from
    address_id    uuid        NOT NULL REFERENCES chain.deposit_addresses (id),
    from_address  text        NOT NULL CHECK (from_address ~ '^0x[0-9a-f]{40}$'),
    asset         text        NOT NULL,
    -- What actually arrives at the hot wallet. For a native sweep this is the
    -- balance minus the gas the transaction may cost; for a token it is the
    -- whole token amount, because the gas is paid in ETH.
    amount        numeric(36,18) NOT NULL CHECK (amount > 0),
    status        text        NOT NULL DEFAULT 'requested'
                  CHECK (status IN ('requested', 'gas_funded', 'broadcast', 'confirmed', 'failed')),
    -- one-directional, the lesson of 0009: a failure must carry its reason,
    -- but clearing the status must not have to clear the reason too
    failure_reason text CHECK (failure_reason IN ('gas_funding', 'broadcast', 'on_chain', 'balance_changed')),
    CONSTRAINT sweeps_failure_has_reason CHECK (status <> 'failed' OR failure_reason IS NOT NULL),

    -- The gas-funding leg, ERC-20 only (§6.4.3). The hot wallet sends the
    -- deposit address exactly enough ETH to pay for the transfer, because an
    -- address that has only ever received tokens cannot pay for anything.
    gas_funding_tx_hash text CHECK (gas_funding_tx_hash ~ '^0x[0-9a-f]{64}$'),
    gas_funding_nonce   bigint CHECK (gas_funding_nonce >= 0),
    gas_funding_amount  numeric(36,18) CHECK (gas_funding_amount > 0),
    gas_funding_raw_tx  bytea,
    -- what the funding transaction itself cost, in the native coin
    gas_funding_cost    numeric(36,18) CHECK (gas_funding_cost >= 0),

    -- The sweep transaction, signed by the deposit address's own key.
    nonce        bigint CHECK (nonce >= 0),
    raw_tx       bytea,
    tx_hash      text CHECK (tx_hash ~ '^0x[0-9a-f]{64}$'),
    broadcast_at timestamptz,
    block_number bigint CHECK (block_number >= 0),
    gas_cost     numeric(36,18) CHECK (gas_cost >= 0),

    -- Same shape as chain.withdrawals: signed means signed.
    CONSTRAINT sweeps_broadcast_has_tx CHECK (
        status NOT IN ('broadcast', 'confirmed')
        OR (nonce IS NOT NULL AND raw_tx IS NOT NULL AND tx_hash IS NOT NULL)),
    -- gas_funded is only reachable with a funding transaction to point at
    CONSTRAINT sweeps_gas_funded_has_tx CHECK (
        status <> 'gas_funded' OR gas_funding_tx_hash IS NOT NULL),

    correlation_id text,
    version      integer     NOT NULL DEFAULT 1,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- One sweep at a time per address and asset. Without this a slow tick would
-- start a second sweep of an address the first has not emptied yet, and the
-- two would race for the same nonce.
CREATE UNIQUE INDEX sweeps_in_flight_uniq ON chain.sweeps (tenant_id, chain_id, address_id, asset)
    WHERE status IN ('requested', 'gas_funded', 'broadcast');

-- The worker's queues.
CREATE INDEX sweeps_open_idx ON chain.sweeps (tenant_id, created_at)
    WHERE status IN ('requested', 'gas_funded', 'broadcast');
-- and the per-address history the sweepable-amount query walks
CREATE INDEX sweeps_address_idx ON chain.sweeps (tenant_id, address_id, asset, status);

-- Privileges (docs/plan-v1.0.md §14).
GRANT SELECT ON chain.sweeps
  TO ex_api, ex_engine, ex_chain, ex_signer, ex_stream, ex_admin, ex_worker, ex_all;

-- The chain role owns the whole machine here. Unlike a withdrawal there is no
-- authorising step and no user to protect: sweeping moves the exchange's own
-- custody between two of its own accounts, so there is nothing for a second
-- role to approve and no split to enforce.
GRANT INSERT, UPDATE ON chain.sweeps TO ex_chain, ex_all;

-- The signer reads the sweep it is asked to sign, to check the destination and
-- the amount against the row (§6.6). It gets no write, exactly as with
-- withdrawals: holding the key must not carry the ability to declare the money
-- moved.
