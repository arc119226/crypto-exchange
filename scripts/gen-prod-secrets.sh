#!/usr/bin/env bash
# Creates the beta VM's secrets (deploy/compose/compose.prod.yaml,
# docs/runbooks/beta-deploy.md):
#   .env.prod            from .env.prod.example (fill in the non-secret values by hand)
#   secrets/prod/*       one file per credential, random where a random value will do
#   secrets/prod/keystore/  is NOT created here: import the mnemonic with
#                        `exchange keys import-mnemonic` (see the end of this script)
#
# Compose bind-mounts each file into its containers as /run/secrets/<name>,
# so the owner and mode on the host are what the process sees. Files are
# root-owned, mode 0640, with the group of the uid that reads them: 65532
# (the distroless nonroot the roles run as) or 999 (postgres, whose initdb
# script reads the per-role passwords). Run it as root.
#
#   sudo scripts/gen-prod-secrets.sh        # idempotent: existing files are kept; FORCE=1 regenerates
set -euo pipefail
cd "$(dirname "$0")/.."

FORCE="${FORCE:-0}"
OUT=secrets/prod
APP_GID=65532   # distroless nonroot
PG_GID=999      # the postgres image's postgres user

log() { printf 'gen-prod-secrets: %s\n' "$*" >&2; }
[ "$(id -u)" = 0 ] || { log "run as root: the files must be owned by root with the readers' groups"; exit 2; }
command -v openssl >/dev/null || { log "openssl is required"; exit 2; }

umask 077
mkdir -p "$OUT"
chmod 0750 "$OUT"
chgrp "$APP_GID" "$OUT"

# write NAME GID VALUE writes the file unless it exists (or FORCE=1)
write() {
  local name="$1" gid="$2" value="$3" path="$OUT/$1"
  if [ -s "$path" ] && [ "$FORCE" != 1 ]; then
    return 0
  fi
  printf '%s' "$value" >"$path"
  chown "root:$gid" "$path"
  chmod 0640 "$path"
  log "wrote $path"
}
read_file() { tr -d '\n' <"$OUT/$1"; }
rand_hex() { openssl rand -hex "$1"; }

# 1. .env.prod
if [ ! -f .env.prod ] || [ "$FORCE" = 1 ]; then
  cp .env.prod.example .env.prod
  chmod 0600 .env.prod
  log "wrote .env.prod from .env.prod.example: fill in EXCHANGE_*_IMAGE, EDGE_DOMAIN, ETH_RPC_URL, ETH_SCAN_START_BLOCK, HOT_WALLET_ADDRESS, BACKUP_S3_ENDPOINT"
fi

# 2. Postgres: the superuser and one password per role, then the DSN each
# role's DATABASE_URL_FILE points at. The password files are read by
# 01-roles.sh (as postgres, on first start) and the backup sidecar; the DSNs
# by the roles.
write postgres_password "$PG_GID" "$(rand_hex 24)"
for role in migrate api engine chain signer stream admin worker all backup; do
  write "db_password_$role" "$PG_GID" "$(rand_hex 24)"
done
for role in migrate api engine chain signer stream admin worker all; do
  write "db_url_$role" "$APP_GID" "postgres://ex_${role}:$(read_file "db_password_$role")@postgres:5432/exchange?sslmode=disable"
done

# 3. NATS and Redis
write nats_password "$APP_GID" "$(rand_hex 24)"
write nats_url "$APP_GID" "nats://exchange:$(read_file nats_password)@nats:4222"
write redis_password "$APP_GID" "$(rand_hex 24)"

# 4. the master keys, the admin credentials, the keystore passphrase
write api_key_master_key "$APP_GID" "$(rand_hex 32)"
write webhook_signing_key "$APP_GID" "$(rand_hex 32)"
write admin_totp_key "$APP_GID" "$(rand_hex 32)"
write admin_api_key "$APP_GID" "$(rand_hex 24)"
write admin_bootstrap_password "$APP_GID" "$(rand_hex 12)"
write wallet_keystore_passphrase "$APP_GID" "$(rand_hex 24)"

# 5. the backup store's credentials: random for --profile backup-local
# (MinIO takes them as its root user), replaced by the real ones for an
# external store
write backup_s3_access_key "$PG_GID" "exchange-backup"
write backup_s3_secret_key "$PG_GID" "$(rand_hex 24)"
chgrp "$PG_GID" "$OUT/backup_s3_access_key" "$OUT/backup_s3_secret_key"

# 6. the JWT signing key
if [ ! -s "$OUT/jwt_private_key" ] || [ "$FORCE" = 1 ]; then
  tmp="$(mktemp)"
  if [ -x bin/exchange ]; then bin/exchange keys gen-jwt --out "$tmp" --force >/dev/null; else go run ./cmd/exchange keys gen-jwt --out "$tmp" --force >/dev/null; fi
  write jwt_private_key "$APP_GID" "$(cat "$tmp")"
  rm -f "$tmp"
fi

# 7. the keystore directory, readable by the signer once the seed is in it
mkdir -p "$OUT/keystore"
chown "root:$APP_GID" "$OUT/keystore"
chmod 0750 "$OUT/keystore"

log "done. Next:"
log "  1. import the mnemonic you generated offline (never anvil's):"
log "       WALLET_KEYSTORE_PASSPHRASE_FILE=$OUT/wallet_keystore_passphrase bin/exchange keys import-mnemonic --from <file> --keystore-dir $OUT/keystore"
log "       chown root:$APP_GID $OUT/keystore/hd-seed.json && chmod 0640 $OUT/keystore/hd-seed.json"
log "     and put the printed hot wallet into .env.prod as HOT_WALLET_ADDRESS"
log "  2. escrow $OUT/ offline, encrypted, with two people (docs/runbooks/backup-restore.md)"
log "  3. the admin bootstrap password is in $OUT/admin_bootstrap_password; change it after the first login"
