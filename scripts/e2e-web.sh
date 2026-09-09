#!/usr/bin/env bash
# Browser smoke for web/trade against a running stack (docs/plan-v1.0.md §12
# Phase 6 DoD). In CI it follows `KEEP=1 make e2e`, so the compose app
# profile is still up on localhost:8080 (api), :8081 (stream) and :8082
# (admin) with the .env that gen-dev-secrets wrote; on a laptop point
# API_URL / WS_URL / ADMIN_URL / ADMIN_API_KEY at whatever is running.
#
# Playwright is pinned in web/trade/package.json and has no npm install script
# -- package-lock.json is lockfile v3 and marks only fsevents with
# hasInstallScript -- so `npm ci` never fetches a browser. The install below is
# the only download, and it is a no-op when a matching browser already sits in
# PLAYWRIGHT_BROWSERS_PATH (~/.cache/ms-playwright by default). On a machine
# that keeps its home directory between runs -- a laptop, a self-hosted CI
# runner -- it happens once, ever. Set PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 to
# skip it when a matching browser is already installed somewhere else.
set -euo pipefail
cd "$(dirname "$0")/../web/trade"

export API_URL="${API_URL:-http://localhost:8080}"
export WS_URL="${WS_URL:-ws://localhost:8081}"
export ADMIN_URL="${ADMIN_URL:-http://localhost:8082}"
if [[ -z "${ADMIN_API_KEY:-}" && -f ../../.env ]]; then
  ADMIN_API_KEY="$(sed -n 's/^ADMIN_API_KEY=//p' ../../.env | tr -d '[:space:]')"
  export ADMIN_API_KEY
fi
[[ -n "${ADMIN_API_KEY:-}" ]] || { echo "e2e-web: ADMIN_API_KEY is required (the faucet)" >&2; exit 2; }

npm ci --no-fund --no-audit
if [[ "${PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD:-0}" != "1" ]]; then
  # PLAYWRIGHT_INSTALL_DEPS=0 asks for the browser without the system
  # libraries: the download needs no privileges and knows nothing about the
  # host's distribution, while --with-deps needs sudo and a package list
  # Playwright ships per Ubuntu release. A machine that installed those
  # libraries once -- a laptop, a self-hosted runner -- sets it to 0.
  if [[ "${PLAYWRIGHT_INSTALL_DEPS:-1}" == "1" ]]; then
    npx playwright install --with-deps chromium
  else
    npx playwright install chromium
  fi
fi
npm run build
npx playwright test "$@"
