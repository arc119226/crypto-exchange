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

# Two independent BIP-44 implementations must agree on m/44'/60'/1'/0/0: the
# `cast` in gen-dev-secrets that produced HOT_WALLET_ADDRESS (and so decided
# which address the contract deployer funded), and the signer's own derivation
# from the encrypted seed. A mismatch means the signer opened a different
# wallet than the stack was set up for.
log "the signer opened the seed the deployer funded"
signer_hot=$("${COMPOSE[@]}" logs --no-color exchange-signer \
  | sed -n 's/.*"hot_wallet":"\([^"]*\)".*/\1/p' | tail -1)
if [ "$signer_hot" != "$HOT_WALLET_ADDRESS" ]; then
  echo "hot wallet mismatch: .env has $HOT_WALLET_ADDRESS, the signer derived ${signer_hot:-<none>}"
  exit 1
fi

log "exchangectl e2e (api -> NATS -> engine)"
"$CTL" e2e --verbose

log "the api hands out a deposit address from the signer's pool"
export EXCHANGE_TOKEN
EXCHANGE_TOKEN=$("$CTL" user register --email "deposit-$STAMP@e2e.local" --password "deposit-$STAMP-pw" --output json | jq -r .access_token)
addr=$("$CTL" deposit-address --asset ETH --output json | jq -r .address)
echo "$addr" | grep -Eq '^0x[0-9a-fA-F]{40}$' || { echo "not an address: $addr"; exit 1; }
# one address per account per chain: the ERC-20 must return the same one
usdc=$("$CTL" deposit-address --asset USDC --output json | jq -r .address)
[ "$addr" = "$usdc" ] || { echo "ETH and USDC gave different addresses: $addr vs $usdc"; exit 1; }
# and it must not be the hot wallet, which is a different BIP-44 account
[ "$addr" != "$HOT_WALLET_ADDRESS" ] || { echo "handed out the hot wallet as a deposit address"; exit 1; }

# The chain half of the flow (docs/plan-v1.0.md §2.3 step 1): real ETH and
# real MockUSDC move on anvil, the chain role sees them, and the balance
# changes. `cast` runs in the same foundry image compose already pins, on the
# host network, because anvil publishes 127.0.0.1:8545.
ANVIL_RPC=http://127.0.0.1:8545
cast_run() {
  docker run --rm --network host --entrypoint cast \
    "ghcr.io/foundry-rs/foundry:${FOUNDRY_TAG}" "$@" --rpc-url "$ANVIL_RPC"
}
# the deployer wrote addresses.json into a volume the app services mount
usdc=$("${COMPOSE[@]}" run --rm --no-deps --entrypoint cat seed /artifacts/addresses.json | jq -r .usdc)
[ -n "$usdc" ] && [ "$usdc" != "null" ] || { echo "could not read the MockUSDC address"; exit 1; }

balance_of() { "$CTL" balances --output json | jq -r --arg a "$1" '.balances[] | select(.asset==$a) | .available' | head -1; }

# wait_for_credit ASSET BEFORE — polls until the balance moves
wait_for_credit() {
  for _ in $(seq 1 45); do
    now=$(balance_of "$1")
    if [ -n "$now" ] && [ "$now" != "$2" ]; then return 0; fi
    sleep 2
  done
  echo "the $1 deposit never reached the balance"
  "$CTL" deposits list || true
  "${COMPOSE[@]}" logs --no-color --tail=50 exchange-chain || true
  return 1
}

log "an on-chain ETH deposit reaches the balance"
before_eth=$(balance_of ETH); before_eth=${before_eth:-0}
sender=$(cast_run rpc eth_accounts | jq -r '.[0]')
# anvil's accounts are unlocked, so the script never handles a key
cast_run rpc eth_sendTransaction "{\"from\":\"$sender\",\"to\":\"$addr\",\"value\":\"0xde0b6b3a7640000\"}" >/dev/null
wait_for_credit ETH "$before_eth"
"$CTL" deposits list --output json | jq -e '.deposits[0].status == "credited"' >/dev/null \
  || { echo "the deposit is recorded but not credited"; "$CTL" deposits list; exit 1; }

log "an on-chain USDC deposit reaches the balance"
before_usdc=$(balance_of USDC); before_usdc=${before_usdc:-0}
cast_run send --unlocked --from "$sender" "$usdc" "transfer(address,uint256)" "$addr" 250500000 >/dev/null
wait_for_credit USDC "$before_usdc"

log "a resting order survives kill -9 of the engine"
session=$("$CTL" user register --email "restart-$STAMP@e2e.local" --password "restart-$STAMP-pw" --output json)
account=$(echo "$session" | jq -r .account_id)
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

# docs/plan-v1.0.md §14: the known development secrets must not appear in any
# container log. The strings are known here because gen-dev-secrets made them,
# which is the only way to assert this honestly — tx and block hashes are hex
# too, so "no 64 hex characters" would be both wrong and useless.
log "no key material in the container logs"
all_logs=$("${COMPOSE[@]}" logs --no-color)
leaked=0
check_absent() { # name value
  [ -n "$2" ] || return 0
  if printf '%s' "$all_logs" | grep -qF -- "$2"; then echo "LEAK: $1 appears in a container log"; leaked=1; fi
}
check_absent WALLET_KEYSTORE_PASSPHRASE "$WALLET_KEYSTORE_PASSPHRASE"
check_absent API_KEY_MASTER_KEY "$API_KEY_MASTER_KEY"
check_absent POSTGRES_PASSWORD "$POSTGRES_PASSWORD"
if [ -f secrets/dev-mnemonic.txt ]; then
  mnemonic=$(tr -d '\n' <secrets/dev-mnemonic.txt)
  check_absent "the dev mnemonic" "$mnemonic"
  check_absent "the dev mnemonic (first three words)" "$(echo "$mnemonic" | cut -d' ' -f1-3)"
fi
[ "$leaked" = "0" ] || exit 1

log "e2e OK"
