#!/usr/bin/env bash
# Shared CLI version selection for live E2E scripts (source this file).
# Live runs use the latest Codex and Claude Code CLIs by default; pin either
# with CODEX_CLI_VERSION / CLAUDE_CLI_VERSION (npm version or dist-tag) to bisect.

CODEX_CLI_VERSION="${CODEX_CLI_VERSION:-latest}"
CLAUDE_CLI_VERSION="${CLAUDE_CLI_VERSION:-latest}"
for _duckway_cli_v in "$CODEX_CLI_VERSION" "$CLAUDE_CLI_VERSION"; do
  case "$_duckway_cli_v" in
    ''|*[!A-Za-z0-9._+-]*) echo "invalid CLI version: $_duckway_cli_v" >&2; return 2 2>/dev/null || exit 2 ;;
  esac
done
unset _duckway_cli_v
export CODEX_CLI_VERSION CLAUDE_CLI_VERSION

# Print one grep-able line with the versions the CLIs actually report.
# Usage: duckway_print_cli_versions "<codex --version output>" "<claude --version output>"
duckway_print_cli_versions() {
  echo "[live-e2e] cli codex=${1:-unknown} claude=${2:-unknown} (requested codex=$CODEX_CLI_VERSION claude=$CLAUDE_CLI_VERSION)"
}
