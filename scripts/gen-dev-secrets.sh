#!/usr/bin/env bash
# Creates the development secrets (docs/plan-v1.0.md §11, §14):
#   .env                        from .env.example, CHANGE_ME values replaced with random hex
#   secrets/jwt/ed25519.pem     JWT signing key (exchange keys gen-jwt)
#   secrets/dev-mnemonic.txt    fresh BIP-39 mnemonic from `cast wallet new-mnemonic`
#   HOT_WALLET_ADDRESS          m/44'/60'/1'/0/0 of that mnemonic, written into .env
#   secrets/keystore/hd-seed.json  that mnemonic encrypted for the signer role
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
  # 4d renamed ANVIL_DEPLOYER_KEY to CONTRACT_DEPLOYER_KEY: the deployer signs
  # on whatever chain it is pointed at, and on a testnet that key is real. An
  # existing .env would otherwise leave compose interpolating an empty value,
  # and the deploy would fail with something that does not mention the rename.
  if grep -q '^ANVIL_DEPLOYER_KEY=' .env && ! grep -q '^CONTRACT_DEPLOYER_KEY=' .env; then
    set_var .env CONTRACT_DEPLOYER_KEY "$(sed -n 's/^ANVIL_DEPLOYER_KEY=//p' .env)"
    log "renamed ANVIL_DEPLOYER_KEY to CONTRACT_DEPLOYER_KEY in .env"
  fi
  # 5a made WEBHOOK_SIGNING_KEY load-bearing: it is now the AES-256 key that
  # seals each endpoint's signing secret, so it has to be 32 bytes. Nothing
  # read it before, so it was generated at 16 like the passwords, and an
  # existing .env would now fail config validation on every role that starts
  # the worker -- with an accurate message, but only after the container has
  # already exited.
  webhook_key="$(sed -n 's/^WEBHOOK_SIGNING_KEY=//p' .env | tr -d '[:space:]')"
  if [[ ! "$webhook_key" =~ ^[0-9a-fA-F]{64}$ ]]; then
    set_var .env WEBHOOK_SIGNING_KEY "$(openssl rand -hex 32)"
    log "WEBHOOK_SIGNING_KEY was not 32 bytes hex; regenerated it (it now encrypts webhook endpoint secrets)"
  fi
else
  cp .env.example .env
  chmod 600 .env
  for key in POSTGRES_PASSWORD WALLET_KEYSTORE_PASSPHRASE ADMIN_BOOTSTRAP_PASSWORD ADMIN_API_KEY; do
    set_var .env "$key" "$(rand_hex)"
  done
  set_var .env API_KEY_MASTER_KEY "$(openssl rand -hex 32)"    # AES-256 key: 32 bytes
  set_var .env WEBHOOK_SIGNING_KEY "$(openssl rand -hex 32)"   # AES-256 key: 32 bytes
  log "wrote .env"
fi

# 2. JWT signing key
if [[ -f secrets/jwt/ed25519.pem && "$FORCE" != 1 ]]; then
  log "secrets/jwt/ed25519.pem exists, keeping it"
else
  go run ./cmd/exchange keys gen-jwt --out secrets/jwt/ed25519.pem --force >/dev/null
  log "wrote secrets/jwt/ed25519.pem"
fi
# `keys gen-jwt` writes 0600, which is right for a real deployment where the
# key arrives as a K8s Secret owned by the process user. Here the file is bind
# mounted into containers that run as distroless nonroot (uid 65532), so a
# 0600 file owned by whoever ran this script is unreadable to them and the api
# role exits with "read jwt key: permission denied". This key only signs
# development sessions and secrets/ is gitignored. Phase 4 will need the same
# for secrets/keystore/hd-seed.json once the signer reads it.
chmod 0755 secrets/jwt
chmod 0644 secrets/jwt/ed25519.pem

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

# 4. HD seed keystore for the signer role
# Two independent derivations of m/44'/60'/1'/0/0 meet here: `cast` above and
# our own BIP-44 code below. They must agree, or the signer is opening a
# different seed than the deployer funded.
passphrase="$(sed -n 's/^WALLET_KEYSTORE_PASSPHRASE=//p' .env)"
[[ -n "$passphrase" ]] || { log "WALLET_KEYSTORE_PASSPHRASE missing from .env"; exit 1; }
if [[ -f secrets/keystore/hd-seed.json && "$FORCE" != 1 ]]; then
  log "secrets/keystore/hd-seed.json exists, keeping it"
else
  out="$(WALLET_KEYSTORE_PASSPHRASE="$passphrase" go run ./cmd/exchange keys import-mnemonic \
    --from secrets/dev-mnemonic.txt --keystore-dir secrets/keystore --force)"
  derived="$(printf '%s' "$out" | sed -n 's/^hot wallet: //p' | tr -d '[:space:]')"
  if [[ "$derived" != "$hot" ]]; then
    log "hot wallet mismatch: cast says $hot, the keystore derives $derived"
    exit 1
  fi
  log "wrote secrets/keystore/hd-seed.json (hot wallet matches cast)"
fi
# Same reason as the JWT key above: the signer container runs as distroless
# nonroot (uid 65532) and bind mounts this directory, so a 0600 file owned by
# whoever ran this script is unreadable to it. Development seed, gitignored.
chmod 0755 secrets/keystore
chmod 0644 secrets/keystore/hd-seed.json

log "done — next: make up-single"
