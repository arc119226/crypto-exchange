#!/usr/bin/env bash
# Creates the development secrets (docs/plan-v1.0.md §11, §14):
#   .env                      from .env.example, CHANGE_ME values replaced with random hex
#   secrets/jwt/ed25519.pem   JWT signing key (exchange keys gen-jwt)
#   secrets/dev-mnemonic.txt  fresh BIP-39 mnemonic from `cast wallet new-mnemonic`
#   HOT_WALLET_ADDRESS        m/44'/60'/1'/0/0 of that mnemonic, written into .env
# Idempotent: existing files are kept; FORCE=1 regenerates everything.
# Requires bash, openssl, go; docker is needed for the mnemonic step (foundry image).
set -euo pipefail
cd "$(dirname "$0")/.."

FORCE="${FORCE:-0}"
FOUNDRY_TAG="$(sed -n 's/^FOUNDRY_TAG=//p' .env.example)"
FOUNDRY_IMAGE="ghcr.io/foundry-rs/foundry:${FOUNDRY_TAG}"
ANVIL_DEFAULT_MNEMONIC="test test test test test test test test test test test junk"

log() { printf 'gen-dev-secrets: %s\n' "$*" >&2; }
rand_hex() { openssl rand -hex 16; }
# set_var FILE KEY VALUE — replace (or append) KEY=... without sed -i (portable to macOS).
set_var() {
  local tmp
  tmp="$(mktemp)"
  awk -v k="$2" -v v="$3" 'index($0, k"=") == 1 { print k"="v; done=1; next } { print } END { if (!done) print k"="v }' "$1" >"$tmp"
  mv "$tmp" "$1"
}
cast() { docker run --rm --entrypoint cast "$FOUNDRY_IMAGE" "$@"; }

mkdir -p secrets/jwt secrets/keystore

# 1. .env
if [[ -f .env && "$FORCE" != 1 ]]; then
  log ".env exists, keeping it (FORCE=1 to regenerate)"
else
  cp .env.example .env
  chmod 600 .env
  for key in POSTGRES_PASSWORD WALLET_KEYSTORE_PASSPHRASE WEBHOOK_SIGNING_KEY ADMIN_BOOTSTRAP_PASSWORD ADMIN_API_KEY; do
    set_var .env "$key" "$(rand_hex)"
  done
  log "wrote .env"
fi

# 2. JWT signing key
if [[ -f secrets/jwt/ed25519.pem && "$FORCE" != 1 ]]; then
  log "secrets/jwt/ed25519.pem exists, keeping it"
else
  go run ./cmd/exchange keys gen-jwt --out secrets/jwt/ed25519.pem --force >/dev/null
  log "wrote secrets/jwt/ed25519.pem"
fi

# 3. mnemonic + hot wallet address (needs docker for the foundry image)
if ! docker info >/dev/null 2>&1; then
  log "WARNING: docker daemon not reachable; skipped the mnemonic. Re-run with docker up, or set HOT_WALLET_ADDRESS in .env by hand before 'make up'."
  exit 0
fi
if [[ -f secrets/dev-mnemonic.txt && "$FORCE" != 1 ]]; then
  log "secrets/dev-mnemonic.txt exists, keeping it"
else
  json="$(cast wallet new-mnemonic --json)"
  phrase="$(printf '%s' "$json" | sed -n 's/.*"mnemonic": *"\([^"]*\)".*/\1/p')"
  [[ -n "$phrase" ]] || { log "could not parse 'cast wallet new-mnemonic --json' output"; exit 1; }
  [[ "$phrase" != "$ANVIL_DEFAULT_MNEMONIC" ]] || { log "refusing anvil's default mnemonic"; exit 1; }
  umask 077
  printf '%s\n' "$phrase" >secrets/dev-mnemonic.txt
  umask 022
  log "wrote secrets/dev-mnemonic.txt (keep it: Phase 4 imports it into the keystore)"
fi
hot="$(cast wallet address --mnemonic "$(<secrets/dev-mnemonic.txt)" --mnemonic-derivation-path "m/44'/60'/1'/0/0" | tr -d '[:space:]')"
[[ "$hot" =~ ^0x[0-9a-fA-F]{40}$ ]] || { log "unexpected address from cast: $hot"; exit 1; }
set_var .env HOT_WALLET_ADDRESS "$hot"
log "HOT_WALLET_ADDRESS=$hot (m/44'/60'/1'/0/0)"
log "done — next: make up-single"
