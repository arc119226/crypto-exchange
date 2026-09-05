# ADR-0001:模組化單體、單一 binary 多角色

- 狀態:已採納(Accepted)
- 日期:2026-09-05
- 相關:[ADR-0000](0000-interview-decisions.md) 第 6 輪;`docs/plan-v1.0.md` §5、§8、§10;審查 finding F07 / F16 / P07 / E06 / L05

## 背景

v0.1 規劃了 11 個各自建置的 Go 微服務。審查指出:(1) 服務邊界在餘額、訂單、簽名三處已被證明是錯的,把未驗證的邊界固化成網路邊界是最貴的修正方式;(2) 單人、Go 初學者要同時維護 11 份 main / config / healthz / Dockerfile / CI;(3) 白牌客戶要的是「一個 image + 一份設定」。訪談確認定位是可商業化的白牌引擎原型,不是微服務練習。

## 選項

1. **維持微服務**:邊界清楚但每個邊界都是網路呼叫與部署單位;錯誤邊界的修正成本最高。
2. **單一 process、無角色**:最簡單,但簽名無法隔離、撮合無法固定單一實例、無法一角色一容器地擴充 api / stream。
3. **模組化單體 + 角色**:單一 `go.mod`、單一 binary,`exchange serve --role=api|engine|chain|signer|stream|admin|worker|all`;模組邊界用 `internal/` package + depguard 維持;部署時可一角色一容器。

## 決定

採選項 3。

- 模組邊界:`internal/{money,matching,ledger,registry,policy,trading,eventbus,marketdata,stream,chain/*,auth,audit,webhook,admin,api,app,telemetry,platform}`,相依方向由 `.golangci.yml` depguard 強制(`money` 只准 stdlib + decimal;`matching` 只准 `money`;`ledger` 不得 import trading/chain/api/admin/registry;`api/admin/stream` 只被 `app` import;`cmd/*` 只准 `app`、`telemetry`、`money`;無人 import `cmd`)。
- 角色:`api`、`engine`(恰好 1 個,PG advisory lock)、`chain`、`signer`(恰好 1 個,唯一掛 keystore)、`stream`、`admin`、`worker`、`all`。角色之間在 `role=all` 為 in-process 呼叫,拆分部署為 NATS request-reply。
- 部署:一個 image(`build/Dockerfile`,distroless)、compose profiles `single`(一個容器)與 `app`(一角色一容器)。
- 客戶只透過 REST / WebSocket / Webhook / 事件契約整合;`internal/` 不承諾 Go API 穩定。

## 後果

- 正面:一份 main、config、健康檢查、Dockerfile、CI;模組邊界改起來是 package 重構而不是服務拆併;`role=all` 讓 E2E 在一個容器內跑。
- 負面:單一 binary 把所有依賴(go-ethereum、NATS、Redis)編進每個角色;各角色仍共用一份 image 的攻擊面。接受:以 DB 角色權限與 secret 掛載限制各容器能做的事(§14)。
- 需要守住的事:depguard 規則每個 PR 都跑(Phase 0 已以故意違規證明會擋);新增模組時先在 `.golangci.yml` 宣告允許的 import。
- 觸發重評:某個角色需要獨立的發佈節奏或不同語言(例如撮合改 Rust);屆時該模組先以 NATS 契約隔離,再拆 binary。
