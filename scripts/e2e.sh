#!/usr/bin/env bash
# End-to-end test of the split deployment (docs/plan-v1.0.md §12 Phase 3,
# §13.3). CI and `make e2e` run this same script, so a green pipeline means
# the exact flow a developer can reproduce locally.
#
# api, engine and admin are separate containers here, so a trade can only
# reach the engine over the NATS command bus: `exchangectl e2e` passing at
# all is the proof that the bus works. On top of that this checks the two
# things a single-process run cannot show — that the engine recovers from
# kill -9 with its book intact, and that halting a market through the admin
# API reaches the engine as market.updated.
set -euo pipefail

cd "$(dirname "$0")/.."

COMPOSE_FILE=deploy/compose/compose.yaml
ENV_FILE=${ENV_FILE:-.env}
COMPOSE=(docker compose -f "$COMPOSE_FILE" --env-file "$ENV_FILE" --profile infra --profile app)
KEEP=${KEEP:-0}
API_URL=${API_URL:-http://localhost:8080}
ADMIN_URL=${ADMIN_URL:-http://localhost:8082}
STAMP=$(date +%s)

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

cleanup() {
  local code=$?
  if [ "$code" -ne 0 ]; then
    log "container logs"
    "${COMPOSE[@]}" ps || true
    "${COMPOSE[@]}" logs --no-color --tail=200 || true
  fi
  if [ "$KEEP" != "1" ]; then
    "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  fi
  exit "$code"
}
trap cleanup EXIT

# The e2e stack needs secrets but not a real wallet: gen-dev-secrets.sh pulls
# the foundry image only to derive a mnemonic, which no Phase 3 role uses.
if [ ! -f "$ENV_FILE" ]; then
  log "generating $ENV_FILE from .env.example"
  cp .env.example "$ENV_FILE"
  for var in POSTGRES_PASSWORD WALLET_KEYSTORE_PASSPHRASE WEBHOOK_SIGNING_KEY ADMIN_BOOTSTRAP_PASSWORD ADMIN_API_KEY; do
    sed -i "s|^$var=.*|$var=$(openssl rand -hex 16)|" "$ENV_FILE"
  done
  sed -i "s|^API_KEY_MASTER_KEY=.*|API_KEY_MASTER_KEY=$(openssl rand -hex 32)|" "$ENV_FILE"
fi
# shellcheck disable=SC1090
set -a; . "./$ENV_FILE"; set +a

log "building exchangectl"
go build -o bin/exchangectl ./cmd/exchangectl
# Connection settings go through the environment rather than flags: --base-url
# is a root flag but --admin-url/--admin-key belong to the `admin` and `e2e`
# subcommands, so one flag prefix cannot serve every call.
export EXCHANGE_API_URL="$API_URL" EXCHANGE_ADMIN_URL="$ADMIN_URL" EXCHANGE_ADMIN_API_KEY="$ADMIN_API_KEY"
CTL=./bin/exchangectl

log "starting infra + one container per role"
"${COMPOSE[@]}" up -d --build --wait
"${COMPOSE[@]}" ps

log "the engine serves the command bus"
"${COMPOSE[@]}" logs --no-color exchange-engine | grep -q "command bus listening" \
  || { echo "engine did not start the command bus"; exit 1; }

log "exchangectl e2e (api -> NATS -> engine)"
"$CTL" e2e --verbose

log "a resting order survives kill -9 of the engine"
session=$("$CTL" user register --email "restart-$STAMP@e2e.local" --password "restart-$STAMP-pw" --output json)
account=$(echo "$session" | jq -r .account_id)
export EXCHANGE_TOKEN
EXCHANGE_TOKEN=$(echo "$session" | jq -r .access_token)
"$CTL" admin fund --account "$account" --asset USDC --amount 5000 --reason "e2e restart check" >/dev/null
"$CTL" orders place --side buy --price 1000 --qty 0.5 --client-order-id "restart-$STAMP" >/dev/null
before=$("$CTL" book ETH-USDC --output json | jq -S 'del(.last_seq)')

docker kill -s KILL "$("${COMPOSE[@]}" ps -q exchange-engine)"
"${COMPOSE[@]}" up -d --wait exchange-engine
after=""
for _ in $(seq 1 30); do
  after=$("$CTL" book ETH-USDC --output json 2>/dev/null | jq -S 'del(.last_seq)' || true)
  [ -n "$after" ] && break
  sleep 2
done
if [ "$before" != "$after" ]; then
  echo "the book changed across the restart"; echo "before: $before"; echo "after:  ${after:-<none>}"
  exit 1
fi

log "halting the market reaches the engine as market.updated"
"$CTL" admin markets set-status ETH-USDC halted --reason "e2e halt"
halted=0
for i in $(seq 1 30); do
  out=$("$CTL" orders place --side buy --price 1000 --qty 0.01 --client-order-id "halted-$STAMP-$i" --output json 2>/dev/null || true)
  if [ "$(echo "$out" | jq -r '.order.reject_reason // empty' 2>/dev/null)" = "market_not_active" ]; then halted=1; break; fi
  sleep 2
done
[ "$halted" = "1" ] || { echo "the engine kept accepting orders on a halted market"; exit 1; }

# cancels still work while halted (docs/plan-v1.0.md §6.6), and the market
# comes back without a restart
for id in $("$CTL" orders list --open --output json | jq -r '.orders[].id'); do
  "$CTL" orders cancel "$id" >/dev/null
done
"$CTL" admin markets set-status ETH-USDC active --reason "e2e resume"
log "e2e OK"
