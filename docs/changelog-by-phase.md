# 逐階段的變更記錄

> 這份文件原本是 [`docs/README.md`](README.md) 的開頭。它記的是每一個 Phase 合併了什麼、以及**在那個 Phase 抓到了什麼真的缺陷、怎麼修的**——這個專案最有價值的東西之一,但它是變更記錄,不是給第一次打開這個 repo 的人看的東西,所以搬到這裡。
>
> 想知道「這是什麼、怎麼跑起來」請看 [`docs/README.md`](README.md);想知道業務層面的全貌請看 [`docs/system-overview.md`](system-overview.md)。
>
> 底下的內容一字未改。

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
