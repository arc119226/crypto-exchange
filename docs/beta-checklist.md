# Beta 上線檢查表

Beta = 自營、封閉、單台 VM、只接 Sepolia(`docs/plan-v1.0.md` §0、§16;部署步驟在 `docs/runbooks/beta-deploy.md`)。這份表寫的是**上線那一天要打勾的東西**、上線期間的節奏、以及這個版本刻意沒有做的事——後者是讓下一個人不用重新發現的部分。

## 這個 beta 的限制(讀完再上線)

- **單機、單 engine**:一台 VM,一個 engine 程序持 advisory lock;engine 重啟 = 幾秒到幾十秒的下單 503(`docs/runbooks/engine-restart.md`)。沒有 HA。
- **只接 Sepolia,沒有真錢**:`ETH_CHAIN_ID` 固定 11155111,主網不在任何檔案裡。
- **可還原的 RPO = 24 小時**:每日 `pg_dump`;WAL 有歸檔但沒有 base backup,所以 PITR 只是文件(`docs/runbooks/backup-restore.md`)。**RTO 本機實測 8 秒**(25.7 MB、231k postings),CI 每個 PR 再量一次(artifact `backup-drill-log`)。
- **還原之後熱錢包 nonce 要人工對帳**(R2/R3),提現在對完之前不能開。
- **沒有 Alertmanager**:告警只在 Prometheus UI 與 Grafana 上。要嘛有人每天看,要嘛接一個外部 Alertmanager。
- **後台只在 127.0.0.1:8082**,經 SSH tunnel;沒有 VPN、mTLS、WAF。
- **secrets 是主機上的檔案**(`secrets/prod/`),沒有 Vault/KMS;丟了 VM 又沒有 escrow,seed 就沒了。
- **對帳每 5 分鐘一輪、沒有閾值**:任何非零 `DIFF` 都是告警(`docs/runbooks/reconciliation-break.md`)。
- **手續費的程式做好了,但費率出廠是 0**(計畫 v1.1 §23、Phase 8;ADR-0011):提現與充值都收得了費,後台的資產頁可以隨時設,`GET /admin/v1/reports/revenue`、後台的「營收」頁與 `exchangectl admin revenue` 看得到三種手續費與 gas 支出。**但三個費率的 seed 值都是 0**,所以不設就跟以前一樣:每一筆提現與歸集的 gas 記 `gas_expense`、沒有對應收入。上線前要決定的是**提現手續費要設多少** —— §23.3 的指引是至少蓋住近 7 天該資產提現的 P90 gas;設得太低 `WithdrawalGasExceedsFee` 會提醒。充值手續費照業界慣例維持 0。
- **前台與後台是繁體中文 / 英文雙語**(ADR-0010):切換器在右上角,後台另看 `Accept-Language`。`Problem.detail`、領域驗證訊息、CLI、日誌仍是英文;runbook 對的是狀態碼,badge 的 `title` 保留原始碼。
- **壓測數字**(`docs/loadtest.md` §8):單市場 ~266 orders/s、`POST /v1/orders` p99 在 100 orders/s 時 ~548 ms。§3.3 的 1,000 orders/s 與 50 ms p99 **未達**,beta 的流量規模應該遠低於此。
- **飽和時的推播延遲會從約 20 ms 變成約 640 ms**,行情的 depth delta 同樣(§3.3 的目標是 200 / 300 ms)。這不是退步,是 Phase 7 群組提交刻意的取捨:事件要等該組 COMMIT 才進 outbox,換來約 2.1 倍吞吐。旋鈕是 `ENGINE_BATCH_SIZE`——佇列空時每組只有一個命令,延遲就跟單命令一樣。**低負載下應該回到 20 ms 等級,但那是推論,沒有量過**;beta 期間值得拿 `stream_push_delay_seconds` 實際看一眼,而不是假設。

## 上線前

- [ ] VM:`deploy/vm/bootstrap.sh` 跑完;`ufw status` 只有 22/80/443;`docker compose version` ≥ 2.24。
- [ ] `sudo scripts/gen-prod-secrets.sh` 跑完;助記詞**離線產生**、匯入後原檔 `shred`;`HOT_WALLET_ADDRESS` 填入 `.env.prod`。
- [ ] **Escrow**:`secrets/prod/` 加密後放在兩個與 VM 無關的地方、兩個人各持解密方式(`docs/runbooks/backup-restore.md`)。日期:＿＿＿＿
- [ ] `.env.prod`:三個 image tag 相同、`EDGE_DOMAIN` 的 DNS 指到 VM、`ETH_RPC_URL`(provider key)、`ETH_SCAN_START_BLOCK`、`BACKUP_S3_ENDPOINT` 是**另一台機器**。
- [ ] Sepolia fixtures 就位(`docs/guides/sepolia.md`);熱錢包有 ≥ 0.1 Sepolia ETH。
- [ ] `systemctl start exchange` → `make ps-prod` 全 healthy;`exchange version` == tag(`docs/runbooks/beta-deploy.md` 第 6–7 步)。
- [ ] 第一個管理員:bootstrap 密碼已換、TOTP 已啟用、第二個管理員已建(`docs/runbooks/admin-totp.md`)。
- [ ] Smoke:註冊 → 注資 → 掛單 → 成交 → 一筆小額提現 `confirmed` → `exchangectl admin reconcile` 全 0。
- [ ] 備份:`run --rm backup once` 一列 ok;**演練一次**並把 RTO 寫進 `docs/runbooks/backup-restore.md` 的表。日期:＿＿＿＿ RTO:＿＿＿＿
- [ ] 密鑰輪替**乾跑**一次(`docs/runbooks/key-rotation.md`):任一把主金鑰 rewrap 到新值再 rewrap 回來,或 JWT 的 B→C 兩步。日期:＿＿＿＿
- [ ] 告警接收人:名字＿＿＿＿;看得到 `http://127.0.0.1:9090/alerts`(tunnel)或已接 Alertmanager;`BackupNeverTaken` 24 小時內不能響。
- [ ] 每本 runbook 由**不是作者的人**讀過一遍:`docs/runbooks/engine-restart.md`、`docs/runbooks/stuck-withdrawal.md`、`docs/runbooks/reorg-alert.md`、`docs/runbooks/hot-wallet-low.md`、`docs/runbooks/reconciliation-break.md`、`docs/runbooks/backup-restore.md`、`docs/runbooks/key-rotation.md`、`docs/runbooks/admin-totp.md`、`docs/runbooks/beta-deploy.md`。

## 上線期間的節奏

| 頻率 | 做什麼 | 依據 |
|---|---|---|
| 每天 | 看 Prometheus alerts、後台首頁(Last backup、trial balance、breaks、pending withdrawals)、`hot_wallet_balance` | `docs/runbooks/hot-wallet-low.md` |
| 每天(自動) | `pg_dump` + WAL 出貨;`admin.backups` 一列 ok | `docs/runbooks/backup-restore.md` |
| 每 5 分鐘(自動) | 對帳;非零就是 `ReconciliationBreak` | `docs/runbooks/reconciliation-break.md` |
| 每週 | 還原演練(`make backup-drill` 或對 VM 的 dump 做 file 模式),RTO 寫進表 | `docs/runbooks/backup-restore.md` |
| 每 90 天 | 三把主金鑰 + JWT 簽章金鑰輪替;之後**更新 escrow** | `docs/runbooks/key-rotation.md` |
| 人員異動 | DB / NATS / Redis 密碼、`ADMIN_API_KEY`、keystore passphrase;更新 escrow | `docs/runbooks/key-rotation.md` |
| 換版本 | `.env.prod` 三個 tag → `make up-prod` → 第 6–9 步 | `docs/runbooks/beta-deploy.md`、`docs/release.md` |

## 刻意沒做(v1.0 之外)

多副本 engine、managed K8s(chart 只在 kind 驗證,ADR-0009)、PITR 演練、Vault/KMS、Alertmanager、blue/green、mTLS/WAF、還原後 nonce 的自動對帳、`fill.executed` 帳戶級事件。清單與理由在 `docs/domain.md` §26。
