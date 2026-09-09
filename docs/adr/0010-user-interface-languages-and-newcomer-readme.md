# ADR-0010:介面語言(繁體中文 / 英文)與新手 README

- 狀態:已採納(Accepted)
- 日期:2026-09-08
- 相關:`docs/plan-v1.0.md` §12 Phase 5(「不做多語系」)、§12 Phase 6;`docs/domain.md` §25 表格「未做(刻意)」、§26.2;`README.md`、`README.en.md`、`docs/README.md`

## 背景

計畫書把多語系列為 Phase 5 不做的事,`docs/domain.md` §25 也把「SPA 的 i18n」放在刻意不做那一格。Phase 7 合併之後、打第一個 `v0.1.0` tag 之前,使用者提出兩件事:(1) 介面要支援中文與英文;(2) 要一份高中生看得懂的 README,介紹專案、教人裝起來操作一遍。兩件事都是「交給人」的最後一哩,跟 Phase 7 的 runbook 是同一種性質:程式沒變,能看懂、能用的人變多了。

要決定的有:範圍到哪裡、用不用套件、後台怎麼翻、公開 API 的錯誤訊息怎麼辦、README 拆幾份、怎麼守住它們不腐爛。

## 決定

### 1. 範圍:交易前台與營運後台;其餘維持英文

`web/trade`(React SPA)與 `internal/admin`(`html/template` + htmx)兩個介面各有繁體中文與英文,右上角一個切換器。`exchangectl` / `exchange` CLI、公開與 admin API、事件契約、log、程式註解與工程文件維持英文;路由、狀態碼、事件名、科目代碑、環境變數這些「運維人員要拿去對 runbook 的字」不翻,單位(bps)不翻。金額永遠是 decimal 字串直接顯示,**不走 `Intl.NumberFormat`**——千分位與小數點在兩種語言裡長得一樣,而且「顯示的數字就是 API 的字串」這件事比在地化重要。時間走 `Intl` 但只用兩個已知合法的 tag(`en-US`、`zh-TW`),瀏覽器回傳的怪 tag 不會讓格式化拋錯。

### 2. 不引入 i18n 套件;目錄有型別、有測試守完整性

兩邊都只是一張 key → 字串的表。前台:`messages.ts` 以英文為來源、`type Key = keyof typeof en`、中文是 `Record<Key, string>`,少一個 key `tsc` 就紅;狀態碼在 `enums.ts`,以原始碼為 key、查不到就回原始碼(API 文件說每個狀態集合都是開放的)。後台:`i18n_en.go` / `i18n_zh_tw.go` 是 Go map,重複 key 是編譯錯誤;`TestMessagesComplete` 檢查兩份 key 集合相等、每個 key 的 `fmt` 動詞多重集合相等(否則渲染出 `%!d(string=…)`)、template 裡每個 `T "key"` 都有、每個 key 都有人用。

英文目錄**一字不差沿用原本的英文**,所以英文渲染結果除了 `<html lang>`、切換器與 badge 的 `title` 之外 byte 相同,`ui_test.go` 與整合測試的一百多個英文斷言不動就過——這是「翻譯沒有偷改行為」的證據。

### 3. 後台:每種語言各 parse 一套 template;cookie > `Accept-Language` > 英文;`POST /admin/lang`

partial 會重綁 `.`(`{{template "pager" .Pager}}`),所以 `.L.T` 這種資料上的方法在 partial 裡拿不到;改成把 `T` / `Tn` / `status` 以語言閉包綁進 FuncMap,每種語言 parse 一套頁面(啟動時多幾毫秒)。語言來源:`admin_lang` cookie(切換器寫的,一年)> `Accept-Language` 第一個 q≠0 的條目(`zh*` → 繁中,其餘英文;信瀏覽器的排序,不自己排)> 英文。切換是純表單的 `POST /admin/lang`,掛在 session 中介層之外,登入頁也能切;`next` 只接受 `/admin` 底下的同源路徑。CSP `default-src 'self'` 不動,沒有任何 inline script。

flash 訊息維持存**已渲染的文字**:handler 在 `setFlash` 之前就用 request 的語言翻好。邊界:60 秒內切語言,那條 flash 是 POST 當時的語言。領域套件自己解釋的驗證錯誤(`registry: invalid input: …`、`ledger: invalid entry: …`)進 flash 時仍是英文——跟 CLI 印的字一樣,接受。

### 4. `Problem.detail` 維持英文;SPA 併一句在地化概述;`Problem.code` 延後

公開 API 的錯誤是 RFC 7807,`detail` 是英文 prose,沒有 machine code。加 `code` 要動 OpenAPI、三份產生物、約 60 個呼叫點,還要先把 `err.Error()` 直接當 detail 的路徑型別化,不是 release 前該做的事。現在的做法:SPA 依 HTTP status 給一句在地化的概述,已知的約十句 detail(`email already registered`、`this user is frozen`、`trading engine unavailable; retry`…)精確比對後翻譯,其餘把英文 detail 併在概述後面(中文 `{generic}({detail})`,英文就是 detail 本身,等於原本的行為)。過期的條目不會壞,只會退化成沒翻。

`Problem.code` 記為 `v0.1.0` 之後的事,範圍約 12 個 code:`email_taken`、`invalid_credentials`、`user_frozen`、`engine_unavailable`、`market_not_cancelable`、`no_deposit_address`、`deposits_disabled`、`withdrawals_disabled`、`invalid_input`、`rate_limited`、`idempotency_conflict`、`not_found`。有了它,`problems.ts` 那張表就從「比對句子」變成「查 code」。

### 5. README 拆成新手版 ×2 + 工程師版

根目錄 `README.md` 變成繁體中文的新手指南(是什麼、要準備什麼、十步照著打、出問題怎麼辦、名詞小抄、接下來看什麼、安全提醒),`README.en.md` 是它的英文版;原本 214 行的工程師索引搬到 `docs/README.md`,三份互相連結。GitHub 只自動渲染根目錄的 `README.md`,所以第一個看到的是新手版——這個專案接下來要交給的人,比要接手程式的人多。

新手版的每一步都有「你會看到」,所以它依賴程式的實際行為:`make gen-dev-secrets` 印什麼、前台 :8088 開得起來、`make faucet` / `make totp-enroll` 存在。為此 compose 多了 `web` profile(edge image + `Caddyfile.dev`,讀者不必裝 Node),Makefile 多了 `faucet` / `totp-enroll` / `ps` / `logs`,拿掉指向不存在子命令的 `demo`。`test/docs/readme_test.go` 守三件事:三份 README 的連結與反引號路徑都存在;中英文版 `##` 數量相同;**程式碼區塊逐一 byte 相同**——指令永遠不該因語言而不同,說明放區塊外。

## 後果

- 每個新的介面字串要寫兩份;前台由 `tsc` 逼、後台由 `TestMessagesComplete` 逼,漏了就紅,不會靜靜地掉回英文。
- 後台的英文渲染 byte 相同,既有測試全部不動;中文另有 `TestTemplatesRenderInZhTW` 與 `TestSetLang`。
- flash 的語言是 POST 當時的語言;SPA 元件 state 裡已存的錯誤字串要到下一次 fetch 才重翻——都寫在程式註解裡,都不值得多一層機制。
- `Problem.detail` 的中文覆蓋率取決於 `problems.ts` 那張表;伺服器改了一句話,中文頁會多出一句英文括號,不會壞。
- 新手 README 的十步是可執行的宣稱;`make up-single` 多起一個 `web` 容器(第一次 build 多 1–2 分鐘,`WEB=0` 略過),CI 的 e2e 不受影響(`scripts/e2e.sh` 明列 profile)。
- CI 的 `paths-ignore` 讓純文件 PR 不跑測試,所以 README 測試在合併到 main 時才跑——與 runbook 測試同一個已接受的取捨。
