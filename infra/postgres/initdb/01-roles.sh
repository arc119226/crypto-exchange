#!/usr/bin/env bash
# Creates the per-role login users used by the exchange binary
# (docs/plan-v1.0.md §14). Runs once on first start of the postgres container
# (docker-entrypoint-initdb.d) and in the testcontainers integration tests.
# In dev every role shares POSTGRES_PASSWORD; production uses DB_PASSWORD_<ROLE>.
# Schemas and grants are created by goose migrations, not here.
set -euo pipefail

: "${POSTGRES_DB:=exchange}"
: "${POSTGRES_USER:=exchange}"
: "${POSTGRES_PASSWORD:?POSTGRES_PASSWORD must be set}"

roles=(ex_migrate ex_api ex_engine ex_chain ex_signer ex_stream ex_admin ex_worker ex_all)

sql=""
for r in "${roles[@]}"; do
  pw_var="DB_PASSWORD_${r^^}"
  pw="${!pw_var:-$POSTGRES_PASSWORD}"
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
SQL
echo "exchange roles created: ${roles[*]}"
