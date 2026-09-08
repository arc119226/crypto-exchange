# Beta 部署:從空 VM 到第一張單

Beta 的形態是一台 VM(4 vCPU / 8 GB,Ubuntu 24.04)跑 `docker compose`,疊三份檔案:`compose.yaml`(服務定義)+ `compose.sepolia.yaml`(鏈的設定;beta 只接 Sepolia,`docs/plan-v1.0.md` §0)+ `compose.prod.yaml`(正式環境的差異:發布版 image、檔案型 secrets、只開 80/443、edge、備份)。這份手冊是從一台什麼都沒有的 VM 走到第一筆真實交易的順序,以及每一步怎麼確認它做對了。Helm chart 在 CI 的 kind 上驗證,不是 beta 的部署方式(ADR-0009)。

## 症狀

用到這份手冊的時候:第一次上線;VM 整台重建(磁碟毀損、換供應商;先看 `docs/runbooks/backup-restore.md` 的還原段,再回到這裡的第 5 步);換版本(只做第 6–9 步)。

## 檢查指令

開始之前要有的東西,以及怎麼確認:

```sh
# 一個 v* tag 的三個 image 都存在(CI 的 image job 在 tag 上推)
docker manifest inspect ghcr.io/arc119226/crypto-exchange:v0.1.0 >/dev/null && echo ok
docker manifest inspect ghcr.io/arc119226/crypto-exchange-edge:v0.1.0 >/dev/null && echo ok
docker manifest inspect ghcr.io/arc119226/crypto-exchange-backup:v0.1.0 >/dev/null && echo ok
# DNS 指到這台 VM(ACME 需要 80/443 從外面可達)
dig +short "$EDGE_DOMAIN"
# Sepolia 的 fixtures(合約已部署、seed 參數)——docs/guides/sepolia.md 的產物
ls deploy/compose/sepolia/sepolia-addresses.json deploy/compose/sepolia/seed-params.json
# 一個 S3 相容儲存的端點與憑證(不是這台 VM)
# 一份離線產生的 BIP-39 助記詞(絕不是 anvil 的 test test … junk)
```

正在跑的 stack:

```sh
make ps-prod                                  # 每個容器 healthy
make logs-prod PROD_SERVICE=exchange-api TAIL=50
docker compose -f deploy/compose/compose.yaml -f deploy/compose/compose.sepolia.yaml -f deploy/compose/compose.prod.yaml --env-file .env.prod exec exchange-api /exchange version --json
curl -s https://$EDGE_DOMAIN/.well-known/jwks.json | jq .keys[].kid
curl -s https://$EDGE_DOMAIN/v1/markets | jq .
```

## 處置

1. **VM**:`sudo REPO_URL=… REF=v0.1.0 deploy/vm/bootstrap.sh`。裝 Docker CE、`ufw`(只放 22/80/443)、`unattended-upgrades`、clone 到 `/opt/exchange`、裝 `exchange.service`(開機 `make up-prod`)。它**不會**啟動 stack。
2. **Secrets**:`cd /opt/exchange && sudo scripts/gen-prod-secrets.sh`。產生 `.env.prod` 與 `secrets/prod/*`(root:65532 或 root:999、0640——compose 是 bind mount,主機上的 owner/mode 就是容器裡看到的)。然後照它印出的指令匯入助記詞:

   ```sh
   sudo env WALLET_KEYSTORE_PASSPHRASE_FILE=secrets/prod/wallet_keystore_passphrase \
     bin/exchange keys import-mnemonic --from /path/to/mnemonic.txt --keystore-dir secrets/prod/keystore
   sudo chown root:65532 secrets/prod/keystore/hd-seed.json && sudo chmod 0640 secrets/prod/keystore/hd-seed.json
   shred -u /path/to/mnemonic.txt
   ```

   印出的 hot wallet 寫進 `.env.prod` 的 `HOT_WALLET_ADDRESS`。**現在就做 escrow**:`tar -C secrets -cf - prod | age -p > exchange-secrets-$(date +%F).tar.age`,放到與 VM 無關的兩個地方。
3. **`.env.prod`**:三個 image、`EDGE_DOMAIN`、`ETH_RPC_URL`(含 provider key,所以這個檔案也是機密)、`ETH_SCAN_START_BLOCK`(MockUSDC 部署區塊)、`BACKUP_S3_ENDPOINT`、`GRAFANA_ADMIN_PASSWORD`。
4. **Sepolia fixtures**:`deploy/compose/sepolia/sepolia-addresses.json`、`seed-params.json`(`docs/guides/sepolia.md`)。合約部署在別台機器做,VM 上不需要 foundry。
5. **起 stack**:`sudo systemctl start exchange`(= `make up-prod`:pull 三個 image → `up -d --wait`)。順序由 `depends_on` 決定:postgres(首次啟動 `01-roles.sh` 用每個角色自己的密碼建角色)→ migrate → seed → 七個角色 → edge。第一次 ACME 要 30 秒左右。
6. **版本**:`exec exchange-api /exchange version --json` 的 `.version` 等於 `.env.prod` 裡的 tag;`GET /admin/v1/system/status`(經 SSH tunnel:`ssh -L 8082:127.0.0.1:8082 vm`)`database: ok`。
7. **Secret 檔權限**:`make ps-prod` 全部 healthy 就是證明——任何一個角色讀不到自己的 secret 檔會在啟動 log 出現 `permission denied` 且 `readyz` 不會綠。逐一驗:`docker compose … exec exchange-api /exchange healthcheck --url http://127.0.0.1:9100/readyz`(對 engine、chain、signer、stream、admin、worker 各做一次)。`exchange-signer` 的 log 有 `hot_wallet=0x…` 且等於 `HOT_WALLET_ADDRESS`。
8. **第一個管理員**:admin role 啟動時用 `ADMIN_BOOTSTRAP_EMAIL` + `secrets/prod/admin_bootstrap_password` 建立;TOTP 用 `docker compose … run --rm exchange-admin admin totp enroll --email admin@example.com`(`docs/runbooks/admin-totp.md`);登入後台換掉 bootstrap 密碼。
9. **Smoke**(從你的筆電):

   ```sh
   export EXCHANGE_API_URL=https://$EDGE_DOMAIN EXCHANGE_ADMIN_URL=http://127.0.0.1:8082   # admin 經 tunnel
   export EXCHANGE_ADMIN_API_KEY=$(sudo cat secrets/prod/admin_api_key)                    # 在 VM 上讀
   bin/exchangectl user register --email you@example.com --password '…'
   bin/exchangectl admin fund --account <id> --asset USDC --amount 100 --reason "beta smoke"
   bin/exchangectl orders place --side buy --price 1000 --qty 0.01 --client-order-id smoke-1
   bin/exchangectl book ETH-USDC
   ```

   瀏覽器開 `https://$EDGE_DOMAIN`,登入、看到訂單簿更新(WebSocket 經 edge 的 `/ws/*`)。
10. **備份**:`docker compose … run --rm backup once`,`admin.backups` 有一列 ok;然後照 `docs/runbooks/backup-restore.md` 做第一次演練並記錄 RTO。
11. **告警接收人**:Prometheus 在 `127.0.0.1:9090`、Grafana 在 `127.0.0.1:3000`(tunnel);沒有 Alertmanager(§14 範圍外),所以要有人看,或接一個外部的 Alertmanager 到 `prometheus.yml`。這是 `docs/beta-checklist.md` 的項目。

換版本(6–9 步的變體):改 `.env.prod` 的三個 tag → `make up-prod`(pull + 重建容器;`stop_grace_period 40s` 讓 engine 把最後一批 commit)→ 第 6、7、9 步。migration 由 `migrate` job 自動套用;有 migration 的版本先在筆電對一份還原的 dump 跑 `make backup-drill` 級的檢查。

## 驗證

- `make ps-prod`:postgres、redis、nats、七個角色、edge、prometheus、grafana、node-exporter、backup 全部 `healthy`/`running`;`jaeger`、`anvil`、`contracts-deployer`、`minio` **不在**清單裡。
- 從外面:`nmap -p- $VM_IP` 只有 22、80、443 開;`curl -sI http://$EDGE_DOMAIN` 是 308 到 https;`https://$EDGE_DOMAIN/v1/markets` 200;`wscat -c wss://$EDGE_DOMAIN/ws/v1/public` 連得上。
- 從裡面:`exchange version` == tag;`/.well-known/jwks.json` 一個 kid;每個角色的 `readyz` 綠;signer 的 `hot_wallet` 等於 `HOT_WALLET_ADDRESS`;chain role log 有 `nonce reconciled`(DB `next_nonce` == 鏈上)。
- 一張單成交、一筆小額提現走到 `confirmed`、後台首頁 `Last backup` 有時間、`docs/beta-checklist.md` 全部打勾。

## 相關指標

`exchange_ready{role}`、`up{job="exchange"}`、`node_filesystem_avail_bytes`(`DiskAlmostFull`)、`backup_last_success_timestamp_seconds`、`hot_wallet_balance{asset="ETH"}`(Sepolia faucet 的 ETH 是 gas,`HotWalletLow` 的門檻在 `compose.sepolia.yaml` 是 0.02)。
