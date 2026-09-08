#!/usr/bin/env bash
# Creates the per-role login users used by the exchange binary
# (docs/plan-v1.0.md §14). Runs once on first start of the postgres container
# (docker-entrypoint-initdb.d) and in the testcontainers integration tests.
# In dev every role shares POSTGRES_PASSWORD; production gives each role its
# own DB_PASSWORD_<ROLE>, or DB_PASSWORD_<ROLE>_FILE pointing at a secret
# file (compose.prod.yaml). scripts/db-roles.sql does the same for a cluster
# that was not initialised by this image. Schemas and grants are created by
# goose migrations, not here.
set -euo pipefail

: "${POSTGRES_DB:=exchange}"
: "${POSTGRES_USER:=exchange}"
: "${POSTGRES_PASSWORD:?POSTGRES_PASSWORD must be set}"

roles=(ex_migrate ex_api ex_engine ex_chain ex_signer ex_stream ex_admin ex_worker ex_all ex_backup)

# password_for prints the role's password: DB_PASSWORD_<ROLE>_FILE, then
# DB_PASSWORD_<ROLE>, then the shared POSTGRES_PASSWORD.
password_for() {
  local file_var="DB_PASSWORD_${1^^}_FILE" pw_var="DB_PASSWORD_${1^^}"
  if [ -n "${!file_var:-}" ]; then
    cat "${!file_var}"
  else
    printf '%s' "${!pw_var:-$POSTGRES_PASSWORD}"
  fi
}

sql=""
for r in "${roles[@]}"; do
  pw=$(password_for "$r")
  sql+="
DO \$\$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '${r}') THEN
    CREATE ROLE ${r} LOGIN PASSWORD '${pw}';
  END IF;
END \$\$;
GRANT CONNECT ON DATABASE \"${POSTGRES_DB}\" TO ${r};"
done

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<SQL
${sql}
-- ex_migrate owns the database (and therefore may CREATE in public, where
-- goose keeps its version table) and every schema created by migrations.
ALTER DATABASE "${POSTGRES_DB}" OWNER TO ex_migrate;
-- ex_backup is what pg_dump runs as: it reads every table and writes only
-- admin.backups (granted by migration 0022).
GRANT pg_read_all_data TO ex_backup;
SQL
echo "exchange roles created: ${roles[*]}"
