# 區塊鏈交易所平台(白牌交易引擎)— 分階段可執行計畫 v1.0

## 1. 文件資訊

| 項目 | 內容 |
|---|---|
| 版本 | v1.0(取代 v0.1;v0.1 自本文件生效日起作廢) |
| 日期 | 2026-09-05 |
| 狀態 | 定案,可執行;每完成一個 Phase 回頭修訂並升版(v1.1、v1.2 …)。勘誤:2026-09-05 §6.1.4(e)/§6.4.2 提現 `cancel_nonce` 路徑的分錄(`docs/domain.md` E1) |
| Repo | `github.com/arc119226/crypto-exchange`(目前僅 `README.md` 與 Go 範本 `.gitignore`) |
| 本文件位置 | `docs/plan-v1.0.md`;重大決策另以 `docs/adr/NNNN-*.md` 記錄 |
| 語言約定 | 設計文件繁體中文;程式碼、註解、commit、API 文件、事件名、表名一律英文 |

閱讀方式:

- 第 2–5 節:定位、範圍、原則、架構。開工前讀一次,之後只在懷疑「該不該做某件事」時回來查。
- 第 6–8 節:領域模型、契約、模組。Phase 0–3 期間會反覆翻閱;分錄範例與狀態機轉移表是實作與測試的直接依據。
- 第 9–11 節:選型、目錄、compose。Phase 0 照抄建立骨架。
- 第 12 節:分階段計畫。每天照著任務清單打勾;每個 Phase 的 DoD 就是「可以進下一階段」的唯一判準。
- 第 13–19 節:品質、安全、觀測、部署、風險、限制、名詞表。需要時查閱。
- 第 20 節:接下來 5 個工作天。現在就看。
- 第 21 節:v0.1 → v1.0 變更對照,每條對應審查 finding id。

## 2. 專案定位與目標

### 2.1 真實定位

這不是「學習用玩具」,也不是要開一間交易所。它是一個**可授權給他人自架交易所的白牌交易引擎的商業化原型**:撮合、帳本、充提、行情、後台是引擎的核心交付物;身分驗證、KYC、通知內容、品牌前台是客戶自己的事,引擎只提供最小參考實作與整合面。

一套部署 = 一個營運方(單租戶);資料模型帶 `tenant_id` 但 v1 不做隔離邏輯(ADR-0003)。

不接主網、不碰真實資金:鏈上互動只走本地 anvil 與 Sepolia 測試網。這條底線保留自 v0.1,且是所有安全取捨的前提。

### 2.2 交付物

類別:**A = 引擎交付物**(有相容承諾,客戶靠它整合);**B = 參考實作**(可整包替換、不在支援承諾內);**C = 明確不做**(見 3.2)。

| 交付物 | 類別 | 形式 | 出現於 |
|---|---|---|---|
| 可部署服務(matching、ledger、trading、marketdata、chain、signer、registry、admin API、對帳、webhook) | A | 單一 container image(`ghcr.io/arc119226/crypto-exchange`),單一 binary 多角色 | Phase 0 起 |
| Helm chart | A | `deploy/helm/exchange`,kind/k3d 驗證 | Phase 7 |
| docker compose | A | `deploy/compose/compose.yaml`(dev / e2e / beta prod profile) | Phase 0 起 |
| OpenAPI 契約 | A | `api/public/v1/openapi.yaml`、`api/admin/v1/openapi.yaml` | Phase 0 起 |
| 事件契約 | A | `api/events/v1/*.json`(JSON Schema)+ `docs/events.md` | Phase 3 起 |
| WebSocket / Webhook 規格 | A | `docs/ws-api.md`、`docs/webhooks.md` | Phase 5–6 |
| 管理後台 | A | Go `html/template` + htmx,嵌入 binary(admin role) | Phase 5 |
| 運維文件 | A | `docs/runbooks/*.md`、Grafana dashboard JSON、alert rules | Phase 6–7 |
| 最小 auth(users 目錄、JWT、API key) | B | `internal/auth`;客戶可換成自家 IdP(JWKS URL 可替換) | Phase 3 |
| 參考交易前台 | B | React + Vite SPA(`web/trade`),可整包替換 | Phase 6 |
| CLI | B | `cmd/exchangectl`(demo、E2E、壓測、replay) | Phase 0 起 |

客戶只透過 REST / WebSocket / Webhook / 事件契約整合;核心程式碼全部在 `internal/`,不承諾任何 Go API 穩定性。

### 2.3 成功標準(v1 完成的定義)

1. `make up` 後,`exchangectl e2e` 在 5 分鐘內自動跑完:建帳 → anvil 充值(ETH 與 mock USDC)→ 入帳 → 下限價/市價單 → 成交 → 手續費入帳 → 提現(限額內自動、超額進後台審核)→ 鏈上確認 → 對帳報表零差異。
2. 隨機下單/取消過程中 `kill -9` engine 容器再啟動,訂單簿與帳本 hold 餘額完全一致,用戶無感(整合測試自動驗證)。
3. 撮合屬性/模糊測試、帳本不變量測試、鏈上 reorg / nonce 卡單整合測試全部在 CI 綠燈。
4. Helm chart 在 kind 上安裝成功並跑完同一套 E2E。
5. Sepolia 上手動完成一次充值 → 提現 → 歸集,runbook 記錄。

### 2.4 作者背景與學習目標

作者:Go 初學(寫過小工具,不熟 goroutine/channel/context/module);有其他語言後端與微服務經驗、web3/Solidity 經驗、Docker/K8s/DevOps 經驗;無交易系統領域知識;全職 30h+/週;無硬期限,以做對為主。

因此本計畫的原則是:**核心(撮合、帳本、狀態機)親手寫並用測試鎖住;基礎設施(HTTP、DB 驅動、NATS、go-ethereum)用成熟套件。** 領域知識缺口以第 6 節的模型與每個 Phase 的「需要補的領域概念」補齊。

這個專案要學會的 Go 主題:

| 主題 | 在哪個 Phase 第一次認真用 |
|---|---|
| module、`internal/` 可見性、`go mod tidy`、單一 module 為何不需要 `go.work` | Phase 0 |
| struct / method / slice / map / sort、值語意 vs 指標語意、不可變 value type 設計 | Phase 0(money)、Phase 1(matching) |
| interface 設計(小介面、依賴反轉、用 interface 切模組邊界) | Phase 0–3 |
| error 處理:`fmt.Errorf("%w")`、`errors.Is/As`、sentinel error、領域錯誤型別 | Phase 1–2 |
| table-driven test、`testify`、golden file、`go test -race`、benchmark | Phase 1 |
| 原生 `go test -fuzz`、`pgregory.net/rapid` 屬性測試 | Phase 1–2 |
| `context` 取消與逾時傳遞、`signal.NotifyContext`、graceful shutdown | Phase 0(app)、Phase 3 |
| goroutine / channel / `select`、`errgroup`、每市場單一 goroutine actor 模式、有界 buffer | Phase 3、Phase 6 |
| `sync.Mutex` / `sync.Once` / `atomic`(只在必要處) | Phase 3、6 |
| `pgx/v5` 連線池、交易、`SELECT ... FOR UPDATE`、`sqlc` 產生型別安全查詢、`goose` migration、`embed.FS` | Phase 0(registry 查詢與 migration)、Phase 2(交易與行鎖) |
| `net/http` + `chi` 中介層、`oapi-codegen` strict-server、自訂 JSON 型別(金額字串化) | Phase 0、3 |
| `crypto/ed25519`、HMAC-SHA256、argon2id、JWT/JWKS、TOTP | Phase 3、5 |
| `nats.go` JetStream:stream / durable consumer / 顯式 ack / 冪等消費 | Phase 3 |
| `log/slog`、Prometheus client、correlation id 貫穿、OpenTelemetry(選用) | Phase 0 起 |
| `go-ethereum`:`ethclient`、`abigen`、`types.Transaction`(EIP-1559)、keystore、`big.Int` 陷阱 | Phase 4 |
| `html/template` + `embed` + htmx | Phase 5 |
| WebSocket(`github.com/coder/websocket`)、每連線 writer goroutine、慢客戶端處理 | Phase 6 |
| 建置:`-ldflags` 版本注入、multi-stage Dockerfile、distroless、`golangci-lint`(depguard、forbidigo) | Phase 0 |

## 3. v1 範圍

### 3.1 功能範圍表

| 領域 | v1 做 | v1 不做(留 backlog) |
|---|---|---|
| 交易 | 現貨限價單 GTC、市價單(買以 quote 金額、賣以 base 數量,剩餘以 IOC 語意取消)、取消;部分成交;`client_order_id` 冪等;tick / step / min_notional 檢查;自成交防護 `cancel_newest`;市場狀態 `active / halted / cancel_only` | stop、post-only、FOK、iceberg、OCO;槓桿、合約;多市場撮合跨進程 |
| 手續費 | 每市場 maker / taker bps(via `fee_schedule_id`);以「收到的資產」扣(買方扣 base、賣方扣 quote);捨入對交易所有利 | 用戶等級費率、平台幣折抵、提現手續費(欄位預留、v1 為 0) |
| 資產 / 市場 | ETH + mock USDC(6 decimals,自行以 Foundry script 部署)+ 預留第二個 ERC-20;交易對 `ETH-USDC`;`assets / markets / fee_schedules / withdrawal_limits` 為 DB registry,後台可編輯;新增市場走受控重啟,引擎支援 `reload` 命令 | 非 EVM 鏈、多鏈 |
| 帳戶 / 帳本 | `users` 與 `accounts` 分表;每用戶自動建立一個現貨帳戶;複式記帳、append-only journal、`balances` 快取表;house 科目;試算平衡端點;管理員調帳(有 reason、有審計) | 子帳戶、內部轉帳、法幣 |
| 充值 | 每用戶 BIP-44 HD 地址;輪詢掃塊;確認數可設定;reorg 回退;ETH 原生轉帳 + ERC-20 `Transfer` 事件 | 合約內部轉帳(internal tx)充值、memo 型鏈 |
| 提現 | 狀態機、限額內自動放行、超額進後台審核、先鎖資金再簽名再廣播、`Idempotency-Key`、nonce 序列化與對帳、卡單重送 | 提現白名單冷卻期(欄位預留)、雙人審核 |
| 歸集 | 排程 sweep 到單一熱錢包;ERC-20 歸集前補 gas;歸集分錄入帳 | CREATE2 forwarder 合約 |
| 認證 | users 目錄、email + password(argon2id)、JWT EdDSA + JWKS、refresh token、API key + HMAC、`role ∈ {user, admin}`、admin 強制 TOTP、`kyc_level` 欄位由客戶系統透過 admin API 寫入 | 用戶端 2FA、OIDC 整合、KYC 文件蒐集、第三方 KYC |
| 後台 | 市場/資產/費率/限額設定、用戶與 kyc_level、餘額與 journal 瀏覽、試算平衡、提現審核佇列、充值列表、對帳報表、審計查詢、webhook 管理、引擎 reload | 客服工單、報表匯出排程 |
| 整合面 | REST(`/v1`、`/admin/v1`)、WebSocket 公開(depth snapshot+delta 帶 seq、trades、ticker、kline)與私有(orders、fills、balances;重連依 seq 補齊)、出站 Webhook(HMAC 簽名、指數退避、投遞記錄)、版本化事件 catalog | 客戶直接訂閱 NATS、gRPC、FIX |
| 部署 | compose(dev/e2e)、單台 VM compose 或單節點 k3s(beta)、Helm chart + kind 驗證 | Docker Swarm、多副本撮合、跨區 HA |

### 3.2 明確不做(v1 out of scope)

主網與真實資金;法幣通道;合約/槓桿;進階單型(stop、post-only、FOK 以外);非 EVM 多鏈;第三方 KYC 整合;用戶端 2FA;多租戶隔離邏輯;做市機器人;行動 App;HA / 多副本撮合;mTLS 服務身分;簡訊/Email 通知內容(只出 webhook);WAF / DDoS / SIEM;安全審計與滲透測試;事後風控偵測(異常交易偵測、自動熔斷——v1 只有同步 policy 檢查與人工停牌)。

### 3.3 規模與非功能目標(設計目標,非承諾)

| 項目 | 目標 | 量測方式 |
|---|---|---|
| 用戶規模 | 封閉 beta:註冊 5,000、同時線上 500、同時 WS 連線 1,000 | `exchangectl loadgen` + Prometheus |
| 下單延遲 | `POST /v1/orders` p99 < 50 ms(`role=all`,本機 Postgres) | `http_request_duration_seconds` |
| 撮合吞吐 | 單市場 ≥ 1,000 orders/s 持續 60 s 不積壓(每命令一筆 PG 交易的上限 ≈ 1 / 單筆交易延遲;達標手段為 runner 內 group commit:一筆交易套用 N 個命令,N ≤ 50。Phase 3 先量測單命令延遲再決定是否實作) | `trading_command_queue_depth`、loadgen |
| 純撮合效能 | `matching.Apply` ≥ 100,000 ops/s 單執行緒(benchmark) | `go test -bench` |
| 行情延遲 | 成交 → 私有 WS 推播 p99 < 200 ms;→ 公開 depth delta p99 < 300 ms | 事件 `occurred_at` vs 送出時間 |
| 引擎恢復 | 100,000 筆 open orders 重建訂單簿 < 30 s | `engine_rebuild_duration_seconds` |
| 充值入帳 | 確認數 × 出塊時間 + ≤ 10 s 掃描延遲 | `chain_scanner_lag_blocks` |
| 提現(自動路徑) | anvil 上 requested → confirmed ≤ 2 min | 狀態停留時間指標 |
| 資料保存 | v1 不刪除任何交易、分錄、事件;outbox 保留 30 天(供 WS resume) | — |
| 備份 | beta:每日 `pg_dump` + WAL 歸檔;RPO ≤ 24 h、RTO ≤ 1 h(人工) | Phase 7 演練 |
| 不追求 | 微秒級延遲、跨機 HA、多活 | — |

## 4. 設計原則(修訂版)

0. **不接主網、不碰真錢。** 鏈上只走 anvil 與 Sepolia;keystore 中的私鑰只控制測試資產。
1. **正確性優先於效能。**(v0.1 保留)先確保帳本平衡與撮合正確,再談延遲;效能目標只是設計上限,不因此犧牲交易性。
2. **事件解耦,但金錢路徑不靠事件。**(v0.1 修訂)所有影響餘額的操作在同一筆 Postgres 交易內完成並寫入 outbox;事件只用於扇出(行情、私有推播、webhook、後台投影),消費者永遠可以重放。
3. **ledger 是餘額的唯一寫入者,凍結也是分錄。**(v0.1 修訂)`internal/ledger` 唯一擁有 `ledger.*` 表;Hold / Release / Settle / Credit / Debit 是它對外的全部寫入介面;資料庫角色權限強制執行,不是靠約定。
4. **鏈上隔離。**(v0.1 保留並具體化)撮合與帳本不依賴鏈節點在線;簽名只在 `internal/chain/signer`,其他模組只能送「請簽這筆 withdrawal_id 的交易」。
5. **Postgres 是唯一真相。** 記憶體訂單簿、Redis 快照、JetStream 事件流都是可重建的衍生物;任何不一致以 Postgres 為準。
6. **冪等一切。** 對外寫入帶 `client_order_id` / `Idempotency-Key`;帳本分錄帶 `idempotency_key`;鏈上事件以 `(chain_id, tx_hash, log_index)` 去重;事件消費者以 `event_id` 去重。同一輸入重放兩次,狀態不變。
7. **契約先行。** OpenAPI 與事件 schema 先寫、再產生程式碼;契約變更只加欄位不改語意,破壞性變更升版。
8. **12-factor。** 設定全由環境變數注入(密鑰支援 `*_FILE`);無本地狀態;`/healthz` `/readyz` `/metrics`;SIGTERM graceful shutdown;啟動時對依賴指數退避重試。
9. **從第一天可觀測。** slog JSON、`correlation_id` 貫穿 HTTP → DB → NATS header → webhook;每個角色有指標;prometheus + grafana 是 compose 的固定成員。

## 5. 系統架構總覽

```
                         ┌────────────────────────────────────────────────────────┐
  Browser / Bots /       │   single binary: exchange serve --role=<role>          │
  Customer systems       │   (one image, one go.mod, internal/* package boundary)  │
        │                │                                                        │
        │ :8080 REST     │  ┌─────────┐  cmd bus  ┌──────────┐   PG tx (orders,   │
        ├───────────────►│  │  api    │──────────►│  engine  │   trades, ledger,  │
        │                │  │ auth,   │ in-proc / │ trading+ │   outbox)          │
        │ :8081 WS       │  │ ratelmt │ NATS req  │ matching │────────┐           │
        ├───────────────►│  ├─────────┤           │ +ledger  │        │           │
        │                │  │ stream  │◄──evt─────│ +relay   │        │           │
        │ :8082 admin UI │  │ md+ws   │           └──────────┘        │           │
        ├───────────────►│  ├─────────┤                ▲              │           │
        │                │  │  admin  │──approve──►┌──────────┐       │           │
        │ ◄── webhooks ──│  │ htmx    │            │  chain   │ PG tx │           │
        │                │  ├─────────┤            │ scanner, │ (dep, │           │
        │                │  │ worker  │◄──evt──    │ withdraw,│ wd,   │           │
        │                │  │ webhook,│            │ sweep,   │ledger,│           │
        │                │  │ recon,  │            │ signer   │outbox)│           │
        │                │  │ kline   │            └────┬─────┘       │           │
        │                │  └─────────┘                 │             │           │
        └────────────────┴──────────────────────────────┼─────────────┼───────────┘
                                                        │ JSON-RPC    │
   ┌──────────┐  ┌──────────┐  ┌──────────────┐  ┌──────▼─────┐ ┌─────▼──────────┐
   │  redis   │  │   nats   │  │ prometheus + │  │   anvil    │ │   postgres 16  │
   │ ratelimit│  │ JetStream│  │   grafana    │  │ (Sepolia   │ │ schemas: auth, │
   │ depth    │  │ (outbox  │  │ (observ.     │  │  by URL)   │ │ ledger,trading,│
   │ snapshot │  │  relay→) │  │  profile)    │  └────────────┘ │ registry,chain,│
   │          │  └──────────┘  └──────────────┘                 │ marketdata,    │
   └──────────┘                                                 │ webhook,audit, │
                                                                │ eventbus       │
                                                                └────────────────┘
```

### 5.1 角色清單

| role | 負責模組 | 對外 port | 實例數 | 備註 |
|---|---|---|---|---|
| `api` | `api`(public REST)、`auth`、`policy`(讀)、`trading`(client)、`ledger`(讀)、`chain/deposit`(指派預生成充值地址)、`chain/withdrawal`(建立提現請求) | 8080 | 1..n | 唯一持有 JWT 私鑰;提供 JWKS;限流 |
| `engine` | `trading`、`matching`、`ledger`(寫)、`registry`(讀 + reload)、`eventbus` relay | 無(ops 9100) | **恰好 1** | PG advisory lock 保證單一實例;唯一 outbox relay |
| `chain` | `chain/{deposit,withdrawal,sweep,hotwallet,evm}`、`ledger`(寫)、`policy` | 無 | 1 | 不持有任何私鑰;簽名一律送 `signer` |
| `signer` | `chain/signer`、`chain/hdwallet`(私鑰派生)、`audit` | 無(ops 9100) | **恰好 1** | 唯一掛載 keystore 的 role;`chain` 以 NATS request-reply `cmd.signer.sign.{tenant}`(逾時 10 s)送 `SignRequest`,`signer` 自行查 DB 驗證後回 raw tx;`role=all` 時為 in-proc 呼叫(ADR-0007) |
| `stream` | `marketdata`、`stream`(WS)、`auth`(驗證) | 8081 | 1..n | 無狀態訂閱者;深度快照放 Redis |
| `admin` | `admin`(htmx UI + admin REST)、`registry`(寫)、`audit`、`webhook`(設定) | 8082 | 1 | admin role 強制 TOTP |
| `worker` | `webhook` dispatcher、對帳 job、kline 聚合、outbox 清理 | 無 | 1 | 全部是事件消費者或排程 |
| `all` | 以上全部在同一進程 | 8080/8081/8082 | 1 | dev、E2E、beta 最小部署 |

子命令:`exchange serve`、`exchange version`、`exchange migrate up|status`、`exchange seed --fixtures`、`exchange healthcheck --url`、`exchange keys gen-jwt|import-mnemonic`、`exchange admin bootstrap`。

架構圖中 `chain` 方塊內的 `signer` 在拆分部署時是獨立的 `signer` role / 容器(見 5.1);圖只表示模組歸屬。

### 5.2 一筆限價買單的路徑(role=all 與多容器相同)

1. `POST /v1/orders`(JWT 或 API key)→ `api` 驗證 token、限流、schema 驗證、`client_order_id` 冪等查詢。
2. `trading.PlaceOrder` 做參數檢查(市場狀態、tick、step、min_notional、`policy.OrderPolicy`)。
3. 命令送進該市場的**單一 goroutine**(`role=all`:channel;拆分部署:NATS request-reply `cmd.trading.{tenant}.{market}`)。
4. goroutine 內:`BEGIN` → `ledger.Hold`(不足即 `ROLLBACK`,回 `REJECTED`,不碰記憶體訂單簿)→ 分配 `seq` → `matching.Apply(cmd)` 得事件 → 寫 `orders`、`trades`、`ledger` Settle/Release 分錄、`outbox` → `COMMIT`。commit 失敗 → 記憶體訂單簿標記髒、從 Postgres 重建。
5. 同步回應:訂單最終狀態與即時成交(API 形狀仍是「接受即回」:客戶端應以私有 WS / `GET /v1/orders/{id}` 取得後續變化)。
6. relay 把 outbox 送到 JetStream;`stream` 推播 depth/trades/私有 orders/fills/balances;`worker` 投遞 webhook、聚合 K 線;`admin` 投影。

## 6. 領域模型

本節是整份文件最重要的部分。所有分錄、狀態機、規則都是 Phase 1–4 的測試案例來源。

### 6.1 科目表與三欄餘額

#### 6.1.1 記帳模型

採真正的複式記帳:每筆 **journal entry** 由多筆 **posting** 組成,每筆 posting 有科目、方向(`debit` / `credit`)、資產、金額;**同一 entry 內、同一資產的 Σdebit = Σcredit**(應用層 assert + DB deferred trigger 雙重保險)。posting 只能 INSERT,不能 UPDATE / DELETE(DB 角色權限強制)。

科目型別與正常餘額(對初學者最重要的一張表):

| 型別 | 正常餘額 | debit 的效果 | credit 的效果 | 例子 |
|---|---|---|---|---|
| `LIABILITY`(交易所欠用戶) | credit | 減少 | 增加 | `user:{account_id}:available:ETH`、`user:{account_id}:hold:ETH`、`pending_withdrawal:ETH` |
| `ASSET`(交易所持有) | debit | 增加 | 減少 | `custody:deposit_addresses:ETH`、`custody:hot:ETH` |
| `REVENUE` | credit | 減少 | 增加 | `fee_revenue:ETH` |
| `EXPENSE` | debit | 增加 | 減少 | `gas_expense:ETH` |
| `EXTERNAL`(系統外世界) | 無 | — | — | `external:ETH` |

會計恆等式(對帳的數學基礎):

```
Σ custody assets (deposit_addresses + hot)
  = Σ user available + Σ user hold + Σ pending_withdrawal      (liabilities)
  + Σ fee_revenue − Σ gas_expense                              (equity)
  + Σ external balance                                          (faucet / adjustment / write-off)
```

`external:{asset}` 是充提對手科目的「系統外」代表:只在**沒有受控鏈上地址參與**的資金進出使用——dev faucet、管理員調帳、對帳沖銷、Sepolia faucet 注資熱錢包。正常營運下它的餘額只由這些已知原因構成,對帳報表逐筆列出。

#### 6.1.2 三欄餘額

每個 `account × asset` 有兩個 ledger 科目:`available` 與 `hold`。`total = available + hold` 是推導值,不存欄位。`ledger.balances(account_id, asset, available, hold, version)` 是快取表,**只維護 `kind = spot` 的用戶帳戶**,在同一交易內更新,`CHECK (available >= 0 AND hold >= 0)`;`available` 恆等於該科目 Σcredit − Σdebit(不變量測試)。house 科目(ASSET / REVENUE / EXPENSE / EXTERNAL)不進 `balances`(它們在 Σcredit − Σdebit 下可為負),餘額由 `SELECT SUM(CASE direction …) FROM postings GROUP BY account_id, asset` 推導;`TrialBalance()` 與對帳報表用此查詢。

#### 6.1.3 帳本原子操作(ledger 對外全部的寫入介面)

| 操作 | 語意 | idempotency_key 樣式 |
|---|---|---|
| `Hold(account, asset, amt, ref)` | available → hold | `hold:order:{order_id}`、`hold:withdrawal:{withdrawal_id}` |
| `Release(account, asset, amt, ref)` | hold → available | `release:order:{order_id}:{seq}`、`release:withdrawal:{id}` |
| `Settle(trade)` | 雙方 hold → 對手方 available + 手續費 + 價差 release | `settle:trade:{trade_id}` |
| `Credit(account, asset, amt, source)` | 充值入帳 / faucet / 調帳 | `deposit:{chain_id}:{tx_hash}:{log_index}`、`adjust:{adjustment_id}` |
| `Post(entry)` | 通用多 posting 分錄(提現、歸集、gas、沖銷) | 各流程定義 |
| `TrialBalance()`、`Balances(account)`、`Entries(filter)` | 查詢 | — |

`idempotency_key` 在 `ledger.journal_entries` 上 UNIQUE;重複呼叫回傳原 entry,不再過帳。

#### 6.1.4 分錄範例

以市場 `ETH-USDC`、fee schedule maker 10 bps / taker 20 bps、買方 B、賣方 S 為例。

**(a) 下單 Hold**:B 下限價買 1.0 ETH @ 2,000 USDC → 凍結 2,000 USDC。

| 科目 | 方向 | 金額 | 效果 |
|---|---|---|---|
| `user:B:available:USDC` | debit | 2000.000000 | available −2000 |
| `user:B:hold:USDC` | credit | 2000.000000 | hold +2000 |

市價買以 quote 金額 Q 下單 → Hold Q USDC;市價賣/限價賣以 base 數量 N → Hold N ETH。

**(b) 成交 Settle(含手續費與價差 Release)**:S 有掛單賣 0.4 ETH @ 1,990(maker)。B 為 taker,成交 0.4 ETH @ 1,990(**成交價永遠是被動方價格**),quote 金額 = 796 USDC。
B 收到 ETH → 手續費 0.4 × 20 bps = 0.0008 ETH;S 收到 USDC → 手續費 796 × 10 bps = 0.796 USDC。

| 科目 | 方向 | 金額 | 效果 |
|---|---|---|---|
| `user:B:hold:USDC` | debit | 796.000000 | B hold −796 |
| `user:S:available:USDC` | credit | 795.204000 | S available +795.204 |
| `fee_revenue:USDC` | credit | 0.796000 | 手續費 |
| `user:S:hold:ETH` | debit | 0.400000000000000000 | S hold −0.4 |
| `user:B:available:ETH` | credit | 0.399200000000000000 | B available +0.3992 |
| `fee_revenue:ETH` | credit | 0.000800000000000000 | 手續費 |
| `user:B:hold:USDC` | debit | 4.000000 | 價差 (2000−1990)×0.4 釋放 |
| `user:B:available:USDC` | credit | 4.000000 | |

驗算:USDC debit 800 = credit 795.204 + 0.796 + 4;ETH debit 0.4 = credit 0.3992 + 0.0008。B 剩餘掛單 0.6 ETH,hold 應為 1,200 USDC:2000 − 796 − 4 = 1200 ✓。

**(c) 取消 / IOC 剩餘 / 終態 Release**:B 取消剩餘 0.6 ETH。

| 科目 | 方向 | 金額 |
|---|---|---|
| `user:B:hold:USDC` | debit | 1200.000000 |
| `user:B:available:USDC` | credit | 1200.000000 |

**(d) 充值 credited**(X ETH 到達 B 的 HD 地址且達確認數):

| 科目 | 方向 | 金額 |
|---|---|---|
| `custody:deposit_addresses:ETH` | debit | X |
| `user:B:available:ETH` | credit | X |

已入帳後被深度 reorg 掉(極少見,超過確認數):寫反向分錄(debit `user:B:available`、credit `custody:deposit_addresses`),若 available 不足則 balances CHECK 失敗 → 進人工處理並告警;不自動追討。

**(e) 提現各狀態**(B 提 X ETH,gas G):

| 狀態轉移 | 分錄 |
|---|---|
| `approved → funds_locked` | debit `user:B:available:ETH` X / credit `user:B:hold:ETH` X(Hold) |
| `signed → broadcast` | debit `user:B:hold:ETH` X / credit `pending_withdrawal:ETH` X |
| `broadcast → confirmed` | debit `pending_withdrawal:ETH` X / credit `custody:hot:ETH` X;另一 entry:debit `gas_expense:ETH` G / credit `custody:hot:ETH` G |
| `→ failed(broadcast)`(自 `signed`:廣播失敗且確認 nonce 未上鏈) | debit `user:B:hold:ETH` X / credit `user:B:available:ETH` X(Release;資金仍在 hold) |
| `→ failed(replaced)`(自 `broadcast`:重送耗盡後以 `cancel_nonce` 取代) | debit `pending_withdrawal:ETH` X / credit `user:B:available:ETH` X(資金已在 `pending_withdrawal`);取代交易 gas:debit `gas_expense:ETH` G′ / credit `custody:hot:ETH` G′ |
| `→ failed`(receipt.status = 0) | gas 分錄照記;X 留在 `pending_withdrawal`,由後台 `resolve` 決定:`refund` = debit `pending_withdrawal` X / credit `user:B:available` X;`retry` = debit `pending_withdrawal` X / credit `user:B:hold` X(回到 `funds_locked`,重新分配 nonce 與簽名) |
| `rejected` / `policy_check` 拒絕 | 無分錄(尚未鎖資金) |

ERC-20 提現:資產分錄同上(asset = USDC),gas 分錄永遠是 ETH。

**(f) 歸集 Sweep**:

| 情境 | 分錄 |
|---|---|
| ETH 歸集 X(扣 gas G) | debit `custody:hot:ETH` (X−G) / debit `gas_expense:ETH` G / credit `custody:deposit_addresses:ETH` X |
| ERC-20 歸集第 1 步:熱錢包補 gas G' 到充值地址 | debit `custody:deposit_addresses:ETH` G' / credit `custody:hot:ETH` G' |
| ERC-20 歸集第 2 步:轉出 X USDC,消耗 gas G | debit `custody:hot:USDC` X / credit `custody:deposit_addresses:USDC` X;debit `gas_expense:ETH` G / credit `custody:deposit_addresses:ETH` G |

歸集只在 custody 科目間移動,**絕不改用戶餘額**。

**(g) Dev faucet / 管理員調帳**:debit `external:USDC` X / credit `user:B:available:USDC` X,需 admin、`reason`、寫審計。這個端點在 Phase 2 就存在(Phase 1–3 沒有鏈上充值時的資金來源),正式產品中就是「手動調帳」功能。

#### 6.1.5 帳本不變量(全部寫成測試)

1. 每筆 journal entry 內每種資產 Σdebit = Σcredit。
2. 對任一科目,`balances` 快取 = Σposting 推導值;`available ≥ 0`、`hold ≥ 0`。
3. 對任一 open order,`Σ hold(order)` = 該單尚未成交部分應凍結金額(限價買:remaining × limit_price;市價買:remaining quote;賣:remaining base)。
4. 同一 idempotency_key 重放兩次,所有餘額不變。
5. 試算平衡:每種資產 Σ(所有科目 debit) − Σ(credit) = 0,`GET /admin/v1/ledger/trial-balance` 恆為 0,`ledger_trial_balance_diff` 指標告警條件 ≠ 0。

### 6.2 訂單狀態機

`trading.orders` 關鍵欄位:`id (ULID)`, `tenant_id`, `account_id`, `market_id`, `client_order_id`, `side buy|sell`, `type limit|market`, `time_in_force gtc|ioc`, `price`, `qty`(市價買為 null), `quote_qty`(市價買用), `filled_qty`, `filled_quote`, `remaining_qty`, `hold_asset`, `hold_amount`, `status`, `reject_reason`, `seq`, `created_at`, `updated_at`。UNIQUE `(tenant_id, account_id, client_order_id)`。

狀態:`open`、`partially_filled`、`filled`、`cancelled`、`rejected`。沒有持久化的中間狀態:在本設計中 Hold、撮合、落庫在同一交易內完成,所以**拒單補償退化為交易回滾,不需要 saga**。

| 起點 | 觸發 | 終點 | 帳本動作 | 事件 |
|---|---|---|---|---|
| (new) | 參數/policy 檢查失敗、市場 halted / cancel_only、餘額不足(Hold 失敗)、市價單空簿 | `rejected`(持久化,含 reason;無分錄) | 無 | `order.rejected` |
| (new) | 限價單無成交,掛入簿 | `open` | Hold | `order.accepted` |
| (new) | 部分成交後掛入簿 | `partially_filled` | Hold + 每筆 Settle | `order.accepted` + `trade.executed`×n + `order.updated` |
| (new) | 全部成交 | `filled` | Hold + Settle×n(+ 價差 Release) | `order.accepted` + `trade.executed`×n + `order.filled` |
| (new) 市價 / IOC | 部分成交後剩餘取消 | `cancelled`(`filled_qty` > 0) | Hold + Settle×n + Release 剩餘 | `order.accepted` + `trade.executed`×n + `order.cancelled` |
| (new) | STP 觸發(新單會與自己的掛單成交) | `cancelled`(剩餘) | Release 剩餘 | `order.accepted` + `order.cancelled(reason=self_trade)` |

規則:**所有非 `rejected` 的新單一律先發 `order.accepted`**(帶 `client_order_id`、`time_in_force`),私有 WS 客戶端才能把後續 `trade.executed` / `order.cancelled` 對應回自己的 `client_order_id`。
| `open` / `partially_filled` | 對手單成交(作為 maker) | `partially_filled` / `filled` | Settle(+ 限價買價差 Release);`filled` 時 Release 殘餘 hold(捨入殘值) | `trade.executed` + `order.updated` / `order.filled` |
| `open` / `partially_filled` | 用戶取消(命令進同一 seq 序列) | `cancelled` | Release 剩餘 | `order.cancelled` |
| `open` / `partially_filled` | 管理員停牌後清簿(v1 不自動;`halted` 保留掛單、允許取消) | — | — | — |
| `filled` / `cancelled` / `rejected` | 取消請求 | 不變(回傳當前狀態,HTTP 200,天然冪等) | 無 | 無 |

拒單原因枚舉:`invalid_price_tick`, `invalid_qty_step`, `below_min_notional`, `market_not_active`, `insufficient_balance`, `empty_book`, `account_frozen`, `policy_denied`, `duplicate_client_order_id_mismatch`(同 key 不同內容 → 422)。

### 6.3 撮合規則

- **Price-time priority**:買方按價格由高到低、賣方由低到高;同價位按 `seq` FIFO。
- **資料結構**(v1):每邊 `map[price]*level` + 已排序 `[]price`(`sort.Search` 二分插入)+ 每個 level 一個 FIFO(slice 或 `container/list`)。學習檢查點:能解釋為何不是「一個排序 slice 放所有訂單」(取消與部分成交的成本、同價位 FIFO 語意)。beta 規模夠用;之後可換 `github.com/google/btree`。
- **限價單**:與對手最佳價比較,可成交則按被動方價格吃單至用完或價格不再滿足,剩餘掛入簿(GTC)。
- **市價買(quote 金額 Q)**:依序吃 ask,每筆 `fill_qty = min(level_qty, floor((Q_remaining / price), qty_step))`,直到 `Q_remaining < price × qty_step` 或簿空;剩餘 Q 取消並 Release。**市價賣(base 數量 N)**:依序吃 bid 至 N 用完或簿空。空簿直接 `rejected`。市場可設 `max_slippage_bps`(相對觸發時最佳價;v1 預設 null = 不限)。
- **部分成交**:每筆成交產生一個 `trade.executed`(`trade_id` ULID、`maker_order_id`、`taker_order_id`、`price`、`qty`、`quote_qty`、`maker_fee`、`taker_fee`、`seq`)。**手續費不在 `matching` 內計算**(`matching.Event.Trade` 沒有 fee 欄位;它只能 import `money`):`trading` 呼叫 `ledger.Settle(trade, ledger.FeeParams{MakerBps, TakerBps, BaseScale, QuoteScale})`,由 ledger 計算並回傳 `(makerFee, takerFee)`,再填入 `trade.executed` payload。`FeeParams` 是 ledger 自有型別,ledger 不 import registry。
- **tick / step / min_notional**:`price % price_tick == 0`、`qty % qty_step == 0`、`price × qty ≥ min_notional`(市價買以 `quote_qty ≥ min_notional`);在 `trading` 檢查,`matching` 以 panic-free 的 error 再驗一次。
- **STP `cancel_newest`**:新單將與同一 `account_id` 的掛單成交時,新單剩餘立即取消(事件帶 `reason=self_trade`),掛單不動。`markets.self_trade_policy` 預留 `allow` / `cancel_oldest`(v1 只實作 `cancel_newest`,`allow` 供測試)。
- **取消**:`Cancel{order_id}` 命令進同一 per-market 序列,「先到先贏」;找不到(已成交)回傳當前狀態。
- **純函式核心**:`func (b *Book) Apply(cmd Command) ([]Event, error)`;不呼叫 `time.Now()`、不用隨機、不做 I/O;`cmd.Seq` 與 `cmd.Timestamp` 由呼叫方(engine goroutine)指派;同一命令序列重放後 book 逐位元一致。`Book.Restore(orders []RestingOrder)` 供啟動重建,`Book.Snapshot()` 供深度與測試。
- **屬性測試不變量**:best bid < best ask(或一方為空);同價位 FIFO;成交價等於 maker 價且落在雙方限價之間;base/quote 數量守恆(成交前後 Σ 掛單 + Σ 成交 = Σ 輸入);任何命令序列不 panic;`Apply` 後 `Snapshot()` 與重放結果 deep-equal;STP 後簿中不存在同帳戶可成交的對敲。

### 6.4 充值、提現、歸集

#### 6.4.1 充值狀態機(`chain.deposits`)

冪等鍵 `(chain_id, tx_hash, log_index)`;原生 ETH 的 `log_index` 固定 `-1`。

| 狀態 | 進入條件 | 帳本 | 事件 |
|---|---|---|---|
| `detected` | 掃描到轉入受控地址的 tx / `Transfer` log,`confirmations < N` | 無 | `deposit.detected` |
| `confirming` | 每次掃描更新 `confirmations = head − block_number + 1` | 無 | (可選)`deposit.confirmations_updated` |
| `credited` | `confirmations ≥ assets.required_confirmations`(anvil = 1、Sepolia 6–12) | `Credit`(6.1.4 d)在同一交易內與狀態更新、outbox 一起 commit | `deposit.credited` |
| `orphaned` | 入帳前所在區塊被 reorg 掉 | 無 | `deposit.orphaned` |
| `orphaned` → `detected` / `confirming` | 同一 `(chain_id, tx_hash, log_index)` 在新的 canonical 鏈重新出現(真實 reorg 的常態):scanner 對每筆偵測結果先 `SELECT … FOR UPDATE`,已存在則 `UPDATE block_number, block_hash, confirmations, status`,**不是** INSERT(否則撞 UNIQUE 被當成重複而永不入帳) | 無 | `deposit.detected` |
| `dropped` | `orphaned` 超過 `ORPHAN_EXPIRY_BLOCKS`(anvil 100、Sepolia 1,000)仍未重現 | 無 | `deposit.dropped` |
| `reversed` | 入帳後被深度 reorg(極少;人工確認後執行) | 反向分錄 | `deposit.reversed` + 告警 |

掃描器(輪詢,不用 subscribe):

1. `chain.scan_cursors(chain_id, last_scanned_block, last_block_hash)`;每 `SCAN_INTERVAL`(anvil 2 s)取 `head`,掃 `[last+1, min(head, last+BATCH)]`。
2. 每個區塊:`eth_getBlockByNumber(full txs)` 比對 `tx.to ∈ 受控地址集合`(原生 ETH,合約內部轉帳 v1 不支援並文件明示);ERC-20 用 `eth_getLogs(address = token contract, topics[0] = Transfer)` 後在記憶體比對 `topics[2] ∈ 受控地址`(不要把數千個地址塞進 topic 陣列——公用 provider 對 topic 陣列長度有上限)。
3. 寫 `chain.blocks(number, hash, parent_hash)` 環狀保留最近 128 塊;推進前檢查 `parent_hash == 前一塊 hash`,不符則回退到最近共同祖先,將受影響且未 credited 的充值標 `orphaned`,重掃;重掃到同一筆 tx 時走「UPDATE 而非 INSERT」(見狀態表 `orphaned → detected`)。
4. 受控地址集合為記憶體 set,啟動時載入 `chain.deposit_addresses`,每次 tick 以 `max(id)` 檢查是否有新地址。
5. 啟動時比對 `chain.chain_state(chain_id, genesis_hash)`;不符或 `head < last_scanned_block` 則拒絕啟動並提示 `make reset`。

充值地址採**預生成地址池**(`api` 不持有任何金鑰):`signer`(拆分部署)/ `chain`(`role=all` 時透過 in-proc signer)在啟動及每次 tick 保證 `chain.deposit_addresses` 中 `account_id IS NULL` 的列數 ≥ `ADDRESS_POOL_MIN`(dev 50),派生時寫入 `(derivation_index, address, account_id NULL)`;`api` 的 `GET /v1/deposit-address` 只做 `UPDATE chain.deposit_addresses SET account_id = $1 WHERE id = (SELECT id FROM chain.deposit_addresses WHERE account_id IS NULL ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED) RETURNING address`(若該帳戶已有地址則直接回傳);池空回 503 並告警。每帳戶每鏈一個地址,ETH 與 ERC-20 共用。

#### 6.4.2 提現狀態機(`chain.withdrawals`)

冪等:`POST /v1/withdrawals` 帶 `Idempotency-Key`(存 key → request hash → response;同 key 不同內容 422;TTL 24 h)。每次轉移寫 `audit_events` 並發 `withdrawal.state_changed`。

| 起點 | 觸發者 | 終點 | 動作 |
|---|---|---|---|
| (request) | `api` | `requested` | 基本驗證(資產可提、地址格式、`amount ≥ min_withdrawal`、available 預檢) |
| `requested` | `chain` worker | `policy_check` → `auto_approved` / `pending_review` / `rejected` | `policy.WithdrawalPolicy`:單筆 `auto_approve_limit`、每日限額(依 `kyc_level`)、`withdraw_enabled`、帳戶凍結旗標;超額 → `pending_review` |
| `pending_review` | admin(TOTP、寫審計、記審核者) | `approved` / `rejected` | 後台佇列 approve / reject |
| `auto_approved` / `approved` | `chain` worker | `funds_locked` | `ledger.Hold`(`hold:withdrawal:{id}`);不足 → `failed(insufficient_balance)` |
| `funds_locked` | `chain` worker + `NonceManager` | `signed` | 組 EIP-1559 tx(`eth_estimateGas` + `SuggestGasTipCap` / `feeHistory`,上限 `MAX_FEE_PER_GAS`)、分配 nonce、`signer.Sign(withdrawal_id, tx)`;**nonce、raw signed tx 與狀態同一交易落庫** |
| `signed` | `chain` worker | `broadcast` | `eth_sendRawTransaction`;記 `tx_hash`;分錄 hold → pending_withdrawal;若節點回「nonce too low / already known」則查鏈上是否已有該 tx |
| `broadcast` | `chain` tracker | `confirmed` / `failed(on_chain)` | 輪詢 receipt;`confirmations ≥ N` 且 `status = 1` → confirmed(分錄 6.1.4 e);`status = 0` → failed(on_chain),gas 入帳,資金留 pending_withdrawal 進人工處置 |
| `broadcast` | `chain` tracker | `broadcast`(重送) | 超過 `REPLACE_AFTER`(anvil 60 s、Sepolia 3 min)未上鏈,同 nonce 費用 +≥10% 重送,最多 `MAX_REPLACEMENTS` 次後告警轉人工(狀態仍為 `broadcast`,等待 admin `resolve`) |
| `broadcast`(重送耗盡) | admin `resolve(action=cancel_nonce)` / `resolve(action=bump)` | `failed(replaced)` / `broadcast` | `cancel_nonce`:以同 nonce 送 0 ETH 自轉(`to = hot`)取代;取代交易確認後 debit `pending_withdrawal` X / credit `user:available` X(資金此時在 `pending_withdrawal`,**不是** hold,所以不是 `Release`;取代交易的 gas 記 `gas_expense`);`bump`:允許再重送一輪。勘誤 2026-09-05,見 `docs/domain.md` E1 |
| `signed`(重啟時) | `chain` worker | `broadcast` | 已簽未廣播 → 重播 raw tx(冪等,同 nonce) |
| `funds_locked` / `signed` | 廣播確定失敗且 `eth_getTransactionCount(hot, latest) ≤ nonce` | `failed(broadcast)` | Release;並由 `NonceManager` **回收 nonce**:若 `nonce == next_nonce − 1` 則 `next_nonce := nonce`;否則以同 nonce 送一筆 0 ETH 自轉(`to = hot`,gas 記 `gas_expense`,寫 `chain.nonce_fills`)填補缺口,填補 tx 走與提現相同的追蹤/重送流程。沒有這步,一次廣播失敗就會讓後續所有提現卡在 `broadcast` |

兩條鐵律:**帳本未先鎖定,絕不簽名;每個狀態落庫後才做下一步,重啟從狀態續跑。**

`NonceManager`(`internal/chain/hotwallet`,`chain` role 內單一 goroutine;`withdrawal` 與 `sweep` 的 gas 補款都經過它):`chain.hot_wallets(chain_id, address, next_nonce)`;啟動時 `pending := eth_getTransactionCount(hot, pending)`,`dbNext := next_nonce`;`pending < dbNext` 且 DB 有對應 `signed`/`broadcast` 列 → 重播;`pending < dbNext` 但 DB 找不到對應列 → 視為缺口,依上表規則以 0 ETH 自轉填補後才轉 ready;`pending > dbNext` → 有未知的外部交易使用了熱錢包私鑰 → 拒絕啟動並告警。

簽名只在 `internal/chain/signer`(拆分部署時為獨立 `signer` role,見 5.1):

```go
type SignRequest struct {
    Kind   SignKind // withdrawal | sweep | gas_fund
    RefID  string   // withdrawal_id / sweep_id
    From   Address  // withdrawal、gas_fund 必為 hot;sweep 必為某個 deposit address
    To     Address
    Asset  string
    Value  money.Amount
    Tx     *types.Transaction // 未簽名的 EIP-1559 tx
}
type Signer interface {
    Sign(ctx context.Context, req SignRequest) (rawTx []byte, txHash Hash, err error)
    Address(ctx context.Context, keyRef KeyRef) (Address, error)
}
```

v1 實作 `KeystoreSigner`,預留 `KMSSigner` 空實作。**種子儲存**:go-ethereum 的 V3 keystore 只能存單一 secp256k1 私鑰,存不了 BIP-39 種子,因此用自訂格式 `secrets/keystore/hd-seed.json`:BIP-39 entropy 以 scrypt(N=2^18)導出金鑰、AES-256-GCM 加密,passphrase 由 `WALLET_KEYSTORE_PASSPHRASE` 注入;`KeystoreSigner` 啟動時解密一次、在記憶體派生熱錢包(`m/44'/60'/1'/0/0`)與充值地址(`m/44'/60'/0'/0/{i}`)金鑰;`exchange keys import-mnemonic --from secrets/dev-mnemonic.txt` 產生此檔(Phase 4a)。`Sign` 內做政策檢查:`withdrawal` → 對應 `withdrawals` 列為 `funds_locked` 且金額、資產、目標地址一致;`sweep` → `From ∈ deposit_addresses` 且 `To == hot`;`gas_fund` → `From == hot`、`To ∈ deposit_addresses`、`Value ≤ MAX_GAS_FUND`;每次簽名寫 `chain.signing_log(kind, ref_id, tx_hash, signed_at)` 並以 `(kind, ref_id)` UNIQUE 防重簽;所有簽名寫審計。

#### 6.4.3 歸集(`chain.sweeps`)

排程(`SWEEP_INTERVAL`,anvil 1 min)掃描 `deposit_addresses` 的鏈上餘額:

- ETH:`balance ≥ sweep_threshold_eth` → 從該地址以派生金鑰(index i)簽 tx 轉 `balance − gas` 到熱錢包。
- ERC-20:`token_balance ≥ sweep_threshold` → 若地址 ETH 不足支付 gas,熱錢包先送精確 gas(tx1,狀態 `gas_funded`),確認後由地址簽 `transfer(hot, amount)`(tx2)。
- 每個充值地址只由 sweeper 使用,nonce 直接取 `PendingNonceAt(address)`。
- 狀態:`requested → gas_funded(僅 ERC-20) → broadcast → confirmed | failed`;分錄見 6.1.4 f;熱錢包 ETH 低於 `HOT_WALLET_MIN_ETH` 時發 `alert.hot_wallet_low` 事件(webhook 可訂閱)。

#### 6.4.4 鏈上對帳(Phase 4c 起,worker 排程)

對每個資產:`Σ ledger custody(deposit_addresses + hot)` vs `Σ 鏈上餘額(所有 HD 地址 + 熱錢包)`;差異 = 在途交易(broadcast 未確認)± 未入帳充值。差異超過閾值寫 `admin.reconciliation_breaks` 並發 `reconciliation.break_detected`。後台第一版就顯示這張表。

### 6.5 數值規範與捨入規則

| 層 | 規範 |
|---|---|
| Go | `internal/money.Amount`(封裝 `shopspring/decimal`,不可變 value type,所有運算回傳新值);`money.Asset{Symbol, Scale}`;禁止 `float32/float64/strconv.ParseFloat/Decimal.InexactFloat64` 出現在 `internal/{money,matching,ledger,trading,chain,marketdata,policy}`(golangci-lint forbidigo) |
| Postgres | 所有金額 `NUMERIC(36,18)`;`price`、`qty`、`quote_qty`、`fee` 皆是 |
| JSON / OpenAPI | 一律 `type: string`,`pattern: ^-?\d+(\.\d+)?$`,`x-go-type: money.Amount`;事件 schema 同 |
| 鏈上邊界 | `internal/chain/evm.ToWei(amount, scale) *big.Int` / `FromWei(*big.Int, scale)`;round-trip 測試含 0、1 wei、2^256−1;`big.Int` 每次 `new(big.Int).Set(...)` 複製,不外露指標 |
| registry | `assets.scale`(ETH 18、USDC 6);`markets.price_tick`(0.01)、`qty_step`(0.0001)、`min_notional`(5 USDC) |
| 精度約束 | 建立市場時驗證 `scale(qty_step) + scale(price_tick) ≤ quote.scale`,保證 `price × qty` 不需捨入(ETH-USDC:4 + 2 ≤ 6) |
| 手續費 | `fee = ceil(amount × bps / 10000)` 至該資產 scale(對交易所有利);同一數字同時出現在扣方與 `fee_revenue`,守恆不變 |
| 市價買換算 | `fill_qty = floor(Q_remaining / price, qty_step)`(向下,對用戶保守);剩餘 quote Release |
| 輸入 | API 收到的 `price`/`qty` 必須恰為 tick/step 整數倍,否則 `rejected`(不自動截斷,避免使用者誤解) |
| 顯示 | 前台以 `assets.display_scale` 顯示,不影響內部精度 |

### 6.6 Registry 欄位(`registry.*`,admin 可編輯,全部帶 `tenant_id`、`created_at`、`updated_at`、`version`)

`assets`:`id`, `symbol`, `name`, `chain_id`, `contract_address`(null = 原生幣), `scale`, `display_scale`, `is_native`, `required_confirmations`, `min_deposit`, `min_withdrawal`, `withdrawal_fee`(v1 = 0), `sweep_threshold`, `deposit_enabled`, `withdraw_enabled`, `status active|disabled`。

`markets`:`id`, `symbol`(`ETH-USDC`), `base_asset_id`, `quote_asset_id`, `price_tick`, `qty_step`, `min_notional`, `max_qty`(可 null), `max_slippage_bps`(可 null), `fee_schedule_id`, `self_trade_policy cancel_newest|allow|cancel_oldest`, `status active|halted|cancel_only|delisted`。

`fee_schedules`:`id`, `name`, `maker_bps`, `taker_bps`, `effective_from`。

`withdrawal_limits`:`asset_id`, `kyc_level`, `auto_approve_limit`(單筆), `daily_limit`, `require_manual_review bool`。

引擎啟動載入 `status ∈ {active, halted, cancel_only}` 市場並各起一個 goroutine;`market.updated` 事件 → 引擎 `reload`:新市場起 goroutine、`halted` 拒新單保留掛單、`cancel_only` 只收取消、`delisted` 需先清簿(v1 人工取消後才可設)。v1 新增市場採受控重啟(先 `reload`,失敗再重啟)。

### 6.7 帳戶模型與 tenant_id

- `auth.users`:`id`, `tenant_id`, `email`, `password_hash`, `role user|admin`, `kyc_level int`(0/1/2,由客戶系統寫入), `status active|frozen`, `totp_secret_enc`, `totp_enabled`。
- `ledger.accounts`:`id (account_id)`, `tenant_id`, `owner_user_id`(house 科目為 null), `kind spot|house`, `house_code`(`fee_revenue`、`custody_hot`、`custody_deposit_addresses`、`gas_expense`、`external`、`pending_withdrawal`), `status`。註冊時自動建立一個 `spot` 帳戶;house 科目由 migration seed。
- 引擎、帳本、鏈上模組一律以 `account_id` 為鍵,不認識 email 或 user 概念;`api` 把 JWT `sub`(user_id)映射到 `account_id`。
- `tenant_id` 出現在所有核心表與事件 envelope,v1 恆為 `default`,不做查詢隔離(ADR-0003 記錄觸發重評條件:第二個付費客戶要求共用基礎設施)。

## 7. 事件與契約

### 7.1 事件 envelope(對外契約,版本化)

```json
{
  "event_id": "01J8Z2K3M4N5P6Q7R8S9T0V1W2",
  "event_type": "trade.executed",
  "schema_version": 1,
  "tenant_id": "default",
  "market_id": "ETH-USDC",
  "account_id": null,
  "seq": 18234,
  "occurred_at": "2026-09-05T08:15:23.412Z",
  "correlation_id": "req_01J8Z2K2...",
  "causation_id": "01J8Z2K3M4N5P6Q7R8S9T0V1W1",
  "payload": { "trade_id": "...", "price": "1990.00", "qty": "0.4000", "...": "..." }
}
```

`seq`:市場域事件為 per-market engine seq;帳戶域事件(orders/fills/balances)另帶 `account_seq`——**每帳戶計數器** `ledger.accounts.next_seq`,在寫 outbox 的同一交易內 `UPDATE ledger.accounts SET next_seq = next_seq + 1 WHERE id = $1 RETURNING next_seq`(該交易本就持有此帳戶的 balances 行鎖,不增加死鎖面);outbox 因此有 `account_seq` 欄位。**不可用 outbox 的 BIGSERIAL id 當序號**:id 在取號時單調,但提交順序不保證,relay 會先發 11 再發 10,客戶端依「丟棄 ≤ last_seq」規則會漏掉 10。`event_id` 為 ULID。`event_type` 固定為 `<domain>.<type>` 兩段,`type` 不得含 `.`(golden 測試檢查所有 catalog 條目符合 `^[a-z_]+\.[a-z_]+$`),否則 subject 段數對不上 stream filter。演進規則:payload 只加欄位、不改語意;破壞性變更 `schema_version + 1` 並新增 schema 檔;golden-file 測試鎖住每版序列化結果。

### 7.2 事件 catalog

| event_type | producer(role) | consumers | 關鍵 payload 欄位 |
|---|---|---|---|
| `order.accepted` | engine | stream(private + depth)、webhook | order_id, client_order_id, account_id, side, type, price, qty, remaining_qty, seq |
| `order.updated` | engine | stream、webhook | order_id, filled_qty, remaining_qty, status, seq |
| `order.filled` | engine | stream、webhook | order_id, filled_qty, filled_quote, seq |
| `order.cancelled` | engine | stream(private + depth)、webhook | order_id, remaining_qty, reason, seq |
| `order.rejected` | engine | stream、webhook | client_order_id, reason |
| `trade.executed` | engine | stream(trades/ticker/kline)、worker(kline)、webhook | trade_id, maker_order_id, taker_order_id, maker_account_id, taker_account_id, price, qty, quote_qty, maker_fee, taker_fee, seq |
| `ledger.posted` | engine / chain / admin | stream(balances)、admin 投影 | entry_id, idempotency_key, postings[] |
| `balance.updated` | engine / chain / admin | stream(private)、webhook | account_id, asset, available, hold, account_seq |
| `deposit.detected` / `deposit.credited` / `deposit.orphaned` / `deposit.reversed` | chain | stream、webhook、admin | deposit_id, account_id, asset, amount, tx_hash, log_index, block_number, confirmations |
| `withdrawal.state_changed` | api / chain / admin | stream、webhook、admin | withdrawal_id, account_id, asset, amount, from_state, to_state, tx_hash, reason |
| `sweep.completed` / `sweep.failed` | chain | admin | sweep_id, asset, amount, gas_used |
| `market.updated` / `asset.updated` / `fee_schedule.updated` | admin | engine(reload)、stream、webhook | id, changed_fields, version |
| `user.kyc_level_updated` / `user.status_updated` | admin | webhook | user_id, kyc_level / status |
| `reconciliation.break_detected` / `alert.hot_wallet_low` | worker / chain | webhook、admin | asset, ledger_total, chain_total, diff |

### 7.3 outbox → JetStream

- `eventbus.outbox(id BIGSERIAL, event_id, event_type, tenant_id, market_id, account_id, account_seq, subject, headers jsonb, payload jsonb, occurred_at, published_at null)`;所有寫入模組在自己的交易內 INSERT。
- Relay(engine role 內單一 goroutine):`LISTEN outbox_new` 喚醒 + 每 100 ms 輪詢,按 `id` 順序 `Publish` 到 JetStream(`Nats-Msg-Id = event_id` 讓 JetStream 去重),成功後 `UPDATE published_at`;未 published 的列數為 `outbox_backlog` 指標。relay 以 `id` 排序只是近似;跨帳戶 / 跨市場順序不保證,消費者一律以 `seq` / `account_seq` 判斷順序。
- Streams 與 subjects(`ex.v1.<domain>.<type>.<tenant>.<scope>`):

| stream | subjects | retention |
|---|---|---|
| `EX_TRADING` | `ex.v1.order.*.*.*`、`ex.v1.trade.*.*.*`、`ex.v1.ledger.*.*.*`、`ex.v1.balance.*.*.*` | 30 天,file |
| `EX_CHAIN` | `ex.v1.deposit.*.*.*`、`ex.v1.withdrawal.*.*.*`、`ex.v1.sweep.*.*.*`、`ex.v1.alert.*.*.*` | 30 天 |
| `EX_REGISTRY` | `ex.v1.market.*.*.*`、`ex.v1.asset.*.*.*`、`ex.v1.fee_schedule.*.*.*`、`ex.v1.user.*.*.*`、`ex.v1.reconciliation.*.*.*` | 90 天 |

- 消費者分兩類:**扇出型**(`stream` role 的 marketdata / private,1..n 副本):每個實例用 ephemeral **ordered consumer**(`jetstream.OrderedConsumer`),不 ack、不寫 `processed_events`,以 `seq` / `account_seq` 去重——若多副本共用同一 durable consumer,JetStream 會把它當 work queue 分流,每台 WS 伺服器只收到一部分事件。**處理型**(`worker-webhook`、`worker-kline`、`admin-projection`、`engine-registry`):durable consumer + 顯式 ack;「業務寫入 + `processed_events(consumer, event_id)`」同一交易後才 ack,唯一鍵衝突視為重複直接 ack。
- `internal/eventbus` 介面:`Publisher`、`Subscriber(consumer, subjects, handler)`,JetStream 是第一個實作;Kafka 換實作不動業務碼。

### 7.4 OpenAPI 端點清單

Public(`api/public/v1/openapi.yaml`,路徑前綴 `/v1`;金額為字串;錯誤採 RFC 7807 problem+json):

| 群組 | 端點 |
|---|---|
| auth | `POST /auth/register`、`POST /auth/login`、`POST /auth/refresh`、`POST /auth/logout`、`GET /.well-known/jwks.json`(無前綴) |
| api keys | `GET/POST /api-keys`、`DELETE /api-keys/{id}`(scopes `read|trade|withdraw`,可設 IP 白名單) |
| account | `GET /account`、`GET /balances`、`GET /ledger/entries` |
| registry | `GET /assets`、`GET /markets`、`GET /markets/{symbol}` |
| trading | `POST /orders`(`client_order_id` 必填)、`DELETE /orders/{id}`、`GET /orders?status=`、`GET /orders/{id}`、`GET /fills` |
| market data | `GET /markets/{symbol}/depth?limit=`(含 `seq`)、`GET /markets/{symbol}/trades`、`GET /markets/{symbol}/ticker`、`GET /markets/{symbol}/klines?interval=` |
| chain | `GET /deposit-address?asset=`、`GET /deposits`、`POST /withdrawals`(`Idempotency-Key` 必填)、`GET /withdrawals`、`GET /withdrawals/{id}` |

Admin(`api/admin/v1/openapi.yaml`,前綴 `/admin/v1`;所有寫入寫 `audit_events`。認證兩種:**人類 admin** = 伺服端 session cookie(登入需 password + TOTP);**機器整合** = admin API key(scope 如 `users:kyc_write`、IP 白名單、HMAC 簽章),不需 TOTP):

| 群組 | 端點 |
|---|---|
| auth | `POST /auth/login`(password + totp)、`POST /auth/totp/enroll`、`POST /auth/totp/confirm` |
| users | `GET /users`、`GET /users/{id}`、`PUT /users/{id}/kyc-level`(客戶系統以 admin API key 呼叫)、`PUT /users/{id}/status` |
| registry | `GET/POST /assets`、`PUT /assets/{id}`;`GET/POST /markets`、`PUT /markets/{id}`、`PUT /markets/{id}/status`;`GET/POST /fee-schedules`、`PUT /fee-schedules/{id}`;`GET/PUT /withdrawal-limits` |
| ledger | `GET /ledger/trial-balance`、`GET /ledger/entries`、`GET /accounts/{id}/balances`、`POST /ledger/adjustments`(reason 必填) |
| chain | `GET /withdrawals?status=pending_review`、`POST /withdrawals/{id}/approve`、`POST /withdrawals/{id}/reject`、`POST /withdrawals/{id}/resolve`(on_chain failed 處置)、`GET /deposits`、`GET /sweeps`、`GET /hot-wallet` |
| reconciliation | `POST /reconciliation/run`、`GET /reconciliation/reports`、`GET /reconciliation/breaks` |
| audit | `GET /audit-events` |
| webhooks | `GET/POST /webhooks`、`PUT/DELETE /webhooks/{id}`、`GET /webhooks/{id}/deliveries`、`POST /webhooks/deliveries/{id}/replay` |
| engine | `POST /engine/reload`、`GET /system/status` |

相容性政策(beta):新增欄位不升版;刪改欄位或語意 → `/v2`;舊版維護期限有付費客戶後再定。

### 7.5 WebSocket(`docs/ws-api.md`)

- 端點:`wss://host/ws/v1/public`、`wss://host/ws/v1/private`(連線後第一則 `{"op":"auth","token":"<jwt>"}` 或 API key 簽章)。
- 訊息:`{"op":"subscribe","channel":"depth","market":"ETH-USDC"}`;伺服器 `{"channel":"depth","type":"snapshot","market":"ETH-USDC","seq":18234,"bids":[["1990.00","0.4000"]],"asks":[]}`,之後 `{"type":"delta","seq":18235,"bids":[["1990.00","0"]],"asks":[...]}`(qty `"0"` = 移除價位)。
- 客戶端規則:取 snapshot(WS 或 `GET /depth`,含 `last_seq`)→ 丟棄 `seq ≤ last_seq` 的 delta → 之後 seq 必須連續,缺號即重抓 snapshot。
- 公開頻道:`depth`、`trades`、`ticker`、`kline.{1m|5m|15m|1h|1d}`。私有頻道:`orders`、`fills`、`balances`;每則帶 `account_seq`;重連時送 `{"op":"resume","since_seq":N}`,伺服器從 `eventbus.outbox`(保留 30 天)補齊 `account_id = X AND account_seq > N` 的事件後接上即時流。
- 心跳:伺服器每 15 s `ping`,30 s 無回應斷線。每連線一個 writer goroutine + 有界 buffer(256),塞滿即斷線(慢客戶端不得拖垮廣播)。

### 7.6 Webhook(`docs/webhooks.md`)

- 後台註冊 endpoint:`url`、`event_types[]`、`secret`(由系統產生,顯示一次)、`status`。
- 投遞:`POST url`,body = envelope;headers `X-Exchange-Event-Id`、`X-Exchange-Event-Type`、`X-Exchange-Timestamp`、`X-Exchange-Signature: v1=hex(hmac_sha256(secret, timestamp + "." + body))`。
- 重試:2xx 成功;否則指數退避 1 m → 5 m → 30 m → 2 h → 12 h → 24 h,6 次後 `dead`(退避序列由 `WEBHOOK_BACKOFF` 設定,整合測試注入 `100ms,200ms,400ms`);`webhook.deliveries` 記錄每次嘗試(狀態碼、耗時、錯誤);後台可查、可手動 replay。
- 至少一次投遞;客戶以 `event_id` 去重。

## 8. 模組清單(`internal/`)

| package | 職責 | public API(概述) | 擁有的 schema / 表 | 允許 import |
|---|---|---|---|---|
| `money` | Amount、Asset、捨入、tick/step 檢查 | `Amount`, `Asset`, `ParseAmount`, `RoundUp/RoundDown/Truncate`, `IsMultipleOf` | 無 | stdlib、shopspring/decimal |
| `matching` | 純函式訂單簿 | `Book`, `Apply`, `Restore`, `Snapshot`, `Command`, `Event` 型別 | 無 | `money` |
| `ledger` | 複式帳本 | `Service{Hold, Release, Settle, Credit, Post, Balances, TrialBalance, Entries}`;`Tx` 介面讓呼叫方傳入 pgx.Tx | `ledger.accounts / journal_entries / postings / balances` | `money`, `platform/pg` |
| `registry` | 資產/市場/費率/限額 registry + 快取 + reload | `Store`, `Cache{Market(symbol), Asset(symbol)}`, `Reload` | `registry.assets / markets / fee_schedules / withdrawal_limits` | `money`, `eventbus`, `audit` |
| `policy` | 同步規則檢查 | `OrderPolicy.Check`, `WithdrawalPolicy.Evaluate → allow/review/deny` | 無(讀 registry、ledger) | `registry`, `ledger`(讀) |
| `trading` | 訂單狀態機、per-market runner、命令匯流排、outbox 寫入 | `Service{PlaceOrder, CancelOrder, GetOrder, ListOrders, ListFills}`, `CommandBus`(in-proc / NATS), `Engine{Start, Reload}` | `trading.orders / trades / market_sequences` | `matching`, `ledger`, `registry`, `policy`, `eventbus`, `money` |
| `eventbus` | envelope、outbox、relay、Publisher/Subscriber、JetStream 實作 | `Envelope`, `Outbox.Append(tx, evt)`, `Relay.Run`, `Publisher`, `Subscriber`, `Processed` | `eventbus.outbox / processed_events` | `platform/pg`, `platform/natsx` |
| `marketdata` | 影子訂單簿、depth delta、trades、ticker、kline 聚合、快照快取 | `BookProjector`, `KlineAggregator`, `SnapshotCache` | `marketdata.klines / tickers` | `eventbus`, `registry`, `money`, `platform/redisx` |
| `stream` | WS 伺服器(公開/私有)、resume | `Server`, `Hub` | 無 | `marketdata`, `auth`, `eventbus` |
| `chain/evm` | ethclient 封裝、wei 轉換、gas、receipt | `Client`, `ToWei`, `FromWei`, `EstimateFees` | `chain.chain_state / blocks` | go-ethereum, `money` |
| `chain/hdwallet` | BIP-44 派生、預生成地址池(只在 `signer` role 內執行) | `Derive(index) (address, keyRef)`, `EnsurePool(ctx, min)` | `chain.deposit_addresses`(寫入池;`chain/deposit` 只做指派) | hdwallet lib, `platform/pg` |
| `chain/signer` | 唯一簽名處(拆分部署為獨立 `signer` role;`role=all` 為 in-proc) | `Signer` 介面(`SignRequest{Kind: withdrawal|sweep|gas_fund, …}`)、`KeystoreSigner`(讀 `hd-seed.json`)、`KMSSigner`(stub)、NATS request-reply server | `chain.signing_log` | `chain/hdwallet`, `audit`, `platform/natsx` |
| `chain/hotwallet` | 熱錢包 nonce 序列化、啟動對帳、缺口回收 | `NonceManager{Next, Release, Reconcile}` | `chain.hot_wallets / nonce_fills` | `chain/evm`, `platform/pg` |
| `chain/deposit` | 掃描器、確認數、reorg、入帳、地址指派 | `Scanner.Run`, `Addresses.Assign(account_id)` | `chain.deposits / scan_cursors` | `chain/evm`, `ledger`, `registry`, `eventbus` |
| `chain/withdrawal` | 狀態機、廣播、追蹤、重送 | `Worker.Run`, `Requests{Create, Approve, Reject, Resolve}` | `chain.withdrawals / idempotency_keys` | `chain/evm`, `chain/signer`(client), `chain/hotwallet`, `ledger`, `policy`, `eventbus`, `audit` |
| `chain/sweep` | 歸集 | `Sweeper.Run` | `chain.sweeps` | `chain/evm`, `chain/signer`(client), `chain/hotwallet`, `ledger` |
| `auth` | users、密碼、JWT/JWKS、refresh、API key、TOTP、中介層 | `Service{Register, Login, Refresh, Logout, IssueAPIKey, VerifyTOTP}`, `Middleware{RequireUser, RequireAdmin, VerifyHMAC}` | `auth.users / credentials / api_keys / refresh_tokens` | `platform/*`, `audit` |
| `audit` | append-only 審計 | `Recorder.Record(actor, action, target, before, after)`, `Query` | `audit.audit_events` | `platform/pg` |
| `webhook` | endpoint 管理、dispatcher、deliveries | `Store`, `Dispatcher.Run` | `webhook.endpoints / deliveries` | `eventbus`, `audit` |
| `admin` | htmx 後台 + admin REST 實作(oapi 產生的 strict server) | `Handler` | `admin.reconciliation_reports / reconciliation_breaks` | 幾乎所有(讀)+ `registry`(寫)+ `chain/withdrawal`(審核) |
| `api` | public REST 實作、限流、冪等中介層 | `Handler` | 無 | `trading`, `ledger`(讀), `auth`, `registry`, `chain/withdrawal`(建立)、`chain/deposit`(指派預生成地址;**不** import `chain/hdwallet`)、`marketdata` |
| `app` | 設定、run loop、健康、shutdown、依賴重試、角色組裝 | `Run(ctx, roles)`, `Config` | 無 | 全部 |
| `telemetry` | slog、correlation id、metrics、otel | `Logger`, `Metrics`, `CorrelationMiddleware` | 無 | stdlib、prometheus、otel |
| `platform/{pg,natsx,redisx}` | 連線、重試、健康探測 | — | 無 | 驅動程式 |

相依方向(depguard 強制):`money` ← `matching` ← `trading`;`ledger` 不得 import `trading/chain/api/admin`;`matching` 只能 import `money` 與 stdlib;`api`、`admin`、`stream` 只被 `app` import;`cmd/*` 只 import `app` 與 `telemetry`;沒有任何 package import `cmd`。跨 schema 寫入以 DB 角色權限擋住(見第 14 節)。

## 9. 技術選型定案表

| 項目 | 定案 | 一句理由 | 對初學者的注意事項 |
|---|---|---|---|
| 語言 | Go 1.23+(go.mod `go 1.23`,`toolchain` 釘住) | 目標語言 | CI 與本機用同一版本;不要手動改 `go.sum` |
| HTTP | `net/http` + `chi/v5` + `oapi-codegen` strict-server(`chi-server`) | 契約先行,產生介面自己實作;不做轉發層 | `ServerInterface` 只在 `api`/`admin` 各實作一次;middleware 順序決定 correlation id 何時可用 |
| DB | Postgres 16 + `pgx/v5` + `sqlc` | 顯式 SQL、編譯期型別安全,學 SQL 不學 ORM | `pgtype.Numeric` ↔ `money.Amount` 的 helper 放 `internal/platform/pg`(`pg.NumericFromAmount / pg.AmountFromNumeric`)寫一次並測邊界(`money` 不 import pgx);交易一律 `defer tx.Rollback(ctx)` |
| Migration | `goose`(純 SQL、forward-only、`embed.FS`) | `exchange migrate` 子命令與 compose 一次性 service 共用 | 不寫 down;schema 改動用新檔;測試每次跑完整 migration |
| 事件 | `nats.go` + JetStream(`-js`、file store、volume) | 沿用 v0.1 選 NATS 的理由(輕量、單一二進位),JetStream 給持久化與 durable consumer,不需 Kafka | core NATS 與 JetStream 是兩套 API;一律用 JetStream `Consume` + 顯式 `Ack` |
| Redis | `redis/go-redis/v9`;只做限流計數、深度快照快取 | 兩件事都可丟可重建;refresh token 以 hash 存 `auth.refresh_tokens`,撤銷 = 刪列,access token 15 min 自然到期,不需要 Redis 撤銷清單 | 不要放任何真相;沒有 Redis 也要能啟動(降級為 in-memory 限流) |
| 數值 | `shopspring/decimal` 封裝 `internal/money` | 可讀、不溢位、貼近 NUMERIC | `Decimal` 是值型別但含指標,永遠用 `money.Amount` 方法;禁止 float |
| 日誌 | `log/slog` JSON | 標準庫 | 固定欄位:`role`, `correlation_id`, `tenant_id`, `market_id`, `order_id`, `account_id` |
| 指標 / 追蹤 | `prometheus/client_golang`;OpenTelemetry trace 選用(Phase 6) | 基本監控是交付物 | histogram bucket 先粗後細 |
| 測試 | `testify`、`pgregory.net/rapid`、原生 `go test -fuzz`、`testcontainers-go`(postgres、nats 模組;anvil 用 `GenericContainer`) | 品質基線 | 整合測試用 build tag `integration`;fuzz 只在 CI 跑 smoke(30–60 s) |
| Lint | `golangci-lint`(depguard、forbidigo、errcheck、govet、staticcheck、revive、gosec) | 強制模組邊界與禁 float | `.golangci.yml` 進 repo;PR 必過 |
| 鏈上 | `go-ethereum`(`ethclient`、`abigen`、`accounts/keystore`、`core/types`) | 標準 | `abigen` 產物進 repo 並由 `make gen` 更新;`big.Int` 共享可變狀態的坑 |
| HD 派生 | `github.com/miguelmota/go-ethereum-hdwallet`(備案 `btcsuite/btcd/btcutil/hdkeychain` + 自行轉 secp256k1) | 直接給 BIP-44 路徑 | 用已知測試向量驗證派生地址;助記詞由 `make gen-dev-secrets`(`cast wallet new-mnemonic`)產生,禁用 anvil 預設;種子存自訂加密檔 `secrets/keystore/hd-seed.json`(scrypt + AES-256-GCM,見 6.4.2),**不用** geth V3 keystore(它存不了 BIP-39 種子) |
| 合約 | Foundry(`forge script` 部署 mock USDC;`anvil` 本地鏈) | 作者熟 Solidity | 映像 pin tag;`anvil --state` 與 `make reset` |
| JWT | `github.com/lestrrat-go/jwx/v2`(EdDSA、JWKS) | 非對稱、私鑰只在 api role | 用 `crypto/ed25519` 產金鑰;JWKS 由 `api` 提供,其他角色以公鑰驗證 |
| 密碼 / TOTP | `golang.org/x/crypto/argon2`;`github.com/pquerna/otp` | 標準 | argon2id 參數固定並測試耗時 |
| WebSocket | `github.com/coder/websocket`(原 nhooyr) | context 原生、簡潔 | 每連線 writer goroutine + 有界 buffer |
| CLI | `spf13/cobra` | 子命令多 | `exchange` 與 `exchangectl` 共用 cobra 慣例 |
| 設定 | `caarlos0/env/v11` typed struct + `*_FILE` | 12-factor | 啟動時驗證必填並 log 非密鑰設定 |
| 後台 UI | `html/template` + htmx + `embed` | 嵌入 binary,零前端建置 | 表單 CSRF token;所有寫入走 admin OpenAPI handler |
| 前台 | React 18 + Vite + TypeScript(`web/trade`);OpenAPI 產生 TS client | 可替換參考實作 | 只呼叫 public API + WS;不進 Go binary |
| 建置 | multi-stage Dockerfile(`golang:1.23` → `gcr.io/distroless/static`),`-ldflags -X main.version` | 一個 image | distroless 無 shell,healthcheck 用 `exchange healthcheck` |
| 任務 | `Makefile` | 通用 | 每個目標一行說明 |
| CI | GitHub Actions | 現成 | job 分層(lint → unit → integration → e2e → image) |
| ADR | `docs/adr/NNNN-title.md`(背景/選項/決定/後果) | 決策可追溯 | 第一批 8 份見第 20 節 |

## 10. Repo 結構

```
crypto-exchange/
├── go.mod  go.sum  Makefile  .golangci.yml  .env.example  README.md  LICENSE
├── .github/
│   └── workflows/ci.yml            # lint, unit, fuzz-smoke, integration, e2e, image, helm(kind)
├── api/
│   ├── public/v1/openapi.yaml
│   ├── admin/v1/openapi.yaml
│   └── events/v1/*.json            # JSON Schema per event_type (+ envelope.json)
├── cmd/
│   ├── exchange/main.go            # serve | migrate | seed | healthcheck | keys | admin
│   └── exchangectl/main.go         # demo / e2e / loadgen / replay / admin ops
├── internal/
│   ├── app/  telemetry/  platform/{pg,natsx,redisx}/
│   ├── money/  matching/  ledger/  registry/  policy/  trading/  eventbus/
│   ├── marketdata/  stream/
│   ├── chain/{evm,hdwallet,signer,hotwallet,deposit,withdrawal,sweep}/
│   ├── auth/  audit/  webhook/
│   ├── api/                        # generated/ (oapi-codegen) + handlers + middleware
│   └── admin/                      # generated/ + handlers + templates/ (embed) + static/
├── migrations/                     # goose, embed.FS, 單一平面目錄(一個版本表);檔名 NNNN_<module>_<desc>.sql
│                                   #   0001_bootstrap_schemas.sql, 0002_registry_core.sql, 0003_ledger_core.sql, ...
├── infra/
│   ├── contracts/                  # foundry.toml, src/MockUSDC.sol, script/Deploy.s.sol, test/
│   ├── postgres/initdb/01-roles.sh # roles, schemas, default privileges (dev)
│   └── observability/{prometheus.yml, grafana/provisioning, dashboards/*.json, alerts/*.yml}
├── build/Dockerfile
├── deploy/
│   ├── compose/compose.yaml  compose.prod.yaml
│   ├── helm/exchange/              # Chart.yaml, values.yaml, templates/
│   └── k3s/                        # beta 單節點筆記與 values 覆蓋
├── web/trade/                      # React + Vite SPA(參考前台)
├── scripts/                        # gen-dev-secrets.sh, e2e.sh, reset.sh, trace.sh
├── docs/
│   ├── plan-v1.0.md  domain.md  events.md  ws-api.md  webhooks.md  api-conventions.md
│   ├── adr/0001-*.md ...
│   └── runbooks/{engine-restart,stuck-withdrawal,reorg-alert,hot-wallet-low,backup-restore}.md
├── secrets/                        # gitignored; gen-dev-secrets 產物(jwt key、keystore)
└── test/
    ├── integration/                # testcontainers(build tag integration)
    ├── e2e/                        # compose 全起的腳本與 Go 測試
    └── fixtures/                   # 撮合 golden files、命令序列
```

不使用 `go.work`(單一 module);`.gitignore` 追加 `secrets/`、`web/trade/node_modules`、`web/trade/dist`、`deploy/compose/artifacts/`。

## 11. docker compose 骨架 v1.0

`deploy/compose/compose.yaml`(需 Docker Compose v2;Makefile 以 `--env-file .env` 執行):

```yaml
name: crypto-exchange

# 只放「非密鑰、所有 role 都需要」的設定;密鑰一律在個別 service 注入(第 14 節)
x-exchange-env: &exchange-env
  EXCHANGE_ENV: dev
  TENANT_ID: default
  NATS_URL: nats://nats:4222
  REDIS_ADDR: redis:6379
  ETH_RPC_URL: http://anvil:8545
  ETH_CHAIN_ID: "31337"
  OPS_ADDR: ":9100"
  LOG_LEVEL: ${LOG_LEVEL:-info}
  JWT_JWKS_URL: http://exchange-api:8080/.well-known/jwks.json

x-exchange: &exchange
  image: ghcr.io/arc119226/crypto-exchange:${EXCHANGE_TAG:-dev}
  build: { context: ../.., dockerfile: build/Dockerfile }
  restart: unless-stopped
  networks: [exchange-net]
  volumes:
    - artifacts:/artifacts:ro          # 不全域掛載 secrets/;各 role 只掛自己需要的子目錄(見下)
  healthcheck:
    test: ["CMD", "/exchange", "healthcheck", "--url", "http://127.0.0.1:9100/readyz"]
    interval: 10s
    timeout: 3s
    retries: 6
    start_period: 20s

x-app-deps: &app-deps
  postgres: { condition: service_healthy }
  redis: { condition: service_healthy }
  nats: { condition: service_healthy }
  migrate: { condition: service_completed_successfully }
  seed: { condition: service_completed_successfully }

services:
  postgres:
    image: postgres:16.4
    profiles: [infra]
    environment:
      POSTGRES_DB: exchange
      POSTGRES_USER: exchange
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
    volumes:
      - pg-data:/var/lib/postgresql/data
      - ../../infra/postgres/initdb:/docker-entrypoint-initdb.d:ro
    ports: ["127.0.0.1:5432:5432"]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U exchange -d exchange"]
      interval: 5s
      timeout: 3s
      retries: 10
    restart: unless-stopped
    networks: [exchange-net]

  redis:
    image: redis:7.4-alpine
    profiles: [infra]
    command: ["redis-server", "--save", "", "--appendonly", "no"]
    ports: ["127.0.0.1:6379:6379"]
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 3s
      retries: 10
    restart: unless-stopped
    networks: [exchange-net]

  nats:
    image: nats:2.10.22-alpine
    profiles: [infra]
    command: ["-js", "-sd", "/data", "-m", "8222"]
    volumes: [nats-data:/data]
    ports: ["127.0.0.1:4222:4222", "127.0.0.1:8222:8222"]
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://127.0.0.1:8222/healthz"]
      interval: 5s
      timeout: 3s
      retries: 10
    restart: unless-stopped
    networks: [exchange-net]

  anvil:
    image: ghcr.io/foundry-rs/foundry:${FOUNDRY_TAG}      # pin 到實際存在的 v1.x tag,寫在 .env
    profiles: [infra]
    entrypoint: ["anvil"]
    command:
      - --host=0.0.0.0
      - --chain-id=31337
      - --block-time=2
      - --accounts=10
      - --balance=10000
      - --state=/data/anvil-state.json
      - --state-interval=10
    volumes: [anvil-data:/data]
    ports: ["127.0.0.1:8545:8545"]
    healthcheck:
      test: ["CMD", "cast", "block-number", "--rpc-url", "http://127.0.0.1:8545"]
      interval: 5s
      timeout: 5s
      retries: 20
    restart: unless-stopped
    networks: [exchange-net]

  contracts-deployer:
    image: ghcr.io/foundry-rs/foundry:${FOUNDRY_TAG}
    profiles: [infra]
    working_dir: /work
    entrypoint: ["sh", "-c"]
    command:
      - >
        forge script script/Deploy.s.sol --rpc-url http://anvil:8545 --broadcast
        --private-key ${ANVIL_DEPLOYER_KEY} --json
    # Deploy.s.sol 以 vm.writeJson 直接寫 /artifacts/addresses.json(forge 的 out/ 是編譯產物,不會有這個檔);
    # foundry.toml 需設 fs_permissions = [{ access = "write", path = "/artifacts" }]
    environment:
      HOT_WALLET_ADDRESS: ${HOT_WALLET_ADDRESS}     # Deploy.s.sol 順便注資 100 ETH + mint USDC
    volumes:
      - ../../infra/contracts:/work
      - artifacts:/artifacts
    depends_on:
      anvil: { condition: service_healthy }
    restart: "no"
    networks: [exchange-net]

  migrate:
    <<: *exchange
    profiles: [app, single]
    command: ["migrate", "up"]
    environment:
      <<: *exchange-env
      DATABASE_URL: postgres://ex_migrate:${POSTGRES_PASSWORD}@postgres:5432/exchange?sslmode=disable
    depends_on:
      postgres: { condition: service_healthy }
    healthcheck: { disable: true }
    restart: "no"

  seed:
    <<: *exchange
    profiles: [app, single]
    command: ["seed", "--fixtures", "/artifacts/addresses.json"]
    environment:
      <<: *exchange-env
      DATABASE_URL: postgres://ex_admin:${POSTGRES_PASSWORD}@postgres:5432/exchange?sslmode=disable
    depends_on:
      migrate: { condition: service_completed_successfully }
      contracts-deployer: { condition: service_completed_successfully }
    healthcheck: { disable: true }
    restart: "no"

  exchange-api:
    <<: *exchange
    profiles: [app]
    command: ["serve", "--role=api"]
    environment:
      <<: *exchange-env
      DATABASE_URL: postgres://ex_api:${POSTGRES_PASSWORD}@postgres:5432/exchange?sslmode=disable
      HTTP_ADDR: ":8080"
      JWT_PRIVATE_KEY_FILE: /secrets/jwt/ed25519.pem     # 唯一持有 JWT 私鑰的 role
    volumes:
      - artifacts:/artifacts:ro
      - ../../secrets/jwt:/secrets/jwt:ro
    ports: ["8080:8080"]
    depends_on: *app-deps

  exchange-engine:
    <<: *exchange
    profiles: [app]
    command: ["serve", "--role=engine"]
    environment:
      <<: *exchange-env
      DATABASE_URL: postgres://ex_engine:${POSTGRES_PASSWORD}@postgres:5432/exchange?sslmode=disable
    depends_on: *app-deps

  exchange-chain:
    <<: *exchange
    profiles: [app]
    command: ["serve", "--role=chain"]
    environment:
      <<: *exchange-env
      DATABASE_URL: postgres://ex_chain:${POSTGRES_PASSWORD}@postgres:5432/exchange?sslmode=disable
      REQUIRED_CONFIRMATIONS_DEFAULT: "1"
      SCAN_INTERVAL: 2s
      SIGNER_SUBJECT: cmd.signer.sign.default            # 拆分部署時以 NATS request-reply 呼叫 signer
    depends_on:
      <<: *app-deps
      anvil: { condition: service_healthy }
      exchange-signer: { condition: service_healthy }

  exchange-signer:                                       # 唯一掛載 keystore 的 role;無對外 port
    <<: *exchange
    profiles: [app]
    command: ["serve", "--role=signer"]
    environment:
      <<: *exchange-env
      DATABASE_URL: postgres://ex_signer:${POSTGRES_PASSWORD}@postgres:5432/exchange?sslmode=disable
      WALLET_KEYSTORE_DIR: /secrets/keystore
      WALLET_KEYSTORE_PASSPHRASE: ${WALLET_KEYSTORE_PASSPHRASE}
    volumes:
      - ../../secrets/keystore:/secrets/keystore:ro
    depends_on: *app-deps

  exchange-stream:
    <<: *exchange
    profiles: [app]
    command: ["serve", "--role=stream"]
    environment:
      <<: *exchange-env
      DATABASE_URL: postgres://ex_stream:${POSTGRES_PASSWORD}@postgres:5432/exchange?sslmode=disable
      WS_ADDR: ":8081"
    ports: ["8081:8081"]
    depends_on: *app-deps

  exchange-admin:
    <<: *exchange
    profiles: [app]
    command: ["serve", "--role=admin"]
    environment:
      <<: *exchange-env
      DATABASE_URL: postgres://ex_admin:${POSTGRES_PASSWORD}@postgres:5432/exchange?sslmode=disable
      ADMIN_ADDR: ":8082"
      ADMIN_BOOTSTRAP_EMAIL: ${ADMIN_BOOTSTRAP_EMAIL:-admin@example.com}
      ADMIN_BOOTSTRAP_PASSWORD: ${ADMIN_BOOTSTRAP_PASSWORD}
      WEBHOOK_SIGNING_KEY: ${WEBHOOK_SIGNING_KEY}
    ports: ["127.0.0.1:8082:8082"]
    depends_on: *app-deps

  exchange-worker:
    <<: *exchange
    profiles: [app]
    command: ["serve", "--role=worker"]
    environment:
      <<: *exchange-env
      DATABASE_URL: postgres://ex_worker:${POSTGRES_PASSWORD}@postgres:5432/exchange?sslmode=disable
      WEBHOOK_SIGNING_KEY: ${WEBHOOK_SIGNING_KEY}
    depends_on: *app-deps

  exchange-all:                                          # 單容器:所有 role 同進程,signer 為 in-proc 呼叫
    <<: *exchange
    profiles: [single]
    command: ["serve", "--role=all"]
    environment:
      <<: *exchange-env
      DATABASE_URL: postgres://ex_all:${POSTGRES_PASSWORD}@postgres:5432/exchange?sslmode=disable
      HTTP_ADDR: ":8080"
      WS_ADDR: ":8081"
      ADMIN_ADDR: ":8082"
      JWT_JWKS_URL: http://127.0.0.1:8080/.well-known/jwks.json
      JWT_PRIVATE_KEY_FILE: /secrets/jwt/ed25519.pem
      WALLET_KEYSTORE_DIR: /secrets/keystore
      WALLET_KEYSTORE_PASSPHRASE: ${WALLET_KEYSTORE_PASSPHRASE}
      WEBHOOK_SIGNING_KEY: ${WEBHOOK_SIGNING_KEY}
      ADMIN_BOOTSTRAP_EMAIL: ${ADMIN_BOOTSTRAP_EMAIL:-admin@example.com}
      ADMIN_BOOTSTRAP_PASSWORD: ${ADMIN_BOOTSTRAP_PASSWORD}
      REQUIRED_CONFIRMATIONS_DEFAULT: "1"
      SCAN_INTERVAL: 2s
    volumes:
      - artifacts:/artifacts:ro
      - ../../secrets/jwt:/secrets/jwt:ro
      - ../../secrets/keystore:/secrets/keystore:ro
    ports: ["8080:8080", "8081:8081", "127.0.0.1:8082:8082"]
    depends_on:
      <<: *app-deps
      anvil: { condition: service_healthy }

  prometheus:
    image: prom/prometheus:v2.54.1
    profiles: [observability]
    volumes:
      - ../../infra/observability/prometheus.yml:/etc/prometheus/prometheus.yml:ro
      - prom-data:/prometheus
    ports: ["127.0.0.1:9090:9090"]
    restart: unless-stopped
    networks: [exchange-net]

  grafana:
    image: grafana/grafana:11.2.0
    profiles: [observability]
    environment:
      GF_SECURITY_ADMIN_PASSWORD: ${GRAFANA_ADMIN_PASSWORD:-admin}
      GF_AUTH_ANONYMOUS_ENABLED: "false"
    volumes:
      - ../../infra/observability/grafana/provisioning:/etc/grafana/provisioning:ro
      - ../../infra/observability/dashboards:/var/lib/grafana/dashboards:ro
      - grafana-data:/var/lib/grafana
    ports: ["127.0.0.1:3000:3000"]
    depends_on: [prometheus]
    restart: unless-stopped
    networks: [exchange-net]

networks:
  exchange-net: {}

volumes:
  pg-data: {}
  nats-data: {}
  anvil-data: {}
  artifacts: {}
  prom-data: {}
  grafana-data: {}
```

注意事項:

- 應用程式仍自帶指數退避重試;`depends_on` 只是加速,不是正確性依賴。
- `infra/postgres/initdb/01-roles.sh` **只建登入角色**(`ex_migrate` 為 schema owner;`ex_api`、`ex_engine`、`ex_chain`、`ex_signer`、`ex_stream`、`ex_admin`、`ex_worker`、`ex_all`)與 `ALTER DEFAULT PRIVILEGES`;schema 一律由 goose `0001_bootstrap.sql` 以 `CREATE SCHEMA IF NOT EXISTS` 建立(避免 initdb 與 migration 重複建 schema);Helm / 外部 Postgres 以 `scripts/db-roles.sql` 執行相同角色腳本。dev 共用 `POSTGRES_PASSWORD`,prod 各自 `DB_PASSWORD_<ROLE>`。
- E2E / CI 每次乾淨啟動(`docker compose down -v`),不依賴 `anvil-data`;本機開發預設保留狀態,`make reset` 同時清 `pg-data`、`nats-data`、`anvil-data`、`artifacts`。
- 部署合約的 deployer 是 anvil 帳戶 #0、nonce 0,地址可預期;`Deploy.s.sol` 先檢查該地址是否已有 code,有則跳過(冪等)。
- `compose.prod.yaml`(beta VM)覆蓋:移除 anvil / contracts-deployer(`seed` 的 `depends_on` 以 `!override` 只留 `migrate`,`command` 改讀 `/config/sepolia-addresses.json`;需 Compose ≥ 2.24)、`ETH_RPC_URL` 指向 Sepolia provider、不開 5432/6379/4222 port、加 `logging` 與 `deploy.resources`。
- prometheus / grafana 屬 `observability` profile,但 `make up` **預設帶上**(原則 9);只有 CI 的 e2e job 以 `OBS=0` 略過以省時間。

Makefile 目標:

| 目標 | 說明 |
|---|---|
| `make up` / `make up-single` | `--profile infra --profile observability --profile app`(或 `single`)`up -d --build --wait`;`OBS=0` 略過 observability |
| `make down` / `make reset` | 停止;`reset` = `down -v` + 刪 `artifacts` + 重新 `up` |
| `make infra-up` | 只起 infra profile,讓本機 `go run` 連進去 |
| `make run ROLE=engine` | `go run ./cmd/exchange serve --role=$(ROLE)`(讀 `.env`,`*_URL` 改 localhost) |
| `make migrate` / `make seed` | 對本機 Postgres 執行 |
| `make gen` | `oapi-codegen`(public/admin server + client)、`sqlc generate`、`abigen`、`go generate ./...` |
| `make lint` | `golangci-lint run` |
| `make test` | `go test -race -short ./...`(單元 + 屬性) |
| `make test-fuzz` | 每個 fuzz target 跑 `FUZZ_TIME`(預設 30s) |
| `make test-integration` | `go test -race -tags integration ./test/integration/... ./internal/...` |
| `make e2e` | `up --wait` + `exchangectl e2e` + `down -v` |
| `make build` / `make image` | 本機 binary / `docker build` |
| `make gen-dev-secrets` | 產生 `.env`(從 `.env.example`)、`secrets/jwt/ed25519.pem`;以 foundry 映像執行 `cast wallet new-mnemonic --json` 寫入 `secrets/dev-mnemonic.txt`(gitignored),並以 `cast wallet address --mnemonic "$(cat secrets/dev-mnemonic.txt)" --mnemonic-derivation-path "m/44'/60'/1'/0/0"` 寫入 `.env` 的 `HOT_WALLET_ADDRESS`。**Phase 0 不需要任何 Go 鏈上程式**;`secrets/keystore/hd-seed.json` 由 Phase 4a 的 `exchange keys import-mnemonic` 產生 |
| `make demo` | `exchangectl demo`(第 12 節各 Phase 的展示腳本) |
| `make trace ID=...` | 以 `correlation_id` grep 所有容器 log |
| `make loadgen` | `exchangectl loadgen --market ETH-USDC --rate 1000 --duration 60s --accounts 100`(命令平均分配到 N 個帳戶以避開 per-account 限流;`--bypass-ratelimit` 僅限 `EXCHANGE_ENV=dev`) |
| `make kind-up` / `make helm-lint` / `make helm-e2e` | Phase 7:kind 叢集、chart lint、chart 安裝 + E2E |

## 12. 分階段計畫

每個 Phase 固定欄位:目標、範圍、產出物、需要的 Go 能力、需要補的領域概念(附建議閱讀)、任務清單(顆粒度 0.5–2 天)、DoD(可自動驗證,對應 CI job)、展示腳本、預估工時(全職週)、風險與退路。工時總計約 24–36 週;沒有硬期限,**DoD 不過不進下一階段**。

### Phase 0 — 領域建模 + walking skeleton

**目標**:在寫任何業務邏輯之前把領域模型寫成文件並驗算;建立單一 module、單一 binary、compose、CI 的骨架,讓「一個 endpoint 從 OpenAPI 到 Postgres 到 testcontainers 到 CI 綠燈」全鏈路跑通。之後各 Phase 不再重選工具。

**範圍**:做 — `docs/domain.md`、ADR ×8、`internal/money`、`internal/app`、`internal/telemetry`、registry 表與 `GET /v1/markets`、compose infra + migrate + seed、CI、`exchangectl` 骨架、`make gen-dev-secrets`。不做 — 撮合、帳本、auth、任何鏈上程式碼(compose 裡的 anvil 與 deployer 要能跑,但沒有 Go 程式碼碰它)。

**產出物**:可 `make up --wait` 全綠的 compose;`GET /v1/markets` 回 seed 的 `ETH-USDC`;`exchangectl markets list`;CI 五個 job(lint、unit、integration、compose-config、image)+ gen-check 綠燈;文件與 ADR。

**需要的 Go 能力**:module 與 `internal/`、struct/method、值型別設計、table-driven test、`context` 基礎、`signal.NotifyContext`、`net/http` + chi、`embed.FS`、`go:generate`、slog、Dockerfile。

**需要補的領域概念**:複式記帳(Fowler《Analysis Patterns》Accounting 章 / martinfowler.com/apsupp/accounting.pdf;TigerBeetle docs「Debits and Credits」);訂單簿與 price-time priority(Harris《Trading and Exchanges》第 4、6 章;任一開源 Go orderbook 的資料結構與測試,只讀不抄);transactional outbox(microservices.io);Idempotency-Key(IETF draft);Coinbase / Kraken 公開 API 文件中的 depth snapshot + delta 協定。

**任務清單**:

- [ ] 讀 Fowler Accounting、TigerBeetle Dr/Cr、Harris 訂單簿章節(1.5 d)
- [ ] 寫 `docs/domain.md`:逐條驗算第 6.1.4 的分錄、6.2 轉移表、6.4 狀態機;找出至少一處本文件的錯誤並修正(1 d)
- [ ] 寫 ADR 0001–0008(見第 20 節清單)(1 d)
- [ ] `go mod init github.com/arc119226/crypto-exchange`;`cmd/exchange`(cobra:`serve`、`migrate`、`healthcheck`、`version`);`internal/app`:typed config、run loop、`/healthz` `/readyz` `/metrics`、SIGTERM drain、依賴指數退避重試(1.5 d)
- [ ] `internal/telemetry`:slog JSON、`CorrelationMiddleware`(`X-Request-Id` → context → log 欄位)、Prometheus registry(1 d)
- [ ] `internal/money`:`Amount`、`Asset`、`ParseAmount`、`Add/Sub/Mul/Cmp`、`RoundUp/RoundDown/Truncate(scale)`、`IsMultipleOf(step)`、JSON string 序列化;`internal/platform/pg`:`pgtype.Numeric` ↔ `money.Amount` helper(`money` 本身不 import pgx);table tests 含 0、負數、1 wei、2^256−1、`Exp ≠ 0`(1.5 d)
- [ ] `.golangci.yml`:depguard(第 8 節規則)、forbidigo(禁 float)、errcheck、staticcheck、gosec(0.5 d)
- [ ] `build/Dockerfile` multi-stage(先 `COPY go.mod go.sum` + `go mod download`)、`exchange healthcheck` 子命令(0.5 d)
- [ ] `deploy/compose/compose.yaml` 第 11 節;`infra/postgres/initdb/01-roles.sh`;`infra/contracts`(`MockUSDC.sol` 6 decimals、`Deploy.s.sol` 冪等部署 + 注資 + 輸出 `addresses.json`);`.env.example`;`scripts/gen-dev-secrets.sh`(2 d)
- [ ] goose `0001_bootstrap.sql`(schemas、`tenant` 慣例、house accounts seed 留給 Phase 2)、`0002_registry.sql`(assets、markets、fee_schedules、withdrawal_limits);`sqlc.yaml` + `registry` 查詢;`exchange seed`(讀 `addresses.json` upsert ETH、USDC、ETH-USDC、預設 fee schedule)(1 d)
- [ ] `api/public/v1/openapi.yaml` 初版(`GET /v1/assets`、`GET /v1/markets`、`GET /v1/markets/{symbol}`、問題型別、金額 string pattern + `x-go-type`);`oapi-codegen` chi strict-server;`internal/api` 實作(1 d)
- [ ] `test/integration`:testcontainers Postgres → 跑完整 migration → seed → HTTP 呼叫 `GET /v1/markets`(1 d)
- [ ] `.github/workflows/ci.yml`:lint、unit、integration、`docker compose config`、image build;`make gen && git diff --exit-code` 檢查產物同步(0.5 d)
- [ ] `cmd/exchangectl`(cobra + 產生的 client):`markets list`、`assets list`(0.5 d)
- [ ] `README.md`:產品邊界一段、`make up` 快速開始(0.5 d)

**DoD(CI job)**:`lint`(depguard 與 forbidigo 生效並有一個故意違規的測試檔證明會被擋,合併前移除)、`unit`(`internal/money` 覆蓋率 ≥ 95%)、`integration`(migration + seed + `GET /v1/markets` 回 `ETH-USDC`,`price_tick = "0.01"`)、`compose-config`、`image`;`docker compose --profile infra --profile single up --wait` 全部 healthy(含 anvil、contracts-deployer 完成、`addresses.json` 存在);`docs/domain.md` 與 8 份 ADR 合併。

**展示腳本**:

```
make gen-dev-secrets && make up-single
curl -s localhost:8080/v1/markets | jq
exchangectl markets list
curl -s localhost:9100/readyz; curl -s localhost:9100/metrics | grep exchange_build_info
cast call $USDC_ADDRESS "decimals()(uint8)" --rpc-url localhost:8545     # 6
```

**預估工時**:3–4 週(任務合計約 15 個工作天含領域閱讀;這是唯一沒有緩衝的 Phase,故放寬)。

**風險與退路**:foundry 映像 tag 與 `--state` 行為隨版本變化 → pin 後寫進 ADR-0008,CI 與本機同 tag;`oapi-codegen` 對 `x-go-type` 的 import 設定卡住 → 先用 string 型別 + handler 內轉換,不阻塞;領域閱讀超時 → 第 6 節已是可執行規格,閱讀以「能解釋每筆分錄為何平衡」為止。

### Phase 1 — 撮合純 library + CLI

**目標**:`internal/matching` 成為無 I/O、確定性、可屬性/模糊測試的訂單簿;`exchangectl replay` 能讀命令檔輸出事件與最終簿。

**範圍**:做 — 限價 GTC、市價(quote/base 語意、IOC 剩餘取消)、取消、部分成交、STP `cancel_newest`、tick/step 檢查、`Restore`/`Snapshot`、事件型別、benchmark、golden files。不做 — 任何 DB、NATS、HTTP、時間、goroutine。

**產出物**:`internal/matching` + 測試;`test/fixtures/matching/*.jsonl` 命令序列與 golden 事件;`exchangectl replay`;`docs/domain.md` 補撮合語意的驗算案例。

**需要的 Go 能力**:slice/map/sort、`sort.Search`、值 vs 指標接收者、interface 最小化、error 型別與 sentinel、table-driven + golden test、`go test -fuzz`、`rapid`、`testing.B`、`pprof` 初步。

**需要補的領域概念**:maker/taker、被動方定價、部分成交的剩餘量語意、IOC、自成交的三種策略、為何成交價落在雙方限價之間(Harris 第 6 章;Kraken / Binance 的 order type 文件)。

**任務清單**:

- [ ] 型別:`Side`、`OrderType`、`TimeInForce`、`Command{NewOrder, Cancel}`、`Event{Accepted, Filled(Trade), Updated, Cancelled, Rejected}`、`RestingOrder`;所有金額 `money.Amount`(0.5 d)
- [ ] 資料結構:`level{price, fifo}`、`side{levels map, sorted prices}`、`Book{bids, asks, byID}`;`insert/remove/best`(1 d)
- [ ] 限價撮合:吃單迴圈、部分成交、剩餘掛簿;成交價 = maker 價(1 d)
- [ ] 市價買(quote)/市價賣(base)、IOC 剩餘取消、空簿拒單、`max_slippage_bps`(1 d)
- [ ] 取消、STP `cancel_newest`(`allow` 供測試)(0.5 d)
- [ ] tick/step/min_notional 驗證函式(`money.IsMultipleOf`)(0.5 d)
- [ ] `Restore([]RestingOrder)`、`Snapshot()`(depth N 與完整)、`Equal`(0.5 d)
- [ ] table-driven 測試:第 6.1.4 例、6.2 每個轉移、價差情境、跨多價位掃單、同價位 FIFO(1.5 d)
- [ ] `rapid` 屬性測試:第 6.3 全部不變量(1 d)
- [ ] `FuzzApply`:隨機命令序列不 panic、重放一致、守恆(0.5 d)
- [ ] benchmark:10 萬掛單簿上 `Apply` 延遲;`pprof` 看一次熱點(0.5 d)
- [ ] `exchangectl replay --file cmds.jsonl [--snapshot]`;golden files 進 `test/fixtures`(1 d)

**DoD(CI)**:`unit`:`go test -race ./internal/matching` 綠、`rapid` 每個屬性 ≥ 1,000 案例;`fuzz-smoke`:`FuzzApply` 30 s 無失敗;benchmark 單執行緒 ≥ 100,000 `Apply`/s(記錄數字,不阻擋);`replay` 對 5 個 golden fixture 輸出逐位元一致;學習檢查點(寫在 PR 描述):能解釋為何每價位 FIFO 而非單一排序 slice、為何 `Apply` 不能呼叫 `time.Now()`。

**展示腳本**:

```
exchangectl replay --file test/fixtures/matching/partial_fill_price_improvement.jsonl
go test -bench=BenchmarkApply -benchmem ./internal/matching
go test -fuzz=FuzzApply -fuzztime=30s ./internal/matching
```

**預估工時**:2–3 週。

**風險與退路**:自成交與市價單邊界案例爆炸 → 先只做限價 + 取消通過所有不變量,再加市價、再加 STP,每一步都有 golden;決定性被 map 迭代順序破壞 → 迭代一律走排序後的 prices slice,測試以 `-count=20` 抓非決定性。

### Phase 2 — 帳本 + Postgres

**目標**:`internal/ledger` 落地第 6.1 全部規則;任意操作序列後試算平衡為零、快取與 journal 一致、重放冪等;提供管理員調帳(dev faucet)端點作為 Phase 2–3 的資金來源。

**範圍**:做 — schema、`Post/Hold/Release/Settle/Credit`、balances 快取與行鎖、試算平衡、`ledger.accounts` 與 house 科目 seed、admin OpenAPI 初版(`POST /admin/v1/ledger/adjustments`、`GET /admin/v1/ledger/trial-balance`、`GET /admin/v1/accounts/{id}/balances`)、`audit` 最小版、testcontainers。不做 — 訂單、事件、auth(admin 端點暫以 `ADMIN_API_KEY` header 保護,Phase 3 換成 JWT)。

**產出物**:`migrations/ledger`、`internal/ledger`、`internal/audit`、admin 契約初版、`exchangectl admin fund`。

**需要的 Go 能力**:`pgx` 連線池、`Begin/Commit/Rollback` 與 `defer`、`SELECT ... FOR UPDATE` 與鎖順序、`sqlc` 產生碼、`goose` embed、`errors.Is/As` 對 `pgconn.PgError`(唯一鍵衝突 → 冪等命中)、context 逾時、`errgroup` 在測試中併發打帳本。

**需要補的領域概念**:借貸與正常餘額、試算平衡、為何 posting 不可改、冪等鍵設計、死鎖與鎖順序(Postgres 文件 13 章)。

**任務清單**:

- [ ] `0003_ledger.sql`:`accounts`、`journal_entries(id, tenant_id, idempotency_key UNIQUE, kind, ref_type, ref_id, correlation_id, created_at)`、`postings(entry_id, account_id, asset, bucket available|hold|house, direction, amount)`、`balances`;deferred trigger 檢查每資產借貸相等;權限用「只 GRANT 需要的」而非 REVOKE(Postgres 對表不預設授 PUBLIC 權限,`REVOKE … FROM PUBLIC` 是 no-op):`GRANT SELECT, INSERT ON ledger.postings, ledger.journal_entries TO ex_engine, ex_chain, ex_admin, ex_all`、`GRANT SELECT … TO ex_api, ex_stream, ex_worker`、`GRANT INSERT ON ledger.accounts TO ex_api`(註冊建帳戶);不授任何角色 UPDATE/DELETE;app 角色不得為 owner(1 d)
- [ ] house 科目 seed(每個 `house_code` 一列,與資產無關;asset 在 posting 上);`Accounts.CreateSpot(user_id)`(0.5 d)
- [ ] `Post(tx, entry)`:應用層 sum-zero assert、冪等命中回原 entry、balances 更新(先按 `(account_id, asset)` 排序再 `FOR UPDATE`)、`CHECK` 違反 → `ErrInsufficient`(1.5 d)
- [ ] `Hold/Release/Settle/Credit` 包裝;`Settle(tx, trade, ledger.FeeParams{MakerBps, TakerBps, BaseScale, QuoteScale}) (makerFee, takerFee, error)` 自算手續費與價差 release(`FeeParams` 為 ledger 自有型別,由 trading 從 registry 轉換);`SettleBatch`(同一交易多筆)(1.5 d)
- [ ] `TrialBalance`、`Balances`、`Entries` 查詢(sqlc)(0.5 d)
- [ ] `internal/audit`:`audit_events` 表 + `Record`(0.5 d)
- [ ] `api/admin/v1/openapi.yaml` 初版 + `internal/admin` REST 實作(調帳、試算平衡、餘額)(1 d)
- [ ] 單元測試(以 6.1.4 每個範例為案例)+ testcontainers 整合測試(1 d)
- [ ] `rapid` 屬性測試:隨機 Hold/Release/Settle/Credit 序列(含故意重放)→ 試算平衡 = 0、快取 = 推導、hold 不變量(1 d)
- [ ] 併發測試:`errgroup` 100 goroutine 對同兩帳戶互轉,`-race` 通過、無死鎖(0.5 d)
- [ ] `exchangectl admin fund --account A --asset USDC --amount 10000 --reason dev`(0.5 d)

**DoD(CI)**:`unit` + `integration` 綠;屬性測試 ≥ 500 序列;「同一 `idempotency_key` 重放兩次餘額不變」「試算平衡恆為 0」「`available < 0` 必被拒」為明確測試名稱;admin 調帳寫入 `audit_events`;`ledger_trial_balance_diff` 指標存在。

**展示腳本**:

```
exchangectl admin fund --account $A --asset USDC --amount 10000 --reason "dev faucet"
exchangectl admin trial-balance          # 每資產 0
psql ... -c "select * from ledger.postings order by id desc limit 6"
```

**預估工時**:2–3 週。

**風險與退路**:`pgtype.Numeric` 轉換精度問題 → Phase 0 helper 已測邊界,出錯就加案例;鎖順序死鎖 → 排序鎖 + 測試;trigger 效能 → v1 規模可接受,必要時改為僅應用層 assert 並保留 trigger 於測試環境。

### Phase 3 — trading 狀態機 + outbox/JetStream + OpenAPI + 最小 auth

**目標**:端到端「註冊 → 入金(調帳)→ 下單 → 成交 → 餘額變動」走 REST;事件進 JetStream;引擎 `kill -9` 後自動恢復;`client_order_id` 冪等;最小 auth 與限流。這是最難也最長的階段。

**範圍**:做 — `trading`(狀態機、per-market goroutine、`CommandBus` in-proc 與 NATS request-reply、orders/trades/market_sequences)、`eventbus`(envelope、outbox、relay、JetStream streams/consumers、`processed_events`)、`policy` 最小版(市場狀態、帳戶凍結、allow-all 限額)、`auth`(register/login/refresh/logout、EdDSA JWT、JWKS、API key + HMAC、role claim、argon2id)、`api` 全部 trading / account / registry 端點、Redis 限流(登入 per IP + per account;下單/取消 per account)、`registry.Reload` + `market.updated` → engine reload、`api/events/v1`、`exchangectl e2e` 第一版。不做 — WebSocket、鏈上、後台 UI、TOTP、webhook。

**產出物**:可用的交易 API;JetStream 中可用 `nats sub` 觀察事件;恢復整合測試;`docs/events.md`。

**需要的 Go 能力**:goroutine/channel/select、每市場 actor、`context` 取消傳播、`errgroup` 收斂、`sync.Once`、JetStream durable consumer 與 ack、NATS request-reply 逾時、HTTP 中介層鏈、JWT/JWKS、HMAC、`time.Ticker`、PG advisory lock、`LISTEN/NOTIFY`。

**需要補的領域概念**:訂單生命週期與「接受即回」語意、outbox 模式與 at-least-once、事件序號與缺口偵測、JWT 與 API key 簽章慣例(Binance/Kraken API 文件的簽章章節)、限流 token bucket。

**任務清單**:

- [ ] `0004_trading.sql`:`orders`(第 6.2 欄位、UNIQUE `(tenant_id, account_id, client_order_id)`、索引 `(market_id, status)`)、`trades`、`market_sequences(market_id, last_seq)`;`0005_eventbus.sql`:`outbox`、`processed_events`(0.5 d)
- [ ] `eventbus`:`Envelope`、`Outbox.Append(tx, ...)`、`Relay`(LISTEN/NOTIFY + 輪詢、順序發布、`Nats-Msg-Id`)、JetStream stream/consumer 宣告(冪等建立)、`Subscriber` 含 `processed_events` 冪等模板(2 d)
- [ ] `trading` 狀態機與 repo:`PlaceOrder` 驗證 → policy → 送命令;`CancelOrder`;查詢(1.5 d)
- [ ] per-market runner:goroutine + channel、交易流程(第 5.2 步驟 4)、seq 分配、commit 失敗重建、`Start` 時 `Restore` open orders、advisory lock、`Reload`(2 d)
- [ ] `CommandBus`:in-proc;NATS request-reply 實作(`cmd.trading.{tenant}.{market}`,逾時 → 503)(1 d)
- [ ] `policy.OrderPolicy`(市場狀態、`ledger.accounts.status`——引擎不認識 user;admin 凍結用戶時同步把其所有帳戶設為 `frozen`、預留限額)(0.5 d)
- [ ] `PUT /admin/v1/markets/{id}/status` 最小實作(暫以 admin API key 保護,發 `market.updated`),供 engine `reload` 整合測試;完整 registry 後台在 Phase 5(0.5 d)
- [ ] `auth`:schema、register/login、argon2id、Ed25519 JWT(15 min)+ refresh(7 d,hash 存 `auth.refresh_tokens`,撤銷 = 刪列,不用 Redis)、JWKS、API key(`X-API-KEY/TIMESTAMP/SIGNATURE`,±30 s,scopes;API key 請求由 `api` 鑄 5 分鐘內部 JWT(`aud=internal`)後轉發給 engine/chain)、`RequireUser/RequireScope` 中介層、`exchange admin bootstrap`(2 d)
- [ ] `api`:全部 public 端點(第 7.4)、RFC 7807 錯誤、`client_order_id` 冪等回原單(200)、Redis token bucket 限流(無 Redis 降級 in-memory)(2 d)
- [ ] `api/events/v1/*.json` + `docs/events.md` + golden-file 序列化測試(1 d)
- [ ] 整合測試:testcontainers PG + NATS(-js);「殺 engine 再啟動,book 與 hold 一致」(用 `Book.Equal` 對比 kill 前快照與重建結果)、「重放同一批事件兩次餘額不變」、「seq 連續無缺口」、「同一 `client_order_id` 重送 10 次只產生一張單一筆 hold」(2 d)
- [ ] `exchangectl`:`user register/login`、`order place/cancel/list`、`book`、`balances`、`e2e`(register×2 → fund → 限價/市價互下 → 驗餘額與試算平衡)(1.5 d)
- [ ] E2E job:compose `single` profile `up --wait` → `exchangectl e2e` → `docker kill -s KILL exchange-all` → 再 `up --wait` → 驗證 open orders 與 book 一致(1 d)
- [ ] 指標:`trading_command_queue_depth`、`trading_apply_duration_seconds`、`outbox_backlog`、`engine_rebuild_duration_seconds`、`http_request_duration_seconds`(0.5 d)

**DoD(CI)**:`unit`、`integration`(含 kill/restart、冪等、事件重放、seq 連續)、`e2e` 綠;`nats stream info EX_TRADING` 顯示事件;同一帳戶登入錯第 6 次觸發 429(per account 5/min);p99 下單延遲(loadgen 100 orders/s 60 s,`--accounts 100`)< 50 ms 記錄於 PR;`docs/events.md` 與 schema 檔同步(golden 測試)。

**展示腳本**:

```
exchangectl e2e --verbose                       # 完整流程 + 斷言
nats --server localhost:4222 sub 'ex.v1.trade.>'   # 另一終端觀察成交事件
docker kill -s KILL crypto-exchange-exchange-all-1 && docker compose up -d --wait
exchangectl book ETH-USDC                       # 與 kill 前一致
```

**預估工時**:4–6 週。

**風險與退路**:goroutine/context 生命週期 bug → runner 先寫成單一函式 `processOne(cmd)` 可同步測試,再包 goroutine;`-race` 必開;NATS request-reply 在 `all` 模式不需要 → 先只做 in-proc,多容器 compose 留到本 Phase 尾;auth 拖太久 → API key 先行(程式化測試夠用),email/password 與 refresh 後補;JetStream 概念混淆 → 先讓 relay 發、`nats` CLI 訂,再寫第一個 Go consumer(`admin-projection` 可最簡單)。

### Phase 4 — 鏈上(4a 充值、4b 提現、4c 歸集、4d Sepolia)

**目標**:在 anvil 上自動化跑完 充值 → 入帳 → 提現(自動與審核)→ 確認 → 歸集 → 對帳;Sepolia 手動驗證一次確認數、reorg 容忍與 gas。

**範圍**:做 — `chain/evm`、`hdwallet`、`signer`、`deposit`、`withdrawal`、`sweep`、`policy.WithdrawalPolicy`、admin 審核 REST(`approve/reject/resolve`,先 CLI 操作)、鏈上對帳查詢、`abigen` binding、anvil 整合測試(`anvil_mine`、`evm_snapshot/evm_revert`、`anvil_reorg`、`evm_setAutomine false`)。不做 — 後台 UI、webhook、合約內部轉帳充值、CREATE2。

**子階段**:

- **4a 充值(2 週)**:`exchange keys import-mnemonic` 產生 `hd-seed.json`;`signer` role 內的 HD 派生與 `deposit_addresses` **預生成地址池**(index 由 DB sequence、`ADDRESS_POOL_MIN`);`GET /v1/deposit-address` 只做指派(`api` 不碰金鑰);scanner 輪詢、原生 ETH 與 ERC-20 路徑、確認數、reorg 回退(含 `orphaned → detected` 的 UPDATE 路徑)、`Credit` 入帳;`genesis_hash` 檢查;指標 `chain_scanner_lag_blocks`、`chain_head_block`。
- **4b 提現(2 週)**:`POST /v1/withdrawals` + `Idempotency-Key`;worker 狀態機(6.4.2);`WithdrawalPolicy`(限額表、kyc_level、每日累計);`NonceManager`;`KeystoreSigner` + 政策檢查 + 簽名審計;EIP-1559 費用與上限;receipt 追蹤、重送、`failed` 兩型;admin approve/reject/resolve。
- **4c 歸集 + 對帳(1 週)**:sweeper(ETH、ERC-20 兩段式)、`sweeps` 表、custody 分錄、熱錢包低水位告警事件、`reconciliation` 查詢(帳本 custody vs 鏈上)與 `exchangectl admin reconcile`。
- **4d Sepolia(0.5–1 週)**:`ETH_RPC_URL`/`ETH_CHAIN_ID`/確認數 6 切換;在 Sepolia 部署同一 `MockUSDC`;faucet 注資熱錢包;手動走充值 → 提現 → 歸集;記錄 gas 與確認時間到 `docs/runbooks/sepolia.md`;不進 CI。

**需要的 Go 能力**:`ethclient`(`BlockByNumber`、`FilterLogs`、`TransactionReceipt`、`PendingNonceAt`、`SuggestGasTipCap`、`FeeHistory`)、`abigen` 產生的 binding、`types.NewTx(&types.DynamicFeeTx{})`、`keystore`、`big.Int` 複製紀律、長時間執行的 worker loop 與 ticker、重試與 backoff、JSON-RPC 測試用 client(`rpc.Client.Call("anvil_mine")`)。

**需要補的領域概念**:確認數 vs finality、reorg 與 `removed` log、nonce 與 pending/latest 差異、EIP-1559(base fee / tip / max fee)、替換交易規則(同 nonce 費用 ≥ +10%)、BIP-32/39/44 路徑、ERC-20 `Transfer` topic 結構、歸集為何要先補 gas(Ethereum docs;goethereumbook.org;EIP-1559 / BIP-44 原文)。

**任務清單**:

- [ ] `0006_chain.sql`:`deposit_addresses`、`deposits`(UNIQUE `(chain_id, tx_hash, log_index)`)、`scan_cursors`、`blocks`、`chain_state`、`withdrawals`、`hot_wallets`、`idempotency_keys`、`sweeps`、`signing_log`(1 d)
- [ ] `chain/evm`:client 封裝、`ToWei/FromWei` round-trip 測試、fee 估算、receipt 輪詢 helper(1 d)
- [ ] `chain/hdwallet` + `exchange keys import-mnemonic`:讀 `secrets/dev-mnemonic.txt` → 以 scrypt + AES-256-GCM 寫 `secrets/keystore/hd-seed.json`;啟動解密後派生 `m/44'/60'/0'/0/{i}` 與熱錢包 `m/44'/60'/1'/0/0`;已知測試向量單元測試;`EnsurePool(min)` 地址池補充(1.5 d)
- [ ] `abigen` MockUSDC binding;`make gen` 納入(0.5 d)
- [ ] scanner:游標、批次掃描、兩條路徑、`blocks` 環與 parent_hash 檢查、reorg 回退、確認數推進、`Credit` 同交易、地址集合刷新(2.5 d)
- [ ] anvil 整合測試(testcontainers `GenericContainer`,`--no-mining` + `anvil_mine` 精準控制):N 確認後才入帳;`anvil_reorg` 後未入帳充值 orphaned、游標回退;`evm_snapshot/revert` 時同步 truncate DB(1.5 d)
- [ ] `WithdrawalPolicy` + `withdrawal_limits` 讀取 + 每日累計查詢(1 d)
- [ ] 提現 API(`Idempotency-Key` 中介層,存 request hash + response)(1 d)
- [ ] `chain/hotwallet.NonceManager`:啟動對帳、缺口回收(0 ETH 自轉填補、`nonce_fills`);`KeystoreSigner`(`SignRequest` 三種 kind 的政策檢查、`signing_log(kind, ref_id)` UNIQUE 防重簽、審計)+ `signer` role 的 NATS request-reply 服務(`cmd.signer.sign.{tenant}`)與 in-proc 版;`KMSSigner` stub(2 d)
- [ ] 提現 worker:每個狀態一個 handler、狀態與 nonce/raw tx 同交易、廣播、追蹤、重送、`failed` 分流、Release/pending_withdrawal 分錄(2.5 d)
- [ ] admin REST:pending_review 佇列、approve/reject/resolve(寫審計、記審核者);`exchangectl admin withdrawals ...`(1 d)
- [ ] 整合測試:限額內自動出金到 confirmed;超額進 pending_review → approve → confirmed;`evm_setAutomine false` 卡單 → 重送;worker `kill` 於 `signed` 後重啟不重複廣播(同 nonce);`Idempotency-Key` 重送 5 次一筆提現(2 d)
- [ ] sweeper(ETH、ERC-20 兩段)、custody 分錄、低水位事件;整合測試「歸集後用戶餘額不變、custody 總額不變」(2 d)
- [ ] 對帳查詢 + `exchangectl admin reconcile`;指標 `hot_wallet_balance`、`withdrawals_pending_review`、`withdrawal_state_duration_seconds`(1 d)
- [ ] `exchangectl e2e` 擴為完整:充值(`cast send` 到用戶地址 / USDC `transfer`)→ 入帳 → 下單成交 → 提現 → 確認 → 歸集 → 對帳零差異(1 d)
- [ ] 4d Sepolia:設定、部署、手動流程、runbook(2–4 d)

**DoD(CI)**:`integration` 新增 anvil 套件全綠(確認數、reorg、卡單重送、重啟不重複廣播、提現冪等、歸集守恆);`e2e` 跑完 2.3 第 1 條全流程且 `reconcile` 差異為 0;無私鑰進 log:整合測試以 `gen-dev-secrets` 產生的測試私鑰、助記詞、passphrase 為已知字串,啟動全流程後斷言所有容器 log 不含這些字串(含去 `0x` 形式),且 `internal/telemetry` 對 `SignRequest`、`*ecdsa.PrivateKey`、raw tx bytes 實作 `slog.LogValuer` 回 `[redacted]` 並有單元測試(tx hash / block hash 本來就會出現在 log,不能用「不含 0x + 64 hex」斷言;`gosec` 是靜態掃描,不檢查執行期 log);Sepolia runbook 含 tx hash 記錄。

**展示腳本**:

```
exchangectl deposit simulate --user alice --asset ETH --amount 1.5        # cast send 到 HD 地址
exchangectl deposits list --user alice                                   # detected → credited
exchangectl withdraw --user alice --asset ETH --amount 0.2 --to 0x...    # 限額內自動
exchangectl withdraw --user alice --asset ETH --amount 50 --to 0x...     # 進 pending_review
exchangectl admin withdrawals approve $ID
exchangectl admin reconcile                                              # diff = 0
```

**預估工時**:5–7 週。

**風險與退路**:`anvil_reorg` 在某些版本行為不同 → 以 `evm_snapshot/evm_revert` + 重新出不同區塊模擬,並 pin 版本;go-ethereum 版本升級破壞 API → `go.sum` 鎖定,升級獨立 PR;nonce 對帳邏輯在 Sepolia 遇到外部干擾 → 熱錢包只由本系統使用,文件明示;歸集複雜 → 4c 可先只做 ETH,ERC-20 兩段式下一輪;Sepolia faucet 取得困難 → 用多個 faucet 或延後 4d,不阻塞 Phase 5。

### Phase 5 — 管理後台 + 對帳 + 提現審核 + Webhook + TOTP

**目標**:營運方能完全透過後台完成設定、審核、對帳與整合管理;客戶系統能透過 webhook 收事件並寫入 kyc_level。

**範圍**:做 — htmx 後台(登入 + TOTP、儀表板、registry CRUD、用戶與 kyc_level/凍結、帳戶餘額與 journal 瀏覽、試算平衡、提現審核佇列、充值/歸集列表、對帳報表與 breaks、審計查詢、webhook 管理、引擎 reload);`webhook` dispatcher(worker role);排程對帳 job + `reconciliation_breaks`;`PUT /admin/v1/users/{id}/kyc-level`(admin API key);`kyc_level` 進 `WithdrawalPolicy`。不做 — 多語系、細粒度 RBAC(只有 admin)、報表匯出。

**產出物**:`internal/admin` 完整、`internal/webhook`、`docs/webhooks.md`、後台可用。

**需要的 Go 能力**:`html/template`(layout、partial、函式表)、`embed`、表單處理與 CSRF、伺服端 session(`auth.admin_sessions(id, user_id, totp_verified_at, expires_at)`,cookie 只放 session id;admin 不簽 JWT,所以 `admin` role 不需要 JWT 私鑰)、TOTP、HTTP client 逾時與重試、`time.Ticker` 排程、JetStream consumer 背壓。

**需要補的領域概念**:對帳(three-way:帳本內部、帳本 vs 鏈上、在途)、審計不可否認性、webhook 簽章與重放攻擊防護(Stripe / GitHub webhook 文件)。

**任務清單**:

- [ ] admin 登入 + TOTP enroll/confirm/verify、`RequireAdmin` cookie 中介層、CSRF(1.5 d)
- [ ] 版面與 htmx 基礎(layout、表格 partial、分頁、flash)(1 d)
- [ ] registry 頁面:assets / markets / fee_schedules / withdrawal_limits CRUD + 狀態切換 + 引擎 reload 按鈕;每次寫入審計 + `*.updated` 事件(2 d)
- [ ] 用戶頁:列表、詳情、kyc_level、凍結;`PUT /users/{id}/kyc-level` API(admin API key scope)(1 d)
- [ ] 帳本頁:試算平衡、帳戶餘額、journal 瀏覽(依 account / ref)、調帳表單(1 d)
- [ ] 提現審核佇列(approve/reject/resolve)、充值、歸集、熱錢包頁(1.5 d)
- [ ] 對帳 job(worker,每 5 min)+ `0007_admin.sql`(`reconciliation_reports`、`reconciliation_breaks`)+ 頁面 + `reconciliation.break_detected` 事件(1.5 d)
- [ ] 審計查詢頁(過濾 actor / action / target)(0.5 d)
- [ ] `webhook`:`0008_webhook.sql`、endpoint CRUD 頁、dispatcher(durable consumer、HMAC、退避、deliveries、dead)、replay(2 d)
- [ ] `docs/webhooks.md`、`exchangectl webhook-sink`(本機接收並驗簽的測試工具)(0.5 d)
- [ ] 測試:對帳能抓出人為植入的錯帳(直接 SQL 插一筆 posting 破壞平衡 → break);提現審核狀態機每個轉移;webhook 重試與簽名驗證;TOTP 錯碼鎖定(1.5 d)

**DoD(CI)**:`integration` 新增:植入錯帳 → `reconciliation_breaks` 有一筆;webhook 端點回 500 兩次後第三次成功,deliveries 有 3 筆;`kyc_level` 改變後提現限額生效;`unit`:template 渲染測試;E2E 增加「超額提現 → 後台 approve(透過 admin API)→ confirmed」。手動:後台 12 個頁面截圖進 `docs/screenshots/`。

**展示腳本**:

```
open http://localhost:8082/admin/login          # admin + TOTP
exchangectl webhook-sink --port 9999 &          # 本機收 webhook
exchangectl admin webhooks create --url http://host.docker.internal:9999 --events 'trade.executed,withdrawal.state_changed'
exchangectl e2e                                  # sink 印出已驗簽事件
```

**預估工時**:3–5 週。

**風險與退路**:後台頁面數量蔓延 → 先做審核、對帳、registry 三頁,其餘用 REST + CLI 過渡;htmx 不熟 → 全部 server-render 表單也可,htmx 只做局部刷新。

### Phase 6 — stream(公開/私有 WS、K 線)+ 參考前台 + 觀測性完善

**目標**:行情與私有推播可用、可重連補齊;參考前台能完整交易;Grafana 有可交付的儀表板與告警。

**範圍**:做 — `marketdata`(影子簿、depth delta、trades、ticker、kline 1m/5m/15m/1h/1d 聚合並持久化、Redis 快照)、`stream`(WS server、頻道、auth、resume、心跳、慢客戶端)、REST `depth/trades/ticker/klines`、`web/trade` SPA、`exchangectl loadgen`、dashboards + alert rules、OTel trace(選用)。不做 — 多副本 stream 的 sticky 問題(單副本)、TradingView 圖表(用 lightweight-charts 即可)。

**需要的 Go 能力**:WebSocket、每連線 writer goroutine + 有界 channel、`select` 含 `default` 丟棄策略、`sync.RWMutex` 讀多寫少、記憶體中的影子簿、時間視窗聚合、Redis 讀寫、pprof 看 goroutine 洩漏。

**需要補的領域概念**:depth 增量協定、K 線 OHLCV 聚合規則(以成交時間歸桶、空桶延續收盤價)、ticker 定義(24h 高低量)。

**任務清單**:

- [ ] `marketdata.BookProjector`:消費 order.* 事件維護影子簿(以 seq 對齊、缺口重建自 `GET /orders?status=open` 內部查詢)、產 depth delta(1.5 d)
- [ ] trades / ticker / kline 聚合器(worker 持久化 `marketdata.klines`;重啟從最後一根 K 線的 seq 重放)(1.5 d)
- [ ] REST 行情端點 + Redis 深度快照快取(0.5 d)
- [ ] `stream` server:連線、subscribe/unsubscribe、公開頻道、snapshot + delta、心跳、有界 buffer 斷線(2 d)
- [ ] 私有頻道:auth、依 account_id 扇出、`account_seq`、`resume` 從 outbox 補齊(1.5 d)
- [ ] `docs/ws-api.md` + 整合測試:重連補齊不漏不重、seq 缺口重抓、慢客戶端被斷而其他客戶端延遲不變(1.5 d)
- [ ] `web/trade`:登入、市場列表、訂單簿(WS)、下單表單、我的訂單/成交/餘額(私有 WS)、充值地址、提現;OpenAPI 產 TS client(4 d)
- [ ] `exchangectl loadgen` + 壓測報告(p99、吞吐、WS 延遲)(1 d)
- [ ] Grafana dashboards(引擎、帳本、鏈上、HTTP/WS、系統)+ alert rules(服務不健康、`ledger_trial_balance_diff != 0`、`chain_scanner_lag_blocks > 20`、`outbox_backlog > 1000`、熱錢包低水位、pending_review 積壓)(1.5 d)
- [ ] OTel trace(選用):HTTP → DB → NATS header → consumer;Tempo 或 Jaeger 在 observability profile(1 d)

**DoD(CI)**:`integration`:WS 重連補齊測試、慢客戶端測試、kline 聚合 golden;`e2e`:前台 Playwright 冒煙(登入 → 下單 → 訂單簿更新 → 餘額變動)(可放 nightly);壓測結果達 3.3 目標或記錄差距;dashboards JSON 與 alert rules 進 `infra/observability` 並被 compose 載入。

**展示腳本**:

```
websocat ws://localhost:8081/ws/v1/public -E <<< '{"op":"subscribe","channel":"depth","market":"ETH-USDC"}'
cd web/trade && npm run dev   # http://localhost:5173
make loadgen                  # 印 p50/p99、成交/秒、WS 推播延遲
open http://localhost:3000    # Grafana: Exchange Overview
```

**預估工時**:4–6 週。

**風險與退路**:前台吃時間 → 先做訂單簿 + 下單 + 餘額三個元件,其餘用 CLI;WS 扇出效能 → 單副本 1,000 連線目標,超過即記錄為限制。

### Phase 7 — Helm + beta 準備(備份、密鑰、runbook)

**目標**:同一 image 以 Helm 部署到 kind 並跑完 E2E;beta 環境(單台 VM compose prod profile 或單節點 k3s)可運維。

**範圍**:做 — Helm chart(每 role 一個 Deployment;engine、chain、signer 為 `replicas: 1` + `strategy: {type: Recreate}`——它們沒有 PVC(Postgres 為真相、keystore 是 Secret),不需要 StatefulSet,而且 StatefulSet 也沒有 Recreate 策略;雙實例由 PG advisory lock 防止;`terminationGracePeriodSeconds` ≥ engine drain 時間;migrate 為 pre-install/pre-upgrade Job、ConfigMap/Secret、Service/Ingress、probes、PDB 選用、NetworkPolicy 選用)、kind CI job、`compose.prod.yaml`、備份/還原腳本與演練、密鑰輪替 runbook、runbooks ×5、版本發布流程(tag → image → chart)、beta checklist。不做 — 多副本撮合、managed K8s 實裝、HA Postgres。

**需要的 Go 能力**:幾乎沒有新的;重點是 `-ldflags` 版本、`exchange version`、優雅關閉在 K8s 的 `terminationGracePeriodSeconds` 對應。

**需要補的領域概念**:交易系統的營運紀律(每日對帳、密鑰保管、事故記錄)。

**任務清單**:

- [ ] chart 骨架、values(image、roles、資源、env、secrets 引用)、probes 對應 `/healthz` `/readyz`(1.5 d)
- [ ] migrate Job hook、engine/chain/signer Deployment(replicas 1、Recreate)、Service/Ingress(api、stream、admin)(1 d)
- [ ] 依賴(postgres/nats/redis)以 subchart 或外部 values 二選一;kind 用 subchart(1 d)
- [ ] CI `helm` job:kind 建叢集 → `helm install` → port-forward → `exchangectl e2e`(anvil 以 chart 內測試 Deployment)(1.5 d)
- [ ] `compose.prod.yaml` + VM 佈署腳本;或 k3s values 覆蓋(1 d)
- [ ] 備份:`pg_dump` 每日 + WAL 歸檔到物件儲存;還原演練並記錄 RTO(1 d)
- [ ] 密鑰:JWT 金鑰輪替(JWKS 雙 kid)、keystore passphrase 更換、webhook secret 重設 runbook(1 d)
- [ ] runbooks:engine 重啟、卡住的提現、reorg 告警、熱錢包低水位、備份還原(1 d)
- [ ] 發布流程:`git tag vX.Y.Z` → GitHub Actions 推 image + chart package + release notes(0.5 d)
- [ ] beta checklist:`docs/beta-checklist.md`(限制清單、監控、告警接收人、對帳頻率)(0.5 d)

**DoD(CI)**:`helm` job 綠(kind 安裝 + E2E);`helm lint` 綠;還原演練文件含實測時間;`exchange version` 與 chart appVersion 一致(CI 檢查);所有 runbook 有「症狀 / 檢查指令 / 處置 / 驗證」四段。

**展示腳本**:

```
make kind-up && make helm-e2e
kubectl get deploy -n exchange        # engine / chain / signer 各 1 replica(Recreate)
scripts/backup.sh && scripts/restore-drill.sh
```

**預估工時**:2–3 週。

**風險與退路**:kind 上 anvil 與 StatefulSet 儲存問題 → 測試用 emptyDir + 每次乾淨部署;chart 複雜化 → 先單一 values 檔,不做多環境覆蓋。

## 13. 測試與 CI 策略

### 13.1 測試金字塔

| 層 | 對象 | 工具 | 執行 |
|---|---|---|---|
| 單元 | money、matching、ledger 計算、policy、狀態機純函式、template | `testing` + testify、golden files | `make test`,每次 push |
| 屬性 / 模糊 | matching 不變量、ledger 隨機序列、money 捨入、事件 envelope 序列化 | `rapid`、`go test -fuzz` | `make test`(rapid)、`fuzz-smoke` job 30–60 s |
| 整合 | ledger + PG、trading + PG + NATS、chain + anvil、webhook、stream | `testcontainers-go`,build tag `integration`,每個測試獨立 schema 或 truncate | `make test-integration`,PR |
| E2E | compose 全起 + `exchangectl e2e`、kill/restart、前台冒煙 | compose、Playwright(nightly) | `make e2e`,PR(main)與 nightly |
| 部署 | Helm on kind | kind、helm | Phase 7 起,PR |
| 壓測 | `loadgen` | 自製 | 手動 / nightly,結果進 docs |

### 13.2 不變量清單(屬性 / 模糊測試的斷言)

撮合:best bid < best ask;同價位 FIFO;成交價 = maker 價且 ∈ [買限價下界, 賣限價上界];base 與 quote 守恆;掛單剩餘量 ≥ 0 且為 step 倍數;命令序列重放後 `Snapshot` deep-equal;STP 後簿中無同帳戶可成交對敲;任何輸入不 panic;`Cancel` 後該單不在簿中且事件帶正確剩餘量。

帳本:每 entry 每資產 Σdebit = Σcredit;`balances` = journal 推導;`available/hold ≥ 0`;open order hold 守恆;冪等重放不變;試算平衡 = 0;house 恆等式(6.1.1)在只用內部操作時 external 為 0。

系統:seq 連續無缺口;`kill -9` engine 後 book 與 DB open orders 一致;事件重投遞不重複入帳;`client_order_id` / `Idempotency-Key` 重送只產生一筆;充值 N 確認前不入帳、reorg 後 orphaned;提現 worker 重啟不重複廣播;歸集不改用戶餘額。

### 13.3 CI workflow(`.github/workflows/ci.yml`)

```
lint ──► unit ──► fuzz-smoke ──► integration ──► e2e ──► image(main/tag)──► helm(kind, Phase 7+)
          │                                        │
          ├─► gen-check(oapi-codegen / sqlc / abigen 產物是否同步)
          └─► compose-config(docker compose config 對 dev 與 prod 覆蓋檔皆可解析)
                                                   └─► e2e-multi(nightly:infra + app 多容器,驗證 NATS request-reply 與跨容器 relay)
```

- `lint`:golangci-lint、`go vet`、`gitleaks`(禁止密鑰入庫)。
- `unit`:`go test -race -short -coverprofile`。
- `fuzz-smoke`:每個 `Fuzz*` 30 s;失敗語料進 `testdata/fuzz` 並成為回歸測試。
- `integration`:需要 Docker;`-tags integration -race -p 1`。
- `e2e`:`docker compose --profile infra --profile single up --wait` → `exchangectl e2e` → kill/restart 驗證 → `down -v`;收集容器 log 為 artifact。
- `e2e-multi`(nightly,Phase 3 尾起):`--profile infra --profile app up --wait`(api / engine / chain / signer / stream / admin / worker 各一容器)→ `exchangectl e2e` → 驗證 NATS request-reply 命令路徑、signer request-reply、relay 跨容器運作。
- `compose-config`:`docker compose -f compose.yaml config` 與 `-f compose.yaml -f compose.prod.yaml config` 皆可解析(用假 `.env`)。
- `image`:multi-arch 可選;`main` 推 `dev`,tag 推版本。
- 版本釘住:Go 以 `go.mod`;容器 image 以 tag;foundry 以 `.env` / CI 變數 `FOUNDRY_TAG`,本機與 CI 一致。

## 14. 安全邊界與密鑰清單

- 只有 `api`(8080)、`stream`(8081)、`admin`(8082,預設綁 127.0.0.1 / 內網)對外開 port;engine、chain、worker 只有 ops port(9100)且不對外。
- 身分傳遞:`api` 驗證用戶 JWT 後**原樣轉發**;API key 請求由 `api` 以同一把 Ed25519 私鑰鑄一枚 5 分鐘內部 JWT(`aud=internal`、含 `account_id`/`scopes`)後轉發;拆分部署時 engine / chain 收到的命令帶此 JWT,內部再以 JWKS 公鑰驗一次;不信任裸 header。
- JWT 私鑰只在 `api` role(`JWT_PRIVATE_KEY_FILE`;compose 只有 `exchange-api` 與 `exchange-all` 掛 `secrets/jwt`);其他角色只設定 `JWT_JWKS_URL`。`admin` 登入不簽 JWT,用伺服端 session(`auth.admin_sessions`,cookie 只放 session id),因此 `admin` role 也只需 JWKS。
- Postgres:每個 schema 一個 owner(`ex_migrate`),每個 process role 一個登入角色(`ex_api / ex_engine / ex_chain / ex_signer / ex_stream / ex_admin / ex_worker / ex_all`);權限採「只 GRANT 需要的」(`REVOKE … FROM PUBLIC` 對表是 no-op):`ledger.postings / journal_entries` 只有 `ex_engine`、`ex_chain`、`ex_admin`(調帳)、`ex_all` 有 `SELECT, INSERT`,其他角色只有 `SELECT`,無人有 UPDATE/DELETE;`ledger.accounts` 另授 `ex_api` INSERT(註冊建 spot 帳戶);`audit.audit_events` 只准 INSERT;`registry.*` 只有 `ex_admin` 可寫;`chain.signing_log` 只有 `ex_signer` 可寫;app 角色不得為 owner。
- 私鑰:只在 `signer` role(拆分部署為獨立容器,`role=all` 時為同進程模組;ADR-0007);`secrets/keystore/hd-seed.json`(gitignored;scrypt + AES-256-GCM)或 K8s Secret,compose 只有 `exchange-signer` 與 `exchange-all` 掛載;passphrase 走 `WALLET_KEYSTORE_PASSPHRASE`;`chain` role 不持有任何金鑰,只能送 `SignRequest`;log 中永不出現私鑰、助記詞、raw tx 以外的簽名材料(以已知測試密鑰字串斷言 + `slog.LogValuer` 遮罩);禁止使用 anvil 預設助記詞。
- 限流(Redis token bucket):`POST /auth/login` per IP 10/min + per account 5/min;`POST /orders`、`DELETE /orders/{id}` per account 20/s;`POST /withdrawals` per account 5/min;參數可設定。
- 審計:`audit.audit_events(id, tenant_id, actor_type user|admin|system|api_key, actor_id, action, target_type, target_id, before jsonb, after jsonb, ip, correlation_id, created_at)` append-only;涵蓋所有 `/admin/v1/*` 寫入、登入、API key 建立/刪除、提現每次狀態轉移、簽名請求、registry 變更、kyc_level 變更。
- 密鑰清單(`.env`,已在 `.gitignore`;`.env.example` 只放佔位;`make gen-dev-secrets` 產生):

| 變數 | 用途 | 持有角色 |
|---|---|---|
| `POSTGRES_PASSWORD`(prod:`DB_PASSWORD_<ROLE>`) | DB | 各 role |
| `JWT_PRIVATE_KEY_FILE` | Ed25519 簽章 | api |
| `WALLET_KEYSTORE_PASSPHRASE` + `WALLET_KEYSTORE_DIR`(`hd-seed.json`) | 熱錢包與 HD 種子 | signer(`role=all` 時為 all) |
| `ADMIN_BOOTSTRAP_EMAIL/PASSWORD` | 首個 admin(啟動時建立,之後可刪) | admin |
| `WEBHOOK_SIGNING_KEY` | 加密儲存各 endpoint secret 的主金鑰 | worker、admin |
| `ADMIN_API_KEY`(Phase 2 過渡)/ admin API key(Phase 3 起由系統簽發) | 客戶系統寫 kyc_level | api、admin |
| `ANVIL_DEPLOYER_KEY`、`HOT_WALLET_ADDRESS` | 開發鏈部署與注資 | contracts-deployer |
| `REDIS_PASSWORD`、`NATS_USER/PASSWORD`(beta 起) | 基礎設施 | 各 role |
| `GRAFANA_ADMIN_PASSWORD` | 觀測 | grafana |

- CI 用 GitHub Actions secrets 注入同名變數;`gitleaks` 在 lint job。
- 明列 out of scope:mTLS / 服務身分、WAF、DDoS、SIEM、HSM。

## 15. 可觀測性

- Log(slog JSON)固定欄位:`ts`, `level`, `msg`, `role`, `version`, `correlation_id`, `tenant_id`, `market_id`, `order_id`, `account_id`, `withdrawal_id`, `event_id`, `err`。`correlation_id` 來源:HTTP `X-Request-Id`(無則產生)→ context → DB `application_name`/journal `correlation_id` 欄位 → outbox `headers` → NATS header `Correlation-Id` → webhook header `X-Exchange-Correlation-Id`。
- 每個角色:`/healthz`(process 活著)、`/readyz`(DB、NATS、Redis 可用;engine 額外要求訂單簿重建完成與 advisory lock 取得;chain 額外要求 RPC 可達與 nonce 對帳通過)、`/metrics`。
- 指標最小集合:

| 指標 | 型別 | 說明 |
|---|---|---|
| `http_request_duration_seconds{route,method,status}` | histogram | API / admin |
| `trading_command_queue_depth{market}` | gauge | 每市場待處理命令 |
| `trading_apply_duration_seconds{market}` | histogram | 含 DB 交易 |
| `trading_orders_total{market,type,status}` | counter | |
| `engine_seq{market}` / `engine_rebuild_duration_seconds` | gauge / histogram | 恢復可觀測 |
| `outbox_backlog` / `outbox_relay_lag_seconds` | gauge | 未發布列數、最舊列年齡 |
| `event_consumer_lag{consumer}` | gauge | JetStream pending |
| `ledger_trial_balance_diff{asset}` | gauge | 告警 ≠ 0 |
| `ledger_entries_total{kind}` | counter | |
| `chain_head_block` / `chain_scanner_lag_blocks` / `chain_last_scanned_block` | gauge | |
| `deposits_total{asset,status}` / `withdrawals_total{asset,status}` | counter | |
| `withdrawal_state_duration_seconds{state}` | histogram | 停留時間 |
| `withdrawals_pending_review` / `hot_wallet_balance{asset}` / `reconciliation_diff{asset}` | gauge | |
| `ws_connections` / `ws_messages_sent_total{channel}` / `ws_slow_client_disconnects_total` | gauge / counter | |
| `webhook_deliveries_total{status}` / `webhook_dead_letters` | counter / gauge | |

- Trace(選用,Phase 6):OTel SDK,HTTP server/client、pgx、NATS publish/consume 各一個 span,`traceparent` 隨 NATS header 傳遞。
- Dashboard 最小集合(JSON 進 repo):Exchange Overview(請求量/延遲/錯誤、命令佇列、outbox、consumer lag)、Ledger(試算平衡、entries/s、調帳)、Chain(head/lag、充提狀態分布、熱錢包餘額、nonce)、Stream(連線、訊息、慢客戶端)、System(容器 CPU/記憶體、PG 連線)。
- 告警最小集合:任一 role `readyz` 失敗 > 1 min;`ledger_trial_balance_diff != 0`;`chain_scanner_lag_blocks > 20`;`outbox_backlog > 1000` 持續 5 min;`hot_wallet_balance{ETH} < 門檻`;`withdrawals_pending_review > 20`;`reconciliation_diff != 0`。

## 16. 部署演進路徑

| 階段 | 環境 | 形態 | 前提條件 |
|---|---|---|---|
| dev / E2E | 本機、CI | compose `infra + single` 或 `infra + app`;anvil | Phase 0 |
| beta(自營封閉) | 單台 VM(4 vCPU / 8 GB)compose `compose.prod.yaml`;或單節點 k3s 用 Helm | `role` 分容器、Sepolia RPC provider、每日備份、prometheus/grafana、告警接收人、`.env` 由密鑰管理工具產生 | Phase 5 全部 DoD、Phase 6 dashboards、Phase 7 備份演練 |
| Helm(kind 驗證) | CI | chart + subchart 依賴 | Phase 7 |
| managed K8s(beta 後段,可選) | GKE / EKS / AKS 單 region | 同一 chart,外部 managed Postgres / NATS(或 subchart)、K8s Secret / external-secrets、Ingress + TLS、NetworkPolicy | chart 在 kind 通過;engine / chain / signer Deployment `replicas: 1` + `Recreate`;PITR 備份;`terminationGracePeriodSeconds` ≥ engine drain 時間 |

「原封不動搬到 K8s」在 v1.0 改寫為可驗證的工程承諾:**可搬遷的是 image、服務邊界、API/事件契約與 12-factor 行為(env 設定、probes、graceful shutdown、依賴重試);部署描述、密鑰管理、網路隔離、服務身分在 Helm 階段預期重做。** CI 的 `helm` job 持續驗證「搬得過去」。

## 17. 風險清單

| 風險 | 影響 | 緩解 |
|---|---|---|
| 作者無領域知識,帳本或撮合模型做錯 | 全盤重寫 | Phase 0 先文件驗算;第 6 節規格 + 不變量測試;每 Phase DoD 綁測試而非「跑起來」 |
| goroutine / context 不熟,放在正確性關鍵路徑 | 難以除錯的競態 | 撮合核心零 goroutine;runner 先以同步函式測;`-race` 常開;actor 模式單一 goroutine 觸碰 book |
| Phase 3 過長、士氣下滑 | 停擺 | 子里程碑(in-proc 先、NATS 後;API key 先、密碼後);每週有可展示的 `exchangectl` 指令 |
| 鏈上四難題(HD、確認/reorg、nonce、歸集)壓在一起 | Phase 4 失控 | 拆 4a–4d,各自 DoD;anvil cheatcode 自動化;Sepolia 只驗證不進 CI |
| foundry / go-ethereum / JetStream 版本漂移 | 建置或行為變化 | 全部 pin;升級獨立 PR;ADR-0008 記錄版本與理由 |
| anvil 狀態與 Postgres 壽命不一致 | 游標套到新鏈 | `--state` volume + `genesis_hash` 檢查 + `make reset` |
| 範圍蔓延(多市場、進階單型、多租戶、前台功能) | 交付延後 | 3.2 不做清單;新需求先進 ADR 討論,不進本 Phase |
| 單人開發、知識集中 | 中斷後難續 | 本文件、ADR、runbook、CI 為第二大腦;每 Phase 結束修訂文件 |
| 效能目標未達 | beta 體驗 | 目標標為設計目標;Phase 6 壓測記錄差距;正確性優先 |
| 密鑰誤入庫 | 安全事故 | `.gitignore`、`gitleaks`、`.env.example` 佔位、`gen-dev-secrets` |
| 白牌客戶要求多租戶 / OIDC / KMS | 架構壓力 | `tenant_id` 預留、`Signer` 介面、JWT 驗證器可替換、事件契約版本化;真的出現需求再開 ADR |
| Sepolia faucet / RPC 限流 | 4d 延遲 | 多 faucet、付費 RPC 免費層;4d 不阻塞 Phase 5 |

## 18. 誠實面對的限制(修訂版)

- 撮合引擎是單一實例、無熱備;重啟期間該市場不可下單(恢復目標 < 30 s);這是 v1 設計選擇,不是缺陷但也不是 HA。
- 簽名採加密 keystore + passphrase,只適合測試資產與封閉 beta;生產必須換 KMS / HSM / MPC(透過 `Signer` adapter),且熱錢包資金管理、冷錢包流程不在 v1。
- 沒有安全審計、滲透測試、法遵(KYC/AML)整合;`kyc_level` 只是欄位,身分驗證是客戶系統的責任;拿真錢上線前這些都是必要條件。
- 內部服務之間視為信任網路(無 mTLS / 服務身分),只靠 `api/stream/admin` 單一入口與 DB 角色隔離。
- 單租戶;`tenant_id` 存在但沒有隔離邏輯與租戶級權限。
- 資料保存不刪除、無分區;Postgres 單實例、無 PITR(beta 為每日備份 + WAL 歸檔)。
- 效能數字是設計目標,只在單機 compose 上量測;未在 K8s 或雲端網路驗證。
- 合約內部轉帳充值、非 EVM 鏈、代幣黑名單、稅務報表都不支援。
- 生產化時預期重做的清單:密鑰管理、網路隔離與服務身分、多副本撮合與 leader election、資料庫拆分與 PITR、鏈節點(自建或付費 provider)、告警通知管道、事件 schema registry。

## 19. 名詞表

| 中文 | 英文 | 說明 |
|---|---|---|
| 撮合 | matching | 依規則把買賣單配對成交 |
| 訂單簿 | order book | 依價位與時間排列的未成交掛單集合 |
| 掛單 / 吃單 | maker / taker | 掛單方提供流動性(被動);吃單方立即成交(主動);成交價永遠是 maker 價 |
| 價格時間優先 | price-time priority | 先比價格再比到達順序(`seq`) |
| 部分成交 | partial fill | 一張單分多筆成交 |
| 市價單 | market order | 不指定價格,以簿上最佳價依序吃到用完;剩餘以 IOC 取消 |
| IOC | immediate-or-cancel | 能立刻成交的部分成交,其餘取消 |
| 自成交防護 | STP, self-trade prevention | 同帳戶對敲時的處理;v1 取消新單剩餘 |
| 最小跳動 / 數量步進 / 最小名目 | price_tick / qty_step / min_notional | 價格與數量的合法粒度與最小成交金額 |
| 複式記帳 | double-entry bookkeeping | 每筆分錄借貸相等,任何餘額變動都有來源與去向 |
| 分錄 / 過帳明細 | journal entry / posting | 一筆業務事件的一組借貸記錄 / 其中一行 |
| 借 / 貸 | debit / credit | 記帳方向;對負債科目(用戶餘額)貸方增加、借方減少 |
| 科目 | account (ledger) | 記帳單位,例如 `user:{id}:available:ETH` |
| 凍結 / 解凍 / 結算 | hold / release / settle | available → hold / hold → available / hold → 對手方 available(含手續費) |
| 試算平衡 | trial balance | 每資產所有科目借方總和 − 貸方總和,必為 0 |
| 對帳 | reconciliation | 比對帳本、快取、鏈上餘額是否一致 |
| 確認數 | confirmations | 交易所在區塊之後又出了幾個區塊;越多越不可能被 reorg |
| 鏈重組 | reorg | 鏈頭被另一條分支取代,原本區塊裡的交易可能消失 |
| nonce | nonce | 同一地址發出交易的序號,必須連續;卡住會阻塞後續交易 |
| 替換交易 | replacement (speed-up) | 用同 nonce、更高費用重送以取代卡住的交易 |
| HD 錢包 / BIP-44 | hierarchical deterministic wallet | 由一個種子按路徑派生無限地址;`m/44'/60'/0'/0/{i}` |
| 歸集 | sweep | 把散在充值地址的資金移到熱錢包 |
| 熱錢包 | hot wallet | 線上持有私鑰、用於自動出金的錢包 |
| 在途提現 | pending withdrawal | 已廣播未確認的出金 |
| gas | gas | 鏈上交易手續費(ETH 支付) |
| 交易性 outbox | transactional outbox | 事件與業務資料同一交易落庫,再由 relay 發布,避免「落庫成功但發布失敗」 |
| 冪等 | idempotency | 同一操作重複執行結果不變;靠冪等鍵實現 |
| 序號 | seq | 每市場單調遞增的命令/事件序號,用於排序、去重、缺口偵測 |
| 持久化事件流 | persistent event stream | JetStream 中可重放的事件記錄 |
| durable consumer | durable consumer | JetStream 記住消費進度的訂閱者 |
| 唯一真相 | single source of truth | Postgres;其他都是可重建衍生物 |
| 12-factor | twelve-factor app | 設定走環境變數、無本地狀態、可拋棄、日誌為串流等原則 |
| 白牌 | white-label | 授權他人以自有品牌部署的產品 |
| 走路骨架 | walking skeleton | 最薄但貫穿所有層的可執行實作 |
| DoD | definition of done | 可自動驗證的完成條件 |
| ADR | architecture decision record | 記錄背景、選項、決定、後果的短文件 |

## 20. 立即行動(接下來 5 個工作天)

| 天 | 做什麼 | 產出 |
|---|---|---|
| Day 1 | 讀 Fowler Accounting Patterns 與 TigerBeetle Dr/Cr 文件;讀 Harris 訂單簿章節;用紙筆重算第 6.1.4 的 (a)(b)(c)(e),確認每筆借貸相等 | `docs/domain.md` 草稿(6.1–6.3),含一份你自己找到的疑問清單 |
| Day 2 | 寫 ADR:0001 modular monolith 單一 binary 多角色;0002 Postgres 為唯一真相 + outbox + JetStream;0003 單租戶但預留 tenant_id;0004 決策數值(decimal + scale,禁 float);0005 帳本模型(複式、hold 為分錄、house 科目);0006 認證(EdDSA JWT + JWKS、API key HMAC、admin TOTP);0007 簽名隔離(keystore + Signer 介面,KMS 預留);0008 工具鏈版本釘住(Go、foundry tag、go-ethereum、nats、postgres) | `docs/adr/0001–0008` |
| Day 3 | `go mod init`;`cmd/exchange`(cobra);`internal/app`(config、run loop、healthz/readyz/metrics、SIGTERM);`internal/telemetry`;`internal/money` + 測試;`.golangci.yml`;Makefile 骨架 | `make lint test` 綠 |
| Day 4 | `build/Dockerfile`;`deploy/compose/compose.yaml` infra profile(postgres/redis/nats/anvil/contracts-deployer)+ `infra/contracts` + `initdb` + `gen-dev-secrets`;goose `0001`、`0002`;`exchange migrate`、`exchange seed` | `make up` 全 healthy,`addresses.json` 產生,registry 有 ETH-USDC |
| Day 5 | `api/public/v1/openapi.yaml`(markets/assets)→ oapi-codegen → handler → sqlc;testcontainers 整合測試;GitHub Actions(lint/unit/integration/compose-config/image);`exchangectl markets list`;README 產品邊界段 | CI 綠;第一個 PR 合併;把本週學到的 Go 疑問寫進 `docs/learning-log.md` |

第一週不寫任何撮合或帳本程式碼。若 Day 1–2 發現本文件的分錄或狀態機有錯,先改文件、再改 ADR、再開工。

## 21. 附錄:v0.1 → v1.0 主要變更對照表

| # | v0.1 | v1.0 | 對應 finding |
|---|---|---|---|
| 1 | 以學習 Go 為主軸的學習專案 | 白牌交易引擎商業化原型;學習目標另列可檢核主題清單 | F35、P01、L03、C03(已駁回的部分改以 2.4 學習主題清單回應) |
| 2 | 11 個微服務、各自 build | modular monolith:單一 go.mod、單一 binary `--role`、`internal/` package 邊界 + depguard | F07、F16、P07、E06、L05 |
| 3 | account-service 凍結、ledger-service 解凍 | account-service 取消;凍結 = ledger 分錄;Hold/Release/Settle 原子操作;DB 角色強制 | F01、L01、E03 |
| 4 | NATS core 傳資金事件 | 金錢路徑同一 PG 交易 + outbox;JetStream 只做扇出;`internal/eventbus` 介面 | F02、E02、P08 |
| 5 | 訂單無擁有者、gateway 編排 saga | `internal/trading` 擁有訂單狀態機;拒單補償 = 交易回滾;無 saga | F03、E03 |
| 6 | 撮合引擎純記憶體、無 seq、無恢復 | `Apply(cmd) -> []event` 純函式;per-market seq;Postgres SoT;啟動重建;kill -9 測試 | F04、E01、L04 |
| 7 | 提現「簽名 → 廣播 → 風控」 | 狀態機 requested → … → funds_locked → signed → broadcast → confirmed/failed;先鎖後簽;nonce 持久化 | F05、F24、P06 |
| 8 | 單一 init.sql、共用 DB | goose migration、每 schema 獨立 owner、每 process 獨立登入角色、`exchange migrate` | F06、E07 |
| 9 | 「原封不動搬到 K8s」 | 12-factor 可驗證承諾 + Helm chart + kind CI;部署層預期重做 | F08、E09 |
| 10 | Phase 1 = gateway/auth/account | Phase 0 領域建模 + skeleton → Phase 1 撮合 library → Phase 2 帳本 → Phase 3 串接 | F09、E10、L03 |
| 11 | 無 DoD、無時程、無測試餘額來源 | 每 Phase 固定欄位、CI 綁 DoD、Phase 2 admin 調帳為 faucet | F10、L08 |
| 12 | 無測試與 CI | 測試金字塔、不變量清單、GitHub Actions 分層 | F11、C04 |
| 13 | 無科目表與手續費模型 | 6.1 科目表、Dr/Cr、分錄範例、手續費以收到資產計費、捨入對交易所有利 | F12、L01 |
| 14 | 數值規範未定 | decimal + scale、NUMERIC(36,18)、JSON string、禁 float lint、wei 邊界轉換 | F13、L02 |
| 15 | 無功能規格 | 3.1 功能範圍表、3.2 不做清單、6.6 registry | F14、P02 |
| 16 | 市價單 / 取消競態 / STP 未定 | 6.2–6.3 明訂 quote/base 語意、IOC、同序列取消、cancel_newest | F15 |
| 17 | 無對外契約 | OpenAPI ×2 契約先行 + oapi-codegen;事件 catalog;WS / Webhook 規格 | F17、P04、C01 |
| 18 | 無非功能需求 | 3.3 數字化設計目標、保存策略、備份 | F18 |
| 19 | risk-service 定位矛盾 | `policy` 同步規則介面(trading / withdrawal);事後偵測不在 v1;compose 相依修正 | F19、P06 |
| 20 | 充值直接改帳本、無狀態機 | 6.4.1 狀態機、輪詢游標、reorg 回退、冪等鍵、ledger 入帳 | F20、F22 |
| 21 | 簽名散落兩處 | 只在 `chain/signer`(拆分部署為獨立 `signer` role,唯一掛載 keystore);`SignRequest` 三種 kind 的政策檢查、`signing_log` 防重簽、審計;`Signer` 介面預留 KMS | F21、P06 |
| 22 | HD 錢包未定 | BIP-44 路徑、`deposit_addresses`、獨立助記詞、`gen-dev-secrets` | F23 |
| 23 | 無歸集 | 4c sweep、custody 科目、對帳恆等式 | F25 |
| 24 | anvil `latest`、command 錯誤、無狀態 | pin tag、`entrypoint: anvil`、`--state` volume、`genesis_hash` 檢查、`make reset` | F26、L06 |
| 25 | auth 無規格 | EdDSA JWT + JWKS、refresh、argon2id、API key HMAC、role claim、admin TOTP | F27、F30 |
| 26 | 內部信任邊界未定 | 14 節:單一入口、轉發 JWT 再驗、DB 角色、NATS 事件 envelope | F28 |
| 27 | 只有 POSTGRES_PASSWORD | 14 節密鑰清單、`.env` + `*_FILE`、`gen-dev-secrets`、gitleaks | F29 |
| 28 | 無 admin 權限模型、後台選用 | role claim Phase 3;admin API 隨對應 Phase;後台為 Phase 5 必做 | F30、P05 |
| 29 | compose 骨架錯誤 | 第 11 節:刪 version、healthcheck、condition、env anchors、ports、migrate/seed job、profiles、pin | F31 |
| 30 | 限流兩個字、無審計 | Redis token bucket 具體參數;append-only `audit_events` | F32 |
| 31 | KYC 矛盾、2FA 蔓延 | `kyc_level` 欄位由客戶系統寫入;用戶 2FA 不做;admin TOTP 必做 | F33、P01 |
| 32 | Echo/Gin、Ganache、Redis pub/sub 未決 | chi + oapi-codegen;anvil;Redis 白名單兩用途(限流計數、深度快照) | F34、E05、L07 |
| 33 | 缺 out-of-scope、風險、名詞表 | 3.2、17、19 節 | F35 |
| 34 | 無 Makefile / profiles / Dockerfile 策略 | 11 節 Makefile 與 profiles;multi-stage Dockerfile | F36 |
| 35 | 無觀測性 | 15 節:slog、correlation id、指標、dashboards、告警;prometheus/grafana 固定成員 | F37、P09、E08 |
| 36 | 無客戶端冪等鍵 | `client_order_id`、`Idempotency-Key`、取消天然冪等 | C02 |
| 37 | 無 ADR、無版本釘住、無修訂機制 | `docs/adr/`、工具鏈釘住、每 Phase 升版 | C05 |
| 38 | notification-service | 取消;改為 webhook dispatcher + 私有 WS | P08、P01 |
| 39 | WebSocket 歸屬重疊 | `stream` role 終結 WS;api 只做 REST;snapshot + delta + seq | E04、C01 |
| 40 | 多租戶未決 | 單租戶、`tenant_id` 預留、ADR-0003 | P03 |
