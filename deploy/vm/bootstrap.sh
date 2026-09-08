#!/usr/bin/env bash
# Prepares an Ubuntu 24.04 VM for the beta stack (docs/runbooks/beta-deploy.md):
# Docker CE from Docker's repository, a firewall that admits only SSH and the
# edge, unattended security updates, the repository under /opt/exchange and a
# systemd unit that brings the stack up on boot. It does not start the stack:
# the secrets, the Sepolia fixtures and .env.prod come first, by hand.
#
#   sudo REPO_URL=https://github.com/arc119226/crypto-exchange.git REF=v0.1.0 deploy/vm/bootstrap.sh
set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/arc119226/crypto-exchange.git}"
REF="${REF:-main}"
DIR="${DIR:-/opt/exchange}"

log() { printf 'bootstrap: %s\n' "$*" >&2; }
[ "$(id -u)" = 0 ] || { log "run as root"; exit 2; }
. /etc/os-release
[ "${ID:-}" = ubuntu ] && [ "${VERSION_ID:-}" = "24.04" ] || log "WARNING: written for Ubuntu 24.04, this is ${PRETTY_NAME:-unknown}"

export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -y -q ca-certificates curl git gnupg ufw unattended-upgrades jq

# 1. Docker CE (docs.docker.com/engine/install/ubuntu)
if ! command -v docker >/dev/null; then
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" \
    >/etc/apt/sources.list.d/docker.list
  apt-get update -q
  apt-get install -y -q docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
fi
systemctl enable --now docker
docker compose version

# 2. the firewall: SSH and the edge only. Docker publishes ports through its
# own iptables chains, which ufw does not see -- which is why
# compose.prod.yaml publishes nothing but 80/443 and 127.0.0.1 ports.
ufw default deny incoming
ufw default allow outgoing
ufw allow 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp
ufw allow 443/udp
ufw --force enable

# 3. security updates without a person
cat >/etc/apt/apt.conf.d/20auto-upgrades <<'CONF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
CONF
systemctl enable --now unattended-upgrades

# 4. the repository
if [ ! -d "$DIR/.git" ]; then
  git clone --branch "$REF" "$REPO_URL" "$DIR"
else
  git -C "$DIR" fetch --tags origin
  git -C "$DIR" checkout "$REF"
fi
chmod 0750 "$DIR"

# 5. the unit: up on boot, down on stop; make up-prod pulls the images
cat >/etc/systemd/system/exchange.service <<UNIT
[Unit]
Description=crypto-exchange beta stack (docker compose)
Requires=docker.service
After=docker.service network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
WorkingDirectory=$DIR
ExecStart=/usr/bin/make up-prod
ExecStop=/usr/bin/make down-prod
TimeoutStartSec=900

[Install]
WantedBy=multi-user.target
UNIT
apt-get install -y -q make
systemctl daemon-reload
systemctl enable exchange.service

log "done. The stack is not started yet. Next, in $DIR:"
log "  1. sudo scripts/gen-prod-secrets.sh, then import the mnemonic it tells you to"
log "  2. fill .env.prod (images, EDGE_DOMAIN, ETH_RPC_URL, ETH_SCAN_START_BLOCK, HOT_WALLET_ADDRESS, BACKUP_S3_ENDPOINT)"
log "  3. deploy/compose/sepolia/sepolia-addresses.json and seed-params.json (docs/guides/sepolia.md)"
log "  4. systemctl start exchange   (= make up-prod), then docs/runbooks/beta-deploy.md from step 5"
