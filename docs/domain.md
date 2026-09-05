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

**tick / step**:API 端「不是整數倍就拒單、不自動截斷」;`matching` 再驗一次回 error 而非 panic。`money.IsMultipleOf` 已在 Phase 0 實作並測試(`internal/money`)。

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
| **E2** | §6.2 拒單原因、§6.3 市價單 | 市價單因 `max_slippage_bps` 一筆都吃不到時沒有定義結果:`empty_book` 不對(簿不空),`cancelled` 也不對(沒有任何成交卻要先 Hold 再 Release)。 | 新增拒單原因 `price_protection`:零成交 → `rejected(price_protection)`(交易回滾、無分錄);已有成交才觸及保護帶 → `cancelled(reason=price_protection)`。Phase 1 實作時採用;計畫 v1.1 補進枚舉。 |
| **E3** | §6.2 STP 列的事件欄 | 只寫 `order.accepted + order.cancelled(self_trade)`,漏掉撞到自己之前可能已與他人成交的 `trade.executed`×n(第 3 節例子)。 | 事件欄補 `trade.executed`×n(n ≥ 0)。文件層級澄清,不影響實作。 |
| **E4** | §6.1.3 `Release` 鍵 `release:order:{order_id}:{seq}` vs §6.1.4(b) | 價差 release 出現在 Settle 的同一 entry 內(鍵 `settle:trade:{trade_id}`),不是獨立 Release;兩者並存但計畫沒說清楚。 | 約定:每筆成交的價差 release 屬於 Settle entry;`release:order:…` 只用於取消 / IOC 剩餘 / `filled` 時的殘值。 |
| E5 | §6.1.1 `external` | 標「無正常餘額」但恆等式需要符號。 | 本文件 1.1 約定 credit − debit。 |
| E6 | §6.6 `assets` | 沒寫 `display_scale ≤ scale`。 | Phase 0 migration 已加 `CHECK (display_scale BETWEEN 0 AND scale)`。 |

E1 是計畫的實質錯誤(會讓一條人工處置路徑在資料庫層失敗),滿足 Phase 0「找出至少一處本文件的錯誤並修正」。

---

## 9. 疑問清單(留給對應 Phase 決定)

1. **(Phase 1)** 限價單「部分成交後價格不再滿足」的判斷是否含等於?建議:對手價嚴格優於或等於限價都成交(標準做法),golden 案例要有「限價恰等於對手價」。
2. **(Phase 1)** `max_slippage_bps` 的基準價是「觸發時的最佳對手價」;若最佳價本身就是唯一一檔,保護帶永遠不觸發,是否要改成以「上一筆成交價」為基準?建議 v1 維持計畫定義,文件明示。
3. **(Phase 2)** `balances` 的 `version` 欄位是否用於樂觀鎖?計畫同時用 `FOR UPDATE`。建議只留 `FOR UPDATE`,`version` 作為除錯用途。
4. **(Phase 2)** 手續費是否允許 0 bps 的市場(做市優惠)?`fee_schedules` CHECK 允許 0,ceil(0) = 0,守恆不受影響 → 可以。
5. **(Phase 3)** `order.accepted` 對「同交易內立刻全部成交」的單也要發(§6.2 規則),事件順序 accepted → executed×n → filled 在 outbox 內以 `id` 排序即可;跨市場順序不保證 → 客戶端只能依 `account_seq`。
6. **(Phase 4)** ERC-20 歸集第 1 步「精確 gas」G′ 的估算若低於實際,第 2 步失敗;建議 G′ = estimate × 1.2 並接受少量 ETH 灰塵留在充值地址(第 6 節)。
7. **(Phase 4)** `withdrawal_fee`(v1 = 0)一旦非零,應在 `funds_locked` 時一併 Hold(X + fee),`confirmed` 時 fee 進 `fee_revenue`;計畫沒有這筆分錄,v1.1 補。
8. **(Phase 5)** 對帳報表的 `external` 明細如何呈現「已知原因」?建議 `journal_entries.kind ∈ {faucet, adjustment, write_off}` + `reason`。

---

## 10. Phase 0 程式碼與 §6.6 的對應

| §6.6 | 實作 | 備註 |
|---|---|---|
| `assets` 欄位 | `migrations/0002_registry_core.sql` `registry.assets` | 全部欄位齊;多加 `display_scale ≤ scale`、`contract_address` 格式、`is_native ⇔ contract_address IS NULL` 的 CHECK |
| `markets` 欄位 | `registry.markets` | `base ≠ quote` CHECK;`self_trade_policy` / `status` 枚舉 CHECK |
| `fee_schedules` | `registry.fee_schedules` | `maker_bps`、`taker_bps` ∈ [0, 10000] |
| `withdrawal_limits` | `registry.withdrawal_limits` | PK `(tenant_id, asset_id, kyc_level)`,`kyc_level` ∈ {0,1,2} |
| 精度約束 | `registry.ValidatePrecision`(`internal/registry/validate.go`) | 單元測試 `TestValidatePrecision` |
| seed 值 | `internal/registry/seed.go` | ETH(scale 18 / display 6 / 1 確認)、USDC(6 / 2)、ETH-USDC(tick 0.01、step 0.0001、min_notional 5、maker 10 / taker 20、`cancel_newest`) |
| 寫入權限 | `GRANT INSERT, UPDATE … TO ex_admin, ex_all` | 整合測試證明 `ex_api` 寫入得到 `42501` |
