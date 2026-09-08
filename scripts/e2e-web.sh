#!/usr/bin/env bash
# Browser smoke for web/trade against a running stack (docs/plan-v1.0.md §12
# Phase 6 DoD). In CI it follows `KEEP=1 make e2e`, so the compose app
# profile is still up on localhost:8080 (api), :8081 (stream) and :8082
# (admin) with the .env that gen-dev-secrets wrote; on a laptop point
# API_URL / WS_URL / ADMIN_URL / ADMIN_API_KEY at whatever is running.
#
# Playwright is pinned in web/trade/package.json; the Chromium it needs is
# installed here unless PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD says a matching one
# already exists (PLAYWRIGHT_BROWSERS_PATH).
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
  npx playwright install --with-deps chromium
fi
npm run build
npx playwright test "$@"
