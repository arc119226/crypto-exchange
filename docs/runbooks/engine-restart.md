# 引擎重啟

engine role 是唯一改訂單簿的程序(單一實例,`docs/plan-v1.0.md` §5.1):它持有 Postgres advisory lock,把命令分組在一筆交易裡提交(Phase 7 的 group commit),記憶體裡的簿永遠等於已提交的狀態,重啟時從資料庫重建。這份手冊寫怎麼安全地重啟它、重啟中外界會看到什麼、以及重啟後怎麼證明簿沒有壞。`kill -9` 也在設計範圍內——`scripts/e2e.sh` 每個 PR 在壓測中砍它一次,`scripts/helm-e2e.sh` 刪它的 pod。

## 症狀

什麼時候要重啟:換版本(`make up-prod` 會做)、engine 的 `readyz` 持續紅(`ExchangeNotReady`)、`trading_command_queue_depth` 一直堆而 `trading_apply_duration_seconds` 沒動(runner 卡住)、`engine_rebuilds_total` 連續上升(每筆交易都失敗後重建——通常是 DB 的問題,不是引擎的)、`trading_batch_fallbacks_total{reason}` 持續上升(整組失敗退回逐筆:`deadlock` / `serialization` 偶爾正常,`error` / `timeout` 持續就是 DB 或網路)。

重啟中外界看到的:api 對下單/撤單回 503(`ErrEngineUnavailable`,cmdbus 5 秒逾時);行情與私有推播停(outbox relay 只在 engine role 跑,`outbox_backlog` 上升,`OutboxBacklog` 5 分鐘後告警);stream role 的簿在 engine 回來後從 DB 重建(`marketdata_book_rebuilds_total`)。這些都在預期內,持續時間 = 停機 + 重建。

## 檢查指令

```sh
# 引擎的狀態
curl -s localhost:9100/readyz                        # engine 檢查:advisory lock 拿到 + 簿重建完成
curl -s localhost:9100/metrics | grep -E '^(engine_seq|engine_open_orders|engine_rebuilds_total|engine_rebuild_duration_seconds_count|trading_command_queue_depth|trading_batch_fallbacks_total|outbox_backlog)'
# 誰持有鎖(重啟卡在「waiting for the engine lock」時)
psql -c "SELECT pid, application_name, client_addr, state, backend_start FROM pg_stat_activity WHERE pid IN (SELECT pid FROM pg_locks WHERE locktype = 'advisory')"
# 簿與資料庫是否一致(每市場)
exchangectl book ETH-USDC --output json | jq '{last_seq, bids: (.bids|length), asks: (.asks|length)}'
psql -c "SELECT m.symbol, s.last_seq, (SELECT max(seq) FROM trading.orders o WHERE o.market_id = s.market_id) AS max_order_seq,
                (SELECT count(*) FROM trading.orders o WHERE o.market_id = s.market_id AND o.status IN ('open','partially_filled')) AS open_orders
         FROM trading.market_sequences s JOIN registry.markets m ON m.id = s.market_id"
# 凍結金額 = open orders 的 hold(壓測與 e2e 用的不變量)
exchangectl admin trial-balance --output json | jq .balanced
```

## 處置

### 正常重啟(換版本、調參數)

1. `docker compose stop exchange-engine`(k8s:`kubectl rollout restart deploy/exchange-engine`)。關機順序是 `internal/app/shutdown.go`:標記 draining(readyz 變紅、LB 停送)→ 等 `SHUTDOWN_DRAIN_DELAY` 2 秒 → 停收命令 → **讓最後一組命令 commit**(最多 10 秒)→ 停 relay(最後一組的 outbox 列先發出去)→ 關 stream。所以 `stop_grace_period` / `terminationGracePeriodSeconds` 是 2 + 10 + 20 + 邊際 = 37–40 秒;**不要調小**,否則 Docker 會在 group commit 中間 `SIGKILL`——那不會弄壞資料(交易要嘛整組 commit 要嘛整組 rollback),但等待中的呼叫者全拿 503。
2. `docker compose up -d exchange-engine`。啟動:等 advisory lock(每秒重試,前一個實例還在關就等它)→ 每個市場一筆 `REPEATABLE READ` 交易讀 `market_sequences.last_seq` + open orders 重建簿(`engine_rebuild_duration_seconds`)→ `readyz` 綠 → 開始收命令。
3. 看驗證段。

### `kill -9`、OOM、節點掛掉

不需要特別處理:Postgres 會在連線斷掉時釋放 advisory lock 並 rollback 未提交的交易;新實例重建的簿就是最後一次 commit 的簿。**唯一的差別是等待中的命令**:api 端的呼叫者拿到 503,客戶端用 `client_order_id` 重送即可(冪等:已提交的回原單,沒提交的重新下)。

### 鎖被別人拿著

`readyz` 一直紅、log 一直 `waiting for the engine lock`:

- 前一個容器還沒關完(正常,最多 40 秒)。
- 另一個 engine 實例在跑(`replicas: 2`、兩個 compose project 指同一個 DB):**這正是鎖存在的理由**,關掉多的那個。chart 對 `roles.engine.replicas > 1` 直接拒絕 render。
- 鎖被一個死掉的連線持有(網路分割):Postgres 在 TCP keepalive / `tcp_user_timeout` 之後才釋放。確認 `pg_stat_activity` 那個 pid 的 `client_addr` 不再存在後 `SELECT pg_terminate_backend(<pid>)`。

### 每筆交易都失敗

`engine_rebuilds_total` 每秒上升、`trading_batch_fallbacks_total{reason="error"}` 跟著:引擎沒壞,是它下面的東西壞了——DB 滿了(`DiskAlmostFull`)、`max_connections`、migration 版本不對(新 image 舊 DB:`migrate` job 先跑)。修那個;引擎每次失敗後重建、下一個命令再試,不需要重啟。

### `ErrSequenceConflict` / `ErrBookInconsistent`

log 出現 `sequence conflict`:交易開始時 `market_sequences.last_seq` 不等於引擎記得的——有另一個寫入者。同上,找出第二個實例。`book inconsistent`(撤單的目標在 DB 是 open 但簿裡沒有):引擎標髒、下個命令前重建,自己會好;持續出現就是 bug,拿 `engine_seq` 與那張單的 `seq` 開 issue。

## 驗證

- `readyz` 綠且 `engine` 檢查為 true;`engine_rebuilds_total` 在重啟後穩定(每次啟動 +1 是正常的)。
- 每個市場:`exchangectl book` 的 `last_seq` == `trading.market_sequences.last_seq` == `max(trading.orders.seq)`;簿的掛單數 == DB 的 open orders 數。這是 `scripts/e2e.sh` 在 `kill -9` 之後斷言的三個等式。
- `exchangectl admin trial-balance` balanced;`ledger.balances.hold` 等於 open orders 的凍結總和(`scripts/e2e.sh` 的 holds 檢查)。
- 下一張單:`POST /v1/orders` 200,`trading_command_queue_depth` 回到 0,`outbox_backlog` 回到 0,stream 客戶端收到 depth delta。
- `trading_batch_fallbacks_total` 不再上升。

## 相關指標

`engine_seq{market}`、`engine_open_orders{market}`、`engine_rebuilds_total`、`engine_rebuild_duration_seconds`、`trading_command_queue_depth{market}`、`trading_apply_duration_seconds{market}`、`trading_batch_size{market}`、`trading_batch_fallbacks_total{market,reason}`、`outbox_backlog`、`exchange_ready{role="engine"}`。
