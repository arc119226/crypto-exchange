# 領域文件:計畫 v1.0 第 6 節的逐項驗算

- 狀態:Phase 0 交付物(`docs/plan-v1.0.md` §12 Phase 0 任務「寫 `docs/domain.md`」)
- 目的:在寫任何撮合或帳本程式碼之前,用紙筆把 §6 的每筆分錄、每個狀態轉移、每條撮合規則重算一遍;找出計畫的錯誤並修正;把驗算結果變成 Phase 1~4 的測試案例名稱。
- 讀法:每一節對應 §6 的一個小節。「驗算」段落只列數字與借貸,「結論」段落只寫一句。第 8 節是本次找到的錯誤與修正,第 9 節是留給後續 Phase 的疑問。

---

## 1. 記帳模型(§6.1)

### 1.1 借貸方向速查

計畫 §6.1.1 的表以「對該科目的效果」描述 debit / credit。用一句話記:**負債與收入「貸方增加」,資產與費用「借方增加」**;用戶餘額是交易所欠用戶的錢,所以是負債。

| 科目 | 型別 | 餘額 = | 例 |
|---|---|---|---|
| `user:{account}:available:{asset}`、`user:{account}:hold:{asset}`、`pending_withdrawal:{asset}` | LIABILITY | Σcredit − Σdebit | 用戶餘額 |
| `custody:deposit_addresses:{asset}`、`custody:hot:{asset}` | ASSET | Σdebit − Σcredit | 交易所控制的鏈上資產 |
| `fee_revenue:{asset}` | REVENUE | Σcredit − Σdebit | 手續費 |
| `gas_expense:{asset}` | EXPENSE | Σdebit − Σcredit | 鏈上 gas |
| `external:{asset}` | EXTERNAL | Σcredit − Σdebit(本文件約定) | faucet、調帳、沖銷 |

`external` 在 §6.1.1 標為「無正常餘額」;恆等式需要一個符號約定,本文件採 credit − debit(與負債同向),下面推導以此為準。

### 1.2 會計恆等式推導

對任一資產,每筆 entry 都有 Σdebit = Σcredit,所以對所有 entry 加總後,**所有科目的 (debit − credit) 之和為 0**。把科目依型別分組並代入 1.1 的餘額定義:

```
Σ_asset (D−C) + Σ_expense (D−C) − Σ_liability (C−D) − Σ_revenue (C−D) − Σ_external (C−D) = 0
custody + gas_expense − (user_available + user_hold + pending_withdrawal) − fee_revenue − external = 0
custody = user_available + user_hold + pending_withdrawal + fee_revenue − gas_expense + external
```

與 §6.1.1 的式子逐項相同。**結論:恆等式成立;`external` 必須以 credit − debit 計,dev faucet 後它是負數,對帳報表要逐筆列出原因。**

### 1.3 分錄逐筆驗算(§6.1.4)

市場 `ETH-USDC`,maker 10 bps、taker 20 bps,B 買方、S 賣方。每張表最後一列是本文件加的「每資產借貸合計」。

**(a) 下單 Hold** — B 限價買 1.0 ETH @ 2000

| 科目 | debit | credit |
|---|---|---|
| `user:B:available:USDC` | 2000.000000 | |
| `user:B:hold:USDC` | | 2000.000000 |
| **USDC 合計** | 2000 | 2000 ✓ |

**(b) 成交 Settle** — S 掛單賣 0.4 @ 1990(maker),B 為 taker 吃 0.4 @ 1990

- 成交價 = maker 價 1990(§6.3)。quote = 0.4 × 1990 = **796**。
- B 收到 ETH → taker 費 = ceil(0.4 × 20 / 10000) = 0.0008 ETH;B 淨得 0.3992 ETH。
- S 收到 USDC → maker 費 = ceil(796 × 10 / 10000) = 0.796 USDC;S 淨得 795.204 USDC。
- 價差 release = (2000 − 1990) × 0.4 = **4** USDC。

| 科目 | debit | credit |
|---|---|---|
| `user:B:hold:USDC` | 796.000000 | |
| `user:S:available:USDC` | | 795.204000 |
| `fee_revenue:USDC` | | 0.796000 |
| `user:S:hold:ETH` | 0.400000000000000000 | |
| `user:B:available:ETH` | | 0.399200000000000000 |
| `fee_revenue:ETH` | | 0.000800000000000000 |
| `user:B:hold:USDC` | 4.000000 | |
| `user:B:available:USDC` | | 4.000000 |
| **USDC 合計** | 800 | 795.204 + 0.796 + 4 = 800 ✓ |
| **ETH 合計** | 0.4 | 0.3992 + 0.0008 = 0.4 ✓ |

B 剩餘掛單 0.6 ETH @ 2000 → hold 應為 1200:2000 − 796 − 4 = **1200** ✓(§6.1.5 不變量 3)。

**(c) 取消剩餘** — B 取消 0.6 ETH

| 科目 | debit | credit |
|---|---|---|
| `user:B:hold:USDC` | 1200.000000 | |
| `user:B:available:USDC` | | 1200.000000 |

B 最終:available = 2000 − 2000 + 4 + 1200 = 1204 USDC、0.3992 ETH;hold = 0 ✓。

**(d) 充值 credited** — X ETH 到 B 的 HD 地址

| 科目 | debit | credit |
|---|---|---|
| `custody:deposit_addresses:ETH` | X | |
| `user:B:available:ETH` | | X |

資產增加(借)、負債增加(貸)✓。反向分錄(深度 reorg)把兩邊對調 ✓。

**(e) 提現** — B 提 X ETH,gas G。把「錢在哪個桶」列成表最容易看出錯:

| 狀態(轉移後) | `user:B:available` | `user:B:hold` | `pending_withdrawal` | `custody:hot` | `gas_expense` |
|---|---|---|---|---|---|
| `requested` … `approved` | X 未動 | | | | |
| `funds_locked`(Hold) | −X | +X | | | |
| `signed` | | X | | | |
| `broadcast`(hold → pending) | | −X | +X | | |
| `confirmed` | | | −X | −X、−G | +G |
| `failed(broadcast)`(自 `signed`,nonce 未上鏈) | +X | −X | | | |
| `failed(on_chain)`(receipt.status = 0) | | | X 留著 | −G | +G |
| `resolve(refund)` | +X | | −X | | |
| `resolve(retry)` → 回到 `funds_locked` | | +X | −X | | |
| `resolve(cancel_nonce)`(自 `broadcast`,見 §8 E1) | +X | | −X | −G′ | +G′ |

每一列借貸相等;每個狀態下 X 恰好在一個桶裡 ✓。**ERC-20 提現:資產列用 USDC 科目,gas 列永遠是 ETH** ✓。

**(f) 歸集**

| 情境 | debit | credit | 合計 |
|---|---|---|---|
| ETH 歸集 X,gas G | `custody:hot:ETH` (X−G)、`gas_expense:ETH` G | `custody:deposit_addresses:ETH` X | (X−G)+G = X ✓ |
| ERC-20 第 1 步:熱錢包補 gas G′ | `custody:deposit_addresses:ETH` G′ | `custody:hot:ETH` G′ | ✓ |
| ERC-20 第 2 步:轉 X USDC、耗 gas G | `custody:hot:USDC` X;`gas_expense:ETH` G | `custody:deposit_addresses:USDC` X;`custody:deposit_addresses:ETH` G | ✓ |

歸集只在 custody / expense 科目間移動,**用戶科目完全不出現** ✓(Phase 4c 測試「歸集後用戶餘額不變」)。若 G′ > G,差額留在 `custody:deposit_addresses:ETH`,仍受控、仍對得上鏈。

**(g) Dev faucet / 調帳** — debit `external:USDC` X / credit `user:B:available:USDC` X。代入 1.2:custody 不變、user +X、external −X → 恆等式 0 = X − X ✓。

### 1.4 追加驗算:市價買跨兩個價位(§6.3 市價語意)

計畫沒有市價單的數字例子,補一個。asks:S1 0.3 ETH @ 1990、S2 0.5 ETH @ 1995;B 市價買 quote Q = 1000 USDC;`qty_step` 0.0001、`price_tick` 0.01。

1. Hold 1000 USDC(`hold:order:{id}`)。
2. 價位 1990:floor(1000 / 1990, 0.0001) = floor(0.50251…) = 0.5025;min(0.3, 0.5025) = **0.3** → quote 597.00;Q_rem = 403。
3. 價位 1995:floor(403 / 1995, 0.0001) = floor(0.202005…) = **0.2020** → quote 402.99;Q_rem = 0.01。
4. 0.01 < 1995 × 0.0001 = 0.1995 → 停止;剩餘 0.01 取消並 Release。

| Trade | 科目 | debit | credit |
|---|---|---|---|
| 1 | `user:B:hold:USDC` | 597.000000 | |
| 1 | `user:S1:available:USDC` | | 596.403000 |
| 1 | `fee_revenue:USDC` | | 0.597000 |
| 1 | `user:S1:hold:ETH` | 0.3 | |
| 1 | `user:B:available:ETH` | | 0.2994 |
| 1 | `fee_revenue:ETH` | | 0.0006 |
| 2 | `user:B:hold:USDC` | 402.990000 | |
| 2 | `user:S2:available:USDC` | | 402.587010 |
| 2 | `fee_revenue:USDC` | | 0.402990 |
| 2 | `user:S2:hold:ETH` | 0.2020 | |
| 2 | `user:B:available:ETH` | | 0.201596 |
| 2 | `fee_revenue:ETH` | | 0.000404 |
| release | `user:B:hold:USDC` | 0.010000 | |
| release | `user:B:available:USDC` | | 0.010000 |

驗算:USDC 597 = 596.403 + 0.597 ✓;402.99 = 402.58701 + 0.40299 ✓;ETH 0.3 = 0.2994 + 0.0006 ✓;0.2020 = 0.201596 + 0.000404 ✓;B hold 最終 1000 − 597 − 402.99 − 0.01 = 0 ✓。訂單:`filled_qty` 0.5020、`filled_quote` 999.99、狀態 `cancelled`(IOC 剩餘)、taker 費合計 0.001004 ETH。

**結論:市價買以 floor 到 `qty_step` 換算,每筆都是精確數,不需要 rounding 科目**(審查曾建議 `exchange:rounding`,計畫 v1.0 以 §6.5 的精度約束取代,驗算證實可行)。

### 1.5 不變量 → 測試名稱(Phase 2)

| §6.1.5 | 測試名稱(`internal/ledger`) |
|---|---|
| 1 每 entry 每資產 Σdebit = Σcredit | `TestPost_RejectsUnbalancedEntry`、DB trigger 測試 `TestTrigger_RejectsUnbalancedEntry` |
| 2 快取 = 推導;available/hold ≥ 0 | `TestBalances_MatchPostings`(rapid)、`TestHold_InsufficientAvailableIsRejected` |
| 3 open order 的 hold = 未成交應凍結 | `TestSettle_HoldEqualsRemainingTimesLimit`(用 1.3(b) 與 1.4 的數字) |
| 4 冪等鍵重放餘額不變 | `TestPost_IdempotentReplay` |
| 5 試算平衡恆為 0 | `TestTrialBalance_IsZeroAfterRandomOps`(rapid,含故意重放) |

---

## 2. 訂單狀態機(§6.2)

逐列檢查轉移表,每列問三個問題:誰觸發、帳本做什麼、發什麼事件。

| 起點 → 終點 | 觸發 | 帳本 | 事件 | 檢查 |
|---|---|---|---|---|
| new → `rejected` | 參數/policy/市場狀態/餘額不足/空簿 | 無(Hold 失敗即 ROLLBACK) | `order.rejected` | ✓ 拒單 = 交易回滾,無 saga |
| new → `open` | 限價單無成交 | Hold | `order.accepted` | ✓ |
| new → `partially_filled` | 部分成交後掛簿 | Hold + Settle×n(+價差 release) | `order.accepted` + `trade.executed`×n + `order.updated` | ✓ |
| new → `filled` | 全部成交 | Hold + Settle×n | `order.accepted` + `trade.executed`×n + `order.filled` | ✓ |
| new(市價/IOC)→ `cancelled` | 剩餘取消 | Hold + Settle×n + Release 剩餘 | … + `order.cancelled` | ✓ 見 1.4 |
| new → `cancelled`(STP) | 會與自己的掛單成交 | Release 剩餘 | `order.accepted` + `order.cancelled(self_trade)` | 見 §8 E3:若先與他人成交再撞到自己,還有 `trade.executed`×n |
| `open`/`partially_filled` → `partially_filled`/`filled` | 作為 maker 被吃 | Settle(+價差 release;`filled` 時 Release 殘值) | `trade.executed` + `order.updated`/`order.filled` | ✓ |
| `open`/`partially_filled` → `cancelled` | 用戶取消(進同一 seq 序列) | Release 剩餘 | `order.cancelled` | ✓ 1.3(c) |
| 終態 → 終態 | 取消請求 | 無 | 無;HTTP 200 回當前狀態 | ✓ 天然冪等 |

拒單原因枚舉齊全性:`invalid_price_tick`、`invalid_qty_step`、`below_min_notional`、`market_not_active`、`insufficient_balance`、`empty_book`、`account_frozen`、`policy_denied`、`duplicate_client_order_id_mismatch`。**缺一個**:市價單因 `max_slippage_bps` 一筆都吃不到(§8 E2)。

`hold_asset` / `hold_amount` 的推導:限價買 = quote、`price × qty`;限價賣 = base、`qty`;市價買 = quote、`quote_qty`;市價賣 = base、`qty`。四種都能在下單時確定 ✓(這正是「市價買以 quote 金額下單」的理由)。

---

## 3. 撮合語意(§6.3)

**價格時間優先**:買方價高者先、賣方價低者先,同價位 `seq` 小者先。驗證案例(Phase 1 golden):

- bids 2000(seq 1)、2000(seq 3)、1999(seq 2);市價賣 0.5 → 先吃 seq 1 再 seq 3,再 1999。
- 限價買 2010 遇 asks 1990、1995、2005 → 三個價位依序成交,每筆成交價 = 該 ask 價,價差 release 分別是 (2010−1990)、(2010−1995)、(2010−2005) × qty。

**STP `cancel_newest`**:A 掛賣 1 ETH @ 2000(seq 5);A 又下限價買 2 ETH @ 2000(seq 9)。撮合時 seq 9 會撞到自己的 seq 5 → seq 9 剩餘全數取消、seq 5 不動;若簿上在 2000 之前還有 B 的賣單 0.5 @ 1999,seq 9 先吃 B 的 0.5(正常 trade),再撞到自己才取消剩餘 1.5。事件:`order.accepted`、`trade.executed`、`order.cancelled(reason=self_trade, remaining_qty=1.5)`。

**純函式**:`Apply(cmd)` 不呼叫 `time.Now()`、不用 map 迭代順序決定任何輸出(價位一律走排序後的 slice),所以「同一命令序列重放後 `Snapshot()` deep-equal」可以用 `-count=20` 驗證。

**tick / step**:API 端「不是整數倍就拒單、不自動截斷」;`matching` 再驗一次回 `rejected` 事件而非 panic。`money.IsMultipleOf` 已在 Phase 0 實作並測試(`internal/money`)。

**Phase 1 定案(原第 9 節疑問 1、2)**:

- 限價恰等於對手價 **成交**(買方限價 ≥ 對手賣價、賣方限價 ≤ 對手買價;`crosses()`),與業界慣例一致;golden `limit_ioc_and_multi_level_sweep` 與單元測試 `TestEqualPricesTradeAndNonCrossingOrdersRest` 鎖住。
- `max_slippage_bps` 的基準價維持計畫定義:**市價單到達時的最佳對手價**,界線 = best × (1 ± bps/10000),以 18 位小數精確計算、不需為 tick 倍數。推論:最佳價位本身永遠在界線內,所以市價單絕不會「零成交卻因保護帶被拒」——`price_protection` 只會是**剩餘量的取消原因**,不會是拒單原因(修正第 8 節 E2 的原始提案)。零成交的市價單只有兩種拒單:對手側為空(`empty_book`)、quote 預算在最佳價連一個 `qty_step` 都買不到(`quote_qty_too_small`,例如 5 USDC 在 100,000 的價位);兩者都在 `Accepted` 之前判定,因此沒有 Hold、沒有 Release。

**Phase 1 實作與事件對照**(`internal/matching`):`Apply` 對每個命令回傳事件序列 `accepted → (trade → maker 的 updated|filled)* → taker 的 filled|updated|cancelled`;拒單只回 `rejected`,找不到的取消回 `cancel_rejected`。`trade.index` 是同一命令內的第 n 筆成交,供 trading 派生確定性的 trade_id。市價買的 `cancelled` 帶 `remaining_quote`(釋放 quote hold),其他訂單帶 `remaining_qty`。golden 檔在 `test/fixtures/matching/`,`exchangectl replay --file <script>` 可重播。

---

## 4. 充值狀態機(§6.4.1)

- 確認數 = head − block_number + 1。anvil `required_confirmations = 1`:偵測到當下 head = block → 1 ≥ 1,同一輪 tick 就 `credited`。Sepolia N = 6:block 100 的充值在 head = 105 時 credited。
- 冪等鍵 `(chain_id, tx_hash, log_index)`,原生 ETH 用 `log_index = -1` ✓ 與 `Transfer` log 不會撞。
- reorg 演練:游標在 120、`blocks` 環保留 (118, h118)、(119, h119)、(120, h120)。新 head 121 的 `parent_hash ≠ h120` → 回退到最近共同祖先(假設 118),把 block 119、120 中 `status ∈ {detected, confirming}` 的充值標 `orphaned`,重掃 119…121;若同一筆 tx 在新鏈重現 → `SELECT … FOR UPDATE` 命中舊列 → **UPDATE**(block_number、block_hash、confirmations、status)而非 INSERT ✓;超過 `ORPHAN_EXPIRY_BLOCKS` 未重現 → `dropped`。
- 入帳後的深度 reorg 只發 `deposit.reversed` + 告警,反向分錄由人工確認執行;若用戶已把錢用掉,`balances` CHECK 會擋住反向分錄 → 進人工處理 ✓(不自動追討)。
- 地址池:`api` 只做 `UPDATE … SET account_id = $1 WHERE id = (SELECT … WHERE account_id IS NULL … FOR UPDATE SKIP LOCKED)`,不碰金鑰 ✓。

---

## 5. 提現狀態機(§6.4.2)

兩條鐵律逐狀態核對:

| 狀態 | 帳本已鎖? | 已簽名? | 已廣播? | 重啟時的動作 |
|---|---|---|---|---|
| `requested` / `policy_check` / `pending_review` / `approved` | 否 | 否 | 否 | 續跑 policy / 等審核 |
| `funds_locked` | **是** | 否 | 否 | 分配 nonce、簽名 |
| `signed` | 是 | **是**(nonce + raw tx 已落庫) | 否 | 重播 raw tx(同 nonce,冪等) |
| `broadcast` | 是(在 pending_withdrawal) | 是 | **是**(tx_hash 已記錄) | 追蹤 receipt / 到期重送 |
| `confirmed` / `failed` | 已結清 | — | — | 無 |

「未鎖不簽」:簽名只在 `funds_locked` 之後 ✓;`Sign` 內再查 DB 確認該 id 為 `funds_locked` 且金額/資產/地址一致 ✓(雙重保險)。「每個狀態落庫後才做下一步」:`signed` 的 nonce 與 raw tx 和狀態同一交易落庫,所以 kill 在廣播前後都能從狀態續跑 ✓。

`NonceManager` 三種啟動情況:pending < dbNext 且 DB 有對應列 → 重播;pending < dbNext 且無對應列 → 缺口,以 0 ETH 自轉填補;pending > dbNext → 有外部交易用了熱錢包私鑰 → 拒絕啟動 ✓ 三種情況互斥且窮盡。

---

## 6. 歸集(§6.4.3)

- ETH:`balance ≥ sweep_threshold` → 轉 `balance − gas`;分錄 1.3(f) ✓。
- ERC-20 兩段式:先補 gas(tx1,狀態 `gas_funded`),再由充值地址簽 `transfer(hot, amount)`(tx2)。充值地址的私鑰只在 `signer` 派生,`SignRequest{Kind: sweep}` 政策:`From ∈ deposit_addresses` 且 `To == hot` ✓;`gas_fund`:`From == hot`、`To ∈ deposit_addresses`、`Value ≤ MAX_GAS_FUND` ✓。
- 熱錢包低於 `HOT_WALLET_MIN_ETH` → `alert.hot_wallet_low` ✓。

---

## 7. 數值規範(§6.5)

**精度約束證明**:價格是 `price_tick` 的整數倍 ⇒ 小數位 ≤ scale(tick);數量是 `qty_step` 的整數倍 ⇒ 小數位 ≤ scale(step);乘積小數位 ≤ scale(tick) + scale(step) ≤ quote.scale ⇒ `price × qty` 在 quote 的 scale 內恰好可表示,不需捨入 ✓。ETH-USDC:2 + 4 ≤ 6 ✓。Phase 0 的 `registry.ValidatePrecision` 就是這條(`internal/registry/validate.go`)。

**手續費捨入**:fee = ceil(amount × bps / 10000) 到該資產 scale。同一個數字同時出現在扣方與 `fee_revenue`,所以守恆不受捨入影響 ✓。例:quote 402.99 × 10 bps = 0.40299 → 6 位小數內精確;若 quote 為 402.995(不可能,因 price×qty ≤ 6 位)才需要 ceil。base 側:0.2020 × 20 bps = 0.000404,18 位內精確。**ceil 只在極端情況生效,但方向固定對交易所有利,不變量測試要包含一個真的需要 ceil 的案例**(例如 maker 1 bps × quote 0.000001 → fee 0.000001)。

**JSON**:Phase 0 已強制 `money.Amount` 只接受字串(`TestAmountWireFormatRejectsNumbers`),OpenAPI `Amount` schema `pattern ^-?[0-9]+(\.[0-9]+)?$` + `x-go-type` ✓。

**鏈上邊界**:`ToWei/FromWei` round-trip 測試含 0、1 wei、2^256−1(Phase 4);Postgres `NUMERIC(36,18)` 放不下 2^256−1 已在 Phase 0 整合測試證明(`TestNumericRoundTripAgainstPostgres` 期望 `22003`)。

---

## 8. 發現的錯誤與修正

| # | 位置 | 問題 | 修正 |
|---|---|---|---|
| **E1** | §6.4.2 `broadcast`(重送耗盡)→ `resolve(cancel_nonce)` 列:「確認後 Release」 | `Release` 在 §6.1.3 定義為 hold → available,但依 §6.1.4(e) 資金在 `signed → broadcast` 時已從 hold 移到 `pending_withdrawal`;對 hold 做 Release 會讓 hold 變負、`balances` CHECK 失敗,錢卡在 `pending_withdrawal`。 | 改為 debit `pending_withdrawal` X / credit `user:available` X(與 `resolve(refund)` 相同),取代交易的 gas 記 `gas_expense`;`failed(broadcast)` 的分錄取決於**前一個桶**而非狀態名稱。**已修正 `docs/plan-v1.0.md`**(§6.1.4(e) 新增一列、§6.4.2 該列改寫)。 |
| **E2** | §6.2 拒單原因、§6.3 市價單 | 市價單「零成交」的情況沒有完整定義:計畫只寫了空簿 → `rejected(empty_book)`;quote 預算小到在最佳價連一個 step 都買不到的情況(5 USDC 對 100,000 的價位)既不是空簿也不該 Hold 後再 Release。 | Phase 1 新增拒單原因 `quote_qty_too_small`,與 `empty_book` 一樣在 `Accepted` 之前判定。保護帶以到達時最佳價為基準,最佳價位永遠在界線內,所以 `price_protection` 只作為**取消原因**(有成交後才觸及界線),不需要拒單原因(第 3 節 Phase 1 定案)。計畫 v1.1 補進枚舉。 |
| **E3** | §6.2 STP 列的事件欄 | 只寫 `order.accepted + order.cancelled(self_trade)`,漏掉撞到自己之前可能已與他人成交的 `trade.executed`×n(第 3 節例子)。 | 事件欄補 `trade.executed`×n(n ≥ 0)。文件層級澄清,不影響實作。 |
| **E4** | §6.1.3 `Release` 鍵 `release:order:{order_id}:{seq}` vs §6.1.4(b) | 價差 release 出現在 Settle 的同一 entry 內(鍵 `settle:trade:{trade_id}`),不是獨立 Release;兩者並存但計畫沒說清楚。 | 約定:每筆成交的價差 release 屬於 Settle entry;`release:order:…` 只用於取消 / IOC 剩餘 / `filled` 時的殘值。 |
| E5 | §6.1.1 `external` | 標「無正常餘額」但恆等式需要符號。 | 本文件 1.1 約定 credit − debit。 |
| E6 | §6.6 `assets` | 沒寫 `display_scale ≤ scale`。 | Phase 0 migration 已加 `CHECK (display_scale BETWEEN 0 AND scale)`。 |

E1 是計畫的實質錯誤(會讓一條人工處置路徑在資料庫層失敗),滿足 Phase 0「找出至少一處本文件的錯誤並修正」。Phase 4b-2 實作 `resolve(cancel_nonce)` 時照這條勘誤走:`settleCancelled` 是 `Post` 而不是 `Release`,並且等取代交易被挖出來才動錢。

---

## 9. 疑問清單(留給對應 Phase 決定)

1. **(Phase 1,已定案 → 第 3 節)** 限價恰等於對手價成交。
2. **(Phase 1,已定案 → 第 3 節)** 保護帶基準維持「到達時的最佳對手價」;`price_protection` 只是取消原因。
3. **(Phase 2)** `balances` 的 `version` 欄位是否用於樂觀鎖?計畫同時用 `FOR UPDATE`。建議只留 `FOR UPDATE`,`version` 作為除錯用途。
4. **(Phase 2)** 手續費是否允許 0 bps 的市場(做市優惠)?`fee_schedules` CHECK 允許 0,ceil(0) = 0,守恆不受影響 → 可以。
5. **(Phase 3)** `order.accepted` 對「同交易內立刻全部成交」的單也要發(§6.2 規則),事件順序 accepted → executed×n → filled 在 outbox 內以 `id` 排序即可;跨市場順序不保證 → 客戶端只能依 `account_seq`。
6. **(Phase 4)** ERC-20 歸集第 1 步「精確 gas」G′ 的估算若低於實際,第 2 步失敗;建議 G′ = estimate × 1.2 並接受少量 ETH 灰塵留在充值地址(第 6 節)。
7. **(Phase 4)** `withdrawal_fee`(v1 = 0)一旦非零,應在 `funds_locked` 時一併 Hold(X + fee),`confirmed` 時 fee 進 `fee_revenue`;計畫沒有這筆分錄,v1.1 補。
8. **(Phase 5)** 對帳報表的 `external` 明細如何呈現「已知原因」?建議 `journal_entries.kind ∈ {faucet, adjustment, write_off}` + `reason`。

---

## 10. Phase 2 程式碼與 §6.1 的對應

| §6.1 | 實作 | 備註 |
|---|---|---|
| 科目型別與方向(6.1.1) | `ledger.HouseCode.Type()`、`AccountType.DebitNormal()`;`HouseBalances` 依型別調整符號 | `external` 採 credit − debit(第 1.1 節約定),faucet 後為負 |
| 三欄餘額(6.1.2) | `ledger.balances(available, hold, version)` 只存 spot 帳戶;`Balance.Total()` 推導 | `version` 每筆 posting +1,只作除錯與測試斷言,鎖用 `FOR UPDATE`(`LockBalance` 的 no-op upsert) |
| 原子操作(6.1.3) | `Service.Hold / Release / Settle / Credit / Adjust / Post`;`idempotency_key` UNIQUE,`INSERT … ON CONFLICT DO NOTHING RETURNING` 判斷重放 | 重放回原 entry、`replayed=true`,不寫任何東西(admin API 以 200 而非 201 表示) |
| 分錄範例 (a)(b)(c)(g) | `TestLedgerPlanExampleFlow`(整合測試)逐筆對數字;`TestBuildSettleEntry_PlanExample`(單元)對 8 筆 posting | (b) 的價差 release 屬於 Settle entry(第 8 節 E4) |
| 手續費(6.5) | `ledger.ComputeFee` = ceil(amount × bps / 10000) 至資產 scale;買方費用在 base、賣方在 quote;maker/taker bps 依 `BuyerIsTaker` | `TestComputeFee` 含真的需要 ceil 的案例 |
| 不變量(6.1.5) | 1 `Entry.Validate` + deferred trigger `ledger.check_entry_balanced`;2 `DerivedBalances` vs `Balances`(每個整合測試結尾)+ CHECK ≥ 0;4 冪等重放;5 `TrialBalance` 與 `ledger_trial_balance_diff` gauge | 3(open order hold 守恆)要等 Phase 3 有訂單才能測 |
| 權限(§14) | 0003 只 GRANT:SELECT 給所有角色;INSERT entries/postings + INSERT/UPDATE balances 給 `ex_engine`、`ex_chain`、`ex_admin`、`ex_all`;INSERT accounts 給 `ex_api`(註冊);無人有 UPDATE/DELETE postings | `TestLedgerRejections` 證明 `ex_all` UPDATE postings → 42501、`ex_api` Hold → 42501 |
| 管理員調帳(g) | `POST /admin/v1/ledger/adjustments`(reason 必填、寫 `audit.audit_events`)、`exchangectl admin fund` | Phase 2–4 以 `ADMIN_API_KEY` 保護,Phase 5 換 session + TOTP |

**Phase 2 學到的事**:Postgres 對表不預設授 PUBLIC 權限,所以「只 GRANT 需要的」就夠,不需要 REVOKE;identity 欄位的 sequence 也要 `GRANT USAGE`;constraint trigger 必須 `FOR EACH ROW`,deferred 到 commit 後每筆 posting 各跑一次(小分錄可接受)。

## 11. Phase 0 程式碼與 §6.6 的對應

| §6.6 | 實作 | 備註 |
|---|---|---|
| `assets` 欄位 | `migrations/0002_registry_core.sql` `registry.assets` | 全部欄位齊;多加 `display_scale ≤ scale`、`contract_address` 格式、`is_native ⇔ contract_address IS NULL` 的 CHECK |
| `markets` 欄位 | `registry.markets` | `base ≠ quote` CHECK;`self_trade_policy` / `status` 枚舉 CHECK |
| `fee_schedules` | `registry.fee_schedules` | `maker_bps`、`taker_bps` ∈ [0, 10000] |
| `withdrawal_limits` | `registry.withdrawal_limits` | PK `(tenant_id, asset_id, kyc_level)`,`kyc_level` ∈ {0,1,2} |
| 精度約束 | `registry.ValidatePrecision`(`internal/registry/validate.go`) | 單元測試 `TestValidatePrecision` |
| seed 值 | `internal/registry/seed.go` | ETH(scale 18 / display 6 / 1 確認)、USDC(6 / 2)、ETH-USDC(tick 0.01、step 0.0001、min_notional 5、maker 10 / taker 20、`cancel_newest`) |
| 寫入權限 | `GRANT INSERT, UPDATE … TO ex_admin, ex_all` | 整合測試證明 `ex_api` 寫入得到 `42501` |

## 12. Phase 3a 程式碼與 §6.2 / §7 的對應(trading + eventbus)

Phase 3 分三個 PR:3a 引擎與事件(本節)、3b auth + public 交易端點 + 限流、3c NATS request-reply 多容器 + compose E2E。

| 計畫 | 實作 | 備註 |
|---|---|---|
| 訂單欄位與狀態(§6.2) | `migrations/0005_trading_core.sql` `trading.orders`;`trading.Status` | 多加 `hold_asset / hold_amount / hold_remaining`:每張單「還凍結多少」,讓不變量 3 變成一句 SQL(見下);`rejected ⇔ reject_reason IS NOT NULL`、市價買 `qty IS NULL` 等以 CHECK 鎖住 |
| 拒單 = 交易回滾、無 saga(§6.2) | `runner.place`:`BEGIN → 推進 seq → SAVEPOINT → ledger.Hold → matching.Apply → (Rejected ⇒ ROLLBACK TO SAVEPOINT) → 寫 orders / trades / 分錄 / outbox → COMMIT` | savepoint 讓「Hold 成功但簿拒單(空簿、quote 太小)」在同一交易內撤銷凍結,拒單仍持久化為 `rejected` 並帶 seq;policy(市場非 active、帳戶凍結)與參數錯誤在進簿前拒絕、不消耗 seq |
| `client_order_id` 冪等 | UNIQUE `(tenant_id, account_id, client_order_id)`;runner 先查:相同內容 → 回原單 `Replayed=true`(含其成交);不同內容 → `ErrClientOrderIDMismatch`(3b 對應 HTTP 422) | 拒單也佔用 `client_order_id`:重送同一 id 得到同一張 rejected 單,語意明確 |
| 每市場單一 goroutine、seq(§5.2、ADR-0002) | `trading.runner`;`trading.market_sequences.last_seq` 以 `UPDATE … WHERE last_seq = $2 − 1` 守衛推進 | 守衛失敗 = 有第二個寫入者 → `ErrSequenceConflict` 並標記重建;seq 只在到達 `Apply`(含 Hold 失敗)時消耗,policy 拒單不佔號 |
| commit 失敗 → 簿標髒重建 | `runner.dirty` → 下一個命令前 `restore()`(從 `orders WHERE status IN (open, partially_filled)` + `last_seq`) | 命令的 DB 工作用 `context.WithoutCancel` + 10 s 逾時,客戶端中途離開不會把已 commit 的命令變成重建 |
| Settle / Release 鍵(§6.1.3、§8 E4) | `settle:trade:{trade_id}`;`release:order:{order_id}:{seq}` 用於取消、IOC 剩餘、`filled` 殘值 | 買方限價價差 release 在 Settle entry 內(`ledger.BuildSettleEntry`);市價買剩餘預算在終態一次 Release |
| 不變量 3:Σhold(order) = 未成交應凍結 | `Σ hold_remaining` over open orders per `(account, hold_asset)` == `ledger.balances.hold`(`assertHoldInvariant`,每個交易測試與屬性測試每輪皆驗) | 這是 Phase 2 唯一無法測的不變量,現在補齊 |
| 事件 envelope(§7.1) | `eventbus.Envelope`;`event_id` ULID(單毫秒內單調);subject `ex.v1.<domain>.<type>.<tenant>.<scope>`,scope = 市場符號(市場域)或 account_id(帳戶域) | `event_type` 兩段式由 CHECK 與 `Validate` 雙重鎖住;`account_seq` 在同一交易內 `UPDATE ledger.accounts SET next_seq = next_seq + 1` |
| 事件 catalog(§7.2) | 3a 發出 `order.accepted / updated / filled / cancelled / rejected`、`trade.executed`、`balance.updated`(每命令每 (account, asset) 一則,取最終餘額) | `ledger.posted` 留給 Phase 5 admin 投影首次消費時再發;payload 結構在 `internal/trading/events.go`,3b 產出 `api/events/v1/*.json` + golden 測試 |
| outbox → JetStream(§7.3) | `migrations/0006_eventbus_core.sql`(`outbox` + `AFTER INSERT` statement trigger `pg_notify('outbox_new')`、`processed_events`);`eventbus.Relay`(LISTEN + 100 ms 輪詢、依 id 批次發布、`Nats-Msg-Id = event_id`、成功後 `published_at`);`eventbus.EnsureStreams`(`EX_TRADING / EX_CHAIN / EX_REGISTRY`,2 分鐘去重窗) | 只有 engine role(持 advisory lock)跑 relay;`TestOutboxRelayPublishesToJetStream` 證明重發同一批列 stream 訊息數不變 |
| 單一引擎實例(§5.1) | `Engine.Start` 以 `pg_try_advisory_lock(hash("exchange-engine:"+tenant))` 在專用連線上取鎖,取不到每秒重試、`/readyz` 的 `engine` 檢查為 false | `TestEngineSingleInstanceLock`:第二個引擎在鎖釋放前無法啟動 |
| policy 最小版(§8) | `policy.Basic`:`active` 才收新單;`halted / cancel_only` 只收取消;凍結帳戶可取消不可下單 | 限額與提現政策在 Phase 4/5 |
| 指標(§15) | `trading_command_queue_depth / trading_apply_duration_seconds / trading_orders_total / trading_trades_total / engine_seq / engine_open_orders / engine_rebuild_duration_seconds / engine_rebuilds_total / outbox_backlog / outbox_relay_lag_seconds / outbox_published_total` | |

**驗證(整合測試,`test/integration/trading_test.go`、`eventbus_test.go`)**:§6.1.4 (a)(b)(c) 經引擎逐數字相符(B `8004 / 1200`、`0.3992 ETH`、S `795.204`、fee `0.796 USDC + 0.0008 ETH`、取消後 `9204`);`client_order_id` 重送 10 次一張單一筆 hold;6 種拒單皆持久化且無分錄;市價買以 quote 預算 `floor(500/2010, 0.0001) = 0.2487`、剩餘 0.113 釋放;IOC、STP;60 筆隨機單後「停掉引擎再啟動」`Snapshot.Equal` 且 hold 不變量、試算平衡、seq 一致;500 輪隨機序列(rapid)每輪驗 hold 不變量;40 goroutine × 10 單無錯、seq 1..400 無缺口。

**Phase 3a 學到的事**:`timestamptz` 只有微秒精度,命令時間戳要先 `Truncate(time.Microsecond)`,否則重建後的 `RestingOrder.Timestamp` 與記憶體不等;pgx 的 `tx.Begin` 在交易內就是 SAVEPOINT,正好對應「Hold 成功但簿拒單」;Prometheus 的 `float64` 不是錢,以 `metrics.go` 的 helper 集中 `//nolint:forbidigo`。


## 13. Phase 3b 程式碼與 §6.7 / §7.4 / §14 的對應(auth + public API + 限流)

3b 讓 §5.2 的路徑第一次從 HTTP 走到引擎:`POST /v1/orders`(JWT 或 API key)→ `api` 驗證、限流 → `trading.Service.PlaceOrder` → 同進程引擎。拆分部署的命令匯流排留給 3c。

| 計畫 | 實作 | 備註 |
|---|---|---|
| users / accounts 分表、引擎只認 `account_id`(§6.7) | `migrations/0007_auth_core.sql`:`auth.users`(email 小寫 + 格式 CHECK、`role user\|admin`、`kyc_level 0..2`、`status active\|frozen`、TOTP 欄位預留給 Phase 5)、`auth.refresh_tokens`、`auth.api_keys`;`auth.Service.Register` 在同一交易內 `CreateUser` + `ledger.CreateSpotAccount(owner)` + 審計 | JWT 的 `account_id` claim 由 `ledger.SpotAccountOf(user_id)` 查得;`api` 之後只把 `Principal.AccountID` 交給 trading / ledger,handler 從不碰 email |
| 密碼(ADR-0006) | argon2id,PHC 字串 `$argon2id$v=19$m=65536,t=3,p=1$…`,參數寫在 hash 內可日後升級;長度 8..128 | 未知 email 也驗一次固定 dummy hash,登入失敗的耗時與帳號存在無關(`TestLoginTimingEqualiser` 級別的保護,不是常數時間承諾) |
| JWT EdDSA + JWKS(ADR-0006) | `auth.Signer` / `auth.Verifier`(`lestrrat-go/jwx/v3`);`kid` = 公鑰 SHA-256 縮圖;claims `sub / account_id / tenant_id / role / scopes / method / iss / aud / iat / exp`;`aud=exchange`;access 15 min、時鐘偏差 30 s;`GET /.well-known/jwks.json` 只回公鑰 | `aud=internal` 的內部 JWT 已在 `Claims` 定義,鑄造與轉發在 3c 的 NATS 命令匯流排一起做 |
| refresh 7 天、可撤銷(ADR-0006) | 只存 SHA-256 hash;`Refresh` 輪替:舊列 `revoked_at + replaced_by`,新列一併寫入;**已輪替的舊 token 再被使用 = 洩漏**,同用戶全部 refresh token 立即撤銷並寫審計 `auth.refresh.reuse_detected`;`Logout` 只撤銷不刪列 | 與 ADR「撤銷 = 刪列」不同:保留列才能辨識重放。登出後或家族已撤銷的 token 再送只回 401、不再重複記 reuse |
| API key HMAC(§14、ADR-0006) | `key_id = ak_<24 hex>`、secret 32 bytes hex 只在建立回應出現一次;secret 以 AES-256-GCM 加密存放,金鑰 `API_KEY_MASTER_KEY`(dev 未設 → 程序生命期的隨機金鑰並警告;非 dev 未設 → API key 停用);canonical string `ts\nMETHOD\nrequestURI\nbody`,`X-API-SIGNATURE = hex(HMAC-SHA256)`,`X-API-TIMESTAMP` 為 unix ms、±30 s;scopes `read\|trade\|withdraw`;IP / CIDR 白名單(不信任 `X-Forwarded-For`,§18);`last_used_at` | 簽章涵蓋 body,中介層先把 body 讀進記憶體(上限 1 MiB)再交給 handler;API key 不能建立 / 撤銷 API key(只能從 session);`docs/api-conventions.md` 有簽章範例 |
| `RequireUser / RequireScope` 中介層(§8) | `auth.Authenticate`:Bearer 或 `X-API-KEY` 三件組 → `Principal` 進 context;沒有憑證直接放行,由 handler 的 `principal()` / `requireScope()` 決定 401 / 403 | JWT session 持有全部 scope;缺 scope 一律 403 |
| 限流(§14) | `internal/ratelimit`:整數 token bucket(每 `Window/N` 補一枚);`Redis`(單一 Lua script,原子)、`Memory`、`Fallback`(Redis 失敗退回記憶體並記 warn);登入 per IP `10/1m`(註冊共用同一桶)+ per account `5/1m`,下單 / 取消 per account `20/1s`;可由 `RATELIMIT_*` 調整 | 429 為 problem+json 並帶 `Retry-After`(秒,向上取整);登入每次嘗試都消耗兩個桶,連續失敗會把帳號桶鎖到補滿(DoD:第 6 次 429) |
| public 端點(§7.4) | `api/public/v1/openapi.yaml`:auth ×4 + JWKS、api-keys ×3、account / balances / ledger entries、registry ×3、orders ×5、fills、depth、trades;`internal/api/{auth,account,trading,marketdata}_handlers.go` | ticker / klines / chain 端點分別留給 Phase 6 / 4 |
| `client_order_id` 冪等與狀態碼 | 新單 201;相同內容重送 200 + 原單;同 id 不同內容 422;業務拒單是 **201 + `status=rejected`**(拒單是一張單,不是錯誤);參數錯誤 400;市場不存在 404;引擎不在 503 | 3a 的 `PlaceOrderResult.Replayed` 直接對應 201 / 200 |
| 帳戶資料只看自己的 | `GET /v1/orders/{id}`、`DELETE`、`GET /v1/fills?order_id=` 都以 `(tenant, account_id, id)` 過濾,他人的單一律 404 / 空;`GET /v1/ledger/entries` 只回呼叫者自己的 posting(對手方與 `fee_revenue` 腿不出現) | `ListFillsByAccount` 的 `order_id` 條件同時要求該單是呼叫者自己的 maker / taker 腿(整合測試抓到的漏洞) |
| 深度(§7.4) | `GET /v1/markets/{symbol}/depth` 直接讀同進程引擎的 `Snapshot`(含 `last_seq`),預設 20 檔、上限 200;沒有引擎回 503 | 3c 拆分部署後由 stream / Redis 快照提供 |
| `exchange admin bootstrap`(§14) | `auth.Service.BootstrapAdmin`:冪等,已存在則不改密碼;寫審計 `auth.admin.bootstrap`;需 `ADMIN_BOOTSTRAP_EMAIL / PASSWORD` | `exchangectl` 目前沒有 admin 登入(admin TOTP + session 在 Phase 5) |
| `exchangectl`(§12 Phase 3) | `user register\|login\|logout\|me`、`api-keys create\|list\|revoke`、`orders place\|cancel\|list\|get`、`balances`、`fills`、`book`、`trades`、`e2e`;憑證 `--token` / `EXCHANGE_TOKEN` 或 `--api-key --api-secret` / `EXCHANGE_API_KEY(_SECRET)`,API key 模式對每個**帶憑證的**請求做 HMAC 簽章;`user register|login|logout` 走 `newAnonymousClient`,一個憑證都不帶 | `e2e` 用 admin API 注資、依 §6.1.4 數字逐項斷言、再驗試算平衡 |

**驗證(整合測試,`test/integration/api_test.go`)**:未帶憑證 401、壞 token 401 + `WWW-Authenticate`;註冊(大小寫不敏感 409、弱密碼 422、格式 400)、登入(錯誤密碼與不存在帳號同為 401)、refresh 輪替 → 舊 token 重放 401 且家族撤銷、登出後 401、登出冪等;§6.1.4 (a)(b)(c) 經 HTTP 逐數字相符(買方 `8004 / 1200`、`0.3992 ETH`、賣方 `795.204`、fee `0.796 USDC` / `0.0008 ETH`、取消後 `9204`);201 / 200 / 422 / 400 / 404;拒單 `insufficient_balance`、`invalid_price_tick` 為 201 + rejected;他人訂單 404;fills 雙方看到同一 `trade_id`;ledger entries 只含自己的 4 條 settle posting;depth / trades;API key 建立、簽 GET 與帶 query、簽 body、篡改 body 401、錯簽 / 過期時間戳 / 錯 secret / 未知 key / 壞 timestamp 皆 401、read key 下單 403、key 不能建 key 403、IP 白名單 403 / 200、撤銷後 401、他人 key 404;第 6 次登入 429 + `Retry-After ≤ 12`;審計計數;admin bootstrap 冪等且 role=admin 可登入。

**Phase 3b 學到的事**:oapi-codegen strict server 不驗 `minLength / minimum`,`client_order_id` 為空與 `depth?limit=0` 要自己處理(前者 400,後者退回預設值);jwx v3 的 `Get` 對陣列 claim 只接受 `[]any`;`gosec` G101 會把名字含 `Token` 的 Lua 常數當成硬編碼憑證,改名即可;fills 的 `order_id` 過濾若只看「該單參與的成交」會讓對手方探測任意 order id 是否與自己成交過。

## 14. Phase 3c 程式碼與 §5.2 / §6.6 / §7 的對應(拆分部署、事件契約)

3a 讓引擎在一筆交易內完成整條命令,3b 讓 HTTP 走得到引擎——但兩者都只在**同一個 process 內**成立。3c 讓 `docs/plan-v1.0.md` §5.2 的路徑在拆分部署下真的成立:`api`、`engine`、`admin` 各一個容器,命令走 NATS。

| 計畫 | 實作 | 備註 |
|---|---|---|
| 命令匯流排(§5.2 步驟 3) | `internal/cmdbus`:`Client` 實作 `trading.CommandBus`、`Serve` 是引擎端;subject `cmd.trading.{tenant}.{market}`;`role=all` 仍然是 in-process(零延遲、不鑄 token) | 放在 `trading` 之外是刻意的:`trading` 只定義介面,不該相依 NATS 與 `auth` |
| 錯誤語意跨容器保持不變 | wire 的 `error.kind` 把 `trading` 的 sentinel 原樣還原(`invalid_request`、`market_not_found`、`order_not_found`、`client_order_id_mismatch`、`unavailable`、`rejected`);`remoteError` 同時保留原訊息與 `errors.Is` | 沒有這層,3b 精心對應的 404 / 422 / 503 會在跨容器時全部塌成 500。這是本套件最重要的一件事,單元測試逐一往返每個 sentinel |
| 引擎不可達 | `nats.ErrNoResponders`、逾時、caller 取消 → `trading.ErrEngineUnavailable` → 503(而不是 500) | 503 是誠實的:引擎可能仍會套用該命令,客戶端以同一個 `client_order_id` 重試即可 |
| 身分傳遞(§14) | `api` 以同一把 Ed25519 私鑰鑄 5 分鐘、`aud=internal` 的 JWT(claims 取自已驗證的 `Principal`);`engine` 以 JWKS 驗證、要求 `trade` scope、並比對 **token 的 account 與命令的 account** | 引擎信任 token,不信任 body 裡的 `account_id`。整合測試涵蓋:拿 A 的 token 動 B 的錢、read-only token 下單、無 token、過期、`aud=exchange` 全部被拒且餘額不變 |
| JWKS 取得 | `auth.RemoteVerifier`:**惰性**抓取(第一次驗證時才抓),遇未知 kid 最多每 30 秒重抓一次(輪替),抓取失敗時沿用舊金鑰 | 不能在啟動時抓:compose 沒有 `engine → api` 的 `depends_on`,啟動時抓會讓引擎相依於 api 先起來 |
| 訂閱策略 | 一個 wildcard 訂閱 `cmd.trading.{tenant}.*` + queue group `engine` | 偏離計畫的「每個 market 一個訂閱」:reload 新增市場後不需要補訂閱,而 subject 版面仍是每市場,日後要分片也不必改客戶端 |
| 消費端(§7.3 處理型) | `eventbus.Subscribe`:durable consumer + 顯式 ack + `NakWithDelay` 退避;無法解碼的訊息直接 ack(毒訊息不該永久卡住 consumer) | 扇出型(Phase 6 的 WS)**不可**用它:共用 durable consumer 是 work queue,每個副本只會收到一部分事件。這點寫進 `docs/events.md` |
| 市場熱載入(§6.6) | `engine-registry` consumer 訂 `EX_REGISTRY` 的 market/asset/fee_schedule,呼叫 `Engine.Reload`;runner 每個命令重讀 `registry.Cache`,所以狀態改變對**下一張單**就生效 | 突發合併不用 timer:每則事件檢查「是否已有一次 reload 在它發布之後開始」,是就 ack、否則 reload。這讓 ack 保持誠實——reload 失敗會 nak 重送 |
| `PUT /admin/v1/markets/{symbol}/status`(§7.4) | 一筆交易內完成 registry 寫入 + 審計 + outbox `market.updated`;狀態沒變就什麼都不寫 | 路徑參數用 **symbol** 而非 §7.4 寫的 `{id}`,與 public 的 `GET /v1/markets/{symbol}` 一致 |
| api role 的 registry 快取 | 沒有同進程引擎時,以 `REGISTRY_REFRESH_INTERVAL`(預設 30 s)重讀 | api 不能自己開 durable consumer:1..n 副本會把事件分掉。引擎才是判斷市場狀態的地方,api 的快取只影響 `delisted` 的擋單 |
| 事件契約(§7.1、§7.2) | `api/events/v1/` envelope + 8 個型別的 JSON Schema;`docs/events.md` 全 catalog(未實作標 planned);`test/contract` 以 golden 檔鎖住序列化並用 schema 驗證,另檢查「schema 檔集合 == 已發布事件型別集合」 | `reject_reason` 刻意留成開放字串:新增一個理由不該讓消費者掛掉 |
| 多容器 E2E(§13.3) | `scripts/e2e.sh`(CI job `e2e` 與 `make e2e` 共用):compose `infra + app` 起 api/engine/admin 各一容器 → `exchangectl e2e` → kill -9 引擎後訂單簿一致 → 停牌後下一張單被拒 | api 與 engine 不同 process,交易只能走 NATS,**e2e 通過本身就是命令匯流排的證明** |

**Phase 3c 學到的事**:`--admin-url` / `--admin-key` 是 `admin` 與 `e2e` 子命令的 flag,不是 root flag,所以 e2e 腳本改用 `EXCHANGE_*` 環境變數才能一個前綴通用(這個 bug 是把腳本實際跑一次才發現的);`TimeInForce` 零值序列化成 `""`、反序列化回 `GTC`,語意與 `effectiveTIF()` 一致所以跨線安全;JetStream 的 durable consumer 對扇出是錯的選擇,契約文件必須寫清楚,否則 Phase 6 會踩到;`audit.audit_events` 是唯一一張「寫入者只有 INSERT、沒有 SELECT」的表,而 `INSERT … RETURNING` 需要 SELECT,所以它的 insert 不能有 `RETURNING`——既有測試一律以 `ex_all`(擁有全部角色的權限)連線,這讓缺陷一路躲到「一個角色一個容器」才現形,因此新增 `TestAPIRolePrivileges` 以 `ex_api` 連線跑註冊與登入,並反向斷言它讀不到審計軌跡。

---

## 15. Phase 4a-1 程式碼與 §6.4.1 / §14 的對應(HD 金鑰與充值地址池)

Phase 4 分四段:4a 充值(再拆 4a-1 金鑰與地址、4a-2 掃描與入帳)、4b 提現、4c 歸集與對帳、4d Sepolia。本節是 4a-1:**完全不碰鏈**,只處理金鑰與地址。

| 計畫 | 實作 | 備註 |
|---|---|---|
| 預生成地址池(§6.4.1) | `chain.deposit_addresses` + `hdwallet.Pool.Ensure`(signer 每 `WALLET_ADDRESS_POOL_INTERVAL` 補到 `WALLET_ADDRESS_POOL_MIN`) | 權限就是設計:`INSERT` 只給 `ex_signer`,`ex_api` 只有 `UPDATE (account_id, assigned_at)` 的**欄位級**授權,所以 api 能認領一列但改不了地址;無人有 `DELETE` |
| `api` 不碰金鑰(§6.4.1、§8) | `internal/chain`(指派)與 `internal/chain/hdwallet`(派生)分成兩個套件,`.golangci.yml` 的 `chain-assignment` 規則禁止前者 import 後者 | api role 只連結得到前者 |
| 指派 SQL | `ClaimDepositAddress`:`FOR UPDATE SKIP LOCKED` 的單一 UPDATE 語句;先查該帳戶既有地址所以冪等 | 競態:同帳戶兩個首次呼叫都會嘗試認領,`(tenant, chain, account_id)` 的 unique index 只讓一個過,輸的那個**讀回贏家的地址**而不是報錯——單一語句失敗不會動到任何列,所以池子不會漏 |
| 派生路徑(§14) | `m/44'/60'/0'/0/{i}` 充值、`m/44'/60'/1'/0/0` 熱錢包;index 由 `chain.deposit_address_index_seq` 明確取號(signer 得先知道 index 才能算地址,所以不能用欄位 DEFAULT) | 拒絕 ≥ 2^31 的 index:那與 hardened index 0 是同一個 32-bit 值,會讓兩列派生出同一個地址 |
| 種子儲存(§14、ADR-0007) | `secrets/keystore/hd-seed.json`:scrypt(N=2^18)+ AES-256-GCM;header(版本、KDF 參數、cipher)綁進 GCM 的 AAD | 有 AAD 才擋得住「改小 N、留著 ciphertext」;`validate()` 另外把 N 上限鎖在 2^20,免得有人塞 2^30 讓 signer OOM |
| `exchange keys import-mnemonic` | 讀助記詞 → 先派生熱錢包確認種子可用 → 才寫檔;印出熱錢包地址 | 印出來是為了對帳:`gen-dev-secrets.sh` 用 `cast wallet address` 算同一條路徑寫進 `HOT_WALLET_ADDRESS`,`scripts/e2e.sh` 再拿 signer log 裡的 `hot_wallet` 比一次——**兩套獨立的 BIP-44 實作必須同意** |
| `GET /v1/deposit-address`(§7.4) | `internal/api/chain_handlers.go`;帳號一律取自已驗證的 principal | 未知資產 404、停用或非本鏈資產 422、**池空 503(不是 500)**;一個地址服務該鏈上所有資產 |
| 不記錄金鑰(§14) | `Wallet` 的 `LogValue` / `String` / `GoString` 都回 `[redacted]`;config 只記 `wallet_keystore_passphrase_set` | `scripts/e2e.sh` 結尾以 `.env` 與 `secrets/dev-mnemonic.txt` 的**已知字串**掃所有容器 log;計畫明講不能用「不含 0x + 64 hex」這種斷言,因為 tx hash 本來就長那樣 |

**驗證(`test/integration/chain_test.go`)**:`Ensure` 補到 min 且重跑補 0;8 個併發帳戶拿到互不相同的地址;同帳戶 6 個併發首次呼叫拿到同一個地址且 DB 只有一列;池空回 `ErrPoolEmpty`、補池後立刻可用;以 `ex_api` 連線能認領、`INSERT` 與改 address 都是 42501、`ex_all` 也不能 `DELETE`;HTTP 層 7 個子測試涵蓋 401 / 404 / 400 / 503 與「ETH 與 USDC 同一個地址」。

**派生正確性有兩個獨立 oracle**:anvil 啟動時會印出 `m/44'/60'/0'/0/` 的私鑰,本套件對同一助記詞算出的三把金鑰與 CI e2e job 裡 anvil 印的完全相同;另外標準測試向量 `abandon … about` 的前三個地址也與公開值相符。

**Phase 4a-1 學到的事**:寫測試時抓到兩個真缺陷——`cosmos/go-bip39` 的 `IsMnemonicValid` **不驗 BIP-39 checksum**(只看字數與字在不在字典裡),所以一個字打錯會靜默派生出完全不同的錢包,必須改用 `NewSeedWithErrorChecking`;`DepositPath` 原本接受 hardened 範圍的 index,會讓兩列共用一個地址。另外兩件事跟功能無關但值得記:生產用的 scrypt 參數(N=2^18)讓一個走 CLI 的測試把 `make test` 從 15 秒拉到 25 秒,所以 KDF 成本做成可注入、happy path 移到 `internal/app`;`ecdsa.PrivateKey.D` 在 Go 1.26 已 deprecated,想「抹除私鑰」反而可能產生無效金鑰,與其做安全劇場不如老實承認 Go 做不到並把力氣放在「金鑰不離開套件、不進 log」。

---

## 16. Phase 4a-2 程式碼與 §6.4.1 / §7.2 的對應(掃描、確認數、reorg、入帳)

4a-1 讓使用者拿得到地址,4a-2 讓匯進去的錢被看見。本節把第 4 節那份設計變成程式。

| 計畫 | 實作 | 備註 |
|---|---|---|
| 冪等鍵 `(chain_id, tx_hash, log_index)` | `chain.deposits` 的 UNIQUE;原生 ETH 用 `log_index = -1` | 依 0001 的約定把 `tenant_id` 也納入 key |
| 確認數 = head − block + 1 | `Scanner.advance`;門檻取自 `registry.assets.required_confirmations`,config 只是沒設定時的後備 | anvil = 1,所以偵測到的同一輪就入帳 |
| 原生 ETH | `BlockByNumber(full)` 比對 `tx.To ∈ 受控地址`,**再查 receipt** | log 只會出現在成功的交易裡,但交易本身不管成敗都會在區塊裡——out-of-gas 的轉帳一毛都沒動 |
| ERC-20 | `FilterLogs(合約, topic0=Transfer)` 後在記憶體比對 `topics[2]` | 不把地址塞進 topic 陣列(§6.4.1);`DecodeTransfer` 只接受標準版面,非標準的回 error 而不是猜 |
| reorg 回退 | `reconcileTip` → `commonAncestor` → `rewind`;`chain.blocks` 是深度 `ETH_BLOCK_RING_DEPTH` 的環 | 找不到共同祖先就停下來報錯,不猜 |
| 重現走 UPDATE | `Scanner.record` 先 `SELECT … FOR UPDATE`,命中就 `UpdateDepositSighting` | §6.4.1 點名的陷阱:INSERT 會撞 UNIQUE 被當重複,那筆充值就**永遠不會入帳** |
| 入帳(§6.1.4 d) | 一筆交易內 `ledger.Credit`(source `custody_deposit_addresses`)+ 狀態 + outbox;冪等鍵 `deposit:{chain}:{tx}:{log}` | 崩潰後重放靠 `journal_entries` 的 UNIQUE 擋掉 |
| 入帳後深度 reorg | 掃描器**不動**已 credited 的列 | 錢可能已經花掉;自動反向分錄會撞 `balances` CHECK。走人工(`docs/runbooks/reorg-alert.md`) |
| genesis 檢查(§6.4.1 步驟 5) | `Scanner.Start` 比對 `chain.chain_state.genesis_hash`,不符或 `head < cursor` 就**拒絕啟動** | anvil volume 被清掉但 Postgres 還記得舊游標,是每個開發者遲早會遇到的 |
| 事件契約(§7.2) | `deposit.detected|credited|orphaned|dropped|reversed`,五種共用一個 payload | `dropped` 在 §7.2 catalog 漏了,以 §6.4.1 為準;`confirmations_updated` 刻意不發 |
| 指標(§15) | `chain_head_block`、`chain_last_scanned_block`、`chain_scanner_lag_blocks`、`chain_reorgs_total`、`chain_unreadable_transfers_total`、`deposits_credited_total{asset}` | |

**驗證分工**:`test/integration/deposit_scripted_test.go` 用**腳本化的鏈**跑 reorg 形狀、失敗交易、ERC-20 scale、以及「已入帳的充值不會被 reorg 收回」;`deposit_test.go` 用 **anvil testcontainer** 跑同一套確認數與 reorg 故事(沒有 docker 就 skip,CI 會跑);ERC-20 對**真的部署出來的** MockUSDC 的路徑歸 `scripts/e2e.sh`。

**Phase 4a-2 學到的事**:為了能在沒有 docker 的機器上真的執行 reorg 邏輯,把節點抽成 `deposit.Chain` 介面——這不是為抽象而抽象,是因為替代方案是把整包最容易寫錯的東西**沒跑過就推上去**,而 3c 的 e2e 已經示範過那要付四輪的代價。腳本化的鏈立刻抓到兩個真缺陷:**等高的 reorg 完全看不見**(`Tick` 只在 `cursor < head` 時對帳,所以把區塊 N 換成另一個區塊 N 之後,來自被丟棄分支的充值看起來還是真的;鏈變短更是永遠不會發現),以及 `orphaned → dropped` 這個轉換**違反它自己的 CHECK**(`orphaned_at_block` 被雙向綁在 `orphaned` 狀態上,但被 drop 的充值必須留著當初是在哪一塊被孤立的)。另外 `money.Amount` 是 NUMERIC(36,18),放不下接近 2^256 的 uint256,所以 `FromWei` 對這種值回 error 而不是截斷——一個壞掉或惡意的 token 真的會發出那種 Transfer。

## 17. Phase 4b-1 程式碼與 §6.4.2 / §14 的對應(提現到「鎖定資金」為止)

4a 讓錢進得來,4b 讓錢出得去。這一半不碰鏈、也不碰任何私鑰:api 記錄請求,chain 依政策判定並在帳本鎖定資金,admin 審核政策擋下來的。簽名、廣播、追蹤由 signer 從 `funds_locked` 接手。

| 計畫 | 實作 | 備註 |
|---|---|---|
| 冪等(`Idempotency-Key`) | key 存在 `chain.withdrawals` 上,配 UNIQUE `(tenant_id, account_id, idempotency_key)` + `request_hash` | **刻意偏離** §6.4.2 的「key → response 快取 + 24h TTL」,見下 |
| 基本驗證 | `Service.Create`:資產可提、位址格式、`amount ≥ min_withdrawal`、available 預檢 | 預檢是善意而非權威——真正的權威是後面的 `Hold` |
| `requested → policy_check → …` | `Worker.decide` + `policy.Basic.Withdraw` | 只有「凍結、資產不可提、低於最低額」直接 reject;**所有限額超標一律進 `pending_review`** |
| 每日限額 | `SumWithdrawnSince` + 滾動 24 小時 | 用滾動視窗而非日曆日:午夜前後兩分鐘可以提兩天的量 |
| `pending_review → approved` | `Reviewer.Review`(admin role) | 核准**不動錢**,只是標記給 chain worker |
| `approved → funds_locked` | `Worker.lockFunds`:`ledger.Hold("hold:withdrawal:{id}")` + 狀態 + outbox,**同一筆交易** | 兩半分開都是錯的:只有 hold 會鎖住一筆永遠不前進的提現;只有狀態會讓 signer 花掉帳本仍稱為 available 的錢 |
| 餘額不足 | `failed(insufficient_balance)` | 請求與鎖定之間餘額會動,所以這是正常路徑而非例外 |
| 事件(§7.2) | `withdrawal.requested`(api 發)+ `withdrawal.state_changed`(其餘每次轉移) | 不做「一狀態一型別」:狀態機還會長(signer 再加三個),而訂閱者本來就全訂 |
| 權限(§14) | api 只有 INSERT;chain 可寫 `status/failure_reason/hold_entry_id`;admin 可寫 review 三欄 | 沒有任何角色同時能「核准」與「執行」 |
| 指標(§15) | `withdrawals_transitions_total{asset,status}`、`withdrawals_pending_review` | 後者值得告警:等人審的提現就是等錢的使用者,系統其他部分不會注意到 |

**一處刻意偏離**:§6.4.2 描述的冪等是「存 key → request hash → response,TTL 24 h」。實作改成把 key 放在提現列上、走 UNIQUE index,重放回傳那一列(200),同 key 不同內容回 422——與 3a 的 `client_order_id` 同一套語意。兩個好處:**沒有 24 小時後同一把 key 會悄悄再開一筆提現的視窗**,而且重放拿到的是提現的**當前狀態**,不是第一次回應的凍結副本。

**兩個只有拆分部署才看得見的權限缺口**(這已經是第三、第四個同類):`withdrawal.requested` 與 INSERT 同一筆交易寫進 outbox,而 account-scoped 事件要 `NextAccountSeq`(`UPDATE … RETURNING`),所以 api 需要 `UPDATE (next_seq) ON ledger.accounts`;政策依 KYC 等級,而等級在帳戶背後的 user 上,所以 chain 需要 `SELECT (id, kyc_level) ON auth.users`。兩個都是欄位級:api 只能推進序號、chain 只能讀等級,讀不到密碼雜湊、TOTP 種子或 email。`TestWithdrawalRolePrivileges` 用真的 `ex_api` / `ex_chain` / `ex_admin` 連線跑完整條路徑釘住這件事,並且**在補上 grant 之前先確認它會紅**。

**Phase 4b-1 學到的事**:測試預期「粉塵金額應被拒絕」卻看著它通過,才發現兩個資產的 `min_withdrawal` 一直是 0——因為在提現政策出現之前沒有任何東西會讀它。真鏈上這代表接受一筆 gas 比金額還貴的 1 wei 提現。`min_deposit` 則刻意留 0:目前沒有程式碼執行它,而**一個沒有程式碼遵守的 registry 值比沒有這個值更糟**。

---

## 18. Phase 4b-2 程式碼與 §6.4.2 / §6.6 的對應(簽名、廣播、追蹤、人工處置)

4b-1 把提現送到 `funds_locked` 就停住;這一半接手,把它變成鏈上一筆真的交易,並在確認後結清帳本。這是整個系統第一次有東西「花掉」錢。

| 計畫 | 實作 | 備註 |
|---|---|---|
| `funds_locked → signed` | `Worker.sign`:先 **pin nonce**、再簽、最後把 nonce + `raw_tx` + 狀態寫在同一筆交易 | 簽名本身在交易外(那是跨行程網路呼叫,握著列鎖等它會讓所有提現排在最慢的 signer 後面) |
| 簽名不可重複(§6.6) | `chain.signing_log` UNIQUE `(tenant_id, kind, ref_id, attempt)`,只有 `ex_signer` 能 INSERT,**沒有任何角色能 UPDATE / DELETE** | `attempt` 是計畫沒寫的一欄:沒有它,加價重送根本簽不出來 |
| 簽的是「意圖」不是交易 | `signer.Request{Kind, RefID, To, Asset, Value, …}`,signer 自己查資料庫、自己組交易 | **刻意偏離** §6.6 的「呼叫端傳 unsigned tx」,見下 |
| `signed → broadcast` | `Worker.broadcast`:先送鏈,再 `hold → pending_withdrawal` + 狀態,同一筆交易 | 順序有意義:鏈上有交易而帳本沒分錄 = 錢無聲離開;分錄有而交易沒送出 = 下一輪重送同一份 bytes 就修好了 |
| `broadcast → confirmed` | `Worker.confirm`:`pending_withdrawal → custody_hot`,**外加一筆 gas 分錄** | 兩筆而非一筆:金額是提現自己的資產,gas 永遠是鏈的原生幣。合成一筆會得到一筆各資產不平衡的分錄 |
| nonce 管理(§6.4.2) | `internal/chain/hotwallet.Manager`:三條啟動規則、`Allocate`、`Recycle`、`fill` | `pending > dbNext` **拒絕啟動**:鏈上有這個資料庫沒配過的交易,代表別人也拿著這把鑰匙 |
| nonce 缺口 | 0 值自轉,先寫 `chain.nonce_fills` 再送出 | 寫在前:送出去但沒記錄的填補,下次啟動會被當成缺口再填一次,而那個 nonce 已經被鏈吃掉了 |
| 重送(§6.4.2) | `maybeReplace`:超過 `ETH_REPLACE_AFTER` 就 `Bump(10 + 10×replacements)%`、同 nonce、`attempt = replacements + 1` | 從**當下**的建議價加價,不是從原本的:市場已經動了,協定只要求贏過真的送出去的那筆 |
| `MAX_REPLACEMENTS` | 用完就停,狀態留在 `broadcast`,`withdrawals_stuck_total` +1 | 跟塞住的 mempool 無限對賭不是策略,是等人 |
| `resolve(bump/cancel_nonce/refund/retry)` | admin 寫四個 `resolve_*` 欄,chain role 的 `ApplyResolutions` 執行 | 見下:這是這個 PR 最大的一處設計決定 |
| `resolve(cancel_nonce)` 的退款 | `settleCancelled`:**取代交易被挖出來之後才退**,而且是 `Post` 不是 `Release` | 兩件事都是 E1 那條勘誤:錢在廣播時就離開 hold 了;而在取代交易確認前,原交易仍可能贏 |
| `failed(on_chain)` | gas 記帳,金額**留在 `pending_withdrawal`** | 交易所已經不欠使用者這筆 available,也還沒付出去。要變成哪一邊是人的決定 |
| 權限(§14) | signer:SELECT 提現 + INSERT signing_log,**不能改提現任何一欄**;chain:交易欄 + 清 resolve 請求;admin:只有四個 resolve 請求欄 | 握著鑰匙不等於能宣告錢已送出 |
| 指標(§15) | `withdrawals_replacements_total`、`withdrawals_stuck_total`、`withdrawals_resolutions_total` | `stuck` 值得告警:它的定義就是「機器放棄了,等人」 |

操作步驟在 [`docs/runbooks/stuck-withdrawal.md`](runbooks/stuck-withdrawal.md)(§9 列的五份 runbook 之一,提前寫,因為它涵蓋的是這個 PR 第一次做出來的人工處置路徑)。

**設計決定:`resolve` 是「請求」而不是「動作」。** 計畫把 `resolve` 畫在 admin 那一側,直覺會寫成一個同步端點。但四個動作沒有一個做得到:`bump` 與 `cancel_nonce` 要簽名和節點,`refund` 與 `retry` 要 `chain.nonce_fills` 的 grant 與提現的狀態欄——admin 角色一個都沒有。**而且不該有**:能寫 `tx_hash` 的角色可以讓一筆提現看起來已經送出,卻沒有任何東西被簽過,那正是 §6.4.2 這條分工要防的事。所以 admin 寫四個請求欄,chain role 在自己的 tick 上執行。附帶好處是操作員的決定會存活過 chain role 的重啟,而不是死在一個 HTTP 請求裡。合法性檢查兩次:當下(操作員還盯著螢幕時給 409)、執行前再一次(廣播中的提現隨時可能確認)。

**設計決定:signer 簽的是意圖,不是交易。** §6.6 的草圖是呼叫端組好 unsigned `types.Transaction` 交給 signer 驗證。ERC-20 讓這條路變窄:代幣提現的 `to` 是合約位址,真正的收款人埋在 calldata 裡,所以「驗證」等於「解碼 calldata 然後相信這次解碼」。改成 signer 自己從已核對過的欄位組交易,那一步就不存在了。

**pin nonce 的理由是一個沒有測試會自己發現的崩潰視窗。** 原本的順序是 allocate → sign → record。行程若死在 sign 與 record 之間,重啟後會配到**另一個** nonce、用 `attempt = 0` 再問一次 signer——而 signing log 會永遠拒絕它,提現就此卡死。現在 nonce 先寫在提現列上並提交,重試問的是同一個意圖,signer 回傳它已經簽出來的那筆交易(`raw_tx` 存在 signing log 裡正是為此)。`TestWithdrawalRecoversFromACrashBetweenSigningAndRecording` 釘住這件事。

**`custody:hot` 會是負的,而且是誠實的。** 錢從 `custody:deposit_addresses` 進來、從 `custody:hot` 出去,中間的歸集是 4c。在那之前熱錢包付出去的比收進來的多,`custody_hot` 這個資產帳戶自然為負。這不會炸:`ledger.balances` 的 `available >= 0` CHECK 只作用在使用者的桶上(`aggregateDeltas` 跳過 house bucket),house 餘額由分錄推導,試算表照樣平衡。**4c-1 之後這個缺口會被歸集補起來**(第 19 節)。

**又一個只有拆分部署才看得見的權限缺口**(第五個了):`RequestWithdrawalResolve` 除了四個請求欄還會把 `resolve_error` 清成 NULL——新的請求不該掛著上一次的失敗訊息——而 grant 裡沒有這一欄。手寫 UPDATE 四個欄位的測試會過,真正的查詢在 `ex_admin` 上是 42501。教訓與 3c、4a-2、4b-1 完全一樣:**權限測試要跑真正的那一句 SQL,不是它的近似**。

**Phase 4b-2 學到的兩件事**,都在沒有測試碰過的程式碼裡:

1. `evm.ToWei` 用「小數位數」而不是「小數的值」判斷精度,於是從 `NUMERIC(36,18)` 讀回來的金額對任何非 18 位的資產都顯得過度精確——**這會拒絕掉每一筆 ERC-20 提現**。50 USDC 從資料庫回來是 `50.000000000000000000`,而十二個零不是丟失的精度。
2. `UpsertHotWallet` 每次啟動都覆寫存起來的位址,於是「這是另一個錢包」那條拒絕永遠不可能觸發——拿錯種子的部署會繼續照著陌生人的計數配 nonce。改成不覆寫,那個比對才有東西可比。

---

## 19. Phase 4c-1 程式碼與 §6.4.3 / §6.1.4(f) 的對應(歸集)

4a 讓錢進來、4b 讓錢出去,但兩邊從來沒有相接:充值落在每個帳戶自己的地址上,提現從熱錢包出去。從 4b-2 開始 `custody:hot` 每一筆提現都更負一點——熱錢包一直在付一筆不是從它這裡收進來的錢。歸集就是把這個缺口接起來的那一步。

**歸集永遠不動使用者餘額。** 它在兩個 house 帳戶之間搬錢並記 gas;被清空地址的那個帳戶,餘額一分都不會變。這是每一個測試最後都會斷言的事。

| 計畫 | 實作 | 備註 |
|---|---|---|
| 掃 `deposit_addresses` 餘額 | `Worker.plan`,每個 tick 一次 | 自己的時鐘(`ETH_SWEEP_INTERVAL`,預設 60s):沒有人在等歸集,而每次掃描是「地址數 × 資產數」次餘額查詢 |
| ETH:`balance ≥ sweep_threshold` → 轉 `balance − gas` | `planOne`,gas 用**費用上限**而非當下價格編列 | 見下:這不是保守,是「不這樣做就會卡死」 |
| ERC-20 兩段式 | `fundGas` → `trackGasFunding` → `send` → `track` | 只收過代幣的地址一滴 ETH 都沒有,付不起任何交易——這就是要兩筆的唯一理由 |
| `requested → gas_funded → broadcast → confirmed | failed` | `chain.sweeps` 的狀態機 | `gas_funded` 只有代幣會經過;原生資產從 `requested` 直接簽 |
| nonce 取 `PendingNonceAt(address)` | `signSweep`,但**簽名前先釘在列上** | 計畫寫「直接取」;直接取在崩潰視窗裡會拿到不同的 nonce,見下 |
| 分錄(§6.1.4 f) | `confirm`:資產 `custody:deposit_addresses → custody:hot`;gas 由**實際送出交易的那個地址**付 | 代幣歸集的 gas 記在 `custody:deposit_addresses`(地址自己付),補 gas 那筆記在 `custody:hot`(熱錢包付)。這個不對稱正是它們必須是兩筆分錄的原因 |
| 簽名(§6.6) | `signer.KindSweep` / `KindGasFund` | **第一次有熱錢包以外的金鑰簽東西**:歸集是充值地址自己送出的 |
| 權限(§14) | chain 擁有整個機器;signer 只有 SELECT;admin 只有讀 | 和提現不同,這裡沒有「授權」這一步:歸集是交易所在自己的兩個科目之間搬自己的錢,沒有東西需要第二個角色核准 |
| 指標(§15) | `sweeps_planned_total`、`sweeps_confirmed_total`、`sweeps_failed_total{reason}` | `failed` 值得告警:充值堆在熱錢包花不到的地址上,第一個看得見的症狀會是「一筆提現付不出來」 |

**兩條規則決定什麼可以歸集**,兩條都是為了讓 `custody:deposit_addresses` 誠實,而不是為了保守而保守:

1. **有還沒入帳的充值的地址,整個跳過。** 現在歸集會和「即將發生的入帳」對撞:帳本還沒記錄這筆錢到過那個地址,卻要記錄它離開了。
2. **歸集金額上限是帳本真的入過帳的數。** 鏈上餘額可以合法地更高——掃描器看不到的合約內部轉帳(§4 列為不做,但沒有東西阻止它發生),或還在確認的充值。把超出的部分掃走,等於讓 `custody:deposit_addresses` 為一筆它從來沒收到的轉帳背書。多出來的錢就留在鏈上,4c-2 的對帳會看到它——那才是它該出現的地方。

**gas 用費用上限編列,不是用當下價格。** 原生歸集要留下足夠付自己的錢。如果 base fee 在送出後上漲,一筆變得付不起的交易**沒辦法加價**——加價需要的餘額正是這個地址沒有的。編列高一點會留下一點灰塵,那是「永遠不會卡死」的價錢。

**nonce 一樣要先釘。** §6.4.3 寫「nonce 直接取 `PendingNonceAt(address)`」。單獨這樣做會踩到 4b-2 找到的同一個崩潰視窗:簽完但還沒記錄就死掉,重啟後 `PendingNonceAt` 可能已經算進了那筆進了 mempool 的交易,於是重試會用**另一個** nonce 簽第二筆——兩筆有效交易,一筆錢。所以 nonce 在簽名前就寫進 `chain.sweeps` 並提交,重試問的是同一個意圖。充值地址不需要 nonce 缺口管理(它只有 sweeper 在用,沒送出去就什麼都沒發生),但釘住這件事還是要做。

**Phase 4c-1 學到的事**:把腳本鏈改成「真的記餘額、挖礦時真的搬動」之後,它立刻抓到 4b-2 的代幣提現測試一直在送熱錢包從來沒有的 USDC——測試通過,只因為假鏈不記帳。同一個改動也逼出 signer 的一個真 bug:`sweepTx` 只接受 `requested`,而代幣歸集是在 `gas_funded` 才簽的(地址要先拿到 ETH 才付得起),於是**每一筆代幣歸集都會被自己的 signer 拒絕**。教訓和前面幾次同一個方向:假的東西越像真的,越早撞到真的問題。

**第三件事是 CI 上的 anvil 抓到的,而且它抓到的不是測試的問題**:testcontainer 的 anvil 是一條乾淨的鏈,上面沒有 compose 才會部署的 MockUSDC,所以 `balanceOf` 打到一個沒有 code 的位址、回空資料、`evm.TokenBalance` 正確地報錯。真正的缺陷在 `Worker.plan` 怎麼處理這個錯誤:它把「一個資產讀不到」變成「整輪掃描失敗」,而且因為迴圈是「地址 × 資產」,同一個錯誤會對每個地址各印一次。放到真實部署,那就是「registry 有一列合約位址打錯,整條歸集看起來壞掉,連讀得到的 ETH 也被算進失敗」——而 ETH 其實一直收得好好的,那個錯誤訊息只會把人帶去錯的地方。

**第四件事來自 e2e,而且它是測試設計的錯不是產品的錯**:腳本原本先斷言「現在這個地址還有東西可以掃」,再去等歸集發生。但 sweeper 是自走的——充值一入帳它就開始動,而腳本要到很後面才走到那一步,那時地址早就空了。同一個誤解也讓提現那段去輪詢 `funds_locked`:那是機器一兩個 tick 就會離開的過渡狀態,從外面輪詢是在賭。教訓是:**一個持續自走的背景程序,腳本能斷言的是結果,不是過程**;要斷言過程就得能控制時間,那是整合測試的位置(`sweep_test.go` 用手動驅動 tick,每一支都精確斷言使用者餘額沒動)。e2e 現在只斷言持久的事實:有 confirmed 的歸集、地址被清空、試算表為零、`custody_deposit_addresses` 不為負。

同一段 e2e 還踩到第二個坑,它跟時間無關而是**表示法**:`GET /v1/deposit-address` 刻意回 EIP-55 checksum(使用者要貼進錢包,大小寫就是防打錯的校驗),而 `chain.*` 的資料表把位址正規化成小寫存,所以 admin API 與 container log 讀回來的都是小寫。腳本把這兩個直接比字串,於是「找不到這個地址的歸集」——兩邊各自都對,錯的是把它們當成同一個字串。跨層比位址前要先確定哪一邊正規化過;`Deposit.address` 的 schema 原本沒寫明大小寫,現在寫了,因為沒寫正是這個錯誤有機會發生的原因。

改法是把兩層迴圈對調(資產在外、地址在內),因為「這個資產讀不讀得到」是資產的性質,一輪問一次就夠,也才能把它當成一個單位跳過。讀不到就 log 一次、`sweeps_unreadable_total{asset}` 加一、換下一個資產,**不讓 tick 失敗**——sweeper 不知道那裡有多少錢所以不能收它,但這不該讓它連讀得到的資產也停手。可見性交給指標,不是交給一個會誤導的失敗。anvil 測試刻意**不**把 USDC 停用來閃過這件事,而是留著它並斷言「以太照樣收到了」,把這次 CI 紅的原因變成它自己的回歸測試。

---

## 20. Phase 4c-2 程式碼與 §6.4.4 / §6.4.3 的對應(鏈上對帳)

4c-1 刻意留了一個缺口:歸集金額的上限是「帳本真的入過帳的數」,鏈上可以合法地更高,多出來的留在鏈上。README 和 §19 都寫著「4c-2 的對帳會看到它」。這一節是那雙眼睛。

| 計畫 | 實作 | 備註 |
|---|---|---|
| `Σ ledger custody` vs `Σ 鏈上餘額` | `reconcile.Worker.Tick`,每個資產一列 | 未指派的池位**也算**:對帳要找的正是帳本不知道的錢,而掃描器同樣跳過池位,所以錢掉在那裡永遠不會入帳 |
| 差異 = 在途 ± 未入帳充值 | `uncredited` + `above_frontier` + `in_flight` 三個修正項 | 每一項都是**精確算出來的**,不是估的,所以容差是 0 |
| 差異超過閾值寫 breaks | `admin.reconciliation_reports` + `admin.reconciliation_breaks` | 沒有閾值:`NUMERIC(36,18)` 是精確的,gas 從 receipt 讀,兩個修正項也精確 |
| 發 `reconciliation.break_detected` | 邊緣觸發:出現或金額改變才發 | 一直存在的 break 留在報告與指標裡;每五分鐘重發一次只會教人設過濾器 |
| 熱錢包低於 `HOT_WALLET_MIN_ETH` 發 `alert.hot_wallet_low`(§6.4.3) | 同上,狀態記在 `chain.hot_wallets.low_alerted_at` | 記在資料庫不是記在行程裡:重啟不該把同一個沒變的狀況再喊一次 |
| worker 排程 | **跑在 `chain` role** | 見下 |
| `exchangectl admin reconcile` | 讀最新一份報告 | admin 沒有節點,量不到餘額,和它簽不了提現是同一條界線 |
| 指標(§15) | `reconciliation_diff{asset}`(符號)、`reconciliation_breaks`、`hot_wallet_balance{asset}`(量值) | 差異用符號的理由和 `ledger_trial_balance_diff` 一樣:要問的是「是不是零」,float64 對 18 位小數會編出不存在的位數 |

### 20.1 為什麼跑在 chain 而不是 worker

§6.4.4 的標題寫 worker,§7.2 的事件表也寫 worker。**這裡刻意不照做。**

對帳要讀鏈上餘額,而只有 `chain` role 撥號到節點(`internal/app/chain_role.go`)。compose 裡 `exchange-worker` 沒有 `depends_on: anvil`。為了照抄一個字而多養一條 RPC 連線與一組授權,買不到任何東西。`chain` 本來就握有節點、地址清單、`deposits/withdrawals/sweeps` 的在途狀態與 ledger 讀取權。

界線沒有變鬆:**產生報告的角色和顯示報告的角色仍然是兩個**,和提現「admin 記錄意圖、chain 動手」同一個形狀。`POST /reconciliation/run`(§7.4 列了)這一輪不做,因為 admin 量不到東西;要做也該是 4b-2 那種「記下請求、chain 執行」的樣子,不是一個同步端點。

### 20.2 恆等式,以及「同一條邊界」

設 `L` 為帳本的 `custody_deposit_addresses + custody_hot`,`C` 為在區塊 `B` 讀到的「全部充值地址 + 熱錢包」。把 `L` 拆成「只記了 ≤ B 的movement」的 `L_B` 加上「>B 的 movement 帶來的淨額」`Δ_above`:

```
L   = L_B + Δ_above
C   = L_B + uncredited − in_flight
⇒  diff = C − L + Δ_above − uncredited + in_flight = 0
```

- `uncredited`:`≤ B` 鏈上看得到、帳本還沒入帳的充值(狀態 `detected`/`confirming`)。鏈上合法地比較高。
- `Δ_above`(`above_frontier`):`> B` 的區塊上帳本已經記了的淨額——入帳的充值 `+`,確認的提現 `−(金額+gas)`,確認的歸集 `−gas`(資產在 `C` 的兩個端點之間移動,淨額 0),補 gas `−gas`,nonce fill `−gas`。
- `in_flight`:`≤ B` 已經挖出來、帳本還沒記的花費。**只有提現**會把價值移出被計算的那組地址;歸集、補 gas、nonce fill 的兩端都在 `C` 裡面,所以它們只花 gas。

**`B` 是哪一個區塊,是這一節最要緊的一行:**

```
B = min(head − required + 1, chain.scan_cursors.last_scanned_block)
```

第一項是掃描器(`scan.go:251`)、提現 worker(`broadcast.go:371`)和歸集(`run.go` 的 `track`)**三個都用的同一條規則**:各自算 `head − block + 1` 再和資產的確認數比,所以 `block ≤ head − required + 1` 就等於「已經記帳了」。少一個區塊會開出一個窗:帳本記了,餘額看不到。而且那個窗**三種 movement 裡有三種會讓 `diff` 變正**(確認的提現、確認的歸集、補 gas),所以它連「保守」都算不上——它會製造假的 break,而假的 break 比沒有對帳更糟。

第二項是因為**帳本對鏈的認識是掃描器的游標,不是節點的 head**。游標之上掃描器連 `chain.deposits` 列都還沒建,所以那裡的一筆充值是「鏈上看得到、帳本沒入帳、`uncredited` 也加不到」——無上限的正差,而且掃描器每落後一個 tick 就會發生一次。

**兩個 process 永遠不共用同一個 head**,所以光把算式對齊還不夠:對帳在 T 讀 head,歸集 worker 在 T+ε 讀到 head+1 並確認一筆挖在 `B+1` 的交易。這就是 `above_frontier` 存在的理由——它從那些列**已經記下來的 block_number** 把帳本在邊界之上做的事精確扣回來。為了讓它精確,這一輪補上了幾個原本沒記的地方:`FailSweep` 記 `block_number`,補 gas 那一腿有了 `gas_funding_block`,失敗的補 gas 走 `FailSweepFunding` 寫進 funding 那一對欄位——**成本和區塊必須成對**,把 funding 的成本配上一個沒發生過的 sweep 區塊,分類就是錯的。

`in_flight` 則是**對每一筆非終態的交易打一次 `Receipt`**,就是擁有它的 worker 自己會打的那一次。列上答不出來:成本要到記帳時才寫,而且沒有存 gas limit 可以當上界,所以任何從列推出來的數字都是估計值——而這個比較沒有容差可以吸收估計值。

### 20.3 兩個快照

鏈那一側靠**把餘額釘在 `B`** 解決:一個一個地址問要花時間,問到第三十個時鏈已經走了幾個區塊,一筆中途被挖出來的歸集會被「來源已經扣掉、目的地還沒加上」地數兩次。`eth_getBalance` 和 `eth_call` 都吃 block 參數,釘住幾乎不花錢(`evm.BalanceAt` / `TokenBalanceAt`)。

但 `BalanceAt` 吃的是**號碼不是 hash**,所以一輪讀到一半發生 reorg,答案會靜靜地混到兩條鏈。所以整輪讀之前與之後各取一次那些高度的 block hash,不一樣就**把這一輪丟掉**——沒有報告好過一份沒人能信的報告。

帳本那一側有兩個以上的查詢,而入帳是一個交易:它把金額同時移出 `detected/confirming` 並移入 custody。分兩次讀會看到金額**兩邊都沒有**(或都有),於是報出一個不存在的 break。所以整組帳本查詢跑在同一個 `REPEATABLE READ` 交易裡。

### 20.4 帳本外進來的錢:這是一筆真的 break

e2e 的熱錢包由 anvil 直接注資 100 ETH + 1,000,000 USDC,帳本完全不知道。第一次跑對帳,兩個資產都會出現百分之百正確的 break。

**不做特例把它藏起來。** §6.1.4(g) 早就寫明這類資金進出的科目是 `external`(「dev faucet、管理員調帳、對帳沖銷、Sepolia faucet 注資熱錢包」),所以補的是**把它記進帳本的路**:`ledger.AdjustHouse`(custody 一腿、`external` 一腿,帶 `Direction`)+ `POST /admin/v1/ledger/house-adjustments` + `exchangectl admin house-adjust`。只允許兩個 custody 科目——它們是「有一個別人可以匯錢進來的地址」在背後撐著的科目;`fee_revenue`、`gas_expense`、`pending_withdrawal` 都是本系統自己的分錄推導出來的,調它們不是記錄事實而是藏 bug。

方向不能省。第一個真的 break 很可能是**少**了錢(見 20.5),只能加不能減的調整補不回來。

e2e 因此走三步:開頭斷言差異是正的(證明偵測不是空話)→ 記下正好那個數 → 跑完整流程 → 結尾**一個字都不記**地回到零。第三步不是套套邏輯:第一步只沖掉了交易所接手之前就在那裡的錢,第三步證明中間每一分錢的移動——入帳、成交、兩筆提現、兩條歸集,以及這些燒掉的每一 wei gas——都被正確記了。

### 20.5 對帳還沒寫完就找到兩個真的缺陷

**一、nonce 補洞的 gas 從來沒進帳本。** §6.4.2 的缺口回收送一筆 0 值自轉吃掉一個 nonce。它移動不了任何東西,但**燒掉熱錢包的 ETH**,而 `internal/chain/hotwallet` 整個套件沒有 import `ledger`:簽名、寫列、送出、忘掉。`chain.nonce_fills` 甚至早就有一個 `status IN ('broadcast','confirmed','failed')` 欄位,但沒有東西會把它推離 `broadcast`——receipt 從來沒被抓過。每補一次洞,`custody_hot` 就比鏈上多一點,永遠不會修正。現在對帳每一輪開頭先追這些 receipt 並記 `debit gas_expense / credit custody_hot`。放在開頭而不是結尾:**系統自己造成的 break 是 bug,不是發現**。

**二、`gas_funding_amount` 會被記成一筆沒發生過的轉帳。** `FundSweepGas` 在釘 nonce 時就寫了金額,而那是**問 signer 之前**——因為簽完沒記就崩潰必須回來問同一個意圖。如果簽名失敗,列上就留著一個正的金額配 NULL 的 tx_hash。下一個 tick 發現地址現在自己付得起 gas,走了捷徑跳過補 gas,而 `markGasFunded` 的金額是**從列上讀的**:於是記了一筆熱錢包從來沒送出去的 ETH。同一條路徑還把釘住的 nonce 丟在那裡,變成之後每一筆熱錢包交易都要排在後面的洞。一次失敗的呼叫,一個永久的帳本錯誤加一個永久的 nonce 洞——而對帳會正確地、永遠地報這兩件事。

兩個都在這一輪修掉,各有一支先確認紅過的回歸測試。

### 20.6 對帳上線第一天就抓到一個真的漏洞

**補 gas 的那筆錢被當成使用者的充值入帳了。**

歸集代幣時,熱錢包會送一小筆 ETH 到充值地址讓它付得起自己的轉帳(§6.4.3)。在鏈上,那就是**一筆流入受監控地址的普通轉帳**——正好是掃描器在找的形狀。於是 `nativeSightings` 把它記成一筆充值並入帳:

- 使用者白得一筆交易所替他墊的 ETH;
- `custody_deposit_addresses` 為**同一筆移動記了兩次**——一次是 sweeper 把它從 `custody_hot` 搬過來,一次是掃描器把它當成從外面到達。

第二點正是 e2e 看到的:`ledger_total` 比 `chain_total` 高,高的數字精確等於那筆 funding。

這個缺陷 **4c-1 就上線了**,而 4c-1 的 e2e 全綠——因為那時候沒有任何東西拿帳本和鏈上比。它撐過了完整的 integration 套件、撐過了兩輪 e2e,直到 §6.4.4 上線的第一天。

**修法是把規則寫對,而不是把那筆交易列成特例**:充值是**從交易所外面**到達的錢。發送方是交易所自己的地址(熱錢包,或它控制的任何充值地址)就不是充值。原生與代幣兩條路徑套同一條規則——今天沒有代幣會這樣流動,但規則講的是錢從哪來,不是講目前剛好存在哪些路徑。

回歸測試同時釘住兩邊:交易所自己送的不入帳,**陌生人送到同一個地址的照樣入帳**。一個會把真充值吞掉的防呆比原本的 bug 更糟。

### 20.7 這一輪學到的事

**「保守一點」不是設計。** 邊界如果選 `head − required`(少一個區塊),直覺會說那比較安全。實際上三種 movement 裡有三種會因此讓 `diff` 變**正**,而正的差異在這個系統裡的意思是「鏈上有帳本不知道的錢」——最需要有人立刻去看的那一類。一個猜錯方向的保守設計,會把每一筆確認的提現都變成一次假警報。要知道方向,只能把四種 movement 各推一遍。

**假的東西越像真的,越早撞到真的問題**(第三次)。腳本鏈原本只記「現在的餘額」,那樣的話「釘在區塊 B 讀」和「讀 head」在測試裡看起來一模一樣——而那正是這一節整段算式存在的理由。給它加上每個區塊的餘額快照之後,把 `+1` 拿掉會讓五支測試變紅;不加的話一支都不會。

**一個檢查的價值,要看它抓到什麼,不是看它綠不綠。** 對帳寫完之後 CI 紅了兩輪,兩輪都是它在報一個**真的存在的缺陷**——而那個缺陷在它之前已經安靜地活了一整個 phase。如果當初為了讓 e2e 快點綠而給它一個容差,或者把補 gas 那筆列成例外,這個洞會繼續在那裡,而且是在「我們有對帳了」的假象底下。

**容差是設計上的懶惰。** 計畫寫「差異超過閾值」。真的把每一項都算精確之後,閾值就不需要了——而且更重要的是,一個有閾值的比較沒辦法證明自己是對的:低於閾值的錯誤永遠不會被發現,而閾值該設多少沒有人能回答。`NUMERIC(36,18)` 是精確的,gas 從 receipt 讀是精確的,兩個修正項也是精確的,所以零就是零。

---

## 21. Phase 4d-1 程式碼與 §6.4 / §12 的對應(把設定拿去對真鏈)

4d 的工作是把同一份程式指向 Sepolia,手動走一次全流程。動手之前先對真的節點做了讀取實測,結果是**四個只在真鏈上才會踩到的缺陷**——不是設定問題,是程式錯了,而且四個都因為同一個原因活到現在:anvil 的 base fee 是零、歷史從不剪、沒有任何東西會拒絕一筆交易。

| §6.4 的說法 | 程式(4d-1 之後) |
|---|---|
| 鏈身分守衛(§6.4.1 step 5) | `chain.chain_state` 存 `(anchor_block, anchor_hash)`;`deposit.Scanner.verifyAnchor` 比對。錨點 = `ETH_SCAN_START_BLOCK` |
| `MAX_FEE_PER_GAS` 停損 | `evm.Fees.Over` + `evm.ErrFeeCeiling`,四個呼叫點各自決定「等」是什麼意思 |
| `MAX_REPLACEMENTS` 後轉人工(§6.4.2) | `MarkWithdrawalReplacementFailed`:被拒絕的重送也算一次 |
| nonce 補洞(§6.4.2) | `hotwallet.Manager.WithMaxFee`,補洞同樣吃上限 |
| 確認數 6 切換(§12 4d) | `seed --confirmations` 讀 `ETH_REQUIRED_CONFIRMATIONS_DEFAULT`;registry 的值才是執行時生效的那個 |
| 每條鏈的門檻不同 | `seed --params`,覆蓋式 JSON;`deploy/seed-params/` |
| `compose.prod.yaml`(§11) | `deploy/compose/compose.sepolia.yaml`,用 §11 自己點名的 `!override` |

### 21.1 錨點:守衛問錯了區塊

守衛本身是對的——「這個資料庫是對著哪條鏈建的」必須在啟動時驗一次,不然一個被清空的 anvil 配上活著的 Postgres 會安靜地跳過中間每一筆充值。錯的是它把錨點釘在 **block 0**:那是啟動路徑上唯一一個深度沒有上限的讀取,也正好是剪過歷史的節點最不可能給你的區塊。

實測(2026-09-07,`ethereum-sepolia-rpc.publicnode.com`):同一個 `eth_getBlockByNumber("0x0")` 請求,幾分鐘之內回過正常區塊,也回過 `{"code":4444,"message":"pruned history unavailable"}`;block 1 與 block 500000 也各失敗過一次,而 head−5,000,000 正常。**免費公開 endpoint 是一池異質後端**,有些剪過歷史,所以「往回讀很深」是機率性的。

新的錨點是 `ETH_SCAN_START_BLOCK`——這個資料庫對這條鏈的視野從哪裡開始。它回答的是同一個問題(同高度、不同雜湊 = 不同鏈),深度卻是操作者自己選的,而且**不需要第二個設定**去和第一個吵架。anvil 把它留在 0,所以錨點還是創世,行為逐位元不變。

順手補上一個一直存在的洞:`ETH_SCAN_START_BLOCK` 只在沒有游標時被讀,所以在活著的資料庫上改它**沒有任何東西會發現**,而它決定的是「哪些區塊算已經掃過」。現在它被記下來了,改了會被拒絕——訊息裡帶著原本記的值,因為修法是改設定,叫人去 reset 會讓他為了一個打字錯誤丟掉一個資料庫。

### 21.2 停損不能是「夾低之後照送」

`Fees.CapAt` 的註解說超過上限的交易「waits instead」,`ETH_MAX_FEE_PER_GAS` 的欄位註解也這樣寫。**兩段都是假的。** 實作把 `FeeCap` 夾到上限然後照樣送出去。

在真的費用市場上這代表用一個市場已經走過的價格廣播:交易卡在 mempool,重送階梯拿它自己去修一個修不好的東西。更糟的是重送——夾低之後的「重送」不見得比原本那筆高 10%,節點會直接拒絕,而拒絕又不計數(21.3)。兩個缺陷疊在一起,結果是一筆提現永遠在重試而沒有人被通知。

換成 `Fees.Over` 之後,**決定權回到呼叫者**,因為只有它知道「等」在它那裡是什麼意思:

| 呼叫點 | 超過上限時 |
|---|---|
| 提現送出 | 留在 `funds_locked`,連 nonce 都還沒拿 |
| 提現重送 | 跳過這一輪,**而且不計數**——操作者的停損不該花掉使用者的重送額度 |
| 歸集(規劃與送出) | 這一輪不做。歸集是家務事,沒有人在等 |
| admin bump | 回一個明確的錯誤。他知道自己在做什麼,送一筆displace不了任何東西的交易只會騙他 |
| nonce 補洞 | 不送。缺口留著,下一輪再補 |

補洞那條刻意**不給豁免**,即使每一個後面的 nonce 都卡在它後面:一個對「費用尖峰時最可能被送出的那筆交易」網開一面的停損不是停損。而且什麼都沒損失——同一個上限已經讓每一筆提現和歸集停下來了,補洞要解開的那個佇列本來就沒有在動。

### 21.3 被拒絕的重送也是一次重送

```go
if err := w.chain.SendRawTransaction(ctx, res.RawTx); err != nil && !errors.Is(err, evm.ErrKnownTransaction) {
    return fmt.Errorf(...)      // ← 這裡 return
}
return inTx(ctx, w.db, func(tx pgx.Tx) error {
    ... ReplaceWithdrawalTx ... // ← replacements + 1 在這裡
```

節點回 `replacement transaction underpriced` 就走這條。結果:`replacements` 永遠是 0,`MAX_REPLACEMENTS` 從不觸發,`withdrawals_stuck_total` 從不動,沒有人被問。那筆提現每 `REPLACE_AFTER` 重試一次,只要行程還活著。

現在失敗路徑會記一次(`MarkWithdrawalReplacementFailed`)並重開視窗,但**不動 `tx_hash` 與 `raw_tx`**:被拒絕的那筆根本不在任何地方,寫它的雜湊會讓 tracker 去追一個永遠不會出現的東西,而它想取代的那筆還活在 mempool 裡,還是有可能上鏈的那一筆。

`evm.ErrUnderpriced` 從 4b-2 定義至今**沒有任何消費者**——`grep` 只找得到定義與回傳。它現在有了,而且和「節點掛了」分開記在 audit 裡。

### 21.4 這一輪學到的事

**註解會說謊,而且說謊的註解比沒有註解更貴。** `CapAt` 那兩段講「會等」的註解,是它活了兩個 phase 的原因:任何人讀到它都會相信這個旋鈕有它該有的語意,不會再去看實作。發現它的方式不是讀程式,是問「這個設定在 Sepolia 上會怎樣」——**把設定放到一個它真的會生效的環境裡去想**,是找出這一類缺陷唯一可靠的辦法。

**一個從不被觸發的分支等於沒有寫。** 費用上限、重送被拒、深度歷史讀取——三條路徑在 anvil 上**物理上不可能**發生:base fee 是零,節點不會拒絕重送,歷史永遠都在。它們不是測試沒寫好,是測試環境讓它們無法存在。這也是為什麼 4d 的實跑不能省:能證明這三條路徑的只有一條真的鏈。

**先量再改。** 這一輪每一個決定背後都有一次真的 RPC 呼叫:head 高度決定 `ETH_SCAN_START_BLOCK` 非設不可(11,651,503,batch 200 就是五萬八千輪),base fee 決定上限設 50 gwei 而不是 5,`eth_getLogs` 帶 address filter 能過決定掃描器不用改。唯一沒被量到就寫進去的東西是那些「照理說」——而 4c-2 那一輪已經證明過,照理說會漏掉真的問題。

**預設值不變,是這一輪最重要的約束。** 錨點、部署腳本、seed 參數三個改動都有「anvil 走預設值,行為完全不變」的設計,而唯一能證明它的東西是 `make e2e` 還是 9/9 綠。一個為了新環境而悄悄改掉舊環境行為的改動,會讓兩邊都變得不可信。

---

## 22. Phase 4d-2:實跑一次之後,量到什麼、又暴露了什麼

4d-1 把設定對準了 Sepolia,4d-2 是**一個人照著 `docs/runbooks/sepolia.md` 從頭走完一次**——領測試幣、部署 MockUSDC、開使用者、充值、歸集、兩條提現路徑、對帳歸零。走的人不是寫這份文件的人,而這正是重點:文件對不對,只有沒有背景知識的讀者能證明。

整趟橫跨 **189 個區塊(約 38 分鐘)**,鏈上留下七筆交易。

### 22.1 量到的數字

| 步驟 | 區塊 | gas | gas 價格 | 成本 (ETH) | 誰付的 |
|---|---|---|---|---|---|
| ETH 充值(faucet → 充值地址) | 11653761 | 21,000 | 2.977 gwei | 0.000062509 | faucet |
| USDC 充值(mint → 充值地址) | 11653755 | 51,178 | 1.037 gwei | 0.000053069 | 部署者 |
| ETH 歸集 | 11653781 | 21,000 | 1.093 gwei | 0.000022954 | 充值地址 |
| USDC 歸集:補 gas | 11653801 | 21,000 | 1.115 gwei | 0.000023422 | 熱錢包 |
| USDC 歸集:轉帳 | 11653820 | 29,669 | 1.005 gwei | 0.000029817 | 充值地址 |
| 提現(自動核可) | 11653923 | 21,000 | 1.060 gwei | 0.000022262 | 熱錢包 |
| 提現(人工審核) | 11653944 | 21,000 | 1.101 gwei | 0.000023118 | 熱錢包 |

**七筆合計 0.000237 ETH**,其中交易所自己付的六筆是 **0.000175 ETH**。整套跑一趟的鏈上成本大約是一杯咖啡的百萬分之一,而 faucet 給的 1.733 ETH 夠跑上萬次——**測試網的成本從來不是限制,冷卻時間才是。**

時間的部分:

| | |
|---|---|
| 充值上鏈 → `credited` | **61 秒**(兩筆都是) |
| 原生歸集(建立 → confirmed) | 240 秒 |
| 代幣歸集(建立 → confirmed) | 479 秒 |
| 提現(自動核可) | 112 秒 |
| 提現(人工審核) | 224 秒——**含操作者自己的反應時間**,不是系統延遲 |
| 對帳一輪 | 3 秒 |

### 22.2 確認數規則被真的鏈證明了

`docs/plan-v1.0.md §6.4.1` 定義 `confirmations = head − block + 1`。這條規則有一個經典的差一錯誤版本(`head − block`),兩者在 anvil 上分不出來,因為出塊是即時的。

在 12 秒一個區塊的鏈上,兩者相差整整一個區塊:

- `head − block + 1`:達到 6 confirmations 時**只過了 5 個區塊** → 60 秒
- `head − block`:要過 6 個區塊 → 72 秒

**實測 61 秒,兩筆都是**(60 秒 + 一個掃描 tick)。規則是對的,而且這是唯一能證明它的環境。

### 22.3 ERC-20 的 gas 取決於收款方原本有沒有餘額

同一個 MockUSDC 的 `transfer`,兩次差了 **21,509 gas**:

| | gas | 收款方 |
|---|---|---|
| 充值(mint → 充值地址) | 51,178 | 餘額 0 → 非 0,**寫進一個新的儲存槽** |
| 歸集(充值地址 → 熱錢包) | 29,669 | 餘額非 0 → 非 0,改寫既有的槽 |

這不是雜訊,是 EVM 的定價:冷啟用一個零值儲存槽要 20,000 gas,改寫一個非零的只要 2,900。

**對費用模型的意義:** 拿歸集那筆(29,669)去估「一次代幣轉帳要多少 gas」,會低估首次收款的情況將近一倍。真正要收使用者提現手續費時,估算必須用**收款方餘額為零**的上界,否則第一個提現到全新地址的使用者就是虧損的那一筆。

### 22.4 費用上限的選擇被驗證了

`compose.sepolia.yaml` 把 `ETH_MAX_FEE_PER_GAS` 設在 50 gwei,理由寫的是「高到正常天氣下不會觸發,低到能擋住尖峰」。

實測我們自己送出的六筆交易,gas 價格落在 **1.005 – 1.115 gwei**——區間只有 11%,而上限是最高值的 **45 倍**。停損整趟一次都沒有觸發,這正是它該有的行為。

(腳本回報的「1.005 – 2.977 gwei」把 faucet 那筆算進去了。2.977 gwei 是 faucet 營運者自己出的價,不是我們的定價決策。)

### 22.5 對帳抓到了一個真的人為錯誤

B3 要操作者把 faucet 給熱錢包的錢記進帳本。ETH 那筆記對了,USDC 那筆**把 ETH 的金額記成了 USDC 的數量**:

```
ASSET  LEDGER    CHAIN      DIFF          
ETH    1.733     1.733      0             ok
USDC   1.733     1000000    999998.267    break
```

零容差的差異判定沒有給這個錯誤任何藏身的餘地,而且**差異的數字本身就指出了錯在哪**:1000000 − 1.733,一眼就看得出帳本記的是另一個資產的數字。

修法也照設計走完了:沖銷(debit 1.733)+ 重記(credit 1000000),兩筆各自帶新的 idempotency key、各自帶說明。**帳本不塗改,只追加**——半年後看的人會看到「有人記錯了,然後有人沖掉重記」,而不是一個對不上的數字。

這是 §6.4.4 第一次在**不是我們自己製造的錯誤**上生效。

### 22.6 歸集的保護在真鏈上被觸發了一次

同一輪裡,sweeper 從同一個充值地址規劃了兩筆歸集:USDC 和 ETH。ETH 那筆確認後把地址上的 ether 掃走了,USDC 那筆在簽名前重新檢查,發現地址已經付不起自己的手續費,於是放棄:

```
64317a52  USDC  250.5  failed (balance_changed)
afac2058  USDC  250.5  confirmed          ← 下一輪重新規劃的
```

**沒有錢遺失,也沒有送出任何註定失敗的交易。** 失敗那筆的 gas 欄是空的,因為它從來沒被簽名——`fail(..., money.Zero, nil)`。

自我修復是**有界的**:11:07:24 失敗,11:11:25 重新規劃,一個歸集週期。不是無限重試。

**這一節原本寫錯了機制,由區塊編號更正。** 原文說 ETH 那筆「連剛補過去當 gas 的 ether 一起掃走」。鏈上的順序不是這樣:

| 交易 | 區塊 |
|---|---|
| ETH 歸集 | 11653781 |
| USDC 歸集:補 gas | 11653801 |
| USDC 歸集:轉帳 | 11653820 |

**補 gas 那筆在 ETH 歸集之後 20 個區塊**,而且後面接的是成功那筆轉帳——所以整趟唯一的補 gas 交易屬於**重新規劃後的 `afac2058`**,不是失敗的 `64317a52`。失敗那筆自己從來沒補過 gas:地址當時有 0.871 ETH,走了 `sweep/run.go` 的「自己付得起」捷徑(`markGasFunded(ctx, row, money.Zero, nil)`,tx hash 是 nil)。**被 ETH 那筆掃走的是地址自己的錢。**

那 0.000023 ETH 的成本數字是對的(實測 0.000023422),但它的意義不是「被掃走的 gas」,而是**這場競態逼出來的一筆額外交易**:地址原本自己付得起,ether 被掃走之後,重新規劃的那筆才需要補。

這個競態在 anvil 上物理上不可能出現——確認是瞬間的,`stillAffordable` 的 gas 檢查從來不會不通過。

**修法是 migration 0015:一個地址同時只能有一筆 sweep,不分資產。** 原本想的兩個方向(從原生金額裡扣掉代幣的 gas 預算、或在有代幣 sweep 等著時延後原生那筆)都被這個取代了,理由在下面 §22.7 的第 2 項。

### 22.7 那趟走出來的八項缺陷

七項是走完之後整理出來的,第八項是驗證前七項的過程中挖出來的——而它比前七項都嚴重。

| # | 缺陷 | 代價 | 修法 |
|---|---|---|---|
| 1 | `retryStop` 從來沒有 stop 過:`retryUntil` 整個迴圈裡沒有一處檢查它 | 指向錯誤鏈的 chain role **永遠重試而不是拒絕啟動**,§6.4.1 step 5 在程式裡不存在 | `retryUntil` 回傳致命錯誤;`retryStop` 搬到 `retry.go` |
| 2 | sweep 的 partial unique index 帶了 `asset`,同一地址的 ETH 與 USDC 可以並存 | 兩筆許諾同一份 ether;最壞的情況是**代幣收不回來** | migration 0015:key 拿掉 `asset` |
| 3 | exchangectl 把憑證附到 `/v1/auth/*` | 過期的權杖擋住**唯一能換掉它的指令** | `newAnonymousClient`,不動 middleware |
| 4 | `newChain` 在 ops server 監聽之前跑完 | 啟動慢時 `/readyz` 不是 503,是**連線被拒絕** | 拆成建構與 `start()`,`start` 在 errgroup 裡 |
| 5 | 對帳把「還沒有這一列」包成錯誤 | 乾淨資料庫、以及**沒有 signer 的部署每 5 分鐘一次 ERROR,永遠** | `errNoCursor` / `errNoHotWallet`,跳過該輪並在 INFO 說原因 |
| 6 | 對帳表格沒有時間 | 一份錄影被當成即時讀數,**兩次** | 表格上方加 `pass ran: … (4m ago)` |
| 7 | `chain recorded` 只在第一次啟動印 | runbook 叫人每次啟動都去找那一行,第二次之後**永遠找不到,也沒有 error 可翻** | 重新驗證的路徑加 `chain verified` |
| 8 | B3 只有 ETH 的金額問鏈,USDC 寫死 | 就是 §22.5 那個被對帳抓到的人為錯誤的來源 | 兩個都問鏈,`AMOUNT` → `ETH_AMOUNT` / `USDC_AMOUNT` |

第 1–3 項在 4d-2a,第 4–8 項在 4d-2b。分成兩個 PR 的理由是可審查性:「`ErrChainChanged` 從來就不致命」埋在八項裡,正是最容易被略讀過去的那一種。

**第 2 項為什麼是「地址互斥」而不是原本想的兩個方向。** 0012 當初把 `asset` 放進 key,理由寫的是「兩者會搶同一個 nonce」——而 nonce 本來就是 per address(`run.go` 問節點的是 `PendingNonceAt(from)`)。真正被搶的是**地址的 ether**,而每一筆都當自己擁有全部:原生 sweep 只留自己的 21000 gas,`fundGas` 和 `stillAffordable` 讀的是原始餘額。既然被爭用的單位是地址,互斥的單位就該是地址。`planOne` 本來就把 unique 違反當正常並 `return nil`,所以 Go 端一行都不用改。

**而真正該擔心的路徑不是 Sepolia 上發生的那一條。** 觀察到的是好結果:代幣 sweep 在簽名前發現付不起,放棄,下一輪重來。壞的那條是原生交易**還在 mempool** 時代幣 sweep 去檢查——餘額看起來還夠,ERC-20 轉帳簽下去廣播了,然後原生那筆上鏈把 ether 拿走,代幣交易付不起自己被丟棄。而 `track()` 對 `ErrNotFound` 是 `return nil` 永不重送(對比 `trackGasFunding` 會重送),partial index 又擋住任何替代,admin 只有 `ListSweeps`。那個地址的代幣就收不回來,除非動資料庫。

### 22.8 「會說謊的註解」第三次

| phase | 缺陷 | 註解說的 | 程式做的 |
|---|---|---|---|
| 4d-1 | `CapAt` | 費用超過上限就等 | 把費用夾到上限然後照送 |
| 4d-2a | `retryStop` | 「拒絕啟動才是唯一誠實的答案」 | 無限重試 |
| 4d-2b | `hotWallet` | 「沒有 signer 的部署也能對帳」 | 每 5 分鐘一次 ERROR,永遠 |

同一個資料夾,連續三個 phase,同一種錯誤。所以問題不是「再仔細一點」,而是**這一類缺陷有沒有共同的形狀可以拿來找**。有,而且三次都一樣:

**註解描述的是一個承諾,而那個承諾在程式裡沒有對應的分支。**

`CapAt` 的註解說「等」,但函式裡沒有任何回傳「還不能送」的路徑。`retryStop` 的註解說「拒絕啟動」,但 `retryUntil` 的迴圈裡沒有一處讀 `retryStop`——`grep retryStop` 只在 `chain_role.go` 有命中。`hotWallet` 的註解說「signer 不在也能跑」,但函式對 `pgx.ErrNoRows` 沒有分支。

三個都可以用**同一個檢查**抓到,而且不需要讀完整段邏輯:

> 註解裡每一個「會 / 不會 / 必須 / 只有」,在同一個函式(或它呼叫的迴圈)裡指得出對應的 `if` 嗎?指不出來就是候選。

§21 說過找這一類缺陷「唯一可靠的辦法」是把設定放到一個它真的會生效的環境裡去想。那句話現在要修正:**那是唯一可靠的辦法,但不是唯一有用的辦法。** 實跑一次的成本是一整趟 Sepolia,而且只照得到你剛好走過的路徑——`retryStop` 那條路,實跑那趟從頭到尾沒有踩到,因為那條鏈一直是對的。上面那個檢查便宜到可以對每一個承諾都問一次,代價是它只抓得到「承諾與程式碼在同一個檔案裡就對不上」的那一種。兩個一起用:實跑決定**該懷疑哪裡**,指 `if` 決定**那裡到底有沒有問題**。

第 8 項也是同一個形狀的文件版:`> --amount 要跟鏈上實際的數量一致` 是一個承諾,而它下面那一段程式碼把 `1000000` 寫死了——承諾與相鄰的程式碼直接矛盾,只是這次「程式碼」是一段 shell。

**這也是為什麼修法要紅燈先行。** 一個註解宣稱的行為,如果寫不出會因為它失敗的測試,那個行為多半就不存在。第 1 項的紅燈甚至不是「失敗」——它**直接掛住**,因為那正是缺陷本身。

### 22.9 獨立驗證這件事本身的成績

七項發現各自派一個 agent 獨立驗證,再派一個 agent 對抗式挑戰(14 個 agent)。結果值得記下來,因為它同時證明了這個做法有用、和我自己的第一版判斷不可靠:

| | 結果 |
|---|---|
| 七項發現 | **全部為真** —— 3 個 confirmed、4 個 partly(缺陷存在,但我描述的機制有誤) |
| 我提的修法 | **三個沒通過挑戰** |
| 額外收穫 | 挑戰過程挖出第八項(`retryStop`),比原本七項都嚴重 |

被否決的三個修法都值得看,因為它們的錯法不一樣:

- **chain role 啟動順序**:我原本要用 `atomic.Bool` 存「起來了沒」。挑戰者指出那樣 `/readyz` 說不出**是哪裡卡住**。而這個 codebase 早就有正確答案——`CheckResult.Err` 是字串、`chainComponents.lastErr` 是 error 不是 bool。要找的東西已經在那裡了。
- **`chain recorded` 只印一次**:我原本打算只改文件。挑戰者發現我寫的替換文字裡有三個新的事實錯誤,其中 `"interval":"12s"` 是錯的——slog 把 `Duration` 編成整數奈秒,使用者真實的 log 裡是 `12000000000`。**修一個誤導的文件時,寫進去三個新的誤導。**
- **B3 的金額**:我原本打算拿 `admin reconcile` 的 CHAIN 欄當輸入。那是刻意落後的 frontier 數字,而 B3 正好是它最舊的時候;更糟的是,拿同一份報告同時當輸入和驗證,會**毀掉當初抓到 §22.5 那個錯誤的那個比對**。

而 §22.6 原本寫錯的機制,也是同一件事的樣本:那是對現象的合理推測,不是對程式碼與區塊順序的驗證。事後把 `sepoliaresults.md` 的區塊編號排一次,一分鐘就定死了順序。

**教訓不是「多派 agent」,是驗證的對象要選對。** 有用的驗證都指向同一種東西:**一個能反駁我的、獨立於我的紀錄**——區塊編號、`grep` 的命中數、紅燈測試、資料庫的欄位。指向「再想一遍」的驗證一次都沒有抓到東西。
