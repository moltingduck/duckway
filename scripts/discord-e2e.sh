#!/usr/bin/env bash
# Deterministic Discord Control Channel E2E suite. No Discord credentials or
# network access are required: the tests use an in-process REST + WebSocket
# fixture but exercise Duckway's real gateway, routing, durable inbox and agent
# progress code paths.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$PROJECT_DIR"

GO_TEST=(go test -count=1)
if [ "${DISCORD_E2E_RACE:-0}" = "1" ]; then
  GO_TEST+=( -race )
fi

echo "[discord-e2e] gateway, resume replay, policy, thread and heartbeat"
"${GO_TEST[@]}" ./internal/server/services -run 'TestDiscord.*E2E' -v

echo "[discord-e2e] durable admission, lane FIFO, fencing and reclaim"
"${GO_TEST[@]}" ./internal/database/queries -run 'TestInbox(Admission|Claim|Expired)' -v

echo "[discord-e2e] single-message progress preview lifecycle"
"${GO_TEST[@]}" ./internal/client -run 'TestDiscordProgressPreviewE2E' -v

echo "[discord-e2e] PASS"
