#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/container-runtime.sh"
source "$ROOT/scripts/ducklord-demo-common.sh"
duckway_init_container_runtime
CODEX_AUTH="$ROOT/live-credentials/codex-auth.json"
CLAUDE_AUTH="$ROOT/live-credentials/claude-credentials.json"
WORK_PARENT="${WORK:-${TMPDIR:-/tmp}}"
RUNTIME="$CONTAINER_RUNTIME"
IMAGE="${IMAGE:-duckway-ducklord-demo:local}"
NET="${NET:-ducklord-demo}"
CREDENTIALS="${DUCKLORD_DEMO_AGENT_CREDENTIALS:-all}"
CREDENTIAL_CLIENTS="${DUCKLORD_DEMO_CREDENTIAL_CLIENTS:-all}"

case "$CREDENTIALS" in all|codex|claude|none) ;; *) echo "invalid DUCKLORD_DEMO_AGENT_CREDENTIALS=$CREDENTIALS" >&2; exit 2;; esac
case "$CREDENTIAL_CLIENTS" in all|client-a|client-b|client-c) ;; *) echo "invalid DUCKLORD_DEMO_CREDENTIAL_CLIENTS=$CREDENTIAL_CLIENTS" >&2; exit 2;; esac

if [ "${DUCKLORD_DEMO_LOCK_HELD:-0}" != 1 ]; then
  ducklord_lock_demo_topology
fi

[ -d "$WORK_PARENT" ] && [ ! -L "$WORK_PARENT" ] || { echo "demo work parent must be a non-symlink directory: $WORK_PARENT" >&2; exit 2; }
WORK="$(mktemp -d "$WORK_PARENT/ducklord-podman-demo.XXXXXX")"
cleanup_work() { rm -rf -- "$WORK"; }
trap cleanup_work EXIT

cleanup_existing() {
  "$RUNTIME" rm -f ducklord-dev ducklion-client-a ducklion-client-b ducklion-client-c >/dev/null 2>&1 || true
  "$RUNTIME" network rm "$NET" >/dev/null 2>&1 || true
}

echo "[ducklord-demo] building local binaries"
CGO_ENABLED=0 go build -o "$WORK/ducklord" "$ROOT/cmd/ducklord"
CGO_ENABLED=0 go build -o "$WORK/duckway" "$ROOT/cmd/client"
CGO_ENABLED=0 go build -o "$WORK/ducklion" "$ROOT/cmd/ducklion"

ssh-keygen -q -t ed25519 -N '' -f "$WORK/id_ed25519"

cat >"$WORK/Containerfile" <<'EOF'
FROM alpine:3.21
RUN apk add --no-cache openssh openssh-client bash ca-certificates ncurses nodejs npm \
 && npm install -g @openai/codex@0.153.4 @anthropic-ai/claude-code@2.1.263 \
 && npm cache clean --force
RUN adduser -D duck && echo "duck:duck-demo-password" | chpasswd && ssh-keygen -A
RUN install -d -m 700 /home/duck/.ssh /root/.ssh /root/.ducklord && chown -R duck:duck /home/duck/.ssh
COPY ducklord /usr/local/bin/ducklord
COPY duckway /usr/local/bin/duckway
COPY ducklion /usr/local/bin/ducklion
COPY id_ed25519.pub /home/duck/.ssh/authorized_keys
RUN chown duck:duck /home/duck/.ssh/authorized_keys && chmod 700 /home/duck/.ssh && chmod 600 /home/duck/.ssh/authorized_keys
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
EOF

echo "[ducklord-demo] building demo image with $RUNTIME"
"$RUNTIME" build -t "$IMAGE" -f "$WORK/Containerfile" "$WORK" >/dev/null

cleanup_existing
"$RUNTIME" network create "$NET" >/dev/null

echo "[ducklord-demo] starting remote clients"
"$RUNTIME" run -d --name ducklion-client-a --hostname client-a --network "$NET" "$IMAGE" >/dev/null
"$RUNTIME" run -d --name ducklion-client-b --hostname client-b --network "$NET" "$IMAGE" >/dev/null
"$RUNTIME" run -d --name ducklion-client-c --hostname client-c --network "$NET" "$IMAGE" >/dev/null
for container in ducklion-client-a ducklion-client-b ducklion-client-c; do
  "$RUNTIME" exec -d -u duck "$container" sh -lc 'mkdir -p $HOME/.duckway; nohup ducklion daemon >$HOME/.duckway/ducklion-daemon.log 2>&1 </dev/null & echo $! >$HOME/.duckway/ducklion-daemon.pid' >/dev/null
done
if [ "$CREDENTIALS" != none ] && { [ -f "$CODEX_AUTH" ] || [ -f "$CLAUDE_AUTH" ]; }; then
  echo "[ducklord-demo] injecting available local agent credentials into remote demo users"
  for container in ducklion-client-a ducklion-client-b ducklion-client-c; do
    if [ "$CREDENTIAL_CLIENTS" != all ] && [ "$CREDENTIAL_CLIENTS" != "${container#ducklion-}" ]; then
      continue
    fi
    "$RUNTIME" exec -u duck "$container" sh -lc 'mkdir -p $HOME/.codex $HOME/.claude && chmod 700 $HOME/.codex $HOME/.claude'
    if { [ "$CREDENTIALS" = all ] || [ "$CREDENTIALS" = codex ]; } && [ -f "$CODEX_AUTH" ]; then
      ducklord_require_demo_secret "$CODEX_AUTH"
      "$RUNTIME" cp "$CODEX_AUTH" "$container":/tmp/codex-auth.json
      "$RUNTIME" exec "$container" install -o duck -g duck -m 600 /tmp/codex-auth.json /home/duck/.codex/auth.json
      "$RUNTIME" exec "$container" rm -f /tmp/codex-auth.json
    fi
    if { [ "$CREDENTIALS" = all ] || [ "$CREDENTIALS" = claude ]; } && [ -f "$CLAUDE_AUTH" ]; then
      ducklord_require_demo_secret "$CLAUDE_AUTH"
      "$RUNTIME" cp "$CLAUDE_AUTH" "$container":/tmp/claude-credentials.json
      "$RUNTIME" exec "$container" install -o duck -g duck -m 600 /tmp/claude-credentials.json /home/duck/.claude/.credentials.json
      "$RUNTIME" exec "$container" rm -f /tmp/claude-credentials.json
    fi
  done
fi
for container in ducklion-client-a ducklion-client-b ducklion-client-c; do
  ready=false
  for _ in $(seq 1 100); do
    if "$RUNTIME" exec -u duck "$container" test -S /home/duck/.duckway/ducklion/ducklion.sock; then
      ready=true
      break
    fi
    sleep 0.05
  done
  if [ "$ready" != true ]; then
    echo "[ducklord-demo] Ducklion daemon did not become ready in $container" >&2
    exit 1
  fi
done

echo "[ducklord-demo] starting dev laptop"
"$RUNTIME" run -d --name ducklord-dev --hostname dev-laptop --network "$NET" "$IMAGE" sleep infinity >/dev/null
"$RUNTIME" cp "$WORK/id_ed25519" ducklord-dev:/root/.ssh/id_ed25519
"$RUNTIME" exec ducklord-dev sh -lc 'chmod 600 /root/.ssh/id_ed25519 && cat >/root/.ssh/config <<EOF
Host client-a client-b client-c
  User duck

Host *
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
EOF'
"$RUNTIME" exec ducklord-dev sh -lc 'umask 077; cat >/root/.ducklord/config.yaml <<EOF
name: dev-laptop
hosts:
  - name: client-a
    host: client-a
    user: duck
    group: lab
  - name: client-b
    host: client-b
    user: duck
    group: lab
EOF'

echo "[ducklord-demo] creating sample remote sessions"
"$RUNTIME" exec -u duck ducklion-client-a sh -lc 'mkdir -p /home/duck/projects/alpha && duckway projects add --name alpha-project /home/duck/projects/alpha' >/dev/null
"$RUNTIME" exec -u duck ducklion-client-b sh -lc 'mkdir -p /home/duck/projects/beta && duckway projects add --name beta-project /home/duck/projects/beta' >/dev/null
"$RUNTIME" exec -u duck ducklion-client-c sh -lc 'mkdir -p /home/duck/projects/gamma && duckway projects add --name gamma-project /home/duck/projects/gamma' >/dev/null
"$RUNTIME" exec ducklord-dev ducklord start client-a --name alpha --kind shell --cwd /home/duck -- bash >/dev/null
"$RUNTIME" exec ducklord-dev ducklord start client-a --name bash --kind shell --cwd /home/duck -- bash >/dev/null
"$RUNTIME" exec ducklord-dev ducklord start client-a --name build --kind shell --cwd /home/duck -- bash >/dev/null
"$RUNTIME" exec ducklord-dev ducklord start client-b --name beta --kind shell --cwd /home/duck -- bash >/dev/null
"$RUNTIME" exec ducklord-dev ducklord send client-a alpha 'i=0; while :; do i=$((i+1)); echo client-a alpha tick $i; sleep 4; done' >/dev/null
"$RUNTIME" exec ducklord-dev ducklord send client-a build 'i=0; while :; do i=$((i+1)); echo client-a build output $i; sleep 6; done' >/dev/null
"$RUNTIME" exec ducklord-dev ducklord send client-b beta 'i=0; while :; do i=$((i+1)); echo client-b beta tick $i; sleep 5; done' >/dev/null

echo "[ducklord-demo] verifying daemon inventory, PTY input, and recovery"
if ! "$RUNTIME" exec ducklord-dev ducklord agents client-a /home/duck --config /root/.ducklord/config.yaml | grep -q '^shell'; then
  echo "[ducklord-demo] remote agent discovery did not report the interactive shell" >&2
  exit 1
fi
sessions="$($RUNTIME exec ducklord-dev ducklord sessions client-a --config /root/.ducklord/config.yaml)"
grep -q 'alpha.*running' <<<"$sessions"
grep -q 'bash.*running' <<<"$sessions"
grep -q 'build.*running' <<<"$sessions"
"$RUNTIME" exec ducklord-dev ducklord send client-a bash 'printf ducklord-e2e-ready' --config /root/.ducklord/config.yaml >/dev/null
ready=false
for _ in $(seq 1 50); do
  if "$RUNTIME" exec ducklord-dev ducklord read client-a bash --lines 20 --config /root/.ducklord/config.yaml | grep -q ducklord-e2e-ready; then
    ready=true
    break
  fi
  sleep 0.05
done
if [ "$ready" != true ]; then
  echo "[ducklord-demo] PTY input/output assertion failed" >&2
  exit 1
fi
"$RUNTIME" exec -u duck ducklion-client-a sh -lc 'kill "$(cat $HOME/.duckway/ducklion-daemon.pid)"'
"$RUNTIME" exec -d -u duck ducklion-client-a sh -lc 'mkdir -p $HOME/.duckway; nohup ducklion daemon >$HOME/.duckway/ducklion-daemon.log 2>&1 </dev/null & echo $! >$HOME/.duckway/ducklion-daemon.pid' >/dev/null
recovered=false
for _ in $(seq 1 100); do
  if "$RUNTIME" exec ducklord-dev ducklord sessions client-a --config /root/.ducklord/config.yaml 2>/dev/null | grep -q 'bash.*running'; then
    recovered=true
    break
  fi
  sleep 0.05
done
if [ "$recovered" != true ]; then
  echo "[ducklord-demo] daemon restart recovery assertion failed" >&2
  exit 1
fi

echo "[ducklord-demo] verifying native shell lifecycle through SSH bridge"
"$RUNTIME" exec ducklord-dev ducklord start client-a --name lifecycle --kind shell --cwd /home/duck -- bash >/dev/null
restart_output="$("$RUNTIME" exec ducklord-dev ducklord restart client-a lifecycle --config /root/.ducklord/config.yaml)"
grep -q 'Restarted .* (generation 2)' <<<"$restart_output"
if force_output="$("$RUNTIME" exec ducklord-dev ducklord restart client-a lifecycle --force --config /root/.ducklord/config.yaml 2>&1)"; then
  echo "[ducklord-demo] shell restart unexpectedly accepted --force" >&2
  exit 1
fi
grep -q 'immediate.*do not accept wait or force' <<<"$force_output"
"$RUNTIME" exec ducklord-dev ducklord send client-a lifecycle 'printf "ducklord-lifecycle-generation-2\\n"' --config /root/.ducklord/config.yaml >/dev/null
lifecycle_ready=false
for _ in $(seq 1 50); do
  if "$RUNTIME" exec ducklord-dev ducklord read client-a lifecycle --lines 20 --config /root/.ducklord/config.yaml | grep -q ducklord-lifecycle-generation-2; then
    lifecycle_ready=true
    break
  fi
  sleep 0.05
done
if [ "$lifecycle_ready" != true ]; then
  echo "[ducklord-demo] restarted shell did not accept PTY input" >&2
  exit 1
fi
"$RUNTIME" exec ducklord-dev ducklord end client-a lifecycle --config /root/.ducklord/config.yaml | grep -q 'Ended '
"$RUNTIME" exec ducklord-dev ducklord sessions client-a --config /root/.ducklord/config.yaml | grep -q 'lifecycle.*stopped.*shell'
if ! "$RUNTIME" exec ducklord-dev ducklord read client-a lifecycle --lines 20 --config /root/.ducklord/config.yaml | grep -q ducklord-lifecycle-generation-2; then
  echo "[ducklord-demo] stopped session did not expose retained generation-2 output" >&2
  exit 1
fi
if ! "$RUNTIME" exec -u duck ducklion-client-a sh -lc 'find "$HOME/.duckway/ducklion/sessions" -name "output.*" -type f -exec stat -c %a {} \; | grep -q . && ! find "$HOME/.duckway/ducklion/sessions" -name "output.*" -type f -exec stat -c %a {} \; | grep -vx 600'; then
  echo "[ducklord-demo] retained output files are missing or not mode 0600" >&2
  exit 1
fi
"$RUNTIME" exec ducklord-dev ducklord destroy client-a lifecycle --config /root/.ducklord/config.yaml | grep -q 'Destroyed '
if "$RUNTIME" exec ducklord-dev ducklord sessions client-a --config /root/.ducklord/config.yaml | grep -q 'lifecycle'; then
  echo "[ducklord-demo] destroyed shell remains in inventory" >&2
  exit 1
fi

cat <<EOF
[ducklord-demo] ready

Open the dev laptop TUI:
  $RUNTIME exec -it ducklord-dev ducklord tui --config /root/.ducklord/config.yaml

Useful checks:
  $RUNTIME exec ducklord-dev ducklord clients --config /root/.ducklord/config.yaml
  $RUNTIME exec ducklord-dev ducklord ssh-hosts
  $RUNTIME exec ducklord-dev ducklord probe client-a --config /root/.ducklord/config.yaml
  $RUNTIME exec ducklord-dev ducklord sessions client-a --config /root/.ducklord/config.yaml
  $RUNTIME exec ducklord-dev ducklord projects client-a --config /root/.ducklord/config.yaml
  $RUNTIME exec ducklord-dev ducklord read client-a alpha --lines 20 --config /root/.ducklord/config.yaml
  $RUNTIME exec -it ducklord-dev ducklord attach-host client-a --config /root/.ducklord/config.yaml

Inside the TUI:
  j/k or arrow keys: move
  mouse click: select a session row
  Enter or right-click: focus the selected session in the right pane
  a: add a ducklion host from ~/.ssh/config (try client-c)
  c: create a session: agent -> host -> project -> agent -> handle, or shell -> host -> project -> handle
  m: open the centered action menu for attach, yield, notifications, and lifecycle
  n: configure notifications for the selected session
  E / R / X: end, restart, or destroy the selected session (with confirmation)
  attach-host mode: same split-pane attach UI scoped to one host; add/new are disabled
  right pane: selected session output preview
  Ctrl-]: return keyboard focus to the left menu
  q: quit

Clean up:
  $RUNTIME rm -f ducklord-dev ducklion-client-a ducklion-client-b ducklion-client-c
  $RUNTIME network rm $NET
EOF
