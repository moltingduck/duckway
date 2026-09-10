#!/usr/bin/env bash
# Live Codex/Claude completion-hook E2E through a real Ducklord -> SSH ->
# Ducklion -> PTY path. Credentials are mounted by the demo and never printed.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/container-runtime.sh"
source "$ROOT/scripts/ducklord-demo-common.sh"
duckway_init_container_runtime
RUNTIME="$CONTAINER_RUNTIME"
RUN_CODEX=1
RUN_CLAUDE=1
ORIGINAL_ARGS=("$@")

for dependency in flock jq stat; do
  command -v "$dependency" >/dev/null 2>&1 || { echo "missing required command: $dependency" >&2; exit 2; }
done

while [ "$#" -gt 0 ]; do
  case "$1" in
    --codex-only) RUN_CODEX=1; RUN_CLAUDE=0 ;;
    --claude-only) RUN_CODEX=0; RUN_CLAUDE=1 ;;
    *) echo "usage: $0 [--codex-only|--claude-only]" >&2; exit 2 ;;
  esac
  shift
done

if [ "${DUCKLORD_DEMO_LOCK_HELD:-0}" != 1 ]; then
  ducklord_reexec_with_demo_lock "$0" "${ORIGINAL_ARGS[@]}"
fi

require_secret() {
  ducklord_require_demo_secret "$1" || exit 2
}
[ "$RUN_CODEX" = 0 ] || require_secret "$ROOT/live-credentials/codex-auth.json"
[ "$RUN_CLAUDE" = 0 ] || require_secret "$ROOT/live-credentials/claude-credentials.json"

if [ "$RUN_CLAUDE" = 1 ]; then
  echo "[claude_code] validating the live credential before starting a PTY"
  if ! "$ROOT/scripts/claude-oauth-live-e2e.sh" --llm-only; then
    echo "[claude_code] FAIL: refresh live-credentials/claude-credentials.json and retry" >&2
    exit 1
  fi
fi

for resource in ducklord-dev ducklion-client-a ducklion-client-b ducklion-client-c; do
  if "$RUNTIME" container inspect "$resource" >/dev/null 2>&1; then
    echo "refusing to replace existing demo container $resource" >&2
    exit 1
  fi
done
if "$RUNTIME" network inspect ducklord-demo >/dev/null 2>&1; then
  echo "refusing to replace existing demo network ducklord-demo" >&2
  exit 1
fi

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  "$RUNTIME" rm -f ducklord-dev ducklion-client-a ducklion-client-b ducklion-client-c >/dev/null 2>&1 || true
  "$RUNTIME" network rm ducklord-demo >/dev/null 2>&1 || true
  exit "$status"
}
trap cleanup EXIT INT TERM

echo "[ducklord-agent-live-e2e] preparing isolated topology with $RUNTIME"
credential_kind=none
if [ "$RUN_CODEX" = 1 ] && [ "$RUN_CLAUDE" = 1 ]; then credential_kind=all
elif [ "$RUN_CODEX" = 1 ]; then credential_kind=codex
elif [ "$RUN_CLAUDE" = 1 ]; then credential_kind=claude
fi
DUCKLORD_DEMO_LOCK_HELD=1 DUCKLORD_DEMO_AGENT_CREDENTIALS="$credential_kind" \
DUCKLORD_DEMO_CREDENTIAL_CLIENTS=client-a CONTAINER_RUNTIME="$RUNTIME" \
  "$ROOT/scripts/ducklord-podman-demo.sh" >/dev/null

if [ "$RUN_CODEX" = 1 ]; then "$RUNTIME" exec ducklion-client-a test -f /home/duck/.codex/auth.json; fi
if [ "$RUN_CLAUDE" = 1 ]; then "$RUNTIME" exec ducklion-client-a test -f /home/duck/.claude/.credentials.json; fi
if [ "$RUN_CODEX" = 0 ]; then "$RUNTIME" exec ducklion-client-a test ! -e /home/duck/.codex/auth.json; fi
if [ "$RUN_CLAUDE" = 0 ]; then "$RUNTIME" exec ducklion-client-a test ! -e /home/duck/.claude/.credentials.json; fi
for remote in ducklion-client-b ducklion-client-c; do
  "$RUNTIME" exec "$remote" test ! -e /home/duck/.codex/auth.json
  "$RUNTIME" exec "$remote" test ! -e /home/duck/.claude/.credentials.json
done

session_json() {
  "$RUNTIME" exec ducklord-dev ducklord sessions client-a --json --config /root/.ducklord/config.yaml
}

restart_ducklion() {
  "$RUNTIME" exec -u duck ducklion-client-a sh -lc '
    pid=$(cat "$HOME/.duckway/ducklion-daemon.pid")
    kill -TERM "$pid"
    for _ in $(seq 1 100); do kill -0 "$pid" 2>/dev/null || break; sleep .05; done
    if kill -0 "$pid" 2>/dev/null; then
      echo "old Ducklion daemon did not stop" >&2
      exit 1
    fi
    for _ in $(seq 1 100); do test ! -S "$HOME/.duckway/ducklion/ducklion.sock" && break; sleep .05; done
    test ! -S "$HOME/.duckway/ducklion/ducklion.sock" || { echo "old Ducklion socket remained" >&2; exit 1; }
    nohup ducklion daemon >"$HOME/.duckway/ducklion-daemon.log" 2>&1 </dev/null &
    echo $! >"$HOME/.duckway/ducklion-daemon.pid"
    for _ in $(seq 1 100); do test -S "$HOME/.duckway/ducklion/ducklion.sock" && exit 0; sleep .05; done
    exit 1'
}

run_agent() {
  local type="$1" handle="$2" marker="$3"
  shift 3
  "$RUNTIME" exec ducklord-dev ducklord start client-a --name "$handle" --agent "$type" \
    --cwd /home/duck/projects/alpha --config /root/.ducklord/config.yaml -- "$@"

  local completed failed status output count json session_id generation
  for _ in $(seq 1 180); do
    json="$(session_json)"
    status="$(printf '%s' "$json" | jq -r --arg handle "$handle" '.[] | select(.name==$handle) | .status')"
    completed="$(printf '%s' "$json" | jq -r --arg handle "$handle" '.[] | select(.name==$handle) | (.activity_sequences.task_completed // 0)')"
    failed="$(printf '%s' "$json" | jq -r --arg handle "$handle" '.[] | select(.name==$handle) | (.activity_sequences.task_failed // 0)')"
    if [ "${completed:-0}" -ge 1 ]; then
      sleep 2
      json="$(session_json)"
      completed="$(printf '%s' "$json" | jq -r --arg handle "$handle" '.[] | select(.name==$handle) | (.activity_sequences.task_completed // 0)')"
      failed="$(printf '%s' "$json" | jq -r --arg handle "$handle" '.[] | select(.name==$handle) | (.activity_sequences.task_failed // 0)')"
      [ "$completed" = 1 ] && [ "$failed" = 0 ] || { echo "[$type] FAIL: completion was duplicated or contradicted: completed=$completed failed=$failed" >&2; return 1; }
      session_id="$(printf '%s' "$json" | jq -r --arg handle "$handle" '.[] | select(.name==$handle) | .session_id')"
      generation="$(printf '%s' "$json" | jq -r --arg handle "$handle" '.[] | select(.name==$handle) | .runtime_generation')"
      restart_ducklion
      for _ in $(seq 1 60); do
        json="$(session_json 2>/dev/null || true)"
        if printf '%s' "$json" | jq -e --arg id "$session_id" --argjson generation "$generation" \
          '.[] | select(.session_id==$id and .runtime_generation==$generation and .activity_sequences.task_completed==1)' >/dev/null 2>&1; then
          break
        fi
        sleep .25
      done
      printf '%s' "$json" | jq -e --arg id "$session_id" --argjson generation "$generation" \
        '.[] | select(.session_id==$id and .runtime_generation==$generation and .activity_sequences.task_completed==1)' >/dev/null || {
          echo "[$type] FAIL: durable completion was lost across Ducklion restart" >&2; return 1; }
      output="$("$RUNTIME" exec ducklord-dev ducklord read client-a "$session_id" --lines 120 --config /root/.ducklord/config.yaml 2>/dev/null || true)"
      count="$(printf '%s' "$output" | { grep -o "$marker" || true; } | wc -l | tr -d ' ')"
      [ "$count" -ge 1 ] || { echo "[$type] FAIL: expected response marker was not retained" >&2; return 1; }
      echo "[$type] PASS: exact completion survived Ducklion restart and response was retained"
      "$RUNTIME" exec ducklord-dev ducklord destroy client-a "$session_id" --config /root/.ducklord/config.yaml >/dev/null
      return 0
    fi
    if [ "${failed:-0}" -ge 1 ]; then
      echo "[$type] FAIL: agent hook reported task_failed (check credential validity)" >&2
      return 1
    fi
	output="$("$RUNTIME" exec ducklord-dev ducklord read client-a "$handle" --lines 120 --config /root/.ducklord/config.yaml 2>/dev/null || true)"
	if printf '%s' "$output" | grep -Eqi 'Select login method|OAuth access token has been revoked|Refresh token not found or invalid|authentication[_ ]error'; then
	  echo "[$type] FAIL: the supplied live credential is invalid or requires login" >&2
	  return 1
	fi
    if [ "$status" = stopped ]; then
      # Final output/activity delivery is durable and can arrive just after the
      # lifecycle transition, so keep polling until the bounded deadline.
      :
    fi
    sleep 1
  done
  echo "[$type] FAIL: no completed response before timeout" >&2
  return 1
}

run_id="$(date +%s)-$$"
failed=0
if [ "$RUN_CODEX" = 1 ]; then
  marker="DUCKLORD_CODEX_LIVE_${run_id}_OK"
  run_agent codex "codex-live-${run_id}" "$marker" /usr/local/bin/codex exec --skip-git-repo-check -m gpt-5.6-luna \
    "Join DUCKLORD_CODEX_LIVE_${run_id}_ and OK without spaces; reply with only the joined text." || failed=1
fi
if [ "$RUN_CLAUDE" = 1 ]; then
  marker="DUCKLORD_CLAUDE_LIVE_${run_id}_OK"
  run_agent claude_code "claude-live-${run_id}" "$marker" /usr/local/bin/claude -p \
    "Join DUCKLORD_CLAUDE_LIVE_${run_id}_ and OK without spaces; reply with only the joined text." || failed=1
fi
exit "$failed"
