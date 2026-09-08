-- +goose Up
-- Phase 7 operations (docs/plan-v1.0.md §12 Phase 7): the backup record,
-- the role that takes backups, and the deletes the retention job needs.

-- admin.backups is written by the backup sidecar after every successful
-- pg_dump upload and WAL drain (scripts/backup.sh, scripts/wal-ship.sh),
-- read by the admin role for GET /admin/v1/system/status and the
-- backup_last_success_timestamp_seconds gauge behind the BackupStale and
-- WalArchiveStale alerts. The row is the proof; the file in the bucket is
-- the backup.
CREATE TABLE admin.backups (
    id          bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind        text        NOT NULL CHECK (kind IN ('dump', 'wal')),
    started_at  timestamptz NOT NULL,
    finished_at timestamptz NOT NULL DEFAULT now(),
    status      text        NOT NULL CHECK (status IN ('ok', 'failed')),
    size_bytes  bigint      NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    location    text        NOT NULL,   -- the object key (dump) or the last archived WAL segment
    error       text
);
CREATE INDEX backups_kind_finished_idx ON admin.backups (kind, finished_at DESC) WHERE status = 'ok';

GRANT SELECT ON admin.backups TO ex_admin, ex_all;
GRANT INSERT ON admin.backups TO ex_all;

-- ex_backup is the login pg_dump runs as: read everything, write only its
-- own record. The role itself is created by infra/postgres/initdb/01-roles.sh
-- (fresh clusters) or scripts/db-roles.sql (existing ones); migrations run
-- as ex_migrate, which cannot create roles, so the grant is conditional and
-- scripts/db-roles.sql repeats it for a cluster that got the role later.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ex_backup') THEN
        GRANT USAGE ON SCHEMA admin TO ex_backup;
        GRANT SELECT, INSERT ON admin.backups TO ex_backup;
    END IF;
END $$;
-- +goose StatementEnd

-- Retention (docs/plan-v1.0.md §7.3: the outbox is kept 30 days). 0006
-- deferred these deletes to "a worker job in a later phase"; the worker
-- role's retention loop is that job (internal/app/retention.go).
GRANT DELETE ON eventbus.outbox, eventbus.processed_events, webhook.events, webhook.deliveries TO ex_worker, ex_all;

-- +goose Down
REVOKE DELETE ON eventbus.outbox, eventbus.processed_events, webhook.events, webhook.deliveries FROM ex_worker, ex_all;
DROP TABLE admin.backups;
