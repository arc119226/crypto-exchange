# Runbook:在 Sepolia 上手動走一次全流程

`docs/plan-v1.0.md` §12 Phase 4d、§2.3 第 5 條。這份文件的目的是**用一條真的鏈拆穿只在 anvil 上成立的假設**——確認數、真實出塊時間、真實費用市場、會剪歷史的公開節點。不進 CI:Sepolia 有 faucet 限制、RPC 限流與真實出塊時間,放進 CI 只會製造間歇性紅燈,而間歇性紅燈會訓練所有人忽略紅燈。

相關:[`reconciliation-break.md`](reconciliation-break.md)、[`stuck-withdrawal.md`](stuck-withdrawal.md)、[`reorg-alert.md`](reorg-alert.md)。

> **底線(`docs/plan-v1.0.md` §0):不接主網、不碰真錢。** 這裡用到的每一把私鑰都只控制測試資產。這份文件裡沒有任何一步該在主網上重複。

---

## 這份文件分兩部分

| | 內容 | 前置條件 |
|---|---|---|
| **Part A** | 領測試幣、部署 MockUSDC、記下三個數字 | **現在就能做**,不需要任何程式改動 |
| **Part B** | 起 Sepolia stack、走完充值 → 提現 → 歸集 → 對帳 | 需要 4d-1 的程式改動(compose 覆蓋檔、Sepolia 設定、`seed --params`) |

Part A 最慢的是 faucet(有冷卻時間、有 captcha),所以先做。做完把 §A5 的三個數字給我。

---

# Part A — 現在就能做

## A0. 你需要準備什麼

**一把全新的 Sepolia 私鑰。** 不要用你已經在用的任何錢包。這把 key 只做兩件事:部署 MockUSDC、鑄 USDC。

```sh
# 用 repo 已經 pin 好的 foundry image,不必在本機裝 foundry
docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 wallet new
```

輸出會給你 `Address` 與 `Private key`。**私鑰放哪裡:**

```sh
mkdir -p secrets
umask 077
printf '0x...\n' > secrets/sepolia-deployer.key   # secrets/ 已經在 .gitignore
```

不要放進 `.env`、不要貼進 chat、不要寫進任何會進 git 的檔案。之後所有指令都用 `--private-key "$(cat secrets/sepolia-deployer.key)"` 帶進去。

**一個 RPC endpoint。** 我實測過(2026-09-07):

| Endpoint | 狀況 |
|---|---|
| `https://ethereum-sepolia-rpc.publicnode.com` | 可用,免註冊。但**是一池異質節點**:同一個「讀很舊的區塊」請求會時好時壞,回過 `{"code":4444,"message":"pruned history unavailable"}` |
| `https://1rpc.io/sepolia` | 可用,免註冊,深度歷史比較穩 |
| `https://rpc.sepolia.org` | **已死**,回 404 |
| `https://sepolia.drpc.org` | 免費方案不含 Sepolia |

**建議去 Alchemy 或 Infura 開一個免費帳號拿專屬 URL**——穩定、有配額看板、不會半途換成剪過歷史的後端。沒有的話用 `1rpc.io/sepolia`,`publicnode` 當備援。

把它存起來(這個不是密鑰,但也不必進 git):

```sh
export SEPOLIA_RPC="https://..."
```

## A1. 產生本機密鑰,拿到熱錢包位址

如果你還沒跑過:

```sh
make gen-dev-secrets
```

這會產生 `.env`、JWT 金鑰、一組新的 BIP-39 助記詞(`secrets/dev-mnemonic.txt`),並把 `m/44'/60'/1'/0/0` 推導出來的熱錢包位址寫進 `.env` 的 `HOT_WALLET_ADDRESS`。全部 gitignore。

```sh
grep HOT_WALLET_ADDRESS .env
```

**這個位址就是熱錢包。** 記下來,A3 要用它領錢。

> 已經跑過而想沿用現有助記詞:直接用現有的就好,同一顆種子在哪條 EVM 鏈上都成立。**不要**為了 Sepolia 跑 `FORCE=1` 重生——那會讓 anvil 那套已經發出去的充值地址全部失效。

## A2. 領測試幣給部署者

你總共需要大約 **0.12 SepoliaETH**,分三個位址:

| 收款位址 | 金額 | 用途 |
|---|---|---|
| A0 的部署者位址 | ~0.02 | 部署 MockUSDC + 鑄幣的 gas |
| `.env` 的 `HOT_WALLET_ADDRESS` | ~0.05 | 付提現、付歸集的補 gas,以及提現金額本身 |
| (Part B 才會知道的充值地址) | ~0.05 | 這一筆**就是那次充值** |

多數 faucet 一天給 0.05,而且**可以指定任意收款位址**——所以你可以在同一時間從三個不同 faucet 分別打到三個位址,不必自己轉帳。

先領前兩個(第三個要等 Part B 拿到充值地址)。常見 faucet(這類服務變動很快,死掉就換一個或直接搜 "sepolia faucet"):

- Google Cloud Web3 faucet
- Alchemy Sepolia faucet(要 Alchemy 帳號)
- Infura Sepolia faucet(要 Infura 帳號)
- `sepolia-faucet.pk910.de`(PoW,不需帳號,掛著跑就會累積)

確認收到:

```sh
docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 \
  balance <位址> --rpc-url "$SEPOLIA_RPC" --ether
```

## A3. 部署 MockUSDC

`MockUSDC` 沒有建構子參數,`mint` 也刻意不設權限(測試鏈專用,合約註解裡寫得很明白)。所以不需要部署腳本,一行 `forge create` 就夠:

在 repo 根目錄跑:

```sh
docker run --rm -v "$PWD/infra/contracts:/w" -w /w \
  ghcr.io/foundry-rs/foundry:v1.8.1 \
  forge create src/MockUSDC.sol:MockUSDC \
    --rpc-url "$SEPOLIA_RPC" \
    --private-key "$(cat secrets/sepolia-deployer.key)" \
    --broadcast
```

> `$(cat ...)` 是**你的 shell** 展開的(不是容器裡),所以路徑相對於 repo 根目錄,私鑰也不會被掛進容器。但它**會留在 shell history** 裡——跑完記得清掉,或改用 `--interactive` 手動貼。
>
> 這一步會在 `infra/contracts/` 底下留下 `out/` 與 `cache/`(第一次還會下載 solc 0.8.28)。兩個都已經 gitignore,刪掉無妨。

輸出會有 `Deployed to:` 與 `Transaction hash:`。拿 tx hash 問出**部署區塊高度**——這個數字很重要,Part B 的 `ETH_SCAN_START_BLOCK` 要用它:

```sh
docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 \
  receipt <tx hash> --rpc-url "$SEPOLIA_RPC" blockNumber
```

## A4. 鑄一些 USDC 給熱錢包

```sh
docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 \
  send <MockUSDC 位址> "mint(address,uint256)" <HOT_WALLET_ADDRESS> 1000000000000 \
    --rpc-url "$SEPOLIA_RPC" --private-key "$(cat secrets/sepolia-deployer.key)"
```

`1000000000000` = 1,000,000 USDC(6 位小數)。

確認:

```sh
docker run --rm --entrypoint cast ghcr.io/foundry-rs/foundry:v1.8.1 \
  call <MockUSDC 位址> "balanceOf(address)(uint256)" <HOT_WALLET_ADDRESS> --rpc-url "$SEPOLIA_RPC"
```

## A5. 把這三個數字給我

| 項目 | 值 |
|---|---|
| MockUSDC 合約位址 | `0x` |
| 部署區塊高度 | |
| 你選的 RPC endpoint | (如果是 Alchemy/Infura 這種帶 API key 的,**只要告訴我是哪家就好,不必給我 URL**) |

順便記一下 Part A 的實測數字,這些會進最終 runbook:

| 步驟 | tx hash | gas used | effective gas price | 實際成本 (ETH) | 送出 → 上鏈 (秒) |
|---|---|---|---|---|---|
| 部署 MockUSDC | | | | | |
| mint USDC | | | | | |

（`cast receipt <hash> --rpc-url "$SEPOLIA_RPC"` 會一次給你 `gasUsed` 與 `effectiveGasPrice`。）

---

# Part B — 等 4d-1 落地之後

> 這一段的指令依賴 4d-1 的改動(`deploy/compose/compose.sepolia.yaml`、`exchange seed --params`、可設定的錨點區塊、費用上限會真的等)。我交付 4d-1 時會把這一段補成可直接執行,並把你 A5 的數字填進設定檔。

大致的流程,先讓你有個底:

1. **寫兩個設定檔**到 `deploy/compose/sepolia/`(gitignore,附 `.example`):`sepolia-addresses.json`(A5 的合約位址與部署區塊)與 `seed-params.json`(faucet 尺寸的門檻:歸集門檻 0.01 ETH、最小提現 0.002 ETH、level-0 自動核可 0.01 ETH)。
2. **`make up-sepolia`**。它用獨立的 compose project name,所以會開一份乾淨的 Postgres,你的 anvil 那套原封不動留著。
   > 為什麼要獨立:`registry.assets` 有 `UNIQUE (tenant_id, symbol)`,再 seed 一次 Sepolia 是**取代**而不是新增。
3. **記帳熱錢包的 faucet 注資**。鏈上有、帳本不知道,對帳一定會報一筆完全正確的差異。用 `external` 科目記進去(§6.1.4 g):
   ```sh
   go run ./cmd/exchangectl admin house-adjust \
     --code custody_hot --asset ETH --amount 0.05 --direction credit \
     --reason "sepolia faucet 注資熱錢包,tx 0x..." \
     --idempotency-key "sepolia-hot-funding-1"
   ```
   USDC 同理。細節見 [`reconciliation-break.md`](reconciliation-break.md) §3。
4. **開使用者、要一個充值地址**,然後**用 faucet 直接打進那個充值地址**——faucet 本身就是外部匯款方,你不用另外準備一個有錢的錢包。USDC 那筆用 A0 的 key `mint` 進去就好。
5. **看它跑完** 6 個確認 → 入帳 → 歸集。真實時間大約 6 × 12s ≈ 72 秒起跳。
6. **走兩次提現**:一筆小額(低於 level-0 自動核可上限,會自動放行)、一筆大額(會排進 admin 審核,用 `exchangectl admin withdrawals review` 核可)。兩條路都要走過。
7. **對帳歸零**:`exchangectl admin reconcile`,每一列 `DIFF` 都要是 0。這是 §2.3 第 5 條的判定條件。
8. 把每一步的 tx hash / gas / 確認秒數填進下面的表,我寫進 4d-2。

## B 的結果表(待填)

| 步驟 | tx hash | block | gas used | effective gas price | 成本 (ETH) | 送出 → confirmed (秒) |
|---|---|---|---|---|---|---|
| 熱錢包 faucet 注資 | | | | | | |
| ETH 充值(faucet → 充值地址) | | | | | | |
| USDC 充值(mint → 充值地址) | | | | | | |
| ETH 歸集 | | | | | | |
| USDC 歸集:補 gas | | | | | | |
| USDC 歸集:轉帳 | | | | | | |
| 提現(自動核可) | | | | | | |
| 提現(人工審核) | | | | | | |

| 觀察 | 值 |
|---|---|
| 充值從上鏈到 `credited` 的實際時間 | |
| 對帳一輪的耗時 | |
| 整段期間 base fee 範圍 | |
| RPC 有沒有被限流 | |

---

## 症狀與處置

| 症狀 | 意思 | 處置 |
|---|---|---|
| chain role 一直重試啟動,log 有 `evm: genesis` 或 `pruned history unavailable` | endpoint 這一刻的後端剪掉了那個區塊 | 4d-1 之後錨點會釘在 `ETH_SCAN_START_BLOCK` 而不是創世,深度大幅變淺。仍然發生就換 endpoint |
| `the node is on a different chain than the cursor` | `ETH_CHAIN_ID` 或錨點與資料庫記的不合 | **不要清資料庫了事**,先確認你指到的是哪條鏈。這是保護不是障礙 |
| 掃描器一直落後,`chain_scanner_lag_blocks` 很大 | `ETH_SCAN_START_BLOCK` 沒設或設太小 | Sepolia head 約 1,165 萬;沒設就是從創世掃,batch 200 要跑約 58,000 輪 |
| `admin reconcile` 一直失敗,log 有讀不到餘額 | 對帳把餘額釘在 `min(head−確認數+1, 掃描游標)` 讀。chain role 停超過約 128 區塊(~26 分鐘)之後,那個高度的**狀態**已經被節點剪掉 | 讓掃描器追上就會自己好。對帳**刻意**整輪失敗而不是把讀不到的當成 0——當成 0 會報「這個資產全部不見了」 |
| 提現停在 `funds_locked` 沒動,log 有費用上限 | base fee 高過 `ETH_MAX_FEE_PER_GAS` | 這是**設計行為**(4d-1 之後):操作者的停損。等費用降,或調高上限 |
| 提現停在 `broadcast` 很久 | 費用不夠或網路壅塞 | 見 [`stuck-withdrawal.md`](stuck-withdrawal.md)。`REPLACE_AFTER` 在 Sepolia 是 3 分鐘 |
| 對帳報 `DIFF > 0` 而金額等於某次 faucet | 你忘了做第 3 步 | 見 [`reconciliation-break.md`](reconciliation-break.md) §3 |
| faucet 領不到 | 冷卻時間 / captcha / 該 faucet 死了 | 換一家。`pk910` 那種 PoW faucet 掛著跑就會累積 |

---

## Sepolia 與 anvil 的差別(一覽)

| | anvil | Sepolia |
|---|---|---|
| chain id | 31337 | 11155111 |
| 出塊 | 2 秒,穩定 | ~12 秒,會漏槽 |
| 確認數 | 1 | 6 |
| reorg | 只有 cheatcode 造出來的 | 真的會發生,通常 1–2 個區塊 |
| base fee | ≈ 0 | ~1 gwei,會浮動 |
| 費用上限 | 從沒被觸發過 | 會被觸發 |
| 歷史 | 全部都在 | 公開節點會剪,而且是一池異質後端 |
| 資金 | 想要多少有多少 | faucet,有冷卻時間 |
| 掃描起點 | 0(創世) | 部署區塊(**一定要設**) |
