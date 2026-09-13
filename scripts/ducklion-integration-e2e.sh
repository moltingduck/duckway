#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/container-runtime.sh"
duckway_init_container_runtime

"$CONTAINER_RUNTIME" run --rm \
  -v "$ROOT:/workspace" -w /workspace \
  -e DUCKWAY_INTEGRATION_E2E=1 \
  docker.io/library/golang:1.25-alpine \
  go test -count=1 -run '^TestDucklionStandaloneIntegrationE2E$' -v ./cmd/client
