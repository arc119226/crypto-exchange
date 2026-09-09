# 把 CI 搬到自己的機器上(self-hosted runner)

> **這份文件假設你只會複製貼上指令。** 每一步都會說在哪台機器上做、要打什麼、應該看到什麼。
>
> 主線是 **Windows 11 + Docker Desktop**(第 2 節),Linux VPS 的作法在第 3 節。之後的章節兩條路線共用。
>
> 對應 `.github/workflows/ci.yml` 開頭的 "Where the jobs run" 註解。

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

而且**計費方式比想像中兇**:GitHub 是把**每一個 job 各自的秒數無條件進位到整分鐘**再加總,不是看整次 run 的牆上時間。所以 run #86 從按下按鈕到全綠是 25 分鐘,帳單記的是 **45 分鐘**——九個 job 各自進位、各自算。

有沒有可能靠優化擠進去?算一下就知道不行:一個月約 810 次 run 要塞進 3,000 分鐘,等於**每次 run 只能 3.7 分鐘**。這個專案光是 `go build -race`、一整套 testcontainers、十幾個容器的 compose stack、外加一個 kind 叢集,任何一項都不只 3.7 分鐘。把所有能做的優化做滿大概省一半,還是額度的四倍。

**結論:日常 CI 必須跑在自己的機器上。** 自己的機器跑多久都不計費,GitHub 只收「協調」的錢(等於零)。

---

## 1. 你的機器夠不夠

| 項目 | 最低 | 建議 |
|---|---|---|
| CPU | 2 核 | **4 核以上** |
| 記憶體 | 8 GB | **16 GB** |
| 空閒磁碟 | 60 GB | **100 GB** |
| 網路 | 連得到 github.com 與 ghcr.io | 同左 |

為什麼要這麼多:`helm` job 會開一個 kind 叢集(Kubernetes,node image 約 900 MB),`e2e` job 會同時起十幾個容器(Postgres、NATS、Redis、MinIO、anvil、六個 exchange 角色、Prometheus、Grafana、Jaeger),而這兩個 job 是**平行跑的**。

Windows 11 要特別注意記憶體:CI 全部跑在 WSL2 裡,而 WSL2 是一台虛擬機,**它拿走的記憶體 Windows 就用不到了**。16 GB 的機器分 10 GB 給 WSL 剛好;8 GB 的機器只能分 6 GB,`e2e` 和 `helm` 會很吃力(第 11 節有折衷方案)。

> **這台機器要當成拋棄式的。** CI 會執行分支上的任意程式碼——那是它的工作。上面不要有你的私鑰、正式環境、或其他重要服務。

---

## 2. Windows 11 + Docker Desktop

CI 的所有腳本都是 bash、Makefile、Linux 容器(`scripts/e2e.sh`、`make e2e`、`sudo apt-get`),**不可能直接跑在 Windows 上**。所以作法是:在 Windows 裡裝一個 Linux(WSL2),runner 跑在那個 Linux 裡,Docker Desktop 借給它用。

```
  Windows 11
  ├─ Docker Desktop ──────────┐   (提供 docker 引擎)
  └─ WSL2 / Ubuntu 24.04      │
     ├─ docker (借用) ◄───────┘
     └─ GitHub runner ×3  ← CI 真正跑的地方
```

### 2.1 裝 WSL2 和 Ubuntu

開 **PowerShell(以系統管理員身分執行)**:

```powershell
wsl --install -d Ubuntu-24.04
```

裝完會叫你**重開機**。重開後它會自動跳出一個 Ubuntu 視窗,要你設一個 Linux 使用者名稱和密碼——**記住這組密碼**,後面 `sudo` 要用。

檢查版本(PowerShell):

```powershell
wsl -l -v
```

要看到 `Ubuntu-24.04    Running    2`。最後那個 **2** 很重要,是 1 的話跑 `wsl --set-version Ubuntu-24.04 2`。

### 2.2 讓 WSL 裡面有 systemd

runner 要裝成開機自動啟動的服務,需要 systemd。在 **Ubuntu 視窗**裡:

```sh
sudo tee /etc/wsl.conf >/dev/null <<'EOF'
[boot]
systemd=true
EOF
```

回到 **PowerShell**,把 WSL 整個關掉再開:

```powershell
wsl --shutdown
wsl -d Ubuntu-24.04
```

在 Ubuntu 裡確認:

```sh
systemctl is-system-running        # running 或 degraded 都算成功
```

### 2.3 分配資源給 WSL

WSL2 預設會拿走一半的記憶體,而且**不會**還給 Windows。明確指定比較好。

在 **PowerShell** 建立設定檔:

```powershell
notepad "$env:USERPROFILE\.wslconfig"
```

貼上(**依你的機器改數字**):

```ini
[wsl2]
# 16 GB 的機器:給 10GB。8 GB 的機器:給 6GB。
memory=10GB
# 總核心數減 2,留給 Windows
processors=6
swap=8GB
# 記憶體用完之後把空的還給 Windows(Windows 11 才有)
sparseVhd=true
```

存檔,然後 `wsl --shutdown` 再開一次才會生效。

### 2.4 讓 Ubuntu 用得到 Docker

打開 **Docker Desktop** → 右上角齒輪 **Settings**:

1. **General** → 勾選 **Use the WSL 2 based engine**。
2. **General** → 勾選 **Start Docker Desktop when you sign in to your computer**。
3. **Resources** → **WSL Integration** → 打開 **Enable integration with additional distros**,把 **Ubuntu-24.04** 的開關打開。
4. 按 **Apply & restart**。

回到 Ubuntu 視窗驗證:

```sh
docker version
docker compose version
docker run --rm hello-world
```

三個都要有正常輸出。`docker compose version` 沒有的話,表示 Docker Desktop 的整合沒開成功,回上一步再看一次。

### 2.5 kind 需要的兩個核心參數

`helm` job 會在 WSL 裡開 Kubernetes,WSL 預設的檔案監看上限太低會失敗。在 Ubuntu 裡:

```sh
sudo tee /etc/sysctl.d/99-kind.conf >/dev/null <<'EOF'
fs.inotify.max_user_watches = 524288
fs.inotify.max_user_instances = 512
EOF
sudo sysctl --system
```

### 2.6 裝 CI 會用到的基本工具

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates curl git jq unzip make
```

（Go、Node、Helm、Foundry 不用裝,CI 每次自己下載對應版本。）

### 2.7 不要讓機器睡著

這是 Windows 路線最容易翻車的地方:**電腦睡著 = runner 離線 = CI 卡住**。

**電源設定**(PowerShell,系統管理員):

```powershell
powercfg /change standby-timeout-ac 0      # 永不進入睡眠
powercfg /change hibernate-timeout-ac 0    # 永不休眠
powercfg /change monitor-timeout-ac 10     # 螢幕 10 分鐘關掉(這個沒關係)
powercfg /h off                            # 關掉休眠檔,順便省 C 槽空間
```

**Windows Update 自動重開**:**設定** → **Windows Update** → **進階選項** → 把**使用中時數**設成你不會用它的整段時間,或直接暫停更新。重開機本身不可怕(下一步會處理自動恢復),但重開到一半的 CI 會變成紅字。

**自動登入**:Docker Desktop 是一個桌面程式,**必須有人登入 Windows 才會跑**。要讓機器重開後自己回到能跑 CI 的狀態,就得讓它自動登入:

```powershell
netplwiz
```

取消勾選「**必須輸入使用者名稱和密碼,才能使用這台電腦**」→ **套用** → 輸入密碼兩次。

> 自動登入的意思是:**任何走到這台電腦前面的人都直接看到桌面**。這就是第 1 節說「當成拋棄式機器」的原因。不要拿放著重要東西的電腦做這件事。

**開機自動啟動 WSL**:Docker Desktop 會自己起來,但 WSL 不會。按 `Win+R` 打 `shell:startup`,在跳出來的資料夾裡新增一個 `start-wsl.bat`,內容:

```bat
@echo off
wsl -d Ubuntu-24.04 -u root -e /bin/true
```

WSL 裡有 systemd(2.2 節),所以只要把它叫醒一次,runner 服務就會自己起來並一直活著。

---

## 3. Linux VPS(另一條路線)

如果你用的是 Linux 主機而不是 Windows,第 2 節整節跳過,改做這些:

```sh
sudo apt-get update
sudo apt-get install -y ca-certificates curl git jq unzip make
curl -fsSL https://get.docker.com | sudo sh
docker --version && docker compose version

# runner 不要用 root 也不要用你自己的帳號跑
sudo useradd -m -s /bin/bash ghrunner
sudo usermod -aG docker ghrunner
# Playwright 的 install --with-deps 會 apt-get 裝瀏覽器的系統相依套件
echo 'ghrunner ALL=(ALL) NOPASSWD:ALL' | sudo tee /etc/sudoers.d/ghrunner
sudo -iu ghrunner        # 之後第 4 節的指令都在這個身分下做
```

最後那行 NOPASSWD 是 GitHub 官方對 self-hosted runner 的建議(很多 `setup-*` action 會裝系統套件),前提一樣是:**這台機器是拋棄式的**。

（Windows/WSL 路線不用另開帳號:WSL 這台虛擬機本身就是那個「專用的、可以砍掉重練的環境」,直接用你 2.1 建的那個使用者。）

---

## 4. 裝 runner(重複三次)

以下在 **Ubuntu 視窗**裡(Windows 路線)或 **`ghrunner` 身分下**(Linux 路線)。

先到 GitHub 網頁上拿 token:

> `https://github.com/arc119226/crypto-exchange` → **Settings** → **Actions** → **Runners** → **New self-hosted runner** → 選 **Linux**

那一頁會顯示**當下最新的版本號**和**一組一次性 token**。版本號記下來(下面寫 `2.XXX.X` 的地方要換掉),token 一小時後失效,過期就回那一頁再按一次。

> 選 **Linux** 不是 Windows——即使你的機器是 Windows,runner 是裝在 WSL 裡的 Ubuntu 上。

```sh
mkdir -p ~/runner-1 && cd ~/runner-1
curl -o r.tar.gz -L https://github.com/actions/runner/releases/download/v2.XXX.X/actions-runner-linux-x64-2.XXX.X.tar.gz
tar xzf r.tar.gz && rm r.tar.gz
./config.sh --url https://github.com/arc119226/crypto-exchange \
  --token <貼上剛才那組 token> \
  --name box-1 --labels exchange-ci --work _work --unattended
```

`--labels exchange-ci` 就是 workflow 要找的標籤,**三個都要一樣**。

裝成自動啟動的服務(`$USER` 就是現在這個帳號):

```sh
sudo ./svc.sh install "$USER"
sudo ./svc.sh start
sudo ./svc.sh status      # 要看到 active (running)
```

**然後整段重複兩次**,換成 `~/runner-2` / `--name box-2` 和 `~/runner-3` / `--name box-3`(每次都要回網頁拿一組**新的** token)。

為什麼要三個?**一個 runner 一次只能跑一個 job。** 現在一次完整的 run 是九個 job、加起來 45 分鐘的機器時間。只裝一個,九個 job 排隊,你要等 45 分鐘才看得到結果;三個平行跑大約 15 分鐘,和現在用 GitHub 的機器差不多。

（記憶體只有 8 GB 的機器裝 **兩個**就好,三個會互相搶記憶體。）

裝完回網頁的 Runners 頁,應該看到三個綠點:`box-1` `box-2` `box-3`,標籤都是 `exchange-ci`。

---

## 5. 按下開關

`.github/workflows/ci.yml` 裡每個 job 寫的是:

```yaml
runs-on: ${{ vars.CI_RUNNER || 'ubuntu-latest' }}
```

也就是:**有設變數就用你的機器,沒設就用 GitHub 的。** 所以開關只是一個倉庫變數:

> **Settings** → **Secrets and variables** → **Actions** → **Variables** 分頁 → **New repository variable**
>
> - Name: `CI_RUNNER`
> - Value: `exchange-ci`

按下 **Add variable**,下一次 push 就會跑在你的機器上。不用改任何程式碼、不用開 PR。

**機器掛了怎麼辦:** 把這個變數**刪掉**(同一頁的垃圾桶圖示),CI 立刻回到 GitHub 的機器上跑。會開始計費,但不會卡住。修好機器再把變數加回去。

順手把 fork PR 關掉(私有倉庫本來就只有協作者能觸發,但明確關掉比較安心):

> **Settings** → **Actions** → **General** → **Fork pull request workflows**,全部不勾。

---

## 6. 例外:release job

`release` job(`.github/workflows/ci.yml` 最後一個)**故意寫死 `ubuntu-latest`**,不吃這個變數。

原因:它一個月只跑幾次(只有推 `v*` tag 時),它要把 chart 推到 ghcr、開 GitHub Release,而發布這件事不該依賴一台家裡的電腦有沒有開著。它的成本可以忽略。

---

## 7. 磁碟會滿

GitHub 的機器每個 job 跑完就整台丟掉,自己的機器不會——每次 build 的 image、每個 kind node image、每個 build cache 都留著。**不管的話幾天就滿了。**

### 7.1 job 裡的自動清理(已經寫好了)

四個會用到 Docker 的 job(`integration` `e2e` `helm` `image`)結尾都掛了一個清理步驟(`.github/actions/reclaim-disk/action.yml`),**只在 `CI_RUNNER` 有值時才跑**。它用的是**有時間過濾**的清法:

```sh
docker container prune -f --filter until=6h
docker image prune -af --filter until=72h
docker builder prune -f --filter until=72h
```

為什麼要加 `until`:三個 runner 共用**同一個** Docker 引擎。如果 `image` job 直接跑 `docker system prune -a`,它會把旁邊 `e2e` job 正在用的容器和 image 一起殺掉。加了時間過濾,只動閒置超過任何單一 job 執行時間的東西,才可以邊跑邊清。

volume 沒有清,因為 `docker volume prune` **沒有** `until` 這個過濾器,分不出死的和活的。`scripts/e2e.sh` 自己會 `compose down -v`,testcontainers 有 Ryuk 收屍,剩下的靠下一節。

### 7.2 每週深度清理

在 Ubuntu 裡排 cron(`crontab -e`,第一次會問要用哪個編輯器,選 nano):

```cron
# 每週日 04:00 停 runner、徹底清、再開
0 4 * * 0 for d in $HOME/runner-1 $HOME/runner-2 $HOME/runner-3; do sudo $d/svc.sh stop; done; docker system prune -af --volumes; for d in $HOME/runner-1 $HOME/runner-2 $HOME/runner-3; do sudo $d/svc.sh start; done
```

停掉 runner 再清,才不會清到跑到一半的 job。

### 7.3 Windows 特有:清了空間也不會回來

這是 WSL 最反直覺的地方。WSL 和 Docker Desktop 的資料放在 Windows 的一個虛擬磁碟檔(`.vhdx`)裡,**這個檔只會長大,不會自己縮小**。你在 Ubuntu 裡 `docker system prune` 清掉 30 GB,`df -h` 會顯示空間變多,但 C 槽的可用空間**一點都沒回來**。

2.3 節的 `sparseVhd=true` 就是在處理這件事(Windows 11 才支援),它會讓虛擬磁碟自動把空的部分還回去。

如果還是不夠,手動壓縮一次(**PowerShell,系統管理員**):

```powershell
wsl --shutdown
# 對現有的磁碟啟用自動歸還(只需要做一次)
wsl --manage Ubuntu-24.04 --set-sparse true
```

或者在 Docker Desktop → **Settings** → **Resources** → **Advanced** 有一顆 **Clean / Purge data**,那是核彈級的:所有 image、容器、volume 全刪,C 槽空間立刻回來,下一次 CI 會慢很多(全部重下載)。

---

## 8. 驗證有沒有成功

1. **看 runner 有沒有接到工作。** 隨便 push 一個 commit,到 Actions 頁點開任何一個 job,展開最上面的 **Set up job**,應該看到:

   ```
   Runner name: 'box-1'
   Runner group name: 'Default'
   ```

   如果看到 `Runner Image: ubuntu-24.04` 之類的,表示變數沒設成功,還在用 GitHub 的機器。

2. **看帳單有沒有停止增加。** **Settings** → **Billing** → **Actions**。自建 runner 跑的 job 在 API 上照樣有時間長度,但**不計入帳單**——要看的是 Billing 頁的實際數字,不是 job 的秒數。跑幾天後,每月用量應該從約 23,400 分鐘掉到 **50~100 分鐘**(只剩 `release`)。

3. **看磁碟。** 在 Ubuntu 裡:

   ```sh
   df -h /
   docker system df
   ```

   跑了一週後使用率應該穩在某個水位不再往上爬。一直漲就是 7.1 的清理沒生效——去 job log 裡找 `reclaim disk` 這個步驟,它會印出當下的磁碟狀況。Windows 還要另外看 C 槽(7.3 節)。

---

## 9. 機器規格不夠的時候

如果只有 2 核 / 8 GB,`e2e` 和 `helm` 會很吃力(前者十幾個容器,後者一整個 Kubernetes)。折衷做法是**只把吃計費最兇、但資源需求中等的 job 搬過去**:

| 搬到自己的機器 | 留在 GitHub |
|---|---|
| `integration`(佔帳單 25%) | `e2e`(要十幾個容器) |
| `unit`(16%) | `helm`(要 kind 叢集) |
| `lint`(15%) | `image` |
| `fuzz-smoke`(9%) | `release` |

這樣搬走 65% 的帳單。作法是把那四個 job 的 `runs-on` 改成另一個變數(例如 `vars.CI_RUNNER_LIGHT`),其餘留 `ubuntu-latest`。

但要知道這只是過渡:剩下的還是每月約 8,200 分鐘,仍然超標。真正的解法是把機器換大一點。

---

## 10. 常見問題

**Q: runner 在網頁上顯示 Offline。**
Ubuntu 裡 `sudo ~/runner-1/svc.sh status` 看服務,`journalctl -u actions.runner.* -n 50` 看 log。Windows 路線最常見的原因是**電腦睡著了**或**沒有自動登入**(2.7 節),其次是重開機後 WSL 沒起來——手動跑一次 `wsl -d Ubuntu-24.04` 就會恢復。

**Q: job 卡在 "Waiting for a runner to pick up this job"。**
標籤對不上。網頁 Runners 頁看三台的標籤是不是都是 `exchange-ci`,和變數 `CI_RUNNER` 的值一字不差。

**Q: `Cannot connect to the Docker daemon` / `permission denied ... docker.sock`。**
Docker Desktop 沒開,或 WSL Integration 沒打開(2.4 節)。Windows 工作列右下角看有沒有鯨魚圖示、是不是綠色的。Linux 路線則是 `ghrunner` 沒進 docker 群組:`sudo usermod -aG docker ghrunner` 之後重啟 runner 服務。

**Q: 電腦重開之後 CI 就不動了。**
按順序檢查:Windows 有沒有自動登入 → Docker Desktop 有沒有跟著開 → `shell:startup` 裡的 `start-wsl.bat` 在不在 → Ubuntu 裡 `sudo ~/runner-1/svc.sh status`。

**Q: C 槽滿了。**
看 7.3 節。先在 Ubuntu 裡 `docker system prune -af --volumes`(記得先停 runner),再回 PowerShell 做 `wsl --shutdown` + `--set-sparse true`。

**Q: `helm` job 在我的機器上失敗,GitHub 上卻是綠的。**
八成是 2.5 節的 inotify 參數沒設,或是記憶體不夠(2.3 節)。job 的 artifact 裡有 `helm-kind-logs`,裡面 `events.txt` 會說是哪個 pod 起不來。

**Q: 想暫時全部回到 GitHub 的機器上。**
刪掉倉庫變數 `CI_RUNNER`。一秒生效,不用改 code。

**Q: 那 timeout 呢?**
每個 job 都有 `timeout-minutes`(15~30 分鐘)。這在自己的機器上一樣重要:一個卡住的 job 會佔住三分之一的 runner 容量,直到有人發現。

**Q: 這樣做安全嗎?**
私有倉庫、單人提交,風險可控。但要記得三件事:機器上不要放別的東西、fork PR 要關掉(第 5 節)、自動登入代表實體接觸就等於登入。WSL 看得到 Windows 的檔案(`/mnt/c`),所以「拋棄式機器」這句話在 Windows 路線上一樣成立。
