#!/usr/bin/env bash
# One-command burst: ./burst.sh <BASE_URL> [extra flags]
# Runs the hot-seat storm, the stampede and the correctness checks, prints the
# outcome distribution and the reconciliation, and exits non-zero on failure.
set -euo pipefail
cd "$(dirname "$0")"
BASE_URL="${1:-http://localhost:8080}"
shift || true
exec go run ./cmd/burst -base-url "$BASE_URL" "$@"
