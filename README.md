# crypto-exchange

白牌交易引擎(white-label exchange engine)的商業化原型:現貨撮合、複式記帳帳本、EVM 充提與歸集、行情推播、管理後台,以單一 Go binary 多角色的模組化單體交付,客戶透過 REST / WebSocket / Webhook 與事件契約整合。

**目前狀態:Phase 2 帳本 + Postgres。** 已合併:Phase 0 walking skeleton(單一 module、單一 binary 多角色、`GET /v1/markets`、compose、CI)、Phase 1 `internal/matching`(無 I/O、確定性訂單簿,屬性 / 模糊 / golden 測試,`exchangectl replay`)。Phase 2 加入 `internal/ledger`(複式記帳、凍結即分錄、balances 快取、冪等鍵、deferred trigger、GRANT-only 權限)、`internal/audit`、admin API(`/admin/v1`,帳戶 / 餘額 / 分錄 / 試算平衡 / 調帳 / 稽核)與 `exchangectl admin`。trading、auth、鏈上程式碼自 Phase 3 起依 `docs/plan-v1.0.md` 第 12 節逐階段加入。

## 產品邊界

- **A. 引擎交付物**(有相容承諾):撮合、帳本、交易狀態機、行情、EVM 充提與歸集、簽名隔離、registry、管理後台、對帳、webhook;以單一 container image + Helm chart + OpenAPI / 事件契約交付。
- **B. 參考實作**(可整包替換):最小 auth(users + JWT + API key)、React 參考前台、`exchangectl` CLI。
- **C. 明確不做**:KYC 文件蒐集、用戶端 2FA、通知內容、主網與真實資金(只接 anvil 與 Sepolia)。

核心程式碼全部在 `internal/`,客戶只透過 REST / WebSocket / Webhook 與事件契約整合;引擎內部一律以 `account_id` 為鍵,不認識 email 或 KYC。

## 快速開始

需求:Go 1.26(`go.mod` 釘住 toolchain,會自動下載)、Docker Desktop 或 Docker Engine + Compose v2、`make`、`openssl`。

```sh
make gen-dev-secrets    # .env(隨機密鑰)、secrets/jwt/ed25519.pem、新的 BIP-39 助記詞與 HOT_WALLET_ADDRESS
make up-single          # postgres / redis / nats / anvil / MockUSDC 部署 / migrate / seed / exchange-all / prometheus / grafana

curl -s localhost:8080/v1/markets | jq          # seed 進去的 ETH-USDC,金額一律字串("price_tick": "0.01")
go run ./cmd/exchangectl markets list           # 同一件事,走產生的 OpenAPI client
curl -s localhost:9100/readyz                   # {"status":"ok", ...}
curl -s localhost:9100/metrics | grep -E 'exchange_build_info|ledger_trial_balance_diff'

# 帳本(admin API,金鑰在 .env 的 ADMIN_API_KEY)
export EXCHANGE_ADMIN_URL=http://localhost:8082 EXCHANGE_ADMIN_API_KEY=$(sed -n 's/^ADMIN_API_KEY=//p' .env)
ACC=$(go run ./cmd/exchangectl admin accounts create)                                   # 開一個現貨帳戶
go run ./cmd/exchangectl admin fund --account $ACC --asset USDC --amount 10000            # dev faucet(external → available,寫審計)
go run ./cmd/exchangectl admin balances $ACC && go run ./cmd/exchangectl admin trial-balance   # 每資產 diff = 0
make artifacts                                  # 把 addresses.json 從 volume 複製到 deploy/compose/artifacts/
cast call $(jq -r .usdc deploy/compose/artifacts/addresses.json) "decimals()(uint8)" --rpc-url localhost:8545   # 6

make down               # 停止(保留資料)
make reset              # 停止並清空 postgres / nats / anvil 狀態與合約產物
```

`make up` 改為每個 role 一個容器(api / engine / chain / signer / stream / admin / worker);`OBS=0` 可略過 prometheus / grafana。

## 開發循環

```sh
make lint               # go vet + golangci-lint(depguard 模組邊界、forbidigo 禁 float;版本釘在 tools/go.mod)
make test               # 單元 + 屬性測試(-race;rapid 每個性質 1,000 個序列)
make test-fuzz          # 每個 Fuzz* 目標跑 FUZZ_TIME(預設 30s)
go run ./cmd/exchangectl replay --file test/fixtures/matching/market_buy_two_levels.jsonl   # 重播撮合腳本、印事件與深度
go test ./internal/matching -run TestGolden -update   # 重新產生 golden(改語意時,diff 要 review)
make gen && make gen-check   # 重新產生 OpenAPI server/client 與 sqlc 程式碼;產物進 repo,CI 比對
make test-integration   # testcontainers(需要 Docker):migration、seed、GET /v1/markets、帳本(算例、500 個隨機序列、100 goroutine 併發)、admin API
TEST_PG_ADMIN_URL=postgres://exchange:test@127.0.0.1:5433/postgres make test-integration   # 改用現成的本機 Postgres(先跑 infra/postgres/initdb/01-roles.sh),每個測試一個新 database
make contracts-test     # 在釘住的 foundry 映像內跑 forge test
make infra-up && make migrate && make seed && make run ROLE=api   # 只起基礎設施,role 在主機上 go run
```

契約先行:改 `api/public/v1/openapi.yaml` → `make gen` → 實作 `internal/api` 的 strict server 介面。資料庫改動一律新增 `migrations/NNNN_<module>_<desc>.sql`(goose、只 forward),查詢寫在 `internal/<module>/queries/*.sql` 交給 sqlc。

## Repo 結構(Phase 0)

```
cmd/exchange          單一 binary:serve --role=api|engine|chain|signer|stream|admin|worker|all、migrate、seed、healthcheck、keys
cmd/exchangectl       開發/營運 CLI(產生的 OpenAPI client)
internal/app          設定、run loop、/healthz /readyz /metrics、SIGTERM drain、依賴退避
internal/api          public REST(oapi-codegen strict server)+ RFC 7807
internal/matching     純函式訂單簿(Apply / Restore / Snapshot;無 I/O、無時鐘)
internal/ledger       複式帳本:Post / Hold / Release / Settle / Credit / Adjust、balances 快取、試算平衡(sqlc)
internal/audit        append-only 稽核紀錄
internal/admin        admin REST(oapi-codegen strict server)+ X-Admin-Api-Key
internal/registry     assets / markets / fee schedules(sqlc)+ seed
internal/money        Decimal 金額型別(禁 float;JSON 字串)
internal/telemetry    slog、correlation id、Prometheus
internal/platform     pgx / NATS / Redis 連線與健康檢查
api/public/v1         公開 OpenAPI 契約
api/admin/v1          admin OpenAPI 契約
migrations            goose SQL(embed)
deploy/compose        compose.yaml(profiles:infra / app / single / observability)
infra/contracts       MockUSDC + 冪等部署腳本(Foundry)
infra/postgres        ex_* 登入角色 initdb 腳本
infra/observability   prometheus / grafana 設定
test/integration      testcontainers 整合測試(build tag integration)
test/fixtures/matching 撮合命令腳本與 golden 事件 / 快照
docs                  計畫、審查、ADR、領域文件
```

## 文件索引

| 文件 | 內容 |
|---|---|
| [`docs/plan-v1.0.md`](docs/plan-v1.0.md) | **分階段可執行計畫 v1.0**(定位、範圍、領域模型、契約、模組、選型、compose、Phase 0~7、測試/CI、安全、觀測、部署、風險) |
| [`docs/review/plan-review-2026-09.md`](docs/review/plan-review-2026-09.md) | v0.1 規劃書審查報告(28 條合併後發現、不採納意見、對 v1.0 的結構性要求) |
| [`docs/domain.md`](docs/domain.md) | 領域文件:科目表、分錄、狀態機、撮合語意的逐項驗算與疑問清單 |
| [`docs/adr/`](docs/adr/) | ADR-0000 需求訪談決策(8 輪 32 題);ADR-0001~0008 架構決策(單體、真相來源、租戶、數值、帳本、認證、簽名、工具鏈) |
| [`docs/archive/plan-v0.1.md`](docs/archive/plan-v0.1.md) | 原始 v0.1 規劃書(已取代,僅供對照) |

## 下一步

Phase 3(`docs/plan-v1.0.md` §12):`internal/trading` 狀態機 + per-market runner、outbox / JetStream、public OpenAPI 的交易端點、最小 auth。DoD 不過不進下一階段。
