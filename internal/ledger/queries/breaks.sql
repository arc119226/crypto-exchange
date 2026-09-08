-- admin.ledger_breaks (0019): the ledger's own reconciliation. Owned by the
-- ledger package because the invariant it records -- debits equal credits per
-- asset -- is the ledger's, even though the table sits in the admin schema
-- next to the chain reconciliation it complements.

-- name: ListOpenLedgerBreaks :many
SELECT * FROM admin.ledger_breaks WHERE tenant_id = $1 AND resolved_at IS NULL ORDER BY asset;

-- name: CountOpenLedgerBreaks :one
SELECT count(*) FROM admin.ledger_breaks WHERE tenant_id = $1 AND resolved_at IS NULL;

-- name: ListLedgerBreaks :many
SELECT * FROM admin.ledger_breaks WHERE tenant_id = $1 ORDER BY detected_at DESC, asset LIMIT $2 OFFSET $3;

-- name: InsertLedgerBreak :one
INSERT INTO admin.ledger_breaks (tenant_id, asset, debits, credits, diff, detected_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ResolveLedgerBreak :execrows
UPDATE admin.ledger_breaks SET resolved_at = $2 WHERE id = $1 AND resolved_at IS NULL;
