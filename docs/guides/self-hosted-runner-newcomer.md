# 讓自己的電腦幫忙跑測試(新手版)

> 這份文件假設你**什麼都不知道**。不需要懂 CI、不需要懂 Docker、不需要會寫程式,照著打就好。
>
> 每一步都有「**你會看到:**」。看到的跟寫的不一樣,先看第 4 節。
>
> 需要更深入的說明——為什麼這樣設定、每個參數在做什麼、Linux 主機怎麼裝——請看工程師版:[`self-hosted-runner.md`](self-hosted-runner.md)。

## 1. 這是什麼?(先花兩分鐘)

每次有人改了這個專案的程式碼,GitHub 會自動開一台機器,把整套系統從頭檢查一遍——編譯、跑測試、把整座交易所架起來買賣一輪、再拆掉。大概 25 分鐘。

這件事叫 **CI**(持續整合)。它很有用,但是**要錢**。

私有專案在 GitHub 上是**按分鐘計費**的。實際量過:這個專案一個月會用掉 Pro 方案額度的 **7.8 倍**。升方案追不上,因為用量還在長。

好消息是:**你可以叫自己的電腦來做這件事,而且完全免費。**

就像洗衣服。每次都拿去自助洗衣店,一次五十塊,洗久了很可觀;家裡有一台閒置的洗衣機的話,自己洗就好,只花一點電費。

**這份文件教你把家裡那台洗衣機接上。**

做完之後:

| | 之前 | 之後 |
|---|---|---|
| 每次檢查花的時間 | 45 個計費分鐘 | 25 分鐘 |
| 每個月的帳單 | 額度的 7.8 倍 | **零** |
| 誰在跑 | GitHub 的機器 | 你家那台 |

## 2. 開始之前要準備什麼

| 要有 | 說明 |
|---|---|
| 一台 Windows 11 電腦 | **平常會開著的那台**。它需要在別人改程式的時候是醒著的 |
| 大約 30 GB 可用磁碟空間 | 檢查的過程會下載不少東西 |
| 16 GB 以上記憶體 | 分 12 GB 給它用 |
| 系統管理員權限 | 有幾個步驟需要 |
| 大約一小時 | 大部分時間在等下載 |

還有一件事要先知道,**這個很重要**:

> **這台電腦要當成「拋棄式」看待。** CI 的工作就是執行別人送上來的程式碼——那是它存在的意義。所以這台機器上**不要放你的私人金鑰、正式環境的密碼、或其他重要的東西**。
>
> 這不是危言聳聽,是這類設定的標準前提。第 7 節會再講一次。

## 3. 照著做

整個過程分成兩半:**前半在 Windows 裡打(藍色的 PowerShell 視窗)**,**後半在 Ubuntu 裡打(黑色的 Ubuntu 視窗)**。每一步都會寫明是哪一個。

### 第 1 步:把 WSL 更新到最新

WSL 是「在 Windows 裡跑 Linux」的功能。Linux 是另一套作業系統,CI 的工具幾乎都是為它寫的。

**PowerShell(用系統管理員身分開啟):**

```powershell
wsl --update
wsl --version
```

**你會看到:** 一串版本號,第一行是 `WSL 版本: 2.x.x.x`。

> 這一步不能跳過。等一下要裝的 Ubuntu 24.04 用了新的打包格式,**WSL 太舊會失敗,而且錯誤訊息看不懂**。

### 第 2 步:裝 Ubuntu 24.04

**PowerShell(系統管理員):**

```powershell
wsl --install -d Ubuntu-24.04
```

**你會看到:** 下載進度,然後要你重開機。重開。

重開之後 Ubuntu 會自己跳出來,問你要設定的使用者名稱和密碼。**這組密碼要記住**,後面每次打 `sudo` 都會用到。它跟你的 Windows 密碼是兩回事。

**為什麼一定是 24.04,不能裝更新的?** 因為 CI 裡有一步要開瀏覽器測試網頁,那個工具**只認得它有支援清單的 Ubuntu 版本**。26.04 太新,清單裡沒有,會直接失敗。這是實際踩到的,不是猜的。

想看有哪些版本可以裝:

```powershell
wsl -l -o
```

**確認裝好了:**

```powershell
wsl -l -v
```

**你會看到:** 表格裡有一列 `Ubuntu-24.04`,VERSION 那欄是 `2`。如果是 `1`,打 `wsl --set-version Ubuntu-24.04 2`。

### 第 3 步:打開 systemd

systemd 是 Linux 的「開機自動啟動」機制。沒有它,等一下裝的東西關掉視窗就沒了。

**Ubuntu 視窗:**

```sh
sudo tee /etc/wsl.conf >/dev/null <<'EOF'
[boot]
systemd=true

[interop]
appendWindowsPath=false

[automount]
enabled=false
EOF
```

會問你密碼,就是第 2 步設的那組。

中間那兩段是**隔離設定**:不要讓 Ubuntu 看到 Windows 的程式和硬碟。為什麼?因為如果 Linux 裡少裝了什麼東西,系統會**默默地改用 Windows 的版本**,然後在一個完全無關的地方壞掉,錯誤訊息看不懂。這也是實際踩到的。

**PowerShell:**

```powershell
wsl --shutdown
```

等 8 秒,再從開始選單打開 Ubuntu。

**Ubuntu 視窗,驗證:**

```sh
systemctl is-system-running
ls /mnt/
```

**你會看到:** 第一行是 `running`(`degraded` 也可以)。第二行**什麼都沒有**——那就對了,代表隔離生效。

### 第 4 步:分配資源,並且不讓它自己睡著

**PowerShell:**

```powershell
notepad "$env:USERPROFILE\.wslconfig"
```

如果檔案不存在,記事本會問要不要新建,按是。把下面這些貼進去(**已經有內容的話是合併,不要整個蓋掉**):

```ini
[general]
instanceIdleTimeout=-1

[wsl2]
memory=12GB
processors=12
swap=8GB
vmIdleTimeout=-1
```

存檔。

那兩個 `-1` 是整份文件裡**最容易被漏掉、後果最嚴重**的設定。

WSL 有**兩個**閒置計時器,不是一個。有文件的那個是 `vmIdleTimeout`;另一個 `instanceIdleTimeout` 沒有寫在官方文件裡,**預設 15 秒**。只設有文件的那個,結果會是:虛擬機還活著,但裡面的 Ubuntu 已經被關掉了——你關掉終端機十五秒之後,它就從 GitHub 上離線。

**PowerShell:**

```powershell
wsl --shutdown
```

等 8 秒,重開 Ubuntu。

**Ubuntu 視窗,驗證:**

```sh
free -m
```

**你會看到:** `Mem:` 那一列的 total 大約是 11000 到 12000 之間。

### 第 5 步:裝工具

**Ubuntu 視窗:**

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates curl git jq unzip make build-essential
```

`build-essential` 是 C 語言編譯器。**它不能漏**——測試裡有一種叫「競態偵測」的檢查需要它,少了就會在一個完全看不出原因的地方失敗。這也是實際踩到的。

```sh
curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
sudo apt-get install -y nodejs
```

**你會看到:** 一堆下載訊息。

**驗證:**

```sh
gcc --version
node --version
command -v npm npx
```

**你會看到:** gcc 有版本號;node 是 `v22.` 開頭;npm 和 npx 都在 `/usr/bin/` 底下。

> 如果 npm 或 npx 顯示的路徑裡有 `/mnt/c/`,表示第 3 步的隔離設定沒生效,回去檢查。

### 第 6 步:裝 Docker

Docker 是「容器」技術——可以想成一個個獨立的小盒子,每個盒子裡裝一個程式,彼此不干擾。CI 需要它來把整座交易所架起來。

**Ubuntu 視窗:**

```sh
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker "$USER"
sudo systemctl enable --now docker
```

**然後關掉 Ubuntu 視窗再重開一次**——加入群組要重新登入才生效。

**驗證:**

```sh
docker run --rm hello-world
docker buildx version
```

**你會看到:** 第一個印出 `Hello from Docker!`。**注意它前面不用加 `sudo`**——如果要加才行,表示群組沒生效,再關掉視窗重開一次。

> **如果你的電腦裝過 Docker Desktop**,請把它對這個 Ubuntu 的整合關掉(Docker Desktop 設定 → Resources → WSL integration),否則兩個引擎會搶同一個位置。
>
> 另外,如果 `docker run` 說找不到 `/var/run/docker.sock`,看第 4 節的第三列。

### 第 7 步:給 kind 兩個核心參數

CI 有一關會在你的電腦裡開一座迷你的 Kubernetes(一種管很多台伺服器的系統),那個工具叫 kind。它需要 Linux 放寬兩個限制。

**Ubuntu 視窗:**

```sh
sudo tee /etc/sysctl.d/99-kind.conf >/dev/null <<'EOF'
fs.inotify.max_user_watches = 524288
fs.inotify.max_user_instances = 512
EOF
sudo sysctl --system
```

**你會看到:** 一串 `* Applying ...` 的訊息,最後兩行是你剛設的兩個值。

### 第 8 步:裝瀏覽器

CI 有一關會開一個沒有畫面的瀏覽器,像真人一樣操作網頁。

**Ubuntu 視窗:**

```sh
npx playwright@1.56.1 install --with-deps chromium
```

**你會看到:** 下載進度,最後沒有錯誤。

**驗證:**

```sh
ls ~/.cache/ms-playwright
```

**你會看到:** 一個 `chromium-` 開頭的資料夾。

> **這是整個過程中唯一需要密碼的一步,而且只做這一次。** 之後 CI 每次跑只會用到已經裝好的瀏覽器,不會再要權限。

### 第 9 步:註冊兩個 runner

「runner」就是「幫忙跑工作的機器人」。一個 runner 一次只能做一件事,所以裝兩個會比較快(25 分鐘 → 18 分鐘)。

第三個就沒意義了,原因在工程師版有算式。

**在瀏覽器裡:** 打開這個專案的 GitHub 頁面 → **Settings** → 左邊選單的 **Actions** → **Runners** → 右上角 **New self-hosted runner** → 作業系統選 **Linux**。

網頁會給你一段指令。**兩個 runner 要各自拿一次,token 一小時就失效。**

**Ubuntu 視窗,第一個:**

```sh
mkdir -p ~/runner-1 && cd ~/runner-1
```

接著貼上網頁給你的 `curl` 和 `tar` 那兩行(**不要貼 `./config.sh` 那行**)。

然後打這一行,把 `<你的TOKEN>` 換成網頁上的:

```sh
./config.sh --url https://github.com/arc119226/crypto-exchange \
  --token <你的TOKEN> --name ci-1 --labels exchange-ci \
  --work _work --unattended
```

`--labels exchange-ci` 是**標籤**,等一下第 10 步的設定要跟它一字不差。

```sh
sudo ./svc.sh install && sudo ./svc.sh start && sudo ./svc.sh status
```

**你會看到:** 最後一行是綠色的 `active (running)`。

**第二個 runner:** 回網頁**再拿一次新的 token**,然後整段重做一次,只有兩個地方要改——目錄改成 `~/runner-2`,名字改成 `--name ci-2`。

**最後,擋掉一個會搗亂的東西:**

```sh
sudo mkdir -p /etc/needrestart/conf.d
echo '$nrconf{override_rc}{qr(^actions\.runner\..*\.service$)} = 0;' | \
  sudo tee /etc/needrestart/conf.d/actions_runner_services.conf
```

Ubuntu 有個功能會在套件更新後自動重啟服務——如果它在 CI 跑到一半重啟 runner,那次檢查就白跑了。

**在瀏覽器裡驗證:** 回到 Runners 那一頁。

**你會看到:** 兩個綠點,名字是 `ci-1` 和 `ci-2`,狀態 **Idle**,標籤都是 `exchange-ci`。

### 第 10 步:按下開關

到目前為止機器都準備好了,但 CI **還是跑在 GitHub 的機器上**。要切過來,得設一個變數。

**在瀏覽器裡:** Settings → **Secrets and variables** → **Actions** → 上面切到 **Variables** 分頁 → **New repository variable**。

- Name:`CI_RUNNER`
- Value:`exchange-ci`(跟第 9 步的標籤一字不差)

> ⚠ **這裡有一個很容易踩的坑。**
>
> 那一頁有兩個區塊:**Repository variables** 和 **Environment variables**。要按的是**上面那個**。
>
> 另外,左邊選單裡還有一個叫 **Environments** 的功能——那是**完全不同的東西**,在那裡建一個叫 `CI_RUNNER` 的環境是沒有用的,CI 看不到它。這個坑是實際踩過的。

**驗證:** 隨便推一個改動,或請人開一個 PR。到 Actions 頁面點進去,展開任何一個工作的 **Set up job**。

**你會看到:** `Runner name: 'ci-1'`(或 `ci-2`)。如果看到 `Runner Image: ubuntu-...`,表示變數沒設對,還在用 GitHub 的機器。

> **想關掉隨時可以**:把 `CI_RUNNER` 這個變數刪掉,下一次就全部回到 GitHub 的機器上。不用改程式、不用等審核,一秒生效。

### 第 11 步:不讓電腦睡著,並且重開機能自己回來

**PowerShell(系統管理員):**

```powershell
powercfg /change standby-timeout-ac 0
powercfg /change hibernate-timeout-ac 0
powercfg /change disk-timeout-ac 0
powercfg /change monitor-timeout-ac 10
powercfg /h off
powercfg -attributes SUB_SLEEP 7bc4a2f9-d8fc-4469-b07b-33eb785aaca0 -ATTRIB_HIDE
powercfg /setacvalueindex SCHEME_CURRENT SUB_SLEEP 7bc4a2f9-d8fc-4469-b07b-33eb785aaca0 0
powercfg /setactive SCHEME_CURRENT
```

前面幾行是「插著電就不要睡」。螢幕 10 分鐘關掉沒關係,那不影響。

**最後三行很重要而且很容易漏**:Windows 還有一個藏起來的設定叫「系統無人參與睡眠逾時」,預設 **2 分鐘**,前面那幾行蓋不到它。機器在沒人操作的狀態下被叫醒時用的就是這個值——不改的話,CI 跑到一半電腦會自己睡著。

**然後讓 Ubuntu 開機自己起來。** WSL 有一個限制:**它沒辦法在你登入 Windows 之前啟動**。所以做法是「你一登入,它就自動起來並且一直活著」。

按 `Win` + `R`,輸入 `shell:startup`,按 Enter。會打開一個資料夾。

在裡面新建一個檔案叫 `keep-wsl-alive.vbs`(**副檔名是 .vbs**),內容:

```vbscript
CreateObject("WScript.Shell").Run "wsl.exe -d Ubuntu-24.04 -- /bin/sh -c ""exec sleep infinity""", 0, False
```

它會在背景開一個什麼都不做的程序,把 Ubuntu「釘」在開著的狀態。用 `.vbs` 而不是 `.bat` 是為了不要每次登入都閃一個黑視窗。

### 第 12 步:重開機測試 —— 這一步不能跳過

**真的把 Windows 重開一次。登入。然後什麼都不要碰。**

等幾分鐘,打開 GitHub 的 Runners 頁面。

**你會看到:** `ci-1` 和 `ci-2` 兩個綠點,自己回到 **Idle**。

**如果沒有回來,這套設定就是沒裝好。** 不要跳過這一步——Windows 每個月會因為更新自動重開一次,那時候如果 runner 回不來,CI 會默默停掉,直到有人發現。

## 4. 出了問題?

| 你看到 | 原因 | 怎麼辦 |
|---|---|---|
| 工作卡在 `Waiting for a runner to pick up this job` | 標籤對不上 | Runners 頁面上的標籤,要跟第 10 步的變數值**一字不差**。大小寫也算 |
| 展開 Set up job 看到 `Runner Image: ubuntu-...` | 變數沒設對 | 多半是設成 Environment 了。回第 10 步的警告框 |
| `go: -race requires cgo` | 少了 C 編譯器 | 第 5 步的 `build-essential` 沒裝到。`gcc --version` 確認一下 |
| `dial unix /var/run/docker.sock: no such file` | Docker 引擎沒起來,或這台機器裝過 Docker Desktop | 先 `sudo systemctl status docker` 看是不是 `active`。如果是 active 卻還是這個錯,打 `readlink -f /var/run`,它應該印 `/run`;印別的就照工程師版第 6A 節修 |
| 打指令時出現 `CMD.EXE ... 不支援 UNC 路徑` | 指令跑到 Windows 去了 | 第 3 步的隔離設定沒生效,或 Linux 版沒裝。`command -v <指令>` 看路徑裡有沒有 `/mnt/c/` |
| 關掉終端機之後 runner 就離線 | 兩個閒置逾時 | 回第 4 步,確認**兩個** `-1` 都在 |
| `helm` 那一關失敗,GitHub 上卻是綠的 | kind 的參數沒生效,或記憶體不夠 | 第 7 步重做一次並重開 Ubuntu。還是不行就把第 4 步的 `memory` 調到 16GB |
| 磁碟滿了 | CI 會累積映像檔 | 工程師版第 10 節有每週清理的做法 |
| 重開機之後 runner 沒回來 | 第 11 步的自動啟動沒生效 | 檢查 `shell:startup` 裡的 `.vbs` 檔存在、副檔名真的是 `.vbs`、裡面的發行版名稱拼對 |

還是不行的話,工程師版的第 14 節有更完整的問答。

## 5. 它會幫你跑哪些事

每次有人改程式,你的電腦會跑七個工作。前兩個是接力,後面三個可以同時跑。

**checks —— 靜態檢查與單元測試(約 2 分鐘)**

二十一項檢查:程式碼風格、架構分層規則(例如「管地址的程式不准碰管金鑰的程式」)、**整個 git 歷史裡有沒有不小心提交過金鑰**、自動產生的程式碼跟規格書一不一致、金錢模組的測試覆蓋率有沒有到 95%、兩個執行檔跑不跑得起來、網頁前台編不編得過、智能合約的測試過不過。

順序是刻意排的:**最便宜、最常壞的先跑**,這樣有問題幾秒就擋下來。

**fuzz-smoke —— 亂數轟炸(約 30 秒)**

對撮合引擎丟 30 秒的隨機資料,看它會不會崩潰。只在程式碼合併之後跑。

**integration —— 整合測試(約 8 分鐘)**

真的開一個資料庫和一個訊息佇列,測帳本記帳對不對、後台功能正不正常、撮合引擎重啟之後訂單簿有沒有跑掉。

**e2e —— 端對端(約 3 分半)**

**這是唯一驗證「拆開部署」的一關。** 七個角色各自一個容器,所以每一筆下單都必須真的走過容器之間的通訊。

它做的事:在一條假的區塊鏈上真的匯錢進來、等確認、下單、成交、提現出去、把散落的錢收回來、跟鏈上對一次帳。中間還會**故意把撮合引擎砍掉**(直接殺掉,不是好好關機),看重開之後訂單簿是不是一分不差。

然後開一個沒有畫面的瀏覽器,像真人一樣註冊、入金、掛單、看它成交。最後備份一次資料庫、還原到另一個資料庫、驗證還原出來的帳是平的。

**helm —— Kubernetes 部署(約 7 分鐘)**

在你的電腦裡開一座迷你的 Kubernetes 叢集,把整座交易所裝進去,跑一輪,**在那裡也故意砍一次撮合引擎**,然後把叢集整個刪掉。

這一關最吃資源,第 7 步那兩個參數就是為它設的。

**image —— 打包(約 4 分鐘)**

三個映像檔都能不能 build 起來,而且**真的能跑**:執行檔印不印得出版本、網頁伺服器的設定檔通不通過驗證、前台的靜態檔有沒有真的烤進去、備份工具在不在裡面。

**release —— 發布**

只在正式發版的時候跑,而且**它固定跑在 GitHub 的機器上,不會用你的電腦**。發布不應該取決於一台家用電腦有沒有開著。

## 6. 名詞小抄

| 名詞 | 意思 |
|---|---|
| **CI** | 持續整合。每次改程式就自動檢查一遍的機制 |
| **workflow** | 一整套檢查流程的定義 |
| **job(工作)** | workflow 裡的一個階段。這個專案有七個 |
| **runner** | 實際執行工作的機器人。這份文件就是在裝它 |
| **self-hosted runner** | 跑在自己機器上的 runner(相對於跑在 GitHub 機器上的) |
| **標籤 label** | 貼在 runner 上的名字,用來指定「這個工作要給誰做」 |
| **WSL** | Windows Subsystem for Linux,在 Windows 裡跑 Linux 的功能 |
| **發行版 distro** | Linux 的一個版本。這裡用 Ubuntu 24.04 |
| **systemd** | Linux 的開機自動啟動機制 |
| **Docker** | 容器技術。把程式裝在互相隔離的小盒子裡 |
| **容器 container** | 那個小盒子,執行中的狀態 |
| **映像檔 image** | 小盒子的模板,還沒跑起來的狀態 |
| **kind** | 在 Docker 裡開一座迷你 Kubernetes 的工具 |
| **Kubernetes** | 管理很多台伺服器上的容器的系統 |
| **sudo** | Linux 的「用系統管理員身分執行」 |

## 7. 安全提醒

**再說一次:這台電腦要當成拋棄式的。**

CI 的工作就是執行分支上的任意程式碼。那是它的功能,不是漏洞。所以:

- **不要在這台機器上放**你的私鑰、正式環境的密碼、或其他重要服務。
- **把 fork PR 的自動執行關掉。** Settings → Actions → General → 找到 fork 相關的核准設定,改成需要人工核准。否則任何陌生人開一個 PR,就等於在你的電腦上執行他寫的程式。
- 這是私有專案 + 只有你一個人提交的情況下風險可控。**如果之後開放給更多人提交,要重新評估。**

如果哪天覺得不對勁,**關掉的方法只有一個動作**:把 GitHub 上的 `CI_RUNNER` 變數刪掉,一切立刻回到 GitHub 的機器上。

## 8. 接下來可以看什麼

| 想知道 | 去看 |
|---|---|
| 每個設定為什麼要這樣、Linux 主機怎麼裝、更完整的疑難排解 | [`self-hosted-runner.md`](self-hosted-runner.md) |
| 為什麼決定要搬,以及量到的數字 | [`../adr/0012-ci-on-a-self-hosted-runner.md`](../adr/0012-ci-on-a-self-hosted-runner.md) |
| 這個專案到底在做什麼(不看程式碼的版本) | [`../system-overview.md`](../system-overview.md) |
| 把交易所在自己電腦上跑起來 | [`../../README.md`](../../README.md) |
