#!/usr/bin/env bash
# Fails when the combined statement coverage of the given packages is below
# the threshold. Usage: scripts/covercheck.sh <min-percent> <package> [package...]
# Example (CI unit job): scripts/covercheck.sh 95 ./internal/money
set -euo pipefail
if [[ $# -lt 2 ]]; then
  echo "usage: $0 <min-percent> <package> [package...]" >&2
  exit 2
fi
min="$1"
shift
profile="$(mktemp)"
trap 'rm -f "$profile"' EXIT
go test -count=1 -coverprofile="$profile" "$@" >/dev/null
total="$(go tool cover -func="$profile" | awk '/^total:/ { sub("%", "", $3); print $3 }')"
echo "coverage: ${total}% (minimum ${min}%) for $*"
if ! awk -v t="$total" -v m="$min" 'BEGIN { exit (t + 0 >= m + 0) ? 0 : 1 }'; then
  echo "coverage ${total}% is below the required ${min}%" >&2
  exit 1
fi
