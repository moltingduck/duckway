#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="${WORK:-/tmp/ducklord-podman-demo}"
RUNTIME="${CONTAINER_RUNTIME:-podman}"
IMAGE="${IMAGE:-duckway-ducklord-demo:local}"
NET="${NET:-ducklord-demo}"

cleanup_existing() {
  "$RUNTIME" rm -f ducklord-dev ducklion-client-a ducklion-client-b ducklion-client-c >/dev/null 2>&1 || true
  "$RUNTIME" network rm "$NET" >/dev/null 2>&1 || true
}

mkdir -p "$WORK"
rm -rf "$WORK"/*

echo "[ducklord-demo] building local binaries"
CGO_ENABLED=0 go build -o "$WORK/ducklord" "$ROOT/cmd/ducklord"
CGO_ENABLED=0 go build -o "$WORK/duckway" "$ROOT/cmd/client"
CGO_ENABLED=0 go build -o "$WORK/ducklion" "$ROOT/cmd/ducklion"

ssh-keygen -q -t ed25519 -N '' -f "$WORK/id_ed25519"

cat >"$WORK/Containerfile" <<'EOF'
FROM alpine:3.21
RUN apk add --no-cache openssh openssh-client bash ca-certificates ncurses
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
"$RUNTIME" exec ducklord-dev ducklord start client-a --name alpha --agent shell --cwd /home/duck -- sh -lc 'i=0; while :; do i=$((i+1)); echo client-a alpha tick $i; if [ $((i % 3)) -eq 0 ]; then echo "[ducklion:done] alpha step $i"; fi; sleep 4; done' >/dev/null
"$RUNTIME" exec ducklord-dev ducklord start client-a --name bash --agent shell --cwd /home/duck -- bash >/dev/null
"$RUNTIME" exec ducklord-dev ducklord start client-a --name build --agent shell --cwd /home/duck -- sh -lc 'i=0; while :; do i=$((i+1)); echo client-a build output $i; sleep 6; done' >/dev/null
"$RUNTIME" exec ducklord-dev ducklord start client-b --name beta --agent shell --cwd /home/duck -- sh -lc 'i=0; while :; do i=$((i+1)); echo client-b beta tick $i; if [ $((i % 2)) -eq 0 ]; then echo "[ducklion:done] beta step $i"; fi; sleep 5; done' >/dev/null

echo "[ducklord-demo] verifying daemon inventory, PTY input, and recovery"
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
  n: create a remote session: choose agent -> host -> project
  attach-host mode: same split-pane attach UI scoped to one host; add/new are disabled
  right pane: selected session output preview
  Ctrl-]: return keyboard focus to the left menu
  q: quit

Clean up:
  $RUNTIME rm -f ducklord-dev ducklion-client-a ducklion-client-b ducklion-client-c
  $RUNTIME network rm $NET
EOF
