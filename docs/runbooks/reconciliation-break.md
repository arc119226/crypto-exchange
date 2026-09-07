# Runbook:對帳出現差異

`reconciliation.break_detected` 響了,或 `exchangectl admin reconcile` 有一列 `DIFF` 不是 0。

這份文件的目的是讓你在**知道錢在哪裡之前不要動任何東西**。對帳的價值全部來自它從不說謊,所以任何「先讓它變綠」的動作都是在拆掉它。

相關:[`docs/domain.md`](../domain.md) §20(恆等式與推導)、[`stuck-withdrawal.md`](stuck-withdrawal.md)、`docs/plan-v1.0.md` §6.4.4。

---

## 0. 先看方向

```
exchangectl admin reconcile
```

```
ASSET  LEDGER   CHAIN    UNCREDITED  ABOVE  IN FLIGHT  DIFF     
ETH    102.3    102.55   0           0      0          0.25     break
USDC   1000500  1000500  0           0      0          0        ok
```

`DIFF = CHAIN − LEDGER + ABOVE − UNCREDITED + IN FLIGHT`,零表示兩邊完全對得上。**沒有閾值**:每一項都是精確算出來的,所以 0.000000000000000001 也是差異。

方向決定急迫性:

| | 意思 | 急迫性 |
|---|---|---|
| **`DIFF > 0`** | 鏈上比帳本多。錢在,但帳本不知道它從哪來 | 高。不知道來源的錢不能當成收入,也不能拿去付提現 |
| **`DIFF < 0`** | 鏈上比帳本少。帳本相信有一筆錢,鏈上沒有 | **最高**。要嘛某個角色記錯帳,要嘛錢真的離開了 |

其他欄位是用來排除已知原因的,先確認它們是 0:`UNCREDITED`(鏈上看得到、還在等確認數)、`ABOVE`(帳本在讀餘額的那個區塊之上已經記了的)、`IN FLIGHT`(已經上鏈、帳本還沒記的)。**這三個不為零不是問題**——它們是修正項,已經算進 `DIFF` 了。它們只是告訴你當下有多少東西在動。

`BLOCK`(JSON 的 `block_height`)是餘額讀取的高度。如果它遠低於節點的 head,掃描器落後了:先處理那件事(見 `reorg-alert.md`),對帳的判斷在掃描器追上之前都不完整。

---

## 1. `DIFF > 0`:找出那筆錢從哪來

按可能性由高到低:

### a) 熱錢包被人從外面注資

最常見,而且完全正當:faucet、從冷錢包轉進來、營運方補資金。鏈上有、帳本不知道。

確認:比對差額與熱錢包最近的入帳。

```
cast balance $HOT_WALLET_ADDRESS --rpc-url $ETH_RPC_URL
```

處理:見第 3 節,記進帳本。

### b) 掃描器看不到的充值

合約內部轉帳(§4 列為不做,但沒有東西阻止它發生),或者代幣合約用了非標準的 Transfer 事件。錢落在某個充值地址上,`chain.deposits` 裡沒有對應的列。

確認:逐一比對每個充值地址的鏈上餘額與 `custody_deposit_addresses` 應該有的份額。

```sql
SELECT a.address, a.account_id,
       (SELECT COALESCE(SUM(d.amount),0) FROM chain.deposits d
         WHERE d.address = a.address AND d.asset = 'ETH' AND d.status = 'credited') AS credited
  FROM chain.deposit_addresses a
 WHERE a.tenant_id = 'default' AND a.chain_id = <chain>;
```

然後對每個 `address` 問一次 `cast balance`。差額最大的那個就是入口。

**這也是 4c-1 刻意留下的情況**:歸集的上限是帳本入過帳的數,所以這筆錢會留在鏈上而不是被掃進 `custody:deposit_addresses`。它出現在這裡是設計,不是意外。

### c) 錢進了一個沒發出去的池位

`chain.deposit_addresses` 裡 `account_id IS NULL` 的列。掃描器跳過它們,所以那裡的入帳永遠不會發生。通常代表有人重用了一個舊地址,或是地址從別的地方外流了。

```sql
SELECT address FROM chain.deposit_addresses
 WHERE tenant_id = 'default' AND chain_id = <chain> AND account_id IS NULL;
```

**如果是這個原因,先想清楚地址是怎麼流出去的**,再談記帳。

---

## 2. `DIFF < 0`:帳本相信有錢而鏈上沒有

**先假設是真的少了錢。** 依序排除:

1. **有交易剛剛上鏈而 worker 還沒記。** 這是 `IN FLIGHT` 應該蓋掉的情況,所以 `DIFF < 0` 而 `IN FLIGHT` 為 0,表示不是這個。再跑一次 `admin reconcile` 確認不是一瞬間的事。
2. **reorg 把一筆已經記帳的充值抹掉了。** 檢查 `chain.deposits` 有沒有 `orphaned`,以及 `reorg-alert.md`。
3. **某個角色記了一筆不該記的帳。** 查 `custody` 兩個科目最近的分錄:
   ```
   exchangectl admin entries --limit 50
   ```
   對照 `docs/domain.md` §1.3 (d)(e)(f) 的分錄表逐筆驗。
4. **金鑰外洩。** 上面都排除掉之後就是這個。**這時不要記帳,先停掉提現**(`ETH_SWEEP_ENABLED=false`、停掉 chain role),保留現場,依事故程序處理。

---

## 3. 記進帳本(只有在你知道錢從哪來之後)

`external` 科目就是為這件事存在的(§6.1.4 g:dev faucet、管理員調帳、對帳沖銷、Sepolia faucet 注資熱錢包)。

```
exchangectl admin house-adjust \
  --code custody_hot \
  --asset ETH \
  --amount 100 \
  --direction credit \
  --reason "faucet 注資熱錢包,tx 0x..." \
  --idempotency-key "hot-funding-2026-09-07"
```

- `--code`:只接受 `custody_hot` 與 `custody_deposit_addresses`。這兩個是「背後有一個別人可以匯錢進來的地址」的科目;其他 house 科目都是本系統自己的分錄推導出來的,調它們不是記錄事實而是藏 bug。
- `--direction`:`credit` = custody 增加(鏈上比帳本多),`debit` = custody 減少。
- `--reason`:**必填,而且要寫得讓半年後的人看得懂**。理想上放 tx hash。這條會進 `audit.audit_events`。
- `--idempotency-key`:重試安全。同一個 key 重送會回原本那筆,不會記兩次。

記完之後等下一輪(`ETH_RECONCILE_INTERVAL`,預設 5 分鐘)確認回到零:

```
exchangectl admin reconcile
```

**永遠不要為了讓數字變綠而記一筆你解釋不了的調整。** 沒有解釋的調整只是把「我們不知道錢在哪」改寫成「我們決定不再問」,而下一次真的少了錢的時候,沒有人會發現。

---

## 4. 熱錢包低水位(`alert.hot_wallet_low`)

不是差異,是餘額低於 `ETH_HOT_WALLET_MIN`。熱錢包付所有提現,所以這是**提現開始失敗之前**的警告。

```
cast balance $HOT_WALLET_ADDRESS --rpc-url $ETH_RPC_URL
exchangectl admin reconcile        # hot_wallet_balance 也在 /metrics
```

處理:

1. **歸集是不是停了?** 正常運作下充值會被收進熱錢包。`exchangectl admin sweeps list` 有沒有 `failed`,`sweeps_failed_total` 指標有沒有在動。歸集卡住的第一個可見症狀就是這個告警。
2. **注資。** 從冷錢包轉進去,然後**用第 3 節把它記進帳本**——否則下一輪對帳會報一筆完全正確的 break。

告警是邊緣觸發的:低於門檻只喊一次,回到門檻之上才會重新武裝。所以「沒有再收到告警」不代表已經好了,要看指標。

---

## 5. 對帳自己不動了

`exchangectl admin reconcile` 回 404,或報告的 `finished_at` 停在很久以前。

- `ETH_RECONCILE_ENABLED` 是不是 false。
- chain role 的 log:`reconciliation pass failed`。常見原因:
  - **節點讀不到某個資產的餘額**——registry 的 `contract_address` 打錯,或節點不支援指定區塊的 `eth_call`。對帳**刻意**在這種情況下整輪失敗而不是把讀不到的當成 0:當成 0 會報「這個資產全部不見了」,把人帶去完全錯的地方。
  - **`the chain moved while balances were being read`**——一輪讀到一半發生 reorg,這一輪被丟掉。偶爾出現是正常的;持續出現表示鏈很不穩,先看 `reorg-alert.md`。
  - **`hot wallet`**——`chain.hot_wallets` 還沒有列,signer 從來沒起來過。
- 位址池很大時一輪要 `(地址數 + 1) × 資產數` 次 RPC 呼叫。如果節點在限流,把 `ETH_RECONCILE_INTERVAL` 調長。
