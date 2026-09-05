# ADR-0005:複式記帳帳本,凍結也是分錄,ledger 是餘額唯一寫入者

- 狀態:已採納(Accepted)
- 日期:2026-09-05
- 相關:[ADR-0000](0000-interview-decisions.md) 第 6 / 8 輪;`docs/plan-v1.0.md` §4 原則 3、§6.1;審查 finding F01 / F12 / L01;`docs/domain.md` §1

## 背景

v0.1 讓 account-service 凍結、ledger-service 解凍,兩者靠 NATS 對齊,無法在單一交易內維持 `available + hold = 帳本餘額`。作者自述沒有複式記帳知識,若臨場決定幾乎必然做成可 UPDATE 的單欄餘額,且無法產生對帳報表。

## 選項

1. **單欄餘額 + UPDATE**:最直覺;沒有審計軌跡,對帳只能靠日誌,錯帳無法定位。
2. **帳本記流水但餘額仍由別的服務維護**:v0.1 的形態,兩個寫入者。
3. **真正的複式記帳**:journal entry 由多筆 posting 組成,每資產 Σdebit = Σcredit;posting 只 INSERT;`balances` 只是同交易內維護的快取;Hold / Release / Settle / Credit / Post 是 ledger 對外全部的寫入介面。

## 決定

採選項 3。

- 科目型別與方向見 `docs/plan-v1.0.md` §6.1.1 與 `docs/domain.md` §1.1;會計恆等式 `custody = user_available + user_hold + pending_withdrawal + fee_revenue − gas_expense + external` 在 `domain.md` §1.2 推導成立。
- 每個 `account × asset` 有 `available` 與 `hold` 兩個科目;**凍結是分錄**(debit available / credit hold),不是欄位更新。
- house 科目:`fee_revenue`、`gas_expense`、`custody:deposit_addresses`、`custody:hot`、`pending_withdrawal`、`external`;不進 `balances`,餘額由 postings 推導。
- `journal_entries.idempotency_key` UNIQUE;重放回原 entry。鍵樣式 `hold:order:{id}`、`settle:trade:{id}`、`deposit:{chain}:{tx}:{log}` 等。
- 手續費由 ledger 在 `Settle` 時依 `ledger.FeeParams{MakerBps, TakerBps, BaseScale, QuoteScale}` 計算(`matching` 不算費、`ledger` 不 import `registry`)。
- 資料庫角色權限強制:`ledger.postings / journal_entries` 只有 `ex_engine`、`ex_chain`、`ex_admin`、`ex_all` 有 `SELECT, INSERT`,無人有 UPDATE / DELETE;`CHECK (available >= 0 AND hold >= 0)` + deferred trigger 檢查每 entry 借貸相等。
- 管理員調帳(對手科目 `external`)從 Phase 2 起存在:開發期是 faucet,正式產品是有 reason 與審計的調帳功能。

## 後果

- 正面:試算平衡恆為 0 是可持續監控的指標;任何餘額都能追到分錄;冪等靠唯一鍵而非約定。
- 負面:每筆成交 6~8 筆 posting,寫入量比單欄更新高;`balances` 行鎖要排序取得以避免死鎖。beta 規模可接受。
- 已知修正:提現 `broadcast` 後的 `cancel_nonce` 路徑必須從 `pending_withdrawal` 退款而非對 hold 做 Release(`domain.md` E1,計畫已更正)。
- Phase 0 已落地:`ledger` schema 與角色權限模型(GRANT-only)在 `migrations/0001`,表本體在 Phase 2 的 `0003_ledger_core.sql`。
