# ADR-0008:工具鏈與依賴版本釘住

- 狀態:已採納(Accepted)
- 日期:2026-09-05
- 相關:`docs/plan-v1.0.md` §9、§11、§13.3、§17;PR #2(Phase 0)

## 背景

計畫要求「本機與 CI 用同一版本」「所有 image 用明確 tag」「升級走獨立 PR」。Phase 0 實作時遇到三個偏離計畫的事實:(1) 計畫寫 Go 1.23+,但 `prometheus/client_golang v1.24.1` 需要 Go 1.25、`sqlc v1.31.1` 需要 Go 1.26,而 Go 1.24 已 EOL;(2) 若把 oapi-codegen / sqlc / golangci-lint 以 `tool` directive 放進主 `go.mod`,主模組的 `go.sum` 與 Docker build context 會被數百個間接依賴撐大;(3) 環境預裝的 golangci-lint 2.5.0 以 Go 1.25 建置,拒絕 Go 1.26 模組。

## 選項

1. **不釘,用 latest**:CI 與本機隨時漂移,`foundry`、`golangci-lint` 都曾因版本改變行為。
2. **釘在主 go.mod 的 `tool` directive**:單一檔案;但工具依賴污染主模組。
3. **主 `go.mod` 釘語言與函式庫,`tools/go.mod` 釘工具,容器 image 釘 tag,foundry 以 `.env.example` 的 `FOUNDRY_TAG` 單一來源**。

## 決定

採選項 3。目前釘住的版本(升級時更新本表):

| 類別 | 項目 | 版本 | 來源 |
|---|---|---|---|
| 語言 | Go | `go 1.26.0`、`toolchain go1.26.8` | `go.mod`;CI `setup-go` 讀 `go.mod` |
| 工具 | oapi-codegen | v2.8.0 | `tools/go.mod`,`go tool -modfile=tools/go.mod oapi-codegen` |
| 工具 | sqlc | v1.31.1 | `tools/go.mod` |
| 工具 | golangci-lint | v2.13.2(v2 設定格式) | `tools/go.mod`;`make lint` / `make fmt` |
| 函式庫 | shopspring/decimal | v1.4.0 | `go.mod` |
| 函式庫 | pgx/v5、goose/v3、chi/v5 | v5.10.0、v3.28.0、v5.3.2 | `go.mod` |
| 函式庫 | oapi-codegen/runtime | v1.7.0 | `go.mod` |
| 函式庫 | cobra、caarlos0/env/v11、prometheus/client_golang | v1.10.2、v11.4.1、v1.24.1 | `go.mod` |
| 函式庫 | nats.go、go-redis/v9 | v1.53.1、v9.22.0 | `go.mod` |
| 測試 | testify、pgregory.net/rapid、testcontainers-go(+ modules/postgres) | v1.12.1、v1.3.0、v0.44.0 | `go.mod` |
| 容器 | postgres、redis、nats | `16.4`、`7.4-alpine`、`2.10.22-alpine` | `deploy/compose/compose.yaml` |
| 容器 | foundry(anvil / forge / cast) | `v1.8.1` | `.env.example` `FOUNDRY_TAG`;compose、`make contracts-test`、CI `foundry-toolchain` 都讀它 |
| 容器 | prometheus、grafana | `v2.54.1`、`11.2.0` | compose |
| 建置 | golang builder、distroless | `golang:1.26.8-bookworm`、`gcr.io/distroless/static-debian12:nonroot` | `build/Dockerfile` |
| 合約 | solc、evm_version | `0.8.28`、`cancun` | `infra/contracts/foundry.toml` |
| 未引入 | uuid、kin-openapi、go-ethereum、jwx、otel、forge-std | — | 到需要的 Phase 才加,各自獨立 PR |

決策細節:

- **Go 1.26**(偏離計畫的 1.23+):理由如背景;`toolchain` 釘住 patch 版,CI 與本機自動下載同一版。
- **`tools/go.mod`**:工具與主模組隔離;`make tools` 印出版本;`go tool -modfile=` 讓不需另裝任何 CLI。
- **golangci-lint 走 `go tool`**,CI 不用 `golangci-lint-action`,避免 action 與本機版本不一致。
- **foundry 不用 forge-std submodule**:`infra/contracts/script/Vm.sol` 宣告本專案用到的少數 cheatcode(簽章逐字抄自 foundry `Vm.sol`),讓 `git clone` 後不需 `submodule update` 就能 `forge build`;需要更多 cheatcode 時再加,或屆時改用 submodule。
- **容器 image 一律 tag**,不用 `latest`;foundry tag 只寫在 `.env.example` 一處。
- **產生碼進 repo**(`internal/api/gen`、`cmd/exchangectl/internal/apiclient`、`internal/registry/sqlcgen`),CI `gen-check` 比對,避免「產生器版本不同 → diff」在 review 時才發現。

## 後果

- 升級政策:任何一列的升級都是獨立 PR,PR 描述引用本 ADR 並更新表格;go-ethereum、foundry、JetStream 等行為敏感的升級要跑完整 integration + e2e。
- 依賴數量:主 `go.mod` 目前的間接依賴多半來自 testcontainers(測試用);Docker build context 以 `.dockerignore` 排除 `docs/ web/ deploy/ infra/ test/`。
- 已知代價:Go 1.26 為新版本,少數第三方工具(如舊版 golangci-lint)尚未跟上;以 `tools/go.mod` 釘住可用的版本解決。
