#!/usr/bin/env bash
# The backup sidecar (docs/runbooks/backup-restore.md, docs/plan-v1.0.md §12
# Phase 7). Postgres archives every completed WAL segment into /wal-archive
# (archive_command in deploy/compose/compose.yaml); this script ships them
# to an S3-compatible store every WAL_SHIP_INTERVAL, takes a pg_dump every
# BACKUP_INTERVAL, prunes objects older than BACKUP_RETENTION_DAYS, and
# records every outcome in admin.backups -- the row the admin role turns
# into backup_last_success_timestamp_seconds and the BackupStale and
# WalArchiveStale alerts read. The row is the proof; the object is the backup.
#
#   backup.sh run    loop forever (the sidecar; the default)
#   backup.sh once   ship WAL, dump, ship again; non-zero on any failure (CI, drills)
#   backup.sh ship   ship WAL once and exit
#
# Environment (every credential also as NAME_FILE):
#   PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE     pg_dump and psql run as ex_backup
#   BACKUP_S3_ENDPOINT BACKUP_S3_BUCKET BACKUP_S3_ACCESS_KEY BACKUP_S3_SECRET_KEY [BACKUP_S3_PREFIX]
#   BACKUP_INTERVAL (24h)  WAL_SHIP_INTERVAL (30s)  BACKUP_RETENTION_DAYS (14)
#   WAL_ARCHIVE_DIR (/wal-archive)  BACKUP_DIR (/backups)
set -euo pipefail

: "${WAL_ARCHIVE_DIR:=/wal-archive}"
: "${BACKUP_DIR:=/backups}"
: "${BACKUP_INTERVAL:=24h}"
: "${WAL_SHIP_INTERVAL:=30s}"
: "${BACKUP_RETENTION_DAYS:=14}"
: "${BACKUP_S3_BUCKET:=exchange-backups}"
: "${BACKUP_S3_PREFIX:=}"
: "${MC_CONFIG_DIR:=/tmp/.mc}"
export MC_CONFIG_DIR

log() { printf '%s backup: %s\n' "$(date -u +%FT%TZ)" "$*" >&2; }
die() { log "$*"; exit 1; }

# read_secret NAME prints NAME, else the contents of the file NAME_FILE names.
read_secret() {
  local file_var="$1_FILE"
  if [ -n "${!1:-}" ]; then printf '%s' "${!1}"; elif [ -n "${!file_var:-}" ]; then tr -d '\n' <"${!file_var}"; fi
}
# seconds turns 30s / 5m / 24h / 7d (or a bare number) into seconds.
seconds() {
  case "$1" in
    *s) echo $(( ${1%s} )) ;;
    *m) echo $(( ${1%m} * 60 )) ;;
    *h) echo $(( ${1%h} * 3600 )) ;;
    *d) echo $(( ${1%d} * 86400 )) ;;
    *) echo $(( $1 )) ;;
  esac
}

PGPASSWORD="$(read_secret PGPASSWORD)"
export PGPASSWORD
access="$(read_secret BACKUP_S3_ACCESS_KEY)"
secret="$(read_secret BACKUP_S3_SECRET_KEY)"
[ -n "${BACKUP_S3_ENDPOINT:-}" ] && [ -n "$access" ] && [ -n "$secret" ] ||
  die "BACKUP_S3_ENDPOINT, BACKUP_S3_ACCESS_KEY and BACKUP_S3_SECRET_KEY are required"
target="backup/${BACKUP_S3_BUCKET}${BACKUP_S3_PREFIX:+/$BACKUP_S3_PREFIX}"
dump_every="$(seconds "$BACKUP_INTERVAL")"
ship_every="$(seconds "$WAL_SHIP_INTERVAL")"
mkdir -p "$WAL_ARCHIVE_DIR" "$BACKUP_DIR"

store_ready() {
  mc alias set backup "$BACKUP_S3_ENDPOINT" "$access" "$secret" >/dev/null 2>&1 || return 1
  mc mb --ignore-existing "backup/$BACKUP_S3_BUCKET" >/dev/null 2>&1
}

# record KIND STARTED STATUS SIZE LOCATION [ERROR] writes one admin.backups
# row as ex_backup (migration 0022). A failure to record is logged, not
# fatal: the backup itself may well have succeeded.
record() {
  psql -v ON_ERROR_STOP=1 -qX -v kind="$1" -v started="$2" -v status="$3" -v size="$4" -v location="$5" -v error="${6:-}" <<'SQL' || log "could not record the $1 outcome in admin.backups"
INSERT INTO admin.backups (kind, started_at, status, size_bytes, location, error)
VALUES (:'kind', :'started'::timestamptz, :'status', :'size'::bigint, :'location', NULLIF(:'error', ''));
SQL
}

# ship_wal uploads every archived segment in name order (which is time
# order) and deletes each only after its upload succeeded, then records the
# batch as one wal row naming the last segment.
ship_wal() {
  local started n=0 bytes=0 last="" f name
  started="$(date -u +%FT%TZ)"
  shopt -s nullglob
  for f in "$WAL_ARCHIVE_DIR"/*; do
    [ -f "$f" ] || continue
    name="$(basename "$f")"
    if ! mc --quiet cp "$f" "$target/wal/$name" >/dev/null 2>&1; then
      shopt -u nullglob
      record wal "$started" failed 0 "$name" "upload failed"
      log "WAL upload of $name failed; $(ls "$WAL_ARCHIVE_DIR" | wc -l) segments waiting"
      return 1
    fi
    bytes=$(( bytes + $(stat -c %s "$f") ))
    rm -f "$f"
    n=$(( n + 1 ))
    last="$name"
  done
  shopt -u nullglob
  if [ "$n" -gt 0 ]; then
    record wal "$started" ok "$bytes" "wal/$last"
    log "shipped $n WAL segment(s) up to $last"
  fi
}

# ship_dumps retries dumps whose upload failed earlier.
ship_dumps() {
  local f name started
  shopt -s nullglob
  for f in "$BACKUP_DIR"/*.dump; do
    name="$(basename "$f")"
    started="$(date -u +%FT%TZ)"
    if mc --quiet cp "$f" "$target/dumps/$name" >/dev/null 2>&1; then
      record dump "$started" ok "$(stat -c %s "$f")" "dumps/$name" ""
      rm -f "$f"
      touch "$BACKUP_DIR/.last-dump"
      log "uploaded the earlier dump $name"
    else
      shopt -u nullglob
      return 1
    fi
  done
  shopt -u nullglob
}

# take_dump writes one custom-format dump, uploads it, records it, and
# only then counts it as taken. A dump that could not be uploaded stays on
# disk for ship_dumps.
take_dump() {
  local started stamp file key size
  started="$(date -u +%FT%TZ)"
  stamp="$(date -u +%Y%m%dT%H%M%SZ)"
  file="$BACKUP_DIR/exchange-$stamp.dump"
  key="$target/dumps/exchange-$stamp.dump"
  log "pg_dump of $PGDATABASE as $PGUSER"
  if ! pg_dump -Fc -Z 6 -f "$file"; then
    rm -f "$file"
    record dump "$started" failed 0 "dumps/exchange-$stamp.dump" "pg_dump failed"
    return 1
  fi
  size="$(stat -c %s "$file")"
  if ! mc --quiet cp "$file" "$key" >/dev/null 2>&1; then
    record dump "$started" failed "$size" "dumps/exchange-$stamp.dump" "upload failed"
    return 1
  fi
  rm -f "$file"
  touch "$BACKUP_DIR/.last-dump"
  record dump "$started" ok "$size" "dumps/exchange-$stamp.dump" ""
  log "dump exchange-$stamp.dump ($size bytes) uploaded"
}

# prune applies the retention to both prefixes. Bucket lifecycle rules would
# do the same on MinIO and S3 alike, but they are configured outside this
# repository; this runs wherever the sidecar runs.
prune() {
  mc rm --recursive --force --older-than "${BACKUP_RETENTION_DAYS}d" "$target/dumps/" >/dev/null 2>&1 || true
  mc rm --recursive --force --older-than "${BACKUP_RETENTION_DAYS}d" "$target/wal/" >/dev/null 2>&1 || true
}

dump_due() {
  [ ! -e "$BACKUP_DIR/.last-dump" ] ||
    [ $(( $(date +%s) - $(stat -c %Y "$BACKUP_DIR/.last-dump") )) -ge "$dump_every" ]
}

mode="${1:-run}"
# Bounded: these run in `once` mode as well, and the CI drill calls that. An
# endpoint that never comes up used to hold the job until GitHub's 6-hour
# ceiling; now it fails in two minutes and says which dependency was missing.
wait_for() {
  local what=$1 tries=${WAIT_TRIES:-24}
  shift
  for _ in $(seq 1 "$tries"); do
    "$@" && return 0
    log "waiting for $what"
    sleep 5
  done
  log "gave up waiting for $what after $((tries * 5))s"
  return 1
}
wait_for "the store at $BACKUP_S3_ENDPOINT" store_ready
wait_for "postgres at ${PGHOST:-localhost}" pg_isready -q

case "$mode" in
  ship) ship_wal ;;
  once)
    ship_wal
    ship_dumps
    take_dump
    prune
    ship_wal
    ;;
  run)
    trap 'log "stopping"; exit 0' TERM INT
    log "shipping WAL every $WAL_SHIP_INTERVAL, dumping every $BACKUP_INTERVAL, keeping $BACKUP_RETENTION_DAYS days in $target"
    while true; do
      ship_wal || true
      ship_dumps || true
      if dump_due; then
        take_dump && prune || true
      fi
      sleep "$ship_every" &
      wait $!
    done
    ;;
  *) die "usage: backup.sh [run|once|ship]" ;;
esac
