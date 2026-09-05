> **已由 [`docs/plan-v1.0.md`](../plan-v1.0.md) 取代(2026-09-05)。** 本檔為原始 v0.1 規劃書,僅供對照;審查意見見 [`docs/review/plan-review-2026-09.md`](../review/plan-review-2026-09.md)。

# 區塊鏈交易所平台 — 架構規劃書

> 版本:v0.1(規劃階段,尚未開發)
> 目標:單一 `docker compose up` 即可在本機完整啟動一套「學習/開發用」交易所系統,涵蓋撮合、帳本、錢包、鏈上整合。

---

## 1. 專案目標與範圍

- 以學習 Go 為主軸,建構一套具備真實交易所核心邏輯(下單、撮合、清算、充提款)的系統
- **不接真實主網、不碰真實資金**——鏈上互動一律使用本地測試鏈或公開測試網,避免安全與資金風險
- 架構採微服務拆分,但開發/學習階段全部塞進同一份 `docker-compose.yml`,方便單機啟動與除錯
- 模塊邊界設計成未來可以「原封不動搬到 K8s」,不用因為要上生產而整個重寫

## 2. 設計原則

1. **正確性優先於效能**:先確保帳本一致、撮合邏輯正確,再談延遲優化
2. **服務間用事件解耦**:撮合引擎只管撮合,成交後發事件出去,誰要訂閱誰處理
3. **金錢邏輯集中管理**:所有跟餘額有關的寫入只能經過 ledger-service,任何模塊都不能繞過去直接改資料庫
4. **鏈上服務隔離**:鏈上互動(簽名、廣播、監聽)獨立成服務,核心撮合/帳本完全不依賴鏈上節點是否在線

## 3. 系統架構總覽

```
┌─────────────────────────────────────────────┐
│              用戶端 / API 閘道層               │
│         api-gateway (REST + WebSocket)        │
└───────────────────────┬───────────────────────┘
                         │
┌───────────────────────▼───────────────────────┐
│                  核心交易服務層                  │
│  auth-service │ account-service │ risk-service  │
│  matching-engine │ ledger-service │ market-data  │
└───────────────────────┬───────────────────────┘
                         │  (NATS 事件匯流排)
┌───────────────────────▼───────────────────────┐
│                資金與鏈上服務層                   │
│  wallet-service │ deposit-listener              │
│  withdrawal-worker │ anvil(本地測試鏈)           │
└───────────────────────┬───────────────────────┘
                         │
┌───────────────────────▼───────────────────────┐
│                   支撐系統層                     │
│   notification-service │ admin-backoffice(選用) │
│   postgres │ redis │ prometheus+grafana(選用)   │
└─────────────────────────────────────────────┘
```

## 4. 模塊清單與技術選型

| 模塊 | 職責 | 技術選型 | Docker 服務名 |
|---|---|---|---|
| API 閘道 | REST/WebSocket 入口、JWT 驗證、限流 | Go + Echo/Gin | `api-gateway` |
| 帳號服務 | 註冊/登入/2FA、KYC 基本資料 | Go + PostgreSQL | `auth-service` |
| 帳戶服務 | 餘額查詢、下單凍結/解凍資金 | Go + PostgreSQL | `account-service` |
| 撮合引擎 | 訂單簿、price-time priority 撮合 | Go(單一 goroutine per market) | `matching-engine` |
| 帳本服務 | 複式記帳、交易結算 | Go + PostgreSQL(`decimal` 型別) | `ledger-service` |
| 行情服務 | K線、深度、成交即時推播 | Go + WebSocket + Redis pub/sub | `market-data-service` |
| 風控服務 | 異常交易偵測、限額、熔斷 | Go(先做簡單規則引擎) | `risk-service` |
| 錢包服務 | HD 錢包地址產生、簽名 | Go + go-ethereum | `wallet-service` |
| 充值監聽 | 監聽測試鏈事件,更新帳本 | Go + go-ethereum | `deposit-listener` |
| 提現處理 | 簽名、廣播交易、風控覆核 | Go + go-ethereum | `withdrawal-worker` |
| 本地測試鏈 | 模擬 Ethereum 節點,免費測試充提款 | Foundry `anvil` 或 Ganache | `anvil` |
| 通知服務 | 成交/風控通知(先用 log 模擬,不接真簡訊) | Go | `notification-service` |
| 管理後台 | 用戶管理、對帳報表(後期選用) | 簡單 Web UI | `admin-backoffice` |
| 事件匯流排 | 服務間非同步通訊 | NATS(比 Kafka 輕量,學習階段夠用) | `nats` |
| 資料庫 | 帳本/用戶/訂單資料 | PostgreSQL | `postgres` |
| 快取 | Session、行情快取 | Redis | `redis` |
| 監控(選用) | 指標與告警 | Prometheus + Grafana | `prometheus` / `grafana` |

> 選 NATS 而非 Kafka:功能對這個規模已經夠用,docker-compose 啟動快、設定簡單,學習成本低很多,之後真的需要 Kafka 的吞吐量再換也不遲。

## 5. Docker Compose 服務骨架(草案)

> 這只是服務骨架示意,實際 `build` context 與程式邏輯留到開發階段再填。

```yaml
version: "3.9"

services:
  postgres:
    image: postgres:16
    environment:
      POSTGRES_DB: exchange
      POSTGRES_USER: exchange
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
    volumes:
      - pg-data:/var/lib/postgresql/data
    networks: [exchange-net]

  redis:
    image: redis:7
    networks: [exchange-net]

  nats:
    image: nats:2
    networks: [exchange-net]

  anvil:
    image: ghcr.io/foundry-rs/foundry:latest
    command: ["anvil", "--host", "0.0.0.0"]
    ports: ["8545:8545"]
    networks: [exchange-net]

  api-gateway:
    build: ./services/api-gateway
    ports: ["8080:8080"]
    depends_on: [auth-service, account-service]
    networks: [exchange-net]

  auth-service:
    build: ./services/auth-service
    depends_on: [postgres]
    networks: [exchange-net]

  account-service:
    build: ./services/account-service
    depends_on: [postgres, nats]
    networks: [exchange-net]

  matching-engine:
    build: ./services/matching-engine
    depends_on: [nats]
    networks: [exchange-net]

  ledger-service:
    build: ./services/ledger-service
    depends_on: [postgres, nats]
    networks: [exchange-net]

  market-data-service:
    build: ./services/market-data-service
    depends_on: [redis, nats]
    networks: [exchange-net]

  risk-service:
    build: ./services/risk-service
    depends_on: [nats]
    networks: [exchange-net]

  wallet-service:
    build: ./services/wallet-service
    depends_on: [anvil, postgres]
    networks: [exchange-net]

  deposit-listener:
    build: ./services/deposit-listener
    depends_on: [anvil, nats]
    networks: [exchange-net]

  withdrawal-worker:
    build: ./services/withdrawal-worker
    depends_on: [anvil, nats, risk-service]
    networks: [exchange-net]

  notification-service:
    build: ./services/notification-service
    depends_on: [nats]
    networks: [exchange-net]

networks:
  exchange-net:

volumes:
  pg-data:
```

## 6. 資料流範例:一筆下單的完整路徑

1. Client 送出下單請求 → `api-gateway`(驗證 JWT)
2. `api-gateway` 轉發到 `account-service`,凍結對應資金
3. 凍結成功 → 訂單送進 `matching-engine`
4. 撮合成功 → 發布「成交事件」到 `nats`
5. `ledger-service` 訂閱事件,寫入複式帳本、解凍資金
6. `market-data-service` 訂閱事件,推播最新深度/成交價到 WebSocket 客戶端
7. `notification-service` 訂閱事件,記錄成交通知(學習階段先 log,不接真簡訊)

充值/提現流程類似,但主角換成 `deposit-listener` / `withdrawal-worker`,透過 `anvil` 本地測試鏈收發交易,不碰真實資金。

## 7. 分階段建置順序

| 階段 | 內容 | 對應先前學習路線 |
|---|---|---|
| Phase 0 | 基礎設施:postgres、redis、nats、anvil 先跑起來 | 環境準備 |
| Phase 1 | api-gateway + auth-service + account-service | 階段 3(Web 服務與資料庫) |
| Phase 2 | matching-engine + ledger-service | 階段 4(撮合引擎) |
| Phase 3 | market-data-service(WebSocket 即時行情) | 階段 4 延伸 |
| Phase 4 | wallet-service + deposit-listener + withdrawal-worker | 階段 5(區塊鏈整合) |
| Phase 5 | risk-service + notification-service | 階段 6 |
| Phase 6(選用) | admin-backoffice、prometheus/grafana | 階段 7 |

## 8. 建議目錄結構

```
exchange-platform/
├── docker-compose.yml
├── .env.example
├── services/
│   ├── api-gateway/
│   ├── auth-service/
│   ├── account-service/
│   ├── matching-engine/
│   ├── ledger-service/
│   ├── market-data-service/
│   ├── risk-service/
│   ├── wallet-service/
│   ├── deposit-listener/
│   ├── withdrawal-worker/
│   └── notification-service/
├── shared/              # 共用 proto 定義 / DTO / 工具函式
├── infra/
│   └── postgres/init.sql
└── docs/
    └── architecture.md  # 本文件
```

## 9. 誠實面對的限制(避免誤導自己)

- 這是**學習/開發環境**規格,不是生產部署架構。單機 docker-compose 沒有高可用性,一台機器掛了全部停擺
- `matching-engine` 在單機上是單一 process,沒有做到真正的水平擴展或容錯,適合學習撮合邏輯,不適合承受真實高併發
- 私鑰簽名流程在這裡是**教學用途**,生產環境的私鑰管理需要 HSM 或 MPC 方案,絕不會用這種方式存放
- 沒有做真正的安全審計、滲透測試、法遵(KYC/AML)整合,這些都是拿真實資金上線前的必要條件

## 10. 下一步

確認模塊清單與技術選型沒問題後,再回來依 Phase 0 → Phase 6 的順序,一個服務一個服務動手實作。
