# 在 Sepolia 測試網上手動走一次全流程

> **這份文件假設你什麼都不知道。** 沒碰過終端機、沒裝過 Docker、不知道什麼是私鑰,都沒關係——每一步都會說在哪裡做、要打什麼、應該看到什麼。看到不懂的名詞就往上翻第 1 節。
>
> 對應 `docs/plan-v1.0.md` §12 Phase 4d、§2.3 第 5 條。

---

## 0. 你要做的是什麼

我們寫了一個加密貨幣交易所。到目前為止它只在**一條假的區塊鏈**上跑過——那條鏈叫 anvil,跑在你自己的電腦裡,錢是假的、出塊瞬間完成、永遠不會出錯。

現在要把同一套程式接到**一條真的區塊鏈**上跑一次。這條鏈叫 **Sepolia**,是以太坊官方的測試網:規則跟真的以太坊一模一樣,但上面的幣沒有價值,可以免費領。

用個比喻:前面都是在駕訓班場地練車,現在要第一次開上真的馬路——路一樣是路,但會塞車、會有紅綠燈、會有你沒預料到的事。

**你要完成的一圈:**

```
   有人把錢匯進來                       錢被收攏到金庫
   (充值)                              (歸集)
       │                                    ▲
       ▼                                    │
   交易所記帳  ──────────────────────────────┤
       │                                    │
       ▼                                    │
   有人把錢領走 ◄───────────────────────────┘
   (提現)

   最後:對帳 —— 帳本上的數字和鏈上真實的錢,一分不差
```

最後那個「一分不差」就是通過的標準。

> **不會花到真錢。** 這裡每一個步驟都在測試網上,幣是免費領的、沒有市場價值。這是專案的鐵則(`docs/plan-v1.0.md` §0):不接主網、不碰真錢。

**大概要花多久:** 順利的話兩到三小時,其中大半在等——等 faucet 的冷卻時間、等區塊確認。第一次裝軟體可能再加一小時。

---

## 1. 名詞:五分鐘看完

先大致有印象就好,用到的時候會再解釋一次。

| 名詞 | 白話解釋 |
|---|---|
| **區塊鏈** | 一本所有人都能看、只能往後加、不能塗改的帳本。 |
| **以太坊 / ETH** | 最有名的區塊鏈之一。ETH 是它的原生貨幣。 |
| **Sepolia** | 以太坊的**測試網**。規則一樣,幣免費,沒有價值。我們全程只用它。 |
| **測試幣 / SepoliaETH** | Sepolia 上的 ETH。免費領,不能換錢。 |
| **faucet(水龍頭)** | 免費發測試幣的網站。你給它一個地址,它打幣給你。通常一天一次。 |
| **地址** | 像銀行帳號,長得像 `0x` 開頭的 42 個字。**可以公開給任何人。** |
| **私鑰** | 那個帳號的密碼,`0x` 開頭 66 個字。**握有它就等於握有裡面的錢。絕對不能給任何人、不能截圖、不能貼到聊天室。** |
| **gas(手續費)** | 在鏈上做任何事都要付的費用,用 ETH 付。轉一次帳大約 0.00005 ETH——很少,但不是零。 |
| **區塊 / 出塊** | 帳本一頁一頁往後加,每一頁叫一個區塊。Sepolia 大約 **12 秒**出一塊。 |
| **確認數** | 你的交易被寫進去之後,又疊了幾頁上去。我們要求 **6** 個確認才承認一筆充值,所以大約要等 **72 秒**。 |
| **智能合約** | 部署在鏈上的一段程式。我們要部署一個叫 `MockUSDC` 的假美元代幣。 |
| **RPC endpoint** | 一個網址,你的電腦透過它跟區塊鏈說話。像是「區塊鏈的客服電話」。 |
| **終端機 / Terminal** | 打指令的黑底白字視窗。第 2 節教你打開。 |
| **Docker** | 一個把整套軟體打包起來執行的工具。我們用它跑資料庫和交易所本體,你不用一個一個裝。 |
| **熱錢包** | 交易所自己的錢包,提現的錢從這裡出去。 |
| **充值地址** | 交易所發給每個使用者、專屬的收款地址。 |
| **歸集(sweep)** | 把散在各個充值地址的錢,收攏進熱錢包。 |
| **對帳(reconcile)** | 比對「帳本上說有多少錢」和「鏈上真的有多少錢」。 |

---

## 2. 東西在哪裡跑?

這件事最容易搞混,先講清楚:

| 東西 | 在哪裡 |
|---|---|
| 你打的每一行指令 | **你自己的筆電**,在終端機裡 |
| 資料庫、交易所程式 | **你自己的筆電**,在 Docker 裡跑(你看不到,但它在) |
| 領測試幣的 faucet | **網頁**,用瀏覽器開 |
| Sepolia 區塊鏈本身 | 網際網路上,別人的電腦。你的筆電透過 RPC endpoint 跟它講話 |

**沒有任何「遠端伺服器」要你登入。** 全部在你自己的機器上,只有兩件事會連到外面:瀏覽器開 faucet 網站,以及程式連到 RPC endpoint。

### 2.1 你的電腦需要是什麼

- **macOS** 或 **Linux**:直接可以,跳到 2.3。
- **Windows**:要先裝 **WSL2**(Windows 裡的 Linux 環境),看下面 2.2。
- 硬碟至少留 **20 GB**,記憶體 **8 GB** 以上。

### 2.2 Windows 專屬:WSL2 設定

在 **PowerShell** 執行:

```
wsl --install
```

跑完**重開機**。(已經裝過的人跳過這步。)

#### ⚠️ 這裡有一個一定會踩的坑

重開機後在 PowerShell 打:

```
wsl -l -v
```

你會看到類似這樣,而且很可能 **`docker-desktop` 被標成預設**:

```
  NAME              STATE           VERSION
* docker-desktop    Running         2
  Ubuntu            Stopped         2
```

**`docker-desktop` 不能拿來工作。** 它是 Docker Desktop 自己建來跑引擎的**內部發行版**:

- 沒有正常的套件管理,`apt` 裝不了東西
- Docker Desktop 更新或按 reset 的時候會被**整個重建**,你放在裡面的東西全部消失
- Docker 官方文件明講它是內部用的

**你要用的是 `Ubuntu`。** 沒有 Ubuntu 的話先裝:

```
wsl --install -d Ubuntu
```

然後把預設改掉:

```
wsl --set-default Ubuntu
wsl -l -v
```

確認 `*` 跑到 `Ubuntu` 那一行,而且 `VERSION` 是 `2`:

```
  NAME              STATE           VERSION
* Ubuntu            Stopped         2
  docker-desktop    Running         2
```

> 第一次開 Ubuntu 會要你設一組 **UNIX 使用者名稱和密碼**。那組密碼跟 Windows 帳號無關,是之後打 `sudo` 用的——**記起來**。

#### 讓 Docker 在 Ubuntu 裡用得到

裝好 Docker Desktop(第 3.2 節)之後,還要做這一步:

打開 Docker Desktop → 右上角齒輪 **Settings** → **Resources** → **WSL Integration** → 把 **Ubuntu** 的開關**打開** → 按 **Apply & Restart**。

不做這步的話,在 Ubuntu 裡打 `docker` 會說 `command not found`。

#### ⚠️ 第二個坑:專案不要放在 `/mnt/c/`

WSL 看得到你的 Windows 磁碟(在 `/mnt/c/`),但**跨檔案系統存取慢到不合理**——git、Go 編譯、Docker 掛載都會慢好幾倍,原本 5 分鐘的東西可能變成 30 分鐘。

所以第 4 節說的 `cd ~` 要照做,那是 Linux 自己的檔案系統(`/home/你的名字`),不要改成 Windows 的路徑。

> 想用 VS Code 編輯檔案的話:在 Ubuntu 的終端機裡打 `code .`,它會自動用 Remote-WSL 模式開,那樣才是對的。

### 2.3 打開終端機

- **macOS**:按 `Command + 空白鍵`,輸入 `Terminal`,按 Enter。
- **Ubuntu / Linux**:按 `Ctrl + Alt + T`。
- **Windows**:開始選單搜尋 `Ubuntu` 點開,或在 PowerShell 打 `wsl` 按 Enter。
  **不是 PowerShell 本身**——這份文件裡除了 2.2 那幾行 `wsl ...` 之外,所有指令都在 Ubuntu 裡打。

**Windows 使用者確認一下你進對地方了:**

```
cat /etc/os-release | head -1
```

要印出 `PRETTY_NAME="Ubuntu ..."`。印出別的東西(或這個檔案根本不存在)就是你跑進 `docker-desktop` 了,回 2.2。

你會看到一個視窗,最後一行有個游標在閃。那一行叫**提示字元(prompt)**,長得像:

```
yourname@yourlaptop ~ %
```

或

```
yourname@yourlaptop:~$
```

**怎麼用:**
- 「執行某個指令」= 把那行字貼進去,按 Enter。
- 貼上:macOS 是 `Command + V`,Linux/WSL 通常是 `Ctrl + Shift + V`(注意有 Shift)。
- 指令跑完,提示字元會再出現。**提示字元沒出現就是還在跑,等它。**
- 想中斷正在跑的東西:按 `Ctrl + C`。

**這份文件裡的規矩:**
- 灰底框裡的東西是要你貼進終端機的。
- `<像這樣的角括號>` 是**你要換掉的東西**,連角括號一起換掉。例如看到 `<你的地址>`,而你的地址是 `0xAbC...`,就整個換成 `0xAbC...`,不要留角括號。
- 有些指令會分好幾行,行尾有一個反斜線 `\`。**整段一起複製貼上**,那是同一個指令。
- **`<...>` 漏換掉不會好好報錯。** 在終端機裡 `<` 是「從檔案讀入」的意思,所以漏換的症狀是 `No such file or directory` 指著一個看起來莫名其妙的詞,而不是「你忘了換」。看到這種錯,先回去找有沒有沒換掉的角括號。
- **從哪裡複製會影響對錯。** 有些顯示方式會幫 markdown 的特殊字元加跳脫:行尾 `\` 變成 `\\`、`_` 變成 `\_`。貼進終端機前掃一眼有沒有這種多餘的反斜線。最保險是直接從 repo 裡的 `docs/runbooks/sepolia.md` 複製。

---

## 3. 安裝四樣東西

一個一個來,每裝完一個就用「驗證」那行確認。

### 3.1 git(拿專案原始碼用的)

**macOS**:在終端機執行

```
xcode-select --install
```

會跳出一個安裝視窗,按「安裝」,等它跑完(可能十幾分鐘)。這一步同時也裝好了 `make`。

**Ubuntu / WSL**:

```
sudo apt update && sudo apt install -y git build-essential curl
```

會問你密碼。Linux 打你的登入密碼;**WSL 打 2.2 設的那組 UNIX 密碼**,不是 Windows 帳號密碼。(**打的時候螢幕不會顯示任何東西,這是正常的**),按 Enter。

**驗證**:

```
git --version
```

看到類似 `git version 2.39.5` 就對了。

### 3.2 Docker Desktop(跑資料庫和交易所用的)

1. 用瀏覽器打開 **https://www.docker.com/products/docker-desktop/**
2. 下載對應你系統的版本(Mac 要注意選 Apple Silicon 還是 Intel——不確定的話,點左上角蘋果 →「關於這台 Mac」,寫 M1/M2/M3/M4 就是 Apple Silicon)。
3. 安裝,然後**打開它**。第一次會要你同意條款、可能要你註冊帳號(可以跳過)。
4. **確認它在跑**:Mac 看螢幕最上方選單列有沒有鯨魚圖示;Windows 看右下角。圖示要是穩定的,不是在轉。

> **Windows 使用者:裝完一定要回去做 2.2 的「讓 Docker 在 Ubuntu 裡用得到」**(Settings → Resources → WSL Integration → 打開 Ubuntu → Apply & Restart)。少了這步,Ubuntu 裡的 `docker` 是不存在的。
>
> 同時確認 Settings → General 的 **"Use the WSL 2 based engine"** 有打勾。

**驗證**(在終端機):

```
docker --version
docker compose version
```

兩行都要印出版本號。如果說 `command not found` 或 `Cannot connect to the Docker daemon`,回去確認 Docker Desktop 真的開著。

### 3.3 Go(編譯交易所程式用的)

> ⚠️ **Linux / WSL 不要用 `apt install golang-go`。** Ubuntu 套件庫裡的 Go 通常太舊,而這個專案要 1.26 以上。用官方 tarball。

**macOS**

1. 打開 **https://go.dev/dl/**
2. 下載最新版的 `.pkg`(Apple Silicon 選 `darwin-arm64`,Intel 選 `darwin-amd64`)。
3. 點兩下安裝,然後**把終端機關掉重開**(不然它找不到新裝的東西)。

**Linux / WSL**

先看你的 CPU 架構:

```
uname -m
```

`x86_64` 就用下面的 `amd64`(絕大多數人是這個);`aarch64` 的話把下面每個 `amd64` 都換成 `arm64`。

整段一起貼:

```
sudo apt update && sudo apt install -y curl
cd ~
GOVER=$(curl -s https://go.dev/VERSION?m=text | head -1)
echo "要裝的版本:$GOVER"
curl -LO https://go.dev/dl/${GOVER}.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf ${GOVER}.linux-amd64.tar.gz
```

> - `GOVER` 那行是去官網問「現在最新的穩定版是哪個」,免得這份文件寫死一個版本號然後過期。
> - `sudo rm -rf /usr/local/go` 是官方文件要求的:先清掉舊的再解壓,不然新舊檔案會混在一起。它只刪 Go 自己那個目錄。
> - `sudo` 問密碼時,**WSL 打的是 2.2 設的 UNIX 密碼**,不是 Windows 帳號密碼。螢幕不會有反應是正常的。

然後讓終端機找得到它:

```
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
```

第一行寫進 shell 設定檔(以後每次開終端機都生效),第二行讓現在這個視窗立刻生效。

**驗證**(兩個平台都一樣):

```
go version
```

要印出 `go version go1.27...` 之類的,**1.26 以上**就可以。印 `command not found` 的話關掉終端機重開再試。

### 3.4 確認 make 有了

```
make --version
```

沒有的話:Mac 回去做 3.1 的 `xcode-select --install`;Ubuntu 執行 `sudo apt install -y build-essential`。

---

## 4. 把專案拿到你的電腦上

**如果你已經有這個專案資料夾了,跳到 4.2。**

### 4.1 下載

```
cd ~
git clone https://github.com/arc119226/crypto-exchange.git
cd crypto-exchange
```

- `cd ~` = 切換到你的家目錄(Mac 是 `/Users/你的名字`,WSL 是 `/home/你的名字`)。
  **WSL 使用者:不要改成 `/mnt/c/...` 底下的路徑**,原因見 2.2 第二個坑。
- `git clone` = 把專案抄一份下來。
- `cd crypto-exchange` = 走進那個資料夾。

### 4.2 走進資料夾,並確認是最新的

**從現在開始,每一個指令都要在這個資料夾裡打。** 每次新開終端機,第一件事就是:

```
cd ~/crypto-exchange
```

(如果你放在別的地方,就換成你的路徑。)

然後把程式更新到最新:

```
git checkout main
git pull
```

> 這一步在 **Part B 開頭還要再做一次**,不是多餘的。Part A 只用到很早就存在的東西;Part B 用的設定檔和指令是後來才加進 repo 的,中間如果有人推了新的 commit,你這裡拉到的就不夠新。

**驗證你在對的地方**:

```
ls
```

應該看到一串資料夾名稱,包括 `cmd`、`docs`、`deploy`、`infra`、`Makefile`。看不到就是走錯資料夾了。

---

# Part A — 準備錢和合約

這一段的目標:拿到三樣東西給下一段用。最慢的是領測試幣(有冷卻時間),所以先做。

## A1. 產生交易所自己的密鑰

執行:

```
make gen-dev-secrets
```

**這一步做了什麼:**它幫交易所產生了一組全新的密鑰,包括**熱錢包**——交易所自己的錢包。這些檔案都存在 `secrets/` 資料夾裡,而那個資料夾被設定成**永遠不會上傳到 GitHub**。

跑的時候會印一堆訊息,最後一行大概是 `done — next: make up-single`。

> 如果它說 `WARNING: docker daemon not reachable`,表示 Docker Desktop 沒開。開起來再跑一次。

**拿到熱錢包地址:**

```
grep '^HOT_WALLET_ADDRESS=' .env
```

會印出類似:

```
HOT_WALLET_ADDRESS=0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC
```

**把等號後面那串 `0x...` 抄下來**,貼到記事本。這是**熱錢包地址**,等一下要用它領錢。

> **⚠️ 已經跑過這個專案的人注意:** 如果你之前就跑過 `make gen-dev-secrets`,直接用現有的就好。**不要**加 `FORCE=1` 重新產生——那會讓你之前發出去的所有充值地址失效。

## A2. 做一把新的私鑰(部署合約用)

我們需要一把**全新的、乾淨的**私鑰,專門用來把 `MockUSDC` 合約放到鏈上。

> **為什麼要新的?** 這把鑰匙等一下會被你貼進指令裡。用一把只在測試網上、除了這件事什麼都沒做過的鑰匙,就算不小心外洩也不痛不癢。**永遠不要拿有真錢的錢包私鑰做這種事。**

執行:

```
docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 wallet new
```

第一次跑會先下載工具,等一兩分鐘。跑完印出兩行:

```
Successfully created new keypair.
Address:     0x1234...
Private key: 0xabcd...
```

**兩個都抄下來**——不過真的漏抄也不要緊,A4 有一行指令可以從私鑰把地址重新算出來。

現在把私鑰存進檔案(這樣後面的指令可以自動讀,不用一直貼):

```
mkdir -p secrets
umask 077
echo '<把 Private key 那串 0x... 貼在這裡>' > secrets/sepolia-deployer.key
umask 022
```

`umask 077` 是讓這個檔案只有你自己讀得到。`secrets/` 資料夾一樣不會上傳到 GitHub。

**驗證存對了:**

```
cat secrets/sepolia-deployer.key
```

要印出你剛剛那串 `0x...`(66 個字)。

## A3. 拿到你的 RPC 網址(Alchemy)

RPC endpoint 是一個網址,你的電腦透過它跟 Sepolia 講話。你已經有 Alchemy 免費帳號了,用它——比免註冊的公開節點穩得多。

### 拿網址的步驟

1. 瀏覽器打開 **https://dashboard.alchemy.com/** 並登入。
2. 左邊選單找 **Apps**(有些版面叫 **Instances**),點 **Create new app**。
3. 填:
   - **Name**:隨便打,例如 `crypto-exchange-sepolia`
   - **Chain / Network**:選 **Ethereum**,網路選 **Ethereum Sepolia**
4. 建好之後進到那個 app,找 **API Key** 或 **Endpoints** 區塊,把 **HTTPS** 那個網址複製起來。長得像:

   ```
   https://eth-sepolia.g.alchemy.com/v2/AbCdEf123456...
   ```

> **⚠️ 網址最後那一長串是你的 API key,等於密碼。** 不要貼到聊天室、不要放進任何會上傳到 GitHub 的檔案、不要截圖給人。這份流程裡它只會存在終端機的環境變數,不會被寫進檔案。
>
> 回報結果給我的時候,**只要說「用 Alchemy」就好,不用給我網址。**

> **免費方案有一個限制會直接影響掃描:** Alchemy 免費方案的 `eth_getLogs` 一次最多只能查 **10 個區塊**,超過就回 400。`compose.sepolia.yaml` 裡的 `ETH_SCAN_BATCH_SIZE` 已經設成 10 來配合它,所以你不用做什麼。
>
> 提這件事是因為**如果你換別家 RPC**,那個上限可能不一樣(有些家不限區塊數、改限回傳筆數)。換家之後掃描一直失敗的話,先想到這個。

### 把它寫進 `.env`

`.env` 是 A1 產生的設定檔,放在專案資料夾裡,**不會上傳到 GitHub**。把網址寫進去,之後所有指令都自己讀得到,你不用每次重貼。

打開它:

```
nano .env
```

> **`nano` 怎麼用**(第一次會用到,之後還會用):
> - 用**方向鍵**移動游標(滑鼠沒用)。
> - 直接打字就是修改。
> - 貼上:macOS `Command + V`,Linux/WSL `Ctrl + Shift + V`。
> - 存檔:`Ctrl + O`,然後按 **Enter** 確認檔名。
> - 離開:`Ctrl + X`。
> - 不想存了:`Ctrl + X`,它問你要不要存時按 `N`。

用方向鍵移到檔案**最後一行的結尾**,按 Enter 換一行,加上這一行(把角括號連同裡面的字換成你的網址):

```
ETH_RPC_URL=<貼上你的 Alchemy HTTPS 網址>
```

> 等號兩邊**不要有空格**,網址**不要加引號**。

`Ctrl + O` → Enter → `Ctrl + X` 存檔離開。

**確認寫進去了:**

```
grep '^ETH_RPC_URL=' .env
```

要印出 `ETH_RPC_URL=https://eth-sepolia...`(你的網址)。

### 讓這個終端機視窗也讀得到

```
export SEPOLIA_RPC=$(sed -n 's/^ETH_RPC_URL=//p' .env)
```

這行的意思是「從 `.env` 裡把那個網址挖出來,設成 `SEPOLIA_RPC`」——所以你不用再貼一次 API key。

> **⚠️ `export` 只在「這個終端機視窗」有效。** 關掉或開新視窗就沒了,要重打上面那一行。好消息是它從 `.env` 讀,所以永遠不用重貼網址。文件後面會再提醒你。

### 驗證它通

```
docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 \
  chain-id --rpc-url "$SEPOLIA_RPC"
```

要印出 **`11155111`**。這是 Sepolia 的身分證號碼。

| 結果 | 意思 | 怎麼辦 |
|---|---|---|
| `11155111` | 對了 | 繼續 |
| 別的數字 | 你的 app 建在別條鏈上 | 回 Alchemy 確認網路選的是 Ethereum **Sepolia** |
| `401` / `Unauthorized` | 網址複製錯了或少了字 | 重新複製一次完整網址 |
| 連不上 / timeout | 網址打錯,或引號沒包好 | 檢查 `export` 那行,網址兩邊要有雙引號 |

### 備援

Alchemy 出問題時可以臨時換成 `https://1rpc.io/sepolia`(不用註冊)。

> 我實測過其他幾家(2026-09-07):`https://rpc.sepolia.org` 已經掛了(回 404);`https://sepolia.drpc.org` 免費方案不含 Sepolia;`https://ethereum-sepolia-rpc.publicnode.com` 能用,但它背後是一群機器、有些刪掉了舊資料,偶爾會回 `pruned history unavailable`。這就是為什麼有專屬帳號比較好。

## A4. 領測試幣

### 先把要收錢的兩個地址印出來

faucet 是網頁,它只要一串 `0x...`。與其往回翻,直接印:

```
echo "─── 熱錢包(A1)───"
sed -n 's/^HOT_WALLET_ADDRESS=//p' .env
echo "─── 部署者(A2)───"
docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 \
  wallet address --private-key "$(cat secrets/sepolia-deployer.key)"
```

會印出兩串 `0x` 開頭的地址。**A2 的那串是從私鑰重新算出來的**,所以就算你 A2 沒抄下來也沒關係。

> 如果它抱怨 `--private-key` 這個參數,改成把 key 直接接在後面:
> `... wallet address "$(cat secrets/sepolia-deployer.key)"`

### 誰要多少

你總共需要大約 **0.12 SepoliaETH**,分給**三個不同的地址**:

| 收款地址 | 大約要多少 | 用途 |
|---|---|---|
| 上面印出來的 **熱錢包(A1)** | 0.05 | 交易所付提現和歸集補 gas |
| 上面印出來的 **部署者(A2)** | 0.02 | A5 部署 MockUSDC 的手續費 |
| 一個等一下才會知道的地址 | 0.05 | **這一筆本身就是「充值」** |

**現在先領前兩個。** 第三個是**充值地址**,要等 Part B 的 B4 開好使用者、跟交易所要一個才會拿到——第三次領錢是 B5 的事。

> 兩個地址是分開領的:faucet 網頁上一次只填一個收款地址,所以要領兩次(或從兩家不同 faucet 各領一次,比較快,不用等冷卻)。

### 怎麼領

faucet 是網站,用**瀏覽器**打開。多數 faucet 一天只給一次,但**你可以指定任何收款地址**,所以可以從不同 faucet 分別打給不同地址。

常見的(這類服務變動很快,連不上就換一個,或直接 Google 搜尋 `sepolia faucet`):

- **Alchemy 的 faucet** — 你已經有帳號了,直接開 **https://www.alchemy.com/faucets/ethereum-sepolia**,用同一個帳號登入。
  > 它可能會要求你的**主網**錢包有一點點 ETH 才給全額。沒有的話它通常還是會給比較少的量,或者就換下面幾家。
- **Google Cloud Web3 Faucet** — 搜尋 `google cloud sepolia faucet`,用 Google 帳號登入
- **https://sepolia-faucet.pk910.de/** — 不用任何帳號。它讓你的瀏覽器算數學題換幣,**開著分頁讓它跑**就會慢慢累積,要多少就跑久一點。幾家都領不到的時候這家最可靠
- **Infura Sepolia Faucet** — 要 Infura 帳號

流程都差不多:貼上收款地址 → 過驗證(打勾、或登入)→ 按領取 → 等幾十秒。

### 確認收到了

**終端機——一次查兩個:**

```
export SEPOLIA_RPC=$(sed -n 's/^ETH_RPC_URL=//p' .env)
CAST="docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1"

HOT=$(sed -n 's/^HOT_WALLET_ADDRESS=//p' .env | tr -d '[:space:]')
DEP=$($CAST wallet address --private-key "$(cat secrets/sepolia-deployer.key)" | tr -d '[:space:]')

# 先驗格式再問餘額。壞掉的值送進 cast,回來的是「ENS 解析失敗」——一個
# 完全指不到真正原因的錯誤訊息。方括號是刻意的,讓看不見的空白顯形。
for v in "$HOT" "$DEP"; do
  [[ "$v" =~ ^0x[0-9a-fA-F]{40}$ ]] || echo "⚠ 這不是合法地址,不要往下跑: [$v]"
done

echo "熱錢包  $HOT"
echo "  餘額: $($CAST balance "$HOT" --rpc-url "$SEPOLIA_RPC" --ether) ETH"
echo "部署者  $DEP"
echo "  餘額: $($CAST balance "$DEP" --rpc-url "$SEPOLIA_RPC" --ether) ETH"
```

> **`--ether` 不能省。** 不加的話印出來的是 **wei**(以太坊的最小單位,1 ETH = 10^18 wei),`0.05 ETH` 會顯示成 `50000000000000000`。

**瀏覽器——看錢從哪來:**

```
https://sepolia.etherscan.io/address/<你的地址>
```

這是 Sepolia 的區塊瀏覽器。**Transactions** 分頁列出每一筆進帳:誰送的、多少、什麼時候,以及 **Transaction Hash**。

**這一頁等一下還會用到**:第 5 節的結果表要填 tx hash,而 faucet 網站通常不會給你——只能從這裡抄。

### 領到多少才夠

| 地址 | 建議 | **實際最低** | 花在哪 |
|---|---|---|---|
| 部署者 | 0.02 | **0.005** | 部署合約約 0.0012(60 萬 gas × ~2 gwei),加兩筆 mint |
| 熱錢包 | 0.05 | **0.015** | 兩筆提現的金額本身(0.003 + 0.008),加提現與補 gas 的手續費 |

faucet 給得比建議少不用緊張,**到最低那一欄就能走完全程**。低於它再去多領一家。

| 你看到 | 意思 |
|---|---|
| `0.000000000000000000` | 還沒到。通常幾十秒到幾分鐘,等一下再查 |
| 比 faucet 標的少 | 正常,有些 faucet 標的是上限 |
| Etherscan 上有好幾筆進帳 | 正常,你從不同 faucet 領的 |
| 超過十分鐘還是 0 | 地址可能貼錯(對一下有沒有少字元),或那家 faucet 沒真的送出。換一家 |

**兩個地址都到最低需求了再繼續。**

## A5. 部署 MockUSDC 合約

`MockUSDC` 是一個假的美元代幣,交易所拿它當「另一種可以交易的資產」。

確認你在專案資料夾裡,然後執行:

```
docker run --rm -v "$PWD/infra/contracts:/w" -w /w \
  --entrypoint forge \
  ghcr.io/foundry-rs/foundry:v1.8.1 \
  create src/MockUSDC.sol:MockUSDC \
    --rpc-url "$SEPOLIA_RPC" \
    --private-key "$(cat secrets/sepolia-deployer.key)" \
    --broadcast
```

> **`--entrypoint forge` 不能省,而且後面只寫 `create` 不寫 `forge create`。** 這個 image 的預設進入點是 `/bin/sh -c`,它只把第一個參數當指令執行、其餘丟給 `$0`、`$1`……所以不指定 entrypoint 的話,實際跑到的是沒有參數的 `forge`,結果是印一頁說明而不是部署。

第一次會下載編譯器,等一兩分鐘。成功的話印出:

```
Deployer: 0x...
Deployed to: 0x5FbD...        ← 這個是「合約地址」,抄下來!
Transaction hash: 0xabc...    ← 這個也抄下來
```

> `$(cat secrets/sepolia-deployer.key)` 是「把那個檔案的內容放在這裡」的意思——由你的終端機處理,私鑰不會被送進 Docker 容器。但它**會留在終端機的歷史紀錄裡**。這是測試網的鑰匙所以還好;真錢的鑰匙絕對不能這樣用。
>
> 跑完 `infra/contracts/` 底下會多出 `out/` 和 `cache/` 兩個資料夾。它們已經被設定成不會上傳,刪不刪都行。

**失敗了怎麼辦:**

| 訊息裡有 | 意思 | 怎麼辦 |
|---|---|---|
| `insufficient funds` | 部署者沒錢 | 回 A4 領錢給 A2 的 Address |
| `nonce is not 0` | 這把鑰匙之前用過 | 回 A2 做一把全新的 |
| 連不上 / timeout | RPC 有問題 | 換一個 endpoint,重做 A3 |

### 查出部署在第幾個區塊

先把 tx hash 存成變數。**要換的只有這一行**,而且很短:

```
TX=0x貼上forge印的TransactionHash
```

（把 `0x...` 整串換掉,不要留角括號、不要留中文。)

```
docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 \
  receipt "$TX" blockNumber --rpc-url "$SEPOLIA_RPC"
```

印出一個數字,例如 `11651234`。**抄下來**,這是「部署區塊高度」。

順便讓鏈自己告訴你合約地址,不用相信抄寫:

```
docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 \
  receipt "$TX" contractAddress --rpc-url "$SEPOLIA_RPC"
```

應該跟 `forge` 印的 `Deployed to:` 一模一樣。存成變數,A6 要用:

```
USDC=0x上面印出來的合約地址
```

## A6. 鑄一些 USDC 給熱錢包

`MockUSDC` 誰都可以鑄(這是測試用合約,故意這樣設計的)。

兩個值先就位。合約地址是 A5 印出來的那個(換過終端機視窗的話變數就沒了,所以這裡重貼一次);熱錢包直接從 `.env` 讀,不用抄:

```
USDC=0x貼上A5的合約地址
HOT=$(sed -n 's/^HOT_WALLET_ADDRESS=//p' .env | tr -d '[:space:]')

docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 \
  send "$USDC" "mint(address,uint256)" "$HOT" 1000000000000 \
  --rpc-url "$SEPOLIA_RPC" \
  --private-key "$(cat secrets/sepolia-deployer.key)"
```

`1000000000000` 是 1,000,000 USDC(這個代幣用 6 位小數,所以 100 萬要寫成 100 萬乘以 100 萬)。

**驗證:**

```
docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 \
  call "$USDC" "balanceOf(address)(uint256)" "$HOT" \
  --rpc-url "$SEPOLIA_RPC"
```

要印出 `1000000000000`(可能後面跟著 `[1e12]`)。

## A7. 檢查點:你現在應該有這些

抄在記事本上,Part B 全部會用到:

| 項目 | 你的值 |
|---|---|
| 熱錢包地址(A1) | `0x` |
| 部署者地址(A2) | `0x` |
| 部署者私鑰(A2) | 已存在 `secrets/sepolia-deployer.key` |
| RPC endpoint(A3) | |
| **MockUSDC 合約地址(A5)** | `0x` |
| **部署區塊高度(A5)** | |

### 順便記下這兩筆的實際成本

**這不是附註,是 4d 要交付的東西本身**(`docs/plan-v1.0.md` §12:「Sepolia runbook 含 tx hash 記錄」)。

整個 Sepolia 這一輪存在的理由,就是量出「真的鏈上要花多少錢、要等多久」。anvil 上 gas 幾乎是零、出塊瞬間完成,所以**這些數字只有這一次實跑拿得到**。

**跑兩次**,一次填一列——先 A5 部署那筆,再 A6 鑄幣那筆:

```
TX=0x貼上要查的那一筆的hash
```

```
docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 \
  receipt "$TX" --rpc-url "$SEPOLIA_RPC"
```

印出一整份收據,裡面找這幾行:

```
blockNumber         11651234
effectiveGasPrice   2000000000
gasUsed             612345
status              1 (success)
transactionHash     0x...
```

`status` 是 `1` 就代表成功。

**每一欄是什麼:**

| 欄位 | 是什麼 |
|---|---|
| tx hash | 那筆交易的編號。`forge` 印過,Etherscan 上也有 |
| gas used | 用掉多少**運算量**——是單位,不是錢。部署合約約 60 萬,單純轉帳固定 21000 |
| effective gas price | 每單位 gas **實際**付了多少,單位是 wei |
| 成本 (ETH) | gas used × effective gas price,換算成 ETH。這才是真的花掉的錢 |
| 送出 → 上鏈(秒) | 你按 Enter 到它進區塊,中間等了多久 |

**兩件你不用做的事:**

- **成本那一欄不用自己算。** 回報 `gasUsed` 和 `effectiveGasPrice` 兩個數字就好,算是推導出來的,不是觀測到的——少一個步驟就少一個出錯的地方。
- **時間不用精確。** 「大概十幾秒」「大概一分鐘」這種程度就夠了,我們要的是量級,不是碼表。

| 步驟 | tx hash | gas used | effective gas price | 成本 (ETH) | 送出 → 上鏈(秒) |
|---|---|---|---|---|---|
| 部署 MockUSDC | | | | | |
| 鑄 USDC | | | | | |

---

# Part B — 把交易所接上去,走完一圈

> **每次新開終端機視窗,先貼這四行**(全部從 `.env` 自己讀,不用你貼任何密碼):
> ```
> cd ~/crypto-exchange
> export SEPOLIA_RPC=$(sed -n 's/^ETH_RPC_URL=//p' .env)
> export EXCHANGE_ADMIN_URL=http://localhost:8082
> export EXCHANGE_ADMIN_API_KEY=$(sed -n 's/^ADMIN_API_KEY=//p' .env)
> ```
> 忘了貼會看到 `set ETH_RPC_URL...` 或 `connection refused` 之類的錯誤。

## B0. 先確認你的 checkout 夠新

**Part B 用到的檔案有一部分是後來才加進 repo 的。** 先拉最新的,再確認它們真的到你機器上了:

```
cd ~/crypto-exchange
git checkout main && git pull
ls deploy/compose/sepolia/ deploy/seed-params/ deploy/compose/compose.sepolia.yaml
```

要看到這些(順序可能不同,內容要一樣):

```
deploy/compose/compose.sepolia.yaml

deploy/compose/sepolia/:
README.md
seed-params.json.example
sepolia-addresses.json.example

deploy/seed-params/:
README.md
anvil.json
sepolia.json
```

**少任何一個,就先不要往下走**,再 `git pull` 一次。還是少的話停在這裡跟我說。

拉完再跑一次這個:

```
make gen-dev-secrets
```

**它不會覆蓋你的 `.env`** —— 你填的 RPC 網址、產生的密碼都留著。它做的是把 `.env` 裡跟著程式一起改過名的設定補上。`git pull` 只更新程式,不會動你機器上的 `.env`,所以這一步要自己跑。

> 「第 4.2 節不是拉過了嗎?」拉過,但那是 Part A 開始之前。Part A 只用到 `make gen-dev-secrets` 和 foundry 的 image,那些很早就在了;Part B 用的 `compose.sepolia.yaml`、`make up-sepolia`、`deploy/seed-params/`、資料庫的 migration 是後來才進來的。中間 repo 有更新的話,你 Part A 開始時拉的那份就不夠。
>
> 這也是為什麼這一節不寫死某個版本號:**能驗證的是「這幾個路徑存在」,不是「你在第幾個 commit」。**

## B1. 寫兩個設定檔

這一節要用的兩個範本檔(`deploy/compose/sepolia/` 底下那兩個 `.example`)是 B0 確認過的東西。**B0 沒過就不要往下**——下面第一個指令就會失敗。

先複製範本:

```
cp deploy/compose/sepolia/sepolia-addresses.json.example deploy/compose/sepolia/sepolia-addresses.json
cp deploy/compose/sepolia/seed-params.json.example       deploy/compose/sepolia/seed-params.json
```

用任何文字編輯器打開 `deploy/compose/sepolia/sepolia-addresses.json`。不知道用什麼的話:

```
open deploy/compose/sepolia/sepolia-addresses.json          # macOS
nano deploy/compose/sepolia/sepolia-addresses.json          # Linux / WSL
```

> `nano` 的用法在 A3 講過了:方向鍵移動、直接打字、`Ctrl + O` → Enter 存檔、`Ctrl + X` 離開。

把 A7 的值填進去,**`chainId` 保持 `11155111` 不要動**:

```json
{
  "chainId": 11155111,
  "deployer": "<A2 的部署者地址>",
  "hotWallet": "<A1 的熱錢包地址>",
  "usdc": "<A5 的 MockUSDC 合約地址>",
  "deployedAtBlock": <A5 的部署區塊高度,不用引號>
}
```

> 注意 `deployedAtBlock` 是數字,**不加引號**;其他三個是文字,**要加引號**。逗號不要漏也不要多。

另一個檔 `seed-params.json` **不用改**,它已經是配合 faucet 金額調過的門檻(歸集門檻 0.01 ETH、最小提現 0.002、自動核可上限 0.005)。想知道為什麼是這些數字,看 `deploy/seed-params/README.md`。

## B2. 啟動

再打開一次 `.env`:

```
nano .env
```

加一行(算法:A5 的部署區塊高度**減 10**):

```
ETH_SCAN_START_BLOCK=<部署區塊高度減 10>
```

例:部署區塊是 `11651234`,就寫 `ETH_SCAN_START_BLOCK=11651224`。

`Ctrl + O` → Enter → `Ctrl + X`。確認一下:

```
grep -E 'ETH_RPC_URL|ETH_SCAN_START_BLOCK' .env
```

兩行都要在。然後啟動:

```
make up-sepolia
```

> **為什麼要減 10、為什麼一定要設?**
>
> 交易所要從某個區塊開始往後掃描,找有沒有人匯錢進來。不設的話它會**從第 0 塊開始掃**——而 Sepolia 現在已經超過 1,165 萬塊,那要掃好幾天。設成部署區塊往前一點點,是留一點餘裕。
>
> **這個數字設定之後不能再改。** 它同時被拿來當「身分標記」:交易所會記住那個區塊的指紋,每次啟動重新核對一次,確保自己還在跟同一條鏈講話。改了會被拒絕啟動(錯誤訊息會告訴你原本記的是多少)。

`make up-sepolia` 第一次要幾分鐘(要編譯程式、下載映像檔)。它結束時不會有什麼特別的訊息,提示字元回來就是好了。

**這是一套獨立的環境**,有自己的資料庫。你之前用 anvil 跑的那套完全不受影響。

### 看它有沒有正常起來

```
make logs-sepolia FOLLOW=1
```

畫面會一直滾。**這是正常的,它在持續印記錄。看夠了按 `Ctrl + C` 離開**(這只會關掉看記錄的畫面,不會關掉交易所)。

要找的是這一行:

```
chain recorded  chain_id=11155111  anchor_block=...  anchor_hash=0x...
```

看到它就表示交易所成功連上 Sepolia 並認明了這條鏈。

**沒看到的話**,往上翻找紅色的 `error`,對照第 6 節的表。

### 確認交易所回應得了

(如果你還沒貼 Part B 開頭那四行,現在貼。)

```
go run ./cmd/exchangectl markets list
```

第一次 `go run` 要編譯,等一兩分鐘。應該印出一個表格,裡面有 `ETH-USDC`。

```
go run ./cmd/exchangectl assets list
```

確認 ETH 和 USDC 的 **`CONFIRMATIONS` 欄是 6**。

## B3. 把 faucet 給熱錢包的錢記進帳本

**這一步不能跳過,而且理由值得懂。**

交易所的帳本不知道 faucet 給了熱錢包錢——鏈上有,帳本上沒有。如果不處理,等一下對帳一定會報一筆差異,而那筆差異是**完全正確的**:帳本確實不知道那些錢從哪來。

真正的交易所遇到這種事(老闆從冷錢包轉錢進來、或收到補助),做法就是這個:記一筆「這筆錢從系統外面進來」。

金額直接問鏈,不要憑印象打——記錯的話對帳不會歸零:

```
HOT=$(sed -n 's/^HOT_WALLET_ADDRESS=//p' .env | tr -d '[:space:]')
AMOUNT=$(docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 \
  balance "$HOT" --rpc-url "$SEPOLIA_RPC" --ether | tr -d '[:space:]')
echo "熱錢包目前有 $AMOUNT ETH,要記這個數字"
```

```
go run ./cmd/exchangectl admin house-adjust \
  --code custody_hot --asset ETH \
  --amount "$AMOUNT" \
  --direction credit \
  --reason "sepolia faucet 注資熱錢包 $(date +%F)" \
  --idempotency-key "sepolia-hot-eth-1"
```

> 有 faucet 那筆的 tx hash 的話,把它加進 `--reason` 更好(從 Etherscan 抄)。這條會進稽核紀錄,是給半年後的人看的。

```
go run ./cmd/exchangectl admin house-adjust \
  --code custody_hot --asset USDC --amount 1000000 --direction credit \
  --reason "A6 鑄給熱錢包 $(date +%F)" \
  --idempotency-key "sepolia-hot-usdc-1"
```

> - `--amount` 要跟鏈上**實際的數量一致**。不確定的話用 A4 那個查餘額的指令看一次。
> - `--reason` 是給半年後的人看的,寫清楚。這條會被寫進稽核紀錄。
> - `--idempotency-key` 讓你重打同一個指令也不會記兩次。

**確認回到零:**

```
go run ./cmd/exchangectl admin reconcile
```

看到類似:

```
ASSET  LEDGER   CHAIN    UNCREDITED  ABOVE  IN FLIGHT  DIFF
ETH    0.05     0.05     0           0      0          0     ok
USDC   1000000  1000000  0           0      0          0     ok
```

**每一列最後都是 `0` / `ok`** 就對了。

> 對帳每 5 分鐘跑一次,所以剛記完可能還是舊資料,等一下再看。不是 0 的話**先不要再記任何帳**,看第 6 節。

## B4. 開一個使用者,拿一個充值地址

```
go run ./cmd/exchangectl user register --email alice@sepolia.test --password 'correct horse battery'
```

它會印出一行 `export EXCHANGE_TOKEN=...`。**把那一整行複製,貼回終端機執行。**(這是那個使用者的登入憑證。)

然後:

```
go run ./cmd/exchangectl deposit-address --asset ETH
```

印出一個 `0x...` 地址。**抄下來——這就是 A4 表格裡的第三個地址。**

## B5. 充值:讓 faucet 直接打進那個地址

回瀏覽器,找一個 faucet,收款地址填 **B4 拿到的充值地址**,領大約 **0.05 SepoliaETH**。

> **為什麼可以這樣?** 充值的定義是「錢從交易所外面進來」。faucet 就是外面。所以你不需要另外準備一個有錢的錢包來模擬「別人匯錢給你」——faucet 本身就扮演那個角色。
>
> (順帶一提:這個交易所有一條規則是「交易所自己送給自己的錢不算充值」。這條規則是上一輪對帳抓到的真實 bug 才補上的。)

順便也給它一些 USDC:

```
USDC=$(sed -n 's/.*"usdc": *"\([^"]*\)".*/\1/p' deploy/compose/sepolia/sepolia-addresses.json)
DEPOSIT=0x貼上上面拿到的充值地址

docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 \
  send "$USDC" "mint(address,uint256)" "$DEPOSIT" 250500000 \
  --rpc-url "$SEPOLIA_RPC" \
  --private-key "$(cat secrets/sepolia-deployer.key)"
```

`250500000` = 250.5 USDC。

### 看它跑

```
go run ./cmd/exchangectl deposits list
```

狀態會這樣變:

```
detected  →  confirming  →  credited
(看到了)     (等確認中)      (入帳了)
```

**大約要 72 秒以上**(6 個確認 × 12 秒)。每隔一會兒重打一次上面的指令看變化。

到 `credited` 之後:

```
go run ./cmd/exchangectl balances
```

會看到那個使用者的餘額。**錢進來了。**

### 歸集會自己發生

充值入帳之後,交易所會自動把錢從充值地址收攏進熱錢包。大約兩分鐘後:

```
go run ./cmd/exchangectl admin sweeps list
```

狀態跑到 `confirmed` 就完成了。

> ETH 是一筆交易;USDC 是兩筆——因為那個充值地址只有 USDC、沒有 ETH 可以付手續費,所以熱錢包要先送一點 ETH 過去給它付油錢,再叫它轉帳。這就是記錄裡會看到 `deposit address funded` 的原因。

## B6. 提現,兩條路都走一次

交易所對提現有兩種處理:小額自動放行,大額要人工審核。**兩種都要走過。**

### 小額(自動)

提現要有個收款地址。用 A2 的部署者地址就行——那是你自己的:

```
TO=$(docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 \
  wallet address --private-key "$(cat secrets/sepolia-deployer.key)" | tr -d '[:space:]')
echo "提現會送到 $TO"
```

```
go run ./cmd/exchangectl withdrawals create --asset ETH --amount 0.003 --to "$TO"
```

```
go run ./cmd/exchangectl withdrawals list
```

會自己一路跑到 `confirmed`。

### 大額(人工審核)

```
go run ./cmd/exchangectl withdrawals create --asset ETH --amount 0.008 --to "$TO"
```

這筆會停在等待審核。用管理員身分看:

```
go run ./cmd/exchangectl admin withdrawals list
```

把它的 `id` 存起來再核准:

```
WID=貼上上面那筆的id
```

```
go run ./cmd/exchangectl admin withdrawals review "$WID" approve --note "sepolia 手動驗證"
```

再看:

```
go run ./cmd/exchangectl withdrawals list
```

一樣會跑到 `confirmed`。

> 卡住超過幾分鐘的話,看第 6 節,或 `docs/runbooks/stuck-withdrawal.md`。

## B7. 對帳:最後的判定

```
go run ./cmd/exchangectl admin reconcile
go run ./cmd/exchangectl admin trial-balance
```

**`reconcile` 每一列的 `DIFF` 都是 `0`**,就通過了。這就是 `docs/plan-v1.0.md` §2.3 第 5 條的標準。

不是 0 的話:**先不要記任何帳**。`docs/runbooks/reconciliation-break.md` 的第一句就是「在知道錢在哪裡之前不要動任何東西」。把 `reconcile` 的輸出貼給我。

## B8. 收工

```
make down-sepolia
```

資料會留著,之後還能再開起來看。

---

## 5. 把結果填回來

**跑這個,把輸出整份貼給我:**

```
ALICE_PASSWORD='correct horse battery' scripts/sepolia-results.sh
```

它會把交易所記錄的每一筆(充值、歸集、提現)的 tx hash 撈出來,對每一筆查 receipt,算好成本和時間,直接輸出兩張填好的表格 —— 同時印在畫面上,也存成 `sepolia-results.md`。

> **為什麼要帶 `ALICE_PASSWORD`:** 充值和提現的紀錄要用 alice 的身分才讀得到,而登入權杖只活 15 分鐘 —— 你走到這裡一定早就過期了。腳本會自己重新登入。
>
> 不帶也能跑,只是充值和提現那幾列會是空的,而且腳本會告訴你為什麼。

### 有兩件它撈不到

**一、熱錢包的 faucet 注資。** 那筆交易發生在交易所外面,系統從來沒看過它。去 [Etherscan](https://sepolia.etherscan.io/) 查熱錢包地址的收款紀錄,拿到 hash 之後重跑一次:

```
FAUCET_TX=0x那筆的hash ALICE_PASSWORD='correct horse battery' scripts/sepolia-results.sh
```

**二、「有沒有哪一步的說明看不懂 / 跟實際不一樣」。** 只有走過的人知道 —— **這一列最重要**,這份文件寫得對不對,只有你能回答。腳本會把它留空並標記出來,請你自己補上。

### 腳本壞掉的話

手動查一筆的方法還在:

```
TX=0x那筆交易的hash
docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 \
  receipt "$TX" --rpc-url "$SEPOLIA_RPC"
```

`gasUsed` × `effectiveGasPrice` ÷ 10^18 就是那筆的成本(ETH)。欄位的意思在 [A7](#順便記下這兩筆的實際成本) 說明過。

要撈的東西是:充值 ×2、歸集 ×3(ETH、USDC 補 gas、USDC 轉帳)、提現 ×2,加上 faucet 注資那筆,總共八列;另外是對帳一輪的耗時、gas 價格區間、有沒有被限流、有沒有 reorg。

### 上一次實跑的數字,給你對照

2026-09-07 走完一次的結果。**你的數字不會一模一樣**(gas 價格會浮動),但差一個數量級就值得停下來看看:

| | 上次實測 |
|---|---|
| 充值上鏈 → 帳戶看得到錢 | **61 秒** |
| 原生(ETH)歸集全程 | 240 秒 |
| 代幣(USDC)歸集全程 | 479 秒 —— 要補 gas、兩筆交易各等 6 個確認 |
| 提現(自動核可)全程 | 112 秒 |
| 對帳一輪 | 3 秒 |
| gas 價格 | 1.0 – 1.1 gwei |
| **整趟七筆交易總成本** | **0.000237 ETH** |

最後一個數字值得記住:**faucet 給的 1.733 ETH 夠你跑上萬次。** 卡住你的永遠是 faucet 的冷卻時間,不是錢。

一個容易誤會的地方:代幣歸集看起來比原生慢一倍,那不是壞掉 —— 它是**兩筆交易**(先補 gas、再轉帳),每筆各等 6 個確認,中間還隔著歸集的輪詢週期。


---

## 6. 出事了怎麼辦

### 6.1 通用招數

**看交易所在講什麼:**

```
make logs-sepolia
```

預設印 `exchange-all` 最後 100 行。要看別的容器或看更多:

```
make logs-sepolia SERVICE=seed          # 換一個容器
make logs-sepolia TAIL=300              # 印多一點
make logs-sepolia FOLLOW=1              # 一直看下去,Ctrl + C 離開
```

**看有哪些東西在跑:**

```
make ps-sepolia
```

> 這兩個以前是很長的 `docker compose ...` 指令,現在收成 make 目標了。原因不是嫌長:那串指令**少了 `--profile`**,而少了它的兩種失敗都不會告訴你少了什麼 —— 看記錄會回 `no such service: nats`,`ps` 則是印一張空表,讓你以為什麼都沒在跑。

**全部重來(會清掉這套 Sepolia 環境的資料,但不影響 anvil 那套):**

```
make down-sepolia
docker volume ls -q | grep '^crypto-exchange-sepolia' | xargs -r docker volume rm
```

然後從 B2 重做。

### 6.2 症狀對照表

| 你看到 | 意思 | 怎麼辦 |
|---|---|---|
| `error: unrecognized subcommand '\'` | 貼進來的行尾是 `\\` 而不是 `\`,多半是從轉義過的顯示版本複製的 | 刪掉多餘的反斜線,順便檢查有沒有 `\_` |
| `-bash: <某個詞>: No such file or directory`,而那個詞來自指令裡的中文說明 | 角括號佔位符沒換掉,`<` 被當成輸入重新導向 | 找到那個 `<...>`,連角括號一起換成真正的值 |
| 指令印出 forge 或 cast 的**說明頁**,什麼都沒做 | `docker run` 少了 `--entrypoint`。這個 image 的進入點是 `/bin/sh -c`,只執行第一個參數 | 加 `--entrypoint forge`(或 `cast`),並把子指令後面那個重複的工具名拿掉 |
| `Failed to resolve ENS name to an address` | 傳給 `cast` 的不是合法地址——多半是從 `.env` 取值時連註解行一起抓到了 | 用 `echo "[$HOT]"` 看它實際是什麼。取值要用 `sed -n 's/^KEY=//p'`(錨定行首);`grep KEY` 會連提到那個名字的註解一起抓 |
| `command not found: docker` / `make` / `go` | 沒裝好,或終端機沒重開 | 回第 3 節;裝完要**關掉終端機重開** |
| `go version` 印出 1.26 以下 | 你用 `apt install golang-go` 裝的,那個版本太舊 | `sudo apt remove -y golang-go`,再照 3.3 用官方 tarball 裝一次 |
| Windows:Ubuntu 裡 `docker` 找不到,但 Docker Desktop 明明開著 | WSL Integration 沒打開 | Docker Desktop → Settings → Resources → WSL Integration → 打開 Ubuntu → Apply & Restart(見 2.2) |
| Windows:`apt` 裝不了東西、家目錄怪怪的 | 你在 `docker-desktop` 那個發行版裡,不是 Ubuntu | `exit` 離開,在 PowerShell 打 `wsl --set-default Ubuntu`,重開(見 2.2) |
| Windows:每個指令都慢得誇張 | 專案放在 `/mnt/c/` 底下 | 搬到 `~`:`cp -r /mnt/c/.../crypto-exchange ~/` 再從那裡跑(見 2.2) |
| `Cannot connect to the Docker daemon` | Docker Desktop 沒開 | 開它,等鯨魚圖示穩定 |
| `set ETH_RPC_URL to a Sepolia endpoint` | `.env` 裡沒有那一行 | 回 A3,確認 `grep '^ETH_RPC_URL=' .env` 印得出來 |
| `set ETH_SCAN_START_BLOCK...` | `.env` 裡沒有那一行 | 回 B2 加上去 |
| `connection refused` / 空白的表格 | 忘了貼 Part B 開頭那四行,或交易所沒起來 | 先貼那四行;還是不行看 6.1 的記錄 |
| `no such file or directory` | 你不在專案資料夾 | `cd ~/crypto-exchange`,再 `ls` 確認 |
| `cp: cannot stat '...json.example': No such file or directory` | 你在對的資料夾,但 checkout 比這份文件舊,那些檔案還沒進到你的機器 | 回 B0:`git checkout main && git pull`,再用 B0 那行 `ls` 確認三個路徑都在 |
| `WARN[0000] The "CONTRACT_DEPLOYER_KEY" variable is not set` | 你的 `.env` 比程式舊,裡面還是舊名字 `ANVIL_DEPLOYER_KEY` | **Sepolia 這條路不受影響,可以繼續**(讀這個變數的服務在 Sepolia 上是關掉的)。但跑一次 `make gen-dev-secrets` 補上,不然之後回去跑 `make up-single` 會壞 |
| `no such service: nats` | 你打的 `docker compose ... logs` 少了 `--profile`。指定服務名稱只會啟用那個服務,不會啟用它依賴的 `nats` | 改用 `make logs-sepolia`(見 6.1),它把 profile 都帶好了 |
| `ps` 印出空的表,但交易所明明在跑 | 同上,少了 `--profile`,compose 解出來的是一個空的服務清單 | 改用 `make ps-sepolia` |
| 記錄裡一直重複 `deposit scan failed` + `Under the Free tier plan, you can make eth_getLogs requests with up to a 10 block range` | 你的 RPC 免費方案限制一次只能查 10 個區塊,而掃描器一次要 200 個。**每一輪都失敗,所以就緒檢查永遠不會綠** | 這在 4d-2 之後已經預設修好(`ETH_SCAN_BATCH_SIZE: "10"`)。還會發生就是你的 checkout 太舊,回 B0 拉最新的,然後 `make down-sepolia` 再 `make up-sepolia` |
| `container crypto-exchange-sepolia-exchange-all-1 is unhealthy` + `make: *** [up-sepolia] Error 1` | 交易所的容器起來了,但 80 秒內沒能就緒。`migrate` 和 `seed` 有 Exited 就代表那兩步是成功的 —— 問題在交易所自己,多半卡在連鏈 | 跑 `make logs-sepolia`。找 `chain rpc` 開頭的重試訊息(RPC 連不上或太慢)或 `different chain`(設定不對)。**把輸出貼給我** |
| 記錄裡有 `pruned history unavailable` | 你的 RPC 背後某台機器刪掉了舊資料 | 換一個 RPC(A3),`make down-sepolia` 後重做 B2 |
| `the node is on a different chain than the cursor` | 資料庫記的鏈跟你現在連的不是同一條,或 `ETH_SCAN_START_BLOCK` 被改過 | **這是保護不是故障。** 錯誤訊息會告訴你原本記的值,設回去。真的要換鏈就照 6.1 全部重來 |
| 任何指令回 `401` / `invalid or expired access token`,但你明明登入過 | 存取權杖只活 15 分鐘(`AUTH_ACCESS_TTL`),過期了就無效 | 重跑一次 B4 的 `user login`(同一組 email / 密碼),把它印的那行 `export EXCHANGE_TOKEN=...` 貼回終端機。**不用先 `unset`** —— 4d-2 起 `user register` / `login` / `logout` 不再把舊憑證附上去 |
| 充值一直停在 `detected` 超過五分鐘 | 掃描器落後,或確認數還不夠 | 先等到兩分鐘以上。還是不動就看記錄,可能是 RPC 被限流 |
| `admin reconcile` 回 404 或資料很舊 | 對帳還沒跑過第一輪 | 等 5 分鐘。還是沒有就看記錄找 `reconciliation pass failed` |
| `DIFF` 不是 0 | 帳本和鏈上對不上 | **先不要動任何東西。** 把整段輸出貼給我,或看 `docs/runbooks/reconciliation-break.md` |
| 提現卡在 `broadcast` 超過 10 分鐘 | 手續費不夠,或網路壅塞 | 看 `docs/runbooks/stuck-withdrawal.md`。系統會自己重送(每 3 分鐘一次,最多 3 次) |
| 記錄裡有 `waiting for the fee market` | gas 價格超過我們設的上限(50 gwei) | **這是設計行為**,不是故障。等價格下來,或把 `compose.sepolia.yaml` 裡的 `ETH_MAX_FEE_PER_GAS` 調高 |
| faucet 說「已經領過了」 | 冷卻時間 | 換一家 faucet。`pk910` 那種掛著跑就會累積 |
| 領不到任何測試幣 | 幾家都掛了 | 停在這裡跟我說,4d 可以延後,不擋後面的工作 |

### 6.3 什麼時候該停下來問我

- 對帳的 `DIFF` 不是 0(**最重要的一個**)
- 出現任何你在上面表格找不到的錯誤
- 有一步的說明跟你看到的畫面對不上

停下來比亂試好。把你打的指令和完整的輸出一起貼給我。

---

## 7. Sepolia 跟 anvil 差在哪(參考)

| | anvil(之前) | Sepolia(現在) |
|---|---|---|
| 出塊 | 2 秒,準時 | 約 12 秒,會不準 |
| 確認數 | 1 | 6(所以要等 72 秒以上) |
| 手續費 | 幾乎是 0 | 真的要付,約 1 gwei |
| reorg(帳本被改寫) | 只有測試故意造的 | 真的會發生,通常 1–2 塊 |
| 歷史資料 | 全部都在 | 公開節點會刪舊資料 |
| 錢 | 想要多少有多少 | faucet,有冷卻時間 |
| 掃描起點 | 0(從頭) | 部署區塊(**一定要設**) |

---

## 附錄:給比較熟的人的快速版

```sh
# A
make gen-dev-secrets && grep '^HOT_WALLET_ADDRESS=' .env
docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 wallet new
export SEPOLIA_RPC="https://eth-sepolia.g.alchemy.com/v2/<your key>" DEPLOY_BLOCK=<from cast receipt>
# faucet -> deployer, hot wallet
docker run --rm -v "$PWD/infra/contracts:/w" -w /w --entrypoint forge \
  ghcr.io/foundry-rs/foundry:v1.8.1 \
  create src/MockUSDC.sol:MockUSDC --rpc-url "$SEPOLIA_RPC" \
  --private-key "$(cat secrets/sepolia-deployer.key)" --broadcast

# B
cp deploy/compose/sepolia/sepolia-addresses.json{.example,}
cp deploy/compose/sepolia/seed-params.json{.example,}
$EDITOR deploy/compose/sepolia/sepolia-addresses.json
# ETH_RPC_URL and ETH_SCAN_START_BLOCK go in .env: compose interpolates from
# --env-file, so every compose command works with no exports at all.
printf 'ETH_RPC_URL=%s\nETH_SCAN_START_BLOCK=%d\n' "$SEPOLIA_RPC" $((DEPLOY_BLOCK - 10)) >> .env
make up-sepolia
export EXCHANGE_ADMIN_URL=http://localhost:8082 \
       EXCHANGE_ADMIN_API_KEY=$(sed -n 's/^ADMIN_API_KEY=//p' .env)
go run ./cmd/exchangectl admin house-adjust --code custody_hot --asset ETH \
  --amount 0.05 --direction credit --reason "faucet, tx 0x..." --idempotency-key sepolia-hot-eth-1
go run ./cmd/exchangectl user register --email a@b.c --password 'correct horse battery'
go run ./cmd/exchangectl deposit-address --asset ETH     # faucet 打這裡
go run ./cmd/exchangectl deposits list                   # 等 credited
go run ./cmd/exchangectl admin sweeps list               # 等 confirmed
go run ./cmd/exchangectl withdrawals create --asset ETH --amount 0.003 --to 0x...
go run ./cmd/exchangectl withdrawals create --asset ETH --amount 0.008 --to 0x...
go run ./cmd/exchangectl admin withdrawals review <id> approve --note ok
go run ./cmd/exchangectl admin reconcile                 # 每列 DIFF = 0
make down-sepolia
```

相關文件:[`reconciliation-break.md`](reconciliation-break.md)、[`stuck-withdrawal.md`](stuck-withdrawal.md)、[`reorg-alert.md`](reorg-alert.md)、[`../domain.md`](../domain.md) §21。
