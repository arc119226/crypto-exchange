# 把 CI 搬到自己的機器上(self-hosted runner)

> **這份文件假設你只會複製貼上指令。** 每一步都會說在哪台機器上做、要打什麼、應該看到什麼。
>
> 主線是 **Windows 11 + WSL2**(第 2 節起),Linux VPS 的作法在第 13 節。裝好之後 CI 會跑哪些 job、每個 job 在證明什麼,在第 12 節。
>
> **完全沒碰過這類東西的人**,先看同一件事的新手版:[`self-hosted-runner-newcomer.md`](self-hosted-runner-newcomer.md)。那一份每一步都寫明「你會看到什麼」,做完再回來看這一份的細節。
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
| 8 GB / 4 核 | 6 GB | 2 | 2(見第 14 節的降級方案) |

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

**為什麼指名 24.04,而不是當下最新的那個。** 這是實測出來的,不是保守:`e2e` job 要開一個無頭瀏覽器跑前台冒煙測試,而 Playwright 的 `install --with-deps` **只認得它有出套件清單的 Ubuntu 版本**。在 26.04 上它會先警告「你的作業系統不在官方支援清單裡」,退回 fallback,然後直接失敗:

```
Cannot install dependencies for ubuntu26.04-x64 with Playwright 1.56.1!
```

而 Playwright 的版本釘在 `web/trade/package.json`,所以這是結構性的,不是設定問題。第 6 節末尾的 Playwright 小節有完整的來龍去脈。**升級這台機器的 Ubuntu 之前,先確認新版本在 Playwright 的支援清單裡。**

如果想確認當下有哪些發行版可以裝,用 `wsl -l -o` 看清單,不要相信任何文件裡寫死的名字(包含這一份)。清單裡沒有 `Ubuntu-24.04` 的話,先跑一次 `wsl --update` 再看;還是沒有的話,Microsoft Store 裡搜 "Ubuntu 24.04" 也可以裝同一個東西。

**`wsl --install` 失敗的話**,多半是兩個 Windows 功能沒開。手動開(PowerShell,系統管理員),然後重開機:

```powershell
dism.exe /online /enable-feature /featurename:Microsoft-Windows-Subsystem-Linux /all /norestart
dism.exe /online /enable-feature /featurename:VirtualMachinePlatform /all /norestart
```

**機器上已經有其他發行版的話**,這樣做不會影響它們——每個發行版有自己的檔案系統,彼此看不到對方。但有兩件事是共用的,後面會踩到:第 4 節的 `.wslconfig` 是**整台虛擬機共用**的(所有發行版分同一份記憶體),而 Docker Desktop 的 WSL integration 是逐發行版開關的(第 6A 節會叫你把這個發行版的關掉)。

**想砍掉重來**,PowerShell:

```powershell
wsl --unregister Ubuntu-24.04
```

這會**永久刪除**那個發行版裡的所有東西——檔案、安裝的套件、runner 的設定,全部。刪完重跑 `wsl --install -d Ubuntu-24.04` 就是全新的。在裝壞了不知道哪裡壞的時候,這比逐項排查快。

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

**Node 用 NodeSource 22,不要用 Ubuntu 內建的。** 24.04 內建是 Node 18,而這個專案要 22。Ubuntu 還把 `npm` 拆成獨立套件,少裝一半就會出現第 15 節那個「指令掉到 Windows 去」的問題。NodeSource 的 `nodejs` 套件內含 npm。

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

### 第二件裝一次的事:foundry

`checks` 要 `forge build` 與 `forge test`,`helm` 要 `cast` 派生一把用完即丟的熱錢包私鑰。兩個 job 都需要 foundry,而版本釘在 `.env.example` 的 `FOUNDRY_TAG`:

```sh
curl -L https://foundry.paradigm.xyz | bash
"$HOME/.foundry/bin/foundryup" --install "$(sed -n 's/^FOUNDRY_TAG=//p' .env.example)"
"$HOME/.foundry/bin/forge" --version
```

最後一行要印出的版本必須和 `FOUNDRY_TAG` 對得上,因為 CI 每次都會檢查這件事(下面說明)。

**為什麼這一步不能交給 CI**——理由和 Playwright 那一段不同,而且是踩到才知道的:

`foundry-rs/foundry-toolchain@v1` 底下的 `foundryup` 會把每一次下載的二進位檔拿去比對 **GitHub 的 artifact attestation**,也就是要多打一通對外的網路請求。2026-09-10,那個端點連續十五分鐘回 HTTP 500:

```
foundryup: found attestation for v1.8.1 version, downloading attestation artifact, checking...
Error:
   0: failed to download https://github.com/foundry-rs/foundry/attestations/43723610/download: HTTP 500 Internal Server Error
```

`checks` 和 `helm` 一起紅,而且**紅在跟 Solidity 完全無關的分支上**——那台機器上明明已經有正確版本的 forge,卻因為要重新下載一次而被外面的故障拖下水。GitHub 當時沒有宣告任何事故,所以也沒有人會來通知你。

所以 workflow 改成:`vars.CI_RUNNER` 有值時**不跑那個 action**,只確認機器上已經有釘住的那個版本,然後把 `~/.foundry/bin` 加進 `PATH`。托管 runner(也就是 `release`)照舊完整安裝與驗證。

**版本釘沒有消失,它換了地方。** 以前由安裝器保證,現在由那個斷言保證,而且它失敗時會直接印出要打的指令:

```
forge on this runner is not the pinned v1.8.1. Reinstall it:
    foundryup --install v1.8.1
```

換句話說,升 `FOUNDRY_TAG` 之後,**這台機器要手動跟上**,否則 `checks` 會紅——這是刻意的,總比安靜地用舊版編譯合約好。

---

## 7. 裝 runner(裝兩個)

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

**然後整段再做一次**,換成 `~/runner-2` / `--name box-2`,回網頁拿一組**新的** token。

要求是:**各自獨立的目錄** + **唯一的 `--name`**(systemd 的 unit 名稱是 `actions.runner.<org>-<repo>.<name>.service`,由 name 衍生)。`_work` 相對於各自的目錄,所以自動就分開了。

為什麼要兩個?**一個 runner 一次只跑一個 job。** PR 的 job 圖是 `checks` → `integration` → (`helm` ∥ `e2e` ∥ `image`):前兩段本來就是串的,只有第三段能平行。

一台機器上實測一次完整的 PR run 是 **25 分鐘**牆鐘,其中第三段的三個 job 依序跑掉了 15 分鐘(helm 7m12、e2e 3m26、image 4m15)。第二個 runner 讓 `helm` 和另外兩個同時跑,第三段縮到約 7m41,整體約 18 分鐘。

**第三個 runner 只再省半分鐘左右**——第三段的長度由最長的 `helm` 決定,而 `e2e` 加 `image` 相加仍然比它短。所以裝兩個就好。每個 runner 閒置時大約吃 80 MB 記憶體、500 MB 磁碟。

裝完回網頁的 Runners 頁,應該看到兩個綠點,標籤都是 `exchange-ci`。

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
docker network prune -f --filter until=6h
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
0 4 * * 0 for d in $HOME/runner-1 $HOME/runner-2; do sudo $d/svc.sh stop; done; docker system prune -af --volumes; for d in $HOME/runner-1 $HOME/runner-2; do sudo $d/svc.sh start; done
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

> 這四項都過了之後,第 12 節說明你的機器現在實際上在跑什麼。

1. **看 runner 有沒有接到工作。** 隨便 push 一個 commit,到 Actions 頁點開任何一個 job,展開最上面的 **Set up job**,應該看到:

   ```
   Runner name: 'box-1'
   Runner group name: 'Default'
   ```

   如果看到 `Runner Image: ubuntu-24.04` 之類的,表示變數沒設成功,還在用 GitHub 的機器。

2. **重開機測試——這一項不能跳過。** 真的把 Windows 重開一次,**不要碰任何東西**,等幾分鐘後看倉庫的 Runners 頁:兩個 runner 應該自己回到 **Idle**。

   這是整個設計裡最容易失敗的地方(WSL 的 session 0 限制、閒置逾時、Docker 引擎)。沒通過這一項,就等於每次 Windows Update 之後 CI 都會停到你發現為止。沒過的話照第 15 節的順序查。

3. **看帳單有沒有停止增加。** **Settings** → **Billing** → **Actions**。自建 runner 跑的 job 在 API 上照樣有時間長度,但**不計入帳單**——要看的是 Billing 頁的實際數字,不是 job 的秒數。跑幾天後,每月用量應該從約 23,400 分鐘掉到 **50~100 分鐘**(只剩 `release`)。

4. **看磁碟。** 在 Ubuntu 裡:

   ```sh
   df -h /
   docker system df
   ```

   跑了一週後使用率應該穩在某個水位不再往上爬。一直漲就是 10.1 的清理沒生效——去 job log 裡找 `reclaim disk` 這個步驟,它會印出當下的磁碟狀況。Windows 那邊還要另外看 C 槽(10.3 節)。

---

## 12. CI 會跑哪些 job,每個 job 在證明什麼

裝好之後,你的機器上會跑七個 job。這一節說明每一個在證明什麼、會起什麼東西、什麼情況會紅、以及它對這台機器的要求——**因為前面每一項設定,都是為了下面某一個 job 存在的**。

秒數是這台機器上量到的實際值(一台 runner,五個 job 依序跑,牆鐘 25m05)。

### job 之間的關係

```
checks ─┬─► integration ─┬─► e2e  ─┐
        │                ├─► helm ─┼─► release(只有 v* tag)
        │                └─► image ┘
        └─► fuzz-smoke(只有 push)
```

前兩段是**串的**——`checks` 不過就不會跑 `integration`。第三段的三個可以平行,所以第二個 runner 才有意義(第 7 節有算式)。

還有一條規則:**合併到 main 的時候幾乎什麼都不跑**。因為這個專案一次只有一個 PR 在飛,合併出來的樹和 PR 上跑過的樹是同一棵,再跑一次是付兩次錢驗同一件事。只有 `fuzz-smoke` 和 `image` 會跑,因為它們是合併才做、PR 上沒做過的事。

---

### `checks` —— 靜態檢查與單元測試(2m20)

**跑什麼:** 21 個步驟,順序是刻意排的——**最便宜、最常失敗的先跑**,壞掉的改動幾秒就擋下來,不會先燒掉二十分鐘。

依序是:`go vet`、golangci-lint、gitleaks、`go.mod` 有沒有 tidy、九支 shell 腳本的語法、產生的程式碼(OpenAPI 與 sqlc)跟 repo 裡的一不一致、`sqlc vet`、compose 三種疊法都算得出來、fuzz target 有沒有跑到 `internal/matching` 以外的地方、單元與屬性測試(`-race`)、`internal/money` 的覆蓋率、兩個 binary 的版本與拒絕空助記詞、前台的 TS client 與 OpenAPI 一致且 build 得過、Solidity 合約 build 與 test。

**它在證明的事裡,有兩個是架構層級的:** golangci-lint 的設定裡有 depguard(擋跨層 import——領域套件不准 import NATS、`internal/chain` 不准 import `hdwallet`)和 forbidigo(擋金錢路徑上的 float)。**架構規則寫在文件裡會腐爛,寫成 linter 不會。**

**gitleaks 掃的是整個 git 歷史,不是工作目錄。** 所以 checkout 用 `fetch-depth: 0`——淺 checkout 會讓它幾乎什麼都沒掃到,而且不會報錯。

**這台機器要有:** gcc(`-race` 需要 cgo)、Node 22、foundry(第 6 節末尾裝一次,CI 在自建 runner 上不再自己裝)。**不需要 Docker。**

**最容易在這裡踩到的:** 缺 `build-essential` 時,前十二步全過,到 `-race` 那一步**零秒失敗**——零秒代表指令根本沒啟動,不是測試沒過。第 6A 節有完整說明。

---

### `fuzz-smoke` —— 亂數轟炸(30 秒 + build)

**跑什麼:** 對 `internal/matching` 的 `FuzzApply` 丟 30 秒隨機輸入。

**只在 push 上跑**——也就是合併到 main 和打 tag 的時候。PR 完全跳過。這是刻意的取捨:PR 上省下的是「單一目標 30 秒的模糊測試」,代價很小。

**它為什麼指名一個 package:** 原本是 `go test -list 'Fuzz.*'` 掃全部 43 個 package 去找那唯一一個目標,每個 package 都要連結一個測試 binary——四分鐘的帳單換一句 grep 的答案。所以現在直接寫死 `./internal/matching/`,並且在 `checks` 裡加一句斷言:**新增的 fuzz target 出現在別的地方會變紅燈,而不是沒人跑**。

**這台機器要有:** 只要 Go。

---

### `integration` —— 整合測試(7m39)

**跑什麼:** 用 testcontainers 起真的 Postgres 與 NATS,測 migration、seed、registry API、帳本、admin API、撮合引擎(kill/restart、屬性、併發)、outbox relay、public API(認證、HMAC、限流 429、狀態碼)。

**一個設計細節:** 這個 job 設了 `CI=true`,作用是讓「找不到 Docker」**變成失敗而不是跳過**。沒有它,一台 Docker 壞掉的機器會安靜地回報全綠。

**這台機器要有:** Docker。

**它是最貴的一個 job**(在托管 runner 上佔帳單的 25%),因為 `test/integration/` 大約會啟動 185 次 Postgres 容器。改成共用一個容器、每個測試一個 database 是已知該做但還沒做的事,記在 ADR-0012。

---

### `e2e` —— 端對端(3m26)

**跑什麼:** 這是**唯一驗證拆分部署的 job**。api / engine / chain / signer / stream / admin / worker 各一個容器,所以每一筆下單都必須真的走過 NATS 命令匯流排——在單一容器的模式下,那段程式碼會退化成直接函式呼叫,永遠測不到。

`scripts/e2e.sh` 有 24 個階段,依序:準備密鑰 → 起十幾個容器 → 引擎的命令匯流排通了 → signer 打開的種子和部署腳本注資的是同一個地址 → **對帳一開始就找到帳本沒被告知過的錢**(dev 鏈直接塞 100 ETH 給熱錢包)→ 記一筆開帳分錄讓對帳歸零 → 走完一輪交易 → 領充值地址 → 鏈上 ETH 充值到帳 → 鏈上 USDC 充值到帳 → 額度內的提現簽名送出並確認 → **收款地址真的收到 0.05 ETH** → 超額的提現停在人工審核 → 代幣提現移動代幣但用原生幣付 gas → resolve 拒絕一個已經來不及的動作 → 歸集 → 充值地址被清空 → **帳本 custody 對得上鏈** → `kill -9` 引擎後掛單還在 → 在一波下單中間 `kill -9`,重建的簿子和資料庫一致 → 停牌以 `market.updated` 傳到引擎 → **容器 log 裡沒有任何金鑰材料**。

之後還有兩步:Playwright 開無頭瀏覽器跑前台冒煙(註冊 → 注資 → 掛單 → 訂單簿出現 → 對手單 → 成交、餘額變動),以及備份還原演練(備份一次 → 還原到拋棄式 DB → 驗試算平衡 / 序號 / 成交 → 印出 RTO)。

**這台機器要有:** Docker(十幾個容器同時跑)、Playwright 的系統函式庫、Node。

**在自建 runner 上,Playwright 那一步只有 10 秒**——因為瀏覽器已經在 `~/.cache/ms-playwright`,而系統函式庫是第 6 節裝過一次的。workflow 用 `PLAYWRIGHT_INSTALL_DEPS` 把「下載瀏覽器」和「裝系統套件」拆開,只跳過後者,所以**快取被清掉的時候它會自癒,不會紅**。

**失敗的時候看什麼:** `e2e-compose-logs` 這個 artifact 有全部容器的 log;腳本的 EXIT trap 會把失敗的行號印在 200 行 log **之後**,所以捲到最下面。

---

### `helm` —— Kubernetes 部署(7m12)

**跑什麼:** build 一個 image → 用 kind 開一座真的 Kubernetes 叢集 → 把 image 載進去 → 建 chart 需要的 Secret 與 ConfigMap → `helm upgrade --install --wait` → 斷言 pod 裡跑的 binary 版本等於 chart 的 `appVersion` → 在叢集裡跑一輪交易 → 掛一張買單並記下訂單簿 → **刪掉 engine pod** → 等它重新拉起來 → 輪詢 30 次,訂單簿要**一字不差**地回來 → 刪掉叢集。

每一個 `exchangectl` 呼叫都是一個用同一個 image 開的拋棄式 pod,**不是 port-forward**——因為 port-forward 會在引擎重啟時斷掉,那樣測的就不是這件事了。

**這台機器要有:** Docker、kind、以及**第 5 節那兩個 inotify 參數**。那兩個參數就是為這個 job 設的:kind 的節點是一個容器裡跑一整套 Kubernetes,會開非常多檔案監看。少了它,叢集建到一半失敗,錯誤訊息跟 inotify 無關。

**它也是最吃記憶體的一個。** 一個 `kindest/node` image 大約 900 MB,而 `helm` 和 `e2e` 在 PR 上是可以平行的。

**失敗的時候看什麼:** `helm-kind-logs` 這個 artifact 裡有 `kubectl get all`、events、每個 pod 的 describe 與 log(含 `--previous`)、以及 `helm get manifest`。`events.txt` 通常一眼就看得出是哪個 pod 起不來。

---

### `image` —— 打包(4m15)

**跑什麼:** build 三個 image,而且每一個 build 完都**真的跑一次**:

- **app image** —— `exchange version` 與 `exchangectl version` 印得出來
- **edge image**(Caddy + 前台) —— `caddy validate` 兩份設定檔都過,而且 `/srv/index.html` 非空、`/srv/assets` 裡真的有 `.js`。**這一項在證明 SPA 真的被烤進去了**,不是 build 成功但檔案是空的
- **backup image** —— `mc` 與 `pg_dump` 在,而且打包進去的 `backup.sh` 語法正確

push 事件才會登入 ghcr 並推上去;PR 只 build 和 smoke。

**PR 上的三個 metadata 步驟會被跳過。** 它們算出來的 tag 與標籤只有 push 的推送步驟在讀,所以在 PR 上是三通沒有人讀結果的 GitHub API 呼叫。2026-09-09 那三通還真的把這個 job 弄紅過兩次——根因是 DNS,寫在第 15 節的常見問題裡。

**這台機器要有:** Docker + buildx。

**PR 上的 build metadata 是固定值**(`COMMIT=dev`、`DATE=1970-01-01T00:00:00Z`),不是真的 commit。原因是 `build/Dockerfile` 把這些烤進 ldflags,每個 commit 都變的話,最後那層 `go build` 的快取**依設計不可能命中**。PR 的建置不會被發布,所以固定它沒有代價。

---

### `release` —— 發布(只有 `v*` tag)

**跑什麼:** 從 ghcr 把剛推上去的 image 拉回來、跑 `version --json`、斷言版本等於 tag;確認 `-edge` 與 `-backup` 在同一個版本也存在;打包 Helm chart 並斷言它的 `appVersion` 等於 tag;推 chart 到 ghcr 的 OCI registry;交叉編譯兩個平台的 `exchangectl`;算 SHA256;開 GitHub Release 並附上檔案。

**這個 job 固定跑在 `ubuntu-latest`,不會用你的機器。** 它一個月跑幾次,而且要推到 ghcr 與開 Release——**發布不該取決於一台家用機器有沒有開著**。這是 ADR-0012 決定 1 的例外條款。

---

### 一句話總結每個設定是為了誰

| 你在第幾節做的事 | 是為了哪個 job |
|---|---|
| §3 systemd | 全部——runner 服務、dockerd、sysctl 都靠它 |
| §4 兩個閒置逾時 | 全部——沒有它,runner 在你關掉終端機 15 秒後離線 |
| §5 inotify | `helm` |
| §6 Docker 引擎 | `integration`、`e2e`、`helm`、`image` |
| §6 `build-essential` | `checks`(`-race` 需要 cgo) |
| §6 Node / npm | `checks`(前台 build)、`e2e`(Playwright) |
| §6 Playwright 系統函式庫 | `e2e` |
| §9 電源與自動啟動 | 全部——每月 Windows Update 重開之後還能自己回來 |
| §10 磁碟清理 | `integration`、`e2e`、`helm`、`image` 累積出來的東西 |


## 13. Linux VPS(另一條路線)

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

## 14. 機器規格不夠的時候

如果只有 2 核 / 8 GB,`e2e` 和 `helm` 會很吃力(前者十幾個容器,後者一整個 Kubernetes)。折衷做法是**只把吃計費最兇、但資源需求中等的 job 搬過去**:

| 搬到自己的機器 | 留在 GitHub |
|---|---|
| `integration`(佔帳單 25%) | `e2e`(要十幾個容器) |
| `checks`(31%,原本的 lint + unit + fast-checks) | `helm`(要 kind 叢集) |
| `fuzz-smoke`(9%) | `image`、`release` |

這樣搬走約 65% 的帳單。作法是把那幾個 job 的 `runs-on` 改成另一個變數(例如 `vars.CI_RUNNER_LIGHT`),其餘留 `ubuntu-latest`。

但要知道這只是過渡:剩下的還是每月約 8,200 分鐘,仍然超標。真正的解法是把機器換大一點。

---

## 15. 常見問題

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

**Q: `checks` 或 `helm` 死在 `foundryup`,錯誤只有 `failed to download ... /attestations/... HTTP 500`。**
不是你的機器,也不是這個分支。`foundryup` 會把下載的二進位檔比對 GitHub 的 artifact attestation,而那個端點會壞——2026-09-10 連壞十五分鐘,`githubstatus.com` 上什麼都沒宣告。它死在安裝階段,`forge` 一次都還沒跑,所以任何測試結果都不能怪。

自建 runner 上現在不該再看到它:workflow 只在托管 runner 才跑那個 action。如果你還是看到了,表示 `CI_RUNNER` 這個倉庫變數沒設或設錯(第 8 節),job 落回了 `ubuntu-latest`。

真的落在托管 runner 而又非過不可時,只能等它自己好——`foundry-toolchain@v1` 沒有任何 input 可以傳 `--force` 進去跳過驗證,而繞過驗證本來也不該當成常態。

**Q: `forge on this runner is not the pinned v...`。**
有人升了 `.env.example` 的 `FOUNDRY_TAG`,而這台機器還停在舊版。照它印出來的那行打一次就好:`foundryup --install <tag>`。這個斷言是刻意會擋的——第 6 節末尾說明了為什麼版本釘從安裝器搬到了這裡。

**Q: C 槽滿了。**
看 10.3 節。路線 6A:先在 Ubuntu 裡 `docker system prune -af --volumes`(記得先停 runner),再 PowerShell `wsl --shutdown` + `wsl --manage Ubuntu-24.04 --compact`。路線 6B:只能用 Docker Desktop 的「Clean up data」。

**Q: `helm` job 在我的機器上失敗,GitHub 上卻是綠的。**
八成是第 5 節的 inotify 參數沒生效(重啟後驗證過了嗎?),或是記憶體不夠(4.2)。job 的 artifact 裡有 `helm-kind-logs`,裡面 `events.txt` 會說是哪個 pod 起不來。

**Q: 我改了 `.wslconfig` 但好像沒作用。**
格式錯誤的 `.wslconfig` 會被**靜默忽略**,不報錯。開 **WSL Settings** App 看它顯示的值對不對,並確認改完有跑 `wsl --shutdown`(要等約 8 秒才會真的停)。

**Q: 某個步驟死於 `Connect Timeout Error`,但機器明明連得上網。**
先不要猜,量一次。這在 2026-09-09 真的發生過,而三個看起來最像的答案全是錯的。

```sh
curl -sS -o /dev/null -w 'dns=%{time_namelookup} conn=%{time_connect} tls=%{time_appconnect} total=%{time_total}\n' \
  https://api.github.com/repos/<owner>/<repo>
```

`total` 很大但 `conn` 減 `dns` 很小,就代表**時間全花在 DNS**,不是網路品質、不是 MTU、也不是 TLS。那次的數字是 `dns=16.092`,其餘三段加起來 0.109 秒。

接著切開 A 和 AAAA:

```sh
time getent ahostsv4 api.github.com >/dev/null   # 只問 A
time getent ahosts   api.github.com >/dev/null   # A + AAAA
time getent ahosts   example.com    >/dev/null   # 對照組
```

那次量到 0.048s / 17.339s / 0.026s——**只有那一個名字的 AAAA 查詢沒人回答**,`example.com` 同時問兩種只要 26 ms。兇手是 `/etc/resolv.conf` 裡的 `nameserver 10.255.255.254`,也就是 WSL 自己的 DNS 代理。

修法是換掉它。先看現有內容,確認沒有 `[network]` 段落再加:

```sh
cat /etc/wsl.conf
sudo tee -a /etc/wsl.conf >/dev/null <<'EOF'

[network]
generateResolvConf = false
EOF

sudo rm -f /etc/resolv.conf
sudo tee /etc/resolv.conf >/dev/null <<'EOF'
nameserver 1.1.1.1
nameserver 8.8.8.8
options timeout:2 attempts:2 single-request-reopen
EOF
```

然後 PowerShell `wsl --shutdown`、重開、再量一次 `dns=`。

**`options timeout:2 attempts:2` 不是裝飾。** 很多 action 的 HTTP 客戶端(Node 的 undici)connect timeout 預設就是 10 秒,而 glibc 的預設是 5 秒試 2 次——一個沒人回答的查詢會拖到 17 秒,剛好落在錯的一側。改成 2 秒試 2 次之後,最糟是 4 秒,**結構上再也撞不到那個門檻**。

**但書:** 寫死公共 DNS 之後,公司內網或 VPN 的名字會查不到。這台機器如果也要連內網,把 nameserver 換成你路由器的位址。

**Q: `image` job 的 `push` 死在 `lookup ghcr.io on 8.8.8.8:53: no such host`。**
和上面那則是同一個主題的不同一層,而且**根因到現在沒有確立**——這則記的是排除過程,不是答案。

2026-09-10 發生過一次:三個 image 全部 build 成功、三個 smoke 全過,**第 13 步 `login to ghcr` 花 7 秒成功**,第 14 步 `push` 在 0 秒後就死在上面那行。重跑就過,而且機器上什麼都沒改。

關鍵的對照是那兩步的落差:`docker/login-action` 跑在**主機**上,它確實連到了 ghcr.io;`push` 跑在 buildx 的 **builder 容器**裡(job log 尾端的 `docker buildx rm builder-...` 就是它)。所以第一個假設是「主機和容器用的不是同一個 resolver」。

**那個假設被實測推翻了。** 量一次就知道:

```sh
cat /etc/resolv.conf                              # 主機
docker run --rm alpine cat /etc/resolv.conf       # 容器
```

兩份**一模一樣**,而且容器那份自己寫著 `# Based on host file: '/etc/resolv.conf' (legacy)` 與 `# Overrides: []`——Docker 原封不動照抄,沒有替換任何東西。「Docker 因為 loopback 而自己換成 8.8.8.8」在這台機器上不成立:`8.8.8.8` 正是上面那則 FAQ 叫你寫進 `/etc/resolv.conf` 的**次要** resolver。

所以那行訊息真正的意思是:**它是最後被試的那一個,也就是連 `1.1.1.1` 都沒答出來。** 兩個互不相干的公共 resolver 同時失效,指向的是封包出不去那一段(WSL 的 NAT 或 Windows 端),不是任何一個 resolver。

因為是間歇的,看設定看不出來,要量「穩不穩」:

```sh
docker run --rm alpine sh -c \
  'for i in $(seq 1 20); do nslookup ghcr.io >/dev/null 2>&1 && printf . || printf X; done; echo'
for i in $(seq 1 20); do getent hosts ghcr.io >/dev/null && printf . || printf X; done; echo
```

事故之後量,容器與主機**都是 20/20 成功**,重現不出來。所以目前的處置就是重跑那個 job,並且知道它可能再發生。真的要根治,方向是讓 builder 不要多走 bridge 到 NAT 那一跳,而不是再去動 resolver。

**順帶一個可以重用的手法。** 當時看起來像「MSI 壞、MSI2 好」(第一次在 MSI 失敗、重跑在 MSI2 成功)。job log 的工作目錄一步就把這個方向刪掉了:一邊是 `actions-runner`、一邊是 `actions-runner2`,同一個使用者、同一個發行版,**共用同一個 Docker daemon**。一個 daemon 只有一份容器 DNS 設定,不可能一台解析得到一台解析不到——差別是時間,不是機器。裝兩個 runner 的代價之一是容易誤以為它們是兩台獨立的機器。

**Q: `e2e` 死在 `dial unix /run/buildkit/buildkitd.sock: connect: no such file or directory`。**
和上面那則相反,這一則**根因確立、也已經修掉了**;寫在這裡是因為錯誤訊息本身看不出成因。

症狀:`compose up --build` 跑了二十幾個建置步驟之後,buildkit 的 socket 憑空消失;整個 job 三十幾秒就結束(正常約三分鐘),容器一個都沒起來。

成因是兩個 job 搶同一份狀態。buildx 把 instance 的定義、以及「目前選取哪一個」的指標放在 `$BUILDX_CONFIG`(預設 `~/.docker/buildx`),而**兩個 runner 是同一個使用者的兩個行程,那份預設是共用的**。第三階段刻意讓 `e2e`、`helm`、`image` 並行,於是:`helm` 的 `setup-buildx-action` 建立並**選取**一個 builder → `e2e`(它沒有自己的 `setup-buildx-action`)沿用了那個選取 → `helm` 收尾時 `docker buildx rm` 把它移除,而 `e2e` 的建置還在跑。

因為只有兩個 runner 對三個 job,`e2e` 常常最後起跑,剛好落進別人的收尾窗口——所以它是間歇的,而且**重跑通常會過**(那一次它獨自執行)。重跑會過正是這類問題最容易被放過的地方。

修法在 `.github/workflows/ci.yml`:那三個 job 的第一步各自把 `BUILDX_CONFIG` 指到自己的 `$RUNNER_TEMP/buildx`。`RUNNER_TEMP` 是每個 runner 行程一份,而同一個 runner 上的 job 是循序的,剛好是這份狀態需要的範圍。`e2e` 因此拿到一個空的狀態目錄,退回用 `default`——daemon 自己的 builder,沒有任何 job 會建立或移除它。

如果又看到這個錯誤,先確認那一步還在、而且排在 `setup-buildx-action` 前面。

**而且不只 buildx。** 一個 daemon 之下,**任何**「兩個 job 各自假設自己獨佔」的資源都會撞。修完 buildx 之後就撞到了第二個:`helm` 與 `image` 都把自己建的 image 標成 `crypto-exchange:ci`,推進同一個 daemon,而且 `VERSION` build-arg 不同——誰的 `--load` 最後落地誰就擁有那個 tag。`helm` 會斷言 pod 裡的版本等於 chart 的 appVersion,所以搶輸的時候它會紅;`image` 的 smoke 只跑 `version` 不比對輸出,所以它搶輸的時候**靜靜地跑了別人的 binary 也照樣綠**。兩邊都中了,只有一邊看得見。

修法是給 `helm` 自己的 tag(`crypto-exchange:ci-helm`)。看到類似的症狀時,先問的不是「這個 job 壞了嗎」,而是「這一輪還有誰在用同一個 daemon 上的同一個名字」。

**Q: 想暫時全部回到 GitHub 的機器上。**
刪掉倉庫變數 `CI_RUNNER`。一秒生效,不用改 code。

**Q: 那 timeout 呢?**
每個 job 都有 `timeout-minutes`(15~30 分鐘)。這在自己的機器上一樣重要:一個卡住的 job 會佔住三分之一的 runner 容量,直到有人發現。

**Q: 這樣做安全嗎?**
私有倉庫、單人提交,風險可控。但要記得四件事:機器上不要放別的東西、fork PR 要關掉(第 8 節)、自動登入代表實體接觸就等於登入(9.3)、WSL 看得到 Windows 的檔案(`/mnt/c`)。「拋棄式機器」這句話在 Windows 路線上一樣成立。
