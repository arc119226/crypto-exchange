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
| `image` / `helm` / `e2e` | 跟 PR 上一樣修,合併到 main,打**新的** tag |
| `release` 的版本斷言 | `build/Dockerfile` 的 `VERSION` build-arg 或 chart 的 `--app-version` 沒接上;修好、新 tag |
| `helm push` 403 | `packages: write` 權限或 ghcr 的 chart 套件可見性;修設定後**重跑 job**(chart 版本還沒推上去時可以重跑;推上去了就要新 tag,OCI chart 版本同樣不可覆蓋) |
| GitHub Release 建立失敗 | `contents: write`;重跑 job(`action-gh-release` 對已存在的 release 是更新) |

## 之後

`docs/runbooks/beta-deploy.md` 的「換版本」:`.env.prod` 的三個 tag → `make up-prod`。chart 使用者:`helm upgrade exchange oci://ghcr.io/arc119226/charts/exchange --version X.Y.Z`。
