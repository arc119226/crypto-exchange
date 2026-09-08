# reorg 告警

reorg 本身不是事故。掃描器**自動處理**未入帳的充值:回退到共同祖先、把受影響的充值標 `orphaned`、重掃新鏈。真正需要人的只有兩種情況:reorg 深過 `chain.blocks` 的環,以及**已入帳**的充值所在區塊被抹掉。

## 症狀

- `chain_reorgs_total` 上升,或掃描器 log 出現 `reorg detected`。
- `deposit.orphaned` / `deposit.dropped` / `deposit.reversed` 事件出現。
- log 出現 `reorg deeper than N blocks`;或 `chain_scanner_lag_blocks` 持續上升、`deposit scan failed` 反覆出現。

| 現象 | 嚴重程度 | 動作 |
|---|---|---|
| `orphaned` 出現,幾分鐘內變回 `credited` | 正常 | 不用做事。這正是設計要處理的情況 |
| `orphaned` 一直沒回來,最後變 `dropped` | 正常 | 該筆交易不在正規鏈上,通知使用者重送 |
| log 出現 `reorg deeper than N blocks` | **需要人** | 「超過環深度」 |
| **已 `credited` 的充值所在區塊被 reorg 掉**(`deposit.reversed`) | **需要人** | 「入帳後被 reorg」 |
| `chain_scanner_lag_blocks` 持續上升 | 需要人 | 掃描器跟不上或卡住,看 `deposit scan failed` 的原因(節點、限流) |

## 檢查指令

```sh
# 掃描器目前的看法
curl -s localhost:9100/metrics | grep -E 'chain_(head_block|last_scanned_block|scanner_lag_blocks|reorgs_total)'

# 受影響的充值
psql -c "SELECT status, count(*) FROM chain.deposits GROUP BY status;"
psql -c "SELECT id, tx_hash, block_number, status, account_id, amount FROM chain.deposits WHERE status IN ('orphaned','dropped') ORDER BY id DESC LIMIT 20;"

# 鏈的說法,用兩個獨立的節點
cast tx <hash> --rpc-url "$ETH_RPC_URL"
cast block <number> --rpc-url "$OTHER_RPC_URL"
```

## 處置

### 超過環深度

`chain.blocks` 只保留最近 `ETH_BLOCK_RING_DEPTH`(預設 128)塊,找不到共同祖先就會停下來而不是亂猜。掃描器會回 error 並在下個 tick 重試,不會前進。

1. 確認鏈真的發生了那麼深的 reorg(不是節點壞掉):換一個 RPC 節點比對 `eth_getBlockByNumber`。
2. 若確實如此,把 `ETH_BLOCK_RING_DEPTH` 調大後重啟 chain role,它會重新找祖先。
3. 若是節點資料損毀,換節點即可,不要動資料庫。

### 入帳後被 reorg

這是唯一會動到錢的情況,而且**掃描器刻意不自動處理**(`docs/plan-v1.0.md` §6.4.1)。

已入帳表示錢已經進使用者的 `available`,可能已經被拿去下單或提現。自動寫反向分錄會在餘額不足時撞上 `balances` 的 CHECK,而且會讓一個誠實的使用者莫名其妙變負數。

1. 確認該筆交易確實不在正規鏈上:`cast tx <hash>` 在兩個獨立節點上都查不到。
2. 記錄 `deposit_id`、`tx_hash`、`account_id`、金額。
3. 由人決定處置,並以 admin 調帳(`POST /admin/v1/ledger/adjustments`,`reason` 必填)執行,不要直接改資料庫。
4. `deposit.reversed` 事件是**告警**,不代表反向分錄已經完成。

### 不要做的事

- 不要手動改 `chain.scan_cursors`:游標與 `chain.blocks` 是一起維護的,單獨改會讓下一次 reorg 偵測失效。
- 不要 `DELETE FROM chain.deposits`:那張表沒有任何角色有 DELETE 權限,這是刻意的。
- 不要在 anvil 上用 `make reset` 以外的方式清鏈:`chain.chain_state` 的 genesis 檢查會擋住,那是保護不是障礙。

## 驗證

- `chain_scanner_lag_blocks` 回到個位數、`chain_last_scanned_block` 跟著 `chain_head_block` 走;`chain_reorgs_total` 停止上升。
- `orphaned` 的充值幾分鐘內回到 `credited` 或變成 `dropped`,沒有一列停在 `orphaned` 超過 `ETH_ORPHAN_EXPIRY_BLOCKS`。
- 入帳後被 reorg 的那一筆:調帳的 `audit.audit_events` 有列、`exchangectl admin reconcile` 下一輪 `DIFF = 0`(`docs/runbooks/reconciliation-break.md`)。
- 調過 `ETH_BLOCK_RING_DEPTH` 的:chain role `readyz` 綠,log 沒有再出現 `deeper than`。

## 相關指標

`chain_head_block`、`chain_last_scanned_block`、`chain_scanner_lag_blocks`(`ChainScannerLagging` 告警)、`chain_reorgs_total`、`deposits_credited_total{asset}`。
