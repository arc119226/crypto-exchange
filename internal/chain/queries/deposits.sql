-- name: GetChainState :one
SELECT * FROM chain.chain_state WHERE tenant_id = $1 AND chain_id = $2;

-- name: InsertChainState :exec
INSERT INTO chain.chain_state (tenant_id, chain_id, genesis_hash)
VALUES ($1, $2, $3);

-- name: GetScanCursor :one
SELECT * FROM chain.scan_cursors WHERE tenant_id = $1 AND chain_id = $2;

-- name: UpsertScanCursor :exec
INSERT INTO chain.scan_cursors (tenant_id, chain_id, last_scanned_block, last_block_hash)
VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id, chain_id) DO UPDATE
SET last_scanned_block = excluded.last_scanned_block,
    last_block_hash    = excluded.last_block_hash,
    updated_at         = now();

-- name: UpsertBlock :exec
-- A rescan after a reorg writes a different hash at the same height, so the
-- conflict target is the height and the hash is overwritten.
INSERT INTO chain.blocks (tenant_id, chain_id, number, hash, parent_hash)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (tenant_id, chain_id, number) DO UPDATE
SET hash = excluded.hash, parent_hash = excluded.parent_hash, seen_at = now();

-- name: GetBlock :one
SELECT * FROM chain.blocks WHERE tenant_id = $1 AND chain_id = $2 AND number = $3;

-- name: DeleteBlocksFrom :execrows
-- Drops the abandoned branch when the cursor rewinds.
DELETE FROM chain.blocks WHERE tenant_id = $1 AND chain_id = $2 AND number >= $3;

-- name: PruneBlocksBelow :execrows
-- Keeps the ring bounded; see the DELETE grant in migration 0009.
DELETE FROM chain.blocks WHERE tenant_id = $1 AND chain_id = $2 AND number < $3;

-- name: GetDepositForUpdate :one
-- The re-detection path of docs/plan-v1.0.md §6.4.1: a deposit that reappears
-- on the new canonical chain must be UPDATEd, never INSERTed, or the unique
-- key rejects it and it can never be credited.
SELECT * FROM chain.deposits
WHERE tenant_id = $1 AND chain_id = $2 AND tx_hash = $3 AND log_index = $4
FOR UPDATE;

-- name: InsertDeposit :one
INSERT INTO chain.deposits (
    tenant_id, chain_id, tx_hash, log_index, address, account_id, asset, amount,
    block_number, block_hash, confirmations, status, correlation_id
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING *;

-- name: UpdateDepositSighting :one
-- Records where a deposit currently sits: a new block on a rescan, or another
-- confirmation on the same one. Clears orphaned_at_block because a sighting
-- means it is back on the canonical chain.
UPDATE chain.deposits
SET block_number = $3, block_hash = $4, confirmations = $5, status = $6,
    orphaned_at_block = NULL, version = version + 1, updated_at = now()
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: MarkDepositCredited :one
UPDATE chain.deposits
SET status = 'credited', confirmations = $3, credited_at = now(),
    version = version + 1, updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND status <> 'credited'
RETURNING *;

-- name: MarkDepositsOrphaned :many
-- Everything not yet credited in the abandoned blocks. Credited deposits are
-- deliberately excluded: undoing those is the manual `reversed` path.
UPDATE chain.deposits
SET status = 'orphaned', orphaned_at_block = $4, version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND chain_id = $2 AND block_number >= $3
  AND status IN ('detected', 'confirming')
RETURNING *;

-- name: ListMaturingDeposits :many
SELECT * FROM chain.deposits
WHERE tenant_id = $1 AND chain_id = $2 AND status IN ('detected', 'confirming')
ORDER BY block_number, id;

-- name: ListExpiredOrphans :many
SELECT * FROM chain.deposits
WHERE tenant_id = $1 AND chain_id = $2 AND status = 'orphaned' AND orphaned_at_block <= $3
ORDER BY id;

-- name: MarkDepositDropped :exec
UPDATE chain.deposits
SET status = 'dropped', version = version + 1, updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND status = 'orphaned';

-- name: ListDepositsByAccount :many
SELECT * FROM chain.deposits
WHERE tenant_id = $1 AND account_id = $2
ORDER BY created_at DESC, id
LIMIT $3 OFFSET $4;

-- name: ListDeposits :many
SELECT * FROM chain.deposits
WHERE tenant_id = $1
  AND (sqlc.arg(status)::text = '' OR status = sqlc.arg(status)::text)
  AND (sqlc.arg(asset)::text = '' OR asset = sqlc.arg(asset)::text)
ORDER BY created_at DESC, id
LIMIT $2 OFFSET $3;

-- name: CountDepositsByStatus :many
-- Backs deposits_total{asset,status} without keeping a counter in memory
-- across restarts.
SELECT asset, status, count(*) AS total
FROM chain.deposits
WHERE tenant_id = $1 AND chain_id = $2
GROUP BY asset, status;
