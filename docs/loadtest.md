# 壓測報告(Phase 6,本機單機)

`docs/plan-v1.0.md` §3.3 的規模目標是設計目標,不是承諾;Phase 6 的 DoD 是「壓測結果達 §3.3 目標或記錄差距」。這份文件記錄 2026-09-08 用 `exchangectl loadgen` 在本機跑出來的數字、與目標的差距、瓶頸在哪、以及日後怎麼補。數字只對這台機器與這個設定有效;換環境請重跑,不要引用這裡的絕對值。

## 1. 結論(先看這段)

| §3.3 目標 | 量到 | 達標 |
|---|---|---|
| 同時 WS 連線 1,000 | 1,030 條連線(1,000 閒置 + 20 公開訂閱 + 10 私有)在 A、B 兩組各撐滿 60 s,0 條提前斷線;run 中取樣伺服器 3,343 goroutine、1,184 fd、RSS 144 MB | ✓ |
| 成交 → 私有 WS 推播 p99 < 200 ms | 客戶端量到 p99 24–26 ms;伺服器端 `stream_push_delay_seconds{channel="orders"}` p99 ≤ 25 ms | ✓ |
| 成交 → 公開 depth delta p99 < 300 ms | 20 個訂閱者:p99 19–21 ms;500 個訂閱者(46,000 則/s 扇出):p99 44 ms | ✓ |
| `POST /v1/orders` p99 < 50 ms | 飽和時 1,183 ms;100 orders/s 穩態時 1,244 ms;10 帳戶輕載時 144 ms | ✗ |
| 單市場 ≥ 1,000 orders/s 持續 60 s 不積壓 | 單市場飽和在 **126 orders/s + 41 cancels/s ≈ 165 命令/s** | ✗ |

行情與推播這半邊(Phase 6 做的東西)全部達標而且餘裕很大;沒達標的兩項都是同一個原因:**引擎每個命令一筆 Postgres 交易,而這筆交易平均要 6.9 ms**,單市場的 runner 是序列的,所以上限就是 1 / 6.9 ms ≈ 145–165 命令/s。§3.3 當初就預告了這件事(「每命令一筆 PG 交易的上限 ≈ 1 / 單筆交易延遲;達標手段為 runner 內 group commit」),Phase 3 量過單命令延遲後決定先不做 group commit;這次的數字把差距量化了,也把「6.9 ms 花在哪」弄清楚了(§5)。

**Phase 7 之後**:引擎改成 pipeline + 群組提交,同一套壓測飽和點從 165 命令/s 到 355–442,`POST` p50 好 2.5×;推播延遲在飽和時變差(事件等該組 COMMIT)。數字與讀法在 §8,§4–§5 保留為 Phase 6 的對照。

## 2. 環境

| 項目 | 值 |
|---|---|
| 機器 | 4 vCPU(Intel Xeon @ 2.80 GHz)、16 GB RAM、Linux 6.18;`ulimit -n 20000` |
| 程式 | 本分支的 `bin/exchange`、`bin/exchangectl`(Go 1.26.8);`exchange serve --role=api,engine,stream,worker,admin` **單一 process** |
| Postgres | 16.13,本機 5433,預設設定(`shared_buffers 128MB`、`synchronous_commit on`、`fsync on`);DB `exchange_loadgen`,連線角色 `ex_all`,`DATABASE_MAX_CONNS=40` |
| NATS | nats-server v2.14.6,JetStream file store,與 Postgres 同一顆磁碟 |
| Redis | **沒有**(`REDIS_ADDR` 空:限流用記憶體,`GET /depth` 走引擎;壓測沒打 `GET /depth`) |
| 設定 | `EXCHANGE_ENV=dev`、`STREAM_ALLOWED_ORIGINS=*`、`JWT_JWKS_URL=http://127.0.0.1:8080/.well-known/jwks.json`、`RATELIMIT_LOGIN_PER_IP=2000/1m`(註冊 100 個帳戶會撞預設的 10/1m),其餘預設(`RATELIMIT_ORDERS_PER_ACCOUNT=20/1s`、`STREAM_WRITE_BUFFER=256`、`STREAM_FLUSH_INTERVAL=50ms`) |
| 產生器 | `exchangectl loadgen` 跑在**同一台機器**(跟伺服器搶 4 顆 vCPU;好處是同一個時鐘,`occurred_at`/`at` → 收到的推播延遲才量得準) |
| 起始狀態 | `exchange migrate up && exchange seed && exchange admin bootstrap`;第一組 run 之前簿上有 1,215 個 seq 的歷史;每組 run 都會留下新的掛單(見 §6) |

沒有 anvil、沒有 Docker:壓測只打交易路徑,鏈上部分不在範圍。

## 3. 方法

`exchangectl loadgen` 的作法(`cmd/exchangectl/loadgen.go`):

1. 註冊 `--accounts` 個帳戶,用 admin API 的 dev faucet 注資(每帳戶 1,000,000 USDC + 1,000 ETH)。
2. 每個帳戶一個 goroutine,以 `--rate / --accounts` 的節奏送限價單:價格在 `--mid ± rand(1..100)% × --spread` 內,每 `--cross-every`(預設 5)張穿越價差變成 taker,每 `--cancel-every`(預設 3)張先取消一張自己最舊的掛單。**每個帳戶同時只有一個請求在飛**(closed loop),所以同時在飛的請求數 ≤ `--accounts`;伺服器飽和時量到的延遲 ≈ 佇列深度 × 服務時間,而佇列深度會停在 `--accounts`。
3. `--ws-clients` 個公開連線訂閱 `depth` + `trades`,`--private-clients` 個私有連線 auth 後收全部私有頻道,`--idle-connections` 個只連上、只回 ping 的閒置連線。
4. 量:`POST /v1/orders` 與 `DELETE` 的客戶端延遲(nearest-rank p50/p95/p99/max)、orders/s、trades/s(從 `trades` 頻道)、depth delta / trade / 私有推播的延遲(訊息的 `at` 或 `occurred_at` → 收到)、depth `seq` 是否連續、提前斷線數。429 與 503 分開計。
5. 伺服器端另外抄 `GET /metrics` 兩次(run 前後)取直方圖差值:`trading_apply_duration_seconds`(引擎一個下單命令含 DB 交易)、`stream_push_delay_seconds{channel}`(事件 `occurred_at` → 進連線的送出佇列)、`http_request_duration_seconds`。

跑法(§7 有完整指令):

```sh
exchangectl loadgen --market ETH-USDC --rate 1000 --duration 60s --accounts 100 \
  --ws-clients 20 --private-clients 10 --idle-connections 1000 --output json
```

## 4. 數字

四組 run。A 是 DoD 要的那組;B 是「目標速率 100 orders/s」的穩態;C 把 `synchronous_commit` 關掉看 fsync 佔多少;D 把公開訂閱者拉到 500 看扇出上限。

### 4.1 產生器看到的

| | A:飽和 | B:穩態 100/s | C:同 A,`synchronous_commit=off` | D:扇出 500 訂閱者 |
|---|---|---|---|---|
| 參數 | rate 1000、60 s、100 帳戶、20/10/1000 | rate 100、60 s、100 帳戶、20/10/1000 | rate 1000、30 s、100 帳戶、5/5/0 | rate 1000、30 s、100 帳戶、500/10/0 |
| orders 送出 / 成功 | 7,414 / 7,414 | 5,900 / 5,900 | 4,137 / 4,137 | 2,076 / 2,076 |
| 429 / 503 / 其他錯 | 0 / 0 / 0 | 0 / 0 / 0 | 0 / 0 / 0 | 0 / 0 / 0 |
| cancels | 2,458 | 1,900 | 1,360 | 697 |
| **orders/s** | **126.0** | 98.3 | 137.9 | 69.2 |
| trades/s | 24.0 | 18.3 | 26.4 | 13.3 |
| `POST /v1/orders` p50 / p95 / p99 / max (ms) | 628 / 828 / **1,183** / 1,443 | 475 / 1,053 / **1,244** / 1,459 | 510 / 912 / 1,169 / 1,303 | 967 / 1,765 / 2,063 / 2,177 |
| `DELETE /v1/orders/{id}` p50 / p99 (ms) | 592 / 1,146 | 275 / 719 | — | — |
| depth delta 收到數 / seq gap | 187,720 / 0 | 148,100 / 0 | 26,695 / 0 | **1,384,500** / 0 |
| depth delta 延遲 p50 / p95 / p99 / max (ms) | 6.6 / 15.9 / **19.0** / 56 | 6.4 / 16.7 / **20.6** / 120 | 5.6 / 15.3 / 17.9 / 57 | 16.1 / 34.9 / **44.2** / 73 |
| trade 推播延遲 p50 / p99 (ms) | 13.8 / 22.4 | 14.6 / 25.2 | — | 26.6 / 50.3 |
| 私有推播延遲 p50 / p95 / p99 / max (ms) | 8.1 / 18.3 / **24.2** / 477 | 7.4 / 18.5 / **26.4** / 55 | 6.4 / 17.5 / 27.6 / 167 | 18.9 / 40.4 / 326 / 478 |
| 私有 frame 收到數 | 2,898 | 2,180 | 778 | 810 |
| 提前斷線 | 0 | 0 | 0 | 0 |

每個 seq 恰好一則 delta:A 的引擎 seq 從 1,215 走到 10,601(9,386 個命令),20 個訂閱者各收到 9,386 則,共 187,720,無 gap;B、D 同樣對得起來(D:2,769 × 500 = 1,384,500)。

### 4.2 伺服器看到的(`/metrics` 差值)

| | A | B | C | D |
|---|---|---|---|---|
| `trading_apply_duration_seconds` 平均 / 樣本 | **6.87 ms** / 7,493 | 6.96 ms / 5,900 | **6.11 ms** / 4,210 | 8.6 ms / 2,172 |
| 同上 p50 ≤ / p99 ≤(桶界) | 10 / 25 ms | 10 / 25 ms | 5 / 25 ms | 10 / 50 ms |
| `stream_push_delay_seconds{depth}` p99 ≤ | 25 ms | 25 ms | 25 ms | 50 ms |
| `stream_push_delay_seconds{orders}` p99 ≤ | 25 ms | 25 ms | 25 ms | 50 ms |
| `trading_command_queue_depth`(run 中取樣) | — | 97 | — | — |
| `ws_connections`(run 中取樣) | — | public 1,020、private 10 | — | public 500、private 10 |
| `go_goroutines` / `process_open_fds`(run 中) | — | 3,343 / 1,184 | — | 1,787 / — |
| RSS(run 後) | 147 MB | 152 MB | 63 MB | 115 MB |
| exchange process CPU(整個 run) | 74 s / 85 s wall | 63 s | 48 s / 40 s | 66 s / 38 s |
| `ws_messages_sent_total{depth}` | 187,720 | 148,100 | 26,695 | 1,384,500 |
| `ws_slow_client_disconnects_total` | 0 | 0 | 0 | 0 |
| `event_consumer_lag`(全部 consumer)/ `outbox_backlog` / `marketdata_kline_writer_lag` | 0 / 0 / 0 | 0 / 0 / 0 | 0 / 0 / 0 | 0 / 0 / 0 |
| `marketdata_book_rebuilds_total` | 1(`reason="start"`) | 0 新增 | — | — |

D 那組 `top` 的快照:exchange 150% CPU、exchangectl 140%、nats-server 20%、postgres 兩個 backend 各 10%,整機 84% busy —— 500 個訂閱者的解碼跟伺服器搶同一顆機器,所以 D 的下單延遲與吞吐比 A 差,不能拿來當伺服器的數字;它證明的是 46,000 則/s 的扇出下 p99 仍 44 ms、沒有慢客戶端被踢。

## 5. 瓶頸在哪:每命令 6.9 ms 的 DB 交易,而且不是 fsync

1. **飽和點就是引擎的序列交易。** A 組 exchange process 只用了 ~0.9 顆 CPU、Postgres 各 backend 10%、`http_requests_in_flight` 與 `trading_command_queue_depth` 都停在 ≈ 帳戶數(B 組取樣 97):請求在市場 runner 前面排隊。165 命令/s × 6.9 ms ≈ 1.1 s → 每秒都是滿的。
2. **fsync 只佔一成。** C 組把 `synchronous_commit` 關掉,平均只從 6.87 ms 降到 6.11 ms(吞吐 126 → 138 orders/s)。剩下的 6 ms 是**交易裡的往返次數**:一張純掛單的命令依序做 `getOrderByClientID`、`ledger.Account`、`BEGIN`、savepoint、`ledger.Hold`(分錄一列、鎖餘額、兩筆 postings、更新餘額:5 次)、`InsertOrder`、`UpdateOrderProgress`、每個帳戶事件一次 `BumpAccountSeq`、每個事件一次 `outbox.Append`(掛單至少 `order.accepted` + `balance.updated`,各兩次)、`advanceSeq`、`COMMIT`,約 17 次往返;有成交再加上每筆成交的 settle(分錄、四個餘額鍵、六筆 postings)與 maker 的 release / update。本機 TCP 往返加執行每次 0.3–0.4 ms,乘起來就是 6 ms。
3. **限流不是因素**:每帳戶 20/1s,100 個帳戶上限 2,000/s;整個壓測 0 個 429。
4. **推播那一邊有很大的餘裕**:A 組 depth p99 19 ms 裡,伺服器端 `stream_push_delay`(事件落地 → 進送出佇列)p99 ≤ 25 ms 桶,其中含 outbox relay 的批次(≤ 100 列或 LISTEN/NOTIFY)與投影器的 50 ms flush 保底(實際幾乎都在 taker 終態事件到時就收斂,用不到保底)。D 組 500 訂閱者只把 p99 推到 44 ms。

### 怎麼補(不在 Phase 6 範圍,記錄給 Phase 7)

按 §3.3 原先的規劃,以及這次量到的分解,依序:

1. **減少往返**:`pgx.Batch` 把一個命令裡互不依賴的 INSERT/UPDATE(postings、trades、outbox 列、order progress)打包成一次往返(pipeline);同一帳戶同一命令的多個事件一次 `next_seq + n` 取號而不是逐事件 bump;`getOrderByClientID` 與 `ledger.Account` 併成一次。估計 17 次 → 6–8 次往返,6.9 ms → 3 ms 上下,吞吐 ×2。純程式改動,不改交易邊界、不改事件契約。
2. **runner 內 group commit**(§3.3 提的 N ≤ 50):一筆交易套用佇列裡累積的 N 個命令,回覆在 commit 後一起發。fsync 與交易固定成本攤掉;單筆延遲在低載時不變(佇列空時 N = 1)。要動 `persistAccepted` 的錯誤處理(一個命令失敗要能整批回滾再逐一重做),配 rapid 測試。做完才有機會碰到 1,000 orders/s。
3. **Postgres 端**:`synchronous_commit=off` 只值 10%,不做(帳本不能冒 crash 掉最後幾筆 commit 的險)。unix socket 取代 TCP loopback 可以省一點往返,部署時順手。

`POST p99 < 50 ms` 要等 1 或 2 做完再量:現在 p99 被佇列吃掉,服務時間本身的 p99(`trading_apply_duration_seconds` ≤ 25 ms 桶)已經在門檻附近。

## 6. 觀察到的小事(已處理或記錄)

- **註冊會撞 `RATELIMIT_LOGIN_PER_IP`**(預設 10/1m 同時管註冊):`--accounts 100` 從同一個 IP 註冊第 11 個就 429;壓測環境要放寬(這裡 2000/1m),`loadgen` 的錯誤訊息會直接印出 429。
- **run 結束時飛行中的請求**:產生器把 context 取消,伺服器看到的是 client 中途斷線 → cmdbus 或 DB 查詢回 `context canceled` → 記成 503(下單)或 500(取消)並打 ERROR log(每個帳戶一個,A 組 79 + 21 = 100 筆)。這不是伺服器的錯,但「客戶端斷線」被算成 5xx 會弄髒儀表板;候選修法是在 api 的錯誤處理把 `r.Context().Err() != nil` 的情況降成 499/debug。產生器這邊已改成不把這些請求算進送出數。
- **簿會越跑越厚**:每組 run 留下未取消的掛單(A 結束 3,072、B 結束 5,271 個 open orders),`--cancel-every 3` 只追蹤最近 50 張。`engine_open_orders` 從 1,215 到 5,271 之間 `trading_apply_duration` 沒有明顯變化(6.87 → 6.96 ms),撮合本身不是瓶頸(`matching` 的 benchmark 另計)。
- **私有推播 max 477 ms**(A 組、D 組各一個樣本;p99 仍 24 ms):沒有追到單一樣本的成因。候選解釋是 auth 之後的 hold 視窗(`STREAM_RESUME_WINDOW` = 1 s):客戶端沒送 `resume`,第一批 frame 要等視窗到期或第一個 op 才放行,量到的就是 hold 的時間而不是推播延遲;要確認得在產生器記下 frame 是不是 auth 後的第一批。

## 7. 重現

```sh
# 一次性:本機 Postgres 5433(角色 ex_*)、nats-server(go install github.com/nats-io/nats-server/v2@v2.14.6)
nats-server -js -sd /tmp/nats -p 4222 &
export EXCHANGE_ENV=dev TENANT_ID=default \
  DATABASE_URL='postgres://ex_all:test@127.0.0.1:5433/exchange_loadgen?sslmode=disable' DATABASE_MAX_CONNS=40 \
  NATS_URL=nats://127.0.0.1:4222 HTTP_ADDR=:8080 WS_ADDR=:8081 ADMIN_ADDR=:8082 OPS_ADDR=:9100 \
  JWT_PRIVATE_KEY_FILE=secrets/jwt/ed25519.pem JWT_JWKS_URL=http://127.0.0.1:8080/.well-known/jwks.json \
  STREAM_ALLOWED_ORIGINS='*' RATELIMIT_LOGIN_PER_IP=2000/1m \
  API_KEY_MASTER_KEY=… ADMIN_API_KEY=… ADMIN_TOTP_KEY=… WEBHOOK_SIGNING_KEY=… \
  ADMIN_BOOTSTRAP_EMAIL=admin@loadgen.local ADMIN_BOOTSTRAP_PASSWORD=…
make build
bin/exchange migrate up && bin/exchange seed && bin/exchange admin bootstrap
bin/exchange serve --role=api,engine,stream,worker,admin &

curl -s http://127.0.0.1:9100/metrics > metrics-before.txt
EXCHANGE_API_URL=http://127.0.0.1:8080 EXCHANGE_WS_URL=ws://127.0.0.1:8081 \
EXCHANGE_ADMIN_URL=http://127.0.0.1:8082 EXCHANGE_ADMIN_API_KEY="$ADMIN_API_KEY" \
bin/exchangectl loadgen --market ETH-USDC --rate 1000 --duration 60s --accounts 100 \
  --ws-clients 20 --private-clients 10 --idle-connections 1000 --output json > report.json
curl -s http://127.0.0.1:9100/metrics > metrics-after.txt
```

`make loadgen` 跑的是同一組參數(對 compose 的 stack 則用 `.env` 的 `ADMIN_API_KEY`)。四個祕密用 `openssl rand -hex 32` 產生,不要用 `.env.example` 的占位值。§4.2 的伺服器端數字是兩份 `/metrics` 的直方圖差值;`stream_push_delay_seconds` 與 `http_request_duration_seconds` 的 p99 只能給到桶界(0.005 … 2.5 s),要精確值請看 Grafana 的 `histogram_quantile`。

## 8. Phase 7 之後:同一套壓測再跑一次

§5 的兩件事都做了(Phase 7 的 `ledger: post an entry in two round trips`、`trading: commit commands in groups over pipelined round trips`),同一台機器、同一組參數再量。環境同 §2,只差引擎的 `ENGINE_BATCH_SIZE=50`(預設)。

### 8.1 每命令的往返數(整合測試 `TestEngineRoundTripBudget` 釘住的數字)

| 命令 | Phase 6 | Phase 7 |
|---|---|---|
| 純掛單 | ≈ 17 | **3** |
| 一筆成交 | 45–55 | **4** |
| 取消 | ≈ 9 | **3** |
| 進簿後拒絕 / 餘額不足 | ≈ 5 | 3 |
| client_order_id 重送 | 2 | 4 |

一組 n 個命令的交易:`BEGIN` + 全部命令的預讀 + 序號鎖 + 第一個命令的鎖 → 每個命令的寫入跟下一個命令的鎖同一趟 → 最後一個命令的寫入 + 帳戶序號 → outbox 列 + 市場序號 + `COMMIT`。滿的一組 50 個純掛單約 53 次往返。

### 8.2 產生器看到的

| | A':飽和(對照 §4 A) | B':穩態 100/s(對照 §4 B) | E:只有引擎,無推播訂閱者 |
|---|---|---|---|
| 參數 | rate 1000、60 s、100 帳戶、20/10/1000 | rate 100、60 s、100 帳戶、20/10/1000 | rate 1000、30 s、100 帳戶、0/0/0 |
| orders 送出 / 成功 | 15,994 / 15,994(A:7,414) | 5,900 / 5,900 | 9,954 / 9,954 |
| 429 / 503 / 其他錯 | 0 / 0 / 0 | 0 / 0 / 0 | 0 / 0 / 0 |
| cancels | 5,316 | 1,900 | 3,307 |
| **orders/s** | **266.5**(A:126.0,×2.1) | 98.3 | **331.8** |
| 命令/s(orders + cancels) | **355**(A:165) | 130 | **442** |
| trades/s | 52.4(A:24.0) | 18.2 | 0(單邊掛單) |
| `POST /v1/orders` p50 / p95 / p99 / max (ms) | 248 / 651 / 1,127 / 1,848(A:628 / 828 / 1,183 / 1,443) | 179 / 415 / **548** / 606(B:475 / 1,053 / 1,244 / 1,459) | 232 / 326 / 395 / 423 |
| `DELETE /v1/orders/{id}` p50 / p99 (ms) | 234 / 1,108 | 103 / 278 | 216 / 386 |
| depth delta 收到數 / seq gap | 401,580 / 0 | 148,060 / 0 | — |
| depth delta 延遲 p50 / p99 (ms) | 177 / **638**(A:6.6 / 19.0) | 122 / **445**(B:6.4 / 20.6) | — |
| 私有推播延遲 p50 / p99 (ms) | 182 / 648(A:8.1 / 24.2) | 148 / 524 | — |
| 提前斷線 / 慢客戶端踢除 | 0 / 0 | 0 / 0 | — |

### 8.3 伺服器看到的(`/metrics` 差值)

| | A' | B' | E |
|---|---|---|---|
| `trading_batch_size` 組數 / 命令數 / 平均每組 | 403 / 20,101 / **49.9** | 219 / 7,404 / 33.8 | 255 / 12,667 / 49.7 |
| `trading_batch_duration_seconds` 平均 | 149 ms | 80 ms | 118 ms |
| 平均每命令(組時間 ÷ 組大小) | **3.0 ms** | 2.4 ms | **2.4 ms** |
| `trading_batch_fallbacks_total` / `engine_rebuilds_total` | 0 / 0 | 0 / 0 | 0 / 0 |
| `trading_apply_duration_seconds`(現在 = 出佇列到該組 COMMIT) | 149 ms 平均 | 106 ms | 118 ms |
| exchange process CPU(整個 run,含 100 個帳戶註冊的 argon2 ≈ 13 s) | 79 s / 85 s wall | 47 s | 56 s / 40 s |
| RSS(run 後) | 177 MB | 180 MB | 118 MB |

### 8.4 讀法

1. **吞吐 ×2.1–2.5,飽和點從 165 命令/s 到 355–442。** E 組沒有推播訂閱者,是引擎本身的數字;A' 多了 20 個訂閱者的 40 萬則 delta 扇出與 10 個私有連線,分掉同一台機器的 CPU。
2. **瓶頸換了位置:從往返次數變成每句 SQL 的執行成本。** 本機的整合測試量到:單一命令 3 次往返 3.1 ms;一組 50 個純掛單 90 ms = 每命令 1.8 ms,只比逐一快 1.7×,雖然往返從 3 次變成約 1 次。也就是說每次往返裡的 8–9 句 SQL(savepoint、hold 的分錄 / 鎖 / postings / 餘額、`InsertOrder RETURNING *`、`UpdateOrderProgress RETURNING *`、兩列 outbox、release)各花 Postgres 約 0.2 ms,往返本身已經不是主角。再往上要減句數(掛單的 INSERT 與 progress UPDATE 合一、outbox 多列一句、拿掉 `RETURNING *`)或分片市場到多個 runner,不在這一期。
3. **群組提交把延遲換成了吞吐:飽和時推播延遲 p99 從 ~20 ms 變成 ~640 ms。** 事件在該組 COMMIT 之後才進 outbox,滿的一組(50 個命令)要 120–150 ms,加上排隊。這是設計上的取捨:`ENGINE_BATCH_SIZE` 就是那個旋鈕,佇列空時每組只有一個命令、延遲與單命令相同;負載高時組變大、吞吐上去、每則事件晚一點。loadgen 是每秒整批送出的(bursty),所以 B' 在 130 命令/s 也累出平均 34 個一組。§3.3 的「公開 depth delta p99 < 300 ms」在飽和時不成立(這裡原本引成「< 100 ms」,§3.3 沒有那個數字,本文件 §1 引的才是對的);在 100 命令/s 以下的真實流量(不是每秒一整批)應該成立,但**那是推論,沒有量**。已列進 `docs/beta-checklist.md` 的已知限制。
4. **`POST` p50 好 2.5×、p99 好 2.3×(B'),但 p99 < 50 ms 仍然沒有。** 現在的 p99 幾乎全是排隊:apply 直方圖裡 ≤ 25 ms 的桶只有個位數,因為 `trading_apply_duration_seconds` 的語意變成「出佇列到該組 COMMIT」。要看服務時間本身,看 `trading_batch_duration_seconds` 除以 `trading_batch_size`。
5. **零 fallback、零重建**:三組 run 沒有一組交易失敗,群組提交的復原路徑只在整合測試(注入故障)裡走過。
6. **註冊很貴**:100 個帳戶的 argon2id 佔了 exchange process 13 s CPU,壓測開頭的 CPU 尖峰是它,不是引擎;profile 要避開前 15 秒。

### 8.5 重現

同 §7,引擎不用額外設定(`ENGINE_BATCH_SIZE` 預設 50;設 1 可以量「只有 pipeline、沒有群組」的數字)。E 組:`--rate 1000 --duration 30s --accounts 100 --ws-clients 0 --private-clients 0 --idle-connections 0`。CPU profile:`curl 'http://127.0.0.1:9100/debug/pprof/profile?seconds=15'`(dev 才開)在 run 開始 25 秒後抓。
