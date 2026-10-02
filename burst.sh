#!/usr/bin/env bash
# One-command burst: ./burst.sh <BASE_URL> [extra flags]
#
# Runs the hot-seat storm, the stampede and the correctness checks, prints the
# outcome distribution and a closing reconciliation, and exits non-zero on any
# failure or any 5xx.
#
# Uses a local Go toolchain if there is one, otherwise runs the same program in
# the golang:1.23-alpine image, so Docker alone is enough. BURST_DOCKER=1
# forces the Docker path.
set -euo pipefail
cd "$(dirname "$0")"

BASE_URL="${1:-http://localhost:8080}"
shift || true

if [[ "${BURST_DOCKER:-0}" != "1" ]] && command -v go >/dev/null 2>&1; then
  exec go run ./cmd/burst -base-url "$BASE_URL" "$@"
fi

if ! command -v docker >/dev/null 2>&1; then
  echo "burst.sh needs either Go 1.23+ or Docker on the PATH" >&2
  exit 2
fi

# Inside a container "localhost" is the container itself. On Linux, share the
# host's network; on macOS and Windows, Docker Desktop exposes the host as
# host.docker.internal instead.
NET_ARGS=()
if [[ "$BASE_URL" =~ ://(localhost|127\.0\.0\.1)([:/]|$) ]]; then
  if [[ "$(uname -s)" == "Linux" ]]; then
    NET_ARGS=(--network host)
  else
    BASE_URL="$(sed -E 's#://(localhost|127\.0\.0\.1)#://host.docker.internal#' <<<"$BASE_URL")"
  fi
fi

echo "Running the burst in golang:1.23-alpine (target $BASE_URL)" >&2
exec docker run --rm ${NET_ARGS[@]+"${NET_ARGS[@]}"} \
  -e CGO_ENABLED=0 \
  -v "$PWD":/src:ro -w /src \
  -v seat-burst-gocache:/root/.cache/go-build \
  -v seat-burst-gomod:/go/pkg/mod \
  golang:1.23-alpine \
  go run ./cmd/burst -base-url "$BASE_URL" "$@"
