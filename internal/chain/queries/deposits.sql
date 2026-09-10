-- name: GetChainState :one
SELECT * FROM chain.chain_state WHERE tenant_id = $1 AND chain_id = $2;

-- name: InsertChainState :exec
-- The anchor is the block this database's view of the chain starts at
-- (ETH_SCAN_START_BLOCK) together with its hash. Recorded once, compared on
-- every start: a different hash is a different chain, and a different block
-- means the setting moved under a live database.
INSERT INTO chain.chain_state (tenant_id, chain_id, anchor_block, anchor_hash)
VALUES ($1, $2, $3, $4);

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
    fee = $4, credited_amount = $5,
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

-- name: CountDepositsInStatus :one
-- The dashboard's "deposits still confirming" tile.
SELECT count(*) FROM chain.deposits
WHERE tenant_id = $1 AND status = ANY(@statuses::text[]);

-- name: MarkDepositsReorged :many
-- Credited deposits whose block was abandoned by a reorg. The status stays
-- 'credited' -- the ledger still holds the credit and must until a reversing
-- entry is posted -- so this only stamps the height, which is what puts the
-- row in the operator's queue. Already-stamped rows are left alone: the first
-- reorg is the one that matters, and a later, shallower one must not move the
-- mark to a height the deposit was never valid at.
UPDATE chain.deposits
SET reorged_at_block = $4, version = version + 1, updated_at = now()
WHERE tenant_id = $1 AND chain_id = $2 AND block_number >= $3
  AND status = 'credited' AND reorged_at_block IS NULL
RETURNING *;

-- name: ListDepositsAwaitingReversal :many
-- The operator's queue: credited deposits the chain no longer shows, with no
-- decision recorded yet.
SELECT * FROM chain.deposits
WHERE tenant_id = $1 AND status = 'credited' AND reorged_at_block IS NOT NULL
  AND reversal_requested_at IS NULL
ORDER BY reorged_at_block, id
LIMIT $2 OFFSET $3;

-- name: CountDepositsAwaitingReversal :one
-- Everything that has been stamped and not yet reversed, whether or not a
-- person has decided about it. The gauge behind DepositAwaitingReversal: this
-- number being anything but zero means an account holds a balance the chain
-- does not back.
SELECT count(*) FROM chain.deposits
WHERE tenant_id = $1 AND status = 'credited' AND reorged_at_block IS NOT NULL;

-- name: RequestDepositReversal :one
-- The admin role's only write to this table. It records a decision; it cannot
-- move money, change the status, or touch a deposit the scanner has not marked.
UPDATE chain.deposits
SET reversal_requested_by = $3, reversal_requested_at = now(), reversal_note = $4,
    version = version + 1, updated_at = now()
WHERE id = $1 AND tenant_id = $2
  AND status = 'credited' AND reorged_at_block IS NOT NULL
  AND reversal_requested_at IS NULL
RETURNING *;

-- name: ClaimDepositReversals :many
-- What the chain role should reverse on this tick.
SELECT * FROM chain.deposits
WHERE tenant_id = $1 AND status = 'credited' AND reversal_requested_at IS NOT NULL
ORDER BY reversal_requested_at
LIMIT $2;

-- name: MarkDepositReversed :one
-- Applied. credited_at goes because 0009 ties it to the status; fee and
-- credited_amount stay, because 0024 tied them to "the ledger moved" instead
-- and they are the record of what was undone.
UPDATE chain.deposits
SET status = 'reversed', reversed_at = now(), credited_at = NULL,
    reversal_requested_at = NULL, reversal_error = NULL,
    version = version + 1, updated_at = now()
WHERE id = $1 AND tenant_id = $2 AND status = 'credited'
RETURNING *;

-- name: RecordDepositReversalError :exec
-- The reversal could not be posted -- almost always because the account has
-- already spent the money. The request is cleared so the worker does not spin
-- on it, and the reason stays for the person who asked.
UPDATE chain.deposits
SET reversal_requested_at = NULL, reversal_error = $3,
    version = version + 1, updated_at = now()
WHERE id = $1 AND tenant_id = $2;
