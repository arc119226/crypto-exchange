# ADR-0000:需求訪談決策記錄(2026-09-05)

- 狀態:已採納(Accepted)
- 日期:2026-09-05
- 相關文件:[`docs/plan-v1.0.md`](../plan-v1.0.md)、[`docs/review/plan-review-2026-09.md`](../review/plan-review-2026-09.md)、[`docs/archive/plan-v0.1.md`](../archive/plan-v0.1.md)

## 背景

v0.1 規劃書以「學習 Go 用交易所」為定位。審查(9 個鏡頭、對抗式驗證)指出資金模型、事件保證、服務邊界與產品定位都需重做。為了讓 v1.0 計畫有明確依據,對作者進行 8 輪 32 題的需求訪談,以下為全部決策。後續 ADR-0001 起(架構形態、唯一真相、租戶、數值、帳本、認證、簽名、工具鏈版本)由作者在 Phase 0 依 `docs/plan-v1.0.md` 第 20 節撰寫,細節以本檔為準。

## 決策


### 第 1 輪:背景與目標
| 題目 | 回答 |
|---|---|
| 專案目的 | **未來可能商業化的原型**(不是純學習) |
| Go 程度 | **初學**:寫過小工具,goroutine/channel/context/module 不熟 |
| 每週投入 | **全職(30h+)** |
| 既有背景 | 後端/微服務(其他語言)、區塊鏈/Solidity/web3、Docker/K8s/DevOps。**缺:交易系統領域知識**(撮合、複式記帳、清算、風控) |

### 第 2 輪:交易功能範圍
| 題目 | 回答 |
|---|---|
| 商業形態 | **白牌 / 交易引擎授權**:賣撮合 + 帳本 + 錢包模組給別人架交易所;重點在 API 設計、模組化、未來多租戶 |
| 交易功能 | **現貨限價單 + 市價單 + 取消**(含部分成交、手續費) |
| 資產範圍 | **ETH + 1~2 個 ERC-20 測試代幣**(自行部署模擬 USDC,交易對 ETH/USDC) |
| 規模假設 | **封閉 beta,數百至數千用戶**:需要持久化事件流、撮合引擎自動恢復、基本監控;不追求極低延遲 |

### 第 3 輪:時程、介面、品質、API
| 題目 | 回答 |
|---|---|
| 時程 | **沒有硬期限,以做對為主** |
| 介面需求 | **管理後台 UI**(市場/資產設定、提現審核、對帳)+ **最小參考交易前台** + **CLI/腳本 demo 工具** |
| 品質基線 | **全選**:單元測試 + 撮合屬性/模糊測試、整合測試(testcontainers)、E2E(compose 全起)、CI(GitHub Actions) |
| API 規格 | **OpenAPI 契約先行 + oapi-codegen 產生骨架** |

### 第 4 輪:鏈上與資金模型
| 題目 | 回答 |
|---|---|
| 鏈環境 | **anvil 為主 + Sepolia 後期驗證**(日常開發/CI 用 anvil;鏈上 Phase 尾端用 Sepolia 驗證確認數、reorg、gas) |
| 託管模型 | **每用戶 HD 充值地址 + 歸集(sweep)到熱錢包,提現從熱錢包出**(BIP-44、ERC-20 歸集需先打 gas、帳本需「鏈上庫存」科目) |
| 提現審核 | **限額內自動放行,超額進後台人工審核**(單筆/日累計限額可設定;狀態機 pending_review → approved/rejected → broadcasting → confirmed/failed) |
| 鍵管理 | **簽名隔離在 signer 模塊,加密 keystore + 介面預留 KMS/HSM adapter** |

### 第 5 輪:產品邊界與演進
| 題目 | 回答 |
|---|---|
| 多租戶 | **資料模型預留 tenant_id,第一版單租戶**(核心表帶 tenant_id、帳本科目依租戶隔離,不做隔離邏輯) |
| Auth/KYC 邊界 | **引擎提供最小 auth(用戶目錄 + JWT + API key),KYC 狀態由客戶系統透過 API/webhook 寫入**;引擎不做身分驗證本身 |
| 部署路徑 | **compose 為開發/E2E 環境,最後一個 Phase 補 Helm chart**;服務遵守 12-factor(env 注入設定、無本地狀態、health endpoint),用 kind/k3d 驗證 |
| 語言約定 | **計畫/設計文件繁中;程式碼/註解/commit/API 文件英文** |

### 第 6 輪:架構取捨(依初步審查結果拍板)
| 題目 | 回答 |
|---|---|
| 服務拆分 | **模組化單體:單一 go.mod、單一 binary 多角色**(`exchange serve --role=all|api|engine|chain|stream`);internal/{matching,ledger,trading,marketdata,chain,auth,admin} 以 package 邊界維持模組邊界;compose 可一角色一容器 |
| 一致性模型 | **交易性核心 + outbox**:引擎純函式 Apply(cmd)→events;同一 Postgres 交易內寫訂單狀態、成交、帳本分錄(hold→settle)、outbox;relay 送 NATS JetStream 給行情/通知/WS;Postgres 為真相,引擎重啟從 open orders 重建;不需 saga |
| 數值規範 | **Decimal + 每資產 scale**:shopspring/decimal 封裝 money 套件(禁 float64)、NUMERIC(36,18)、JSON 字串;assets.scale、markets.price_tick/qty_step;鏈上邊界做 wei↔decimal |
| 階段順序 | **領域建模 + walking skeleton → 撮合純 library → 帳本 → trading/API/auth → 鏈上 → …**(由內而外) |
| 部署(聊天中決定) | **不用 Docker Swarm**。開發/E2E 用 compose;自家封閉 beta 用單台 VM + compose(prod profile)或單節點 k3s;Helm chart 在最後 Phase 以 kind/k3d 驗證;有第二位營運人員或第一個付費客戶再上託管 K8s |

### 第 7 輪:UI、整合面、admin 安全、交付形態
| 題目 | 回答 |
|---|---|
| UI 技術 | **後台 Go html/template + htmx(嵌進同一 binary,無 Node);參考前台 React/Vite SPA(web/ 目錄)** |
| 整合面 | **Webhook 出站 + WebSocket**:客戶在後台註冊 endpoint;order/trade/deposit/withdrawal 事件 HMAC 簽名 + 重試 + 投遞記錄;事件 catalog 為對外契約;notification-service 取消 |
| Admin 2FA | **admin 角色強制 TOTP(RFC 6238)**;一般用戶 2FA 交給客戶系統 |
| 交付形態 | **可部署服務為交付物**(container image + Helm chart + OpenAPI/事件契約 + 後台);核心放 internal/,客戶只透過 API 整合 |

### 第 8 輪:交易領域規則(補充審查提出的必答題)
| 題目 | 回答 |
|---|---|
| 手續費 | **每市場 maker/taker bps,以「收到的資產」扣費**(買方扣 base、賣方扣 quote;捨入對交易所有利);schema 預留 fee_schedule_id |
| 市價單 | **市價買以 quote 金額、市價賣以 base 數量下單;剩餘 IOC 取消**(凍結金額確定) |
| 自成交 | **STP = cancel-newest(取消 taker 剩餘量)** |
| 帳戶模型 | **每用戶一個現貨帳戶,引擎一律以 account_id 為鍵**(users 與 accounts 分表) |

### 審查員提出、由我依預設值決定(未另外詢問)
- 市場/資產設定模型:**DB 為真相、後台可編輯;新增市場走受控重啟**,引擎支援 reload 命令(預留熱載入)。
- 撮合引擎恢復語意:隨「交易性核心 + Postgres 為真相」自然得到 **完整恢復**(重啟後從 open orders 重建,用戶無感)。
- 事件流技術:**NATS JetStream**,存取包在 internal/eventbus 介面後(客戶日後可換 Kafka)。
- K 線:v1 由 stream 角色從成交事件聚合 1m/5m/1h 存 Postgres;Redis 只放深度快照與限流計數。

## 後果

- v1.0 計畫的每一節都必須與上述決策一致;若日後推翻任一決策,需新增 ADR 說明並升版計畫。
- 「依預設值決定」的四項為主代理在訪談外依審查建議選定的預設值,作者可在 Phase 0 以新 ADR 覆蓋。
