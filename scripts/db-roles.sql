-- The exchange's per-role login users (docs/plan-v1.0.md §14), for a
-- Postgres that was not initialised by the compose image: a managed
-- database behind the Helm chart's externals, or a cluster that predates
-- a role (ex_backup arrived in Phase 7). infra/postgres/initdb/01-roles.sh
-- does the same job on first start of the postgres container.
--
-- Run as a superuser (or a CREATEROLE user that owns the database), once
-- per cluster, with the passwords as psql variables:
--
--   psql "$ADMIN_URL" -v db=exchange \
--     -v pw_migrate=… -v pw_api=… -v pw_engine=… -v pw_chain=… -v pw_signer=… \
--     -v pw_stream=… -v pw_admin=… -v pw_worker=… -v pw_all=… -v pw_backup=… \
--     -f scripts/db-roles.sql
--
-- Every role gets its own password; a role that already exists keeps its
-- password (rotate with ALTER ROLE … PASSWORD, docs/runbooks/key-rotation.md).
-- Schemas and table grants come from the migrations, not from here.

\set ON_ERROR_STOP on

SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', r.name, r.password)
  FROM (VALUES
    ('ex_migrate', :'pw_migrate'), ('ex_api', :'pw_api'), ('ex_engine', :'pw_engine'),
    ('ex_chain', :'pw_chain'), ('ex_signer', :'pw_signer'), ('ex_stream', :'pw_stream'),
    ('ex_admin', :'pw_admin'), ('ex_worker', :'pw_worker'), ('ex_all', :'pw_all'),
    ('ex_backup', :'pw_backup')
  ) AS r(name, password)
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r.name)
\gexec

SELECT format('GRANT CONNECT ON DATABASE %I TO %I', :'db', r)
  FROM unnest(ARRAY['ex_migrate', 'ex_api', 'ex_engine', 'ex_chain', 'ex_signer', 'ex_stream', 'ex_admin', 'ex_worker', 'ex_all', 'ex_backup']) AS r
\gexec

-- ex_migrate owns the database (and therefore may CREATE in public, where
-- goose keeps its version table) and every schema the migrations create.
SELECT format('ALTER DATABASE %I OWNER TO ex_migrate', :'db')
\gexec

-- ex_backup is what pg_dump runs as: it reads every table and writes only
-- its own record. The table grant is repeated here because migration 0022
-- can only grant it when the role already exists.
GRANT pg_read_all_data TO ex_backup;
SELECT 'GRANT USAGE ON SCHEMA admin TO ex_backup'
 WHERE EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'admin')
\gexec
SELECT 'GRANT SELECT, INSERT ON admin.backups TO ex_backup'
 WHERE EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'admin' AND tablename = 'backups')
\gexec
