# ADR-0003:v1 單租戶,所有業務表預留 tenant_id

- 狀態:已採納(Accepted)
- 日期:2026-09-05
- 相關:[ADR-0000](0000-interview-decisions.md) 第 5 輪;`docs/plan-v1.0.md` §2.1、§6.7;審查 finding P03

## 背景

白牌引擎「一套部署 = 一個營運方」是 v1 的形態,但商業上可能出現第二個客戶要求共用基礎設施。審查建議乾脆不加 `tenant_id`(避免半吊子隔離);訪談決定預留欄位但不做隔離邏輯。

## 選項

1. **不加 tenant_id**:schema 最簡單;日後多租戶要對每張核心表做遷移並回填。
2. **完整多租戶**:租戶級權限、查詢隔離、per-tenant 限流與配額。v1 沒有需求,成本全是投機。
3. **預留欄位、恆為單一值**:核心表帶 `tenant_id text NOT NULL DEFAULT 'default'` 並納入唯一鍵;事件 envelope 帶 `tenant_id`;沒有任何查詢隔離或租戶權限邏輯。

## 決定

採選項 3。

- 慣例(Phase 0 起):每張業務表 `tenant_id text NOT NULL DEFAULT 'default'`,唯一鍵一律含 `tenant_id`(例:`registry.assets UNIQUE (tenant_id, symbol)`);`TENANT_ID` 環境變數注入 `app.Config`,API handler 以它查詢。
- NATS subject 第四段為 tenant(`ex.v1.<domain>.<type>.<tenant>.<scope>`)。
- 不做:租戶級 RBAC、資料隔離測試、per-tenant 設定 UI。

## 後果

- 正面:第二個租戶出現時是「加邏輯」而不是「改 schema + 回填」;事件與 subject 從第一天就能按租戶過濾。
- 負面:每張表多一欄、每個唯一鍵多一段;開發者可能誤以為已有隔離。文件與 README 明示「v1 恆為 `default`」。
- 觸發重評:第二個付費客戶要求共用基礎設施。屆時開新 ADR 決定隔離層級(schema-per-tenant vs row-level)與租戶管理 API。
