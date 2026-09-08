#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d -t duckway-runtime-test-XXXXXX)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"

printf '%s\n' '#!/bin/sh' 'exit 1' >"$TMP/bin/docker"
printf '%s\n' '#!/bin/sh' 'case "$1 $2" in "info ") exit 0;; "compose version") exit 0;; esac' 'exit 1' >"$TMP/bin/podman"
chmod +x "$TMP/bin/docker" "$TMP/bin/podman"

result="$(PATH="$TMP/bin:/usr/bin:/bin" ROOT="$ROOT" bash -c '
  unset CONTAINER_RUNTIME
  . "$ROOT/scripts/container-runtime.sh"
  duckway_init_compose_runtime
  printf "%s|%s" "$CONTAINER_RUNTIME" "${DUCKWAY_COMPOSE[*]}"
')"
if [ "$result" != "podman|podman compose" ]; then
  echo "automatic Podman selection failed: $result" >&2
  exit 1
fi

printf '%s\n' '#!/bin/sh' 'case "$1 $2" in "info ") exit 0;; "compose version") exit 0;; esac' 'exit 1' >"$TMP/bin/docker"
result="$(PATH="$TMP/bin:/usr/bin:/bin" ROOT="$ROOT" bash -c '
  unset CONTAINER_RUNTIME
  . "$ROOT/scripts/container-runtime.sh"
  duckway_init_compose_runtime
  printf "%s|%s" "$CONTAINER_RUNTIME" "${DUCKWAY_COMPOSE[*]}"
')"
if [ "$result" != "docker|docker compose" ]; then
  echo "automatic Docker selection failed: $result" >&2
  exit 1
fi

if CONTAINER_RUNTIME=not-installed PATH="$TMP/bin:/usr/bin:/bin" ROOT="$ROOT" bash -c '
  . "$ROOT/scripts/container-runtime.sh"
  duckway_init_container_runtime
' >"$TMP/missing.out" 2>&1; then
  echo "missing explicit runtime was accepted" >&2
  exit 1
fi
grep -Fq "configured container runtime is not installed" "$TMP/missing.out"

echo "[container-runtime] PASS"
