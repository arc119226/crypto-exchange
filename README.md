# crypto-exchange

白牌交易引擎(white-label exchange engine)的商業化原型:現貨撮合、複式記帳帳本、EVM 充提與歸集、行情推播、管理後台,以單一 Go binary 多角色的模組化單體交付,客戶透過 REST / WebSocket / Webhook 與事件契約整合。

**目前狀態:Phase 1 撮合純 library。** Phase 0 的 walking skeleton(單一 module、單一 binary 多角色、`GET /v1/markets` 從 OpenAPI 到 Postgres、compose、CI)已合併;`internal/matching` 是無 I/O、確定性的訂單簿(限價 GTC / IOC、市價 quote/base 語意、取消、部分成交、STP `cancel_newest`、滑價保護帶),以屬性 / 模糊 / golden 測試鎖住,`exchangectl replay` 可重播命令腳本。帳本、trading、auth、鏈上程式碼自 Phase 2 起依 `docs/plan-v1.0.md` 第 12 節逐階段加入。

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
curl -s localhost:9100/metrics | grep exchange_build_info
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
make test-integration   # testcontainers(需要 Docker):migration → seed → GET /v1/markets
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
internal/registry     assets / markets / fee schedules(sqlc)+ seed
internal/money        Decimal 金額型別(禁 float;JSON 字串)
internal/telemetry    slog、correlation id、Prometheus
internal/platform     pgx / NATS / Redis 連線與健康檢查
api/public/v1         OpenAPI 契約
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

Phase 2(`docs/plan-v1.0.md` §12):`internal/ledger` 複式帳本 + Postgres(Hold / Release / Settle / Credit、試算平衡、管理員調帳)。DoD 不過不進下一階段。
