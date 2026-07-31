#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="${WORK:-/tmp/ducklord-podman-demo}"
RUNTIME="${CONTAINER_RUNTIME:-podman}"
IMAGE="${IMAGE:-duckway-ducklord-demo:local}"
NET="${NET:-ducklord-demo}"

cleanup_existing() {
  "$RUNTIME" rm -f ducklord-dev ducklion-client-a ducklion-client-b >/dev/null 2>&1 || true
  "$RUNTIME" network rm "$NET" >/dev/null 2>&1 || true
}

mkdir -p "$WORK"
rm -rf "$WORK"/*

echo "[ducklord-demo] building local binaries"
go build -o "$WORK/ducklord" "$ROOT/cmd/ducklord"
go build -o "$WORK/ducklion" "$ROOT/cmd/ducklion"

ssh-keygen -q -t ed25519 -N '' -f "$WORK/id_ed25519"

cat >"$WORK/Containerfile" <<'EOF'
FROM alpine:3.21
RUN apk add --no-cache openssh openssh-client bash ca-certificates ncurses
RUN adduser -D duck && echo "duck:duck-demo-password" | chpasswd && ssh-keygen -A
RUN mkdir -p /home/duck/.ssh /root/.ssh /root/.ducklord && chown -R duck:duck /home/duck/.ssh
COPY ducklord /usr/local/bin/ducklord
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

echo "[ducklord-demo] starting dev laptop"
"$RUNTIME" run -d --name ducklord-dev --hostname dev-laptop --network "$NET" "$IMAGE" sleep infinity >/dev/null
"$RUNTIME" cp "$WORK/id_ed25519" ducklord-dev:/root/.ssh/id_ed25519
"$RUNTIME" exec ducklord-dev sh -lc 'chmod 600 /root/.ssh/id_ed25519 && cat >/root/.ssh/config <<EOF
Host *
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
EOF'
"$RUNTIME" exec ducklord-dev sh -lc 'cat >/root/.ducklord/config.json <<EOF
{
  "clients": [
    {"name":"client-a","host":"client-a","user":"duck","group":"lab"},
    {"name":"client-b","host":"client-b","user":"duck","group":"lab"}
  ]
}
EOF'

echo "[ducklord-demo] creating sample remote sessions"
"$RUNTIME" exec -u duck ducklion-client-a ducklion start --name alpha --agent shell --cwd /home/duck -- sh -lc 'i=0; while :; do i=$((i+1)); echo client-a alpha tick $i; if [ $((i % 3)) -eq 0 ]; then echo "[ducklion:done] alpha step $i"; fi; sleep 4; done' >/dev/null
"$RUNTIME" exec -u duck ducklion-client-a ducklion start --name bash --agent shell --cwd /home/duck -- bash >/dev/null
"$RUNTIME" exec -u duck ducklion-client-a ducklion start --name build --agent shell --cwd /home/duck -- sh -lc 'i=0; while :; do i=$((i+1)); echo client-a build output $i; sleep 6; done' >/dev/null
"$RUNTIME" exec -u duck ducklion-client-b ducklion start --name beta --agent shell --cwd /home/duck -- sh -lc 'i=0; while :; do i=$((i+1)); echo client-b beta tick $i; if [ $((i % 2)) -eq 0 ]; then echo "[ducklion:done] beta step $i"; fi; sleep 5; done' >/dev/null

cat <<EOF
[ducklord-demo] ready

Open the dev laptop TUI:
  $RUNTIME exec -it ducklord-dev ducklord tui --config /root/.ducklord/config.json

Useful checks:
  $RUNTIME exec ducklord-dev ducklord clients --config /root/.ducklord/config.json
  $RUNTIME exec ducklord-dev ducklord sessions client-a --config /root/.ducklord/config.json
  $RUNTIME exec ducklord-dev ducklord read client-a alpha --lines 20 --config /root/.ducklord/config.json

Inside the TUI:
  j/k or arrow keys: move
  mouse click: select a session row
  Enter or right-click: focus the selected session in the right pane
  n: create a new remote session on the selected client (example: scratch -- bash)
  right pane: selected session output preview
  Ctrl-]: return keyboard focus to the left menu
  q: quit

Clean up:
  $RUNTIME rm -f ducklord-dev ducklion-client-a ducklion-client-b
  $RUNTIME network rm $NET
EOF
