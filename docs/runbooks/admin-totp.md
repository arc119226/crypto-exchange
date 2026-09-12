# Admin 登入與 TOTP

後台(admin role 的 `/admin`)的登入是密碼加一個 RFC 6238 的 TOTP 碼;密碼證明你知道一個秘密,碼證明你手上有那支手機。這份手冊寫的是營運方會遇到的幾件事:第一次啟用、換 secret、被鎖住、以及 admin 本人出事的時候。程式碼與計畫的對應在 `docs/domain.md` §24。

## 症狀

- 第一次上線:管理員用密碼登入後停在 `/admin/totp`,頁面說「No authenticator yet」——這不是錯,`exchange admin bootstrap` 只給密碼,不給 TOTP。
- 換手機、authenticator 遺失、疑似 secret 外洩。
- 頁面說「Locked for fifteen minutes」;或一個管理員一直被鎖而他自己沒有在按。
- 一個管理員要被停權,或**最後一個** active 的管理員被凍結的請求回 409(「last active administrator」)。
- admin role 拒絕啟動,log 說沒有 `ADMIN_TOTP_KEY`。

## 檢查指令

```sh
# 誰啟用了、誰被鎖到什麼時候
psql "$DATABASE_URL" -c "SELECT email, role, status, totp_enabled, totp_locked_until, totp_last_step FROM auth.users WHERE role = 'admin'"
# 誰什麼時候從哪裡登入過
psql "$DATABASE_URL" -c "SELECT user_id, ip, created_at, last_seen_at, revoked_at FROM auth.admin_sessions ORDER BY created_at DESC LIMIT 20"
# 最近的登入與 TOTP 事件(後台 Audit 頁可用 actor_id 濾出一個人)
psql "$DATABASE_URL" -c "SELECT created_at, action, actor_type, actor_id, ip FROM audit.audit_events WHERE action LIKE 'auth.admin.%' ORDER BY id DESC LIMIT 30"
# 這把鑰匙有沒有設(值不會印)
docker compose logs exchange-admin 2>/dev/null | grep -o 'admin_totp_key_set=[a-z]*' | tail -1
```

上面兩句查詢的 `ip` 欄在 beta 上**認不出是誰**:後台沒有掛在 edge 上,operator 是用 `ssh -L` 進來的,所以每一位 operator 的來源位址都是同一個固定值。要分辨人看 `actor_id`。理由與其他部署約束一起寫在 `docs/runbooks/beta-deploy.md` 的「部署約束」那一節。

## 處置

### 第一次啟用

1. `exchange admin bootstrap` 只給第一個管理員密碼(`ADMIN_BOOTSTRAP_EMAIL` / `ADMIN_BOOTSTRAP_PASSWORD`),**不給** TOTP。
2. 持 DB 憑證的人跑:

   ```sh
   # compose
   docker compose exec exchange-all exchange admin totp enroll --email admin@example.com
   # 或在主機上,DATABASE_URL 指向 ex_admin / ex_all
   exchange admin totp enroll --email admin@example.com --qr-out /tmp/admin.png
   ```

   它把 otpauth URL 與 base32 secret 印到**你的終端機**(不進容器 log),`--qr-out` 另存一張 QR 圖。這是 secret 唯一出現的一次。
3. 管理員把 secret 加進 authenticator,回到 `/admin/totp` 輸入它顯示的第一個 code。驗過之後 `totp_enabled = true`,session 換發成正式的 8 小時 session,之後每次登入都要碼。

為什麼 secret 只由 CLI 發、不讓瀏覽器在登入到一半時自己產:登入到一半的人只證明了密碼。讓那個人自己發 secret,密碼外洩就等於帳號外洩,第二因子就不是第二因子了。

### 換 secret(換手機、疑似外洩)

再跑一次 `exchange admin totp enroll --email X`。它會:發新 secret(`totp_enabled` 回到 `false`,等第一個 code 確認)、**撤銷該管理員所有的後台 session**、記審計 `auth.admin.totp.enroll`(actor `system`)。舊的 authenticator 從這一刻起沒用。

### 被鎖住

- 連續輸錯 5 次碼 → `totp_locked_until = now + 15 分鐘`,鎖定期間**正確的碼也拒絕**。等 15 分鐘;不需要任何人介入。
- 鎖定只計 TOTP 錯誤。密碼階段錯誤不鎖帳號(否則知道 email 的人每 15 分鐘就能把 admin 鎖一次),改用每個來源 IP 的節流(`LOGIN_PER_IP`,預設 10/分鐘),超過回 429。那個桶在**行程的記憶體裡**,前提是後台只有一個副本——而那是 chart **強制**的,不是預設值:`roles.admin.replicas` 只接受 1(`deploy/helm/exchange/templates/_helpers.tpl` 的 `exchange.isSingleton`),理由不只是這個節流,見 `docs/runbooks/beta-deploy.md` 的「部署約束」。鎖定不受影響,它記在資料庫。
- 同一個 30 秒窗口內用同一個碼登入兩次,第二次會被拒(`totp_last_step`:配中的那一步記下來,不再接受它或更早的步)。這不是鎖定,等下一個碼就好。
- 一直被鎖而且不是自己按的:有人拿到密碼在猜碼。用另一個管理員把這個帳號凍結(`exchangectl admin users freeze <id> --reason ...`,或後台的 Users 頁),換密碼,再 enroll。

### 凍結管理員、最後一個管理員

- 凍結一個 `role = admin` 的用戶會同時撤銷他所有的後台 session;他既登不進去也續不了。
- **最後一個 active 的管理員不能被凍**(API 回 409):凍了就沒人能進後台解凍。先用 `exchange admin bootstrap` 之外的方式(SQL 或另一個管理員)建第二個。

### 金鑰

- `ADMIN_TOTP_KEY`(32 bytes hex)封住所有管理員的 secret。它是獨立的一把,**不是** `API_KEY_MASTER_KEY`——那把能解開每一個 API key secret,admin role 沒有理由拿到它。admin role 沒有這把鑰匙會拒絕啟動。
- 鑰匙遺失 = 所有 secret 打不開 = 每個管理員都要重新 enroll。它在 `secrets/prod/admin_totp_key`,escrow 與輪替見 `docs/runbooks/backup-restore.md`、`docs/runbooks/key-rotation.md`(`exchange keys rewrap --domain totp`)。

## 驗證

- 啟用/換 secret 之後:該管理員 `totp_enabled = true`、`totp_locked_until` 為 NULL;`audit.audit_events` 有 `auth.admin.totp.enroll`(actor `system`)接著 `auth.admin.totp.confirmed`;舊 session 全部 `revoked_at` 有值;用舊 authenticator 的碼登入被拒。
- 解鎖之後:`totp_locked_until < now()`,正確的碼登得進去,`auth.admin.totp.verified` 出現。
- 凍結之後:該用戶 `status = frozen`、所有 session `revoked_at` 有值、`auth.admin.login.failed` 沒有再增加。
- 換過 `ADMIN_TOTP_KEY` 之後:每個管理員用既有 authenticator 仍登得進去(rewrap 成功的證明)。

## 附錄:Cookie、session 與審計動作

- cookie `admin_session`:`HttpOnly; SameSite=Lax; Path=/admin`。`Secure` 由 `ADMIN_COOKIE_SECURE` 決定;`EXCHANGE_ENV=dev` 預設關(本機 http),其他環境預設開——admin role 自己沒有 TLS,beta 是經 SSH tunnel 的 `http://localhost:8082`(瀏覽器視 localhost 為 secure context,cookie 照送)。
- 密碼過了、碼還沒過:pending session,10 分鐘,只能到 `/admin/totp` 與登出。碼過了:verified session,`ADMIN_SESSION_TTL`(預設 8h)。登出是 `revoked_at` 時間戳,列不刪。
- 審計動作:`auth.admin.login`(成功,actor `admin`)、`auth.admin.login.failed`(actor `user`:失敗的嘗試不算管理員)、`auth.admin.totp.confirmed`、`auth.admin.totp.verified`、`auth.admin.totp.failed`、`auth.admin.totp.locked`、`auth.admin.totp.enroll`(actor `system`)、`auth.admin.logout`。
