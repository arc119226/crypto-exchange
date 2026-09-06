-- +goose Up
-- Deposit address pool (docs/plan-v1.0.md §6.4.1, §14; ADR-0007).
--
-- Addresses are pre-generated, never derived on demand: the signer role — the
-- only role that holds the HD seed — keeps at least ADDRESS_POOL_MIN free rows
-- here, and GET /v1/deposit-address just claims one. That is why the api role
-- gets UPDATE on two columns and no INSERT at all: it must be able to hand a
-- user an address without ever touching key material.
--
-- One address per account per chain; ETH and every ERC-20 on that chain share
-- it (§6.4.1). Addresses are stored lower-case so uniqueness and the scanner's
-- lookups cannot be defeated by case; the API renders the EIP-55 checksummed
-- form.

-- Derivation indices come from a sequence rather than max()+1 so two signers
-- can never mint the same m/44'/60'/0'/0/{i}. The signer draws nextval first
-- and passes the index in explicitly, because it has to derive the address
-- before it has a row to insert. A gap only means one index is never used.
CREATE SEQUENCE chain.deposit_address_index_seq AS bigint START WITH 0 MINVALUE 0;

CREATE TABLE chain.deposit_addresses (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id        text        NOT NULL DEFAULT 'default',
    chain_id         bigint      NOT NULL,
    -- BIP-44 m/44'/60'/0'/0/{derivation_index}
    derivation_index bigint      NOT NULL CHECK (derivation_index >= 0),
    address          text        NOT NULL CHECK (address ~ '^0x[0-9a-f]{40}$'),
    -- NULL while the row is a free slot in the pool
    account_id       uuid        REFERENCES ledger.accounts (id),
    created_at       timestamptz NOT NULL DEFAULT now(),
    assigned_at      timestamptz,
    CHECK ((account_id IS NULL) = (assigned_at IS NULL))
);
CREATE UNIQUE INDEX deposit_addresses_index_uniq ON chain.deposit_addresses (tenant_id, chain_id, derivation_index);
CREATE UNIQUE INDEX deposit_addresses_address_uniq ON chain.deposit_addresses (tenant_id, chain_id, address);
-- one address per account per chain (§6.4.1); free slots are exempt
CREATE UNIQUE INDEX deposit_addresses_account_uniq ON chain.deposit_addresses (tenant_id, chain_id, account_id)
    WHERE account_id IS NOT NULL;
-- the pool query: count free slots, and take the oldest one
CREATE INDEX deposit_addresses_free_idx ON chain.deposit_addresses (tenant_id, chain_id, id)
    WHERE account_id IS NULL;

-- Privileges (docs/plan-v1.0.md §14): everybody reads (the scanner needs the
-- controlled-address set), only the signer derives, and the api may claim a
-- free row but may not rewrite an address or its derivation index — hence the
-- column list on UPDATE. Nobody gets DELETE: an address that has ever been
-- handed out must stay associated with its account forever.
GRANT SELECT ON chain.deposit_addresses
  TO ex_api, ex_engine, ex_chain, ex_signer, ex_stream, ex_admin, ex_worker, ex_all;
GRANT INSERT ON chain.deposit_addresses TO ex_signer, ex_all;
GRANT UPDATE (account_id, assigned_at) ON chain.deposit_addresses TO ex_api, ex_all;
GRANT USAGE ON SEQUENCE chain.deposit_address_index_seq TO ex_signer, ex_all;
