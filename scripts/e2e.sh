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
set -Eeuo pipefail

cd "$(dirname "$0")/.."

COMPOSE_FILE=deploy/compose/compose.yaml
COMPOSE=(docker compose -f "$COMPOSE_FILE" --env-file .env --profile infra --profile app)
KEEP=${KEEP:-0}
API_URL=${API_URL:-http://localhost:8080}
ADMIN_URL=${ADMIN_URL:-http://localhost:8082}
STAMP=$(date +%s)

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

# The container dump below is 200 lines from a dozen services, which is more
# than the tail CI shows: an error printed before it scrolls out of reach. So
# remember where the script died and say it last, where it survives.
FAILED_LINE=""
trap 'FAILED_LINE=$LINENO' ERR

cleanup() {
  local code=$?
  if [ "$code" -ne 0 ]; then
    log "container logs"
    "${COMPOSE[@]}" ps || true
    "${COMPOSE[@]}" logs --no-color --tail=200 || true
    log "e2e failed: exit $code at $0:${FAILED_LINE:-?}"
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
"${COMPOSE[@]}" up -d --build --wait --wait-timeout 600
"${COMPOSE[@]}" ps

log "the engine serves the command bus"
# Read the log into a variable before testing it: under pipefail, a pipeline
# whose reader exits early (grep -q, head) fails with 141 when the writer is
# still writing, and `docker compose logs` writes line by line.
engine_logs=$("${COMPOSE[@]}" logs --no-color exchange-engine)
grep -q "command bus listening" <<<"$engine_logs" \
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

# Reconciliation (§6.4.4): what the ledger says the exchange holds on chain
# against what the chain says.
#
# It starts out disagreeing, and it is right to. The deployer funded the hot
# wallet with 100 ETH and a million MockUSDC straight from anvil, and no
# transaction this ledger produced put them there -- so the very first pass
# finds money the ledger knows nothing about. That is exactly what a break is
# for, and the fix is a ledger entry, not a special case in the comparison.
#
# This runs before anything else moves, so the figures are still.
#
# Assumes a clean stack. cleanup() takes the volumes down after every run, so
# `make e2e` gives one; after `KEEP=1 make e2e`, run `make reset` first.
log "reconciliation starts by finding what the ledger was never told about"

# The chain role writes a pass every ETH_RECONCILE_INTERVAL, and cannot write a
# useful one until the scanner's cursor has reached the block the hot wallet
# was funded in -- the frontier is held back to that cursor on purpose. So wait
# for a pass that can actually see the hot wallet rather than reading the first
# one that appears.
wait_for_reconcile_report() {
  for _ in $(seq 1 60); do
    if out=$("$CTL" admin reconcile --output json 2>/dev/null) \
       && echo "$out" | jq -e '[.lines[] | select(.asset=="ETH") | (.chain_total|tonumber) > 0] | any' \
          >/dev/null 2>&1; then
      echo "$out"
      return 0
    fi
    sleep 2
  done
  echo "no reconciliation pass that can see the hot wallet after 120s" >&2
  "$CTL" admin reconcile >&2 || true
  "${COMPOSE[@]}" logs --no-color --tail=80 exchange-chain >&2 || true
  return 1
}

# wait_for_reconciled — polls until the latest pass has nothing left over.
wait_for_reconciled() {
  for _ in $(seq 1 60); do
    if "$CTL" admin reconcile --output json 2>/dev/null | jq -e '.balanced == true' >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  echo "reconciliation never reached zero: $1" >&2
  "$CTL" admin reconcile >&2 || true
  # The report gives the two sides as one number each, which says a difference
  # exists but not which account holds it. The house balances split the ledger
  # side into custody_hot and custody_deposit_addresses, and that is the first
  # thing anyone diagnosing this needs: a difference in the hot wallet and one
  # on the deposit addresses have nothing in common except the arithmetic.
  "$CTL" admin trial-balance >&2 || true
  "${COMPOSE[@]}" logs --no-color --tail=80 exchange-chain >&2 || true
  return 1
}

opening=$(wait_for_reconcile_report)
opening_eth=$(echo "$opening" | jq -r '.lines[] | select(.asset=="ETH") | .diff')
awk -v v="$opening_eth" 'BEGIN { exit !(v + 0 > 0) }' \
  || { echo "the first pass should have found the pre-funded hot wallet, got diff $opening_eth"; exit 1; }

# Book it, the way an operator who has established where it came from would:
# custody gains, external loses (§6.1.4 g). One entry per asset that is over.
unbooked=$(echo "$opening" | jq -r '.lines[] | select((.diff|tonumber) > 0) | "\(.asset)=\(.diff)"')
for line in $unbooked; do
  a=${line%%=*}
  d=${line#*=}
  "$CTL" admin house-adjust --code custody_hot --asset "$a" --amount "$d" --direction credit \
    --reason "the dev chain funded the hot wallet before the exchange took it over" \
    --idempotency-key "e2e-opening:$a" >/dev/null
done
wait_for_reconciled "after booking the hot wallet's opening balance"
log "the opening balance is booked and reconciliation is at zero"

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

# The same address in the two representations this system deliberately keeps.
# GET /v1/deposit-address hands out the EIP-55 checksummed form, because a user
# pastes it into a wallet and the mixed case is what catches a typo. The chain
# tables store it normalised to lower case, so anything read back from the
# admin API or a container log comes out that way. Comparing across the two is
# the one place they meet, and it needs the conversion spelled out rather than
# both sides flattened: flattening would hide a mismatch that is real.
addr_lc=$(echo "$addr" | tr 'A-Z' 'a-z')

# The chain half of the flow (docs/plan-v1.0.md §2.3 step 1): real ETH and
# real MockUSDC move on anvil, the chain role sees them, and the balance
# changes. `cast` runs in the same foundry image compose already pins, on the
# host network, because anvil publishes 127.0.0.1:8545.
ANVIL_RPC=http://127.0.0.1:8545
cast_run() {
  docker run --rm --network host --entrypoint cast \
    "ghcr.io/foundry-rs/foundry:${FOUNDRY_TAG}" "$@" --rpc-url "$ANVIL_RPC"
}
# Where MockUSDC lives. The deployer wrote it to /artifacts/addresses.json, but
# that file cannot be read from a container here: the exchange image is
# distroless, so it holds no `cat` and no shell. Ask the registry instead —
# `exchange seed` put the same address there, so this also proves the chain
# deployer -> addresses.json -> seed -> registry is connected.
usdc_contract=$("$CTL" assets list --output json \
  | jq -r '.assets[] | select(.symbol=="USDC") | .contract_address')
[ -n "$usdc_contract" ] && [ "$usdc_contract" != "null" ] \
  || { echo "the registry has no USDC contract address"; exit 1; }

balance_of() { "$CTL" balances --output json | jq -r --arg a "$1" '[.balances[] | select(.asset==$a) | .available][0] // empty'; }

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
# Deploy.s.sol mints the 1,000,000 USDC to the hot wallet, not to the deployer,
# so `sender` starts with none and a bare transfer would revert. Mint to it
# first: that keeps the deposit itself an ordinary EOA-to-address transfer,
# whose Transfer log carries a real `from` rather than the zero address.
cast_run send --unlocked --from "$sender" "$usdc_contract" "mint(address,uint256)" "$sender" 250500000 >/dev/null
cast_run send --unlocked --from "$sender" "$usdc_contract" "transfer(address,uint256)" "$addr" 250500000 >/dev/null
wait_for_credit USDC "$before_usdc"

# The withdrawal half (docs/plan-v1.0.md §6.4.2), now all the way to the chain:
# the policy decides, the funds are locked, the signer signs over NATS, the
# chain role broadcasts, and the money actually arrives.
withdrawal_status() {
  "$CTL" withdrawals list --output json | jq -r --arg id "$1" '.withdrawals[] | select(.id==$id) | .status'
}

# wait_for_status ID STATUS — polls until the withdrawal reaches it.
#
# Sixty seconds because reaching `confirmed` is five ticks and a block: policy,
# lock, sign, broadcast, receipt. Each is deliberately its own committed step
# (§6.4.2), so the machine is never faster than the tick interval times the
# number of states it has to cross.
wait_for_status() {
  for _ in $(seq 1 60); do
    [ "$(withdrawal_status "$1")" = "$2" ] && return 0
    sleep 1
  done
  echo "withdrawal $1 never reached $2 (now: $(withdrawal_status "$1"))"
  "$CTL" withdrawals list || true
  "${COMPOSE[@]}" logs --no-color --tail=50 exchange-chain || true
  return 1
}

# A destination that has never held anything, unique to this run. Starting
# from zero is what lets the checks below be exact equalities rather than
# "something changed": the balance afterwards is the withdrawal and nothing
# else, on a chain whose state survives restarts.
payout=$(printf '0x%040x' "$STAMP")

# on_chain BALANCE-COMMAND... — the balance as a plain number. Newer foundry
# appends a scientific-notation hint ("50000000000000000 [5e16]"), so take the
# first field either way.
on_chain() { cast_run "$@" | awk 'NR==1 {print $1}'; }

log "a withdrawal inside the limits is signed, sent and confirmed"
# Only terminal states are waited on. `funds_locked` and the hold that goes
# with it last a tick or two before the machine moves on, so polling for them
# from outside is a race the script loses whenever a round trip is slow.
# available is the durable half: it drops when the funds are held and never
# comes back. That the hold entry itself is made is asserted deterministically
# in TestWithdrawalAutoApprovesAndLocksFunds, which drives the ticks by hand.
available_before=$(balance_of ETH)
[ "$(on_chain balance "$payout")" = "0" ] || { echo "$payout is not a fresh address"; exit 1; }
auto=$("$CTL" withdrawals create --asset ETH --amount 0.05 --to "$payout" --output json | jq -r .id)

# Everything past here needs the signer, the nonce manager and a real
# transaction. This is the only check in the suite that can tell a valid
# signature from a plausible one: a wrong one produces a withdrawal that looks
# sent and moves nothing.
wait_for_status "$auto" confirmed
available_after=$(balance_of ETH)
[ "$available_before" != "$available_after" ] || {
  echo "the withdrawal confirmed but the balance did not move: still $available_before"
  "$CTL" balances; exit 1
}
received=$(on_chain balance "$payout")
[ "$received" = "50000000000000000" ] || {
  echo "the withdrawal confirmed but $payout holds $received wei, not 0.05 ETH"
  "$CTL" withdrawals list; exit 1
}
log "the destination really received 0.05 ETH"

# The transaction hash is on the withdrawal, and the chain has mined it.
tx=$("$CTL" withdrawals list --output json | jq -r --arg id "$auto" '.withdrawals[] | select(.id==$id) | .tx_hash')
echo "$tx" | grep -Eq '^0x[0-9a-fA-F]{64}$' || { echo "no tx_hash on the confirmed withdrawal: $tx"; exit 1; }
cast_run receipt "$tx" --json | jq -e '.blockNumber != null' >/dev/null \
  || { echo "the chain has no receipt for $tx"; cast_run receipt "$tx" || true; exit 1; }

# The same key with the same request is the same withdrawal, not a second one.
key="e2e-$STAMP-idem"
first=$("$CTL" withdrawals create --asset ETH --amount 0.02 --to "$sender" --idempotency-key "$key" --output json | jq -r .id)
again=$("$CTL" withdrawals create --asset ETH --amount 0.02 --to "$sender" --idempotency-key "$key" --output json | jq -r .id)
[ "$first" = "$again" ] || { echo "the same Idempotency-Key produced two withdrawals: $first and $again"; exit 1; }

log "a withdrawal over the limit waits for an administrator"
# 0.5 ETH is above the seeded level-0 ceiling of 0.1.
big=$("$CTL" withdrawals create --asset ETH --amount 0.5 --to "$sender" --output json | jq -r .id)
wait_for_status "$big" pending_review
"$CTL" admin withdrawals list --output json | jq -e --arg id "$big" '.withdrawals | map(.id) | index($id) != null' >/dev/null \
  || { echo "the withdrawal is not in the admin review queue"; "$CTL" admin withdrawals list; exit 1; }
"$CTL" admin withdrawals review "$big" approve --note "e2e" >/dev/null
wait_for_status "$big" confirmed

# A USDC withdrawal takes the same path through a contract call, and pays its
# gas in ETH. The hot wallet was minted 1,000,000 USDC by Deploy.s.sol.
log "a token withdrawal moves the token and pays gas in the native coin"
token=$("$CTL" withdrawals create --asset USDC --amount 25 --to "$payout" --output json | jq -r .id)
wait_for_status "$token" confirmed
usdc_received=$(on_chain call "$usdc_contract" "balanceOf(address)(uint256)" "$payout")
# 25 USDC in the token's own 6 decimals. That the number is right and not
# merely non-zero is the check that the scale conversion survived the round
# trip through NUMERIC(36,18).
[ "$usdc_received" = "25000000" ] || {
  echo "the USDC withdrawal confirmed but $payout holds $usdc_received base units, not 25 USDC"
  "$CTL" withdrawals list; exit 1
}

# resolve is refused on a withdrawal that has nothing to resolve, which is the
# 409 an operator sees rather than a silent no-op.
log "resolve refuses an action the withdrawal has outgrown"
if "$CTL" admin withdrawals resolve "$auto" bump --note "e2e" >/dev/null 2>&1; then
  echo "bumping a confirmed withdrawal was accepted"; exit 1
fi

# Collection (§6.4.3): the money the user deposited landed on their own
# address, and the hot wallet has been paying withdrawals out of its own
# balance. Sweeping is what closes that gap.
#
# The sweeper runs on its own clock and starts as soon as a deposit is
# credited, which is long before this line. So what is asserted here is the
# *outcome* -- there are confirmed sweeps and the address is empty -- not the
# act of sweeping, which the script has no way to be present for. Anything
# that needs to observe the steps belongs in test/integration/sweep_test.go,
# where the ticks are driven by hand.
log "deposits are collected into the hot wallet"

# wait_for_sweep ASSET — polls until a confirmed sweep of $addr in that asset
# exists. It usually already does.
wait_for_sweep() {
  for _ in $(seq 1 60); do
    if "$CTL" admin sweeps list --output json \
       | jq -e --arg a "$addr_lc" --arg s "$1" \
         '[.sweeps[] | select(.from_address==$a and .asset==$s and .status=="confirmed")] | length > 0' >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  echo "no confirmed $1 sweep of $addr_lc"
  "$CTL" admin sweeps list || true
  "${COMPOSE[@]}" logs --no-color --tail=80 exchange-chain || true
  return 1
}
wait_for_sweep ETH
wait_for_sweep USDC

# Emptied: what is left is the gas the native sweep had to budget for itself,
# far below the 0.05 ETH threshold that would make another sweep worthwhile.
# The token has no such reserve, so it goes to exactly zero.
addr_eth_left=$(on_chain balance "$addr")
[ "$addr_eth_left" -lt 50000000000000000 ] || {
  echo "the sweep confirmed but $addr still holds $addr_eth_left wei"; exit 1
}
addr_usdc_left=$(on_chain call "$usdc_contract" "balanceOf(address)(uint256)" "$addr")
[ "$addr_usdc_left" = "0" ] || {
  echo "the token sweep confirmed but $addr still holds $addr_usdc_left base units"; exit 1
}
log "the deposit addresses were emptied into the hot wallet"

# The trial balance is the invariant the whole ledger rests on, and sweeping is
# the first thing that writes to two house accounts in one entry.
tb=$("$CTL" admin trial-balance --output json)
echo "$tb" | jq -e '.balanced == true' >/dev/null \
  || { echo "the trial balance is not zero after sweeping"; "$CTL" admin trial-balance; exit 1; }
# And custody never claims a transfer it did not receive: sweeping is capped at
# what the ledger was actually credited for, so this account cannot go negative
# however much turns up on an address.
echo "$tb" | jq -e '[.house[] | select(.code=="custody_deposit_addresses") | (.balance|tonumber) < 0] | any | not' >/dev/null \
  || { echo "custody_deposit_addresses went negative"; "$CTL" admin trial-balance; exit 1; }

# And the whole of it against the chain (§6.4.4, and the Phase 4 DoD).
#
# Nothing is booked here. The opening balance was recorded before any of this
# started, so everything that has happened since -- deposits credited, a trade
# settled, two withdrawals paid and confirmed, both addresses collected into
# the hot wallet, and every wei of gas all of that burned -- had to be booked
# by the exchange itself for this to come back to zero.
#
# That is what makes the assertion worth making: the first pass proved the
# comparison can find money the ledger does not know about, and this one proves
# it has been told about all of it.
log "ledger custody matches the chain"
wait_for_reconciled "after the full deposit, trade, withdraw and collect cycle"
"$CTL" admin reconcile

log "a resting order survives kill -9 of the engine"
session=$("$CTL" user register --email "restart-$STAMP@e2e.local" --password "restart-$STAMP-pw" --output json)
account=$(echo "$session" | jq -r .account_id)
EXCHANGE_TOKEN=$(echo "$session" | jq -r .access_token)
"$CTL" admin fund --account "$account" --asset USDC --amount 5000 --reason "e2e restart check" >/dev/null
"$CTL" orders place --side buy --price 1000 --qty 0.5 --client-order-id "restart-$STAMP" >/dev/null
before=$("$CTL" book ETH-USDC --output json | jq -S 'del(.last_seq)')

docker kill -s KILL "$("${COMPOSE[@]}" ps -q exchange-engine)"
"${COMPOSE[@]}" up -d --wait --wait-timeout 180 exchange-engine
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

log "kill -9 in the middle of a burst leaves the book equal to the database"
# The engine commits commands in groups (docs/plan-v1.0.md §5.2 group commit);
# a group cut off mid-transaction must roll back as a whole, and the restarted
# book must be exactly what the database says. Ten seconds of load, the
# engine killed a few seconds in: the generator sees 503s for a moment
# (callers waiting on the killed transaction) and carries on.
burst_dir=$(mktemp -d)
EXCHANGE_API_URL="$API_URL" EXCHANGE_WS_URL="${WS_URL:-ws://localhost:8081}" EXCHANGE_ADMIN_URL="$ADMIN_URL" EXCHANGE_ADMIN_API_KEY="$ADMIN_API_KEY" \
  "$CTL" loadgen --market ETH-USDC --rate 200 --duration 10s --accounts 10 --ws-clients 0 --private-clients 0 \
  --output json > "$burst_dir/burst.json" 2> "$burst_dir/burst.err" &
burst=$!
sleep 4
docker kill -s KILL "$("${COMPOSE[@]}" ps -q exchange-engine)"
"${COMPOSE[@]}" up -d --wait --wait-timeout 180 exchange-engine
wait "$burst" || { echo "loadgen failed during the burst"; cat "$burst_dir/burst.err"; exit 1; }
jq '{orders_sent, orders_ok, unavailable_503, errors}' "$burst_dir/burst.json"
psql_check() {
  "${COMPOSE[@]}" exec -T postgres psql -U exchange -d exchange -Atc "$1"
}
# The market sequence against what was committed. An order row carries the
# seq of the command that accepted or rejected it, but a cancel consumes a
# seq and writes no order row (the loadgen cancels every third order), so
# the orders are only a lower bound; the market's newest outbox event is
# the exact witness (the same invariant as scripts/sql/restore-checks.sql,
# docs/domain.md §26.3).
seq_db=$(psql_check "SELECT s.last_seq || ' ' || greatest(COALESCE((SELECT max(o.seq) FROM trading.orders o WHERE o.market_id = s.market_id), 0), COALESCE((SELECT max(e.seq) FROM eventbus.outbox e WHERE e.market_id = m.symbol), 0)) || ' ' || COALESCE((SELECT max(o.seq) FROM trading.orders o WHERE o.market_id = s.market_id), 0) FROM trading.market_sequences s JOIN registry.markets m ON m.id = s.market_id WHERE m.symbol = 'ETH-USDC'")
read -r last_seq witnessed orders_max <<<"$seq_db"
[ -n "$last_seq" ] && [ "$last_seq" = "$witnessed" ] && [ "$last_seq" -ge "$orders_max" ] ||
  { echo "market sequence, orders and events disagree after the burst: last_seq=$last_seq newest event or order=$witnessed max(orders.seq)=$orders_max"; exit 1; }
book_seq=$("$CTL" book ETH-USDC --output json | jq -r .last_seq)
[ "$book_seq" = "$last_seq" ] || { echo "the restarted book is at seq $book_seq, the database at $last_seq"; exit 1; }
holds=$(psql_check "SELECT count(*) FROM (SELECT b.account_id, b.asset, b.hold, COALESCE(sum(o.hold_remaining), 0) AS held FROM ledger.balances b LEFT JOIN trading.orders o ON o.account_id = b.account_id AND o.hold_asset = b.asset AND o.status IN ('open', 'partially_filled') GROUP BY 1, 2, 3) x WHERE hold <> held")
[ "$holds" = "0" ] || { echo "$holds balances disagree with the open orders' holds after the burst"; exit 1; }
"$CTL" admin trial-balance --output json | jq -e '.balanced == true' >/dev/null || { echo "trial balance broke during the burst"; exit 1; }
for id in $("$CTL" orders list --open --output json | jq -r '.orders[].id'); do
  "$CTL" orders cancel "$id" >/dev/null
done

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
  # a here-string, not a pipeline: `printf | grep -q` has the same shape as
  # the race above (a found leak could read as "no leak" if printf were
  # still writing when grep exits), so it is written the safe way too
  if grep -qF -- "$2" <<<"$all_logs"; then echo "LEAK: $1 appears in a container log"; leaked=1; fi
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
