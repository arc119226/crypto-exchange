-- name: InsertSweep :one
INSERT INTO chain.sweeps (
    tenant_id, chain_id, address_id, from_address, asset, amount, correlation_id
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetSweep :one
SELECT * FROM chain.sweeps WHERE tenant_id = $1 AND id = $2;

-- name: ClaimSweeps :many
-- The worker's queue, oldest first. SKIP LOCKED for the same reason
-- ClaimWithdrawals uses it: a second worker takes different rows rather than
-- waiting behind the first.
SELECT * FROM chain.sweeps
WHERE tenant_id = $1 AND status = ANY(@statuses::text[])
ORDER BY created_at
LIMIT $2
FOR UPDATE SKIP LOCKED;

-- name: ListSweeps :many
SELECT * FROM chain.sweeps
WHERE tenant_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: FundSweepGas :one
-- Records the gas-funding transaction before it is sent, so a crash cannot
-- lose track of ETH the hot wallet has already committed.
UPDATE chain.sweeps
SET gas_funding_nonce = $3, gas_funding_raw_tx = $4, gas_funding_tx_hash = $5,
    gas_funding_amount = $6, version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: MarkSweepGasFunded :one
-- The funding transaction is mined: the address can now pay for its own
-- transfer, and the ETH that moved is booked.
UPDATE chain.sweeps
SET status = 'gas_funded', gas_funding_cost = $3, version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: AllocateSweepNonce :one
-- Pins the nonce before anything is signed, the same discipline
-- AllocateWithdrawalNonce enforces: a crash between signing and recording must
-- come back asking for the same transaction, not a different one.
UPDATE chain.sweeps
SET nonce = $3, version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: SignSweep :one
UPDATE chain.sweeps
SET raw_tx = $3, tx_hash = $4, version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: MarkSweepBroadcast :one
UPDATE chain.sweeps
SET status = 'broadcast', broadcast_at = now(), version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: MarkSweepConfirmed :one
UPDATE chain.sweeps
SET status = 'confirmed', block_number = $3, gas_cost = $4,
    version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: FailSweep :one
UPDATE chain.sweeps
SET status = 'failed', failure_reason = $3, gas_cost = $4,
    version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND id = $2
RETURNING *;

-- name: SweptFromAddress :one
-- How much of this address's asset the ledger has already moved to the hot
-- wallet. Only confirmed sweeps count: one still in flight has not moved
-- anything, and one that failed never will.
SELECT COALESCE(SUM(amount + COALESCE(gas_cost, 0)), 0)::numeric(36,18) AS total
FROM chain.sweeps
WHERE tenant_id = $1 AND address_id = $2 AND asset = $3 AND status = 'confirmed';

-- name: CreditedToAddress :one
-- How much of this asset the ledger believes arrived at this address: the sum
-- of the deposits it actually credited.
--
-- This is the ceiling on what may be swept. The chain balance can be higher --
-- an internal contract transfer the scanner does not see (§4 puts those out of
-- scope but nothing prevents them), or a deposit still confirming -- and
-- sweeping money custody:deposit_addresses was never credited for would make
-- that account claim a transfer it never received.
SELECT COALESCE(SUM(d.amount), 0)::numeric(36,18) AS total
FROM chain.deposits d
JOIN chain.deposit_addresses a
  ON a.tenant_id = d.tenant_id AND a.chain_id = d.chain_id AND a.address = d.address
WHERE d.tenant_id = $1 AND a.id = $2 AND d.asset = $3 AND d.status = 'credited';

-- name: CountUnsettledDeposits :one
-- Deposits on this address that the ledger has not credited yet. An address
-- with any of them is not swept at all: the sweep would move money the ledger
-- is about to record arriving, and the two would cross.
SELECT count(*) FROM chain.deposits d
JOIN chain.deposit_addresses a
  ON a.tenant_id = d.tenant_id AND a.chain_id = d.chain_id AND a.address = d.address
WHERE d.tenant_id = $1 AND a.id = $2 AND d.status IN ('detected', 'confirming', 'orphaned');

-- name: GetDepositAddressByID :one
SELECT * FROM chain.deposit_addresses WHERE tenant_id = $1 AND id = $2;

-- name: GetDepositAddressByAddress :one
SELECT * FROM chain.deposit_addresses
WHERE tenant_id = $1 AND chain_id = $2 AND address = $3;
