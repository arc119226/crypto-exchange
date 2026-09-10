# ADR-0006:認證 — EdDSA JWT + JWKS、API key HMAC、admin TOTP;auth 是可替換的參考實作

- 狀態:已採納(Accepted)
- 日期:2026-09-05
- 相關:[ADR-0000](0000-interview-decisions.md) 第 5 / 7 輪;`docs/plan-v1.0.md` §3.1、§7.4、§14;審查 finding F27 / F28 / F30 / F33 / P01

## 背景

白牌引擎的客戶多半有自己的身分系統;引擎若把註冊 / 登入 / KYC / 2FA 放進核心,等於逼客戶「用你的帳號系統,或拆掉你的核心」。同時,引擎自己的管理後台需要強認證,內部角色之間需要可驗證的身分傳遞而不是裸 header。

## 選項

1. **HS256 共用 secret**:最簡單;但每個能驗證的角色都能簽發,內網任一 process 可冒充任何用戶。
2. **只支援外部 IdP(OIDC)**:引擎不做用戶目錄;demo、E2E、封閉 beta 都要先架 IdP。
3. **最小 auth 作為參考實作 + 非對稱 JWT**:users 目錄、email + password(argon2id)、Ed25519 簽發、其他角色以 JWKS 驗證;API key + HMAC 給程式化交易;驗證器設計成可換成客戶 IdP 的 JWKS URL。

## 決定

採選項 3。

- JWT:EdDSA(Ed25519),私鑰只在 `api` role(`JWT_PRIVATE_KEY_FILE`;compose 只有 `exchange-api` 與 `exchange-all` 掛 `secrets/jwt`),其他角色只設定 `JWT_JWKS_URL`。claims 固定 `sub / account_id / tenant_id / role / iat / exp`;access 15 分鐘;refresh 7 天、hash 存 `auth.refresh_tokens`,撤銷 = 刪列(不用 Redis)。
- API key:`X-API-KEY / X-API-TIMESTAMP / X-API-SIGNATURE`(HMAC-SHA256,±30 s),scopes `read|trade|withdraw`,可設 IP 白名單;`api` 為 API key 請求鑄 5 分鐘內部 JWT(`aud=internal`)後轉發。
- 內部身分傳遞:`role=all` 為 Go 呼叫;拆分部署時命令帶 JWT,engine / chain 以 JWKS 再驗一次;不信任裸 header。
- admin:密碼 + TOTP(RFC 6238)登入,**伺服端 session**(`auth.admin_sessions`,cookie 只放 session id),因此 `admin` role 不需要 JWT 私鑰;機器整合用 admin API key(scope 如 `users:kyc_write`)。首個 admin 由 `ADMIN_BOOTSTRAP_EMAIL/PASSWORD` 建立。
- 邊界:引擎不做 KYC、不做用戶端 2FA;`users.kyc_level` 由客戶系統透過 admin API 寫入;引擎內部一律以 `account_id` 為鍵。
- 限流:Redis token bucket(登入 per IP 10/min + per account 5/min;下單 / 取消 per account 20/s;提現 per account 5/min),無 Redis 降級為 in-memory。

## 後果

- 正面:私鑰單點、驗證多點;客戶換 IdP 只換驗證器設定;admin 攻擊面與用戶 JWT 分離。
- 負面:兩套認證機制(JWT、session)要各自測;API key HMAC 需要客戶端正確處理時間偏移。
- Phase 0 已落地:`exchange keys gen-jwt` 產 Ed25519 PKCS#8 PEM(0600);`JWT_PRIVATE_KEY_FILE` / `JWT_JWKS_URL` 已在 `app.Config`。實作在 Phase 3(用戶)與 Phase 5(admin TOTP)。
- Phase 3b 已落地(`internal/auth`、`internal/ratelimit`、`migrations/0007_auth_core.sql`;對應表見 `docs/domain.md` §13):register / login / refresh / logout、argon2id、Ed25519 JWT(`lestrrat-go/jwx/v3`,計畫 §9 寫 v2,採用時 v3 已是穩定版)、JWKS、API key HMAC(scopes、IP 白名單)、`auth.Authenticate` 中介層、`exchange admin bootstrap`、登入 / 下單限流。與本 ADR 文字的兩處差異:(1) refresh token **撤銷不刪列**,保留 `revoked_at + replaced_by` 才能辨識「已輪替的 token 被重放」並撤銷整個家族;(2) API key 的 secret 以 AES-256-GCM 加密存放,金鑰 `API_KEY_MASTER_KEY`(api role 持有;dev 未設則為程序生命期的隨機金鑰,非 dev 未設則停用 API key),而非只存 hash——HMAC 驗證需要明文 secret。`aud=internal` 的內部 JWT 鑄造與轉發隨 3c 的 NATS 命令匯流排實作;admin TOTP + session 在 Phase 5。
- Phase 3c 落地內部身分傳遞:拆分部署時 `api` 以同一把 Ed25519 私鑰鑄一枚 5 分鐘、`aud=internal` 的 JWT 隨每個命令送到 `engine`,引擎以 JWKS 驗證、要求 `trade` scope,並比對 token 的 `account_id` 與命令的 `account_id` —— 引擎信任 token,不信任 body。JWKS 由 `auth.RemoteVerifier` **惰性**抓取(第一次驗證時才抓、遇未知 kid 最多每 30 秒重抓一次、抓取失敗沿用舊金鑰),因為 compose 沒有 `engine → api` 的 `depends_on`,啟動時抓會讓引擎相依於 api 先起來。`role=all` 走 in-process 匯流排,不鑄也不驗 token。
- Phase 7 已落地 JWKS 金鑰輪替流程(`docs/runbooks/key-rotation.md`):`JWT_PRIVATE_KEY_FILE` 與 `JWT_PREVIOUS_KEY_FILE` 兩把並存,signer 同時發佈兩個 kid,驗證端接受兩者;分三階段推(所有副本的 JWKS 都有兩個 kid → 換簽章金鑰 → 撤下舊 kid),因此輪替期間不會有任何一枚在途的 token 驗不過。守著它的是 `TestSignerPublishesThePreviousKey` 與 `TestRemoteVerifierAcceptsBothKidsDuringRotation`。
- Phase 5c / 5d 落地 admin 登入(`internal/auth/adminsession.go`、`internal/admin/session.go`、`migrations/0018_auth_admin_sessions.sql`;對應表見 `docs/domain.md` §24):密碼 → pending session(10 分鐘)→ TOTP 碼 → verified session(8 小時),session 為隨機 token、表存 SHA-256、撤銷是時間戳;TOTP secret 以獨立的 `ADMIN_TOTP_KEY` 加密,**只由 `exchange admin totp enroll` 產生**,瀏覽器只收第一個 code。與本 ADR 文字的三處差異:(1)CSRF 用 Go 1.25 的 `http.CrossOriginProtection` + `SameSite=Lax` 而非每張表單的 token;(2)「scope 如 `users:kyc_write`」的 admin API key **未做**,機器整合仍用靜態 `ADMIN_API_KEY`;(3)凍結用戶時 `Login`、`Refresh`、API key 驗證與現貨帳戶在同一筆交易裡一起關,因為 `users.status` 在此之前除了登入沒有人讀。
