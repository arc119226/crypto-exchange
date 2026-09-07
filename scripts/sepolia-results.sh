#!/usr/bin/env bash
# Collect the Sepolia walkthrough's result tables (docs/runbooks/sepolia.md §5).
#
# Almost every number §5 asks for is already in the system: deposits, sweeps
# and withdrawals all record their transaction hashes, and the chain will hand
# back a receipt for each. Asking an operator to find eight hashes by hand,
# run `cast receipt` eight times and copy forty-eight cells is how a
# transcription error gets into the record.
#
# What this cannot get is called out rather than left blank:
#
#   - the faucet transaction that funded the hot wallet. It happened outside
#     the exchange, so nothing here ever saw it. Pass FAUCET_TX=0x... to have
#     it looked up anyway.
#   - whether any step's description matched what actually happened. Only the
#     person who walked it knows, and it is the most valuable row in §5.
#
# Usage, from the repository root with the Sepolia stack running:
#
#   scripts/sepolia-results.sh
#   ALICE_PASSWORD='correct horse battery' scripts/sepolia-results.sh
#   FAUCET_TX=0xabc... scripts/sepolia-results.sh
#
# Nothing here writes to the chain or the database; it only reads.
set -uo pipefail   # deliberately not -e: a missing cell must not kill the run

cd "$(dirname "$0")/.." || exit 1

OUT="${OUT:-sepolia-results.md}"
ALICE_EMAIL="${ALICE_EMAIL:-alice@sepolia.test}"
ALICE_PASSWORD="${ALICE_PASSWORD:-}"
FAUCET_TX="${FAUCET_TX:-}"
FOUNDRY_TAG="$(sed -n 's/^FOUNDRY_TAG=//p' .env.example | tr -d '[:space:]')"
FOUNDRY_IMAGE="ghcr.io/foundry-rs/foundry:${FOUNDRY_TAG:-v1.8.1}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
NOTES="$WORK/notes.txt"   # every failure lands here and is printed at the end
: >"$NOTES"

note() { echo "  - $*" >>"$NOTES"; }
say()  { echo "$*" >&2; }

# ---------------------------------------------------------------- prerequisites
for tool in docker python3; do
	command -v "$tool" >/dev/null || { say "需要 $tool,但找不到。裝好再跑一次。"; exit 2; }
done
[[ -f .env ]] || { say "找不到 .env,你在專案資料夾裡嗎?"; exit 2; }

# Anchored at the start of the line: `grep KEY .env` also matches the comments
# that mention the key, which is how a comment fragment once reached `cast`.
RPC="$(sed -n 's/^ETH_RPC_URL=//p' .env | tr -d '[:space:]')"
ADMIN_KEY="$(sed -n 's/^ADMIN_API_KEY=//p' .env | tr -d '[:space:]')"
[[ -n "$RPC" ]] || { say "ETH_RPC_URL 不在 .env 裡。回 A3。"; exit 2; }
[[ -n "$ADMIN_KEY" ]] || { say "ADMIN_API_KEY 不在 .env 裡。"; exit 2; }

export EXCHANGE_ADMIN_URL="${EXCHANGE_ADMIN_URL:-http://localhost:8082}"
export EXCHANGE_ADMIN_API_KEY="$ADMIN_KEY"

# ------------------------------------------------------------------ user token
# The access token lives 15 minutes (AUTH_ACCESS_TTL), and this script runs
# after a walkthrough that takes longer than that: faucet cooldowns, six
# confirmations at twelve seconds, a two-minute sweep cycle. So the token in
# the environment is expired by construction.
#
# Worse, the CLI attaches whatever token it has to *every* request including
# `user login`, and the API rejects a present-but-invalid credential before
# the handler runs -- so an expired token blocks the one call that would
# replace it. Clearing it first is not tidiness, it is the only way out.
unset EXCHANGE_TOKEN
if [[ -n "$ALICE_PASSWORD" ]]; then
	say "登入 $ALICE_EMAIL ..."
	login="$(go run ./cmd/exchangectl user login \
		--email "$ALICE_EMAIL" --password "$ALICE_PASSWORD" 2>&1)"
	token="$(printf '%s\n' "$login" | sed -n 's/^export EXCHANGE_TOKEN=//p' | tr -d '[:space:]')"
	if [[ -n "$token" ]]; then
		export EXCHANGE_TOKEN="$token"
	else
		note "登入失敗,充值與提現兩張表會是空的。CLI 說:$(printf '%s' "$login" | tail -1)"
	fi
else
	note "沒有給 ALICE_PASSWORD,所以沒有登入:充值與提現的列會是空的。重跑時加上 ALICE_PASSWORD='...' 就會自己登入。"
fi

# --------------------------------------------------------------------- collect
# Each call writes its own file. A failure leaves an empty file and a note;
# the table still gets built from whatever did come back.
collect() { # collect <name> <cli args...>
	local name="$1"; shift
	local out="$WORK/$name.json" err="$WORK/$name.err"
	if go run ./cmd/exchangectl "$@" --output json >"$out" 2>"$err"; then
		[[ -s "$out" ]] || note "$name 回了空的內容。"
	else
		note "$name 撈不到:$(grep -v '^exit status' "$err" 2>/dev/null | tail -1 | cut -c1-200)"
		: >"$out"
	fi
}

say "收集交易所這邊的紀錄 ..."
collect sweeps      admin sweeps list
collect reconcile   admin reconcile
if [[ -n "${EXCHANGE_TOKEN:-}" ]]; then
	collect deposits    deposits list
	collect withdrawals withdrawals list
else
	: >"$WORK/deposits.json"; : >"$WORK/withdrawals.json"
fi

# ------------------------------------------------------------------- receipts
# `cast receipt` prints aligned key/value lines:
#
#   blockNumber         11652830
#   effectiveGasPrice   1071972616
#   gasUsed             399930
#
# Parsed as text on purpose. A --json flag may well exist, but this format is
# one we have actually seen; guessing a flag is what made `forge create` print
# its help page and exit 0 earlier in this same runbook.
receipt() { # receipt <txhash> -> writes $WORK/rcpt-<hash>.txt
	local tx="$1"
	[[ "$tx" =~ ^0x[0-9a-fA-F]{64}$ ]] || { note "跳過一個格式不對的交易編號:[$tx]"; return 1; }
	local f="$WORK/rcpt-$tx.txt"
	[[ -s "$f" ]] && return 0
	if ! docker run --rm --entrypoint cast "$FOUNDRY_IMAGE" \
		receipt "$tx" --rpc-url "$RPC" >"$f" 2>"$WORK/rcpt.err"; then
		note "查不到 $tx 的 receipt:$(tail -1 "$WORK/rcpt.err" | cut -c1-160)"
		: >"$f"
		return 1
	fi
}

block_ts() { # block_ts <number> -> writes $WORK/blk-<n>.txt
	local n="$1"
	[[ "$n" =~ ^[0-9]+$ ]] || return 1
	local f="$WORK/blk-$n.txt"
	[[ -s "$f" ]] && return 0
	docker run --rm --entrypoint cast "$FOUNDRY_IMAGE" \
		block "$n" --rpc-url "$RPC" >"$f" 2>/dev/null || { : >"$f"; return 1; }
}

# python picks which hashes matter; bash fetches them. Keeps the selection
# logic in one place instead of splitting it across two languages.
say "挑出要查的交易 ..."
python3 - "$WORK" <<'PY' >"$WORK/wanted.txt"
import json, os, sys
work = sys.argv[1]
def load(name):
    try:
        with open(os.path.join(work, name + ".json")) as f:
            return json.load(f)
    except Exception:
        return {}
want = []
for s in load("sweeps").get("sweeps", []):
    for k in ("tx_hash", "gas_funding_tx_hash"):
        if s.get(k):
            want.append(s[k])
for d in load("deposits").get("deposits", []):
    if d.get("tx_hash"):
        want.append(d["tx_hash"])
for w in load("withdrawals").get("withdrawals", []):
    if w.get("tx_hash"):
        want.append(w["tx_hash"])
seen = set()
for h in want:
    if h not in seen:
        seen.add(h)
        print(h)
PY

[[ -n "$FAUCET_TX" ]] && echo "$FAUCET_TX" >>"$WORK/wanted.txt"

n=0
while read -r tx; do
	[[ -z "$tx" ]] && continue
	n=$((n + 1))
	receipt "$tx"
done <"$WORK/wanted.txt"
say "查了 $n 筆 receipt。"

# Block timestamps, for "on chain -> credited" on the deposit rows.
python3 -c '
import json, os, sys
work = sys.argv[1]
try:
    d = json.load(open(os.path.join(work, "deposits.json")))
except Exception:
    sys.exit()
for x in d.get("deposits", []):
    if x.get("block_number"):
        print(x["block_number"])
' "$WORK" | sort -u | while read -r b; do
	[[ -n "$b" ]] && block_ts "$b"
done

# ---------------------------------------------------------------------- render
say "組表 ..."
python3 - "$WORK" "$FAUCET_TX" <<'PY' >"$WORK/tables.md"
import json, os, re, sys
from datetime import datetime, timezone

work, faucet_tx = sys.argv[1], sys.argv[2]
DASH = "—"

def load(name):
    try:
        with open(os.path.join(work, name + ".json")) as f:
            return json.load(f)
    except Exception:
        return {}

def kv(path):
    """cast prints aligned `key   value` lines; the value may carry a suffix
    such as `1 (success)`, so keep only the first token."""
    out = {}
    try:
        with open(path) as f:
            for line in f:
                parts = line.split()
                if len(parts) >= 2:
                    out[parts[0]] = parts[1]
    except Exception:
        pass
    return out

def rcpt(tx):
    if not tx:
        return {}
    return kv(os.path.join(work, "rcpt-%s.txt" % tx))

def when(s):
    if not s:
        return None
    try:
        return datetime.fromisoformat(s.replace("Z", "+00:00"))
    except ValueError:
        return None

def secs(a, b):
    x, y = when(a), when(b)
    if not x or not y:
        return DASH
    return "%.0f" % (y - x).total_seconds()

def short(tx):
    return "`%s`" % tx if tx else DASH

prices = []

def row(label, tx, lifecycle=DASH):
    r = rcpt(tx)
    blk = r.get("blockNumber", DASH)
    used = r.get("gasUsed")
    price = r.get("effectiveGasPrice")
    cost = DASH
    if used and price:
        try:
            prices.append(int(price))
            cost = "%.9f" % (int(used) * int(price) / 1e18)
        except ValueError:
            pass
    return "| %s | %s | %s | %s | %s | %s | %s |" % (
        label, short(tx), blk, used or DASH, price or DASH, cost, lifecycle)

sweeps = load("sweeps").get("sweeps", [])
deps = load("deposits").get("deposits", [])
wds = load("withdrawals").get("withdrawals", [])

# Newest first from the API; a sweep that failed and was retried leaves two
# rows for one asset, and the confirmed one is the one that moved money.
def pick_sweep(asset):
    ok = [s for s in sweeps if s.get("asset") == asset and s.get("status") == "confirmed"]
    return ok[0] if ok else None

def pick_dep(asset):
    d = [x for x in deps if x.get("asset") == asset]
    return d[0] if d else None

rows = []
rows.append(row("熱錢包 faucet 注資", faucet_tx or None))

for asset, label in (("ETH", "ETH 充值(faucet → 充值地址)"),
                     ("USDC", "USDC 充值(mint → 充值地址)")):
    d = pick_dep(asset)
    life = DASH
    if d:
        blk = d.get("block_number")
        ts = kv(os.path.join(work, "blk-%s.txt" % blk)).get("timestamp") if blk else None
        if ts and d.get("credited_at"):
            c = when(d["credited_at"])
            if c:
                delta = c.timestamp() - int(ts)
                life = "%.0f" % delta
                # Six confirmations at twelve seconds is ~72s; a scanner tick
                # adds a few more. Anything outside this range is not a
                # measurement, it is a symptom -- a mismatched block, or a
                # clock that disagrees. Say so rather than let it be copied
                # into the record as though it meant something.
                if delta < 0 or delta > 3600:
                    life += " ⚠️ 不合理,別直接抄"
    rows.append(row(label, (d or {}).get("tx_hash"), life))

s_eth = pick_sweep("ETH")
rows.append(row("ETH 歸集", (s_eth or {}).get("tx_hash"),
                secs((s_eth or {}).get("created_at"), (s_eth or {}).get("updated_at"))))

s_usdc = pick_sweep("USDC")
rows.append(row("USDC 歸集:補 gas", (s_usdc or {}).get("gas_funding_tx_hash")))
rows.append(row("USDC 歸集:轉帳", (s_usdc or {}).get("tx_hash"),
                secs((s_usdc or {}).get("created_at"), (s_usdc or {}).get("updated_at"))))

# Oldest first reads better here: the auto-approved one was created first.
for w in list(reversed(wds))[:2]:
    kind = "人工審核" if (w.get("review_note") or w.get("status") == "pending_review") else "自動核可"
    rows.append(row("提現(%s)" % kind, w.get("tx_hash"),
                    secs(w.get("created_at"), w.get("updated_at"))))
for _ in range(2 - len(wds)):
    rows.append(row("提現(未進行)", None))

print("| 步驟 | tx hash | block | gas used | effective gas price | 成本 (ETH) | 建立→最後狀態變更(秒) |")
print("|---|---|---|---|---|---|---|")
for r in rows:
    print(r)
print()
print("> 最後一欄是 `updated_at − created_at`,也就是**建立到最後一次狀態變更**,")
print("> 不是「送出到確認」—— 資料庫記的就是前者。充值那兩列是「上鏈到入帳」")
print("> (`credited_at` 減區塊時間),那個才是 §5 觀察表真正問的東西。")
print()

# ---- observations
rec = load("reconcile")
pass_len = secs(rec.get("started_at"), rec.get("finished_at"))
orphaned = [d for d in deps if d.get("status") == "orphaned"]

if prices:
    prices.sort()
    gwei = lambda w: "%.3f" % (w / 1e9)
    gas_note = "%s – %s gwei(中位數 %s)" % (
        gwei(prices[0]), gwei(prices[-1]), gwei(prices[len(prices) // 2]))
else:
    gas_note = DASH

dep_life = [c for c in (r.split("|")[7].strip() for r in rows[1:3]) if c != DASH]

print("| 觀察 | 值 |")
print("|---|---|")
print("| 充值從上鏈到 `credited` 實際花多久 | %s |" %
      (" / ".join("%s 秒" % x for x in dep_life) if dep_life else DASH))
print("| 對帳一輪要多久 | %s |" % ("%s 秒" % pass_len if pass_len != DASH else DASH))
print("| 整段期間 gas 價格大概多少 | %s |" % gas_note)
print("| RPC 有沒有被限流,或出現 `pruned history unavailable` | **要自己看**:`make logs-sepolia TAIL=500 \\| grep -iE 'rate limit\\|pruned\\|429'` |")
if orphaned:
    reorg = "**有**,%d 筆 orphaned" % len(orphaned)
elif deps:
    reorg = "沒有(%d 筆充值紀錄裡沒有 orphaned)" % len(deps)
else:
    # Reporting "no reorg" from an empty list would be a lie of omission: the
    # question was never asked. Say which it was.
    reorg = "**不知道** —— 沒撈到任何充值紀錄,沒有查過"
print("| 有沒有遇到 reorg | %s |" % reorg)
print("| **有沒有哪一步的說明看不懂 / 跟實際不一樣** | **只有你能回答 —— 這一列最重要** |")
print()

if not faucet_tx:
    print("> **faucet 注資那一列是空的**,而且撈不到:那筆交易發生在交易所外面,")
    print("> 系統從來沒看過它。去 Etherscan 找熱錢包的收款紀錄,拿到 hash 之後")
    print("> 重跑一次 `FAUCET_TX=0x... scripts/sepolia-results.sh`。")
    print()
PY

# ------------------------------------------------------------------- assemble
{
	echo "# Sepolia 實測結果"
	echo
	echo "由 \`scripts/sepolia-results.sh\` 產生。填進 docs/runbooks/sepolia.md §5。"
	echo
	cat "$WORK/tables.md"
	if [[ -s "$NOTES" ]]; then
		echo "## 沒撈到的部分"
		echo
		cat "$NOTES"
		echo
	fi
} >"$OUT"

cat "$OUT"
say ""
say "也存了一份到 $OUT —— 整份貼回來就行。"
