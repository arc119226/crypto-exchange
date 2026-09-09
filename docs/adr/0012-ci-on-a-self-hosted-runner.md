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

**關鍵算術**:每月約 810 次 run 要塞進 3,000 分鐘,等於每次 run 只能 **3.7 分鐘**。這個 stack 光是一次 `go build -race`、一整套 testcontainers、十幾個容器的 compose、一個 kind 叢集,任何一項都超過 3.7 分鐘。而把所有 workflow 層面的優化做滿之後**實測只從 45 分鐘降到 44 分鐘**(逐 job 的對照表在「後果」),仍是額度的七倍多。

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

### 2. 兩個 runner 實例,共用一個 Docker daemon,清理必須加時間過濾

一個 runner 一次只跑一個 job,所以牆鐘由實例數與 job 依賴圖一起決定。這條原本寫「三個實例把牆鐘壓回約 15 分鐘」,那是 45 分鐘 job 時間除以三推出來的,沒有看依賴圖。**實際的圖是 `checks` → `integration` → (`helm` ∥ `e2e` ∥ `image`),前兩段是串的,第三段的長度由最長的 `helm` 決定。** 用一台 runner 上量到的每個 job 的秒數(見「後果」)推:

| runner 數 | 第三段 | 總牆鐘 |
|---|---|---|
| 1 | 7m12 + 3m26 + 4m15 依序 = 14m53 | **25m05(實測)** |
| 2 | helm 7m12 ∥ (e2e 3m26 → image 4m15 = 7m41) = 7m41 | 約 18m |
| 3 | max(7m12, 3m26, 4m15) = 7m12 | 約 17m |

**第三個實例只買到約半分鐘**,因為它只是讓 `image` 不必等 `e2e`,而兩者相加仍短於 `helm`。所以是兩個,不是三個。

兩個實例共用同一個 Docker daemon,所以**每一個 prune 都必須帶 `until` 過濾**:`image` job 裡一句沒有過濾的 `docker system prune -a` 會刪掉旁邊 `e2e` job 正在用的容器與 image。`.github/actions/reclaim-disk/action.yml` 因此是 `container prune --filter until=6h`、`image prune --filter until=72h`、`builder prune --filter until=72h`、`network prune --filter until=6h`,而且完全不碰 volume——`docker volume prune` 沒有 `until`,分不出死的和活的。volume 交給 `compose down -v`、testcontainers 的 Ryuk,和停掉 runner 之後的每週深度清理。

清理步驟只在 `vars.CI_RUNNER != ''` 時執行:托管 runner 整台都會被丟掉,在上面清理是花錢整理一台即將刪除的 VM。

### 3. workflow 層面的削減照樣全做

因為退路要夠便宜,而且自建 runner 的時間也是時間:

- **每個 job 加 `timeout-minutes`**。原本零個,全部吃 GitHub 預設的 **360 分鐘**,而 `scripts/backup.sh` 有兩個沒有次數上限的 `until … sleep 5`、`scripts/e2e.sh` 三處 `compose up --wait` 沒有 `--wait-timeout`。任何一次卡住 = 360 分鐘 = 一次燒掉 12% 的月額度。三個沒有上限的等待也一併加了上限。
- **`lint` + `unit` + `fast-checks` 併成一個 `checks`**。三個 job 各裝一次同樣的 Go 工具鏈、各被進位一次:12.1 分鐘的帳單換約 8 分鐘的工作。工具鏈改成用到才裝,所以 `make lint` 失敗不會先付一套 Solidity 工具鏈的錢。
- **`test-fuzz` 直接指名 `./internal/matching/`**。原本用 `go test -list 'Fuzz.*'` 掃全部 43 個 package 找唯一一個 target,每個 package 都要連結一個測試二進位檔——四分鐘的帳單換一句 grep 的答案。`checks` job 加一句 grep 斷言,新增的 fuzz target 會變成紅燈而不是沒人跑。
- **停止每個 commit 都廢掉 image 快取**。`build/Dockerfile` 把 `VERSION` / `COMMIT` / `DATE` 烤進 ldflags,而 workflow 傳的是 `github.sha` 與 PR 的 `updated_at`,每個 commit 都變,所以最後那層 `go build` 的快取**依設計不可能命中**。PR 的建置不會被發布,改成固定值;push 與 tag 維持真實 metadata。
- ~~**Chromium 只下載一次**~~——**這一項是錯的,已撤銷**。原本以為 `npm ci` 會觸發 `@playwright/test` 的 postinstall 下載一次、`playwright install` 再下載一次。但 `web/trade/package-lock.json` 是 lockfile v3,全檔只有 `fsevents` 帶 `hasInstallScript`,Playwright 的三個套件都沒有 install script,所以 `npm ci` 從來沒有下載過瀏覽器。改動因此是淨負面(還拿掉了一個能用的逃生口),已還原。記在這裡是因為它示範了一件事:**「優化」在量測之前只是猜測**,而這份 ADR 的其他數字都是量出來的。
- **artifact 保留 7 天**,`backup-drill-log` 只在失敗時上傳,`DOCKER_BUILD_SUMMARY` 與 `DOCKER_BUILD_RECORD_UPLOAD` 關掉。
- **`setup-go` / `setup-node` 只在托管 runner 上用快取**(`cache: ${{ vars.CI_RUNNER == '' }}`):自建機器的 `~/go/pkg/mod` 與 `~/.cache/go-build` 本來就在自己的磁碟上,上傳到 GitHub 買不到任何東西——量到 `lint` 的 `Post Run setup-go` 單獨就 82 秒。

### 6. Windows 機器上用 WSL 裡的 Docker Engine,不用 Docker Desktop

使用者的機器是 Windows 11 且已裝 Docker Desktop,直覺是直接用它。查證之後改成以 WSL 裡的 `docker-ce` 為主線:

- **Docker Desktop 必須有人登入 Windows 才會跑,任何付費層級都沒有無頭或服務模式。** Docker 自己的 roadmap issue #515 至今未解,Docker Desktop 也完全不支援 Windows Server;`com.docker.service` 只是 Hyper-V 與 Windows 容器的特權輔助程式,WSL2 模式下不會自動啟動。
- **4.75.0 起 `/var/run/docker.sock` 的符號連結在 WSL 重啟後不會被重建**,手動修的變通做法重開機又沒了。這台機器每月會被 Windows Update 重開一次,等於週期性斷線。**這一項在第一次實跑就被觀測到**:改 `.wslconfig` 後的 `wsl --shutdown` 之後,互動 shell 裡的 `docker version` 正常,但 `image` job 在 `docker/setup-buildx-action` 一秒內死於 `dial unix /var/run/docker.sock: connect: no such file or directory`。寫這條決定時它還只是引用別人的 bug 報告。
- **Docker Desktop 的資料在自己的 `docker_data.vhdx`**,和 Ubuntu 的 `ext4.vhdx` 是不同的虛擬磁碟,官方沒有支援的縮小方法。放在 WSL 裡則可以用 `wsl --manage <distro> --compact` 真的把空間還給 Windows。

Docker Desktop 仍寫進 guide 當替代路線,把上面三項代價寫清楚,讓使用者自己選。

**這不解決登入問題**:WSL 本身也無法在登入前啟動(Microsoft 列為已知問題,session 0 不支援),所以自動登入無論走哪條路線都是必要的。換 Docker 引擎買到的是「不會週期性斷線」與「磁碟收得回來」。

## 後果

- **workflow 層面的優化實測只省了 1 分鐘,不是原本預估的 15 分鐘。** 拿 run #86(改動前)對 run #89(改動後),同一條分支、同樣是 PR 事件,逐 job 比:

  | job | #86 前 | #89 後 | 差 |
  |---|---|---|---|
  | lint + unit + fast-checks → `checks` | 5 + 6 + 2 = 13 | 11 | −2 |
  | integration | 12 | 11 | −1 |
  | e2e | 8 | 11 | **+3** |
  | helm | 8 | 7 | −1 |
  | image | 4 | 4 | 0 |
  | **合計(計費)** | **45** | **44** | **−1** |
  | 牆鐘 | 25 | 31 | +6 |

  合併 job 本身確實省了 2 分鐘,方向是對的。但 e2e 多花的 3 分鐘全部在 `actions/setup-go` 裡——還原加上傳從 93 秒變成 249 秒。`Post Run` 只在主鍵沒命中時才上傳,而 #89 幾乎每個 job 都在上傳(checks 80s、integration 68s、helm 65s、e2e 146s),表示 Go 快取整批 miss。`go.sum` 沒動,所以合理的解釋是**倉庫 10 GB 的 Actions 快取配額被擠爆**:三個 docker build 的 `cache-to: type=gha,mode=max` 和 Go 快取共用同一個配額。

  這強化而不是削弱本 ADR 的結論——**托管 runner 上連快取都不穩定,而自建 runner 上 `cache: ${{ vars.CI_RUNNER == '' }}` 讓這 249 秒整個消失**。同時它也說明為什麼決定 3 的優化不能當成解法:它們是給退路用的。
- 一次 A/B 不是量測。上面的數字本身也有抖動,最終要看的是 Settings → Billing 跑一週的實際數字。
- **搬到自建 runner 之後的第一次全綠 run(一台 runner,`runner_id` 全部相同):**

  | job | 耗時 |
  |---|---|
  | checks | 2m20s |
  | integration | 7m39s |
  | helm | 7m12s |
  | e2e | 3m26s |
  | image | 4m15s |
  | **牆鐘** | **25m05s** |
  | **計費** | **0 分鐘** |

  對照托管的 #89(44 計費分鐘 / 31 分鐘牆鐘):**一台自建 runner 已經比托管的三台平行還快,而且不計費。** 快的來源不是機器比較猛,是不必再等網路——三項步驟級的證據:

  - **`actions/setup-go` 兩端都是 0 秒**(還原與 `Post Run` 都是)。上一條推論「10 GB 快取配額被三個 `type=gha,mode=max` 擠爆」並預測 `cache: ${{ vars.CI_RUNNER == '' }}` 會讓 e2e 那 249 秒消失——**量到 0 秒,預測成立**。
  - **Playwright 那一步 10 秒**。`PLAYWRIGHT_INSTALL_DEPS` 只關掉 `--with-deps`、保留瀏覽器下載,而瀏覽器已在 `~/.cache/ms-playwright`,所以整步是 no-op。上面那條被撤銷的 Chromium 猜測,正確的版本長這樣。
  - **同一個 app image 在一次 run 裡建了兩次**:`helm` 的「build the image under test」4m19s、`image` 的「build (load locally)」2m40s,合計約 7 分鐘,佔 25 分鐘牆鐘的 **28%**。這是下面「沒做但值得做」那一項第一次有數字。
- 預期每月計費從約 23,400 分鐘降到 **50~100 分鐘**(只剩 `release`),額度使用率 780% → 約 3%。
- **`checks` 是循序的**:lint 失敗會擋住 unit,要多推一次才知道第二個失敗。這是刻意的取捨——失敗的 run 佔帳單 28%,提早停損比一次報完所有失敗值錢。步驟因此照「最便宜、最常失敗」排序。
- **摘要列的名字變少**:紅燈寫 `checks` 而不是 `contracts`。GitHub 仍會指出失敗的 step,失去的只是摘要那一行的名字。
- **自建 runner 是單點,而 Windows 路線的單點特別脆**:三件事疊在一起——WSL 無法在登入前啟動(session 0 不支援)、未公開的 `instanceIdleTimeout` 預設 15 秒就終止發行版、Windows Update 每月大約重開一次(2026 年 7 月起合併成每月一次)。所以 guide 把「重開機之後不碰任何東西,runner 要自己回到 Idle」列為**必測項目**,沒過就不算裝好。退路是刪掉倉庫變數,但那時又開始計費。
- **安全邊界變了**:自建 runner 執行的是分支上的任意程式碼。私有倉庫 + 單人提交風險可控,但 `docs/guides/self-hosted-runner.md` 把三件事寫成必做:機器上不放別的東西、fork PR 全關、機器當拋棄式的看待。Windows 路線還多一條——自動登入代表實體接觸就等於登入。
- **本機沒有 Docker,`e2e` / `integration` / `helm` 的改動只能靠 CI 驗證**。這反過來是自建 runner 最大的附帶價值:驗證這類改動的邊際成本會變成零。
- 沒做但值得做的兩件事留在後面:app image 一次 run 建兩次(`image` 與 `helm` 各一次,約 7 分鐘、佔牆鐘 28%;e2e 走 compose 用既有的)應該改成建一次、多處載入;`test/integration/` 約 185 次 Postgres 容器啟動應該改成共用一個容器、每個測試一個 database。兩件都要動測試碼或 compose,風險與這次的 workflow 改動不同量級,而且搬到自建 runner 之後它們只影響牆鐘、不影響帳單。
