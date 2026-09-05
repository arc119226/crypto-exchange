# ADR-0007:簽名隔離 — 獨立 signer role、Signer 介面、加密 HD 種子檔、預留 KMS

- 狀態:已採納(Accepted)
- 日期:2026-09-05
- 相關:[ADR-0000](0000-interview-decisions.md) 第 4 輪;`docs/plan-v1.0.md` §5.1、§6.4.2、§9、§14;審查 finding F05 / F21 / F23 / P06

## 背景

v0.1 讓 wallet-service 與 withdrawal-worker 都持有私鑰並各自簽名,且「簽名 → 廣播 → 風控覆核」順序顛倒。白牌客戶日後可能要求 KMS / HSM;測試階段又必須能用本地 keystore 全自動跑 E2E。

## 選項

1. **私鑰放在 chain role**:最少元件;任何能觸發提現流程的程式碼都能簽任意交易。
2. **第一天就接 KMS**:安全,但 anvil / CI 無法離線跑,且 KMS 供應商綁定。
3. **獨立 `signer` role + `Signer` 介面**:v1 實作 `KeystoreSigner`(本地加密檔),預留 `KMSSigner`;`chain` role 不持有任何金鑰,只能送 `SignRequest`。

## 決定

採選項 3。

- `signer` 是唯一掛載 keystore 的 role(compose 只有 `exchange-signer` 與 `exchange-all` 掛 `secrets/keystore`),無對外 port;`chain` 以 NATS request-reply `cmd.signer.sign.{tenant}`(逾時 10 s)送 `SignRequest`;`role=all` 時為 in-process 呼叫。
- `SignRequest{Kind: withdrawal|sweep|gas_fund, RefID, From, To, Asset, Value, Tx}`;`Sign` 內做政策檢查:`withdrawal` → 對應 `withdrawals` 列為 `funds_locked` 且金額 / 資產 / 地址一致;`sweep` → `From ∈ deposit_addresses` 且 `To == hot`;`gas_fund` → `From == hot`、`To ∈ deposit_addresses`、`Value ≤ MAX_GAS_FUND`。每次簽名寫 `chain.signing_log`,`(kind, ref_id)` UNIQUE 防重簽,並寫審計。
- 鐵律:**帳本未先鎖定,絕不簽名**;每個狀態落庫後才做下一步。
- 種子儲存:go-ethereum 的 V3 keystore 只能存單一 secp256k1 私鑰,存不了 BIP-39 種子,因此用自訂格式 `secrets/keystore/hd-seed.json`(entropy 以 scrypt N=2^18 導出金鑰、AES-256-GCM 加密,passphrase 由 `WALLET_KEYSTORE_PASSPHRASE` 注入)。啟動解密一次,在記憶體派生熱錢包 `m/44'/60'/1'/0/0` 與充值地址 `m/44'/60'/0'/0/{i}`。
- 充值地址採預生成地址池(`signer` 派生、寫入 `chain.deposit_addresses`,`api` 只做指派),`api` 永遠碰不到金鑰。
- 開發助記詞由 `make gen-dev-secrets` 以 `cast wallet new-mnemonic` 產生,**禁止 anvil 預設助記詞**(腳本主動拒絕)。
- log 永不出現私鑰、助記詞、raw tx 以外的簽名材料:`SignRequest`、私鑰型別實作 `slog.LogValuer` 回 `[REDACTED]`(Phase 0 的 `telemetry.Secret` 是同一機制),Phase 4 以已知測試密鑰字串斷言所有容器 log 不含它們。

## 後果

- 正面:簽名審計單點;KMS / HSM 只是換 `Signer` 實作;`chain` 被攻破也拿不到金鑰。
- 負面:多一個必須健康的 role;request-reply 多一跳延遲(提現不是延遲敏感路徑)。
- 限制:加密 keystore + passphrase 只適合測試資產與封閉 beta;生產必須換 KMS / HSM / MPC,熱錢包資金管理與冷錢包流程不在 v1(`docs/plan-v1.0.md` §18)。
- Phase 0 已落地:`signer` role 骨架(ops server + 健康檢查)、compose 中的 `exchange-signer`、`secrets/keystore` 掛載、`exchange keys import-mnemonic` 佔位(exit 3);實作在 Phase 4a/4b。
