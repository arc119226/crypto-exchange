# crypto-exchange(工程師版 README)

> 這是給工程師的索引。第一次接觸、想照著做一遍的人請看 [`../README.md`](../README.md)(繁體中文)或 [`../README.en.md`](../README.en.md)(English)。
>
> 想從業務角度理解這套系統在做什麼——不看程式碼的那種——請看 [`system-overview.md`](system-overview.md)。

## 這是什麼

一套**白牌交易所引擎**(white-label exchange engine)。「白牌」的意思是:引擎是我們寫的,品牌是客戶的。客戶拿去掛上自己的名字開一家交易所,不必自己重寫撮合、帳本、充提與後台。

它包含一家交易所螢幕後面的那一整套:現貨撮合、複式記帳帳本、EVM 鏈上的充值與提現與歸集、行情推播、管理後台。

交付形態是**單一 Go binary,啟動時用參數決定它今天扮演哪個角色**。同一個執行檔可以只當 API、只當撮合引擎、或一次全兼。這叫模組化單體(modular monolith):程式碼像單一個應用程式一樣好改,部署起來卻可以像微服務一樣拆開。決定的理由在 ADR-0001。

客戶不碰我們的資料庫,也看不到我們內部的訊息佇列。他們只透過四個東西整合:REST API、WebSocket、出站 Webhook,以及事件契約。這四個都有版本化的規格文件,在下面的文件索引裡。

> **一條沒有例外的界線**:v1 **只連得上 anvil(本機假鏈)與 Sepolia(以太坊測試網),永遠不接主網、不碰真錢**。要跨過這條線需要通過 [`plan-v1.0.md`](plan-v1.0.md) §22 的閘門,那是 v2 的事。

## 五分鐘看懂它怎麼組起來

### 七個角色

一個執行檔,`serve --role=<名字>` 決定它啟動哪些部分。逗號可以串接,`all` 是全部。

| 角色 | 它負責什麼 | 它**不能**做什麼 |
|---|---|---|
| `api` | 對外的 REST(:8080)。註冊登入、查餘額、領充值地址、送出提現**請求**、下單 | 不能改訂單簿、不能鎖錢、不能簽名。連提現那張單它都沒有 UPDATE 權限 |
| `engine` | 撮合引擎。每個市場一條專屬的處理線,靠一把 Postgres advisory lock 保證整個資料庫只有一個引擎。順便跑 outbox relay 與命令匯流排的伺服端 | — |
| `chain` | 鏈上的事,除了金鑰:掃描充值、執行提現、歸集、對帳。五條各自獨立的 tick 迴圈 | 不持有任何私鑰 |
| `signer` | **唯一會打開 HD 種子的處理程序**。維持充值地址池,並簽署具名的意圖 | 沒有對外的埠。只從內部的請求-回應通道接受工作 |
| `stream` | WebSocket(:8081)。公開頻道靠一份影子訂單簿,私有頻道可以從 outbox 補資料重連 | — |
| `admin` | 後台(:8082)。機器用 API key,人用密碼加 TOTP。另外每 30 秒跑一次帳本自檢 | **沒有節點連線,也沒有金鑰** |
| `worker` | 沒有人在等的工作:寄 webhook、寫 K 線、清舊資料 | — |

每個角色另外都開一個維運埠(:9100),提供 `/healthz`、`/readyz`、`/metrics`。

### 它們怎麼互相講話

三條路,用途完全不同,不要混用:

**一、請求-回應(NATS request-reply)。** 需要等答案的時候用。`api` 問 `engine`「這張單收不收」走 `internal/cmdbus`;`chain` 請 `signer`「幫我簽這個」走 `internal/signerbus`。跨容器之後仍然保持 404 / 422 / 503 的錯誤語意,命令帶一個 `aud=internal` 的內部 JWT——**引擎信任那個 token,絕不信任請求內容裡寫的帳號**。當多個角色住在同一個處理程序時,這兩條路自動退化成直接函式呼叫。

**二、事件廣播(outbox → JetStream)。** 不需要等答案的時候用,而且這是整套系統最重要的一個設計:

任何角色要發事件,**是把它寫進自己那筆業務交易裡的 `eventbus.outbox` 資料表**,不是直接丟給訊息佇列。交易提交,事件才存在;交易回滾,事件跟著消失。然後一個常駐的 relay 監聽資料庫通知,把還沒送出的列搬到 JetStream,並用 `event_id` 當去重鍵。

結果是兩個方向都關死了:「事情發生了但沒人被通知」不可能,「通知發出去了但事情沒發生」也不可能。這是 ADR-0002。

> 一個要記得的後果:**relay 只跑在 `engine` 角色裡**。一個沒有 engine 的部署,chain 與 admin 的事件會一直躺在 outbox 裡不動。

**三、快取(Redis)。** 限流計數與深度快照。**永遠不是真相來源**,掉了就重算,健康檢查把它列為非關鍵。

### 資料放在哪

Postgres 是唯一的真相來源。JetStream 只做扇出,Redis 只做快取,兩者都可以清空重建。

## 跑起來

需求:Go 1.26(`go.mod` 釘住 toolchain,會自動下載)、Docker Desktop 或 Docker Engine + Compose v2、`make`、`openssl`。

選用:Node ≥ 22(只有 `web/trade` 的開發模式與 Playwright 需要;compose 的 `web` 服務會在容器裡 build 前台)、`jq` / `curl` / `websocat`(下面的範例用到)。Foundry 不必裝,合約走 docker。

### 第 1 步:產生只屬於你的密鑰

```sh
make gen-dev-secrets
```

它會寫出 `.env`(隨機密鑰)、`secrets/jwt/ed25519.pem`、一組新的 BIP-39 助記詞與對應的 `HOT_WALLET_ADDRESS`,以及 `secrets/keystore/hd-seed.json`(signer 用的加密種子)。

這些檔案**全部 gitignored**,而且每個人產生的都不一樣。

> 4a-1 之前跑過這個專案的人要再跑一次,否則 signer 沒有種子,起不來。

### 第 2 步:把整座交易所開起來

```sh
make up-single
make ps
```

`up-single` 起 postgres / redis / nats / anvil、部署 MockUSDC、跑 migration 與 seed、起一個包含全部角色的 `exchange-all`、prometheus / grafana,以及前台(:8088)。

看某個服務的 log:`make logs SERVICE=exchange-all TAIL=200 FOLLOW=1`。

### 第 3 步:確認它活著

```sh
open http://localhost:8088
curl -s localhost:8080/v1/markets | jq
curl -s localhost:9100/readyz
```

前台右上角可以切中文 / EN。`/v1/markets` 會回 seed 進去的 ETH-USDC,**金額一律是字串**(`"price_tick": "0.01"`)——理由在 ADR-0004。

`WEB=0` 可以不起前台。維運埠 9100 只有 `up-single` 發布到 host,拆分部署要 `docker compose exec` 進容器。

同一件事走產生的 OpenAPI client:

```sh
go run ./cmd/exchangectl markets list
```

### 第 4 步:開一個帳戶,給它一些錢

admin API 的金鑰在 `.env` 的 `ADMIN_API_KEY`。

```sh
export EXCHANGE_ADMIN_URL=http://localhost:8082
export EXCHANGE_ADMIN_API_KEY=$(sed -n 's/^ADMIN_API_KEY=//p' .env)

ACC=$(go run ./cmd/exchangectl admin accounts create)
go run ./cmd/exchangectl admin fund --account $ACC --asset USDC --amount 10000
go run ./cmd/exchangectl admin balances $ACC
go run ./cmd/exchangectl admin trial-balance
```

`admin fund` 是 dev 用的水龍頭:它從 `external` 這個科目撥錢到你的可用餘額,並寫一筆稽核紀錄。`trial-balance` 每個資產的 diff 都要是 0——那是複式記帳的自檢。

不想裝 Go、也不想把金鑰匯出到 shell 的話,同一件事可以在容器裡做:

```sh
make faucet ACCOUNT=$ACC ASSET=USDC AMOUNT=10000
```

### 第 5 步:用人的身分登入後台

```sh
make totp-enroll EMAIL=$(sed -n 's/^ADMIN_BOOTSTRAP_EMAIL=//p' .env)
open http://localhost:8082/admin/login
```

`totp-enroll` 在容器裡跑 `exchange admin totp enroll`,印出 otpauth URL 與 secret——**只印這一次**,把它加進你的 authenticator。

密碼是 `.env` 的 `ADMIN_BOOTSTRAP_PASSWORD`。第一個驗證碼完成啟用,之後每次登入都要碼。

### 第 6 步:當一次使用者

```sh
go run ./cmd/exchangectl user register --email alice@example.com --password 'correct horse battery'
export EXCHANGE_TOKEN=...
```

`register` 會印出一行 `export EXCHANGE_TOKEN=...`,把它貼回 shell。

```sh
go run ./cmd/exchangectl admin fund \
  --account $(go run ./cmd/exchangectl user me --output json | jq -r .account_id) \
  --asset USDC --amount 10000

go run ./cmd/exchangectl orders place --side buy --price 2000 --qty 0.5
go run ./cmd/exchangectl book ETH-USDC
go run ./cmd/exchangectl balances
go run ./cmd/exchangectl orders list --open
```

下單的三種結果:成功是 201 open;帶同一個 `--client-order-id` 重送會拿到 200 與**原本那張單**(冪等);餘額不足是 201 但狀態 rejected——不是 4xx,因為請求本身沒有錯。狀態碼的完整規則在 [`api-conventions.md`](api-conventions.md)。

機器整合用 API key,不用 JWT:

```sh
go run ./cmd/exchangectl api-keys create --scopes read,trade
```

secret **只顯示一次**。之後每個請求用 `--api-key` / `--api-secret` 做 HMAC 簽章。

跑完整的一輪:

```sh
go run ./cmd/exchangectl e2e --verbose
```

兩個使用者走完 [`plan-v1.0.md`](plan-v1.0.md) §6.1.4 的數字,並驗證試算平衡。

### 第 7 步:拆分部署

上面是全部角色擠在一個容器裡。真正的形態是每個角色一個容器,下單要走 NATS:

```sh
make up
make e2e
```

`make e2e` 是那條路徑的完整驗證:`exchangectl e2e`、`kill -9` 引擎之後訂單簿一致、停牌之後下一張單被拒。

熱改市場狀態,引擎會自己重載,不用重啟:

```sh
go run ./cmd/exchangectl admin markets set-status ETH-USDC halted --reason "maintenance"
```

拿合約位址:

```sh
make artifacts
cast call $(jq -r .usdc deploy/compose/artifacts/addresses.json) "decimals()(uint8)" --rpc-url localhost:8545
```

`make up` 每個 role 一個容器(api / engine / chain / signer / stream / admin / worker)。`OBS=0` 略過 prometheus / grafana / jaeger,`WEB=0` 略過前台。

### 第 8 步:Webhook

客戶那一面的完整說明在 [`webhooks.md`](webhooks.md)。

```sh
WH=$(go run ./cmd/exchangectl admin webhooks create \
    --url http://host.docker.internal:9999 \
    --events 'trade.executed,withdrawal.state_changed' --output json)

go run ./cmd/exchangectl webhook-sink --port 9999 --secret $(echo $WH | jq -r .secret) &
go run ./cmd/exchangectl e2e --verbose
```

`webhook-sink` 是一個本機接收器,它**用伺服器簽章時的同一份程式碼驗簽**,所以 sink 印得出來就代表簽章真的對得上。

```sh
EP=$(echo $WH | jq -r .id)
go run ./cmd/exchangectl admin webhooks deliveries $EP
go run ./cmd/exchangectl admin webhooks replay $EP $DELIVERY_ID
```

replay 會讓客戶再收到一次,`event_id` 相同——**至少一次投遞是契約,不是免責聲明**。

### 第 9 步:行情、推播與觀測

wire format 在 [`ws-api.md`](ws-api.md)。

```sh
go run ./cmd/exchangectl ticker ETH-USDC
go run ./cmd/exchangectl klines ETH-USDC --interval 1m --limit 5

websocat ws://localhost:8081/ws/v1/public <<< '{"op":"subscribe","channel":"depth","market":"ETH-USDC"}'
websocat ws://localhost:8081/ws/v1/private <<< "{\"op\":\"auth\",\"token\":\"$EXCHANGE_TOKEN\"}"
```

公開頻道先給一份 snapshot,之後每個 seq 一則 delta。私有頻道通過認證之後,下單會推 orders / fills / balances 三個頻道。

前台的開發模式(Vite 會把 `/v1` 與 `/ws` 代理到 8080 / 8081,改程式立即重載):

```sh
(cd web/trade && npm ci && npm run dev)
make web-e2e
make loadgen
```

```sh
open http://localhost:3000     # Grafana:五個 dashboard
open http://localhost:9090/alerts
open http://localhost:16686    # Jaeger
```

Grafana 帳號是 admin,密碼在 `.env` 的 `GRAFANA_ADMIN_PASSWORD`。Jaeger 裡找 `exchange-api` 的 `POST /v1/orders`,可以看它一路走到 engine、Postgres 與每個 consumer。壓測數字與瓶頸分析在 [`loadtest.md`](loadtest.md)。

### 第 10 步:營運相關

每一步的操作手冊在 [`runbooks/`](runbooks/),上線清單在 [`beta-checklist.md`](beta-checklist.md)。

```sh
make up BACKUP=1
make backup-drill
```

`BACKUP=1` 多起 minio 與 backup sidecar:每日 `pg_dump`、每 30 秒出貨 WAL,結果寫進 `admin.backups`。`backup-drill` 現在備份一次、還原到一個拋棄式資料庫、驗試算平衡與序號與成交、印出 RTO——**CI 每個 PR 都跑一次**。

密鑰相關:

```sh
go run ./cmd/exchange keys jwt-public --in secrets/jwt/ed25519.pem

DATABASE_URL=postgres://ex_migrate:...@localhost:5432/exchange \
  API_KEY_MASTER_KEY=新 API_KEY_MASTER_KEY_PREVIOUS=舊 \
  go run ./cmd/exchange keys rewrap --domain api-keys
```

`rewrap` 在一筆交易裡把每一列改成用新金鑰封的,冪等。輪替流程在 [`runbooks/key-rotation.md`](runbooks/key-rotation.md)。

Kubernetes 與發布:

```sh
make helm-lint
make kind-up && make helm-e2e && make kind-down
make release-check TAG=v0.1.0
```

`helm-lint` 不需要叢集。`kind-*` 那組是 CI 的 helm job 在本機的樣子,需要 kind + kubectl + docker。`release-check` 檢查 chart 的 appVersion 與 binary 都報同一個版本([`release.md`](release.md))。

單台 VM 的正式形態(secrets 全是檔案、只開 80/443,步驟在 [`runbooks/beta-deploy.md`](runbooks/beta-deploy.md)):

```sh
sudo scripts/gen-prod-secrets.sh && make up-prod
```

收工:

```sh
make down     # 停止,保留資料
make reset    # 停止並清空 postgres / nats / anvil 狀態與合約產物
```

### 接真的鏈(Sepolia,手動,不進 CI)

逐步操作在 [`guides/sepolia.md`](guides/sepolia.md),它的 Part A 不需要上面任何東西就能開始。

```sh
make up-sepolia
make down-sepolia
```

用獨立的 project name 與 volume,不會和本機的 anvil 打架。

## 開發循環

### 每天會打的

```sh
make lint
make test
make gen && make gen-check
make test-integration
```

`lint` 是 `go vet` + golangci-lint + gitleaks。golangci-lint 的設定裡有兩條**架構規則**,不是風格規則:depguard 擋跨層 import(領域套件不准 import NATS、管地址的套件不准 import 管金鑰的套件),forbidigo 擋金額路徑上的 float。**這些不靠人記得,靠工具擋。**

`secrets-scan` 只跑 gitleaks,而且掃的是 **git 歷史不是工作目錄**——`.env` 與 `secrets/` 是本機真金鑰,本來就 gitignored,每次都報一遍只會教人關掉它。

`test` 是單元加屬性測試,帶 `-race`。屬性測試每個性質跑 1,000 個序列。

`gen-check` 重新產生 OpenAPI server / client 與 sqlc 程式碼,然後跟 repo 裡的比對——產物是進版控的。

`test-integration` 需要 Docker,用 testcontainers 起 Postgres 與 NATS。

### 偶爾會用到的

```sh
make test-fuzz
go run ./cmd/exchangectl replay --file test/fixtures/matching/market_buy_two_levels.jsonl
go test ./internal/matching -run TestGolden -update
make contracts-test
```

`replay` 重播一段撮合腳本並印出事件與深度。`-update` 重新產生 golden 檔——**改語意的時候才用,而且 diff 一定要 review**。

不想用 testcontainers、想接現成的服務:

```sh
TEST_PG_ADMIN_URL=postgres://exchange:test@127.0.0.1:5433/postgres make test-integration
TEST_NATS_URL=nats://127.0.0.1:4222 make test-integration
```

Postgres 那條要先跑過 `infra/postgres/initdb/01-roles.sh`,每個測試會開一個新 database。NATS 那條要 `nats-server -js`(outbox relay 的測試會 purge 它用到的 stream)。

只起基礎設施、role 在主機上跑:

```sh
make infra-up && make migrate && make seed && make run ROLE=api
```

### 兩條不成文但必須遵守的規矩

**契約先行。** 改 `api/public/v1/openapi.yaml` → `make gen` → 實作 `internal/api` 的 strict server 介面。反過來先寫 handler 再補規格,產生的 client 就會跟伺服器對不上。

**資料庫只往前。** 一律新增 `migrations/NNNN_<module>_<desc>.sql`(goose,只 forward),查詢寫在 `internal/<module>/queries/*.sql` 交給 sqlc。

### 介面語言(ADR-0010)

前台與後台都是繁體中文 / 英文,**英文是來源,中文照它翻**。

- 前台字串在 `web/trade/src/i18n/messages.ts`(`tsc` 用 `Record<Key, string>` 逼中文補齊),狀態碼在 `enums.ts`,`Problem.detail` 的對照在 `problems.ts`。
- 後台在 `internal/admin/i18n_en.go` 與 `internal/admin/i18n_zh_tw.go`,template 用 `{{T "key"}}`,`TestMessagesComplete` 檢查兩份 key 相同、`fmt` 動詞相同、沒有死 key。

**不翻的東西**:CLI、API、事件、log、工程文件維持英文;路由與狀態碼不翻;金額不走 `Intl.NumberFormat`(它會塞千分位並依語系改小數點,金額是精確字串,不是給人讀的數字)。

## Repo 結構

| 路徑 | 這裡放什麼 |
|---|---|
| `cmd/exchange` | 單一 binary:`serve --role=...`、migrate、seed、healthcheck、keys(gen-jwt / jwt-public / import-mnemonic / rekey / rewrap) |
| `cmd/exchangectl` | 開發與營運 CLI,走產生的 OpenAPI client |
| `internal/app` | 組裝的地方:設定、run loop、健康檢查、SIGTERM drain、依賴退避 |
| `internal/api` | public REST(oapi-codegen strict server)+ RFC 7807 + 限流 |
| `internal/auth` | 最小 auth 參考實作:users、argon2id、Ed25519 JWT / JWKS、refresh 輪替、API key HMAC |
| `internal/ratelimit` | token bucket(Redis Lua / 記憶體 / Fallback) |
| `internal/matching` | **純函式訂單簿**——無 I/O、無時鐘、無亂數、無 goroutine。同樣的命令重放出同樣的簿 |
| `internal/trading` | 訂單狀態機、每市場一條 runner(一組 ≤ 50 個命令一筆交易)、`client_order_id` 冪等、重建 |
| `internal/eventbus` | 事件 envelope、outbox、JetStream relay 與 stream 宣告、durable / ordered consumer |
| `internal/cmdbus` | api → engine 的請求-回應命令匯流排(含 `aud=internal` JWT) |
| `internal/signerbus` | chain → signer 的請求-回應簽名通道 |
| `internal/policy` | 純決策函式:下單規則與提現規則 |
| `internal/ledger` | 複式帳本。**balances 只有它能寫** |
| `internal/audit` | append-only 稽核紀錄,跟被稽核的變更同一筆交易 |
| `internal/chain` | 充值地址指派。**刻意不能派生金鑰**——lint 擋它 import `hdwallet` |
| `internal/chain/deposit` | 區塊掃描器:reorg 安全的游標、原生幣與 ERC-20 兩條路徑、確認數、入帳 |
| `internal/chain/withdrawal` | 提現狀態機:請求、政策與鎖錢、簽名與廣播與追蹤、人工處置 |
| `internal/chain/sweep` | 把散在各充值地址的錢收進熱錢包。**只動兩個 house 科目,不動使用者餘額** |
| `internal/chain/reconcile` | 帳本 custody 對鏈上餘額,容差為零 |
| `internal/chain/signer` | `Signer` 介面與 keystore 實作。`kms.go` 是編譯得過但會回「還沒接」的 KMS 接頭 |
| `internal/chain/hdwallet` | BIP-32/39 HD 錢包、派生路徑、地址池 |
| `internal/chain/hotwallet` | 熱錢包的 nonce 配置與回收,啟動時對鏈校正 |
| `internal/chain/evm` | 以太坊 JSON-RPC client、手續費建議與上限、wei 換算 |
| `internal/admin` | admin REST + 伺服器渲染的後台(html/template + htmx,zh-TW / en) |
| `internal/webhook` | 出站投遞:HMAC 簽章、退避排程、deliveries、endpoint 管理與 replay |
| `internal/registry` | assets / markets / fee schedules / withdrawal limits + seed |
| `internal/money` | Decimal 金額型別(禁 float;JSON 一律字串) |
| `internal/marketdata` | 影子訂單簿投影、K 線聚合與落地、ticker、Redis 深度快照 |
| `internal/stream` | WebSocket server:公開頻道、私有頻道(auth、`account_seq`、resume)、慢客戶端斷線 |
| `internal/telemetry` | slog、correlation id、Prometheus、OpenTelemetry、密鑰遮蔽 |
| `internal/platform` | pgx / NATS / Redis 連線與健康檢查、pgx tracer、`secretbox`(AES-256-GCM 信封 + 輪替 Keyring) |
| `api/public/v1` | 公開 OpenAPI 契約 |
| `api/admin/v1` | admin OpenAPI 契約 |
| `api/events/v1` | 事件的 JSON Schema |
| `migrations` | goose SQL,embed 進 binary |
| `deploy/compose` | `compose.yaml`(六個 profile)、`compose.sepolia.yaml`、`compose.prod.yaml` |
| `deploy/helm/exchange` | Helm chart(每 role 一個 Deployment、migrate hook);`helm_test.go` 在 `make test` 裡 lint + kubeconform |
| `deploy/vm` | Ubuntu 24.04 的 `bootstrap.sh`(Docker CE、ufw、systemd unit) |
| `build` | app 的 Dockerfile、`edge/`(Caddy + 前台)、`backup/`(備份 sidecar) |
| `scripts` | gen-dev-secrets、gen-prod-secrets、e2e、helm-e2e、kind-secrets、backup、restore-drill |
| `infra/contracts` | MockUSDC 與冪等部署腳本(Foundry) |
| `infra/postgres` | `ex_*` 登入角色的 initdb 腳本、WAL 歸檔設定 |
| `infra/observability` | prometheus(含十一條告警)/ grafana 設定與五個 dashboard |
| `web/trade` | 參考前台(React + Vite + TS;OpenAPI 產 TS client;zh-TW / en) |
| `test/integration` | testcontainers 整合測試(build tag `integration`) |
| `test/docs` | 文件本身的測試:runbook 四段結構、三份 README 的連結與中英文一致 |
| `test/fixtures/matching` | 撮合命令腳本與 golden 事件 / 快照 |
| `docs` | 你正在看的這裡 |

## 文件索引

### 先讀這些

| 文件 | 內容 |
|---|---|
| [`../README.md`](../README.md) | 新手指南(繁體中文):十步照著打,把交易所在自己電腦裡跑一遍。不需要會寫程式 |
| [`docs/system-overview.md`](system-overview.md) | **不看程式碼的系統總覽**:七個角色像哪些部門、錢與資料怎麼跑、CI 全綠代表什麼、以後怎麼擴充 |
| [`docs/plan-v1.0.md`](plan-v1.0.md) | **分階段可執行計畫 v1.1**(定位、範圍、領域模型、契約、模組、選型、Phase 0~9、測試/CI、安全、觀測、部署、風險;v1.1 加 §22 v2 主網閘門、§23 營收模型。檔名維持 v1.0) |

### 設計與決策

| 文件 | 內容 |
|---|---|
| [`docs/adr/`](adr/) | ADR-0000 需求訪談決策(8 輪 32 題);ADR-0001~0013 架構決策(單體、真相來源、租戶、數值、帳本、認證、簽名、工具鏈、beta 形態與備份政策、介面語言與新手 README、營收模型與主網路線、CI 跑在自建 runner、三種讀者的文件) |
| [`docs/domain.md`](domain.md) | 領域文件:科目表、分錄、狀態機、撮合語意的逐項驗算;各 Phase 程式碼與計畫的對應表與事後檢討 |
| [`docs/changelog-by-phase.md`](changelog-by-phase.md) | 逐階段的變更記錄:每個 Phase 合併了什麼、**在那個 Phase 抓到什麼真缺陷、怎麼修的** |
| [`docs/review/plan-review-2026-09.md`](review/plan-review-2026-09.md) | v0.1 規劃書審查報告(28 條合併後發現、不採納的意見、對 v1.0 的結構性要求) |

### 對外契約(客戶會讀的,英文)

| 文件 | 內容 |
|---|---|
| [`docs/api-conventions.md`](api-conventions.md) | Public API 慣例:金額字串、problem+json、JWT / API key HMAC 簽章、限流、`client_order_id` 狀態碼(English) |
| [`docs/events.md`](events.md) | 事件契約:envelope、subject 與 stream、排序與去重、consumer 型別、catalog、相容規則(English) |
| [`docs/webhooks.md`](webhooks.md) | 出站 Webhook:簽章與驗證、重試排程、**至少一次投遞的實際後果**、endpoint 管理與 replay(English) |
| [`docs/ws-api.md`](ws-api.md) | WebSocket:公開 / 私有頻道的訊息、depth 的客戶端規則、`account_seq` 與 resume、錯誤碼(English) |

### 營運

| 文件 | 內容 |
|---|---|
| [`docs/runbooks/`](runbooks/) | 九本四段式(症狀 / 檢查指令 / 處置 / 驗證)手冊:engine 重啟、卡住的提現、reorg 告警、熱錢包低水位、對帳差異、備份還原、密鑰輪替、beta 部署、admin TOTP |
| [`docs/beta-checklist.md`](beta-checklist.md) | Beta 上線檢查表:這個 beta 的限制(單機、RPO 24h、無 PITR)、上線前的勾選項、運維節奏、刻意沒做的 |
| [`docs/release.md`](release.md) | 發布:`vX.Y.Z` tag 做什麼、版本斷言、`make release-check`、失敗時怎麼辦 |

### 教學與報告

| 文件 | 內容 |
|---|---|
| [`docs/guides/sepolia.md`](guides/sepolia.md) | 從零到 Sepolia 實跑(4d 的逐筆交易、gas 與區塊) |
| [`docs/guides/self-hosted-runner.md`](guides/self-hosted-runner.md) | **把 CI 搬到自己的機器**(工程師版):私有倉庫的帳單為什麼是額度的 7.8 倍、Windows 11 + WSL2 的完整步驟、七個 CI job 各在證明什麼、`vars.CI_RUNNER` 一鍵切換與回退、共用 daemon 的安全清理 |
| [`docs/guides/self-hosted-runner-newcomer.md`](guides/self-hosted-runner-newcomer.md) | 同一件事的**新手版**:每一步都寫明「你會看到什麼」 |
| [`docs/loadtest.md`](loadtest.md) | 本機壓測:Phase 6 的四組 run、瓶頸(每命令 17 次往返);§8 group commit 之後再量一次(3 次往返、266 orders/s) |
| [`docs/screenshots/`](screenshots/) | 後台每一頁的截圖(`make screenshots` 產生)與參考前台的交易頁 / 錢包頁 |
| [`docs/archive/plan-v0.1.md`](archive/plan-v0.1.md) | 原始 v0.1 規劃書(已取代,僅供對照) |

## 目前進度

Phase 0–7 全數完成,[`plan-v1.0.md`](plan-v1.0.md) §12 的每個 DoD 都有對應的測試或 CI job。4d 在 Sepolia 上實跑過。`v0.1.0` 之前補了介面雙語與新手 README(ADR-0010),之後把 CI 搬到自建 runner(ADR-0012)。

每個 Phase 合併了什麼、抓到什麼缺陷,在 [`changelog-by-phase.md`](changelog-by-phase.md)。Phase 7 的對應與偏離在 [`domain.md`](domain.md) §26,四個沒有唯一答案的決定在 ADR-0009。

## 產品邊界

這一節說的是「什麼有相容承諾、什麼可以整包換掉、什麼刻意不做」。

**A. 引擎交付物,有相容承諾。** 撮合、帳本、交易狀態機、行情、EVM 充提與歸集、簽名隔離、registry、管理後台、對帳、webhook。交付形態是單一 container image + Helm chart + OpenAPI 與事件契約。

**B. 參考實作,可以整包替換。** 最小的 auth(users + JWT + API key)、React 參考前台、`exchangectl` CLI。客戶已經有自己的會員系統時,換掉 auth 不影響引擎——因為引擎內部一律以 `account_id` 為鍵,**它不認識 email,也不認識 KYC**。

**C. 明確不做。** KYC 文件蒐集、使用者端 2FA、通知內容、主網與真實資金。

## 下一步

接下來是 beta 本身:照 [`runbooks/beta-deploy.md`](runbooks/beta-deploy.md) 起一台 VM、打勾 [`beta-checklist.md`](beta-checklist.md)、每週演練一次還原、每 90 天輪一次密鑰。

已知的缺口寫在 checklist 的第一段:可還原的 RPO 是 24 小時(WAL 有歸檔但沒有 base backup)、還原後熱錢包 nonce 要人工對帳、沒有 Alertmanager、單市場 266 orders/s 離 §3.3 的 1,000 還有距離——下一個瓶頸是每句 SQL 的 Postgres 成本,不再是往返數。
