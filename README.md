# crypto-exchange

白牌交易引擎(white-label exchange engine)的商業化原型:現貨撮合、複式記帳帳本、EVM 充提與歸集、行情推播、管理後台,以單一 Go binary 多角色的模組化單體交付,客戶透過 REST / WebSocket / Webhook 與事件契約整合。

**目前狀態:Phase 4c-1 進行中(歸集;本 PR)。Phase 3、4a 與 4b 已完成。** 已合併:Phase 0 walking skeleton、Phase 1 `internal/matching`(無 I/O、確定性訂單簿)、Phase 2 `internal/ledger`(複式記帳、凍結即分錄、冪等鍵)與 admin API、Phase 3a `internal/trading` + `internal/eventbus`(每市場 runner、一筆交易內 Hold → Apply → 成交 / 分錄 / outbox、重啟重建、JetStream relay)、Phase 3b `internal/auth` + `internal/ratelimit` + public API(JWT / refresh / API key HMAC、限流、`client_order_id` 冪等)。3c 讓拆分部署真的能交易:`internal/cmdbus`(NATS request-reply 命令匯流排,跨容器仍保持 404 / 422 / 503 的錯誤語意,命令帶 `aud=internal` JWT)、`eventbus` 消費端與引擎的`market.updated` 熱載入、`PUT /admin/v1/markets/{symbol}/status`、`api/events/v1/*.json` + `docs/events.md` 事件契約(golden + JSON Schema 測試),以及每個 PR 都跑的多容器 `make e2e`。

4a-1 已合併:`internal/chain/hdwallet`(BIP-44 派生、scrypt + AES-256-GCM 的 `hd-seed.json`)、`exchange keys import-mnemonic`、signer role 維護的**預生成充值地址池**、`GET /v1/deposit-address`——api role 只認領地址,永遠拿不到金鑰。

4a-2 已合併,鏈上的錢真的被看見:`internal/chain/evm`(ethclient 封裝、wei 邊界)、`internal/chain/deposit`(掃描器:原生 ETH 與 ERC-20 兩條路徑、確認數、reorg 回退與孤立/丟棄、`Credit` 入帳)、`deposit.*` 事件契約、`GET /v1/deposits`,以及 `scripts/e2e.sh` 裡真的用 `cast` 把 ETH 與 MockUSDC 打進充值地址再等餘額變動。

4b-1 已合併,是提現的前半段,**不碰鏈也不碰任何私鑰**:`POST /v1/withdrawals`(必帶 `Idempotency-Key`)、`policy.WithdrawalPolicy`(單筆與每日限額依 KYC 等級,超標一律進人工審核而不是拒絕)、chain role 的 worker 把提現推到 `funds_locked`(`ledger.Hold` 與狀態同一筆交易)、admin 的審核佇列,以及 `exchangectl withdrawals` / `exchangectl admin withdrawals`。

4b-2 已合併,讓錢真的出得去:`internal/chain/signer`(簽的是**意圖**而不是別人組好的交易——ERC-20 的收款人埋在 calldata 裡,自己組就不必解碼再相信解碼)、`chain.signing_log` 的 UNIQUE `(kind, ref_id, attempt)` 讓一個意圖只能被簽一次、`internal/signerbus`(signer role 的 NATS request-reply,私鑰永遠不上線)、`internal/chain/hotwallet` 的 nonce 管理(三條啟動規則,鏈上有我們沒配過的 nonce 就**拒絕啟動**)、`funds_locked → signed → broadcast → confirmed` 與 §6.1.4(e) 的分錄(確認時 gas 是**另一筆**分錄,永遠記在原生幣)、EIP-1559 加價重送與上限,以及 `POST /admin/v1/withdrawals/{id}/resolve` 的四種人工處置。

`resolve` 是**請求**而不是動作:admin role 沒有節點也沒有金鑰,它只寫四個請求欄,chain role 在自己的 tick 上執行——能寫 `tx_hash` 的角色可以讓一筆提現看起來已經送出卻什麼都沒簽過。處置的操作步驟在 [`docs/runbooks/stuck-withdrawal.md`](docs/runbooks/stuck-withdrawal.md)。

本 PR(4c-1)是歸集,把充值和提現接起來:`internal/chain/sweep`(ETH 一筆、ERC-20 兩筆——只收過代幣的地址一滴 ETH 都沒有,付不起自己的轉帳,所以熱錢包要先補 gas)、§6.1.4(f) 的 custody 分錄、`sweep.*` 事件、`GET /admin/v1/sweeps`。signer 也第一次用熱錢包以外的金鑰簽東西:歸集是充值地址自己送出的,而**用哪把金鑰由地址列上的 derivation index 決定,不由請求決定**。

**歸集永遠不動使用者餘額**:它在兩個 house 帳戶之間搬錢並記 gas,被清空地址的那個帳戶餘額一分不變。這也是 `custody:hot` 之前為什麼會是負的——熱錢包一直在付一筆不是從它這裡收進來的錢,而這個 PR 把缺口接上了。

兩條規則決定什麼可以歸集,都是為了讓 `custody:deposit_addresses` 誠實:**有還沒入帳的充值的地址整個跳過**,而且**歸集金額上限是帳本真的入過帳的數**——鏈上餘額可以合法地更高(掃描器看不到的合約內部轉帳),把那部分掃走等於讓 custody 為一筆從來沒收到的轉帳背書。多的錢留在鏈上,4c-2 的對帳會看到它。

## 產品邊界

- **A. 引擎交付物**(有相容承諾):撮合、帳本、交易狀態機、行情、EVM 充提與歸集、簽名隔離、registry、管理後台、對帳、webhook;以單一 container image + Helm chart + OpenAPI / 事件契約交付。
- **B. 參考實作**(可整包替換):最小 auth(users + JWT + API key)、React 參考前台、`exchangectl` CLI。
- **C. 明確不做**:KYC 文件蒐集、用戶端 2FA、通知內容、主網與真實資金(只接 anvil 與 Sepolia)。

核心程式碼全部在 `internal/`,客戶只透過 REST / WebSocket / Webhook 與事件契約整合;引擎內部一律以 `account_id` 為鍵,不認識 email 或 KYC。

## 快速開始

需求:Go 1.26(`go.mod` 釘住 toolchain,會自動下載)、Docker Desktop 或 Docker Engine + Compose v2、`make`、`openssl`。

```sh
make gen-dev-secrets    # .env(隨機密鑰)、secrets/jwt/ed25519.pem、新的 BIP-39 助記詞與 HOT_WALLET_ADDRESS、
                        # secrets/keystore/hd-seed.json(signer 的加密種子)
                        # 4a-1 之前跑過的人要再跑一次,否則 signer 沒有種子起不來
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

# 交易(public API;JWT session 或 API key HMAC,規格見 docs/api-conventions.md)
go run ./cmd/exchangectl user register --email alice@example.com --password 'correct horse battery'   # 印出 export EXCHANGE_TOKEN=...
export EXCHANGE_TOKEN=...                                                                              # 貼上一步的輸出
go run ./cmd/exchangectl admin fund --account $(go run ./cmd/exchangectl user me --output json | jq -r .account_id) --asset USDC --amount 10000
go run ./cmd/exchangectl orders place --side buy --price 2000 --qty 0.5    # 201 open;同 --client-order-id 重送 → 200 原單;餘額不足 → 201 rejected
go run ./cmd/exchangectl book ETH-USDC && go run ./cmd/exchangectl balances && go run ./cmd/exchangectl orders list --open
go run ./cmd/exchangectl api-keys create --scopes read,trade               # secret 只顯示一次;之後 --api-key/--api-secret 對每個請求 HMAC 簽章
go run ./cmd/exchangectl e2e --verbose                                     # 兩個用戶走完 docs/plan-v1.0.md §6.1.4 的數字並驗試算平衡

# 拆分部署(api / engine / admin 各一容器,命令走 NATS)
make up                    # infra + app profile;--role=api 的交易端點此時由 NATS 命令匯流排送到引擎
make e2e                   # 上面那條路徑的完整驗證:exchangectl e2e、kill -9 引擎後訂單簿一致、停牌後下一張單被拒
go run ./cmd/exchangectl admin markets set-status ETH-USDC halted --reason "maintenance"   # 引擎熱載入,無需重啟
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
make test-integration   # testcontainers(需要 Docker):migration、seed、帳本、admin API、trading 引擎(kill/restart、屬性、併發)、outbox relay、public API(auth、HMAC、限流 429、201/200/422)
TEST_PG_ADMIN_URL=postgres://exchange:test@127.0.0.1:5433/postgres make test-integration   # 改用現成的本機 Postgres(先跑 infra/postgres/initdb/01-roles.sh),每個測試一個新 database
TEST_NATS_URL=nats://127.0.0.1:4222 ...                                                    # 同理改用現成的 `nats-server -js`(outbox relay 測試會 purge 它用到的 stream)
make contracts-test     # 在釘住的 foundry 映像內跑 forge test
make infra-up && make migrate && make seed && make run ROLE=api   # 只起基礎設施,role 在主機上 go run
```

契約先行:改 `api/public/v1/openapi.yaml` → `make gen` → 實作 `internal/api` 的 strict server 介面。資料庫改動一律新增 `migrations/NNNN_<module>_<desc>.sql`(goose、只 forward),查詢寫在 `internal/<module>/queries/*.sql` 交給 sqlc。

## Repo 結構

```
cmd/exchange          單一 binary:serve --role=api|engine|chain|signer|stream|admin|worker|all、migrate、seed、healthcheck、keys
cmd/exchangectl       開發/營運 CLI(產生的 OpenAPI client)
internal/app          設定、run loop、/healthz /readyz /metrics、SIGTERM drain、依賴退避
internal/api          public REST(oapi-codegen strict server)+ RFC 7807 + 限流
internal/auth         最小 auth 參考實作:users、argon2id、Ed25519 JWT / JWKS、refresh 輪替、API key HMAC、Authenticate 中介層
internal/ratelimit    token bucket(Redis Lua / 記憶體 / Fallback)
internal/matching     純函式訂單簿(Apply / Restore / Snapshot;無 I/O、無時鐘)
internal/trading      訂單狀態機、每市場 runner(一筆 PG 交易:Hold → Apply → 成交 / 分錄 / outbox)、client_order_id 冪等、重建
internal/eventbus     事件 envelope、outbox、JetStream relay / streams、durable consumer
internal/cmdbus       api → engine 的 NATS request-reply 命令匯流排(含 aud=internal JWT)
internal/policy       同步下單規則(市場狀態、帳戶凍結)
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
| [`docs/domain.md`](docs/domain.md) | 領域文件:科目表、分錄、狀態機、撮合語意的逐項驗算與疑問清單;各 Phase 程式碼與計畫的對應表 |
| [`docs/api-conventions.md`](docs/api-conventions.md) | Public API 慣例:金額字串、problem+json、JWT / API key HMAC 簽章、限流、`client_order_id` 狀態碼(English) |
| [`docs/events.md`](docs/events.md) | 事件契約:envelope、subject 與 stream、排序與去重、consumer 型別、catalog、相容規則(English);schema 在 [`api/events/v1/`](api/events/v1) |
| [`docs/adr/`](docs/adr/) | ADR-0000 需求訪談決策(8 輪 32 題);ADR-0001~0008 架構決策(單體、真相來源、租戶、數值、帳本、認證、簽名、工具鏈) |
| [`docs/archive/plan-v0.1.md`](docs/archive/plan-v0.1.md) | 原始 v0.1 規劃書(已取代,僅供對照) |

## 下一步

Phase 3(`docs/plan-v1.0.md` §12)分三個 PR 全數合併,DoD 全滿足。Phase 4 鏈上分 4a 充值、4b 提現、4c 歸集與對帳、4d Sepolia 驗證;4a 再拆 4a-1(金鑰與地址,已合併)與 4a-2(掃描與入帳,本 PR)。DoD 不過不進下一階段。
