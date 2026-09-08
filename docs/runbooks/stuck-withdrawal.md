# 卡住的提現

提現的前半段(政策、審核、鎖定)全自動,不需要人。**後半段有一個機器故意放棄的點**:一筆交易加價重送 `ETH_MAX_REPLACEMENTS` 次(預設 3)之後還沒被挖出來,worker 就停手,把提現留在 `broadcast`。跟塞住的 mempool 無限對賭不是策略——每一次加價都是真的錢,而且如果原因不是手續費(nonce 缺口、節點壞掉、鏈停了),加再多也沒用。所以到這裡需要一個人判斷:**繼續出價,還是把這筆交易作廢**。

## 症狀

- `withdrawals_stuck_total` 上升(它的定義就是「機器放棄、等人」,值得告警)。
- chain role log 出現 `withdrawal has exhausted its replacements and needs a decision`。
- 使用者回報提現停在 `broadcast` 太久;後台 Withdrawals 頁有一列 `broadcast` 的 `replacements` 等於上限。
- 一整排提現都不動:通常是 nonce 缺口,見處置的最後一段。

## 檢查指令

```sh
# 這筆提現目前的樣子
exchangectl admin withdrawals list --output json | jq '.withdrawals[] | select(.status=="broadcast")'

# 或直接查(chain role 的連線)
psql -c "SELECT id, status, nonce, tx_hash, cancel_tx_hash, replacements,
                broadcast_at, resolve_action, resolve_error
         FROM chain.withdrawals WHERE status = 'broadcast' ORDER BY broadcast_at;"

# 鏈怎麼看這個 nonce
cast tx <tx_hash> --rpc-url "$ETH_RPC_URL"
cast nonce <hot_wallet> --rpc-url "$ETH_RPC_URL"                    # 已挖出的
cast nonce <hot_wallet> --block pending --rpc-url "$ETH_RPC_URL"    # 含 mempool
psql -c "SELECT next_nonce FROM chain.hot_wallets;"
psql -c "SELECT nonce, reason, status, tx_hash FROM chain.nonce_fills ORDER BY nonce DESC LIMIT 10;"
```

| 現象 | 這是什麼 | 動作 |
|---|---|---|
| `replacements < MAX`,`broadcast_at` 是剛剛 | 正常,還在自動重送 | 不用做事 |
| `replacements = MAX`,鏈上手續費仍高於我們的出價 | 純手續費問題 | `resolve bump` |
| `replacements = MAX`,手續費已經回落但交易還在 mempool | 節點沒有重播,或被丟棄了 | `resolve bump`(重簽會產生新的一筆並重新送出) |
| 這個 nonce **之前**還有 nonce 沒被挖 | 缺口,後面全部排隊 | 見「nonce 缺口」 |
| 使用者已經取消 / 位址是錯的 / 就是不該送出去 | 需要作廢 | `resolve cancel_nonce` |
| `status = failed`,`failure_reason = on_chain` | 交易被挖出來但 revert 了,gas 花掉了,金額留在 `pending_withdrawal` | `resolve refund` 或 `resolve retry` |

## 處置

四個處置:

```sh
exchangectl admin withdrawals resolve <id> bump         --note "gas spike, 2025-xx-xx"
exchangectl admin withdrawals resolve <id> cancel_nonce --note "user asked to cancel"
exchangectl admin withdrawals resolve <id> refund       --note "token contract rejected it"
exchangectl admin withdrawals resolve <id> retry        --note "destination fixed"
```

**這四個都是「請求」,不是「立刻執行」。** admin role 沒有節點也沒有金鑰,它只把請求寫在提現列上(`resolve_action` 等四欄);真正執行的是 chain role 的下一個 tick。所以:

- 指令回來得很快**不代表事情做完了**。回頭看 `status` / `cancel_tx_hash`,或看 `withdrawals_resolutions_total`。
- 請求會存活過 chain role 的重啟。這是刻意的:操作員的決定不該死在一個 HTTP 請求裡。
- 一次只能有一個請求在等。已經有一個沒執行完時再送第二個會得到 409,而不是悄悄覆蓋。
- 提現在請求與執行之間變了狀態(例如 bump 送出後它自己確認了),chain role 會**清掉請求並把原因寫進 `resolve_error`**,不會硬做。

### `bump` 做什麼

同一個 nonce、同一個收款人與金額,用**當下**的建議手續費再加 `10 + 10×replacements`%,重新簽(`attempt = replacements + 1`)並送出。錢不動,帳本不動——它還是同一筆提現,只是換了一份出價更高的 bytes。

`ETH_MAX_FEE_PER_GAS` 是操作員的停損。設了它之後 `bump` 也不會超過,所以在極端行情下 bump 可能無效;那時該選 `cancel_nonce`。

### `cancel_nonce` 做什麼,以及為什麼不立刻退款

送一筆**同 nonce 的 0 值自轉**去搶那個 nonce。兩筆交易競爭同一個 nonce,鏈決定誰贏。

**在取代交易被挖出來之前,什麼都不會退。** 原交易仍可能先被挖到——那時錢真的離開了,而我們已經把它退還給使用者,就變成付兩次。所以提現留在 `broadcast`(多一個 `cancel_tx_hash`),等哪一筆先拿到收據:

- 取代交易贏 → `failed(replaced)`,`pending_withdrawal → available` 退回使用者,自轉的 gas 記 `gas_expense`。
- 原交易贏 → 一切照常 `confirmed`,提現就是成功了。

這是 `docs/domain.md` 勘誤 E1 的實作:退款是一筆 `Post`(`pending_withdrawal → available`),不是 `Release`——錢在廣播那一刻就離開 hold 了。

### `refund` / `retry` 只用於 `failed(on_chain)`

交易被挖出來但 revert:gas 花掉了,金額卡在 `pending_withdrawal`。交易所已經不欠使用者這筆 available,也還沒付出去,兩邊都不對——所以機器不猜,由人決定:

- `refund`:金額回 `available`,提現結束。
- `retry`:金額回 `hold`,提現回到 `funds_locked`,用**新的** nonce 再走一次。

對 `broadcast` 的提現用這兩個會得到 409,反之亦然。

### nonce 缺口

一個沒有交易在用的 nonce 會讓它**後面所有**提現都挖不出來,而每一筆看起來都像「手續費太低」。先用檢查指令裡的三個 `cast nonce` / `next_nonce` 確認這才是實際原因。

正常情況下 chain role 會自己補:啟動時掃過所有配出去但鏈沒看到的 nonce,沒有提現持有的就用 0 值自轉填掉,`chain.nonce_fills` 會有一列 `reason = 'startup_gap'`。**重啟 chain role 就會做這件事**,這也是缺口最快的處置方式。

如果重啟後它**拒絕啟動**並說:

```
hotwallet: the chain shows transactions this exchange did not send
```

停下來。這代表鏈上有這個資料庫沒有配過的 nonce,也就是**這把熱錢包金鑰在別的地方也在用**——可能是另一個環境指到了同一個種子,也可能是金鑰外洩(或是剛從備份還原,`docs/runbooks/backup-restore.md` 的 R2/R3)。不要用調參數的方式讓它啟動:它會開始配發別人也在用的 nonce。

1. 比對:`cast nonce <hot_wallet>` 對上 `chain.hot_wallets.next_nonce`,差幾筆。
2. 用 `cast tx` 逐筆看那幾個 nonce 的交易是誰送的、送去哪裡。
3. 是另一個環境誤指同一個種子 → 關掉它,再重啟 chain role。
4. 不是 → 當作金鑰外洩處理:停掉提現(把 signer role 停掉即可,chain role 會停在 `funds_locked` 而不會出錯),換種子(`docs/runbooks/key-rotation.md`),把剩餘資產搬走。

同樣的錯誤也會在**存起來的位址與 signer 推導出來的位址不同**時出現(訊息裡有 `this signer derives`)。那通常是換了種子或改了 derivation path,不是外洩;確認 `WALLET_KEYSTORE_DIR` 掛的是對的 keystore。

## 驗證

- 處置被執行:下一個 chain tick 之後 `resolve_action` 清空、`resolve_error` 為 NULL;`withdrawals_resolutions_total{action}` +1。
- `bump`:`tx_hash` 換了、`replacements` +1,幾個區塊內 `status = confirmed`。
- `cancel_nonce`:`cancel_tx_hash` 有值;之後 `status` 是 `failed`(替代交易贏,使用者的 `available` 回來,`exchangectl balances` 看得到)或 `confirmed`(原交易贏)。兩者都算成功結案。
- `refund` / `retry`:`exchangectl admin entries --limit 5` 看得到對應的分錄(`pending_withdrawal → available` 或 `→ hold`);`retry` 的提現回到 `funds_locked` 並拿到新 nonce。
- nonce 缺口:重啟後 `chain.nonce_fills` 多一列 `startup_gap` 且 `status = confirmed`;`cast nonce --block pending` 等於 `next_nonce`;排在後面的提現開始確認。
- `psql -c "SELECT id, status, resolve_error FROM chain.withdrawals WHERE resolve_error IS NOT NULL;"` 沒有你不認得的錯誤。

## 相關指標

```sh
curl -s localhost:9100/metrics | grep -E 'withdrawals_(stuck|replacements|resolutions|transitions)_total'
```

- `withdrawals_stuck_total`:機器放棄的次數。
- `withdrawals_replacements_total`:重送次數。持續上升代表 gas 估算長期偏低,調 `ETH_REPLACE_AFTER` 或看節點的 `eth_maxPriorityFeePerGas` 建議。
- `withdrawals_resolutions_total{action}`:人工處置的次數。`cancel_nonce` 頻繁出現代表有更上游的問題。
