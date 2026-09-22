#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d -t ducklord-demo-common-test-XXXXXX)"
trap 'rm -rf "$TMP"' EXIT
export XDG_RUNTIME_DIR="$TMP/run"
source "$ROOT/scripts/ducklord-demo-common.sh"

DRIVER="$TMP/driver.sh"
printf '%s\n' '#!/usr/bin/env bash' 'set -euo pipefail' \
  'source "$ROOT/scripts/ducklord-demo-common.sh"' \
  'if [ "${DUCKLORD_DEMO_LOCK_HELD:-0}" != 1 ]; then ducklord_reexec_with_demo_lock "$0"; fi' \
  'nohup sleep 2 >/dev/null 2>&1 &' >"$DRIVER"
chmod 700 "$DRIVER"
ROOT="$ROOT" "$DRIVER"

LOCK="$(ducklord_demo_lock_file)"
flock -n --close "$LOCK" true || {
  echo "demo lock leaked into a background child" >&2
  exit 1
}

HELD="$TMP/held"
flock "$LOCK" sh -c 'touch "$1"; sleep 1' sh "$HELD" &
HOLDER=$!
for _ in $(seq 1 100); do [ -e "$HELD" ] && break; sleep .01; done
if flock -n --close "$LOCK" true; then
  echo "a second demo lock holder was accepted" >&2
  exit 1
fi
wait "$HOLDER"

unlink "$LOCK"
ln -s "$TMP/symlink-target" "$LOCK"
if ROOT="$ROOT" XDG_RUNTIME_DIR="$XDG_RUNTIME_DIR" bash -c \
  'source "$ROOT/scripts/ducklord-demo-common.sh"; ducklord_reexec_with_demo_lock true' >/dev/null 2>&1; then
  echo "symlink demo lock was accepted" >&2
  exit 1
fi
unlink "$LOCK"

SECRET="$TMP/credential.json"
printf '{}\n' >"$SECRET"
chmod 644 "$SECRET"
if ducklord_require_demo_secret "$SECRET" >/dev/null 2>&1; then
  echo "wide credential permissions were accepted" >&2
  exit 1
fi
chmod 600 "$SECRET"
ducklord_require_demo_secret "$SECRET"

# The container E2E must derive every cleanup target from its isolated names.
TUI_E2E="$ROOT/scripts/ducklord-tui-e2e.sh"
grep -F 'NAME_PREFIX="${DUCKLORD_TUI_E2E_NAME_PREFIX:-ducklord-tui-e2e-${BASHPID}}"' "$TUI_E2E" >/dev/null
grep -F 'for resource in "$DEV_CONTAINER" "$CLIENT_A" "$CLIENT_B" "$CLIENT_C" "$CLIENT_D"' "$TUI_E2E" >/dev/null
grep -F 'container inspect -f' "$TUI_E2E" >/dev/null
grep -F '"$RUNTIME" rm -f "$resource"' "$TUI_E2E" >/dev/null
grep -F 'network inspect -f' "$TUI_E2E" >/dev/null
grep -F '"$OWNER_TOKEN"' "$TUI_E2E" >/dev/null
grep -F '"$RUNTIME" network rm "$NET"' "$TUI_E2E" >/dev/null
DEMO_SCRIPT="$ROOT/scripts/ducklord-podman-demo.sh"
grep -F 'NAME_PREFIX="${DUCKLORD_DEMO_NAME_PREFIX:-}"' "$DEMO_SCRIPT" >/dev/null
grep -F 'NET="${NET:-${NAME_PREFIX}ducklord-demo}"' "$DEMO_SCRIPT" >/dev/null
grep -F -- '--require-owned-network' "$DEMO_SCRIPT" "$TUI_E2E" >/dev/null
grep -F 'NET_WAS_SUPPLIED=0' "$DEMO_SCRIPT" >/dev/null
grep -F 'network inspect -f' "$DEMO_SCRIPT" >/dev/null

echo "[ducklord-demo-common] PASS"
