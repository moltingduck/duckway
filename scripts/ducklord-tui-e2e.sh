#!/usr/bin/env bash
# Real-terminal Ducklord create E2E. The fixture is credential-free and drives
# TUI input through a PTY into the SSH stdio bridge and a native Ducklion PTY.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/container-runtime.sh"
source "$ROOT/scripts/ducklord-demo-common.sh"
duckway_init_container_runtime
RUNTIME="$CONTAINER_RUNTIME"
SETUP_LOG="$(mktemp -t ducklord-tui-e2e-XXXXXX.log)"
if [ "${DUCKLORD_DEMO_LOCK_HELD:-0}" != 1 ]; then
	rm -f "$SETUP_LOG"
	ducklord_reexec_with_demo_lock "$0" "$@"
fi

for resource in ducklord-dev ducklion-client-a ducklion-client-b ducklion-client-c; do
	if "$RUNTIME" container inspect "$resource" >/dev/null 2>&1; then
		echo "[ducklord-tui-e2e] refusing to replace existing container $resource" >&2
		rm -f "$SETUP_LOG"
		exit 1
	fi
done
if "$RUNTIME" network inspect ducklord-demo >/dev/null 2>&1; then
	echo "[ducklord-tui-e2e] refusing to replace existing network ducklord-demo" >&2
	rm -f "$SETUP_LOG"
	exit 1
fi

cleanup() {
	local status=$?
	trap - EXIT INT TERM
  "$RUNTIME" rm -f ducklord-dev ducklion-client-a ducklion-client-b ducklion-client-c >/dev/null 2>&1 || true
  "$RUNTIME" network rm ducklord-demo >/dev/null 2>&1 || true
	if [ "$status" -ne 0 ]; then
		echo "[ducklord-tui-e2e] setup tail:" >&2
		tail -n 40 "$SETUP_LOG" >&2 || true
	fi
	rm -f "$SETUP_LOG"
	exit "$status"
}
trap cleanup EXIT INT TERM

echo "[ducklord-tui-e2e] preparing credential-free Ducklion topology with $RUNTIME"
DUCKLORD_DEMO_LOCK_HELD=1 DUCKLORD_DEMO_AGENT_CREDENTIALS=none DUCKLORD_DEMO_SHELL_ONLY=1 \
CONTAINER_RUNTIME="$RUNTIME" "$ROOT/scripts/ducklord-podman-demo.sh" >"$SETUP_LOG"
"$RUNTIME" exec ducklord-dev sh -lc "sed 's/^name: .*/name: e2e-inspector/' /root/.ducklord/config.yaml >/tmp/e2e-inspector.yaml"

echo "[ducklord-tui-e2e] driving create modal, SSH bridge, and remote PTY"
DUCKLORD_TUI_CONTAINER_E2E=1 \
DUCKLORD_E2E_RUNTIME="$RUNTIME" \
DUCKLORD_E2E_CONTROLLER=ducklord-dev \
go test -count=1 ./cmd/ducklord -run '^TestDucklordCreateTUIContainerE2E$' -v

echo "[ducklord-tui-e2e] PASS"
