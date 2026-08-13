#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="${WORK:-/tmp/ducklord-podman}"
RUNTIME="${CONTAINER_RUNTIME:-podman}"
IMAGE="${IMAGE:-duckway-ducklord:local}"
NAME="${NAME:-ducklord}"
SSH_DIR="${SSH_DIR:-$HOME/.ssh}"
DUCKLORD_DIR="${DUCKLORD_DIR:-$HOME/.ducklord}"
NETWORK_ARGS="${DUCKLORD_PODMAN_NETWORK_ARGS:---network host}"
SSH_MOUNT_MODE="${DUCKLORD_PODMAN_SSH_MOUNT:-rw}"
RUN_UID="${DUCKLORD_PODMAN_UID:-$(id -u)}"
RUN_GID="${DUCKLORD_PODMAN_GID:-$(id -g)}"

if [ "$#" -eq 0 ]; then
  set -- tui --config /home/ducklord/.ducklord/config.json
fi

mkdir -p "$WORK" "$DUCKLORD_DIR"

echo "[ducklord-podman] building local ducklord"
go build -o "$WORK/ducklord" "$ROOT/cmd/ducklord"

cat >"$WORK/ducklord-entrypoint" <<'EOF'
#!/bin/sh
set -eu

uid="${DUCKLORD_UID:-1000}"
gid="${DUCKLORD_GID:-1000}"
group="ducklord"
user="ducklord"

if ! getent group "$gid" >/dev/null 2>&1; then
  addgroup -g "$gid" -S "$group" >/dev/null
fi
if ! getent passwd "$uid" >/dev/null 2>&1; then
  adduser -S -D -H -u "$uid" -G "$(getent group "$gid" | cut -d: -f1)" -h /home/ducklord "$user" >/dev/null
fi

mkdir -p /home/ducklord
chown "$uid:$gid" /home/ducklord
export HOME=/home/ducklord
exec su-exec "$uid:$gid" /usr/local/bin/ducklord "$@"
EOF
chmod +x "$WORK/ducklord-entrypoint"

cat >"$WORK/Containerfile" <<'EOF'
FROM alpine:3.21
RUN apk add --no-cache openssh-client bash ca-certificates ncurses su-exec shadow
RUN mkdir -p /home/ducklord/.ssh /home/ducklord/.ducklord
COPY ducklord /usr/local/bin/ducklord
COPY ducklord-entrypoint /usr/local/bin/ducklord-entrypoint
ENTRYPOINT ["ducklord-entrypoint"]
EOF

echo "[ducklord-podman] building image with $RUNTIME"
"$RUNTIME" build -t "$IMAGE" -f "$WORK/Containerfile" "$WORK" >/dev/null

run_args=(
  --rm
  --name "$NAME"
  -e HOME=/home/ducklord
  -e "DUCKLORD_UID=$RUN_UID"
  -e "DUCKLORD_GID=$RUN_GID"
  -w /home/ducklord
)
if [ -t 0 ] && [ -t 1 ]; then
  run_args+=(-it)
fi

# shellcheck disable=SC2206
network_parts=($NETWORK_ARGS)
run_args+=("${network_parts[@]}")

if [ -d "$SSH_DIR" ]; then
  run_args+=(-v "$SSH_DIR:/home/ducklord/.ssh:$SSH_MOUNT_MODE")
else
  echo "[ducklord-podman] warning: SSH_DIR does not exist: $SSH_DIR" >&2
fi
run_args+=(-v "$DUCKLORD_DIR:/home/ducklord/.ducklord:rw")

if [ -n "${SSH_AUTH_SOCK:-}" ] && [ -S "$SSH_AUTH_SOCK" ]; then
  sock_dir="$(dirname "$SSH_AUTH_SOCK")"
  run_args+=(-v "$sock_dir:$sock_dir:rw" -e "SSH_AUTH_SOCK=$SSH_AUTH_SOCK")
fi

echo "[ducklord-podman] starting ducklord"
exec "$RUNTIME" run "${run_args[@]}" "$IMAGE" "$@"
