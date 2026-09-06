# ADR-0002:Postgres 為唯一真相、交易性核心 + outbox、JetStream 只做扇出

- 狀態:已採納(Accepted)
- 日期:2026-09-05
- 相關:[ADR-0000](0000-interview-decisions.md) 第 6 輪;`docs/plan-v1.0.md` §4 原則 2/5、§5.2、§7.3;審查 finding F02 / F03 / F04 / E01 / E02

## 背景

v0.1 把成交→記帳、充值→入帳放在無持久化的 NATS core 上(at-most-once),撮合引擎純記憶體、無序號、無恢復,訂單沒有擁有者,gateway 被迫做 saga。任何一次重啟都會漏記或重複記帳。封閉 beta 需要「引擎重啟用戶無感」與「事件不漏不重」。

## 選項

1. **Event sourcing**:事件流為真相、狀態全由重放得到。正確但對初學者與單人團隊的除錯成本高,快照 / schema 演進都要自建。
2. **NATS core + 補償(saga)**:沿用 v0.1,補冪等與補償交易。補償路徑爆炸,資金正確性靠約定。
3. **交易性核心 + outbox**:訂單狀態、成交、帳本分錄、outbox 在同一筆 Postgres 交易內 commit;relay 把 outbox 送到 JetStream;消費者只做扇出與投影;引擎重啟從 open orders 重建。

## 決定

採選項 3。

- 撮合核心是純函式 `matching.Apply(cmd) ([]Event, error)`,不做 I/O、不看時鐘;`seq` 與時間戳由呼叫方指派。
- 每市場一個 goroutine:`BEGIN → ledger.Hold → 分配 seq → Apply → 寫 orders/trades/分錄/outbox → COMMIT`;Hold 失敗即 ROLLBACK 回 `rejected`,拒單補償退化為交易回滾,**不需要 saga**。
- Postgres 是唯一真相;記憶體訂單簿、Redis 快照、JetStream 事件流都是可重建的衍生物。啟動時從 `status ∈ {open, partially_filled}` 依 `(price, seq)` 重建。
- outbox → JetStream:relay 以 `Nats-Msg-Id = event_id` 去重;streams `EX_TRADING` / `EX_CHAIN` / `EX_REGISTRY`,subjects `ex.v1.<domain>.<type>.<tenant>.<scope>`;扇出型消費者用 ordered consumer 以 `seq`/`account_seq` 去重,處理型消費者用 durable consumer + `processed_events` 冪等表。
- 事件 envelope 與 catalog 是對外契約(版本化、golden 測試)。

## 後果

- 正面:資金路徑的正確性只依賴 Postgres 交易;`kill -9` 引擎後 book 與 hold 一致成為可自動驗證的 DoD(Phase 3);消費者永遠可以重放。
- 負面:每個命令一筆 PG 交易,吞吐受單筆交易延遲限制(§3.3 目標 1,000 orders/s,必要時 runner 內 group commit);跨市場 / 跨帳戶的事件順序不保證,客戶端必須依 `seq` / `account_seq`。
- 需要守住的事:`account_seq` 在寫 outbox 的同一交易內 `UPDATE ledger.accounts SET next_seq = next_seq + 1`,**不可用 outbox 的 BIGSERIAL 當序號**(取號單調但提交順序不保證)。
- Phase 0 已落地的部分:migration 以 goose 管理、`registry` 表為 admin 可編輯的 DB 真相、`eventbus` schema 已預留。
- Phase 3a 落地:`internal/trading`(每市場 runner、savepoint 包住 Hold + Apply、守衛式 seq、`hold_remaining` 不變量、advisory lock 單實例、重啟重建)、`internal/eventbus`(envelope、outbox、LISTEN/NOTIFY relay、`Nats-Msg-Id` 去重、streams 宣告、`processed_events`);對應表與驗證見 `docs/domain.md` §12。
