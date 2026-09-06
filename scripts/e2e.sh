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
COMPOSE=(docker compose -f "$COMPOSE_FILE" --env-file .env --profile infra --profile app)
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

# The same preparation a developer runs (`make gen-dev-secrets && make up`),
# so this exercises the documented path rather than a CI-only one. It is
# idempotent and keeps an existing .env. Hand-rolling it here is what broke
# the job twice: the stack needs more than random passwords — the api role
# reads secrets/jwt/ed25519.pem and the contract deployer refuses the zero
# HOT_WALLET_ADDRESS, both of which this script produces.
log "preparing dev secrets (.env, JWT key, mnemonic)"
scripts/gen-dev-secrets.sh
# shellcheck disable=SC1091
set -a; . ./.env; set +a

# Without a docker daemon gen-dev-secrets warns and skips the mnemonic, which
# leaves the placeholder address the deployer rejects. Say so here instead of
# letting it surface as a Solidity revert inside compose.
if [ "${HOT_WALLET_ADDRESS:-}" = "0x0000000000000000000000000000000000000000" ]; then
  echo "HOT_WALLET_ADDRESS is still the placeholder: gen-dev-secrets could not reach docker" >&2
  exit 1
fi

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
