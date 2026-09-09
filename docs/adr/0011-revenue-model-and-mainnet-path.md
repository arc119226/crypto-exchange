# ADR-0011:營收模型(三種手續費與報表)與接主網的路線

- 狀態:已採納(Accepted)
- 日期:2026-09-09
- 相關:`docs/plan-v1.0.md` v1.1 §2.1、§3.1、§4 原則 0、§6.1.4 (h)(i)、§6.4.2、§6.6、§12 Phase 8 / Phase 9、§22、§23;`docs/domain.md` §6(`withdrawal_fee` 的註記);ADR-0005(帳本模型)、ADR-0007(簽名隔離)

## 背景

PR #28 之後、打 `v0.1.0` 之前,使用者提出兩個計畫書層級的問題:(1) 測試階段不碰真錢,但正式階段最終要接主網、碰真錢——計畫書把「不接主網」寫成沒有終點的鐵律;(2) 交易所靠手續費賺錢,交易手續費要後台可調,出入金也要抽平台手續費;既然撮合在平台內部,交易是否可以不上鏈、靠帳本記帳、出入金時才結算。

逐檔比對程式碼之後的現況:

1. **撮合本來就在鏈下**:`internal/matching` 只依賴 `internal/money`,`internal/trading` 的依賴樹沒有任何 `internal/chain/*`;成交是 `ledger.BuildSettleEntry` 的分錄與一則 outbox 事件,鏈只在充值、提現、歸集、補 gas、nonce 補洞時被碰。這是 §4 原則 4 的設計,只是分散在四節裡,沒有一段講清楚它是托管模型與它的義務。
2. **交易手續費已在收、後台可調**:每市場 maker / taker bps,成交時以收到的資產扣、進 `fee_revenue`;後台頁、admin API、CLI 三處可改,改了寫審計、發事件、引擎立即生效。沒有用戶等級費率(§3.1 明列不做)。
3. **提現手續費有欄位、沒有程式收它**:`registry.assets.withdrawal_fee` 存在、驗證、後台可填、公開 API 會回,但 `internal/chain/withdrawal`、`internal/ledger`、`internal/policy` 零引用;seed 為 0。用戶提 X 只扣 X,gas 全記 `gas_expense` 由熱錢包出。每一筆提現對平台都是淨虧損。
4. **充值手續費不存在**:沒有欄位、沒有程式;ERC-20 歸集前補 gas 的成本也由平台吸收。
5. **主網沒有程式閘門**:`ETH_CHAIN_ID=1` 過設定驗證;擋它的是 compose / Helm / `.env.prod.example` 與文件。真要接主網要換的:KMS/HSM/MPC signer(現在只有 `KeystoreSigner`)、真 USDC 地址(seed 直接吃 `addresses.json`)、mainnet 的 seed-params、gas 上限、確認數、RPC provider。
6. **沒有營收報表**:試算平衡的 house 科目能看 `fee_revenue` / `gas_expense` 的累計,`trading.trades` 有每筆 fee,但沒有期間報表、沒有指標、沒有面板。

「除了 gas 平台也要收手續費」這句話裡的 gas 要先釐清:gas 不是平台的收入,是平台付給網路的成本。平台的收入只有手續費;提現手續費的定價就是要蓋住 gas 再加利潤。

## 決定

### 1. 托管式、鏈下撮合、鏈上只在充提時結算——維持,並寫成明文

這是現在的架構,v2 也不改;每筆成交上鏈的 DEX 式結算明列不做(§22.3)。托管的義務——試算平衡、鏈上對帳、日後的 proof-of-reserves——寫進計畫書 §23.1,讓「用戶餘額是負債、鏈上資產是資產、兩邊隨時要對得起來」成為一句可以引用的話。

### 2. 提現手續費:每資產固定金額 + 可選比例、外加、`confirmed` 入帳、請求時快照、失敗不收

- `fee = withdrawal_fee + ceil(amount × withdrawal_fee_bps / 10000)`;`withdrawal_fee_bps` 預設 0。固定金額蓋 gas,比例給大額抽成;兩者都是 registry 欄位,後台可調、寫審計、發 `asset.updated`。
- **外加**:用戶填「要送多少」,收款方拿到整數,平台扣 `amount + fee`。預檢改為 `available ≥ amount + fee`;限額與 `min_withdrawal` 都看 `amount`。這比「內扣」少一個「實收金額」的欄位與語意,也和 `docs/domain.md` 早先記下的設計一致。
- **入帳時點**:`funds_locked` 時 `Hold(amount + fee)`;`broadcast` 時 amount 進 `pending_withdrawal`、fee 留在 hold;`confirmed` 時另一筆 `withdrawal:fee:{id}` 把 fee 從 hold 轉到 `fee_revenue`。每一條失敗路徑(`failed(broadcast)`、`failed(replaced)`、`resolve(refund)`)都把 fee 退回 available;`retry` 不動。平台沒把錢送出去就不收費——這是第三條鐵律。
- **快照**:`requested` 時算好寫在 `chain.withdrawals.fee / fee_asset`,之後改費率只影響新請求;在途的提現用它請求當時的價格,用戶看到的就是最後扣的。
- 不做跟隨即時 gas 的動態計價(需要即時估價與法幣換算,v2)。

### 3. 充值手續費:欄位與分錄先做,預設 0

`deposit_fee_bps`,入帳時 `X − fee` 給用戶、`fee` 進 `fee_revenue`;reorg 反向連 fee 一起反向。seed 為 0:業界慣例充值免費,收費會擋住資金進來,成本靠 `min_deposit` 與 `sweep_threshold` 控制;營運方要收隨時在後台開。

### 4. 營收報表與指標

`GET /admin/v1/reports/revenue(.csv)`、後台「營收」頁、`exchangectl admin revenue`:每資產六欄(交易費 maker / taker、提現費、充值費、gas、淨額、筆數),數字定義成可以直接用 SQL 驗證的式子;`ledger_fee_revenue_total{asset,source}` 與 `ledger_gas_expense_total{asset}` 兩個 counter,`WithdrawalGasExceedsFee` 告警。跨資產不換算法幣(v2)。

### 5. 主網是 v2 的目標,閘門制;程式閘門 v1.1 先做

計畫書 §2.1、§3.2、§4 原則 0 改成「v1 不接主網;v2 接,但只在 §22.1 的九道閘門全部通過之後」。閘門:程式閘門、密鑰託管(KMS/HSM/MPC、冷熱分離)、外部審計、法遵(KYC/AML、地址篩查、牌照)、風控(法幣等值限額、全站上限、雙人審核、kill switch)、鏈基礎設施(RPC ×2、確認數、gas 上限)、真實資產(token allowlist、真 USDC)、營運(on-call、事件響應 runbook、PITR、準備金)、放量計畫(內部帳戶 → 白名單 → 開放)。程式閘門——已知主網 id 命中時要求 `MAINNET_ACKNOWLEDGED=true`、`EXCHANGE_ENV=prod`、非 keystore signer,缺一拒絕啟動——半天的工,可以在 v1.1 就做,讓「不接主網」從文件變成測試斷言。

### 6. 不做

用戶等級費率(`users.fee_tier` 覆寫 schedule)、平台幣折抵、動態 gas 計價、法幣換算的損益表——全部列 v2 backlog(§23.6);DEX 式結算、非托管、槓桿、法幣通道——v2 也不做。

## 後果

- 這份 ADR 只改文件:計畫書升 v1.1(檔名維持 `plan-v1.0.md`,引用它的地方上百處)、新增 §22–§24 與 Phase 8 / Phase 9。實作分別是 Phase 8(手續費與報表,約 6–8 天)與 Phase 9(閘門制,不估天數)的 PR。
- Phase 8 的 schema 與 API 只加欄位不改語意(§4 原則 7):`Asset` 多兩欄、`Withdrawal` / `Deposit` 多 fee 欄;`POST /v1/withdrawals` 的預檢從 `available ≥ amount` 變成 `≥ amount + fee`,在 seed 費率為 0 時行為不變。
- seed 費率全為 0,所以 Phase 8 合併後所有既有測試、e2e 與 README 的數字不變;非零費率的路徑靠新的 scripted-chain 測試與「fee 守恆」不變量守。
- 營運方從此要看一張表決定費率:提現手續費低於 gas 會有告警,但決定權在人。
- `README.md` 第 7 節「只接測試鏈」在 v1 仍正確;Phase 9 通過閘門後才改。
