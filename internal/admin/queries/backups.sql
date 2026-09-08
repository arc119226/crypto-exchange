-- The backup record (migration 0022): written by the backup sidecar as
-- ex_backup, read here by the admin role for the dashboard and for the
-- backup_last_success_timestamp_seconds gauge.

-- name: LatestBackups :many
SELECT DISTINCT ON (kind) kind, finished_at, size_bytes, location
FROM admin.backups
WHERE status = 'ok'
ORDER BY kind, finished_at DESC;
