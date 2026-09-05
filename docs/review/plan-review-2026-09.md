# 架構規劃書 v0.1 審查報告

## 審查方法

兩組審查工作流共 9 個鏡頭:一般鏡頭 6 位(架構、撮合/帳本、鏈上、安全、DevOps、需求完整性)加去重與完整性批評者(id 以 C 開頭);補充鏡頭 3 位(產品/白牌引擎、演進路徑、學習路徑,id 以 P/E/L 開頭)。每個 finding 都經「事實正確性、比例原則、可落地性」三個獨立鏡頭對抗式驗證,並以訪談定案的真實定位(白牌交易引擎商業原型、Go 初學但有後端/web3/DevOps 經驗、封閉 beta、以做對為主)重新衡量嚴重度與建議。統計:原始 95 個 finding(一般 68 + 補充 27)→ 去重/合併後 69 個(37 去重 + 5 批評者 + 27)→ 成立 68、駁回 1。本報告再把兩組高度重疊的 finding 合併為 28 條。

## 總評

v0.1 的設計原則、技術選型與「不碰真錢」的底線都是對的,但文件停留在「服務清單 + compose 骨架」的層次,沒有領域模型、資金模型與事件保證,照著做會在 Phase 2 就撞上必須重寫的資金正確性問題。最大的三個結構性問題:(1) **資金模型缺席**——餘額有兩個寫入者、訂單沒有 owner、撮合引擎無恢復、資金事件走無持久化的 NATS core,四者疊加是一個系統性的正確性缺口;(2) **拆分先於邊界驗證**——在邊界已被證明錯誤的情況下固化成 11 個網路服務,Phase 順序又把凍結邏輯排在帳本之前;(3) **定位錯位**——文件以學習專案寫成,而真實定位是白牌商業原型,產品邊界、契約、對帳、管理後台、監控全被標為「選用」或不存在。訪談已推翻第 1 節定位,且第 3~8 節幾乎每節都要改;68 個成立的 finding 中約三分之二已被作者的決策覆蓋,但沒有一條寫在文件裡。修補 v0.1 只會留下一份與決策不符的文件,建議動工前直接以 v1.0 取代。

## 做得好的地方

1. **範圍底線誠實**:「不接真實主網、不碰真實資金」並在 compose 直接落地 anvil;第 9 節逐條承認無 HA、單 process 撮合、私鑰僅教學用、無法遵。這是最常被忽略的安全界線。
2. **四條設計原則正是白牌引擎需要的性格**:正確性優先、事件解耦、餘額只經 ledger、鏈上隔離。問題只在第 4、6 節沒有貫徹,原則本身應完整保留為 v1.0 的不變量。
3. **「單一 goroutine per market」是正確的決定性撮合模型**:天然免鎖、可重放、可做屬性/模糊測試,是自動恢復的基礎,不需要為了恢復改演算法。
4. **技術選型務實且有取捨理由**:NATS 升級 JetStream 即可滿足持久化事件流;anvil 的 anvil_mine / evm_snapshot / anvil_reorg 是本機測試充提款狀態機的最佳工具;Postgres decimal 顯示對金額精度有意識。這些選擇日後都不必推翻。
5. **鏈上層拆 wallet / deposit-listener / withdrawal-worker** 與業界拆法一致,讓鏈節點離線不影響撮合與帳本。
6. **文件可被審查**:第 6 節七步端到端路徑、compose 骨架與目錄結構讓設計矛盾能被具體指出;只對外開 gateway 與 anvil port、預留 .env.example,顯示「gateway 唯一入口」與「密鑰不入庫」的意識。

## 必須修正(critical)

### 1. 餘額有兩個寫入者,凍結不是帳本操作(F01 / L01 / E03)
- 章節:第 2 節原則 3、第 4 節 account-service / ledger-service、第 6 節步驟 2 與 5、第 7 節 Phase 1/2
- 問題:凍結(available→hold)本身就是餘額寫入,計畫卻讓 account-service 凍結、ledger-service 解凍,兩者只靠 NATS 非同步對齊。不論共用 balances 表或各持副本,都無法在單一交易內維持「available + hold = 帳本餘額」的不變量;暗示的心智模型是「UPDATE balance」而非由分錄推導餘額。Phase 1 先做凍結、Phase 2 才出現帳本真相,必然返工。
- 證據:「所有跟餘額有關的寫入只能經過 ledger-service」vs「帳戶服務 | 餘額查詢、下單凍結/解凍資金」、「api-gateway 轉發到 account-service,凍結對應資金」。
- 建議:account-service 併入 ledger;每個 account × asset 有 available 與 hold 兩個科目,Hold / Release / Settle / Credit / Debit 為 ledger 對外唯一的原子命令,同一 DB 交易內檢查足額並以 order_id / trade_id / withdrawal_id 作 UNIQUE 冪等鍵;trading、chain 只能對 ledger 發命令。原則 3 改寫為「ledger 是唯一擁有 accounts/entries 表的模組,凍結亦為帳本操作」,並把「∀asset: Σavailable + Σhold = Σ分錄借貸」寫成 Phase 1 屬性測試驗收。
- 狀態:已由決策「帳本:複式記帳、available/hold 科目、account-service 併入 ledger」處理;命令介面與不變量需寫進 v1.0。

### 2. 資金路徑事件走無持久化的 NATS core,無 outbox、無冪等鍵、無事件契約(F02 / E02 / P08)
- 章節:第 2 節原則 2、第 4 節事件匯流排、第 5 節 nats、第 6 節步驟 4–7
- 問題:官方 nats image 預設不啟用 JetStream,core NATS 為 at-most-once;成交→記帳、充值→入帳都放在這條通道上,ledger 任何一次重啟就漏記,重送則重複記帳。事件沒有 event_id / seq / schema 版本,消費者無去重;對白牌客戶而言事件 schema 就是整合介面,ad-hoc JSON 每次升級都會打壞客戶整合。
- 證據:`nats: image: nats:2`(無 `-js`、無 volume);「撮合成功 → 發布『成交事件』到 nats」「ledger-service 訂閱事件,寫入複式帳本」;全文無 ack、durable、冪等、outbox 字眼。
- 建議:影響資金的事件(OrderAccepted / Trade / OrderCancelled / DepositConfirmed / Withdrawal.*)採 transactional outbox,與訂單狀態、成交、分錄同一 Postgres 交易落庫,relay 送 JetStream(包在 internal/eventbus 介面後);消費端 durable consumer + 顯式 ack,以 UNIQUE(trade_id)、(chain_id, tx_hash, log_index)、processed_events 表冪等。統一信封 `{event_id, type, version, occurred_at, seq, correlation_id, tenant_id, payload}`,事件 catalog 放 api/events/<type>.v1.json,相容規則「只加欄位不改語意,否則 version+1」並以 golden 檔測試鎖住。compose 的 nats 改 `nats:2-alpine` + `-js -sd /data -m 8222` + volume。Phase 2 驗收:「kill ledger 再啟動,所有成交恰好記帳一次;同一事件投遞兩次餘額不變」。
- 狀態:已由決策「交易性核心 + outbox + JetStream、事件 catalog 為對外契約」處理;stream / consumer 命名、信封欄位、冪等鍵與 catalog 清單需寫進 v1.0。

### 3. 撮合引擎無持久化、無序號、無恢復,SoT 未宣告(F04 / E01 / L04)
- 章節:第 4 節 matching-engine、第 5 節 compose、第 6 節步驟 3、第 9 節
- 問題:引擎純記憶體、無 DB 依賴,重啟後訂單簿消失但 held 仍鎖在帳本;沒有 seq 讓下游排序與去重;引擎記憶體、Postgres 訂單表、market-data 深度三個「真相」沒有宣告誰是 SoT;送單協定也未定。若寫成「NATS callback 收單、直接 publish」的 goroutine,既不能純 go test,也無法確定性重放,補恢復等於重寫介面。
- 證據:「撮合引擎 | … | Go(單一 goroutine per market)」、`matching-engine: depends_on: [nats]`、第 9 節僅「沒有做到真正的水平擴展或容錯」。
- 建議:引擎核心為純函式 `Apply(cmd) []Event`,無 I/O、不呼叫 time.Now()/rand,時間戳與 seq 由命令攜帶;每市場單調遞增 seq,NewOrder / Cancel 先以 seq 落命令日誌(與 outbox 同交易)再套用記憶體;所有事件帶 (market, seq, event_id)。Postgres 為 SoT,啟動時載入 OPEN / PARTIALLY_FILLED 訂單依 (price, seq) 重建,再從最後已處理 seq 重放;快照後期再加。送單為同步 in-process 呼叫回傳 accept / reject。刪除「重啟即清空」退路。Phase 2 驗收:「隨機下單/取消中 kill -9 引擎再啟動,orderbook 與 ledger held 完全一致,事件不漏不重」。
- 狀態:已由決策「引擎純函式、Postgres 為真相、從 open orders 重建」處理;seq / 命令日誌 / graceful shutdown flush / 驗收條件需寫進 v1.0。

### 4. 沒有模組擁有「訂單」與其狀態機,gateway 被迫做 saga(F03 / E03)
- 章節:第 4 節模塊清單、第 6 節步驟 2–3
- 問題:模塊表中沒有任何服務負責訂單的建立、儲存、狀態與取消;第 6 節只有 happy path,「先凍結、成功再送撮合」的編排落在 gateway。凍結後送撮合失敗、引擎拒單、部分成交剩餘、取消,都沒有人 Release,資金會永久凍結。v1 明確包含取消與部分成交,沒有狀態機無法定義驗收。
- 證據:「API 閘道 | REST/WebSocket 入口、JWT 驗證、限流」卻執行「凍結成功 → 訂單送進 matching-engine」;資料庫職責寫「帳本/用戶/訂單資料」但無 owner。
- 建議:新增 trading 模組擁有 orders / trades 表與狀態機 PENDING_HOLD → OPEN → PARTIALLY_FILLED → FILLED / CANCELLED,加 REJECTED 分支;每個轉移標明觸發者與帳本動作(Hold / Release / Settle)。負責參數驗證(tick / step / min_notional / 市場狀態)→ policy 檢查 → ledger.Hold → 送撮合 → 拒單 Release → 終態 Release 剩餘。第 6 節補取消、拒單、部分成交、市價單四條非 happy path;gateway 退回純入口。狀態 enum 直接進 OpenAPI Order schema。加定時 reaper 處理卡在 PENDING 的訂單。
- 狀態:需寫進 v1.0 計畫(階段方向已含 trading 模組,但狀態機與失敗路徑未定義)。

### 5. 提現缺少持久化狀態機、資金安全順序與簽名隔離(F05 / P06 / F21)
- 章節:第 4 節 withdrawal-worker / wallet-service、第 5 節 compose、第 6 節末段
- 問題:職責順序寫成「簽名 → 廣播 → 風控覆核」,覆核者與執行者同 process;worker 無 DB 依賴,無法持久化狀態,廣播後崩潰重啟就會雙重出金,或帳本已扣但從未廣播。「簽名」同時落在兩個服務,等於兩處持有私鑰。提現政策(自動放行門檻、誰能核准)是客戶的合規決定,不能寫死在規則引擎。
- 證據:「提現處理 | 簽名、廣播交易、風控覆核」、「錢包服務 | HD 錢包地址產生、簽名」、`withdrawal-worker: depends_on: [anvil, nats, risk-service]`、「充值/提現流程類似」。
- 建議:狀態機 REQUESTED → POLICY_CHECK → AUTO_APPROVED | PENDING_REVIEW → APPROVED → FUNDS_LOCKED(ledger.Hold,withdrawal_id 冪等)→ SIGNED(nonce 已落 DB)→ BROADCAST(tx_hash 已記錄)→ CONFIRMED → SETTLED;失敗分支 REJECTED、BROADCAST_FAILED(確認 nonce 未上鏈才 Release)、FAILED_ON_CHAIN(gas 入 custody:gas,凍結保留、進人工審核)。鐵律:「帳本未先凍結絕不簽名」「每個狀態落庫後才做下一步,重啟從狀態續跑」。簽名只在 cmd/signer 獨立 process:`Sign(withdrawal_id, unsignedTx)` 只簽 DB 中 FUNDS_LOCKED 且金額/地址一致的請求,重複 id 拒簽,每次簽名寫審計;Signer interface 第一版 keystore、預留 KMS adapter。政策(per-asset 門檻、日限額、是否雙人)存 registry 由後台設定。
- 狀態:已由決策「提現狀態機、先鎖資金再簽名再廣播、signer 模組、限額內自動/超額人工」處理;完整狀態列表、失敗分支與 Sign API 政策檢查需寫進 v1.0。

### 6. 引擎與「客戶自建部分」的產品邊界沒有畫出來(P01 / F33)
- 章節:第 3 節架構圖、第 4 節 auth-service / notification-service、第 6 節步驟 1
- 問題:auth / KYC / 2FA / 通知被放在核心層,等於逼客戶「用你的帳號系統,或拆掉你的核心」;第 6 節把 JWT 驗證寫進下單流程,讓撮合/帳本 API 與特定身分模型耦合;「KYC 基本資料」又與第 9 節「沒有做 KYC/AML」矛盾。這些功能對引擎競爭力零貢獻,卻會吸走單人的時間。
- 證據:「帳號服務 | 註冊/登入/2FA、KYC 基本資料」列在「核心交易服務層」;「notification-service 訂閱事件,記錄成交通知」。
- 建議:新增「產品邊界」一節分三類——(A) 引擎交付物:matching、ledger、trading、market-data、chain/signer、registry、admin API、對帳/審計、webhook;(B) 參考實作(可整包替換、不在支援承諾內):最小 auth(用戶目錄 + JWT + API key)、參考前台、CLI;(C) 明確不做:KYC 文件蒐集、簡訊/郵件通知、客服。引擎內部 API 與事件只認 account_id;users 表只留 kyc_status 由客戶系統經 API/webhook 寫入;notification-service 改為 webhook-dispatcher(HMAC 簽章、指數退避、投遞記錄、dead-letter)。
- 狀態:已由決策「產品邊界、最小 auth、KYC 由客戶寫入、notification 取消、Webhook 出站」處理;需寫進 v1.0 並改寫第 1、3、6 節。

### 7. 數值規範未定:Go 端型別、各資產 scale、捨入方向、tick/step(F13 / L02)
- 章節:第 4 節 ledger-service、第 8 節 shared/
- 問題:只寫 Postgres decimal;Go 端若用 int64,10 ETH 的 wei(1e19)即溢位,float64 破壞守恆;撮合、帳本、錢包三模組各自選型會在 DTO 邊界截斷精度。手續費與 quote 金額的捨入方向不統一會破壞「每幣別加總為零」。一旦進 schema 與 OpenAPI 就要全面遷移。
- 證據:「帳本服務 | 複式記帳、交易結算 | Go + PostgreSQL(decimal 型別)」;全文無 Go 端金額型別、scale、捨入、tick/lot。
- 建議:新增「數值規範」小節:internal/money 封裝 shopspring/decimal,golangci-lint forbidigo 在 money / ledger / trading / matching / chain 禁止 float32/64 與 ParseFloat;DB NUMERIC(36,18);OpenAPI 金額一律 `type: string` + pattern,oapi-codegen 以 x-go-type 對應。assets.scale(ETH=18、USDC=6),markets.price_tick / qty_step / min_notional;價格須為 tick 整數倍否則拒單、數量截斷至 qty_step;quote 金額以 quote scale 截斷、手續費對交易所有利向上取整、餘數入 exchange:rounding 科目。wei↔decimal 轉換集中在 chain 模組單一函式並有 round-trip 測試。
- 狀態:已由決策「Decimal + 每資產 scale、捨入對交易所有利」處理;lint 規則、tick/step 驗證與 rounding 科目需寫進 v1.0。L02 建議的「最小單位整數 big.Int」為等價替代,作者選 decimal 可行,關鍵是全專案統一。

## 建議修正(major)

### 8. 11 個獨立服務把「模組邊界」與「部署單位」綁死,Go module 佈局未定(F07 / E06 / L05 / P07 / F16)
- 章節:第 1、4、5、8 節
- 問題:每個服務重複 main / config / DB / NATS / healthz / Dockerfile / CI,而 F01、F03、F06 已證明邊界是錯的,把未驗證的邊界固化成網路邊界是最貴的修正方式;`build: ./services/<name>` 的 context 根本看不到 shared/;11 個 go.mod 或一個大雜燴 shared 都會卡在 module 地獄。白牌客戶要的是「一個 image + 一份設定」。
- 證據:「架構採微服務拆分」、11 個 `build: ./services/*`、「shared/ # 共用 proto 定義 / DTO / 工具函式」。
- 建議:單一 go.mod;`cmd/exchange`(`serve --role=gateway,trading,ledger,chain,marketdata,webhook|all`)、`cmd/signer`(獨立 process,安全邊界)、`cmd/exchange-cli`;`internal/{auth,ledger,trading,matching,marketdata,risk,chain,signer,webhook}` 各有明確 Go interface,`internal/platform`(config、slog、pgx、nats、metrics)、`internal/contract`(事件 schema、money、ID 型別);depguard 鎖住跨模組 import。刪 shared/ 與 proto;共用一份 multi-stage Dockerfile 加 CMD build arg;`deploy/compose/`、`deploy/helm/`、`migrations/<module>/`、`infra/anvil/`、`web/admin/`、`web/trade/`。compose 初期:postgres / redis / nats / anvil / exchange / signer / migrate。
- 狀態:已由決策「模組化單體、單一 go.mod、單一 binary 多角色、核心放 internal/」處理;目錄結構、role 清單與 signer 獨立 process 需寫進 v1.0。

### 9. Phase 順序倒置:凍結排在帳本之前、缺純 library 里程碑、Phase 4 依賴 Phase 5(F09 / L03 / E10)
- 章節:第 7 節、第 5 節 withdrawal-worker
- 問題:Phase 1 先做 gateway + auth + account-service,在撮合命令模型與帳本未存在時就把凍結介面定死;Phase 2 同時做撮合 + 帳本 + NATS,除錯時分不清是演算法錯還是事件丟失;Phase 0 就起 anvil 但 Phase 4 才用;Phase 4 的 compose 因 depends_on 尚不存在的 risk-service 直接失敗。
- 證據:「Phase 1 | api-gateway + auth-service + account-service」「Phase 2 | matching-engine + ledger-service」、`withdrawal-worker: depends_on: [..., risk-service]` 而 risk 在 Phase 5。
- 建議:由內而外——Phase 0 walking skeleton(repo 骨架、OpenAPI 草稿、CI、testcontainers、compose 基礎設施、mock USDC 部署腳本、slog/healthz/metrics);Phase 1 ledger 核心(科目、Hold/Release/Settle/Credit、admin 入帳端點、不變量測試)+ 最小 auth;Phase 2a matching 純 package + 屬性/模糊測試 + CLI replay;Phase 2b trading 狀態機 + 命令日誌 + outbox + 恢復 + pre-trade policy 介面(allow-all);Phase 3 JetStream relay + market-data + WS + 參考前台 + CLI demo;Phase 4 chain(HD、deposit、sweep、withdrawal、signer)先 anvil 後 Sepolia;Phase 5 管理後台(registry、提現審核、對帳)+ webhook + 事後風控;Phase 6 儀表板打磨 + Helm。
- 狀態:已由決策「階段方向」處理;需寫進 v1.0 並修正 depends_on。

### 10. 每個 Phase 沒有完成定義,測試策略(含鏈上測試方法)未落到計畫(F10 / L08 / F11 / C04)
- 章節:第 7 節、第 10 節、第 2 節原則 1
- 問題:表格只有服務名,「做完 matching-engine」無法判定;Phase 1~3 沒有合法的資金來源(充值在 Phase 4);全文無測試、CI 字眼,原則 1 無驗證手段。鏈上路徑(確認數、reorg、卡單重送)在 anvil 預設 automine 下完全不可見,計畫沒把 anvil 當可程式化的測試工具。
- 證據:「Phase 2 | matching-engine + ledger-service」、「一個服務一個服務動手實作」、anvil `command: ["anvil", "--host", "0.0.0.0"]`。
- 建議:第 7 節改成每 Phase 一小節:範圍 / 完成定義(`make test` / `make e2e` 可自動驗證)/ 展示腳本 / 非目標。範例 DoD——Phase 1:隨機轉帳/凍結/釋放 fuzz 後每 asset postings 加總為零;Phase 2:兩帳戶互下限價/市價成交、部分成交、取消、收費;不變量 best bid < best ask、同價位 FIFO、成交價為被動方價格、重放後 book 一致;kill -9 引擎後一致;Phase 4:anvil_mine N 驗證確認數、anvil_reorg 驗證未入帳、evm_setAutomine false 驗證卡單重送、worker 重啟不重複廣播。Phase 1 加「管理員入帳/調帳」端點(走 ledger 正式分錄、對手科目 exchange:adjustment、需 reason 與審計),開發當 faucet、正式即對帳調整功能。CI 第一個 commit 即有 go vet / golangci-lint / go test -race / oapi-codegen 同步檢查 / compose config。
- 狀態:已由決策「品質基線、每 Phase 有 DoD」處理;具體 DoD、不變量清單與 anvil 測試策略需寫進 v1.0。

### 11. 科目表、分錄樣板、手續費分錄與對帳公式未設計(F12 / L01 / P05)
- 章節:第 4 節帳本服務、第 6 節步驟 5
- 問題:「複式記帳」只有四個字,沒有科目就不知道一筆成交寫幾條 posting、手續費進哪裡、鏈上資產對應哪個內部科目;作者自述無複式記帳知識,臨場決定幾乎必然做成可 UPDATE 的單欄餘額,且無法產生對帳報表。
- 證據:「帳本服務 | 複式記帳、交易結算」;全文無「手續費」「科目」「分錄」「借貸」。
- 建議:明列科目表(帶 tenant_id):user:{account}:{asset}:available / hold、exchange:fee:{asset}、exchange:adjustment:{asset}、exchange:rounding:{asset}、custody:deposit_addresses:{asset}、custody:hot:{asset}、custody:gas:{asset}、external:{asset}。journal_entry = 一組 postings,每幣別加總為零(應用層 assert + DB trigger),postings 只 INSERT。標準分錄:成交(買方 hold quote → 賣方 available quote;賣方 hold base → 買方 available base;各方以收到資產扣費入 exchange:fee;限價買優於限價成交時價差立即 Release)、充值(external → user available,同時 external → custody:deposit_addresses)、歸集(deposit_addresses → hot,gas 入 custody:gas)、提現(hold → external)。手續費由 ledger 在 Settle 時依 markets 的 maker/taker bps 計算。對帳公式:Σ用戶(available+hold)+ fee = Σ受控地址鏈上餘額 − 在途提現。這些樣板即 Phase 1/2 單元測試。
- 狀態:已由決策「帳本、手續費」處理;科目表、分錄樣板與對帳公式需寫進 v1.0。

### 12. v1 功能範圍與 assets / markets / fee_schedules registry 未寫入(F14 / P02)
- 章節:第 1、4、7 節
- 問題:全文沒有任何交易對、資產、訂單類型、手續費、精度或市場狀態;「per market」暗示多市場卻無 market 概念;anvil 上只有 ETH,沒有 mock 代幣就沒有交易對,這在 Phase 2 就會被逼出來。管理後台要能設定市場/資產,參數若寫死在 code 或 env,每個客戶部署都要改 code。
- 證據:「撮合引擎 | 訂單簿、price-time priority 撮合」;infra 只有 `postgres/init.sql`。
- 建議:新增「v1 功能範圍」規格表:ETH/USDC(mock USDC 6 decimals,部署腳本放 infra/anvil/,Phase 0 執行並把地址寫入 assets 表)、限價 GTC + 市價 + 取消、部分成交、手續費、單一現貨帳戶(users / accounts 分表,引擎以 account_id 為鍵)。Phase 1 建 assets(tenant_id, symbol, scale, chain_id, contract_address nullable, is_native, required_confirmations, min_withdraw, deposit_enabled, withdraw_enabled, status)、markets(base, quote, price_tick, qty_step, min_notional, self_trade_policy, fee_schedule_id, status ∈ {ACTIVE, HALTED, CANCEL_ONLY})、fee_schedules(maker_bps, taker_bps)三張表 + admin CRUD(寫審計)。引擎啟動載入 ACTIVE 市場各起一 goroutine,支援 reload 命令;HALTED 拒新單但保留掛單、允許取消。
- 狀態:已由決策「功能、資產、registry 為 DB、受控重啟 + reload」處理;表欄位、市場狀態語意與部署腳本需寫進 v1.0。

### 13. 撮合語意未定義:市價單凍結、取消競態、部分成交逐筆結算、自成交(F15)
- 章節:第 4 節撮合引擎、第 6 節
- 問題:市價買單下單時不知成交價,不定義凍結語意 Hold 無法實作;取消若直接改 DB 而非進引擎 seq 序列,會與同時的成交競態造成重複 Release;部分成交要逐筆扣 hold、最後一筆才 Release 剩餘。這些是 Phase 2 屬性測試的核心情境。
- 證據:「撮合引擎 | 訂單簿、price-time priority 撮合」;第 6 節只有「撮合成功 → 發布成交事件」「解凍資金」。
- 建議:新增「撮合語意」小節:市價買以 quote 金額下單並全額 Hold quote,市價賣以 base 數量 Hold base,IOC 吃對手簿至用完或觸及保護帶(相對最佳價 ±X%)後剩餘取消並 Release,空簿直接 REJECTED 不凍結;取消作為命令進同一 per-market seq 序列,由引擎裁決並發 OrderCancelled(帶 remaining_qty / remaining_hold);每筆 Trade 觸發 Settle,終態 Release 剩餘,不變量「Σhold(order) = 未成交部分應凍結」;markets.self_trade_policy ∈ {ALLOW, CANCEL_NEWEST, CANCEL_OLDEST},v1 實作 CANCEL_NEWEST,事件帶 self_trade_prevented。
- 狀態:已由決策「市價語意、IOC、STP = cancel-newest」處理;取消進 seq、逐筆 Settle、保護帶與不變量需寫進 v1.0。

### 14. 資料所有權、schema 隔離、migration 與 DB 存取層未定(F06 / E07 / L07)
- 章節:第 4、5、8 節
- 問題:四個服務共用單一 DB 與單一 role,沒有任何一張表的擁有者;init.sql 只在 volume 首次建立時執行,Phase 1 後每次改表都要砍資料,beta 有真實資料後更不可能;原則 3 只是口頭約定。DB 層若臨場選 GORM,帳本需要的顯式交易、FOR UPDATE、NUMERIC 映射會成為負資產。
- 證據:`POSTGRES_USER: exchange` 單一帳號、`infra/postgres/init.sql`、各服務僅「Go + PostgreSQL」。
- 建議:新增「模組 → 擁有的表」對照表,規則「一張表只有一個擁有者可寫」:auth.users / api_keys、ledger.accounts / journal_entries / postings、trading.orders / trades、registry.assets / markets / fee_schedules、chain.deposit_addresses / deposits / withdrawals / scan_cursor / sweeps、marketdata.klines、audit.events。單一 Postgres 以 per-module schema + 獨立 DB role,postings 對所有 role 禁止 UPDATE/DELETE、只有 ledger role 可 INSERT。migration 用 goose 或 golang-migrate(embed.FS),`exchange migrate` 子命令 + compose 一次性 migrate service(`condition: service_completed_successfully`),同一命令給 Helm pre-install hook;app 啟動只檢查版本。DB 層 pgx/v5 + sqlc,pgtype.Numeric ↔ money 的 helper 寫一次並測試。
- 狀態:需寫進 v1.0;migration 工具、schema/role 隔離程度與 pgx+sqlc 仍待作者決定。

### 15. 對外契約缺席:endpoint 清單、WS 公開/私有頻道、snapshot+seq 協定、WS 歸屬(F17 / P04 / C01 / E04)
- 章節:第 3、4、6、8 節
- 問題:全文沒有一個 REST endpoint 或 WS channel;shared/ 的「proto」暗示 gRPC 與 OpenAPI 決策衝突;WebSocket 同時出現在 gateway 與 market-data;只規劃公開行情,下單者本人沒有任何管道知道自己的單成交了;沒有 seq 與 snapshot,客戶端斷線後無法重建訂單簿。對白牌客戶而言契約就是交付物。
- 證據:「API 閘道 | REST/WebSocket 入口」、「行情服務 | K線、深度、成交即時推播 | Go + WebSocket + Redis pub/sub」、「shared/ # 共用 proto 定義」。
- 建議:`api/public/v1/openapi.yaml` 與 `api/admin/v1/openapi.yaml` 為單一真相,oapi-codegen strict-server(std-http 或 chi generator),CLI 與 E2E 直接用產生的 client;endpoint 清單:auth / API key、GET balances、POST/DELETE/GET orders、GET trades、GET markets、GET deposit-address、POST/GET withdrawals;admin:assets / markets / fee_schedules CRUD、withdrawals review、ledger adjustment、reconciliation、audit、users kyc_status。WS 規格寫 docs/ws-api.md:公開 orderbook / trades / kline(snapshot 含 last_seq + 連續 seq 的 delta,缺號重抓 snapshot)、私有 orders / fills / balances(JWT 或 API key 認證,依 account_id 路由);WS 只由一個 role 終結,每副本無狀態訂閱 NATS 就地過濾。第 6 節每一步標明呼叫者與協定,compose depends_on 據此對照。
- 狀態:已由決策「OpenAPI 公開/管理兩份、REST + WS(公開 + 私有)+ Webhook」處理;endpoint 清單、WS 協定與歸屬需寫進 v1.0;HTTP router 選擇仍待作者決定。

### 16. 對外寫入 API 缺客戶端冪等鍵,HTTP 重試即重複凍結/重複提現(C02 / E03)
- 章節:第 4 節 api-gateway、第 6 節步驟 1–3 與末段
- 問題:交易所最常見的重複資金操作不是來自事件匯流排,而是客戶端 timeout 後重送同一個 POST;提現規劃為限額內自動放行,重送一次就是兩筆自動出金。這必須以 schema 唯一約束形式進入第一版 orders / withdrawals 表與 OpenAPI。
- 證據:步驟 1「Client 送出下單請求 → api-gateway(驗證 JWT)」到步驟 2「凍結對應資金」之間無任何請求識別。
- 建議:OpenAPI 規定所有動資金的 POST 必帶冪等鍵:下單 `client_order_id`(orders 表 UNIQUE(tenant_id, account_id, client_order_id),重送回原訂單與 200);提現 `Idempotency-Key` header,儲存 key → (request hash, 狀態碼, 回應),同 key 不同 payload 回 422,TTL 24h;取消端點天然冪等。Phase 2 / 4 整合測試:「同一請求重送 N 次只產生一筆凍結/訂單/提現」,CLI 重試時帶同一鍵。
- 狀態:需寫進 v1.0 計畫。

### 17. 風控同步/非同步定位矛盾,pre-trade / pre-withdraw 檢查不在流程中(F19 / P06)
- 章節:第 4 節 risk-service、第 5 節 compose、第 6、7 節
- 問題:「限額、熔斷」是必須同步阻擋的 pre-trade 檢查,「異常交易偵測」是事後消費,介面完全不同;risk 只掛 NATS 卻被 withdrawal-worker 同步依賴;下單七步全程沒有風控。等 Phase 5 才決定,Phase 1–4 的路徑都要回頭插呼叫點。
- 證據:「風控服務 | 異常交易偵測、限額、熔斷」、`risk-service: depends_on: [nats]`、下單流程無 risk。
- 建議:切兩層——(a) `OrderPolicy` / `WithdrawalPolicy` interface 放 trading 與 chain 模組內,Phase 2 以 allow-all + 市場狀態檢查實作,Phase 4 補提現限額(超額回 NeedsReview 進 PENDING_REVIEW 而非拒絕)、白名單、用戶凍結旗標;規則參數存 DB 由後台設定;失敗即拒不凍結。(b) 事後偵測為 Phase 5 的 JetStream 消費者,結果(熔斷、凍結用戶)寫回 markets.status / 旗標由 (a) 讀取並發 RiskAlert。第 6 節補「1.5 policy 檢查」;刪除 withdrawal-worker 對 risk 的啟動相依。
- 狀態:提現限額/人工審核已由決策處理;policy 介面需寫進 v1.0;事後風控偵測的 v1 範圍仍待作者決定。

### 18. 充值流程無狀態機、確認數、reorg 處理、游標與冪等鍵;原生 ETH 與 ERC-20 偵測未區分(F20 / F22 / L06)
- 章節:第 2 節原則 3、第 4 節 deposit-listener、第 5 節 compose、第 6 節末段
- 問題:deposit-listener「更新帳本」再度違反原則 3;無 DB 就無處存游標,重啟重掃即重複入帳;anvil 單節點永不 reorg,缺口在本機完全不可見,切到 Sepolia 後被 reorg 掉的充值已入帳且可被提走。「監聽事件」只適用 ERC-20 Transfer log,原生 ETH 轉帳沒有 event,必須掃區塊交易的 to 欄位。
- 證據:「充值監聽 | 監聽測試鏈事件,更新帳本」、`deposit-listener: depends_on: [anvil, nats]`。
- 建議:狀態機 DETECTED → CONFIRMING → CONFIRMED(達 assets.required_confirmations,anvil=1、Sepolia 6~12)→ CREDITED,只有 CREDITED 由 ledger 過帳;listener 只掃鏈與發 DepositConfirmed 事件,持久化 last_scanned_block 與每個已處理區塊的 (number, hash, parent_hash),推進前檢查 parent_hash 連續性,不符回退共同祖先重掃並把未入帳充值標 ORPHANED;超過確認數後的深 reorg 不自動回滾、告警進人工對帳。冪等鍵 (chain_id, tx_hash, log_index),原生 ETH 用哨兵值。兩條偵測路徑:ETH 用 eth_getBlockByNumber(full txs) 比對 to;ERC-20 用 eth_getLogs 過濾 Transfer topic,掃描區間限 N 區塊;合約內部轉帳明列 v1 不支援。reorg 測試用 anvil_reorg / evm_snapshot。
- 狀態:已由決策「充值狀態機(確認數可設定)」處理;游標/區塊 hash、reorg 回退、冪等鍵與兩條偵測路徑需寫進 v1.0。

### 19. HD 派生、nonce 管理、gas / pending / 重送、歸集與熱錢包補充未設計(F23 / F24 / F25 / L06)
- 章節:第 4 節 wallet-service / withdrawal-worker、第 5 節 compose
- 問題:無派生路徑與 index 分配規則,兩個服務各自派生會重複;直接沿用 anvil 預設助記詞(全球皆知)會讓錢包模組失真。單一熱錢包多 goroutine 或重啟後 nonce 重複/跳號是最常見的故障;anvil 即時出塊讓 gas、pending、replacement 全部測不到。沒有歸集,熱錢包終究會空,「用戶總餘額 = 鏈上資產」也不成立。
- 證據:「錢包服務 | HD 錢包地址產生、簽名」、「提現處理 | 簽名、廣播交易」;全文無 nonce、gas、sweep、熱錢包。
- 建議:充值地址 m/44'/60'/0'/0/{index},熱錢包用獨立 account 層級或獨立 keystore;deposit_addresses(tenant_id, account_id, chain_id, derivation_index UNIQUE, address UNIQUE),index 由 DB sequence 分配,每帳戶每鏈一個地址、ETH 與 ERC-20 共用;種子只在 signer,scrypt+AES 加密,passphrase 由 secret 注入,`make gen-dev-secrets` 產生開發用種子,明文禁止 anvil 預設助記詞;派生用官方測試向量做單元測試。熱錢包 nonce 由單一 goroutine 序列化並與 withdrawal row 同交易落庫,啟動時與 eth_getTransactionCount(pending) 對帳,不一致拒絕啟動;EIP-1559 估價 + 每筆上限 + 每日 gas 預算;超時以同 nonce 更高費用重送,次數上限後轉人工。歸集為排程 sweep job:超過最小歸集額才歸集,ETH 轉 balance − gas,ERC-20 兩段式(熱錢包先補 gas),各地址自有 nonce;熱錢包餘額低於門檻告警,開發環境由 anvil 預設帳戶注資。
- 狀態:已由決策「BIP-44 HD 充值地址 + 排程歸集、nonce 序列化持久化、加密 keystore」處理;派生路徑、表結構、nonce 對帳、重送策略與歸集規則需寫進 v1.0。

### 20. 對帳、審計日誌與 admin 角色/權限被排到 Phase 6「選用」(P05 / F30 / F32)
- 章節:第 4 節 admin-backoffice / api-gateway、第 7 節 Phase 6
- 問題:對帳是唯一能發現「事件丟失、reorg 未回滾、nonce 錯亂、帳本寫錯」的機制,對引擎買家是採購門檻,對無領域知識的作者是自我驗證工具;Phase 4–5 就需要核准提現、停牌、凍結帳戶,沒有角色只能直接改 DB 且無審計軌跡;「限流」只有兩個字。
- 證據:「管理後台 | 用戶管理、對帳報表(後期選用)」、「Phase 6(選用)| admin-backoffice」;全文無「審計」「角色」。
- 建議:Phase 1 就在 JWT 加 role claim(user | admin | operator)與 RequireRole 中介層,所有管理動作走 `/admin/v1/*` 並進 admin OpenAPI,UI 後補;初始 admin 由 ADMIN_BOOTSTRAP_PASSWORD 建立;admin 強制 TOTP。Phase 2 起提供 `GET /admin/v1/ledger/trial-balance`(每 asset Σpostings = 0)與 `ledger_trial_balance_diff` 指標;Phase 4 起定時鏈上對帳 job,差異寫 reconciliation_breaks 並發事件。append-only audit_events(tenant_id, actor_id, role, action, target, before/after, ip, ts),所有 `/admin/*` 寫入與 auth 事件、提現各狀態、簽名、registry 變更都寫入,DB role 只授 INSERT。限流用 token bucket(v1 單副本 in-memory,多副本再上 Redis),先套登入與下單/取消端點。
- 狀態:已由決策「管理後台(市場/資產、提現審核、對帳)、admin TOTP」處理;RBAC、審計表、trial-balance 與對帳 job 排程需寫進 v1.0。

### 21. 認證、內部信任邊界與密鑰清單沒有規格(F27 / F28 / F29)
- 章節:第 4 節 auth-service / api-gateway、第 5 節 compose、第 9 節
- 問題:JWT 演算法、access/refresh 與撤銷、密碼雜湊未決;若用 HS256 共用 secret,gateway 與內部服務都持「可簽發」的鑰匙;gateway 塞 X-User-ID header 則任何能連到內網的 process 都能冒充任何用戶;單一 DB 帳號讓每個服務都能 UPDATE 餘額。compose 只注入 POSTGRES_PASSWORD,JWT 金鑰、keystore passphrase、admin 初始密碼來源空白,最可能的結果是硬編碼進公開 repo。
- 證據:「API 閘道 | … JWT 驗證」、`POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}` 為唯一密鑰、「私鑰簽名流程在這裡是教學用途」。
- 建議:新增「認證與安全邊界」小節:JWT 建議 EdDSA(Ed25519),私鑰只在 auth,其他模組以 JWKS 驗證;claims 固定 sub / account_id / tenant_id / role / iat / exp;access 15 分鐘 + refresh 存 Postgres 可撤銷;密碼 argon2id;API key + HMAC 請求簽章(timestamp + nonce)給程式化交易;驗證器設計成可替換(客戶 IdP 的 JWKS URL)。內部身分傳遞:單體內為 Go 呼叫,拆分後原樣轉發 JWT 再驗;NATS subject 授權與 Redis 密碼列為 Helm 前必做。密鑰清單:JWT 金鑰對、WALLET_KEYSTORE_PASSPHRASE(keystore 放 signer 專用 volume)、ADMIN_BOOTSTRAP_PASSWORD、各 DB role 密碼;全走 .env / docker secrets / `*_FILE`,CI 加 gitleaks。
- 狀態:最小 auth + JWT + API key、admin TOTP、加密 keystore 已由決策處理;JWT 簽章方式、內部身分傳遞與密鑰清單需寫進 v1.0,前兩者仍待作者決定。

### 22. 觀測性標為「選用」,無結構化 log 與貫穿 HTTP / NATS 的 correlation id(F37 / E08 / P09)
- 章節:第 4 節監控、第 6 節、第 7 節 Phase 6
- 問題:一筆下單橫跨多個 role 與 NATS,沒有 correlation id 連本機都難追;自動恢復若沒有重放耗時、consumer lag、對帳差額等指標,無法知道恢復是否成功;橫切關注點事後補的成本遠高於一開始用同一 telemetry 套件帶進去。
- 證據:「監控(選用)| 指標與告警 | Prometheus + Grafana」、「Phase 6(選用)」。
- 建議:Phase 0 起 `internal/platform/telemetry`:slog JSON(固定欄位 role / correlation_id / tenant_id / market / order_id),correlation_id 從 HTTP header 帶進 NATS message header 與事件信封;每個 role 暴露 /healthz、/readyz(DB、NATS、registry 載入完成,matching 額外要求訂單簿重建完成)、/metrics;業務指標最小集合:order_accept_latency、engine_seq 與各 consumer 已處理 seq 之差、outbox 積壓、啟動重放耗時、PENDING 訂單數、ledger_trial_balance_diff、hot_wallet_balance、withdrawals_pending_review、chain 掃描落後區塊數。prometheus + grafana 進 compose 預設啟動,dashboard JSON 與 alert rules 放 deploy/observability/ 隨 Helm 交付;`make trace ORDER_ID=` 以 correlation_id grep 所有容器 log。
- 狀態:已由決策「觀測性從 Phase 0 起」處理;指標清單、readyz 定義與 NATS header 傳遞需寫進 v1.0。

### 23. compose 與 anvil 服務定義的具體錯誤(F31 / F26)
- 章節:第 4 節本地測試鏈、第 5 節
- 問題:`version` 已廢棄;短語法 depends_on 只保證啟動不保證可連線,無 healthcheck、無 restart,首次 `up` 應用服務就因連不上 DB 而死;Go 服務沒有任何環境變數;init.sql 未掛載。anvil:foundry image 的 ENTRYPOINT 是 `/bin/sh -c`,陣列 command 會讓 `--host 0.0.0.0` 被吃掉、綁在 127.0.0.1;`latest` 浮動;純記憶體鏈每次 down/up 歸零但 Postgres 保留游標與 nonce,開發第一週就會反覆出現像 bug 的不一致;預設無 `--block-time`,確認數邏輯永遠走不到;Ganache 已 sunset。
- 證據:`version: "3.9"`、`depends_on: [postgres]`、`image: ghcr.io/foundry-rs/foundry:latest`、`command: ["anvil", "--host", "0.0.0.0"]`、「Foundry anvil 或 Ganache」。
- 建議:刪 version;postgres / redis / nats / anvil 加 healthcheck(pg_isready、redis-cli ping、wget /healthz、cast block-number),depends_on 改 `condition: service_healthy`,應用端仍做指數退避 retry;`x-common-env` anchor 注入 DATABASE_URL / NATS_URL / ETH_RPC_URL 等並在 .env.example 列齊;開發期開 5432 / 6379 / 4222 / 8222 port;`restart: unless-stopped`;image 釘版本。anvil 改 `entrypoint: ["anvil"]` + `--host 0.0.0.0 --block-time 2 --chain-id 31337 --state /data/anvil-state.json`,掛 volume 與 pg-data 同壽命;`make reset` 同時清兩者並重部署 mock USDC;chain 模組啟動時比對 chain_id 與 block 0 hash,鏈頭 < 游標即拒絕啟動。刪除 Ganache。
- 狀態:anvil pin 版本與狀態持久化已由決策處理;其餘修正需寫進 v1.0。

## 可以改善(minor)

### 24. 「原封不動搬到 K8s」宣稱不成立,應改為可驗證的 12-factor 需求(F08 / E09)
- 章節:第 1 節第 4 點、第 5 節、第 9 節
- 問題:與第 9 節「不是生產部署架構」自相矛盾;可搬遷的是模組邊界、image 與契約,部署描述、密鑰、網路隔離必然重做。作者有 K8s 經驗且已規劃 Helm,誤導風險低,但 graceful shutdown、依賴 retry、config 注入是 Phase 1 骨架就要做的事。
- 證據:「模塊邊界設計成未來可以『原封不動搬到 K8s』」。
- 建議:改寫為「可運維性需求」:config 由 env / `*_FILE` 注入並啟動時驗證;SIGTERM → readyz 轉 false → 停止接收 → 等 in-flight 命令落庫與 outbox flush、drain consumer → 關連線;對 Postgres / NATS / RPC 指數退避,depends_on 只當加速;matching 在 K8s 固定 replicas=1 + Recreate。第 9 節補「生產化必須重做清單」(撮合容錯、KMS adapter、DB 拆分、RPC provider、mTLS)。
- 狀態:已由決策「12-factor、Helm 最後 Phase」處理;需寫進 v1.0。

### 25. Redis 角色與 NATS 重疊,「Session」與 JWT 關係未定(E05 / F34)
- 章節:第 4 節快取、行情服務
- 問題:行情走 Redis pub/sub 形成第二條總線,失敗語意不同、單人維護成本翻倍;JWT 之下的「Session」未定義。非正確性問題,但白牌交付應少一個元件。
- 證據:「快取 | Session、行情快取 | Redis」、「行情服務 | … Redis pub/sub」。
- 建議:Redis 用途白名單只允許多副本限流計數器、可重建的深度快照快取、refresh token 撤銷清單;推播一律走 NATS;v1 單副本 gateway 可先不部署 Redis。
- 狀態:仍待作者決定(v1 是否保留 Redis)。

### 26. 非功能需求:K 線 / 成交歷史儲存位置與容量基線未定(F18)
- 章節:第 4 節行情服務、第 5 節 market-data
- 問題:Redis pub/sub 不保存資料,K 線與成交歷史不在任何儲存體職責內,Phase 3 必然返工;封閉 beta 需要一組數字供壓測與告警閾值。
- 證據:market-data `depends_on: [redis, nats]`,無 postgres。
- 建議:trades 表由 trading 寫入 Postgres 永久保存;K 線由 marketdata 消費 Trade 事件聚合(1m/5m/15m/1h/1d)寫 klines 表,重啟從最後 seq 補齊;保存期限明寫「不刪除」;容量基線如單市場 100 orders/s、下單到 WS 推播 p99 < 500ms、1,000 併發 WS、引擎恢復 < 30s,並明列不追求 HA 與微秒延遲。
- 狀態:需寫進 v1.0;容量數字仍待作者決定。

### 27. 多租戶決策未記錄(P03)
- 章節:第 1、9 節
- 問題:計畫隱含單租戶但無明文;P03 建議不加 tenant_id,作者決定預留欄位,兩者都可行,關鍵是寫成 ADR 並定義觸發重評的條件,避免「半吊子隔離」。
- 證據:全文無 tenant / operator 維度。
- 建議:ADR「v1 單租戶、每客戶一套部署;所有業務表帶 tenant_id NOT NULL 並納入唯一鍵但恆為單一值;重評條件 = 第二個付費客戶要求共用基礎設施」。
- 狀態:已由決策「預留 tenant_id、v1 單租戶」處理;需寫成 ADR。

### 28. 文件一致性、缺 out-of-scope / 風險 / 名詞表、無 ADR 與版本釘住、本地開發流程(F34 / F35 / C05 / F36)
- 章節:第 3、4、7、8、10 節
- 問題:服務名稱、根目錄名、「Echo/Gin」「anvil 或 Ganache」未決項散落;「對應先前學習路線」欄只有作者當下懂;第 10 節假定「確認後即定案」,但幾乎每個假設都會被推翻,幾個月後文件與程式必然脫節;Go / go-ethereum / foundry 版本未釘;沒有 Makefile 與 compose profiles,改一行 Go 要 build 11 個 image。
- 證據:「確認模塊清單與技術選型沒問題後…一個服務一個服務動手實作」;第 7 節「階段 6」「階段 7」無名稱。
- 建議:docs/adr/ 每個重大決定一份(架構形態、餘額唯一寫入者、事件保證、數值型別、JWT、signer、tenant_id);第 10 節改為「每完成一個 Phase 回頭修訂並升版」;go.mod 釘 go / toolchain,所有 image 用明確 tag;新增 out-of-scope(主網、法幣、槓桿、多鏈、KYC 文件、HA、極低延遲)、風險表(無領域知識、Go 併發不熟、anvil 與 DB 壽命不一致、go-ethereum 版本)、名詞表;Makefile(`up / run ROLE= / test / e2e / lint / gen / migrate / reset / demo`)、compose `profiles: [infra] / [app]` 讓開發中的 role 在主機 `go run`;第 1 節依訪談改寫定位。
- 狀態:需寫進 v1.0。

## 審查過但不採納或降級的意見

- **C03 駁回**「學習 Go 是第一目標卻無可檢核學習成果與 build-vs-buy 邊界」:真實定位為商業原型,前提不成立;build-vs-buy 由產品形態隱含(撮合/帳本/錢包自寫,HTTP、pgx、decimal、go-ethereum 用套件)。
- **F08 major → minor**「原封不動搬到 K8s」:作者有 K8s 經驗且已規劃 Helm,剩下的是措辭與 12-factor 需求清單。
- **F11 major → minor**「測試策略完全缺席」:作者已定品質基線,屬文件未同步;有價值的只剩「該測哪些不變量」。
- **F18 major → minor**「無延遲/吞吐目標」:作者不追求極低延遲,只需粗略容量基線。
- **E05 major → minor**「Redis 兩條總線」:非正確性問題,屬元件精簡。
- **P02 critical → major**「registry 缺席」:補三張表即可,不需重做既有設計。
- **P03 major → minor**「多租戶未決」:隱含設計已是單租戶,補 ADR 即可。
- **F31 部分降級**「compose 錯誤」:骨架自稱草案且作者有 Docker 經驗,一位驗證者視為實作期例行修正。
- **L07 部分失實**:oapi-codegen 原生支援 echo / gin / chi / std-http generator,「與閘道轉發相衝」並不成立,選型未定與缺 migration 仍成立。
- **與作者決策衝突而不採納的建議**:市價單延到 v1.1(F14 / F15,v1 已含市價);`pkg/` 對外 library 授權形態(L05 / E10,作者定為 internal/ + 只透過 API 整合);不預留 tenant_id(P03);「重啟即清空掛單」退路(F04);compose 初期只有一個 app 容器(F07,與 signer 獨立 process 衝突);Redis pub/sub 行情推播;最小單位整數 big.Int(L02,作者選 decimal);2FA 完全 out-of-scope(F33,admin 強制 TOTP)。
- **反向升級**:F32(審計/限流)、F37(觀測性)、C04(鏈上測試)、E10(Phase 順序)原為 minor,在白牌定位下三位驗證者一致升為 major,已反映在上文。

## 對 v1.0 計畫的結構性要求

1. **定位與產品邊界**:改寫第 1 節為白牌引擎商業原型;交付物 (A) / 參考實作 (B) / 明確不做 (C) 三類清單;單租戶 ADR(tenant_id 預留策略與重評條件);out-of-scope、風險表、名詞表。
2. **v1 功能規格與 registry**:交易對、資產、訂單類型、部分成交、手續費模型、帳戶模型;assets / markets / fee_schedules 表欄位、市場狀態語意、受控重啟與 reload;mock USDC 部署腳本。
3. **領域模型**:科目表、每個業務事件的標準分錄、訂單狀態機、撮合語意(市價 / IOC / 取消進 seq / STP)、充值與提現狀態機、所有不變量列表。
4. **數值規範**:money 套件、scale、NUMERIC、JSON 字串、tick / step 驗證、捨入方向與 rounding 科目、wei↔decimal 邊界、lint 規則。
5. **架構形態與程式佈局**:模組化單體、role 清單、cmd / internal / api / migrations / deploy 目錄、signer 獨立 process、模組 → 表對照、schema / role 隔離、migration 工具與流程、DB 存取層。
6. **一致性與事件契約**:outbox 交易範圍、JetStream stream / consumer 命名、事件信封與 catalog、冪等鍵表、相容規則、可容忍遺失的事件分類、webhook dispatcher 規格。
7. **撮合恢復設計**:純函式 Apply、seq 與命令日誌、SoT 宣告、重建 / 重放 / 快照策略、graceful shutdown、kill -9 驗收條件。
8. **介面契約**:公開 / 管理兩份 OpenAPI 的 endpoint 清單、WS 公開 / 私有頻道與 snapshot+seq 協定、WS 歸屬、客戶端冪等鍵、風控 policy 介面與規則參數。
9. **安全設計**:JWT 演算法與 claims、API key 簽章、內部身分傳遞、DB role 權限、密鑰清單與注入方式、signer 政策檢查與 KMS adapter、RBAC 與 admin TOTP、審計事件表、限流策略。
10. **鏈上整合設計**:HD 派生與地址表、確認數與 reorg 回退、原生 ETH 與 ERC-20 兩條偵測路徑、nonce 序列化與重送、歸集規則、custody 科目、對帳公式、anvil 參數與測試 RPC 策略、Sepolia 切換方式。
11. **可運維性與觀測性**:12-factor 需求、healthz / readyz / metrics 定義、指標最小集合、correlation id 傳遞、compose 修正後的完整草案、Helm 前置需求、生產化重做清單。
12. **分階段計畫與工程紀律**:每 Phase 的範圍 / DoD / 展示腳本 / 非目標,測試金字塔與 CI 內容,ADR 目錄與計畫修訂機制,工具鏈版本釘住,Makefile 與 compose profiles。
