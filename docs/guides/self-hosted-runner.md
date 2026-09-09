# 把 CI 搬到自己的機器上(self-hosted runner)

> **這份文件假設你只會複製貼上指令。** 每一步都會說在哪台機器上做、要打什麼、應該看到什麼。
>
> 主線是 **Windows 11 + WSL2**(第 2 節起),Linux VPS 的作法在第 12 節。
>
> 對應 `.github/workflows/ci.yml` 開頭的 "Where the jobs run" 註解與 `docs/adr/0012-ci-on-a-self-hosted-runner.md`。

---

## 0. 為什麼要做這件事

這個倉庫是**私有**的。私有倉庫用 GitHub 提供的機器跑 CI,**每一分鐘都要付錢**;公開倉庫才免費。

實際量過(建庫後的 3.2 天、88 次 CI run,數字從 GitHub 的 API 撈的):

| 指標 | 數字 |
|---|---|
| 3.2 天用掉 | 2,492 分鐘 |
| 換算成一個月 | 約 23,400 分鐘 |
| Pro 帳號一個月的額度 | 3,000 分鐘 |
| 超出 | **7.8 倍** |

而且**計費方式比想像中兇**:GitHub 是把**每一個 job 各自的秒數無條件進位到整分鐘**再加總,不是看整次 run 的牆上時間。所以一次 run 從按下按鈕到全綠是 25 分鐘,帳單記的是 **45 分鐘**——九個 job 各自進位、各自算。

有沒有可能靠優化擠進去?算一下就知道不行:一個月約 810 次 run 要塞進 3,000 分鐘,等於**每次 run 只能 3.7 分鐘**。這個專案光是 `go build -race`、一整套 testcontainers、十幾個容器的 compose stack、外加一個 kind 叢集,任何一項都不只 3.7 分鐘。實測把 workflow 能做的優化做滿,45 分鐘只降到 44 分鐘。

**結論:日常 CI 必須跑在自己的機器上。** 自己的機器跑多久都不計費——GitHub 官方寫得很白:「GitHub Actions usage is free for self-hosted runners」,計費額度只適用於 GitHub 托管的 runner。

---

## 1. 先量你的機器

在**那台 Windows 機器**上開 **PowerShell**,貼這幾行:

```powershell
Get-CimInstance Win32_ComputerSystem |
  Select-Object @{n='RAM_GB';e={[math]::Round($_.TotalPhysicalMemory/1GB,1)}}, NumberOfLogicalProcessors
Get-PSDrive C | Select-Object @{n='Free_GB';e={[math]::Round($_.Free/1GB,1)}}
winver
wsl --version
```

拿到的四個數字決定後面所有設定:

| 你有的 | 給 WSL 的記憶體 | 給 WSL 的核心 | 裝幾個 runner |
|---|---|---|---|
| 32 GB / 8 核以上 | 20 GB | 6 | 3 |
| 16 GB / 4~8 核 | 10 GB | 總核心數 − 2 | 3 |
| 8 GB / 4 核 | 6 GB | 2 | 2(見第 13 節的降級方案) |

**C 槽至少要 60 GB 可用**:kind 的 node image 約 900 MB、compose 一整套 image 好幾 GB,再加上 Go 與 npm 的快取。

`wsl --version` 如果報錯說 `Invalid command line option: --version`,表示你用的是舊版內建 WSL,第 2 節第一步會處理。

> **這台機器要當成拋棄式的。** CI 會執行分支上的任意程式碼——那是它的工作。上面不要有你的私鑰、正式環境、或其他重要服務。第 9 節的自動登入會讓這件事更重要。

---

## 2. 裝 WSL2 和 Ubuntu

CI 的所有腳本都是 bash、Makefile、Linux 容器(`scripts/e2e.sh`、`make e2e`、`sudo apt-get`),**不可能直接跑在 Windows 上**。所以作法是:在 Windows 裡裝一個 Linux,runner 跑在那個 Linux 裡。

開 **PowerShell(以系統管理員身分執行)**:

```powershell
wsl --update
wsl --install -d Ubuntu-24.04
```

**`wsl --update` 不能跳過。** Ubuntu 24.04 用的是 WSL 新的 tar 封裝格式,需要 **WSL 2.4.10 以上**;版本太舊會以看不懂的錯誤失敗。

如果想確認當下有哪些發行版可以裝,用 `wsl -l -o` 看清單,不要相信任何文件裡寫死的名字(包含這一份)。

裝完會叫你**重開機**。重開後會自動跳出一個 Ubuntu 視窗,要你設一個 Linux 使用者名稱和密碼——**記住這組密碼**,後面 `sudo` 要用。

檢查(PowerShell):

```powershell
wsl -l -v
```

要看到 `VERSION` 欄是 **2**:

```
  NAME            STATE           VERSION
* Ubuntu-24.04    Running         2
```

是 `1` 的話跑 `wsl --set-version Ubuntu-24.04 2`,並且 `wsl --set-default-version 2` 讓以後裝的都預設 2。

---

## 3. 開 systemd

runner 要裝成服務、sysctl 設定要能持久化、Docker 要開機自動起——**這三件事全都依賴 systemd**,所以這一步必須排在它們前面。

在 **Ubuntu 視窗**裡:

```sh
sudo tee /etc/wsl.conf >/dev/null <<'EOF'
[boot]
systemd=true
EOF
```

回 **PowerShell**:

```powershell
wsl --shutdown
wsl -d Ubuntu-24.04
```

在 Ubuntu 裡確認:

```sh
systemctl is-system-running        # running 或 degraded 都算成功
```

**注意事項:**

- 目前的 Ubuntu 映像**已經預設開啟 systemd**,所以上面那段通常是沒作用的。照做無害,而且讓這份文件對舊映像也成立。
- systemd 需要 **WSL 0.67.6 以上**(第 2 節的 `wsl --update` 已經處理)。
- `wsl.conf` 的 `[boot]` 區段**只有 Windows 11 與 Server 2022 有**。
- **不要**因為開了 systemd 就以為 WSL 會一直活著——它不會,理由在第 4 節。

---

## 4. `.wslconfig`:資源分配,以及兩個會讓機器停擺的逾時

這一節是整份文件最容易出錯的地方,請完整照做。

### 4.1 兩個閒置計時器,不是一個

WSL 有**兩個**獨立的閒置計時器,而官方文件只寫了其中一個:

| 計時器 | 鍵 | 預設 | 官方文件有寫嗎 |
|---|---|---|---|
| distro 閒置 → 終止執行個體 | `general.instanceIdleTimeout` | **15000 毫秒(15 秒)** | **沒有** |
| VM 閒置(所有 distro 都停了)→ 關掉 VM | `wsl2.vmIdleTimeout` | 60000 毫秒 | 有 |

`instanceIdleTimeout` 只出現在 WSL 的原始碼(`WslCoreConfig.h`)和開始選單裡 **WSL Settings** 這個 App 的「Distribution idle timeout」欄位,`.wslconfig` 的官方參考頁完全沒提。

**這就是為什麼很多人關掉終端機十五秒後 runner 就離線了。** 只設 `vmIdleTimeout` 是常見的錯誤:VM 還活著,但 distro 已經被砍掉。

而且 Microsoft 講得很明白:**「systemd 服務不會讓你的 WSL 執行個體保持存活」**。runner 跑成 systemd 服務不代表 WSL 會為它留著。

### 4.2 設定檔

在 **PowerShell**:

```powershell
notepad "$env:USERPROFILE\.wslconfig"
```

貼上(**記憶體和核心數依第 1 節的表格改**):

```ini
[general]
# 未公開的鍵,預設 15000 毫秒。負值 = 永不因閒置而終止。
# 可以在開始選單的 WSL Settings 裡用「Distribution idle timeout」交叉確認。
instanceIdleTimeout=-1

[wsl2]
memory=10GB
processors=6
swap=8GB
vmIdleTimeout=-1
```

存檔,回 PowerShell 跑 `wsl --shutdown`,等約 8 秒再重開。

**注意事項:**

- **格式寫錯的 `.wslconfig` 會被靜默忽略**,不會報錯。所以要驗證,不要假設——開 **WSL Settings** 這個 App 對一次數字。
- `.wslconfig` 是**所有 WSL2 發行版共用**的,沒辦法單獨給某一個 distro 編列預算。
- 不設 `memory` 的話預設是**主機記憶體的一半,沒有上限**。(「或 8 GB 取小」是 Windows 10 時代的舊行為,已經不適用。)
- `swap` 不設的話預設是 memory 的 25%,進位到整數 GB。
- **不要加 `sparseVhd` 或 `autoMemoryReclaim`。** 前者見第 10 節;後者 Microsoft 自己說會「break the docker daemon when running it as a service in WSL」,正好是我們第 6 節要做的事。

---

## 5. kind 需要的兩個核心參數

`helm` job 會在 WSL 裡開一個 Kubernetes 叢集。Ubuntu 的預設檔案監看上限是 `max_user_watches=8192` / `max_user_instances=128`,kind 官方要求 `524288` / `512`,不改的話 pod 會噴 "too many open files"。

在 Ubuntu 裡:

```sh
sudo tee /etc/sysctl.d/99-kind.conf >/dev/null <<'EOF'
fs.inotify.max_user_watches = 524288
fs.inotify.max_user_instances = 512
EOF
sudo sysctl --system
```

**這一步依賴第 3 節的 systemd。** 沒有 systemd 的話,WSL 的 init **根本不會讀** `/etc/sysctl.conf` 或 `/etc/sysctl.d/`(microsoft/WSL#4232),設定只在當下這個 session 有效,重開就沒了。有 systemd 的話 `systemd-sysctl.service` 會在每次開機套用。

驗證方式是**完整重啟一次再看**。PowerShell 跑 `wsl --shutdown`,重開 Ubuntu,然後:

```sh
sysctl fs.inotify.max_user_watches fs.inotify.max_user_instances
```

兩個值都還在,才算成功。

> **關於 kind 在 WSL2 的已知問題**:kind 官方列的兩個最兇的問題——節點 IP 從 Windows 主機不可達、`sessionAffinity: ClientIP` 因為 WSL 核心缺 `xt_recent` 模組而壞掉——**都不影響這個專案**。`deploy/helm/` 沒有用 `sessionAffinity`、NodePort、LoadBalancer 或 hostPort,而 `scripts/helm-e2e.sh` 是用 `kubectl exec` 從叢集內部跑測試。你如果去讀 kind 的 known-issues 頁看到那兩條,不用緊張。
>
> 還有一個可能會遇到的是 cgroup v2 設定問題(`error adding pid to cgroups`),kind 的文件有處置方式。

---

## 6. Docker 引擎:兩條路線

CI 需要一個 Docker 引擎。有兩種裝法,**建議用 6A**。

### 6A(建議)WSL 裡的 Docker Engine

在 **Ubuntu 視窗**裡:

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates curl git jq unzip make build-essential
curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
sudo apt-get install -y nodejs
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER"
sudo systemctl enable --now docker
```

**`build-essential` 不能漏。** `make test` 是 `go test -race`,race detector 需要 cgo,cgo 需要 C 編譯器,而 WSL 的 Ubuntu 映像預設不含 gcc。這在第一次實跑就踩到了:`checks` job 的前十二步全過(`go vet`、golangci-lint、gitleaks、sqlc、oapi-codegen 都是純 Go;`make build` 是 `CGO_ENABLED=0`),到 `-race` 那一步**零秒失敗**——零秒代表指令根本沒啟動,不是測試沒過。

**Node 用 NodeSource 22,不要用 Ubuntu 內建的。** 24.04 內建是 Node 18,而這個專案要 22。Ubuntu 還把 `npm` 拆成獨立套件,少裝一半就會出現第 14 節那個「指令掉到 Windows 去」的問題。NodeSource 的 `nodejs` 套件內含 npm。

**登出再登入**(關掉 Ubuntu 視窗重開),讓群組生效,然後驗證:

```sh
gcc --version
node --version                  # v22.x
command -v npm npx              # 都要 /usr/bin,不能是 /mnt/c
docker version
docker compose version
docker buildx version
docker run --rm hello-world
```

全部都要有正常輸出。`docker buildx` 少了的話 `image` job 會在 `docker/setup-buildx-action` 那一步失敗。

如果你已經裝了 Docker Desktop,**把它對這個 distro 的 WSL Integration 關掉**(Settings → Resources → WSL integration → 把 Ubuntu-24.04 的開關關掉 → Apply),否則兩個引擎會搶 `/var/run/docker.sock`。Docker Desktop 本身可以留著手動用。

**如果這個 distro 以前開過 Docker Desktop 的 WSL Integration,先檢查 `/var/run`。** 症狀是引擎明明活著但誰都連不上:

```
$ sudo systemctl status docker
   Active: active (running)
   ...dockerd[43804]: ...msg="API listen on /run/docker.sock"

$ ls -l /var/run/docker.sock
ls: cannot access '/var/run/docker.sock': No such file or directory
```

`dockerd` 跑的是 `-H fd://`,也就是 systemd socket activation,而 `docker.socket` 的 `ListenStream` 是 **`/run/docker.sock`**——這是正確的。標準 Ubuntu 上 `/var/run` 是一條指向 `/run` 的符號連結,所以兩個路徑等價;而 docker CLI、buildx、compose 全都預設連 `/var/run/docker.sock`。這條符號連結不見了,CI 就連不上。

一行診斷:

```sh
readlink -f /var/run        # 要印 /run
```

印出別的東西(或 `/var/run` 是個真目錄)就修它:

```sh
sudo systemctl stop docker.socket docker.service
sudo mv /var/run /var/run.bak      # /var/run 不存在的話跳過這行
sudo ln -s /run /var/run
```

然後 PowerShell `wsl --shutdown`、重開 distro,驗證 `readlink -f /var/run` 是 `/run`、`ls -l /var/run/docker.sock` 看得到、`docker ps` 正常,再 `sudo rm -rf /var/run.bak`。

修 `/var/run` 而不是只補一條 `/var/run/docker.sock` → `/run/docker.sock`,是因為 `/run` 是 tmpfs、每次開機重建,只補 socket 那條每次重開都要重補;而 `/var` 在磁碟上,`/var/run` 這條符號連結補一次就持久。何況 `/var/run` 指向 `/run` 是 FHS 與 systemd 的前提,壞著會拖累其他寫 pid 檔的服務,不只 docker。

**全新安裝、從來沒開過 Docker Desktop Integration 的 distro 不會遇到這個。**

**為什麼建議這條:**

- `dockerd` 由 systemd 管,開機自己起,不依賴任何 GUI 程式。
- 容器資料放在 Ubuntu 自己的虛擬磁碟裡,所以第 10 節的空間回收**真的有效**。
- 沒有下面 6B 那個「Windows 端沒把符號連結重建回來」的回歸問題(上面那個 `/var/run` 的檢查是不同的東西:那是舊 Integration 留下的痕跡,修一次就好,不會每次重開機復發)。

### 6B(替代)Docker Desktop

你已經裝好 Docker Desktop 的話也可以用它,但要知道代價。

**設定步驟:**

1. **Settings → General** → 「Use the WSL 2 based engine」。**在支援 WSL2 的機器上這個選項預設就是開的,而且通常根本不顯示**——看不到不用找,那是正常的。
2. **Settings → Resources → WSL integration** → 把 **Ubuntu-24.04** 的開關打開 → 按 **Apply**(按鈕就叫 Apply,不是 "Apply & restart")。
3. 如果 distro 清單是空的,表示 Docker Desktop 在 Windows 容器模式:右鍵工具列的鯨魚圖示 → **Switch to Linux containers**。

**不需要**把使用者加進 `docker` 群組。Docker Desktop 的存取是靠 `/mnt/wsl/docker-desktop/...` 這個整台 VM 共用的掛載與符號連結給的,是檔案層級不是使用者層級。驗證:

```sh
readlink -f /var/run/docker.sock
docker version
```

**代價,四項:**

- **Docker Desktop 必須有人登入 Windows 才會跑。任何付費層級都沒有無頭或服務模式**——Docker 自己的 roadmap issue #515 至今未解,Docker Desktop 也完全不支援 Windows Server。`com.docker.service` 那個 Windows 服務只是 Hyper-V 與 Windows 容器用的特權輔助程式,WSL2 模式下根本不會自動啟動。(不過第 9 節的自動登入本來就是必要的,所以這一項不是額外的成本。)
- **4.75.0 起有一個回歸**:WSL 重啟之後 `/var/run/docker.sock` 的符號連結不會被重建。手動 `sudo ln -sf /mnt/wsl/docker-desktop/shared-sockets/guest-services/docker.proxy.sock /var/run/docker.sock` 可以救,但**重開機後又沒了**。對一台每月被 Windows Update 重開一次的機器,這是週期性斷線。

  **這在第一次實跑就發生了。** 設定 `.wslconfig` 需要 `wsl --shutdown`,重開之後 `docker version` 在互動 shell 裡還是好的,但 CI 的 `image` job 在 `docker/setup-buildx-action` 一秒內死掉:

  ```
  failed to connect to the docker API at unix:///var/run/docker.sock;
  dial unix /var/run/docker.sock: connect: no such file or directory
  ```

  路線 6A 沒有這個問題:`dockerd` 是發行版裡的 systemd 服務,socket 由它自己建立與持有,不依賴任何 Windows 端的程式在正確的時機補一條符號連結。
- **Docker 的資料在另一個虛擬磁碟**(`%LOCALAPPDATA%\Docker\wsl\data\docker_data.vhdx`,新版可能在 `...\wsl\disk\` 底下),**官方沒有支援的縮小方法**,只有核彈級的「Clean up data」。第 10 節的空間回收對它無效。
- **Windows 帳號之間不共用容器與映像**,所以跑 Docker Desktop 的 Windows 帳號每次都必須是同一個。

### 兩條路線共通

testcontainers 在 WSL2 上是開箱即用的。**不要**去設 `DOCKER_HOST=tcp://localhost:2375`——那是舊版 WSL 的建議,而且等於在一台跑 CI 的機器上開一個沒有認證也沒有 TLS 的 root 等級 daemon。

### 還有一件裝一次的事:Playwright 的系統函式庫

`e2e` job 會用 Playwright 開瀏覽器跑前台冒煙測試。瀏覽器本身 CI 每次會自己抓(抓過就留在 `~/.cache/ms-playwright`,之後是 no-op),但它依賴的**系統函式庫**要你先裝一次:

```sh
npx playwright@1.56.1 install --with-deps chromium
ls ~/.cache/ms-playwright
```

版本要對得上 `web/trade/package.json` 裡釘的那個。

**為什麼這一步不能交給 CI:** `--with-deps` 內部是 `sudo -- sh -c "apt-get ..."`。要 CI 每次跑它,等於得給 runner 帳號免密碼 sudo。所以 workflow 在 `vars.CI_RUNNER` 有值時把 `PLAYWRIGHT_INSTALL_DEPS` 設成 `0`,`scripts/e2e-web.sh` 就只要瀏覽器、不碰 `--with-deps`。**這台機器因此完全不需要 NOPASSWD。**

**發行版版本要對。** `--with-deps` 只認得 Playwright 有出套件清單的那幾個 Ubuntu 版本。實測 Ubuntu 26.04 會直接拒絕:

```
BEWARE: your OS is not officially supported by Playwright;
installing dependencies for ubuntu26.04-x64 as a fallback.
Cannot install dependencies for ubuntu26.04-x64 with Playwright 1.56.1!
```

這是第 2 節指定 **24.04** 而不是最新版的原因。想用更新的 Ubuntu,得先確認你釘的 Playwright 版本支援它。

---

## 7. 裝 runner(重複兩到三次)

在 **Ubuntu 視窗**裡,用你在第 2 節建的那個使用者(不用另外開帳號——WSL 這台虛擬機本身就是那個可以砍掉重練的環境)。

先到 GitHub 網頁上拿 token:

> `https://github.com/arc119226/crypto-exchange` → **Settings** → **Actions** → **Runners** → **New self-hosted runner** → 選 **Linux**

> 選 **Linux** 不是 Windows——即使你的機器是 Windows,runner 裝在 WSL 裡的 Ubuntu 上。

那一頁會顯示三段指令,裡面有**當下的版本號**和**一組一次性 token**。**直接照那一頁貼**,不要用這份文件裡的版本號——runner 註冊之後會自己更新,寫死版本只會過期。撰寫本文時最新是 v2.337.0,下載網址長這樣(tag 有 `v`、檔名沒有):

```
https://github.com/actions/runner/releases/download/v2.337.0/actions-runner-linux-x64-2.337.0.tar.gz
```

**token 一小時後失效**,過期就回那一頁再按一次。

```sh
mkdir -p ~/runner-1 && cd ~/runner-1
# ↓ 用網頁上那一頁顯示的 curl 與 tar 指令
curl -o r.tar.gz -L https://github.com/actions/runner/releases/download/v2.337.0/actions-runner-linux-x64-2.337.0.tar.gz
tar xzf r.tar.gz && rm r.tar.gz
./config.sh --url https://github.com/arc119226/crypto-exchange \
  --token <貼上剛才那組 token> \
  --name box-1 --labels exchange-ci --work _work --unattended --replace
```

**`config.sh` 不能用 `sudo` 跑**,它會拒絕(除非設 `RUNNER_ALLOW_RUNASROOT=1`)。只有下面的 `svc.sh` 需要 sudo。

`--labels exchange-ci` 是**附加**在自動標籤(`self-hosted`、`Linux`、`X64`)之外的。因為我們的 `runs-on` 是單一字串,**倉庫變數 `CI_RUNNER` 的值必須和這個標籤一字不差**。標籤名裡不能有逗號。`--replace` 讓同名 runner 重新註冊時覆蓋舊的而不是失敗。

裝成服務:

```sh
sudo ./svc.sh install
sudo ./svc.sh start
sudo ./svc.sh status      # 要看到 active (running)
```

`sudo ./svc.sh install` 不帶參數就會裝成你目前這個帳號。它需要 systemd(第 3 節),而且**不會檢查**——沒有 systemd 的話會噴 systemctl 的錯而不是清楚的訊息。

Ubuntu 還要擋掉 `needrestart` 在 job 執行到一半重啟 runner:

```sh
sudo mkdir -p /etc/needrestart/conf.d
echo '$nrconf{override_rc}{qr(^actions\.runner\..*\.service$)} = 0;' | \
  sudo tee /etc/needrestart/conf.d/actions_runner_services.conf
```

**然後整段重複一到兩次**,換成 `~/runner-2` / `--name box-2`(和 `~/runner-3` / `--name box-3`),每次都要回網頁拿一組**新的** token。

要求是:**各自獨立的目錄** + **唯一的 `--name`**(systemd 的 unit 名稱是 `actions.runner.<org>-<repo>.<name>.service`,由 name 衍生)。`_work` 相對於各自的目錄,所以自動就分開了。

為什麼要多個?**一個 runner 一次只跑一個 job。** 現在一次完整的 run 是九個 job、加起來 45 分鐘的機器時間。只裝一個,九個 job 會排隊,你要等 45 分鐘才看得到結果;三個平行跑大約 15 分鐘,和 GitHub 托管的機器差不多。每個 runner 閒置時大約吃 80 MB 記憶體、500 MB 磁碟。

裝完回網頁的 Runners 頁,應該看到兩到三個綠點,標籤都是 `exchange-ci`。

---

## 8. 按下開關

`.github/workflows/ci.yml` 裡每個 job 寫的是:

```yaml
runs-on: ${{ vars.CI_RUNNER || 'ubuntu-latest' }}
```

也就是:**有設變數就用你的機器,沒設就用 GitHub 的。** 開關只是一個倉庫變數:

> **Settings** → **Secrets and variables** → **Actions** → **Variables** 分頁 → **New repository variable**
>
> - Name: `CI_RUNNER`
> - Value: `exchange-ci`

**那一頁有兩個區塊,點錯完全沒有效果。** 上面是 **Environment variables**,下面是 **Repository variables**,要按的是**後者**的 **New repository variable**。

環境變數在這裡行不通,而且不是設定問題是機制問題:GitHub 的文件寫「Configuration variables at the environment level are automatically available **after their environment is declared by the runner**」——環境要等 runner 宣告之後才解析,而 `runs-on` 就是決定 runner 的那一刻,比那更早。`ci.yml` 也沒有任何 job 宣告 `environment:`。所以放在環境裡的 `CI_RUNNER` 永遠是空字串,每個 job 都會安靜地落回 `ubuntu-latest`,看起來就像什麼都沒發生。

左側選單的 **Environments** 是完全不同的功能,不要在那裡建東西。

按 **Add variable**,下一次 push 就會跑在你的機器上。不用改任何程式碼、不用開 PR。

**已經開跑的 run 不會回頭讀新變數。** 變數是在 run 開始時解析的,所以設定之前就排進去的 run 仍然跑在 GitHub 的機器上。在 Actions 頁對那個 run 按 **Re-run all jobs** 就會用新值重跑,不需要新的 commit。

**機器掛了怎麼辦:** 把這個變數**刪掉**,CI 立刻回到 GitHub 的機器上跑。會開始計費,但不會卡住。修好機器再把變數加回去。

順手關掉 fork PR 的 workflow:

> **Settings** → **Actions** → **General** → **Fork pull request workflows** → 取消勾選 **"Run workflows from fork pull requests"**

(同一區塊還有 "Send write tokens to workflows from pull requests"、"Send secrets to workflows from pull requests"、"Require approval for fork pull request workflows",要關的是第一個。)

GitHub 的官方警告是:「Self-hosted runners should almost never be used for public repositories, because any user can open pull requests against the repository and compromise the environment.」私有倉庫本來就只有協作者能觸發,但明確關掉比較安心。另外要記得 `pull_request_target` 觸發的 workflow 是在**基底分支**的脈絡下執行,而且**不受核准設定約束**——這個倉庫沒有用它,以後也別用。

---

## 9. 讓機器不睡著、而且重開機能自己回來

這一節是整個設計裡風險最高的部分。三件事疊在一起:**WSL 不能在登入前啟動**、**Docker Desktop 需要互動式登入**(如果你走 6B)、**Windows Update 每月會重開一次**。

### 9.1 電源

**PowerShell(系統管理員)**:

```powershell
powercfg /change standby-timeout-ac 0      # 永不睡眠
powercfg /change hibernate-timeout-ac 0    # 永不休眠
powercfg /change disk-timeout-ac 0         # 硬碟不要停轉
powercfg /change monitor-timeout-ac 10     # 螢幕 10 分鐘關掉(這個沒關係)
powercfg /h off                            # 關掉休眠檔,順便省 C 槽空間
```

**這樣還不夠。** 最經典的坑是 **System unattended sleep timeout,預設 2 分鐘**:機器在「無人值守」狀態下被喚醒時(排程、網路喚醒),Windows 用的是這個值而不是上面的 `standby-timeout-ac`。結果就是機器醒來跑兩分鐘又睡回去。這個設定預設是隱藏的,要先解除隱藏:

```powershell
powercfg -attributes SUB_SLEEP 7bc4a2f9-d8fc-4469-b07b-33eb785aaca0 -ATTRIB_HIDE
powercfg /setacvalueindex SCHEME_CURRENT SUB_SLEEP 7bc4a2f9-d8fc-4469-b07b-33eb785aaca0 0
powercfg /setactive SCHEME_CURRENT
```

還要檢查與處理:

```powershell
powercfg /a           # 出現 "Standby (S0 Low Power Idle)" 表示是 Modern Standby,行為不同
powercfg /requests    # 誰在阻止睡眠
powercfg /waketimers  # 誰會喚醒它
powercfg /lastwake    # 上次是誰喚醒的
```

- **筆電**要設關蓋不動作:`powercfg /setacvalueindex SCHEME_CURRENT SUB_BUTTONS LIDACTION 0`。
- **網路卡**:裝置管理員 → 你的網路介面卡 → 內容 → 電源管理 → 取消「允許電腦關閉這個裝置以節省電源」。
- 注意 `powercfg /change` **只改當下作用中的電源計畫**。計畫被 OEM 工具或功能更新換掉,設定就沒了——值得每隔一陣子回來確認。

### 9.2 Windows Update

它**一定會**自動重開,擋不掉,只能安排時間:

- **設定 → Windows Update → 進階選項 → 使用中時數**,設成你的 CI 最忙的那段時間(手動設定上限 18 小時)。
- **暫停更新**最多 35 天,而且要先讓它裝完才能再暫停一次。

好消息是 2026 年 7 月起,Windows 11 24H2/25H2/26H1 把驅動程式、.NET 與韌體更新**合併成每月一次重開**。所以大約是每月一次,不是每週。

### 9.3 自動登入

**WSL 無法在登入前啟動。** Microsoft 把這列為已知問題:「Launching Windows Subsystem for Linux from session zero does not currently work」。Session 0 正是 Windows 服務、以及排程工作裡「不論使用者是否登入都執行」的執行環境。所以:

| 方式 | 可行嗎 |
|---|---|
| 啟動資料夾(`shell:startup`)的捷徑 | **可以**,但只在使用者登入時 |
| 排程工作,觸發程序「**登入時**」+「僅在使用者登入時執行」 | **可以**(建議用這個) |
| 排程工作,觸發程序「啟動時」 | **不行** |
| 排程工作,「不論使用者是否登入都執行」 | **不行** |

所以要讓機器重開後自己恢復,**自動登入是必要的,不是可選的**。

> **這是一個安全取捨:任何走到這台電腦前面的人都會直接看到已登入的桌面。** 這就是第 1 節說「當成拋棄式機器」的原因。不要拿放著重要東西的電腦做這件事。不想做的話就跳過這一小節——代價是每次重開機後你要自己登入一次,在那之前 CI 是停的。

**第一步:讓自動登入的選項出現。** Windows 11 開著 Windows Hello 時會把 `netplwiz` 的勾選框藏起來。兩個辦法擇一:

- **設定 → 帳戶 → 登入選項 → 其他設定** → 關掉「為了改善安全性,只允許此裝置上的 Microsoft 帳戶使用 Windows Hello 登入」。(這個開關只對 Microsoft 帳戶有效,本機帳戶看不到。)
- 或改登錄檔後**重開機**:

  ```powershell
  REG ADD "HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\PasswordLess\Device" /v DevicePasswordLessBuildVersion /t REG_DWORD /d 0 /f
  ```

  (這個值預設是 `2`,設成 `0` 會讓勾選框回來。)

**第二步:設定自動登入。** 有兩種做法,**用第一種**:

- **建議:[Sysinternals Autologon](https://learn.microsoft.com/en-us/sysinternals/downloads/autologon)。** 它把密碼存進 **LSA secret**,不是登錄檔明文。下載執行、填帳號密碼、按 Enable。
- 不建議:`netplwiz` 取消勾選「必須輸入使用者名稱和密碼」。它能用,但**不是 Microsoft 正式文件記載的方法**。
- **不要**直接用 KB324737 的 `AutoAdminLogon` 登錄檔做法。Microsoft 自己的警告寫得很清楚:「when autologon is turned on, the password is stored in the registry in plain text. The specific registry key that stores this value can be remotely read by the Authenticated Users group.」

登出或重新啟動時**按住 Shift** 可以略過自動登入一次。另外注意:用別的帳號互動式登入過,會改寫 `DefaultUserName` 而讓自動登入失效。

### 9.4 開機自動啟動 WSL,並且讓它活著

Docker Desktop(6B)會跟著登入自己起來,但 **WSL 不會**,而且第 4 節說過,光把它叫醒一次是不夠的——`wsl -d Ubuntu-24.04 -e /bin/true` 執行完就結束,執行個體會被閒置逾時砍掉。

所以要跑一個**常駐**的行程把它釘住。按 `Win+R` 打 `shell:startup`,在跳出來的資料夾裡新增 `keep-wsl-alive.vbs`,內容:

```vbscript
' 把 WSL 執行個體釘住:這個 sleep 永遠不結束,所以 WSL 不會因為閒置被回收。
' 0 = 隱藏視窗,False = 不等它結束。
CreateObject("WScript.Shell").Run "wsl.exe -d Ubuntu-24.04 -- /bin/sh -c ""exec sleep infinity""", 0, False
```

用 `.vbs` 而不是 `.bat` 是為了不要每次登入都閃一個命令列視窗。

第 4 節的兩個 `-1` 是主要的保險,這個保活行程是第二層——兩個都做。runner 的 systemd 服務仍然保留,它負責的是「runner 掛了要重啟」,不是「讓 WSL 活著」。

---

## 10. 磁碟會滿

GitHub 的機器每個 job 跑完就整台丟掉,自己的機器不會——每次 build 的 image、每個 kind node image、每個 build cache 都留著。**不管的話幾天就滿了。**

### 10.1 job 裡的自動清理(已經寫好了)

四個會用到 Docker 的 job(`integration` `e2e` `helm` `image`)結尾都掛了一個清理步驟(`.github/actions/reclaim-disk/action.yml`),**只在 `CI_RUNNER` 有值時才跑**。它用的是**有時間過濾**的清法:

```sh
docker container prune -f --filter until=6h
docker image prune -af --filter until=72h
docker builder prune -f --filter until=72h
```

為什麼要加 `until`:多個 runner 共用**同一個** Docker 引擎。如果 `image` job 直接跑 `docker system prune -a`,它會把旁邊 `e2e` job 正在用的容器和 image 一起殺掉。加了時間過濾,只動閒置超過任何單一 job 執行時間的東西,才可以邊跑邊清。

volume 沒有清,因為 `docker volume prune` **沒有** `until` 這個過濾器,分不出死的和活的。`scripts/e2e.sh` 自己會 `compose down -v`,testcontainers 有 Ryuk 收屍,剩下的靠下一節。

### 10.2 每週深度清理

在 Ubuntu 裡先確認 cron 有在跑:

```sh
systemctl is-enabled cron     # 預期 enabled
sudo systemctl enable --now cron
systemctl status cron
```

然後 `crontab -e`(第一次會問編輯器,選 nano):

```cron
# 每週日 04:00 停 runner、徹底清、再開
0 4 * * 0 for d in $HOME/runner-1 $HOME/runner-2 $HOME/runner-3; do sudo $d/svc.sh stop; done; docker system prune -af --volumes; for d in $HOME/runner-1 $HOME/runner-2 $HOME/runner-3; do sudo $d/svc.sh start; done
```

停掉 runner 再清,才不會清到跑到一半的 job。

> cron **只在 WSL 執行個體活著的時候才跑**。第 4 節的兩個 `-1` 和第 9.4 節的保活行程做好了,這一點才成立。

### 10.3 清了空間為什麼沒回來

WSL 的資料放在 Windows 的一個虛擬磁碟檔(`.vhdx`)裡,**這個檔只會長大,不會自己縮小**。你在 Ubuntu 裡 `docker system prune` 清掉 30 GB,`df -h` 會顯示空間變多,但 C 槽的可用空間**一點都沒回來**——`prune` 釋放的是 vhdx **內部**的空間。

**路線 6A(WSL 裡的 Docker)可以真的收回來。** 在 **PowerShell**:

```powershell
wsl --shutdown
wsl --manage Ubuntu-24.04 --compact
```

`--compact` 是目前官方支援的壓縮方式,前提是**該 distro 已經停止**。

> **不要用 `wsl --manage ... --set-sparse true`。** 這個功能目前**預設被封鎖**,WSL 自己的錯誤訊息寫的是「Sparse VHD support is currently disabled due to potential data corruption」(會弄壞 ext4 檔案系統)。網路上會看到教人加 `--allow-unsafe` 繞過去——**不要照做**,那是繞過一個字面上寫著資料損毀的警告。同理,`.wslconfig` 裡也不要加 `sparseVhd`(順帶一提,它屬於 `[experimental]` 區段而不是 `[wsl2]`;放錯區段 WSL 會靜默忽略)。

**路線 6B(Docker Desktop)收不回來。** 它的資料在自己的 `docker_data.vhdx` 裡,是和 Ubuntu 不同的虛擬磁碟,對 Ubuntu 下 `--compact` 完全沒有作用。官方沒有支援的縮小方法,只剩核彈級的 **Docker Desktop → 問號圖示 → Troubleshoot → "Clean up data"**(所有 image、容器、volume 全刪,下一次 CI 會慢很多)。這是建議走 6A 的實際理由之一。

---

## 11. 驗證有沒有成功

1. **看 runner 有沒有接到工作。** 隨便 push 一個 commit,到 Actions 頁點開任何一個 job,展開最上面的 **Set up job**,應該看到:

   ```
   Runner name: 'box-1'
   Runner group name: 'Default'
   ```

   如果看到 `Runner Image: ubuntu-24.04` 之類的,表示變數沒設成功,還在用 GitHub 的機器。

2. **重開機測試——這一項不能跳過。** 真的把 Windows 重開一次,**不要碰任何東西**,等幾分鐘後看倉庫的 Runners 頁:兩到三個 runner 應該自己回到 **Idle**。

   這是整個設計裡最容易失敗的地方(WSL 的 session 0 限制、閒置逾時、Docker 引擎)。沒通過這一項,就等於每次 Windows Update 之後 CI 都會停到你發現為止。沒過的話照第 14 節的順序查。

3. **看帳單有沒有停止增加。** **Settings** → **Billing** → **Actions**。自建 runner 跑的 job 在 API 上照樣有時間長度,但**不計入帳單**——要看的是 Billing 頁的實際數字,不是 job 的秒數。跑幾天後,每月用量應該從約 23,400 分鐘掉到 **50~100 分鐘**(只剩 `release`)。

4. **看磁碟。** 在 Ubuntu 裡:

   ```sh
   df -h /
   docker system df
   ```

   跑了一週後使用率應該穩在某個水位不再往上爬。一直漲就是 10.1 的清理沒生效——去 job log 裡找 `reclaim disk` 這個步驟,它會印出當下的磁碟狀況。Windows 那邊還要另外看 C 槽(10.3 節)。

---

## 12. Linux VPS(另一條路線)

如果你用的是 Linux 主機而不是 Windows,第 2 到 6 節和第 9 節整段跳過,改做這些:

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates curl git jq unzip make
curl -fsSL https://get.docker.com | sudo sh
docker --version && docker compose version

# runner 不要用 root 也不要用你自己的帳號跑
sudo useradd -m -s /bin/bash ghrunner
sudo usermod -aG docker ghrunner
# Playwright 的 install --with-deps 會用 apt-get 裝瀏覽器的系統相依套件
echo 'ghrunner ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/ghrunner
sudo -iu ghrunner        # 之後第 7 節的指令都在這個身分下做
```

最後那行 NOPASSWD 是 GitHub 官方對 self-hosted runner 的建議(很多 `setup-*` action 會裝系統套件),前提一樣是:**這台機器是拋棄式的**。

第 5 節的 sysctl 一樣要做,第 7、8、10、11 節照走(10.3 的 vhdx 問題不存在,Linux 上 `docker system prune` 釋放的就是真實磁碟空間)。

---

## 13. 機器規格不夠的時候

如果只有 2 核 / 8 GB,`e2e` 和 `helm` 會很吃力(前者十幾個容器,後者一整個 Kubernetes)。折衷做法是**只把吃計費最兇、但資源需求中等的 job 搬過去**:

| 搬到自己的機器 | 留在 GitHub |
|---|---|
| `integration`(佔帳單 25%) | `e2e`(要十幾個容器) |
| `checks`(31%,原本的 lint + unit + fast-checks) | `helm`(要 kind 叢集) |
| `fuzz-smoke`(9%) | `image`、`release` |

這樣搬走約 65% 的帳單。作法是把那幾個 job 的 `runs-on` 改成另一個變數(例如 `vars.CI_RUNNER_LIGHT`),其餘留 `ubuntu-latest`。

但要知道這只是過渡:剩下的還是每月約 8,200 分鐘,仍然超標。真正的解法是把機器換大一點。

---

## 14. 常見問題

**Q: 重開機之後 runner 沒有自己回來。**
按這個順序查,每一步都確認過再往下:
1. Windows 有沒有真的自動登入到桌面?(9.3)
2. `shell:startup` 裡的 `keep-wsl-alive.vbs` 在不在?工作管理員裡有沒有 `wsl.exe`?(9.4)
3. PowerShell 跑 `wsl -l -v`,Ubuntu 的 STATE 是不是 `Running`?
4. `.wslconfig` 的兩個 `-1` 有沒有生效?開 **WSL Settings** App 對一次「Distribution idle timeout」。(4.2)
5. Ubuntu 裡 `sudo ~/runner-1/svc.sh status`;`journalctl -u 'actions.runner.*' -n 50` 看 log。
6. 走 6B 的話:Docker Desktop 有沒有跟著起來?`readlink -f /var/run/docker.sock` 還在不在?

**Q: runner 在網頁上顯示 Offline,但我沒重開機。**
八成是機器睡著了(9.1,特別是那個 2 分鐘的 unattended sleep timeout),或是 WSL 執行個體被 15 秒的閒置逾時砍掉了(4.1)。

**Q: job 卡在 "Waiting for a runner to pick up this job"。**
標籤對不上。網頁 Runners 頁看標籤是不是 `exchange-ci`,和倉庫變數 `CI_RUNNER` 的值一字不差。

**Q: `Cannot connect to the Docker daemon` / `permission denied ... docker.sock`。**
路線 6A:`sudo systemctl status docker`;`groups` 裡有沒有 `docker`(加完群組要登出再登入);服務是 running 卻還是連不上的話,`readlink -f /var/run` 要印 `/run`(見第 6A 節)。
路線 6B:Docker Desktop 有沒有開;WSL Integration 有沒有打開;`readlink -f /var/run/docker.sock` 是不是還指向 `/mnt/wsl/docker-desktop/...`(那個回歸問題)。

**Q: C 槽滿了。**
看 10.3 節。路線 6A:先在 Ubuntu 裡 `docker system prune -af --volumes`(記得先停 runner),再 PowerShell `wsl --shutdown` + `wsl --manage Ubuntu-24.04 --compact`。路線 6B:只能用 Docker Desktop 的「Clean up data」。

**Q: `helm` job 在我的機器上失敗,GitHub 上卻是綠的。**
八成是第 5 節的 inotify 參數沒生效(重啟後驗證過了嗎?),或是記憶體不夠(4.2)。job 的 artifact 裡有 `helm-kind-logs`,裡面 `events.txt` 會說是哪個 pod 起不來。

**Q: 我改了 `.wslconfig` 但好像沒作用。**
格式錯誤的 `.wslconfig` 會被**靜默忽略**,不報錯。開 **WSL Settings** App 看它顯示的值對不對,並確認改完有跑 `wsl --shutdown`(要等約 8 秒才會真的停)。

**Q: 想暫時全部回到 GitHub 的機器上。**
刪掉倉庫變數 `CI_RUNNER`。一秒生效,不用改 code。

**Q: 那 timeout 呢?**
每個 job 都有 `timeout-minutes`(15~30 分鐘)。這在自己的機器上一樣重要:一個卡住的 job 會佔住三分之一的 runner 容量,直到有人發現。

**Q: 這樣做安全嗎?**
私有倉庫、單人提交,風險可控。但要記得四件事:機器上不要放別的東西、fork PR 要關掉(第 8 節)、自動登入代表實體接觸就等於登入(9.3)、WSL 看得到 Windows 的檔案(`/mnt/c`)。「拋棄式機器」這句話在 Windows 路線上一樣成立。
