#!/usr/bin/env bash
# release_check.sh — mandatory pre-release verification gate.
#
# Runs the full quality + e2e battery before every release. Any failing step
# aborts the run with a non-zero exit and the gateway must NOT be released.
#
#   ./scripts/release_check.sh
#
# Optional: RELEASE_SKIP_DOCKER=1 skips the container build step.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root_dir"

step() { printf '\n\033[1m[%s/%s] %s\033[0m\n' "$1" "$total" "$2"; }

total=8
step 1 "gofmt (no unformatted files)"
test -z "$(gofmt -l .)"

step 2 "go vet"
go vet ./...

step 3 "go test (all packages)"
go test ./...

step 4 "build gateway binary"
go build -o /dev/null ./cmd/gateway

step 5 "e2e: core capabilities"
scripts/e2e.sh

step 6 "e2e: api-key auth / rate limits / quotas"
scripts/e2e_apikey.sh

step 7 "e2e: release features (strategies / DLP / idempotency / admin / breaker)"
scripts/e2e_release.sh

step 8 "build container image"
if [[ "${RELEASE_SKIP_DOCKER:-0}" == "1" ]] || ! command -v docker >/dev/null 2>&1; then
  printf 'docker unavailable; skipping container build (RELEASE_SKIP_DOCKER=%s)\n' "${RELEASE_SKIP_DOCKER:-0}"
else
  docker build -t go-ai-gateway:release-check .
fi

printf '\n\033[1;32mAll release checks passed — safe to release.\033[0m\n'
