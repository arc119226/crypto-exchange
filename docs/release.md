# 發布

一個 `vX.Y.Z` 的 tag 就是一次發布:CI 的 `release` job 在完整的 pipeline(checks、integration、e2e、helm、image)全綠之後推 chart 與三個 image、開 GitHub Release。沒有手動步驟,也沒有「在筆電上 build 一下」的路徑——筆電上能做的只有事前檢查。

## Tag 規則

- 從 `main` 打,`vX.Y.Z`(semver;`v0.1.0` 是第一個 beta)。`git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0`。
- 一個 tag 只跑一次。**image tag 不能覆蓋**:`ghcr.io/arc119226/crypto-exchange:0.1.0` 推上去就是那個 digest。發布壞了就打新的 tag(`v0.1.1`),不要刪 tag 重打;GitHub Release 草稿可以刪,registry 裡的 image 不會刪。
- `docs/plan-v1.0.md` §12 的 Phase 勾選、`docs/domain.md` 的 Phase 段落、`docs/beta-checklist.md` 在 tag **之前**改好——release notes 由 GitHub 自動從 PR 產生,文件是 tag 裡的內容。

## CI 在 tag 上做什麼(`.github/workflows/ci.yml` 的 `release` job)

1. `needs: [image, helm, e2e]`:三個 image 已推上 ghcr(`image` job 在 push 事件推;tag 是 push 事件),chart 在 kind 上裝過並跑過 e2e,compose e2e 與備份演練通過。
2. `helm package deploy/helm/exchange --version X.Y.Z --app-version vX.Y.Z` → `helm push` 到 `oci://ghcr.io/arc119226/charts`(chart 名 `exchange`)。
3. **版本斷言**(DoD):`docker run ghcr.io/arc119226/crypto-exchange:X.Y.Z version --json` 的 `.version` 等於 `vX.Y.Z`;`helm show chart` 的 `appVersion` 等於 `vX.Y.Z`。任何一個不等,job 紅、不開 Release。
4. `softprops/action-gh-release`:自動 release notes、附 chart 的 `.tgz` 與 `exchangectl` 的 linux/amd64、darwin/arm64 二進位。

## 事前檢查(筆電)

```sh
make release-check TAG=v0.1.0
```

在本機做 CI 會做的兩個版本斷言:`helm package --version 0.1.0 --app-version v0.1.0` 得出來且 `helm show chart` 的 `appVersion` 是 `v0.1.0`;用同樣的 ldflags build 出來的 `exchange version --json` 印 `v0.1.0`。工作樹必須乾淨(一次發布就是 `main` 上的一個 commit)。它不需要 Docker,也不跑 `make lint && make test`——那是 tag 上 CI 的事,tag 之前 PR 已經跑過。

## 失敗時

| 紅在哪 | 怎麼辦 |
|---|---|
| `image` / `helm` / `e2e`,而且是**程式的問題** | 跟 PR 上一樣修,合併到 main,打**新的** tag |
| `image` / `helm` / `e2e`,但**不是這個 commit 的問題**(基礎設施、flaky test) | **Re-run failed jobs,tag 不用換。** 這三個 job 在失敗時什麼都還沒發布出去,重跑是從同一個 commit 重做一次;就算某個 image 已經推成功,重推同樣的 bytes 是同一個 digest。`v0.1.0` 就走過這條:`integration` 掉在一個 flaky 的 WebSocket 測試上,重跑即綠 |
| `release` 的版本斷言 | `build/Dockerfile` 的 `VERSION` build-arg 或 chart 的 `--app-version` 沒接上;修好、新 tag |
| `helm push` 403 | `packages: write` 權限或 ghcr 的 chart 套件可見性;修設定後**重跑 job**(chart 版本還沒推上去時可以重跑;推上去了就要新 tag,OCI chart 版本同樣不可覆蓋) |
| GitHub Release 建立失敗 | `contents: write`;重跑 job(`action-gh-release` 對已存在的 release 是更新) |

## 這條路徑第一次執行:`v0.1.0`(2026-09-10)

在那之前這份文件整份都是**推論**——`release` job 從來沒有跑過,只被 `make release-check` 的本機乾跑與 `image` job 的 build 驗過。`v0.1.0` 是第一次真的執行它,結果是:**發布邏輯本身零缺陷,四個斷言與四個產物第一次就全對。**

| 項目 | 實測 |
|---|---|
| 牆鐘 | 從推 tag 到 Release 發出來共 **30m54**,其中重跑那一輪是 19m44。**兩個數字要一起看**:19m44 那一輪的 `checks` 與 `fuzz-smoke` 沿用了前一次的結果沒有重跑,所以它不是一次乾淨的完整跑;30m54 才是「打了 tag 之後實際等多久」,而它包含了下面那次 flake |
| `release` 跑在哪 | GitHub 托管的機器,照第 1 節釘的——發布不依賴自建 runner |
| 版本斷言 | 通過。它是**把已經推上去的 image 拉下來**跑 `version --json` 比對,不是拿本機 build 的結果推論 |
| chart | `helm push` 成功,`appVersion` 等於 tag |
| GitHub Release | 四個附件:chart 的 `.tgz`、`exchangectl` 的 linux-amd64 與 darwin-arm64、`SHA256SUMS` |

唯一一次紅燈是 `integration` 掉在一個 flaky 的 WebSocket 測試(慢速客戶端被斷線之後,連線數的斷言沒有等伺服器回收),重跑就過,**tag 沒有換**。那個競態已經修掉了;它出現在發布這一次,正好說明上面那張表為什麼要把「程式的問題」和「不是這個 commit 的問題」分開寫——在發布當下最不該做的事,就是花時間判斷一個紅燈是不是真的。

## 之後

`docs/runbooks/beta-deploy.md` 的「換版本」:`.env.prod` 的三個 tag → `make up-prod`。chart 使用者:`helm upgrade exchange oci://ghcr.io/arc119226/charts/exchange --version X.Y.Z`。
