-- Deliberately no RETURNING: migration 0004 grants writers INSERT but not
-- SELECT, and `INSERT … RETURNING` needs SELECT on the columns it returns.
-- Adding one makes every role but ex_admin and ex_all fail with 42501 as
-- soon as they run in their own container (see TestAPIRolePrivileges).
-- name: InsertAuditEvent :exec
INSERT INTO audit.audit_events (tenant_id, actor_type, actor_id, action, target_type, target_id, before, after, ip, correlation_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: ListAuditEvents :many
SELECT * FROM audit.audit_events
WHERE tenant_id = $1
  AND (sqlc.arg(action)::text = '' OR action = sqlc.arg(action)::text)
  AND (sqlc.arg(target_type)::text = '' OR target_type = sqlc.arg(target_type)::text)
  AND (sqlc.arg(target_id)::text = '' OR target_id = sqlc.arg(target_id)::text)
  AND (sqlc.arg(actor_type)::text = '' OR actor_type = sqlc.arg(actor_type)::text)
  AND (sqlc.arg(actor_id)::text = '' OR actor_id = sqlc.arg(actor_id)::text)
ORDER BY id DESC
LIMIT $2 OFFSET $3;
