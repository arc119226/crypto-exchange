# 熱錢包低水位

熱錢包付所有提現與歸集的 gas。餘額低於 `ETH_HOT_WALLET_MIN` 不是對帳差異,是**提現開始失敗之前**的警告;原因有兩種——歸集停了(該進來的沒進來),或是提現正常地把它花光了。

## 症狀

- `alert.hot_wallet_low` 事件,或 Prometheus 的 `HotWalletLow`(`hot_wallet_balance{asset="ETH"} <` 門檻,compose 的 anvil 是 1、Sepolia overlay 是 0.02)。
- 提現卡在 `funds_locked` 或 `broadcast` 失敗,chain role log 有 `insufficient funds for gas`。
- 歸集 `exchangectl admin sweeps list` 出現 `failed`、`sweeps_failed_total` 上升,或 `sweeps_planned_total` 停止增加。

告警是**邊緣觸發**的:低於門檻只喊一次,回到門檻之上才會重新武裝。所以「沒有再收到告警」不代表已經好了,要看指標。

## 檢查指令

```sh
cast balance $HOT_WALLET_ADDRESS --rpc-url $ETH_RPC_URL
curl -s localhost:9100/metrics | grep -E 'hot_wallet_balance|sweeps_(planned|confirmed|failed)_total|withdrawals_transitions_total'
exchangectl admin sweeps list --output json | jq '.sweeps[] | select(.status=="failed")'
exchangectl admin reconcile                 # DIFF 應該是 0;低水位不是差異
psql -c "SELECT status, count(*) FROM chain.withdrawals GROUP BY status;"
```

## 處置

1. **歸集是不是停了?** 正常運作下充值會被收進熱錢包。`exchangectl admin sweeps list` 有沒有 `failed`、`ETH_SWEEP_ENABLED` 是不是被關了、signer role 有沒有在跑(歸集要簽名)。歸集卡住的第一個可見症狀就是這個告警;修好它之後餘額會自己回來。
2. **注資。** 從冷錢包(Sepolia:faucet)轉進 `HOT_WALLET_ADDRESS`,然後**立刻把它記進帳本**,否則下一輪對帳會報一筆完全正確的 break:

   ```sh
   exchangectl admin house-adjust --code custody_hot --asset ETH --amount 0.5 --direction credit \
     --reason "top up hot wallet, tx 0x..." --idempotency-key "hot-funding-2026-09-08"
   ```

   細節與為什麼只准調 `custody_hot` / `custody_deposit_addresses`,見 `docs/runbooks/reconciliation-break.md` 的第 3 節。
3. **門檻不對?** `ETH_HOT_WALLET_MIN` 應該是「幾天份的 gas」:看 `withdrawals_transitions_total{to="confirmed"}` 與 `sweeps_confirmed_total` 的日均乘上當下 gas price。太低會讓提現在你收到告警前就失敗;太高會一直響。

## 驗證

- `cast balance` 高於門檻,`hot_wallet_balance{asset="ETH"}` 同步(chain role 每 `ETH_RECONCILE_INTERVAL` 更新)。
- 注資那筆的 `house-adjust` 有 `audit.audit_events` 列;下一輪 `exchangectl admin reconcile` 的 ETH `DIFF = 0`。
- 停住的提現在下一個 tick 送出並 `confirmed`;`sweeps_failed_total` 不再上升。
- 告警重新武裝:餘額回到門檻之上後,再次跌破會再收到一次事件。

## 相關指標

`hot_wallet_balance{asset}`、`sweeps_planned_total`、`sweeps_confirmed_total`、`sweeps_failed_total`、`withdrawals_transitions_total{to}`、`reconciliation_diff{asset}`。
