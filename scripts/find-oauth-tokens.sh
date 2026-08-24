#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/find-oauth-tokens.sh [options]

Find OAuth token locations for Claude Code, Codex, and Duckway-managed modes.
By default this prints paths and field status only; token values are redacted.

Options:
  --mode auto|claude|codex|duckway|enterprise
      Which mode to inspect. Default: auto.
  --home DIR
      Home directory to inspect. Default: $HOME.
  --duckway-live-dir DIR
      Local ignored live credential directory. Default: ./live-credentials.
  --codex-home DIR
      Codex config directory to inspect. Default: $CODEX_HOME or ~/.codex.
  --print-values
      Print token values. Use only in a private shell; output may contain secrets.
  -h, --help
      Show this help.

Common files:
  Claude OAuth:       ~/.claude/.credentials.json
  Claude config:      ~/.claude.json, ~/.claude/settings.json, or $CLAUDE_CONFIG_DIR/*
  Codex OAuth:        ~/.codex/auth.json or $CODEX_HOME/auth.json
  Duckway live tests: ./live-credentials/claude-credentials.json
                      ./live-credentials/codex-auth.json
EOF
}

MODE="auto"
HOME_DIR="${HOME:-}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DUCKWAY_LIVE_DIR="$REPO_ROOT/live-credentials"
CODEX_HOME_DIR="${CODEX_HOME:-$HOME_DIR/.codex}"
PRINT_VALUES=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --mode)
      MODE="${2:-}"
      shift 2
      ;;
    --home)
      HOME_DIR="${2:-}"
      shift 2
      ;;
    --duckway-live-dir)
      DUCKWAY_LIVE_DIR="${2:-}"
      shift 2
      ;;
    --codex-home)
      CODEX_HOME_DIR="${2:-}"
      shift 2
      ;;
    --print-values)
      PRINT_VALUES=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

case "$MODE" in
  auto|claude|codex|duckway|enterprise) ;;
  *)
    echo "--mode must be one of: auto, claude, codex, duckway, enterprise" >&2
    exit 2
    ;;
esac

if [[ -z "$HOME_DIR" ]]; then
  echo "HOME is not set; pass --home DIR" >&2
  exit 2
fi

json_query() {
  local path="$1"
  local expr="$2"

  if [[ ! -f "$path" ]]; then
    return 1
  fi

  if command -v jq >/dev/null 2>&1; then
    jq -r "$expr // empty" "$path" 2>/dev/null
    return
  fi

  python3 - "$path" "$expr" <<'PY' 2>/dev/null
import json
import sys

path, expr = sys.argv[1], sys.argv[2]
with open(path, "r", encoding="utf-8") as f:
    data = json.load(f)

cur = data
for part in expr.strip(".").split("."):
    if not part:
        continue
    if isinstance(cur, dict):
        cur = cur.get(part)
    else:
        cur = None
    if cur is None:
        break

if cur is not None:
    print(cur)
PY
}

print_value() {
  local label="$1"
  local value="$2"

  if [[ -z "$value" ]]; then
    printf '  %-30s missing\n' "$label"
    return
  fi

  if [[ "$PRINT_VALUES" -eq 1 ]]; then
    printf '  %-30s %s\n' "$label" "$value"
    return
  fi

  local len="${#value}"
  if [[ "$len" -le 12 ]]; then
    printf '  %-30s present (%s chars)\n' "$label" "$len"
  else
    printf '  %-30s present (%s chars, redacted: %s...%s)\n' "$label" "$len" "${value:0:6}" "${value: -4}"
  fi
}

print_file_header() {
  local title="$1"
  local path="$2"

  echo
  echo "== $title =="
  echo "path: $path"
  if [[ -f "$path" ]]; then
    local mode
    mode="$(stat -c '%a' "$path" 2>/dev/null || stat -f '%Lp' "$path" 2>/dev/null || echo unknown)"
    echo "status: found (mode $mode)"
  else
    echo "status: missing"
  fi
}

inspect_claude_oauth_file() {
  local title="$1"
  local path="$2"

  print_file_header "$title" "$path"
  [[ -f "$path" ]] || return 0

  print_value "claudeAiOauth.accessToken" "$(json_query "$path" ".claudeAiOauth.accessToken" || true)"
  print_value "claudeAiOauth.refreshToken" "$(json_query "$path" ".claudeAiOauth.refreshToken" || true)"
  print_value "claudeAiOauth.expiresAt" "$(json_query "$path" ".claudeAiOauth.expiresAt" || true)"
  print_value "claudeAiOauth.subscriptionType" "$(json_query "$path" ".claudeAiOauth.subscriptionType" || true)"
  print_value "claudeAiOauth.rateLimitTier" "$(json_query "$path" ".claudeAiOauth.rateLimitTier" || true)"
}

inspect_claude_config_file() {
  local title="$1"
  local path="$2"

  print_file_header "$title" "$path"
  [[ -f "$path" ]] || return 0

  print_value "oauthAccount.emailAddress" "$(json_query "$path" ".oauthAccount.emailAddress" || true)"
  print_value "oauthAccount.organizationUuid" "$(json_query "$path" ".oauthAccount.organizationUuid" || true)"
  print_value "oauthAccount.accountUuid" "$(json_query "$path" ".oauthAccount.accountUuid" || true)"
}

inspect_codex_auth_file() {
  local title="$1"
  local path="$2"

  print_file_header "$title" "$path"
  [[ -f "$path" ]] || return 0

  print_value "tokens.access_token" "$(json_query "$path" ".tokens.access_token" || true)"
  print_value "tokens.refresh_token" "$(json_query "$path" ".tokens.refresh_token" || true)"
  print_value "tokens.id_token" "$(json_query "$path" ".tokens.id_token" || true)"
  print_value "tokens.account_id" "$(json_query "$path" ".tokens.account_id" || true)"
}

inspect_enterprise() {
  echo
  echo "== Claude Enterprise Provider Mode =="
  print_value "CLAUDE_CODE_USE_BEDROCK" "${CLAUDE_CODE_USE_BEDROCK:-}"
  print_value "CLAUDE_CODE_USE_VERTEX" "${CLAUDE_CODE_USE_VERTEX:-}"
  print_value "AWS_PROFILE" "${AWS_PROFILE:-}"
  print_value "AWS_REGION" "${AWS_REGION:-${AWS_DEFAULT_REGION:-}}"
  print_value "ANTHROPIC_VERTEX_PROJECT_ID" "${ANTHROPIC_VERTEX_PROJECT_ID:-}"
  print_value "CLOUD_ML_REGION" "${CLOUD_ML_REGION:-}"
  print_value "GOOGLE_APPLICATION_CREDENTIALS" "${GOOGLE_APPLICATION_CREDENTIALS:-}"

  local claude_settings="$HOME_DIR/.claude/settings.json"
  print_file_header "Claude settings" "$claude_settings"
  if [[ -f "$claude_settings" ]]; then
    print_value "env.CLAUDE_CODE_USE_BEDROCK" "$(json_query "$claude_settings" ".env.CLAUDE_CODE_USE_BEDROCK" || true)"
    print_value "env.CLAUDE_CODE_USE_VERTEX" "$(json_query "$claude_settings" ".env.CLAUDE_CODE_USE_VERTEX" || true)"
    print_value "env.AWS_PROFILE" "$(json_query "$claude_settings" ".env.AWS_PROFILE" || true)"
    print_value "env.ANTHROPIC_VERTEX_PROJECT_ID" "$(json_query "$claude_settings" ".env.ANTHROPIC_VERTEX_PROJECT_ID" || true)"
  fi

  if [[ -n "${CLAUDE_CONFIG_DIR:-}" && "$CLAUDE_CONFIG_DIR/settings.json" != "$claude_settings" ]]; then
    claude_settings="$CLAUDE_CONFIG_DIR/settings.json"
    print_file_header "Claude settings (CLAUDE_CONFIG_DIR)" "$claude_settings"
    [[ -f "$claude_settings" ]] || return 0
    print_value "env.CLAUDE_CODE_USE_BEDROCK" "$(json_query "$claude_settings" ".env.CLAUDE_CODE_USE_BEDROCK" || true)"
    print_value "env.CLAUDE_CODE_USE_VERTEX" "$(json_query "$claude_settings" ".env.CLAUDE_CODE_USE_VERTEX" || true)"
    print_value "env.AWS_PROFILE" "$(json_query "$claude_settings" ".env.AWS_PROFILE" || true)"
    print_value "env.ANTHROPIC_VERTEX_PROJECT_ID" "$(json_query "$claude_settings" ".env.ANTHROPIC_VERTEX_PROJECT_ID" || true)"
  fi
}

if [[ "$PRINT_VALUES" -eq 1 ]]; then
  cat >&2 <<'EOF'
WARNING: --print-values will print OAuth access and refresh tokens.
Do not paste this output into issue trackers, chat, shell history, or logs.
EOF
fi

echo "mode: $MODE"
echo "home: $HOME_DIR"
echo "claude config dir: ${CLAUDE_CONFIG_DIR:-<default>}"
echo "codex home: $CODEX_HOME_DIR"
echo "duckway live dir: $DUCKWAY_LIVE_DIR"

CLAUDE_CREDENTIALS_DEFAULT="$HOME_DIR/.claude/.credentials.json"
CLAUDE_CONFIG_DEFAULT="$HOME_DIR/.claude.json"
CLAUDE_SETTINGS_DEFAULT="$HOME_DIR/.claude/settings.json"
CLAUDE_CREDENTIALS_CONFIG_DIR="${CLAUDE_CONFIG_DIR:-}/.credentials.json"
CLAUDE_CONFIG_CONFIG_DIR="${CLAUDE_CONFIG_DIR:-}/.claude.json"
CLAUDE_SETTINGS_CONFIG_DIR="${CLAUDE_CONFIG_DIR:-}/settings.json"

CODEX_AUTH="$CODEX_HOME_DIR/auth.json"
CODEX_AUTH_DEFAULT="$HOME_DIR/.codex/auth.json"
DUCKWAY_CLAUDE="$DUCKWAY_LIVE_DIR/claude-credentials.json"
DUCKWAY_CODEX="$DUCKWAY_LIVE_DIR/codex-auth.json"

if [[ "$MODE" == "auto" || "$MODE" == "claude" ]]; then
  inspect_claude_oauth_file "Claude Code OAuth" "$CLAUDE_CREDENTIALS_DEFAULT"
  inspect_claude_config_file "Claude Code account config" "$CLAUDE_CONFIG_DEFAULT"
  if [[ -n "${CLAUDE_CONFIG_DIR:-}" ]]; then
    inspect_claude_oauth_file "Claude Code OAuth (CLAUDE_CONFIG_DIR)" "$CLAUDE_CREDENTIALS_CONFIG_DIR"
    inspect_claude_config_file "Claude Code account config (CLAUDE_CONFIG_DIR)" "$CLAUDE_CONFIG_CONFIG_DIR"
  fi
fi

if [[ "$MODE" == "auto" || "$MODE" == "codex" ]]; then
  inspect_codex_auth_file "Codex OAuth" "$CODEX_AUTH"
  if [[ "$CODEX_AUTH" != "$CODEX_AUTH_DEFAULT" ]]; then
    inspect_codex_auth_file "Codex OAuth (default ~/.codex)" "$CODEX_AUTH_DEFAULT"
  fi
fi

if [[ "$MODE" == "auto" || "$MODE" == "duckway" ]]; then
  inspect_claude_oauth_file "Duckway live Claude OAuth" "$DUCKWAY_CLAUDE"
  inspect_codex_auth_file "Duckway live Codex OAuth" "$DUCKWAY_CODEX"
fi

if [[ "$MODE" == "auto" || "$MODE" == "enterprise" ]]; then
  inspect_enterprise
fi

echo
echo "Tip: real OAuth refresh-token files should be mode 600. Duckway live credentials belong under live-credentials/ and are git-ignored."
