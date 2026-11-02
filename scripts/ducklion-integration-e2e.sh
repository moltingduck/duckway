#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/container-runtime.sh"
duckway_init_container_runtime

"$CONTAINER_RUNTIME" run --rm \
  -v "$ROOT:/workspace" -w /workspace \
  -e DUCKWAY_INTEGRATION_E2E=1 -e GOTOOLCHAIN=local \
  docker.io/library/golang:1.26.10-alpine@sha256:5cb67f19c5e7994dcb80c074f7cfca8aeeada45bbdca035f73df269cefdcce61 \
  go test -count=1 -run '^TestDucklionStandaloneIntegrationE2E$' -v ./cmd/client
