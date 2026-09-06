-- name: InsertAuditEvent :one
INSERT INTO audit.audit_events (tenant_id, actor_type, actor_id, action, target_type, target_id, before, after, ip, correlation_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: ListAuditEvents :many
SELECT * FROM audit.audit_events
WHERE tenant_id = $1
  AND (sqlc.arg(action)::text = '' OR action = sqlc.arg(action)::text)
  AND (sqlc.arg(target_type)::text = '' OR target_type = sqlc.arg(target_type)::text)
  AND (sqlc.arg(target_id)::text = '' OR target_id = sqlc.arg(target_id)::text)
ORDER BY id DESC
LIMIT $2 OFFSET $3;
