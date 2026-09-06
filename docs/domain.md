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

E1 是計畫的實質錯誤(會讓一條人工處置路徑在資料庫層失敗),滿足 Phase 0「找出至少一處本文件的錯誤並修正」。

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
| `exchange admin bootstrap`(§14) | `auth.Service.BootstrapAdmin`:冪等,已存在則不改密�major;寫審計 `auth.admin.bootstrap`;需 `ADMIN_BOOTSTRAP_EMAIL / PASSWORD` | `exchangectl` 目前沒有 admin 登入(admin TOTP + session 在 Phase 5) |
| `exchangectl`(§12 Phase 3) | `user register\|login\|logout\|me`、`api-keys create\|list\|revoke`、`orders place\|cancel\|list\|get`、`balances`、`fills`、`book`、`trades`、`e2e`;憑證 `--token` / `EXCHANGE_TOKEN` 或 `--api-key --api-secret` / `EXCHANGE_API_KEY(_SECRET)`,API key 模式對每個請求做 HMAC 簽章 | `e2e` 用 admin API 注資、依 §6.1.4 數字逐項斷言、再驗試算平衡 |

**驗證(整合測試,`test/integration/api_test.go`)**:未帶憑證 401、壞 token 401 + `WWW-Authenticate`;註冊(大小寫不敏感 409、弱密碼 422、格式 400)、登入(錯誤密碼與不存在帳號同為 401)、refresh 輪替 → 舊 token 重放 401 且家族撤銷、登出後 401、登出冪等;§6.1.4 (a)(b)(c) 經 HTTP 逐數字相符(買方 `8004 / 1200`、`0.3992 ETH`、賣方 `795.204`、fee `0.796 USDC` / `0.0008 ETH`、取消後 `9204`);201 / 200 / 422 / 400 / 404;拒單 `insufficient_balance`、`invalid_price_tick` 為 201 + rejected;他人訂單 404;fills 雙方看到同一 `trade_id`;ledger entries 只含自己的 4 條 settle posting;depth / trades;API key 建立、簽 GET 與帶 query、簽 body、篡改 body 401、錯簽 / 過期時間戳 / 錯 secret / 未知 key / 壞 timestamp 皆 401、read key 下單 403、key 不能建 key 403、IP 白名單 403 / 200、撤銷後 401、他人 key 404;第 6 次登入 429 + `Retry-After ≤ 12`;審計計數;admin bootstrap 冪等且 role=admin 可登入。

**Phase 3b 學到的事**:oapi-codegen strict server 不驗 `minLength / minimum`,`client_order_id` 為空與 `depth?limit=0` 要自己處理(前者 400,後者退回預設值);jwx v3 的 `Get` 對陣列 claim 只接受 `[]any`;`gosec` G101 會把名字含 `Token` 的 Lua 常數當成硬編碼憑證,改名即可;fills 的 `order_id` 過濾若只看「該單參與的成交」會讓對手方探測任意 order id 是否與自己成交過。
