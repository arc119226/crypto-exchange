#!/usr/bin/env bash
# Restores the newest pg_dump into a throwaway database and proves the copy
# is whole (docs/runbooks/backup-restore.md): every migration applied, the
# trial balance zero, the balance cache equal to its postings, every
# market's sequence equal to its last order, no hole in the trades. Prints
# the seconds from start to all checks green -- the RTO the beta checklist
# quotes. The same steps, pointed at the real cluster and database name,
# are the restore itself.
#
#   BACKUP_STORE=s3 (BACKUP_S3_* as backup.sh) | file:///dir/with/dumps
#   DRILL_ADMIN_URL      superuser DSN of the cluster to restore into (its postgres database)
#   DRILL_DB             database to create (exchange_drill)
#   EXPECTED_MIGRATION   highest migration that must be applied (default: read from migrations/)
#   SOURCE_DATABASE_URL  optional: a live database whose row counts are printed next to the copy's
#   KEEP=1               leave the restored database in place
set -euo pipefail
t0="$(date +%s)"
: "${BACKUP_STORE:=s3}"
: "${DRILL_DB:=exchange_drill}"
: "${MC_CONFIG_DIR:=/tmp/.mc}"
: "${DRILL_ADMIN_URL:?the superuser DSN of the cluster to restore into}"
export MC_CONFIG_DIR
here="$(cd "$(dirname "$0")" && pwd)"

log() { printf '%s restore-drill: %s\n' "$(date -u +%FT%TZ)" "$*" >&2; }
die() { log "$*"; exit 1; }
read_secret() {
  local file_var="$1_FILE"
  if [ -n "${!1:-}" ]; then printf '%s' "${!1}"; elif [ -n "${!file_var:-}" ]; then tr -d '\n' <"${!file_var}"; fi
}

checks="${RESTORE_CHECKS_SQL:-}"
for c in "$here/sql/restore-checks.sql" /usr/local/share/exchange/restore-checks.sql; do
  [ -n "$checks" ] || [ ! -f "$c" ] || checks="$c"
done
[ -n "$checks" ] || die "restore-checks.sql not found (set RESTORE_CHECKS_SQL)"

if [ -z "${EXPECTED_MIGRATION:-}" ]; then
  [ -d "$here/../migrations" ] || die "EXPECTED_MIGRATION is required outside the repository"
  EXPECTED_MIGRATION="$(ls "$here/../migrations" | sed -n 's/^\([0-9]*\)_.*\.sql$/\1/p' | sort -n | tail -1 | sed 's/^0*//')"
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# 1. the newest dump
case "$BACKUP_STORE" in
  s3)
    access="$(read_secret BACKUP_S3_ACCESS_KEY)"
    secret="$(read_secret BACKUP_S3_SECRET_KEY)"
    [ -n "${BACKUP_S3_ENDPOINT:-}" ] && [ -n "$access" ] && [ -n "$secret" ] || die "BACKUP_S3_ENDPOINT, BACKUP_S3_ACCESS_KEY and BACKUP_S3_SECRET_KEY are required"
    mc alias set backup "$BACKUP_S3_ENDPOINT" "$access" "$secret" >/dev/null
    target="backup/${BACKUP_S3_BUCKET:-exchange-backups}${BACKUP_S3_PREFIX:+/$BACKUP_S3_PREFIX}/dumps"
    newest="$(mc ls "$target/" | awk '{print $NF}' | grep '\.dump$' | sort | tail -1 || true)"
    [ -n "$newest" ] || die "no dump under $target"
    mc --quiet cp "$target/$newest" "$work/$newest" >/dev/null
    dump="$work/$newest"
    ;;
  file://*)
    dir="${BACKUP_STORE#file://}"
    dump="$(ls -1 "$dir"/*.dump 2>/dev/null | sort | tail -1 || true)"
    [ -n "$dump" ] || die "no *.dump under $dir"
    ;;
  *) die "BACKUP_STORE must be s3 or file://<dir>" ;;
esac
size="$(stat -c %s "$dump")"
log "restoring $(basename "$dump") ($size bytes) into $DRILL_DB"

# 2. a fresh database owned by ex_migrate, exactly as 01-roles.sh makes the real one
psql "$DRILL_ADMIN_URL" -v ON_ERROR_STOP=1 -qX \
  -c "DROP DATABASE IF EXISTS \"$DRILL_DB\"" \
  -c "CREATE DATABASE \"$DRILL_DB\" OWNER ex_migrate"
base="${DRILL_ADMIN_URL%%\?*}"
query="${DRILL_ADMIN_URL#"$base"}"
drill_url="${base%/*}/$DRILL_DB$query"

# 3. restore as the superuser with every object owned by ex_migrate; the
# grants inside the dump name the ex_* roles, which the cluster already has
pg_restore --no-owner --role=ex_migrate --exit-on-error -j 2 -d "$drill_url" "$dump"

# 4. the copy is at the schema the binaries expect
applied="$(psql "$drill_url" -qtAX -c "SELECT max(version_id) FROM goose_db_version WHERE is_applied")"
[ "$applied" = "$EXPECTED_MIGRATION" ] || die "migrations: the copy is at $applied, expected $EXPECTED_MIGRATION"

# 5. the invariants
psql "$drill_url" -v ON_ERROR_STOP=1 -qX -f "$checks"

# 6. the source, for comparison (it may have moved on since the dump)
if [ -n "${SOURCE_DATABASE_URL:-}" ]; then
  log "row counts, source vs copy"
  for t in ledger.journal_entries ledger.postings trading.orders trading.trades auth.users; do
    printf '  %-24s %10s %10s\n' "$t" \
      "$(psql "$SOURCE_DATABASE_URL" -qtAX -c "SELECT count(*) FROM $t")" \
      "$(psql "$drill_url" -qtAX -c "SELECT count(*) FROM $t")" >&2
  done
fi

rto=$(( $(date +%s) - t0 ))
echo "restore drill: OK dump=$(basename "$dump") size_bytes=$size migration=$applied rto_seconds=$rto"
if [ "${KEEP:-0}" != 1 ]; then
  psql "$DRILL_ADMIN_URL" -qX -c "DROP DATABASE \"$DRILL_DB\""
fi
