#!/usr/bin/env bash
# Offline test of the live credential preflight decision logic. Uses fixture
# credentials with fake tokens and stub refresh commands; no network, no live calls.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PREFLIGHT="$ROOT/scripts/live-credentials-preflight.sh"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/duckway-preflight-test.XXXXXX")"
trap 'rm -rf "$TEST_ROOT"' EXIT INT TERM

SECRET_ACCESS="fake-access-token-SECRET-aaaaaaaaaaaaaaaaaaaa"
SECRET_REFRESH="fake-refresh-token-SECRET-bbbbbbbbbbbbbbbbbbbb"
CALLS="$TEST_ROOT/calls"
fail() { echo "FAIL: $*" >&2; exit 1; }

jwt() { # jwt EXP_SECONDS -> fake unsigned JWT containing the secret marker
  python3 -I - "$1" <<'PY'
import base64, json, sys
b = lambda d: base64.urlsafe_b64encode(json.dumps(d).encode()).rstrip(b"=").decode()
print(b({"alg": "none"}) + "." + b({"exp": int(sys.argv[1]), "marker": "SECRET-jwt-claim"}) + ".SECRET-sig")
PY
}
now_s() { date +%s; }

write_claude() { # write_claude EXPIRES_MS [refresh] [mode]
  python3 -I - "$CLAUDE_AUTH" "$1" "${2-$SECRET_REFRESH}" <<'PY'
import json, sys
json.dump({"claudeAiOauth": {"accessToken": "fake-access-token-SECRET-aaaa", "refreshToken": sys.argv[3], "expiresAt": int(sys.argv[2])}}, open(sys.argv[1], "w"))
PY
  chmod "${3:-600}" "$CLAUDE_AUTH"
}
write_codex() { # write_codex EXP_SECONDS [refresh] [mode]
  local tok; tok="$(jwt "$1")"
  python3 -I - "$CODEX_AUTH" "$tok" "${2-$SECRET_REFRESH}" <<'PY'
import json, sys
json.dump({"tokens": {"access_token": sys.argv[2], "refresh_token": sys.argv[3], "id_token": "fake-id"}}, open(sys.argv[1], "w"))
PY
  chmod "${3:-600}" "$CODEX_AUTH"
}

# make_stub NAME KIND: stub refresh executable. KIND: ok | fail | norefresh | badmode | noop
make_stub() {
  local stub="$TEST_ROOT/stub-$1.sh"
  cat >"$stub" <<STUB
#!/usr/bin/env bash
echo "$1" >>"$CALLS"
echo "stub output leaking $SECRET_ACCESS" 
case "$2" in
  ok) NEW_EXP=\$(( \$(date +%s) + 7200 )) ;;
  noop) exit 0 ;;
  fail) exit 3 ;;
  norefresh) NEW_EXP=\$(( \$(date +%s) + 7200 )) ;;
  badmode) NEW_EXP=\$(( \$(date +%s) + 7200 )) ;;
esac
REFRESH="$SECRET_REFRESH"; [ "$2" = norefresh ] && REFRESH=""
if [ "$1" = claude ]; then
  python3 -I - "\$CLAUDE_AUTH" "\$((NEW_EXP*1000))" "\$REFRESH" <<'PY'
import json, sys
json.dump({"claudeAiOauth": {"accessToken": "new", "refreshToken": sys.argv[3], "expiresAt": int(sys.argv[2])}}, open(sys.argv[1], "w"))
PY
  CF="\$CLAUDE_AUTH"
else
  python3 -I - "\$CODEX_AUTH" "\$NEW_EXP" "\$REFRESH" <<'PY'
import base64, json, sys
b = lambda d: base64.urlsafe_b64encode(json.dumps(d).encode()).rstrip(b"=").decode()
t = b({"alg": "none"}) + "." + b({"exp": int(sys.argv[2])}) + ".sig"
json.dump({"tokens": {"access_token": t, "refresh_token": sys.argv[3]}}, open(sys.argv[1], "w"))
PY
  CF="\$CODEX_AUTH"
fi
if [ "$2" = badmode ]; then chmod 644 "\$CF"; else chmod 600 "\$CF"; fi
STUB
  chmod 700 "$stub"
  echo "$stub"
}

export CLAUDE_AUTH="$TEST_ROOT/claude-credentials.json"
export CODEX_AUTH="$TEST_ROOT/codex-auth.json"
export DUCKWAY_LIVE_CREDENTIALS_DIR="$TEST_ROOT"
unset DUCKWAY_LIVE_PREFLIGHT DUCKWAY_PREFLIGHT_ACTIVE

# run_case NAME EXPECT_RC EXPECT_CALLS(0|1) STUB_KIND PROVIDER...
run_case() {
  local name="$1" want_rc="$2" want_calls="$3" kind="$4"; shift 4
  : >"$CALLS"
  local out rc=0
  out="$(DUCKWAY_PREFLIGHT_REFRESH_CLAUDE_CMD="$(make_stub claude "$kind")" \
         DUCKWAY_PREFLIGHT_REFRESH_CODEX_CMD="$(make_stub codex "$kind")" \
         "$PREFLIGHT" "$@" 2>&1)" || rc=$?
  local calls; calls="$(wc -l <"$CALLS" | tr -d ' ')"
  [ "$rc" -eq "$want_rc" ] || fail "$name: exit $rc, wanted $want_rc; output: $out"
  if [ "$want_calls" = 1 ]; then [ "$calls" -ge 1 ] || fail "$name: refresh not called"
  else [ "$calls" -eq 0 ] || fail "$name: refresh unexpectedly called"; fi
  case "$out" in
    *SECRET*) fail "$name: output leaked token material: $out" ;;
  esac
  echo "ok - $name"
}

HOUR_MS=3600000
now_ms() { echo $(( $(now_s) * 1000 )); }

write_claude $(( $(now_ms) + 2 * HOUR_MS ))
run_case "claude valid: no refresh" 0 0 ok claude
write_codex $(( $(now_s) + 7200 ))
run_case "codex valid: no refresh" 0 0 ok codex
run_case "both valid, default providers" 0 0 ok

write_claude $(( $(now_ms) + 600 * 1000 ))
run_case "claude near-expiry: refresh" 0 1 ok claude
write_codex $(( $(now_s) + 600 ))
run_case "codex near-expiry: refresh" 0 1 ok codex

write_claude $(( $(now_ms) - 1000 ))
run_case "claude expired: refresh" 0 1 ok claude
write_codex $(( $(now_s) - 100 ))
run_case "codex expired: refresh" 0 1 ok codex

# Margin override: 3-hour margin makes a 2-hour token near-expiry.
write_claude $(( $(now_ms) + 2 * HOUR_MS ))
: >"$CALLS"
DUCKWAY_LIVE_PREFLIGHT_MARGIN_SECS=10800 DUCKWAY_PREFLIGHT_REFRESH_CLAUDE_CMD="$(make_stub claude ok)" "$PREFLIGHT" claude >/dev/null 2>&1 \
  || fail "margin override run failed"
[ "$(wc -l <"$CALLS" | tr -d ' ')" -eq 1 ] || fail "margin override did not trigger refresh"
echo "ok - margin override"

write_claude $(( $(now_ms) - 1000 ))
run_case "refresh command fails" 1 1 fail claude
write_claude $(( $(now_ms) - 1000 ))
run_case "refresh succeeds but leaves token expired" 1 1 noop claude
write_codex $(( $(now_s) - 100 ))
run_case "codex refresh command fails" 1 1 fail codex
write_claude $(( $(now_ms) - 1000 ))
run_case "refreshed file lacks refresh token" 1 1 norefresh claude
write_codex $(( $(now_s) - 100 ))
run_case "codex refreshed file lacks refresh token" 1 1 norefresh codex
write_claude $(( $(now_ms) - 1000 ))
run_case "refreshed file mode is not 0600" 1 1 badmode claude

write_claude $(( $(now_ms) - 1000 )) "" 600
run_case "expired and no refresh token: no refresh attempted" 1 0 ok claude
write_claude $(( $(now_ms) + 2 * HOUR_MS )) "$SECRET_REFRESH" 644
run_case "wrong mode before: rejected" 1 0 ok claude
rm -f "$CLAUDE_AUTH"
run_case "missing file" 1 0 ok claude
run_case "unknown provider" 2 0 ok bogus

write_claude $(( $(now_ms) - 1000 ))
: >"$CALLS"
out="$(DUCKWAY_LIVE_PREFLIGHT=skip "$PREFLIGHT" claude 2>&1)" || fail "explicit skip failed"
case "$out" in *SKIPPED*) ;; *) fail "skip did not announce itself" ;; esac
[ ! -s "$CALLS" ] || fail "skip called refresh"
echo "ok - explicit skip"

echo "live credential preflight regression checks passed"
