# ADR-0012:CI 跑在自建 runner 上,GitHub 托管只當退路

- 狀態:已採納(Accepted)
- 日期:2026-09-09
- 相關:`.github/workflows/ci.yml`、`.github/actions/reclaim-disk/action.yml`、`docs/guides/self-hosted-runner.md`、`docs/plan-v1.0.md` §13.3(CI 的 DoD);ADR-0008(工具鏈釘版)、ADR-0009(beta 形態)

## 背景

倉庫是私有的,私有倉庫的 GitHub Actions **每一分鐘都計費**;公開倉庫的標準 runner 免費。使用者已升到 Pro(每月 3,000 分鐘)仍然月月超標。

用 GitHub 的 API 量了建庫後 3.2 天、88 次 run(每個 job 的秒數 `ceil(x/60)` 相加,這就是 GitHub 的計費規則):

| 指標 | 數字 |
|---|---|
| 3.2 天計費 | 2,492 分鐘 |
| 換算每月 | 約 23,400 分鐘 = 額度的 **7.8 倍** |
| 每日 run 數 | 28 / 29 / 23 |
| 一次完整 PR run | **45 分鐘**(牆鐘只有 25) |
| 失敗的 run | 688 分鐘(28%) |
| 被取消的 run | 210 分鐘(8%) |
| 合併到 main | 674 分鐘(27%) |

牆鐘 25 分鐘卻計費 45 分鐘,是因為計費是**每個 job 各自無條件進位到整分鐘後相加**,不是看整次 run。九個平行的 job 就進位九次。

各 job 的計費佔比(3.2 天):integration 25%、unit 16%、lint 15%、e2e 13%、image 9%、fuzz-smoke 9%、fast-checks 與它併掉的三個 7%、helm 3%。量到的 setup / teardown 佔比:`fast-checks` **71%**(55 秒裝工具鏈、23 秒做事)、`lint` 25%、`unit` 16%。

**關鍵算術**:每月約 810 次 run 要塞進 3,000 分鐘,等於每次 run 只能 **3.7 分鐘**。這個 stack 光是一次 `go build -race`、一整套 testcontainers、十幾個容器的 compose、一個 kind 叢集,任何一項都超過 3.7 分鐘。把所有 workflow 層面的優化做滿約省三分之一(45 → 30 分鐘),仍是額度的五倍。

也就是說:**這不是優化題,是選址題。** 已經在做的三件事不動——`concurrency.cancel-in-progress`、合併到 main 只跑兩個 job、docs-only PR 跳過——它們已經是這類題目的標準答案。

## 決定

### 1. 日常 CI 跑在自建 runner 上,一個倉庫變數當開關

九個 job 裡的八個改成:

```yaml
runs-on: ${{ vars.CI_RUNNER || 'ubuntu-latest' }}
```

倉庫變數 `CI_RUNNER` 設成 runner 的標籤就全部走自建;**刪掉變數就全部回到 GitHub 托管**,不用 commit、不用 review、一秒生效。退路必須是一個動作,否則機器掛掉的那天壓力會逼出壞決定。

例外是 `release`,寫死 `ubuntu-latest`:它一個月跑幾次、要推 ghcr 與開 GitHub Release,而發布不該依賴一台家用機器有沒有開著。

考慮過但不採納的替代方案:

- **把倉庫改公開**——公開倉庫免費。使用者明確要求維持私有,不再討論。
- **只靠優化**——上面的算術否決了它。優化仍然全做(見決定 3),但它是給退路用的,不是解法。
- **付費加購分鐘**——每月約 20,000 分鐘的超額,長期成本遠高於一台閒置機器,而且用量還在長。

### 2. 三個 runner 實例,共用一個 Docker daemon,清理必須加時間過濾

一個 runner 一次只跑一個 job;一次完整 run 是 45 分鐘的 job 時間,單一 runner 就是 45 分鐘牆鐘。三個實例把牆鐘壓回約 15 分鐘,和托管相當。

三個實例共用同一個 Docker daemon,所以**每一個 prune 都必須帶 `until` 過濾**:`image` job 裡一句沒有過濾的 `docker system prune -a` 會刪掉旁邊 `e2e` job 正在用的容器與 image。`.github/actions/reclaim-disk/action.yml` 因此是 `container prune --filter until=6h`、`image prune --filter until=72h`、`builder prune --filter until=72h`,而且完全不碰 volume——`docker volume prune` 沒有 `until`,分不出死的和活的。volume 交給 `compose down -v`、testcontainers 的 Ryuk,和停掉 runner 之後的每週深度清理。

清理步驟只在 `vars.CI_RUNNER != ''` 時執行:托管 runner 整台都會被丟掉,在上面清理是花錢整理一台即將刪除的 VM。

### 3. workflow 層面的削減照樣全做

因為退路要夠便宜,而且自建 runner 的時間也是時間:

- **每個 job 加 `timeout-minutes`**。原本零個,全部吃 GitHub 預設的 **360 分鐘**,而 `scripts/backup.sh` 有兩個沒有次數上限的 `until … sleep 5`、`scripts/e2e.sh` 三處 `compose up --wait` 沒有 `--wait-timeout`。任何一次卡住 = 360 分鐘 = 一次燒掉 12% 的月額度。三個沒有上限的等待也一併加了上限。
- **`lint` + `unit` + `fast-checks` 併成一個 `checks`**。三個 job 各裝一次同樣的 Go 工具鏈、各被進位一次:12.1 分鐘的帳單換約 8 分鐘的工作。工具鏈改成用到才裝,所以 `make lint` 失敗不會先付一套 Solidity 工具鏈的錢。
- **`test-fuzz` 直接指名 `./internal/matching/`**。原本用 `go test -list 'Fuzz.*'` 掃全部 43 個 package 找唯一一個 target,每個 package 都要連結一個測試二進位檔——四分鐘的帳單換一句 grep 的答案。`checks` job 加一句 grep 斷言,新增的 fuzz target 會變成紅燈而不是沒人跑。
- **停止每個 commit 都廢掉 image 快取**。`build/Dockerfile` 把 `VERSION` / `COMMIT` / `DATE` 烤進 ldflags,而 workflow 傳的是 `github.sha` 與 PR 的 `updated_at`,每個 commit 都變,所以最後那層 `go build` 的快取**依設計不可能命中**。PR 的建置不會被發布,改成固定值;push 與 tag 維持真實 metadata。
- **Chromium 只下載一次**。`npm ci` 觸發 `@playwright/test` 的 postinstall 下載一次,`playwright install` 再下載一次,而只有後者能一併裝系統相依套件。前者永久關掉。
- **artifact 保留 7 天**,`backup-drill-log` 只在失敗時上傳,`DOCKER_BUILD_SUMMARY` 與 `DOCKER_BUILD_RECORD_UPLOAD` 關掉。
- **`setup-go` / `setup-node` 只在托管 runner 上用快取**(`cache: ${{ vars.CI_RUNNER == '' }}`):自建機器的 `~/go/pkg/mod` 與 `~/.cache/go-build` 本來就在自己的磁碟上,上傳到 GitHub 買不到任何東西——量到 `lint` 的 `Post Run setup-go` 單獨就 82 秒。

## 後果

- 預期每月計費從約 23,400 分鐘降到 **50~100 分鐘**(只剩 `release`),額度使用率 780% → 約 3%。
- **`checks` 是循序的**:lint 失敗會擋住 unit,要多推一次才知道第二個失敗。這是刻意的取捨——失敗的 run 佔帳單 28%,提早停損比一次報完所有失敗值錢。步驟因此照「最便宜、最常失敗」排序。
- **摘要列的名字變少**:紅燈寫 `checks` 而不是 `contracts`。GitHub 仍會指出失敗的 step,失去的只是摘要那一行的名字。
- **自建 runner 是單點**:機器掛掉、睡著、或 Windows 自動更新重開,CI 就停。退路是刪一個變數,但那時又開始計費——所以決定 3 的優化不能省。
- **安全邊界變了**:自建 runner 執行的是分支上的任意程式碼。私有倉庫 + 單人提交風險可控,但 `docs/guides/self-hosted-runner.md` 把三件事寫成必做:機器上不放別的東西、fork PR 全關、機器當拋棄式的看待。Windows 路線還多一條——自動登入代表實體接觸就等於登入。
- **本機沒有 Docker,`e2e` / `integration` / `helm` 的改動只能靠 CI 驗證**。這反過來是自建 runner 最大的附帶價值:驗證這類改動的邊際成本會變成零。
- 沒做但值得做的兩件事留在後面:app image 一次 run 建三次(`image`、`helm`、e2e 的 `compose up --build`)應該改成建一次、三處載入;`test/integration/` 約 185 次 Postgres 容器啟動應該改成共用一個容器、每個測試一個 database。兩件都要動測試碼或 compose,風險與這次的 workflow 改動不同量級,而且搬到自建 runner 之後它們只影響牆鐘、不影響帳單。
