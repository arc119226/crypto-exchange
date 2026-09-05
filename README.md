# crypto-exchange

白牌交易引擎(white-label exchange engine)的商業化原型:現貨撮合、複式記帳帳本、EVM 充提與歸集、行情推播、管理後台,以單一 Go binary 多角色的模組化單體交付,客戶透過 REST / WebSocket / Webhook 與事件契約整合。

**目前狀態:規劃完成、尚未開工。** 所有設計與分階段計畫見 `docs/`。

## 文件索引

| 文件 | 內容 |
|---|---|
| [`docs/plan-v1.0.md`](docs/plan-v1.0.md) | **分階段可執行計畫 v1.0**(定位、範圍、領域模型、契約、模組、選型、compose、Phase 0~7、測試/CI、安全、觀測、部署、風險) |
| [`docs/review/plan-review-2026-09.md`](docs/review/plan-review-2026-09.md) | v0.1 規劃書審查報告(28 條合併後發現、不採納意見、對 v1.0 的結構性要求) |
| [`docs/adr/0000-interview-decisions.md`](docs/adr/0000-interview-decisions.md) | 需求訪談決策記錄(8 輪 32 題);ADR-0001 起由 Phase 0 撰寫 |
| [`docs/archive/plan-v0.1.md`](docs/archive/plan-v0.1.md) | 原始 v0.1 規劃書(已取代,僅供對照) |

## 下一步

照 `docs/plan-v1.0.md` 第 20 節「立即行動(接下來 5 個工作天)」開始 Phase 0:領域建模 + walking skeleton。
