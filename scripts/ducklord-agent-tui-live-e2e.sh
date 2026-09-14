#!/usr/bin/env bash
# Real shell-first Codex/Claude interaction through Ducklord's Project TUI.
# Uses an isolated demo topology and never prints a live PTY framebuffer.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/container-runtime.sh"
source "$ROOT/scripts/ducklord-demo-common.sh"
duckway_init_container_runtime
RUNTIME="$CONTAINER_RUNTIME"
ORIGINAL_ARGS=("$@")
AGENTS=(codex claude)
case "${1:-}" in
  "") ;;
  --codex-only) AGENTS=(codex) ;;
  --claude-only) AGENTS=(claude) ;;
  *) echo "usage: $0 [--codex-only|--claude-only]" >&2; exit 2 ;;
esac
[ "$#" -le 1 ] || { echo "usage: $0 [--codex-only|--claude-only]" >&2; exit 2; }
if [ "${DUCKLORD_DEMO_LOCK_HELD:-0}" != 1 ]; then
  ducklord_reexec_with_demo_lock "$0" "${ORIGINAL_ARGS[@]}"
fi

credential_kind=all
if [ "${AGENTS[*]}" = codex ]; then credential_kind=codex; fi
if [ "${AGENTS[*]}" = claude ]; then credential_kind=claude; fi
for agent in "${AGENTS[@]}"; do
  case "$agent" in
    codex) ducklord_require_demo_secret "$ROOT/live-credentials/codex-auth.json" ;;
    claude) ducklord_require_demo_secret "$ROOT/live-credentials/claude-credentials.json" ;;
  esac
done
if [ "$credential_kind" != codex ]; then
  echo "[live-tui] validating Claude OAuth before starting containers"
  "$ROOT/scripts/claude-oauth-live-e2e.sh" --llm-only || {
    echo "[live-tui] Claude credential is unavailable; refresh live-credentials/claude-credentials.json" >&2
    exit 1
  }
fi

for resource in ducklord-dev ducklion-client-a ducklion-client-b ducklion-client-c ducklion-client-d; do
  if "$RUNTIME" container inspect "$resource" >/dev/null 2>&1; then
    echo "[live-tui] refusing to replace existing container $resource" >&2
    exit 1
  fi
done
if "$RUNTIME" network inspect ducklord-demo >/dev/null 2>&1; then
  echo "[live-tui] refusing to replace existing network ducklord-demo" >&2
  exit 1
fi
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  "$RUNTIME" rm -f ducklord-dev ducklion-client-a ducklion-client-b ducklion-client-c ducklion-client-d >/dev/null 2>&1 || true
  "$RUNTIME" network rm ducklord-demo >/dev/null 2>&1 || true
  exit "$status"
}
trap cleanup EXIT INT TERM

DUCKLORD_DEMO_LOCK_HELD=1 DUCKLORD_DEMO_AGENT_CREDENTIALS="$credential_kind" \
DUCKLORD_DEMO_CREDENTIAL_CLIENTS=client-a CONTAINER_RUNTIME="$RUNTIME" \
  "$ROOT/scripts/ducklord-podman-demo.sh" >/dev/null
"$RUNTIME" exec ducklord-dev cp /root/.ducklord/config.yaml /tmp/e2e-inspector.yaml
for agent in "${AGENTS[@]}"; do
  for layout in single split; do
    if [ "${DUCKLORD_LIVE_TUI_SPLIT_ONLY:-0}" = 1 ] && [ "$layout" != split ]; then continue; fi
    echo "[live-tui] testing interactive $agent in $layout shell-first Session pane layout"
    split=0
    if [ "$layout" = split ]; then split=1; fi
    DUCKLORD_AGENT_LIVE_TUI_E2E=1 DUCKLORD_AGENT_LIVE_TUI_SPLIT="$split" DUCKLORD_E2E_RUNTIME="$RUNTIME" \
    DUCKLORD_E2E_CONTROLLER=ducklord-dev DUCKLORD_E2E_AGENT="$agent" \
      go test -count=1 ./cmd/ducklord -run '^TestDucklordInteractiveAgentLiveTUIContainerE2E$' -v
  done
done
echo "[live-tui] PASS"
