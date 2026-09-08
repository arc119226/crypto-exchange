# ADR-0009:Beta 形態、chart 的依賴、group commit 的交易邊界、備份政策

- 狀態:已採納(Accepted)
- 日期:2026-09-08
- 相關:`docs/plan-v1.0.md` §5.1、§7.3、§11、§12 Phase 7、§14、§16;`docs/domain.md` §26;PR #27

## 背景

Phase 7 要把系統交給營運方,四件事沒有計畫書上的唯一答案:(1) beta 跑在哪裡——§16 寫「單台 VM compose 或單節點 k3s」;(2) Helm chart 的依賴(Postgres / NATS / Redis / anvil)怎麼給——§12 寫「subchart 或外部 values 二選一」;(3) 引擎的 group commit 把幾個命令放進一筆交易、失敗了怎麼辦;(4) 備份要多細——每日 dump、WAL 歸檔、PITR,還是全部。

## 決定

### 1. Beta 是單台 VM 跑 compose;chart 只在 CI 的 kind 上驗證

`deploy/compose/compose.prod.yaml` 疊在 `compose.yaml` 與 Sepolia overlay 上,是 beta 唯一的部署方式。Helm chart(`deploy/helm/exchange`)每個 PR 在 kind 上安裝並跑 e2e,證明「搬得過去」(§16 的工程承諾),但不是 beta 的形態。

理由:beta 是自營封閉、單 engine、一個人運維;k3s 帶來的東西(排程、rollout、Secret 物件)在單節點上沒有一樣是 compose 給不了的,而多出來的是一整層要學要修的東西。chart 存在的價值是日後 managed K8s 的路徑,那條路徑在 CI 上每天被驗證,不需要 beta 走它。

### 2. Chart 的依賴內建、只在 `dev.enabled=true` 時 render、而且是 hook

不用 Bitnami 或其他 subchart。chart 自帶最小的 postgres / nats / redis / anvil Deployment(emptyDir)與一個 bootstrap Job(部署合約、seed、admin bootstrap),只有 `dev.enabled=true` 才 render;正式部署把 values 指向外部服務。這些依賴是 `pre-install,pre-upgrade` hook(權重 −10),migrate Job 是權重 0 的 hook,bootstrap Job 權重 5。

理由:subchart 的版本、values 結構、安全預設都是別人的,而我們只需要「CI 上有一個能用的 Postgres」;內建的 Deployment 幾十行,而且 image tag 與 compose 同一份(`helm_test.go` 核對)。做成 hook 不是選擇而是必要:migrate 是 pre-install hook,它跑在一般資源建立之前,等同 chart 的 Postgres 若是一般資源就永遠等不到。後果是每次 `helm upgrade` 重建 dev 依賴,資料歸零——對 CI 與 kind 這正是要的(§16「每次乾淨部署」);`helm uninstall` 不刪 hook 資源,CI 直接刪叢集,人用 `kubectl delete -l app.kubernetes.io/instance=<rel>`(NOTES.txt)。另一條路(獨立的 `exchange-dev-deps` chart 先裝)少了 hook 的怪異但多了一個要維護、要對版本的 chart,不選。

### 3. Group commit:一組 ≤ 50 個命令一筆交易,商業失敗在 savepoint 裡,基礎設施失敗整組退回逐筆

runner 一次撈最多 `ENGINE_BATCH_SIZE`(預設與上限 50)個命令放進同一筆 Postgres 交易;每個命令一個 `SAVEPOINT`,餘額不足、進簿後拒絕、replay、終態取消這些**商業結果**是那個命令自己的回覆,交易繼續;DB 錯誤、deadlock、序號衝突、簿不一致這些**基礎設施失敗**讓整組 ROLLBACK、簿標髒重建,然後**用同一份程式碼一次一個命令重跑這一組**。回覆一律在 COMMIT 之後送。撈取遇到查詢、或同帳戶同 `client_order_id` 的第二張單就停,那個請求下一組再處理;同一組內「先成交後取消」用記憶體裡的列回覆,不消耗序號。

理由:交易邊界不變(一筆交易要嘛全部 commit 要嘛全部 rollback)、事件契約不變(市場 seq 與 outbox id 同向、每帳戶 `account_seq` 與 id 同向、一個命令的事件連續),外界看到的簿永遠等於已提交狀態——只是「已提交」的粒度從一個命令變成一組。上限 50 不往上開:一組失敗就是一次重建,而且 cmdbus 呼叫端的 5 秒逾時要蓋得住一組的 10 秒預算之內大部分的情況。退回逐筆而不是「重試整組」,因為壞的往往是一個命令(deadlock 除外),逐筆重跑讓其他 49 個不陪葬。

### 4. 備份 = 每日 `pg_dump` + WAL 歸檔到 S3 相容儲存;演練只還原 dump;PITR 只寫文件

sidecar(postgres image + `mc`)每 24 小時 `pg_dump -Fc`、每 30 秒把 Postgres `archive_command` cp 到本機 volume 的 WAL 段出貨,兩者按天數保留,每個結果一列 `admin.backups`,admin role 把最新成功列變成指標與告警。CI 每個 PR 做一次「dump → 還原到拋棄式 DB → 驗不變量 → 印 RTO」。**沒有** base backup,所以 WAL 現在不能用來做時間點還原;PITR 的做法只寫在 runbook。

理由:dump 是能演練、能量 RTO、能在任何 Postgres 上還原的東西;WAL 歸檔的成本幾乎是零(`archive_command` 只是本機 cp,儲存端故障永遠不會卡 Postgres)而且是日後補 base backup 就能 PITR 的材料。`pg_basebackup` 要 REPLICATION 權限與 `pg_hba` 放行遠端 replication,stock image 不放行,做進 sidecar 就要自訂 Postgres image——beta 的 RPO 是 24 小時(可以把 `BACKUP_INTERVAL` 調到每小時,dump 3 秒),寫進 `docs/beta-checklist.md` 當已知限制。**備份範圍不只 DB**:`secrets/prod/`(seed、passphrase、JWT 私鑰、主金鑰)不在 dump 裡,沒有它們的還原簽不了一筆提現;runbook 與 checklist 要求離線加密 escrow。

## 後果

- 想上 managed K8s 的時候,chart 是現成的,但 dev 依賴要換成外部 values;hook 式依賴不適合生產。
- 引擎的一次重啟或一組失敗,等待中的呼叫者最多 50 個一起拿 503;客戶端要能用 `client_order_id` 重送。
- 還原 = 最多 24 小時的資料損失 + runbook 的 R1–R10 人工步驟(purge JetStream、帳戶序號跳號、熱錢包 nonce 人工結案)。
- 密鑰輪替(§14)與備份是兩條線:輪替後要更新 escrow,還原後要確認 escrow 的密鑰是當時的那一套。
