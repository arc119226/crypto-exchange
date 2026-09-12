# ADR-0012:CI 跑在自建 runner 上,GitHub 托管只當退路

- 狀態:已採納(Accepted),2026-09-12 修訂(Amended)——見文末「修訂:倉庫公開之後」
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

**`release` 是唯一的例外。** 2026-09-09 曾經短暫有第二個,那件事值得完整記下來,因為錯誤訊息離根因有三層,而看起來最像的假設全都是錯的。

**症狀。** `image` job 在自建 runner 上死於一行 `##[error]Connect Timeout Error`,發生在第 3 個步驟 `docker/metadata-action`,build 連開始都沒有。run 98(pull request)與 run 100(合併到 main,所以 main 紅了)各一次,中間隔 40 分鐘。同一步驟在 09:55 的 run 97 只花 **1 秒**就通過,而且中間沒有任何 workflow 改動。

**三個被排除的假設。** 每一個都很有說服力,每一個都被一項證據推翻:

| 假設 | 推翻它的證據 |
|---|---|
| GitHub 把 Node 20 的 action 強制升到 Node 24 | 那則警告在**成功那一次**的 log 裡就已經在了 |
| WSL 的 IPv6 出不去 | `getent ahosts` 只回 IPv4——根本沒有 AAAA 記錄可以連 |
| WSL2 的 MTU 問題(症狀確實像:有些主機正常、有些慢到爆) | `curl` 的分段計時顯示 TCP 連線 **34 ms**、TLS **37 ms**。路徑好得不能再好 |

**根因。** 分段計時把它指出來:

```
dns=16.092  conn=16.126  tls=16.163  total=16.201
```

16.2 秒裡有 **16.09 秒是 DNS**,其餘三段加起來 0.109 秒。再用 `getent` 往下切一層就釘死了:

```
ahostsv4 api.github.com   0.048s     只問 A
ahosts   api.github.com  17.339s     A + AAAA
ahosts   example.com      0.026s     A + AAAA
```

所以壞的不是 IPv6(`example.com` 同時問 A 和 AAAA 只要 26 ms)、不是這台機器、也不是 GitHub,而是 **`api.github.com` 的 AAAA 查詢沒有人回答**。`/etc/resolv.conf` 指向 `nameserver 10.255.255.254`——WSL 自己的 DNS 代理。查詢石沉大海,glibc 等滿預設的 5 秒 × 2 次再加重試,湊出 17 秒,然後放棄、只回 IPv4。

**為什麼偏偏是 `metadata-action` 死。** 它底層是 Node 的 undici,**connect timeout 預設 10 秒**。16 秒剛好落在錯的一側。`actions/checkout` 沒有這種短逾時,所以它只是慢,不會紅——這就是為什麼整個 job 看起來只有一步壞掉。09:55 那次通過不是狀態比較好,是抽籤抽中。

**修法在機器上,不在 workflow 裡。** `/etc/wsl.conf` 加 `[network] generateResolvConf = false`,`/etc/resolv.conf` 改成靜態的公共 DNS,並加上 `options timeout:2 attempts:2`。最後那一項是結構性的保險:就算之後又有名字沒人回答,最糟也只等 4 秒,**再也撞不到 undici 的 10 秒門檻**。

**期間的繞道與它量到的數字。** 修好之前,`image` 在 push 事件上被釘到 `ubuntu-latest`——理由和 `release` 一樣,發布不該依賴一台家用機器,而 `v*` tag 走的正是同一條 push 路徑,`v0.1.0` 只差這一通打不出去的 API 就會失敗。兩次實跑量到 **4m09**(run 102)與 **3m50**(run 103),都是 **4–5 個計費分鐘**;`fuzz-smoke` 留在自建 runner 上不計費。DNS 修好之後這一行就改回一般的開關了。

**留下來的一件事。** 三個 `metadata-action` 步驟保留了 `if: github.event_name == 'push'`。它的理由跟這次的網路無關:那三個步驟算出來的 tag 與 label **只有 push 步驟在讀**,而那些步驟本來就全是 push-only,所以在 pull request 上它們是三通沒有人讀結果的 API 呼叫,也就是三個白給的失敗點。清理步驟的條件則改了又改回 `vars.CI_RUNNER != ''`:繞道期間用過 `runner.environment == 'self-hosted'`,那個寫法只驗證過它在托管機器上會 skip,沒驗證過它在這台 runner 上會等於 `self-hosted`——猜錯的話清理會安靜地不執行,而症狀要等磁碟滿了才看得到。job 不再換機器之後,原本的條件就精確了。

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

### 7. 工具鏈的安裝是機器設定的一部分,不是 CI 的一部分

自建 runner 上,`foundry-toolchain` 這個 action 不跑;取而代之的是一個斷言:機器上必須已經有釘住的那個版本,然後把 `~/.foundry/bin` 加進 `PATH`。托管 runner 照舊完整安裝與驗證。

**這是被一次故障逼出來的,而它暴露的問題比故障本身大。**

**症狀。** 2026-09-10,`checks` 連續三次死在 `foundryup`,間隔 6 分鐘與 15 分鐘,三次的訊息一字不差:

```
foundryup: found attestation for v1.8.1 version, downloading attestation artifact, checking...
Error:
   0: failed to download https://github.com/foundry-rs/foundry/attestations/43723610/download: HTTP 500 Internal Server Error
```

`githubstatus.com` 上沒有任何事故。同一個步驟在前一天的 PR #36 上只花 5 秒。

**根因不在這台機器,也不在這個分支。** `foundryup` 會把下載的二進位檔比對 GitHub 的 artifact attestation——那是供應鏈驗證,方向是對的,但它是一通額外的對外請求。那台機器上**已經有正確版本的 forge**(log 裡 foundryup 自己說「already installed」),卻仍然為了驗證一份它不需要重新下載的東西而去打那通 API,然後被外面的故障拖下水。

**為什麼這件事比一次 500 嚴重。** 它讓「工具鏈安裝」變成每一個 job、每一次執行的外部依賴,而那個依賴和這個 repo 的內容完全無關。當天紅掉的分支裡**一行 Solidity 都沒有**。同一個 action 也是 `helm` 的第 3 步,所以它擋住的是兩個 job × 每一條分支 × 每一次 push。

**修法,以及它的代價。** Playwright 的系統函式庫早就是「機器設定時裝一次,CI 不碰」(`docs/guides/self-hosted-runner.md` 第 6 節),foundry 現在用同一個形狀。**版本釘沒有消失,它換了地方**:以前由安裝器保證,現在由斷言保證,而斷言失敗時直接印出要打的指令。代價很具體——升 `.env.example` 的 `FOUNDRY_TAG` 之後,那台機器要手動跟上,否則 `checks` 會紅。**這是刻意的**,總比安靜地用舊版編譯合約好。

**考慮過但沒採用的:** `foundryup --force` 會跳過驗證,但 `foundry-toolchain@v1` 沒有任何 input 傳得進去(它的 input 只有 `version`、`network`、`cache`、`cache-key`、`cache-restore-keys`),所以要用它得先在兩個 job 裡把 action 換成直接呼叫;而且關掉一個供應鏈驗證來換 CI 綠燈,在一個 gitleaks 掃全歷史、工具版本全釘死的 repo 裡是反方向的。

**留下來的一件事。** 那個斷言用 `vars.CI_RUNNER != ''` 當「這個 job 在自建 runner 上」的代理,而這只對 `runs-on` 寫成那個開關的 job 成立。目前兩個用到 foundry 的 job 都是;唯一釘死在托管機器上的 `release` 沒有 foundry 步驟。**加第三個 foundry job 時要重新確認這件事**,否則它會在托管機器上走進「機器上已經有了」那條路然後找不到 forge。

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
- **兩個 runner 的第一次全綠 run,牆鐘落在決定 2 推算的區間上——但組成不同。** run 34426360810(`e2e` 與 `image` 同時在 01:48:42 起跑,所以確實是兩台):

  | job | 耗時 |
  |---|---|
  | checks | 45s |
  | integration | 8m06 |
  | e2e | 3m43 |
  | image | 3m32 |
  | helm | 4m49 |
  | **牆鐘** | **17m21**(01:39:45 → 01:57:06) |
  | **計費** | **0 分鐘** |

  決定 2 的表用依賴圖推「兩個 runner → 約 18m」,量到 17m21。**但這不是 `setup-go` 那種強度的驗證**:那個 18m 是拿一台 runner 時的 job 秒數推的(`helm` 7m12、`e2e` 3m26、`image` 4m15),而這次 `helm` 只有 **4m49**、快了 2m23,`integration` 反而慢了 27 秒。總數對上是因為快取更熱,不是因為每一段都預測準了。誠實的說法是**牆鐘落在預測值上,各段的組成不同**;要真的驗證那張表,得在同一批快取狀態下比一台與兩台。
- 預期每月計費從約 23,400 分鐘降到 **50~100 分鐘**(只剩 `release`),額度使用率 780% → 約 3%。
- **`checks` 是循序的**:lint 失敗會擋住 unit,要多推一次才知道第二個失敗。這是刻意的取捨——失敗的 run 佔帳單 28%,提早停損比一次報完所有失敗值錢。步驟因此照「最便宜、最常失敗」排序。
- **摘要列的名字變少**:紅燈寫 `checks` 而不是 `contracts`。GitHub 仍會指出失敗的 step,失去的只是摘要那一行的名字。
- **自建 runner 是單點,而 Windows 路線的單點特別脆**:三件事疊在一起——WSL 無法在登入前啟動(session 0 不支援)、未公開的 `instanceIdleTimeout` 預設 15 秒就終止發行版、Windows Update 每月大約重開一次(2026 年 7 月起合併成每月一次)。所以 guide 把「重開機之後不碰任何東西,runner 要自己回到 Idle」列為**必測項目**,沒過就不算裝好。退路是刪掉倉庫變數,但那時又開始計費。
- **安全邊界變了**:自建 runner 執行的是分支上的任意程式碼。私有倉庫 + 單人提交風險可控,但 `docs/guides/self-hosted-runner.md` 把三件事寫成必做:機器上不放別的東西、fork PR 全關、機器當拋棄式的看待。Windows 路線還多一條——自動登入代表實體接觸就等於登入。
- **本機沒有 Docker,`e2e` / `integration` / `helm` 的改動只能靠 CI 驗證**。這反過來是自建 runner 最大的附帶價值:驗證這類改動的邊際成本會變成零。
- 沒做但值得做的兩件事留在後面:app image 一次 run 建兩次(`image` 與 `helm` 各一次,約 7 分鐘、佔牆鐘 28%;e2e 走 compose 用既有的)應該改成建一次、多處載入;`test/integration/` 約 185 次 Postgres 容器啟動應該改成共用一個容器、每個測試一個 database。兩件都要動測試碼或 compose,風險與這次的 workflow 改動不同量級,而且搬到自建 runner 之後它們只影響牆鐘、不影響帳單。

---

## 修訂:倉庫公開之後(2026-09-12)

這份 ADR 的整套論證建立在第一句上——「倉庫是私有的,私有倉庫的 GitHub Actions 每一分鐘都計費」。倉庫在 ADR-0014 之後公開,那句話不再成立,連帶兩件事跟著變。

**一、成本理由消失,時間理由留著。** 公開倉庫的標準 runner 免費且不計量,所以本文那張「45 → 44 計費分鐘」的表與 3,000 分鐘額度的算術,從此只是歷史紀錄。自建 runner 現在買到的是**牆鐘**:兩台自建 17m21 對托管 31 分鐘(見「後果」),大約 13 分鐘。省時間,不省錢。就這樣把它退役也是合理的選擇。

**二、pull request 不再走自建 runner,而且這件事寫進了程式而不是設定。** `ci.yml` 用的是 `pull_request` 而不是 `pull_request_target`,所以 GitHub 執行的是 **PR head 自己那份 workflow 檔**。公開之後任何人都能 fork、改寫 `runs-on`、刪光步驟、換成自己的一行。而這些 runner 是同一個互動式 WSL 使用者的兩個行程,那個使用者在 `docker` 群組裡(透過 `docker run -v /:/host` 等同 root),`/mnt/c` 掛著整顆 Windows 硬碟。GitHub 自己的文件就說不要在公開倉庫用自建 runner。

所以 `runs-on` 變成:

```yaml
runs-on: ${{ (github.event_name == 'pull_request' && github.event.repository.private == false) && 'ubuntu-latest' || vars.CI_RUNNER || 'ubuntu-latest' }}
```

**刻意不是「請記得把 `CI_RUNNER` 變數刪掉」。** 那是一個人要記得的設定;這是程式的性質,之後誰再把變數設回去也重新打不開那扇門。合併到 main 與 `v*` tag 仍然吃 `CI_RUNNER`——只有推得動 main 的人才觸發得了,程式本來就是可信的。

**`.private == false` 那一半是實測換來的,不是嚴謹過頭。** 第一版把所有 pull request 無條件釘在托管 runner 上,結果每個托管 job 在三秒內失敗、沒有任何 log,而同一次推送裡仍然跑在 MSI 上的 `docs` 正常通過——那是額度用盡的特徵,也正是本文第一節那張表在講的事(3.2 天 2,492 計費分鐘 / 每月 3,000)。所以這個開關跟著**可見性**走而不是靠人在對的那一天扳:私有時繼續用那台機器(看不到的倉庫沒有人 fork 得了,而且只有協作者開得了 PR),公開的那一刻每個 pull request 自己改走托管,那裡的分鐘免費且不計量。

同一條式子也套進 `docs.yml` 與新的 `dco.yml`。`docs.yml` 尤其要:它沒有 `if`、沒有 `needs`、沒有任何閘門,所以公開之後它會是 fork 把程式送上自建 runner 最便宜的一條路——一個只改 markdown 的 pull request。

**三、決定 2 的那個代理被這次改動弄壞,一起修了。** 本文「留下來的一件事」記著:步驟層用 `vars.CI_RUNNER != ''` 當作「這個 job 在自建機器上」的代理,只有在 `runs-on` 就是那個開關時才成立。現在不成立了。17 處步驟條件收斂成 workflow 層的一個 `SELF_HOSTED`,而且 `checks` 印出它解析後的值——這類條件算錯不會紅,只會安靜地重裝一次 foundry,或者安靜地不清磁碟直到幾週後滿掉。
