#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d -t duckway-live-credentials-XXXXXX)"
trap 'rm -rf "$TMP"' EXIT

printf '{"tokens":{"access_token":"fixture-not-a-secret"}}\n' >"$TMP/codex-auth.json"
printf '{"claudeAiOauth":{"accessToken":"fixture-not-a-secret"}}\n' >"$TMP/claude-credentials.json"
chmod 600 "$TMP"/*.json

output="$(DUCKWAY_LIVE_CREDENTIALS_DIR="$TMP" "$ROOT/scripts/ducklord-podman-demo.sh" --check-live-credentials)"
grep -q 'Codex live credential: ready' <<<"$output"
grep -q 'Claude live credential: ready' <<<"$output"
if grep -q 'fixture-not-a-secret' <<<"$output"; then
  echo "credential value leaked by preflight" >&2
  exit 1
fi

chmod 644 "$TMP/codex-auth.json"
if DUCKWAY_LIVE_CREDENTIALS_DIR="$TMP" "$ROOT/scripts/ducklord-podman-demo.sh" --check-live-credentials >"$TMP/out" 2>"$TMP/err"; then
  echo "unsafe credential permissions were accepted" >&2
  exit 1
fi
grep -q 'permissions 644' "$TMP/err"

chmod 600 "$TMP/codex-auth.json"
mv "$TMP/claude-credentials.json" "$TMP/claude-target.json"
ln -s "$TMP/claude-target.json" "$TMP/claude-credentials.json"
if DUCKWAY_LIVE_CREDENTIALS_DIR="$TMP" "$ROOT/scripts/ducklord-podman-demo.sh" --check-live-credentials >"$TMP/out" 2>"$TMP/err"; then
  echo "credential symlink was accepted" >&2
  exit 1
fi
grep -q 'non-symlink' "$TMP/err"

echo "live credential injection preflight passed"
