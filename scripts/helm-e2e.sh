#!/usr/bin/env bash
# Installs the chart into the current cluster (kind, in CI) and proves it
# works: `exchangectl e2e` from inside the cluster, the engine pod deleted
# and the book found intact, and `exchange version` in the running api pod
# equal to the chart's appVersion (docs/plan-v1.0.md §12 Phase 7 DoD).
#
#   scripts/helm-e2e.sh            # after scripts/kind-secrets.sh
#
# RELEASE, NAMESPACE, IMAGE_REPOSITORY, IMAGE_TAG and APP_VERSION override
# the defaults below. KEEP=1 leaves the release installed.
set -euo pipefail
cd "$(dirname "$0")/.."

RELEASE="${RELEASE:-exchange}"
NAMESPACE="${NAMESPACE:-default}"
IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-crypto-exchange}"
IMAGE_TAG="${IMAGE_TAG:-ci}"
CHART=deploy/helm/exchange
APP_VERSION="${APP_VERSION:-$(sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' "$CHART/Chart.yaml")}"
IMAGE="$IMAGE_REPOSITORY:$IMAGE_TAG"
ADMIN_KEY="$(cat ".kind/$RELEASE/admin-api-key")"
STAMP="$(date +%s)"
KUBECTL=(kubectl --namespace "$NAMESPACE")

log() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

# ctl runs exchangectl inside the cluster from the same image, so the e2e
# talks to the Services the way a client in the cluster would and nothing
# depends on a port-forward that the engine restart below would break.
ctl() {
  local name="ctl-$STAMP-$RANDOM"
  "${KUBECTL[@]}" run "$name" --rm -i --restart=Never --quiet --image="$IMAGE" --image-pull-policy=Never \
    --env="EXCHANGE_API_URL=http://$RELEASE-api:8080" \
    --env="EXCHANGE_ADMIN_URL=http://$RELEASE-admin:8082" \
    --env="EXCHANGE_ADMIN_API_KEY=$ADMIN_KEY" \
    --command -- /exchangectl "$@"
}

log "packaging the chart as app version $APP_VERSION"
pkg_dir="$(mktemp -d)"
trap 'rm -rf "$pkg_dir"' EXIT
helm package "$CHART" --app-version "$APP_VERSION" --version "0.1.0-ci" --destination "$pkg_dir" >/dev/null
tgz="$(ls "$pkg_dir"/exchange-*.tgz)"

log "installing $RELEASE from $tgz with image $IMAGE"
helm upgrade --install "$RELEASE" "$tgz" --namespace "$NAMESPACE" \
  -f "$CHART/values-kind.yaml" \
  --set "image.repository=$IMAGE_REPOSITORY" --set "image.tag=$IMAGE_TAG" \
  --set "dev.postgres.initdbConfigMap=$RELEASE-initdb" --set "dev.contracts.configMap=$RELEASE-contracts" \
  --wait --timeout 10m
"${KUBECTL[@]}" get pods -l "app.kubernetes.io/instance=$RELEASE"

log "exchange version inside the api pod equals the chart's appVersion"
running="$("${KUBECTL[@]}" exec "deploy/$RELEASE-api" -- /exchange version --json | jq -r .version)"
installed="$(helm get metadata "$RELEASE" --namespace "$NAMESPACE" -o json | jq -r .appVersion)"
[ "$running" = "$installed" ] || { echo "running $running, chart appVersion $installed"; exit 1; }
[ "$running" = "$APP_VERSION" ] || { echo "running $running, expected $APP_VERSION"; exit 1; }
echo "version $running"

log "the plan's worked example runs against the Services"
ctl e2e

log "a resting order survives the engine pod being deleted"
ctl user register --email "helm-$STAMP@e2e.local" --password "helm-$STAMP-pw" --output json >"$pkg_dir/session.json"
account="$(jq -r .account_id "$pkg_dir/session.json")"
token="$(jq -r .access_token "$pkg_dir/session.json")"
ctl admin fund --account "$account" --asset USDC --amount 5000 --reason "helm e2e" >/dev/null
"${KUBECTL[@]}" run "ctl-$STAMP-place" --rm -i --restart=Never --quiet --image="$IMAGE" --image-pull-policy=Never \
  --env="EXCHANGE_API_URL=http://$RELEASE-api:8080" --env="EXCHANGE_TOKEN=$token" \
  --command -- /exchangectl orders place --side buy --price 1000 --qty 0.5 --client-order-id "helm-$STAMP" >/dev/null
before="$(ctl book ETH-USDC --output json | jq -S 'del(.last_seq)')"
"${KUBECTL[@]}" delete pod -l "app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/component=engine" --wait=true
"${KUBECTL[@]}" rollout status "deploy/$RELEASE-engine" --timeout=5m
after=""
for _ in $(seq 1 30); do
  after="$(ctl book ETH-USDC --output json 2>/dev/null | jq -S 'del(.last_seq)' || true)"
  [ -n "$after" ] && [ "$after" != "null" ] && break
  sleep 2
done
if [ "$before" != "$after" ]; then
  echo "the book changed across the engine restart"; echo "before: $before"; echo "after:  ${after:-<none>}"; exit 1
fi
echo "$before" | jq -c .

if [ "${KEEP:-0}" != 1 ]; then
  log "uninstalling"
  helm uninstall "$RELEASE" --namespace "$NAMESPACE" >/dev/null
fi
log "helm e2e passed"
