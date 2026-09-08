#!/usr/bin/env bash
# Creates what the Helm chart expects to find in a test cluster and never
# renders itself (deploy/helm/exchange/values.yaml, "secrets"): the four
# Secrets with one key per credential, plus two ConfigMaps that hand the
# chart repository files it must not carry copies of (the contract sources
# for anvil, the role-creating initdb script for Postgres).
#
#   scripts/kind-secrets.sh [release] [namespace]
#
# Needs kubectl pointed at the cluster, go (builds bin/exchange), openssl,
# and cast (foundry-toolchain in CI) or docker for the foundry image. Writes
# the admin API key to .kind/<release>/admin-api-key for scripts/helm-e2e.sh.
# Idempotent: every object is applied, not created.
set -euo pipefail
cd "$(dirname "$0")/.."

RELEASE="${1:-exchange}"
NAMESPACE="${2:-default}"
FOUNDRY_TAG="$(sed -n 's/^FOUNDRY_TAG=//p' .env.example)"
ANVIL_DEFAULT_MNEMONIC="test test test test test test test test test test test junk"
OUT=".kind/$RELEASE"
mkdir -p "$OUT"
chmod 700 "$OUT"

log() { printf 'kind-secrets: %s\n' "$*" >&2; }
rand_hex() { openssl rand -hex "$1"; }
apply() { kubectl --namespace "$NAMESPACE" create "$@" --dry-run=client -o yaml | kubectl --namespace "$NAMESPACE" apply -f - >/dev/null; }
cast() {
  if command -v cast >/dev/null 2>&1; then command cast "$@"; else docker run --rm --entrypoint cast "ghcr.io/foundry-rs/foundry:${FOUNDRY_TAG}" "$@"; fi
}

[ -x bin/exchange ] || make build >/dev/null

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# 1. database: one login per role, all with their own password
pg_password="$(rand_hex 16)"
db_args=(--from-literal="POSTGRES_PASSWORD=$pg_password")
for role in migrate api engine chain signer stream admin worker; do
  # the throwaway Postgres shares one password across roles (01-roles.sh
  # falls back to POSTGRES_PASSWORD); a real cluster gives each its own
  db_args+=(--from-literal="DATABASE_URL_$(echo "$role" | tr a-z A-Z)=postgres://ex_${role}:${pg_password}@${RELEASE}-postgres:5432/exchange?sslmode=disable")
done
apply secret generic "$RELEASE-db" "${db_args[@]}"

# 2. JWT signing key
bin/exchange keys gen-jwt --out "$tmp/ed25519.pem" --force >/dev/null
apply secret generic "$RELEASE-jwt" --from-file="ed25519.pem=$tmp/ed25519.pem"

# 3. HD seed for the signer, and the hot wallet address the contract
# deployer funds. Two independent derivations of m/44'/60'/1'/0/0 must agree
# (the same check gen-dev-secrets.sh makes).
phrase="$(cast wallet new-mnemonic --json | sed -n 's/.*"mnemonic": *"\([^"]*\)".*/\1/p')"
[ -n "$phrase" ] || { log "could not parse 'cast wallet new-mnemonic --json'"; exit 1; }
[ "$phrase" != "$ANVIL_DEFAULT_MNEMONIC" ] || { log "refusing anvil's default mnemonic"; exit 1; }
printf '%s\n' "$phrase" >"$tmp/mnemonic.txt"
hot="$(cast wallet address --mnemonic "$phrase" --mnemonic-derivation-path "m/44'/60'/1'/0/0" | tr -d '[:space:]')"
passphrase="$(rand_hex 16)"
derived="$(WALLET_KEYSTORE_PASSPHRASE="$passphrase" bin/exchange keys import-mnemonic --from "$tmp/mnemonic.txt" --keystore-dir "$tmp/keystore" --force | sed -n 's/^hot wallet: //p' | tr -d '[:space:]')"
[ "$derived" = "$hot" ] || { log "hot wallet mismatch: cast says $hot, the keystore derives $derived"; exit 1; }
apply secret generic "$RELEASE-keystore" --from-file="hd-seed.json=$tmp/keystore/hd-seed.json" --from-literal="passphrase=$passphrase"

# 4. application secrets
admin_key="$(rand_hex 16)"
apply secret generic "$RELEASE-app" \
  --from-literal="API_KEY_MASTER_KEY=$(rand_hex 32)" \
  --from-literal="WEBHOOK_SIGNING_KEY=$(rand_hex 32)" \
  --from-literal="ADMIN_TOTP_KEY=$(rand_hex 32)" \
  --from-literal="ADMIN_API_KEY=$admin_key" \
  --from-literal="ADMIN_BOOTSTRAP_PASSWORD=$(rand_hex 16)" \
  --from-literal="NATS_URL=nats://${RELEASE}-nats:4222" \
  --from-literal="HOT_WALLET_ADDRESS=$hot"
umask 077
printf '%s\n' "$admin_key" >"$OUT/admin-api-key"

# 5. repository files the chart's hooks mount
apply configmap "$RELEASE-contracts" \
  --from-file=foundry.toml=infra/contracts/foundry.toml \
  --from-file=MockUSDC.sol=infra/contracts/src/MockUSDC.sol \
  --from-file=Deploy.s.sol=infra/contracts/script/Deploy.s.sol \
  --from-file=Vm.sol=infra/contracts/script/Vm.sol
apply configmap "$RELEASE-initdb" --from-file=01-roles.sh=infra/postgres/initdb/01-roles.sh

log "secrets $RELEASE-{db,jwt,keystore,app} and configmaps $RELEASE-{contracts,initdb} applied in $NAMESPACE; hot wallet $hot; admin key in $OUT/admin-api-key"
