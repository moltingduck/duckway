#!/usr/bin/env bash
# Run independent Claude OAuth refresh and LLM live E2E cases.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLAUDE_AUTH="${CLAUDE_AUTH:-$ROOT/live-credentials/claude-credentials.json}"
RUN_REFRESH=1
RUN_LLM=1

while [ "$#" -gt 0 ]; do
  case "$1" in
    --refresh-only) RUN_REFRESH=1; RUN_LLM=0 ;;
    --llm-only) RUN_REFRESH=0; RUN_LLM=1 ;;
    *) echo "Unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

if [ ! -r "$CLAUDE_AUTH" ]; then
  echo "Missing readable CLAUDE_AUTH file: $CLAUDE_AUTH" >&2
  exit 2
fi
mode="$(stat -c '%a' "$CLAUDE_AUTH" 2>/dev/null || stat -f '%Lp' "$CLAUDE_AUTH" 2>/dev/null || true)"
if [ "$mode" != "600" ]; then
  echo "Refusing to use $CLAUDE_AUTH because permissions are $mode; expected 600" >&2
  exit 2
fi
CLAUDE_AUTH="$(cd "$(dirname "$CLAUDE_AUTH")" && pwd)/$(basename "$CLAUDE_AUTH")"

run_case() {
  name="$1"
  test_name="$2"
  echo "[$name] running $test_name"
  if DUCKWAY_TEST_CLAUDE_OAUTH_LIVE=1 \
     DUCKWAY_LIVE_CREDENTIALS_STRICT=1 \
     DUCKWAY_CLAUDE_LIVE_CREDENTIALS="$CLAUDE_AUTH" \
     go test ./internal/server/services -run "^${test_name}$" -count=1 -v; then
    echo "[$name] PASS"
    return 0
  fi
  echo "[$name] FAIL" >&2
  return 1
}

failed=0
if [ "$RUN_REFRESH" = "1" ]; then
  run_case refresh TestClaudeCodeOAuthLiveDuckwayUploadRefreshE2EIfCredentialsExist || failed=1
fi
if [ "$RUN_LLM" = "1" ]; then
  run_case llm TestClaudeCodeOAuthLiveDuckwayLLME2EIfCredentialsExist || failed=1
fi
exit "$failed"
