#!/usr/bin/env bash
# Live credential expiry preflight. Refreshes a provider credential only when
# it is expired or expires within a margin, then verifies the writeback.
#
# Usage: scripts/live-credentials-preflight.sh [codex] [claude]   (default: both)
#
# Environment:
#   CODEX_AUTH / CLAUDE_AUTH                      credential files (default: live credentials dir)
#   DUCKWAY_LIVE_PREFLIGHT_MARGIN_SECS            refresh margin, default 1800
#   DUCKWAY_LIVE_PREFLIGHT=skip                   explicit opt-out (offline/CI-like use only)
#   DUCKWAY_PREFLIGHT_REFRESH_CODEX_CMD/_CLAUDE_CMD  executable replacing the default
#                                                 `<provider>-oauth-live-e2e.sh --refresh-only` (tests)
# Tokens are never printed; refresh output is kept in a private temp file and
# only a redacted tail is shown on failure.

set -euo pipefail
set +x

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

if [ "${DUCKWAY_LIVE_PREFLIGHT:-}" = skip ]; then
  echo "[live-preflight] SKIPPED by DUCKWAY_LIVE_PREFLIGHT=skip (credentials not checked)" >&2
  exit 0
fi
# The refresh scripts source this preflight too; never recurse.
if [ "${DUCKWAY_PREFLIGHT_ACTIVE:-}" = 1 ]; then exit 0; fi

source "$SCRIPT_DIR/live-credentials-dir.sh"
LIVE_CREDENTIALS_DIR="$(duckway_resolve_live_credentials_dir "$REPO_ROOT")"
CODEX_AUTH="${CODEX_AUTH:-$LIVE_CREDENTIALS_DIR/codex-auth.json}"
CLAUDE_AUTH="${CLAUDE_AUTH:-$LIVE_CREDENTIALS_DIR/claude-credentials.json}"
MARGIN_SECS="${DUCKWAY_LIVE_PREFLIGHT_MARGIN_SECS:-1800}"
case "$MARGIN_SECS" in ''|*[!0-9]*) echo "[live-preflight] invalid margin: $MARGIN_SECS" >&2; exit 2 ;; esac

PROVIDERS=("$@")
[ "${#PROVIDERS[@]}" -gt 0 ] || PROVIDERS=(codex claude)

LOG_DIR="$(mktemp -d "${TMPDIR:-/tmp}/duckway-live-preflight.XXXXXX")"
trap 'rm -rf "$LOG_DIR"' EXIT INT TERM

fail() { echo "[live-preflight] FAIL: $*" >&2; exit 1; }

# Prints "<expiry_ms> <refresh_present 0|1>"; never prints token material.
read_expiry() {
  python3 -I - "$1" "$2" <<'PY'
import base64, json, sys
provider, path = sys.argv[1], sys.argv[2]
try:
    data = json.load(open(path))
    if provider == "claude":
        o = data.get("claudeAiOauth") or {}
        exp = int(o.get("expiresAt") or 0)
        refresh = o.get("refreshToken") or ""
    else:
        t = data.get("tokens") or data
        access = t.get("access_token") or data.get("access_token") or ""
        refresh = t.get("refresh_token") or data.get("refresh_token") or ""
        payload = access.split(".")[1]
        payload += "=" * (-len(payload) % 4)
        exp = int(json.loads(base64.urlsafe_b64decode(payload))["exp"]) * 1000
except Exception:
    print("0 0")
    sys.exit(0)
print(exp, 1 if refresh else 0)
PY
}

file_mode() { stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1" 2>/dev/null || true; }
now_ms() { python3 -I -c 'import time; print(int(time.time()*1000))'; }

check_provider() {
  local provider="$1" file override script out
  case "$provider" in
    codex) file="$CODEX_AUTH"; override="${DUCKWAY_PREFLIGHT_REFRESH_CODEX_CMD:-}"; script="$SCRIPT_DIR/codex-oauth-live-e2e.sh" ;;
    claude) file="$CLAUDE_AUTH"; override="${DUCKWAY_PREFLIGHT_REFRESH_CLAUDE_CMD:-}"; script="$SCRIPT_DIR/claude-oauth-live-e2e.sh" ;;
    *) echo "[live-preflight] unknown provider: $provider (use codex|claude)" >&2; exit 2 ;;
  esac
  [ -f "$file" ] && [ -r "$file" ] || fail "$provider credential file missing or unreadable: $file"
  [ "$(file_mode "$file")" = 600 ] || fail "$provider credential file mode is not 0600: $file"

  local exp present now remaining
  read -r exp present <<<"$(read_expiry "$provider" "$file")"
  now="$(now_ms)"
  remaining=$(( (exp - now) / 1000 ))
  if [ "$exp" -gt 0 ] && [ "$remaining" -gt "$MARGIN_SECS" ]; then
    echo "[live-preflight] $provider access token valid for ${remaining}s (margin ${MARGIN_SECS}s); no refresh"
    return 0
  fi
  if [ "$exp" -le 0 ]; then
    echo "[live-preflight] $provider expiry unreadable; refreshing"
  else
    echo "[live-preflight] $provider access token expires in ${remaining}s (margin ${MARGIN_SECS}s); refreshing"
  fi
  [ "$present" = 1 ] || fail "$provider credential has no refresh token; a new login is required"

  out="$LOG_DIR/$provider-refresh.log"
  local rc=0
  if [ -n "$override" ]; then
    DUCKWAY_PREFLIGHT_ACTIVE=1 "$override" >"$out" 2>&1 || rc=$?
  else
    case "$provider" in
      codex) DUCKWAY_PREFLIGHT_ACTIVE=1 CODEX_AUTH="$file" "$script" --refresh-only >"$out" 2>&1 || rc=$? ;;
      claude) DUCKWAY_PREFLIGHT_ACTIVE=1 CLAUDE_AUTH="$file" "$script" --refresh-only >"$out" 2>&1 || rc=$? ;;
    esac
  fi
  if [ "$rc" -ne 0 ]; then
    echo "[live-preflight] $provider refresh exited $rc; redacted tail:" >&2
    tail -n 10 "$out" | sed -E 's/eyJ[A-Za-z0-9_.-]+/[redacted]/g; s/[A-Za-z0-9_.-]{32,}/[redacted]/g' >&2
    fail "$provider refresh failed"
  fi

  [ "$(file_mode "$file")" = 600 ] || fail "$provider credential file mode is not 0600 after refresh: $file"
  read -r exp present <<<"$(read_expiry "$provider" "$file")"
  now="$(now_ms)"
  [ "$exp" -gt "$now" ] || fail "$provider credential still expired after refresh (writeback failed?)"
  [ "$present" = 1 ] || fail "$provider credential has no refresh token after refresh"
  echo "[live-preflight] $provider refreshed; new access token valid for $(( (exp - now) / 1000 ))s"
}

for provider in "${PROVIDERS[@]}"; do check_provider "$provider"; done
echo "[live-preflight] OK: ${PROVIDERS[*]}"
