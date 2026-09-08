# 備份與還原

資料庫每天一份 `pg_dump`、WAL 每 30 秒歸檔到 S3 相容儲存(`scripts/backup.sh`;compose 的 `backup` profile 用 MinIO 代替),每次成功或失敗都寫一列 `admin.backups`,admin role 把最新一列變成 `backup_last_success_timestamp_seconds{kind}` 與後台首頁的「Last backup」。這份手冊寫怎麼確認備份在跑、怎麼還原、還原之後系統的哪些部分**不會**自己回到正確狀態。演練腳本 `scripts/restore-drill.sh` 就是還原步驟本身,CI 的 `e2e` job 每個 PR 跑一次(`make backup-drill`)。

**備份範圍不只資料庫**:`secrets/prod/`(HD seed、passphrase、JWT 私鑰、三把主金鑰)不在 dump 裡。一份完美的資料庫還原沒有 seed 就簽不了任何一筆提現、所有已發出的充值位址全部變孤兒。佈署時與每次密鑰輪替之後,把 `secrets/prod/` 做一份**離線加密 escrow**(`age`/`gpg` 加密後放到與 VM 無關的地方,兩個人各持一份),`docs/beta-checklist.md` 有勾選項;演練不涵蓋它。

## 症狀

- 告警 `BackupStale`(最新成功 dump 超過 26 小時)、`WalArchiveStale`(最新 WAL 出貨超過 30 分鐘)、`BackupNeverTaken`(跑了一天沒有任何成功 dump)。
- 後台首頁「Last backup」是灰的或日期不對;`GET /admin/v1/system/status` 的 `last_backup` 為 null 或過舊。
- `DiskAlmostFull`:`/wal-archive` 堆積(sidecar 上傳不了)會把磁碟吃滿,磁碟滿了 Postgres 就停;這是備份問題長成可用性問題的樣子。
- 需要還原:磁碟壞、誤刪、migration 弄壞資料、VM 整台重建。

## 檢查指令

```sh
# 最近幾次備份的結果(kind、時間、大小、物件位置、錯誤)
psql "$DATABASE_URL" -c "SELECT kind, status, finished_at, size_bytes, location, error FROM admin.backups ORDER BY id DESC LIMIT 10"
# 儲存端真的有東西(compose:MinIO;prod:外部 S3)
docker compose --profile backup run --rm --entrypoint mc backup ls backup/exchange-backups/dumps/
docker compose --profile backup run --rm --entrypoint mc backup ls backup/exchange-backups/wal/ | tail -3
# 等待出貨的 WAL(正常:0~1 個;一直增加 = sidecar 上傳失敗)
docker compose exec postgres sh -c 'ls /wal-archive | wc -l'
# Postgres 自己的歸檔狀態(failed_count > 0 = archive_command 在失敗,通常是磁碟或權限)
docker compose exec postgres psql -U exchange -d exchange -c "SELECT archived_count, last_archived_wal, last_archived_time, failed_count, last_failed_wal FROM pg_stat_archiver"
# sidecar 的 log
docker compose logs --tail 50 backup
# 指標
curl -s http://127.0.0.1:9100/metrics | grep backup_last_success
```

## 處置

### 備份沒在跑

1. `docker compose logs backup`:`waiting for the store` = S3 端點或憑證錯;`upload failed` = bucket 不存在或權限不夠(sidecar 用 `mc mb --ignore-existing` 建 bucket,外部 S3 需要 `s3:CreateBucket` 或先手動建好);`pg_dump failed` = `ex_backup` 密碼錯或角色不存在(既有叢集用 `scripts/db-roles.sql` 補角色,再跑 `migrations/0022` 的 grant 或 `db-roles.sql` 尾段的兩句 GRANT)。
2. 手動補一份:`docker compose --profile backup run --rm backup once`(出貨 WAL → dump → 出貨;任一步失敗回非零)。
3. `pg_stat_archiver.failed_count` 在漲:進 postgres 容器看 `/wal-archive` 可寫(`chown postgres`,compose 的 entrypoint 包裝已處理)與磁碟空間。

### 還原(R1–R10)

先決定要回到哪個時間點 T = 最新 dump 的 `finished_at`。T 之後到現在發生的所有事(訂單、成交、充提)都不在 dump 裡;WAL 歸檔**不能**用來把 pg_dump 往前推(那需要 `pg_basebackup` 的物理備份,見「PITR」)。

1. **停所有角色**(`docker compose stop exchange-api exchange-engine exchange-chain exchange-signer exchange-stream exchange-admin exchange-worker`,或 `exchange-all`)。**提現保持停止**直到第 6 步做完。
2. 還原 = 演練腳本指向真的叢集與資料庫名:

   ```sh
   # 新 VM:先起 postgres(01-roles.sh 建角色)與 minio/外部 S3 的憑證
   docker compose --profile infra up -d --wait postgres
   # 把最新 dump 還原成 exchange(DRILL_DB 就是目標庫名;KEEP=1 保留)
   docker compose --profile backup run --rm --entrypoint /usr/local/bin/restore-drill.sh \
     -e BACKUP_STORE=s3 -e DRILL_DB=exchange -e KEEP=1 -e EXPECTED_MIGRATION=<migrations/ 最大編號> \
     -e DRILL_ADMIN_URL="postgres://exchange:$POSTGRES_PASSWORD@postgres:5432/postgres?sslmode=disable" backup
   ```

   它會 `DROP DATABASE IF EXISTS` 目標庫——對正在用的 `exchange` 這是刻意的,所以第 1 步要先停角色。腳本以 superuser `pg_restore --no-owner --role=ex_migrate` 還原、驗 migration 版本、跑 `scripts/sql/restore-checks.sql`(試算平衡、餘額快取 = postings、每市場 seq = 最新的 order / outbox 事件 seq(cancel 消耗 seq 但不留 order 列,所以只比 orders 會誤報)、成交 idx 無洞、每 entry ≥ 2 postings)並印 RTO。
3. **R1 purge JetStream**:stream 對 `event_id` 去重,還原後 outbox 會重發 T 之前已發過的事件,舊的去重表會把它們吃掉,而 (T, now] 的事件已經永久遺失。用 `natsio/nats-box` 清三條 stream:`nats stream purge EX_TRADING -f && nats stream purge EX_CHAIN -f && nats stream purge EX_REGISTRY -f`(compose:`docker run --rm --network crypto-exchange_exchange-net natsio/nats-box:0.16.0 nats --server nats://nats:4222 stream purge EX_TRADING -f`)。
4. **R4 帳戶序號跳號**:私有 WS 的 `resume since_seq` 與客戶端快取的 `account_seq` 可能比還原後的值大;引擎重啟後從 DB 的 `next_seq` 繼續,會發出客戶端已看過的序號、客戶端把它們當重複丟掉。`UPDATE ledger.accounts SET next_seq = next_seq + 1000000;` 一次跳過整段(stream 對超過現值的 `since_seq` 回 `resume_failed`,客戶端從 REST 重載,這是 Phase 7 加的保護)。
5. **R2/R3 熱錢包 nonce**:T 之後送出的提現交易鏈上已經發生,DB 不知道。chain role 啟動時比對 `chain.hot_wallets.next_nonce` 與鏈上 pending nonce,鏈上較大就 `ErrForeignTransaction` 拒絕出貨——**這是設計上的保護,不要繞過**。人工結案:對 `[db next_nonce, 鏈上 pending)` 的每個 nonce 用 `cast tx <hash>`(從 explorer 找該位址的交易)對到一筆 `chain.withdrawals` 列:付款發生了 → 用後台把它標成 `confirmed`(產生 `pending_withdrawal → custody_hot` 的帳本移動);沒發生或不是提現 → `failed` + 退款。全部對完才 `UPDATE chain.hot_wallets SET next_nonce = <鏈上 pending>`,再起 chain / signer。Phase 7 不自動化這一步。
6. **R5/R6 事件與 webhook**:(T, now] 的事件遺失、webhook 沒送;T 之前最後一批 outbox 列會重送(webhook 本來就是 at-least-once,客戶端要能吃重複)。通知 webhook 客戶用 REST 補抓 T 之後的資料。
7. **R7 充值**:chain role 從 `chain.scan_cursors` 重掃,T 之後的充值會重新入帳(正確);期間的 sweep 與提現變成「鏈上少了 gas」的對帳差異 → `exchangectl admin house-adjust` 依 `docs/runbooks/reconciliation-break.md`。
8. **R8** K 線與 `kline_cursors` 在同一份 dump,worker 會從游標補;**R10** Redis 只有快取,啟動自己重建。
9. 起角色(`docker compose up -d`),看 `readyz` 全綠;**提現最後起**(第 5 步之後)。

### PITR(未演練)

WAL 歸檔的用途是日後補上 `pg_basebackup` 之後做時間點還原:`restore_command = 'mc cp backup/exchange-backups/wal/%f %p'`(或先 `mc mirror` 下來再 `cp`)、`recovery_target_time = 'T'`、`touch recovery.signal`。目前**沒有**自動的 base backup(sidecar 的 `ex_backup` 角色沒有 REPLICATION 權限、stock image 的 `pg_hba` 不放行遠端 replication 連線),所以 WAL 現在只是為了那一天先存著;要做 PITR 得先手動 `pg_basebackup -h postgres -U exchange -Ft -z -D <dir>` 並上傳。這是 beta checklist 的已知缺口。

## 驗證

- 備份:`admin.backups` 最新的 `kind='dump'` 列 `status='ok'` 且 `finished_at` < 24 小時;`kind='wal'` 列 < 30 分鐘(閒置系統最多 15 分鐘一段);`mc ls` 看得到對應物件;告警不亮。
- 還原:`restore-drill.sh` 印 `restore drill: OK ... rto_seconds=N`;還原後 `exchangectl admin trial-balance` balanced、`exchangectl book ETH-USDC` 的 `last_seq` 等於 `trading.market_sequences.last_seq`、chain role `readyz` 綠(nonce 對帳通過)、一筆小額提現走完。
- 自動化:整合測試 `TestBackupRecordReachesTheOperator`(角色權限、最新列、gauge、system status);CI `e2e` job 的 `backup-drill` 步驟(MinIO → `backup.sh once` → `restore-drill.sh`),log 在 artifact `backup-drill-log`。

### 演練紀錄

| 日期 | 環境 | dump 大小 | 資料量 | RTO(秒) | 備註 |
|---|---|---|---|---|---|
| 2026-09-08 | 本機檔案模式(`BACKUP_STORE=file://`,4 vCPU,Postgres 16 同機),`exchange_loadgen` = `docs/loadtest.md` §8 壓測後的資料 | 25.7 MB(`-Fc -Z 6`,`pg_dump` 3 秒) | 86,739 entries / 231,268 postings / 59,100 orders / 11,500 trades / 217,398 outbox / 501 users | **8** | 還原(`pg_restore -j 2`)約 5 秒 + 檢查;不含從儲存端下載 |
| 2026-09-08 | 本機 S3 模式(`BACKUP_STORE=s3`,MinIO 與 Postgres 16 同機),同一份 `exchange_loadgen` | 25.7 MB | 同上 | **14** | `backup.sh once`(WAL 出貨 + dump 上傳)之後 `restore-drill.sh` 從 MinIO 下載再還原;含下載 |
| (CI) | compose + MinIO,`e2e` job 的 `backup-drill` 步驟 | artifact `backup-drill-log` | e2e 的資料 | log 尾行 `rto_seconds` | 含 MinIO 下載;每個 PR 一次 |

RPO:dump 24 小時;WAL 歸檔閒置最多 15 分鐘(`archive_timeout=900`)、忙碌時一段 16 MB 就歸檔,出貨延遲 ≤ 30 秒——但 WAL 目前只能配合手動 `pg_basebackup` 使用(見 PITR),所以**可還原的 RPO 就是 24 小時**。要縮短就把 `BACKUP_INTERVAL` 調小(dump 3 秒、25 MB 的成本可以每小時一次)。

## 相關指標

`backup_last_success_timestamp_seconds{kind}`(admin role,每 30 秒讀 `admin.backups`)、`pg_stat_archiver`(Postgres 內部,沒有 exporter;用 psql 看)、`node_filesystem_avail_bytes`(prod overlay 的 node-exporter,`DiskAlmostFull`)。
