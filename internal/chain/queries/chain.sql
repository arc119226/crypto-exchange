-- name: CountFreeDepositAddresses :one
SELECT count(*) FROM chain.deposit_addresses
WHERE tenant_id = $1 AND chain_id = $2 AND account_id IS NULL;

-- name: NextDepositAddressIndex :one
-- Drawn before deriving, because the address is a function of the index.
SELECT nextval('chain.deposit_address_index_seq')::bigint;

-- name: InsertDepositAddress :execrows
-- ON CONFLICT keeps a refill idempotent if the same address is derived twice
-- (a rewound sequence); the caller counts what it actually created.
INSERT INTO chain.deposit_addresses (tenant_id, chain_id, derivation_index, address)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;

-- name: GetDepositAddressByAccount :one
SELECT * FROM chain.deposit_addresses
WHERE tenant_id = $1 AND chain_id = $2 AND account_id = $3;

-- name: ClaimDepositAddress :one
-- The pool claim of docs/plan-v1.0.md §6.4.1: SKIP LOCKED so concurrent
-- callers take different rows instead of queueing behind one another.
UPDATE chain.deposit_addresses AS claimed
SET account_id = $3, assigned_at = now()
WHERE claimed.id = (
    SELECT free.id FROM chain.deposit_addresses AS free
    WHERE free.tenant_id = $1 AND free.chain_id = $2 AND free.account_id IS NULL
    ORDER BY free.id
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING claimed.*;

-- name: ListDepositAddresses :many
-- The scanner's controlled-address set; ordered by id so it can resume from
-- the highest id it has already seen.
SELECT * FROM chain.deposit_addresses
WHERE tenant_id = $1 AND chain_id = $2
ORDER BY id;
