# crypto-exchange(工程師版 README)

> 這是給工程師的索引。第一次接觸、想照著做一遍的人請看 [`../README.md`](../README.md)(繁體中文)或 [`../README.en.md`](../README.en.md)(English)。

白牌交易引擎(white-label exchange engine)的商業化原型:現貨撮合、複式記帳帳本、EVM 充提與歸集、行情推播、管理後台,以單一 Go binary 多角色的模組化單體交付,客戶透過 REST / WebSocket / Webhook 與事件契約整合。

**目前狀態:Phase 7 完成,`v0.1.0` 之前補了介面雙語與新手 README(ADR-0010)。Phase 7:Helm chart 每個 PR 在 kind 上安裝並跑 E2E、單台 VM 的正式 compose、每日備份與每個 PR 一次的還原演練(本機 RTO 8 秒)、每一種密鑰的輪替、九本四段式 runbook、tag 即發布;在那之前把引擎改成 pipelined round trips + group commit(純掛單 17 → 3 次往返、單市場 165 → 266 orders/s)。Phase 0–6 全數合併,4d 在 Sepolia 上實跑過([`docs/guides/sepolia.md`](guides/sepolia.md))。** 已合併:Phase 0 walking skeleton、Phase 1 `internal/matching`(無 I/O、確定性訂單簿)、Phase 2 `internal/ledger`(複式記帳、凍結即分錄、冪等鍵)與 admin API、Phase 3a `internal/trading` + `internal/eventbus`(每市場 runner、一筆交易內 Hold → Apply → 成交 / 分錄 / outbox、重啟重建、JetStream relay)、Phase 3b `internal/auth` + `internal/ratelimit` + public API(JWT / refresh / API key HMAC、限流、`client_order_id` 冪等)。3c 讓拆分部署真的能交易:`internal/cmdbus`(NATS request-reply 命令匯流排,跨容器仍保持 404 / 422 / 503 的錯誤語意,命令帶 `aud=internal` JWT)、`eventbus` 消費端與引擎的`market.updated` 熱載入、`PUT /admin/v1/markets/{symbol}/status`、`api/events/v1/*.json` + `docs/events.md` 事件契約(golden + JSON Schema 測試),以及每個 PR 都跑的多容器 `make e2e`。

4a-1 已合併:`internal/chain/hdwallet`(BIP-44 派生、scrypt + AES-256-GCM 的 `hd-seed.json`)、`exchange keys import-mnemonic`、signer role 維護的**預生成充值地址池**、`GET /v1/deposit-address`——api role 只認領地址,永遠拿不到金鑰。

4a-2 已合併,鏈上的錢真的被看見:`internal/chain/evm`(ethclient 封裝、wei 邊界)、`internal/chain/deposit`(掃描器:原生 ETH 與 ERC-20 兩條路徑、確認數、reorg 回退與孤立/丟棄、`Credit` 入帳)、`deposit.*` 事件契約、`GET /v1/deposits`,以及 `scripts/e2e.sh` 裡真的用 `cast` 把 ETH 與 MockUSDC 打進充值地址再等餘額變動。

4b-1 已合併,是提現的前半段,**不碰鏈也不碰任何私鑰**:`POST /v1/withdrawals`(必帶 `Idempotency-Key`)、`policy.WithdrawalPolicy`(單筆與每日限額依 KYC 等級,超標一律進人工審核而不是拒絕)、chain role 的 worker 把提現推到 `funds_locked`(`ledger.Hold` 與狀態同一筆交易)、admin 的審核佇列,以及 `exchangectl withdrawals` / `exchangectl admin withdrawals`。

4b-2 已合併,讓錢真的出得去:`internal/chain/signer`(簽的是**意圖**而不是別人組好的交易——ERC-20 的收款人埋在 calldata 裡,自己組就不必解碼再相信解碼)、`chain.signing_log` 的 UNIQUE `(kind, ref_id, attempt)` 讓一個意圖只能被簽一次、`internal/signerbus`(signer role 的 NATS request-reply,私鑰永遠不上線)、`internal/chain/hotwallet` 的 nonce 管理(三條啟動規則,鏈上有我們沒配過的 nonce 就**拒絕啟動**)、`funds_locked → signed → broadcast → confirmed` 與 §6.1.4(e) 的分錄(確認時 gas 是**另一筆**分錄,永遠記在原生幣)、EIP-1559 加價重送與上限,以及 `POST /admin/v1/withdrawals/{id}/resolve` 的四種人工處置。

`resolve` 是**請求**而不是動作:admin role 沒有節點也沒有金鑰,它只寫四個請求欄,chain role 在自己的 tick 上執行——能寫 `tx_hash` 的角色可以讓一筆提現看起來已經送出卻什麼都沒簽過。處置的操作步驟在 [`docs/runbooks/stuck-withdrawal.md`](runbooks/stuck-withdrawal.md)。

4c-1 是歸集,把充值和提現接起來:`internal/chain/sweep`(ETH 一筆、ERC-20 兩筆——只收過代幣的地址一滴 ETH 都沒有,付不起自己的轉帳,所以熱錢包要先補 gas)、§6.1.4(f) 的 custody 分錄、`sweep.*` 事件、`GET /admin/v1/sweeps`。歸集永遠不動使用者餘額,而且刻意只收「帳本真的入過帳的數」:鏈上餘額可以合法地更高(掃描器看不到的合約內部轉帳),把那部分掃走等於讓 custody 為一筆從來沒收到的轉帳背書。多的錢留在鏈上。

**4c-2 是對帳,去看那筆多出來的錢。** 每個資產比對「帳本的 `custody_deposit_addresses + custody_hot`」與「全部充值地址 + 熱錢包的鏈上餘額」,`GET /admin/v1/reconciliation` 與 `exchangectl admin reconcile` 顯示結果(§6.4.4)。兩側從來不會看著同一個瞬間,所以有兩個修正項——鏈上看得到但還沒入帳的充值,以及帳本在邊界之上已經記了的 movement——**兩個都是精確算出來的,所以容差是零**。餘額全部釘在同一個區塊讀,而那個區塊就是掃描器、提現 worker 和歸集三者記帳用的同一條邊界,再往回夾到掃描器的游標:帳本對鏈的認識是那個游標,不是節點的 head。

差異不為零就寫 `admin.reconciliation_breaks` 並發 `reconciliation.break_detected`;熱錢包低於 `ETH_HOT_WALLET_MIN` 發 `alert.hot_wallet_low`。兩者都是邊緣觸發的——一直存在的狀況留在報告和指標裡,每五分鐘重喊一次只會教人設過濾器。

**第一次跑對帳一定會找到東西**,而且它是對的:dev 鏈直接給熱錢包 100 ETH,沒有任何一筆本系統的交易把它放進去。答案是 `exchangectl admin house-adjust`(§6.1.4 g 的 `external` 科目),不是在比較裡加一條例外。e2e 因此走三步:斷言第一次的差異是正的、記下正好那個數、跑完整流程之後**一個字都不記**地回到零。

**對帳上線第一天就抓到一個 4c-1 留下的真漏洞**:歸集代幣時熱錢包補給充值地址的那筆 gas,在鏈上就是一筆流入受監控地址的普通轉帳,於是掃描器把它當成使用者的充值入帳了——使用者白得一筆交易所墊的 ETH,而 `custody_deposit_addresses` 為同一筆移動記了兩次。它撐過了完整的 integration 套件和兩輪 e2e,直到有東西真的拿帳本去和鏈上比。修法是把規則寫對:**充值是從交易所外面到達的錢**,發送方是自己的地址就不是充值。

對帳另外還抓到兩個:nonce 補洞燒掉的 gas 從來沒進帳本(`hotwallet` 整個套件沒 import `ledger`),以及一次失敗的簽名會讓下一個 tick 記下一筆熱錢包從來沒送出去的 ETH。細節在 [`docs/domain.md`](domain.md) §20 與 [`docs/runbooks/reconciliation-break.md`](runbooks/reconciliation-break.md)。

4d 是拿這一整套去對真的鏈:Sepolia 上手動走完充值 → 提現 → 歸集 → 對帳,七筆交易的 hash、gas 與區塊都記在 [`docs/guides/sepolia.md`](guides/sepolia.md)。準備階段對真節點做讀取實測就抓到四個只在真鏈上才會踩到的缺陷,實跑之後又找到八個,細節在 [`docs/domain.md`](domain.md) §21–§22。

**Phase 5a 已合併,worker role 第一次做真的工作:出站 webhook 的投遞路徑。** `internal/webhook` 從 JetStream 收事件、寫進自己的佇列、按 §7.6 的排程(1m → 5m → 30m → 2h → 12h → 24h)投遞,每一次嘗試連狀態碼、耗時、錯誤一起記進 `webhook.deliveries`。簽章是 `HMAC-SHA256(secret, timestamp + "." + body)`,刻意不是 API key 那一套。退避排程住在資料庫而不是 JetStream,因為 nak 的延遲是單一固定值配 30s AckWait,撐不過第一分鐘。

**本 PR(5b)是把它接上人**:`POST/GET /admin/v1/webhooks`、`PUT {id}` 與 `{id}/status`、投遞紀錄查詢、手動 replay,加上 `exchangectl admin webhooks` 與 `exchangectl webhook-sink`——一個本機接收並**驗簽**的工具,用的是伺服器簽章時的同一份程式碼。**沒有 DELETE**:投遞歷史必須活得比整合關係久,所以結束的 endpoint 是停用而不是刪除。

設計後台這一步本身抓到三個 5a 留下的缺陷,而且沒有一個讀 schema 讀得出來:0016 的「防重複投遞」索引其實站在 POST 的**下游**,擋不了重送、只能藏住重送的紀錄;replay 的嘗試會撞上舊一輪的編號被 `ON CONFLICT DO NOTHING` 默默吞掉(replay 一筆 dead 的投遞會產生**零列**紀錄);而一個過期的結算會刪掉別人剛排進去的佇列列。修法是給「一輪投遞」一個 `run_id`,細節與教訓在 [`docs/domain.md`](domain.md) §23。客戶要讀的那一面在 [`docs/webhooks.md`](webhooks.md),包括**投遞成功之後晚到的重送會讓客戶再收到一次**這件事——至少一次投遞是契約,不是免責聲明。

## 產品邊界

- **A. 引擎交付物**(有相容承諾):撮合、帳本、交易狀態機、行情、EVM 充提與歸集、簽名隔離、registry、管理後台、對帳、webhook;以單一 container image + Helm chart + OpenAPI / 事件契約交付。
- **B. 參考實作**(可整包替換):最小 auth(users + JWT + API key)、React 參考前台、`exchangectl` CLI。
- **C. 明確不做**:KYC 文件蒐集、用戶端 2FA、通知內容、主網與真實資金(只接 anvil 與 Sepolia)。

核心程式碼全部在 `internal/`,客戶只透過 REST / WebSocket / Webhook 與事件契約整合;引擎內部一律以 `account_id` 為鍵,不認識 email 或 KYC。

## 快速開始

需求:Go 1.26(`go.mod` 釘住 toolchain,會自動下載)、Docker Desktop 或 Docker Engine + Compose v2、`make`、`openssl`。選用:Node ≥ 22(只有 `web/trade` 的 `npm run dev` 與 Playwright 需要;compose 的 `web` 服務會在容器裡 build 前台)、`jq` / `curl` / `websocat`(下面的範例用到)。Foundry 不必裝,合約走 docker。

```sh
make gen-dev-secrets    # .env(隨機密鑰)、secrets/jwt/ed25519.pem、新的 BIP-39 助記詞與 HOT_WALLET_ADDRESS、
                        # secrets/keystore/hd-seed.json(signer 的加密種子)
                        # 4a-1 之前跑過的人要再跑一次,否則 signer 沒有種子起不來
make up-single          # postgres / redis / nats / anvil / MockUSDC 部署 / migrate / seed / exchange-all / prometheus / grafana / web(前台 :8088)
make ps                 # 每個容器的狀態;make logs SERVICE=exchange-all TAIL=200 FOLLOW=1 看 log

open http://localhost:8088                      # 參考前台(edge image 內 build 好的靜態檔 + Caddy 代理 /v1 與 /ws;WEB=0 不起它);右上角切 中文 / EN
curl -s localhost:8080/v1/markets | jq          # seed 進去的 ETH-USDC,金額一律字串("price_tick": "0.01")
go run ./cmd/exchangectl markets list           # 同一件事,走產生的 OpenAPI client
curl -s localhost:9100/readyz                   # {"status":"ok", ...}(9100 只有 up-single 發布到 host;拆分部署要 docker compose exec 進容器)
curl -s localhost:9100/metrics | grep -E 'exchange_build_info|ledger_trial_balance_diff'

# 帳本(admin API,金鑰在 .env 的 ADMIN_API_KEY)
export EXCHANGE_ADMIN_URL=http://localhost:8082 EXCHANGE_ADMIN_API_KEY=$(sed -n 's/^ADMIN_API_KEY=//p' .env)
ACC=$(go run ./cmd/exchangectl admin accounts create)                                   # 開一個現貨帳戶
go run ./cmd/exchangectl admin fund --account $ACC --asset USDC --amount 10000            # dev faucet(external → available,寫審計)
make faucet ACCOUNT=$ACC ASSET=USDC AMOUNT=10000                                          # 同一件事,在容器內跑 exchangectl,不需要 Go 也不需要匯出金鑰
go run ./cmd/exchangectl admin balances $ACC && go run ./cmd/exchangectl admin trial-balance   # 每資產 diff = 0

# 後台(admin role 的 :8082;人用密碼 + TOTP 登入,機器用上面的 API key)
make totp-enroll EMAIL=$(sed -n 's/^ADMIN_BOOTSTRAP_EMAIL=//p' .env)   # 在容器內跑 exchange admin totp enroll:印 otpauth URL 與 secret,只印這一次;加進 authenticator
open http://localhost:8082/admin/login          # 密碼是 .env 的 ADMIN_BOOTSTRAP_PASSWORD;第一個 code 完成啟用,之後每次登入都要碼;右上角切 中文 / EN

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

# Webhook(§7.6;客戶那一面的說明在 docs/webhooks.md)
WH=$(go run ./cmd/exchangectl admin webhooks create --url http://host.docker.internal:9999 \
    --events 'trade.executed,withdrawal.state_changed' --output json)   # secret 只顯示一次
go run ./cmd/exchangectl webhook-sink --port 9999 --secret $(echo $WH | jq -r .secret) &
go run ./cmd/exchangectl e2e --verbose                                  # sink 印出已驗簽的事件
EP=$(echo $WH | jq -r .id)
go run ./cmd/exchangectl admin webhooks deliveries $EP                  # 每次嘗試的狀態碼、耗時、錯誤
go run ./cmd/exchangectl admin webhooks replay $EP $DELIVERY_ID         # 重送(客戶會再收到一次,event_id 相同)

# 行情與推播(Phase 6;wire format 在 docs/ws-api.md)
go run ./cmd/exchangectl ticker ETH-USDC && go run ./cmd/exchangectl klines ETH-USDC --interval 1m --limit 5
websocat ws://localhost:8081/ws/v1/public <<< '{"op":"subscribe","channel":"depth","market":"ETH-USDC"}'   # snapshot,之後每個 seq 一則 delta
websocat ws://localhost:8081/ws/v1/private <<< "{\"op\":\"auth\",\"token\":\"$EXCHANGE_TOKEN\"}"            # 之後下單:orders / fills / balances 三個頻道都會推
(cd web/trade && npm ci && npm run dev)         # 前台的開發模式 http://localhost:5173(Vite 把 /v1 與 /ws 代理到 8080 / 8081;改程式立即重載)
make web-e2e                                    # Playwright 冒煙:註冊 → 注資 → 掛單 → 訂單簿出現 → 對手單 → 成交、餘額變動
make loadgen                                    # 60 s 壓測;數字與瓶頸分析在 docs/loadtest.md
open http://localhost:3000                      # Grafana(admin / .env 的 GRAFANA_ADMIN_PASSWORD):Exchange Overview / Ledger / Chain / Stream / System
open http://localhost:9090/alerts               # Prometheus:§15 的七條告警(沒有 Alertmanager)
open http://localhost:16686                     # Jaeger:找 exchange-api 的 POST /v1/orders,看它一路到 engine、Postgres 與每個 consumer

# 營運(Phase 7;每一步的手冊在 docs/runbooks/,上線清單在 docs/beta-checklist.md)
make up BACKUP=1                                # 多 minio + backup sidecar:每日 pg_dump、每 30 s 出貨 WAL,結果寫 admin.backups
make backup-drill                               # 現在備份一次 → 還原到拋棄式 DB → 驗試算平衡 / 序號 / 成交 → 印 RTO(CI 每個 PR 跑)
go run ./cmd/exchange keys jwt-public --in secrets/jwt/ed25519.pem      # JWKS 的 kid;輪替流程在 docs/runbooks/key-rotation.md
DATABASE_URL=postgres://ex_migrate:...@localhost:5432/exchange API_KEY_MASTER_KEY=新 API_KEY_MASTER_KEY_PREVIOUS=舊 \
  go run ./cmd/exchange keys rewrap --domain api-keys                    # 主金鑰輪替:一筆交易把每一列改成新金鑰封的,冪等
make helm-lint                                  # chart:lint + template + kubeconform(不需要叢集)
make kind-up && make helm-e2e && make kind-down # 需要 kind + kubectl + docker:CI helm job 在本機的樣子
make release-check TAG=v0.1.0                   # 發布前:chart appVersion 與 binary 都報 v0.1.0(docs/release.md)
sudo scripts/gen-prod-secrets.sh && make up-prod   # 單台 VM 的正式形態(docs/runbooks/beta-deploy.md;secrets 全是檔案、只開 80/443)

make down               # 停止(保留資料)
make reset              # 停止並清空 postgres / nats / anvil 狀態與合約產物

# 真的鏈(Sepolia,手動、不進 CI)
# 逐步操作在 docs/guides/sepolia.md;它的 Part A 不需要這裡的任何東西就能開始
make up-sepolia         # compose.yaml + compose.sepolia.yaml,獨立的 project name 與 volume
make down-sepolia
```

`make up` 改為每個 role 一個容器(api / engine / chain / signer / stream / admin / worker);`OBS=0` 可略過 prometheus / grafana / jaeger,`WEB=0` 可略過前台的 `web` 服務。

## 開發循環

```sh
make lint               # go vet + golangci-lint(depguard 模組邊界、forbidigo 禁 float)+ gitleaks;版本全釘在 tools/go.mod
make secrets-scan       # 只跑 gitleaks(掃 git 歷史,不掃工作目錄——.env 與 secrets/ 是本機真金鑰,本來就 gitignored)
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

介面語言(ADR-0010):前台與後台都是繁體中文 / 英文,英文是來源、中文照它翻。前台的字串在 `web/trade/src/i18n/messages.ts`(`tsc` 用 `Record<Key, string>` 逼中文補齊)、狀態碼在 `enums.ts`、`Problem.detail` 的對照在 `problems.ts`;後台在 `internal/admin/i18n_en.go` / `i18n_zh_tw.go`,template 用 `{{T "key"}}`,`TestMessagesComplete` 檢查兩份 key 相同、`fmt` 動詞相同、沒有死 key。CLI、API、事件、log、工程文件維持英文;路由與狀態碼不翻;金額不走 `Intl.NumberFormat`。

## Repo 結構

```
cmd/exchange          單一 binary:serve --role=api|engine|chain|signer|stream|admin|worker|all、migrate、seed、healthcheck、keys(gen-jwt / jwt-public / import-mnemonic / rekey / rewrap)
cmd/exchangectl       開發/營運 CLI(產生的 OpenAPI client)
internal/app          設定、run loop、/healthz /readyz /metrics、SIGTERM drain、依賴退避
internal/api          public REST(oapi-codegen strict server)+ RFC 7807 + 限流
internal/auth         最小 auth 參考實作:users、argon2id、Ed25519 JWT / JWKS、refresh 輪替、API key HMAC、Authenticate 中介層
internal/ratelimit    token bucket(Redis Lua / 記憶體 / Fallback)
internal/matching     純函式訂單簿(Apply / Restore / Snapshot;無 I/O、無時鐘)
internal/trading      訂單狀態機、每市場 runner(一組 ≤ 50 個命令一筆 PG 交易,pipelined round trips,失敗退回逐筆)、client_order_id 冪等、重建
internal/eventbus     事件 envelope、outbox、JetStream relay / streams、durable consumer
internal/cmdbus       api → engine 的 NATS request-reply 命令匯流排(含 aud=internal JWT)
internal/policy       同步下單規則(市場狀態、帳戶凍結)
internal/ledger       複式帳本:Post / Hold / Release / Settle / Credit / Adjust、balances 快取、試算平衡(sqlc)
internal/audit        append-only 稽核紀錄
internal/admin        admin REST(oapi-codegen strict server)+ X-Admin-Api-Key;html/template + htmx 後台(zh-TW / en,i18n_*.go)
internal/webhook      出站投遞:HMAC 簽章、退避排程、deliveries、endpoint 管理與 replay
internal/registry     assets / markets / fee schedules(sqlc)+ seed
internal/money        Decimal 金額型別(禁 float;JSON 字串)
internal/marketdata   影子訂單簿投影(depth delta)、K 線聚合與落地、ticker、Redis 深度快照(sqlc)
internal/stream       WebSocket server:公開頻道(depth / trades / ticker / kline)、私有頻道(auth、account_seq、resume 自 outbox)、慢客戶端斷線
internal/telemetry    slog、correlation id、Prometheus、OpenTelemetry(HTTP span、traceparent 注入 / 抽取)
internal/platform     pgx / NATS / Redis 連線與健康檢查、pgx query / batch tracer、pool collector、pg.Batch;secretbox 是 auth 與 webhook 共用的 AES-256-GCM 信封 + 輪替用的 Keyring
api/public/v1         公開 OpenAPI 契約
api/admin/v1          admin OpenAPI 契約
migrations            goose SQL(embed)
deploy/compose        compose.yaml(profiles:infra / app / single / observability / web / backup)、compose.sepolia.yaml、compose.prod.yaml(beta VM:檔案型 secrets、edge、node-exporter)
deploy/helm/exchange  Helm chart(每 role 一個 Deployment、migrate hook、dev.enabled 的測試依賴 hook);helm_test.go 在 make test 裡 lint + kubeconform
deploy/vm             Ubuntu 24.04 的 bootstrap.sh(Docker CE、ufw、systemd unit)
build                 Dockerfile(Go image)、edge/(Caddy + 前台;Caddyfile 是 beta 的 TLS 站、Caddyfile.dev 是 compose 的 :8088)、backup/(postgres 客戶端 + mc 的備份 sidecar)
scripts               gen-dev-secrets、gen-prod-secrets、e2e、helm-e2e、kind-secrets、backup、restore-drill(+ sql/restore-checks.sql)、db-roles.sql
infra/contracts       MockUSDC + 冪等部署腳本(Foundry)
infra/postgres        ex_* 登入角色 initdb 腳本、postgresql.prod.conf(WAL 歸檔)
infra/nats            nats.prod.conf(密碼登入)
infra/observability   prometheus(含 alerts.yml 十一條)/ grafana 設定與五個 dashboard JSON;observability_test.go 驗指標名
web/trade             參考前台(React + Vite + TS;OpenAPI 產 TS client;zh-TW / en 目錄在 src/i18n;Playwright 冒煙以中文介面跑,另有語言切換 spec)
test/integration      testcontainers 整合測試(build tag integration)
test/docs             runbooks_test.go:每本 runbook 四段、引用的檔案 / 子命令 / 指標都存在;readme_test.go:三份 README 的連結都存在、中英文版章節與程式碼區塊一致
test/fixtures/matching 撮合命令腳本與 golden 事件 / 快照
docs                  計畫、審查、ADR、領域文件、runbooks/(四段式營運手冊)、guides/(Sepolia 教學)、beta-checklist、release
```

## 文件索引

| 文件 | 內容 |
|---|---|
| [`docs/plan-v1.0.md`](plan-v1.0.md) | **分階段可執行計畫 v1.1**(定位、範圍、領域模型、契約、模組、選型、compose、Phase 0~9、測試/CI、安全、觀測、部署、風險;v1.1 加 §22 v2 主網閘門、§23 營收模型與三種手續費、§24 對照;檔名維持 v1.0) |
| [`docs/review/plan-review-2026-09.md`](review/plan-review-2026-09.md) | v0.1 規劃書審查報告(28 條合併後發現、不採納意見、對 v1.0 的結構性要求) |
| [`docs/domain.md`](domain.md) | 領域文件:科目表、分錄、狀態機、撮合語意的逐項驗算與疑問清單;各 Phase 程式碼與計畫的對應表 |
| [`docs/api-conventions.md`](api-conventions.md) | Public API 慣例:金額字串、problem+json、JWT / API key HMAC 簽章、限流、`client_order_id` 狀態碼(English) |
| [`docs/events.md`](events.md) | 事件契約:envelope、subject 與 stream、排序與去重、consumer 型別、catalog、相容規則(English);schema 在 [`api/events/v1/`](../api/events/v1) |
| [`docs/webhooks.md`](webhooks.md) | 出站 Webhook:簽章與驗證、重試排程、**至少一次投遞的實際後果**、endpoint 管理與 replay(English) |
| [`docs/ws-api.md`](ws-api.md) | WebSocket:公開 / 私有頻道的訊息、depth 的客戶端規則、`account_seq` 與 resume、錯誤碼與斷線原因(English) |
| [`docs/loadtest.md`](loadtest.md) | 本機壓測:Phase 6 的四組 run、瓶頸(每命令 17 次往返);§8 Phase 7 group commit 之後再量一次(3 次往返、266 orders/s) |
| [`docs/runbooks/`](runbooks/) | 九本四段式(症狀 / 檢查指令 / 處置 / 驗證)營運手冊:engine 重啟、卡住的提現、reorg 告警、熱錢包低水位、對帳差異、備份還原、密鑰輪替、beta 部署、admin TOTP |
| [`docs/guides/sepolia.md`](guides/sepolia.md) | 從零到 Sepolia 實跑的教學(4d 的逐筆交易、gas 與區塊) |
| [`docs/beta-checklist.md`](beta-checklist.md) | Beta 上線檢查表:這個 beta 的限制(單機、RPO 24h、無 PITR)、上線前的勾選項、運維節奏、刻意沒做的 |
| [`docs/release.md`](release.md) | 發布:`vX.Y.Z` tag 做什麼、版本斷言、`make release-check`、失敗時怎麼辦 |
| [`docs/screenshots/`](screenshots/) | 後台每一頁的截圖(`make screenshots` 產生)與參考前台的交易頁 / 錢包頁;新手 README 引用其中兩張 |
| [`docs/adr/`](adr/) | ADR-0000 需求訪談決策(8 輪 32 題);ADR-0001~0011 架構決策(單體、真相來源、租戶、數值、帳本、認證、簽名、工具鏈、beta 形態與備份政策、介面語言與新手 README、營收模型與主網路線) |
| [`docs/archive/plan-v0.1.md`](archive/plan-v0.1.md) | 原始 v0.1 規劃書(已取代,僅供對照) |

## 下一步

Phase 0–7 全數完成,`docs/plan-v1.0.md` §12 的每個 DoD 都有對應的測試或 CI job。Phase 7 的對應與偏離在 [`docs/domain.md`](domain.md) §26,四個沒有唯一答案的決定在 ADR-0009。

接下來是 beta 本身:照 [`docs/runbooks/beta-deploy.md`](runbooks/beta-deploy.md) 起一台 VM、打勾 [`docs/beta-checklist.md`](beta-checklist.md)、每週演練一次還原、每 90 天輪一次密鑰。已知的缺口寫在 checklist 的第一段:可還原的 RPO 是 24 小時(WAL 有歸檔但沒有 base backup)、還原後熱錢包 nonce 要人工對帳、沒有 Alertmanager、單市場 266 orders/s 離 §3.3 的 1,000 還有距離——下一個瓶頸是每句 SQL 的 Postgres 成本,不再是往返數。
