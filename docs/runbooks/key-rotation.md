# 密鑰輪替

系統持有的每一種密鑰都能在不停止交易的前提下換掉;這份手冊寫每一種怎麼換、換的時候會看到什麼、怎麼證明換完了。密鑰清單與持有角色在 `docs/plan-v1.0.md` §14;程式碼對應在 `docs/domain.md` §26。

輪替的共同形狀:**先讓新舊兩把都能用,再切換,再拿掉舊的**。JWT 靠 JWKS 同時發布兩個 kid;三把 secretbox 主金鑰靠 `*_PREVIOUS` 讓程序兩把都能開、再用 `exchange keys rewrap` 把資料庫裡的列全部改成新金鑰封的;keystore passphrase 靠 `exchange keys rekey` 先寫暫存檔、驗證開得了、才蓋掉舊檔。

## 症狀

什麼時候要做:

- **例行**:beta 期間每 90 天輪一次三把主金鑰與 JWT 簽章金鑰(`docs/beta-checklist.md`);keystore passphrase 與 DB / NATS / Redis 密碼在人員異動時換。
- **外洩或疑似外洩**:任何一把出現在 log、ticket、聊天室、失控的筆電上。外洩情境**不走 grace period**:JWT 直接拿掉那把(所有用它簽的 access token 立刻失效,客戶端靠 `POST /v1/auth/refresh` 復原;refresh token 是 DB 列,不受影響);主金鑰照常 rewrap,但先做、不排程。
- **人員異動**:離職者拿過 `secrets/prod/` 的任何檔案。

不需要輪替的情境(常被誤判):webhook endpoint 自己的 secret 外洩 → 用 `exchangectl admin webhooks rotate-secret <id> --grace 24h`(既有功能,migration 0020),與主金鑰無關;某個 API key 外洩 → 用戶自己 revoke,與 `API_KEY_MASTER_KEY` 無關。

## 檢查指令

現在是哪一把:

```sh
# JWT:JWKS 正在發布的 kid(輪替中會有兩個;第一個是簽章用的那把)
curl -s http://localhost:8080/.well-known/jwks.json | jq -r '.keys[].kid'
# 某個 PEM 檔對應的 kid(私鑰或公鑰檔都可以)
bin/exchange keys jwt-public --in secrets/jwt/ed25519.pem >/dev/null
# 一枚 token 是用哪個 kid 簽的
jq -R 'split(".")[0] | @base64d | fromjson | .kid' <<<"$ACCESS_TOKEN"

# 主金鑰:各角色啟動 log 的 config 行只印「有沒有設」,不印值
docker compose logs exchange-api 2>/dev/null | grep -o 'api_key_master_key_set=[a-z]* api_key_master_key_previous_set=[a-z]*' | tail -1
# 資料庫裡還有多少列是舊金鑰封的:rewrap 本身就是檢查——沒有 *_PREVIOUS 時它只驗證每一列都能用現任金鑰開,一列都不改
DATABASE_URL=postgres://ex_migrate:...@localhost:5432/exchange bin/exchange keys rewrap --domain api-keys

# keystore:passphrase 對不對、熱錢包是哪個(signer 啟動 log 的 hot_wallet 要等於 HOT_WALLET_ADDRESS)
docker compose logs exchange-signer 2>/dev/null | grep -o 'hot_wallet=0x[0-9a-fA-F]*' | tail -1

# 上一次 rewrap 的紀錄
psql "$DATABASE_URL" -c "SELECT created_at, target_id, after FROM audit.audit_events WHERE action = 'secrets.rewrap' ORDER BY id DESC LIMIT 5"
```

## 處置

### JWT 簽章金鑰(`JWT_PRIVATE_KEY_FILE` / `JWT_PREVIOUS_KEY_FILE`)

驗證方(engine、chain、stream、admin,以及 api 自己)只認 JWKS 裡的 kid;`RemoteVerifier` 每 15 分鐘重抓一次,遇到不認識的 kid 會提早重抓一次、但同一份 JWKS **30 秒內最多重抓一次**。所以切換之後的 30 秒內,拿新 kid 的 token 打到還沒重抓的角色可能收到 401;這是設計上的節流(`remote_verifier.go`),不要調。

**多副本 api(Helm、`roles.api.replicas > 1`)三階段**——滾動更新中舊 pod 只發布舊 key、新 pod 用新 key 簽,驗證方從舊 pod 抓到 JWKS 就會拒絕新 token,所以「先發布、再簽」:

| 階段 | `JWT_PRIVATE_KEY_FILE` | `JWT_PREVIOUS_KEY_FILE` | 做什麼 |
|---|---|---|---|
| A | 舊 | **新的公鑰**(`keys jwt-public --in new.pem --out previous.pem`) | 滾動;所有副本的 JWKS 都有兩個 kid,仍用舊 key 簽 |
| B | 新 | 舊(私鑰或公鑰檔皆可) | 滾動;新 token 用新 kid,舊 token 仍驗得過 |
| C | 新 | (拿掉) | **等 30 分鐘**再滾動:`AUTH_ACCESS_TTL` 15 m + `ENGINE_INTERNAL_TOKEN_TTL` 5 m + RemoteVerifier 重抓 15 m 的邊際 |

單容器 beta(compose)只做 B 與 C:

```sh
bin/exchange keys gen-jwt --out secrets/jwt/ed25519.new.pem
mv secrets/jwt/ed25519.pem secrets/jwt/previous.pem && mv secrets/jwt/ed25519.new.pem secrets/jwt/ed25519.pem
# .env: JWT_PREVIOUS_KEY_FILE=/secrets/jwt/previous.pem
docker compose up -d exchange-api            # B
# 30 分鐘後
sed -i '/^JWT_PREVIOUS_KEY_FILE=/d' .env && rm secrets/jwt/previous.pem
docker compose up -d exchange-api            # C
```

chart 的對應是 `jwt.previous: true` + secret `exchange-jwt` 多一個 `previous.pem` key。**同一把設兩次會拒絕啟動**(同 thumbprint);`JWT_PREVIOUS_KEY_FILE` 不能單獨設。

外洩:跳過 grace,直接換成新 key 且不設 `JWT_PREVIOUS_KEY_FILE`。所有在線用戶的 access token 立刻失效;前台會用 refresh token 換新的,API key 用戶不受影響(API key 不是 JWT)。

### 三把 secretbox 主金鑰(`API_KEY_MASTER_KEY`、`WEBHOOK_SIGNING_KEY`、`ADMIN_TOTP_KEY`)

封存格式是 `nonce || ciphertext`,沒有 key id;程序用 keyring:**永遠用現任金鑰封,開的時候先試現任、再試 `_PREVIOUS`**。輪替四步,零停機:

1. 產生新金鑰:`openssl rand -hex 32`。
2. `.env`(或 secret 檔):`X=新`、`X_PREVIOUS=舊`。**逐角色重啟**持有那把的角色(`API_KEY_MASTER_KEY`:api;`WEBHOOK_SIGNING_KEY`:worker、admin;`ADMIN_TOTP_KEY`:admin)。這一刻起新建的列用新金鑰封,舊列照開。
3. 以 `ex_migrate`(表的 owner;app 角色沒有這些欄位的 UPDATE)跑:

   ```sh
   DATABASE_URL=postgres://ex_migrate:...@localhost:5432/exchange \
   X=新 X_PREVIOUS=舊 bin/exchange keys rewrap --domain api-keys    # 或 webhook / totp
   ```

   一筆交易把所有列改成新金鑰封的(webhook 連 grace period 中的 `previous_secret_enc` 一起),寫一列 `audit.audit_events`(`secrets.rewrap`,actor `system`,`after` 有 scanned / rewrapped)。冪等:再跑一次 rewrapped = 0。遇到任何一列兩把都開不了 → 整筆回滾、什麼都不改,回報那一列的 id;那是「早就沒人能開的列」,先查它。
4. 拿掉 `X_PREVIOUS`,再逐角色重啟。

三把各自獨立,可以分開輪。`_PREVIOUS` 單獨設或等於現任會拒絕啟動(`config.go` 的 keyring 驗證)。

### HD seed keystore passphrase(`WALLET_KEYSTORE_PASSPHRASE`)

seed 本身不變、所有位址不變;換的只是打開檔案的 passphrase。signer 只在啟動時讀 keystore,而且 compose 把它掛成唯讀、distroless 沒有 shell,所以在主機上做:

```sh
docker compose stop exchange-signer                      # role=all 時停 exchange-all
WALLET_KEYSTORE_PASSPHRASE=舊 WALLET_KEYSTORE_NEW_PASSPHRASE=新 \
  bin/exchange keys rekey --keystore-dir secrets/keystore  # 印 hot wallet;兩者都接受 _FILE
# 換 secret:.env 的 WALLET_KEYSTORE_PASSPHRASE 或 secrets/prod/wallet_keystore_passphrase
docker compose up -d exchange-signer
```

`rekey` 先寫 `hd-seed.json.tmp`(0600)、用新 passphrase 重新解開比對助記詞、才 `rename` 蓋掉舊檔;中途任何一步失敗舊檔原封不動、暫存檔刪掉。新舊相同會拒絕。兩次 scrypt(N = 2^18)各約一秒、256 MiB。**做完立刻更新離線 escrow**(`docs/runbooks/backup-restore.md`):escrow 裡的舊 passphrase 從這一刻起開不了 seed。

### 其他

- **webhook endpoint secret**:`exchangectl admin webhooks rotate-secret <id> --grace <≤168h>`;grace 內兩把都簽(`X-Exchange-Signature` 帶兩個)。
- **`ADMIN_API_KEY`**:純 env 常數,換值、重啟 api 與 admin、通知持有者。
- **DB 密碼**:`ALTER ROLE ex_api PASSWORD '...'` → 換該角色的 `DATABASE_URL` secret 檔 → 只重啟那個角色;八個角色逐一做,任一時刻只有一個角色在重啟。
- **NATS / Redis 密碼**:改 `infra/nats/nats.prod.conf` 的 secret 檔 / `redis_password` → 重啟 nats 或 redis → 逐角色換 `NATS_URL` / `REDIS_PASSWORD` 的 secret 檔並重啟(這段所有角色會斷線重連,cmdbus 呼叫端回 503 幾秒)。
- **`CONTRACT_DEPLOYER_KEY`**:只在 dev 鏈用,不輪替;Sepolia 的部署者是一次性的。

## 驗證

- JWT:B 之後 `jwks.json` 有兩個 kid、新登入的 token header 是第一個 kid、舊 token 在到期前打 `GET /v1/account` 仍 200;C 之後只剩一個 kid,`grep -c '401' ` 在 api log 沒有明顯上升。整合測試 `TestSignerPublishesThePreviousKey`、`TestRemoteVerifierAcceptsBothKidsDuringRotation` 守住行為。
- 主金鑰:rewrap 輸出 `N rows scanned, N re-sealed`,再跑一次是 `N scanned, 0 re-sealed`;`audit.audit_events` 有兩列 `secrets.rewrap`;拿掉 `_PREVIOUS` 重啟後,用一把**舊** API key 打 HMAC 請求仍 200、一個既有 webhook endpoint 仍收到簽名正確的投遞、admin 用既有 authenticator 仍登得進去。`TestRewrapMovesEveryDomainToTheCurrentKey` 是同一件事的自動化版本。
- keystore:`rekey` 印出的 hot wallet == `HOT_WALLET_ADDRESS`;signer 啟動 log 的 `hot_wallet` 相同;舊 passphrase 開不了(`bin/exchange keys rekey` 用舊的再跑一次會回「current passphrase」錯);`secrets/keystore/` 只剩 `hd-seed.json`。`TestRekeyKeystore` 守住。
- 通用:輪替後 24 小時內 `exchange_ready` 全部角色為 1、`http_request_duration_seconds{status="401"}` 沒有異常、`webhook_deliveries_total{status="failed"}` 沒有跳升。

## 相關指標

`exchange_ready{role}`、`http_request_duration_seconds{status}`、`webhook_deliveries_total{status}`、`trading_command_queue_depth`(NATS 重連期間會堆);沒有金鑰專屬指標,「哪一把在用」以 JWKS 的 kid 與 config log 為準。
