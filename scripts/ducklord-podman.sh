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
DUCKWAY_VERSION="${DUCKWAY_VERSION:-}"
if [ -z "$DUCKWAY_VERSION" ]; then
  DUCKWAY_VERSION="$(git -C "$ROOT" describe --always --dirty 2>/dev/null || printf podman)"
fi

extra_run_args=()
ducklord_args=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --podman-arg | --podman-opt | --podman-run-arg)
      if [ "$#" -lt 2 ]; then
        echo "[ducklord-podman] missing value for $1" >&2
        exit 2
      fi
      extra_run_args+=("$2")
      shift 2
      ;;
    --podman-volume)
      if [ "$#" -lt 2 ]; then
        echo "[ducklord-podman] missing value for $1" >&2
        exit 2
      fi
      extra_run_args+=(-v "$2")
      shift 2
      ;;
    --podman-env)
      if [ "$#" -lt 2 ]; then
        echo "[ducklord-podman] missing value for $1" >&2
        exit 2
      fi
      extra_run_args+=(-e "$2")
      shift 2
      ;;
    --)
      shift
      ducklord_args=("$@")
      break
      ;;
    *)
      ducklord_args=("$@")
      break
      ;;
  esac
done

if [ "${#ducklord_args[@]}" -eq 0 ]; then
  ducklord_args=(tui --config /home/ducklord/.ducklord/config.json)
fi

mkdir -p "$WORK" "$DUCKLORD_DIR"

cat >"$WORK/Containerfile" <<'EOF'
FROM docker.io/library/golang:1.25-alpine AS build
ARG DUCKWAY_VERSION=podman
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -buildvcs=false \
  -ldflags="-s -w -X github.com/hackerduck/duckway/internal/version.Embedded=${DUCKWAY_VERSION}" \
  -o /out/ducklord ./cmd/ducklord

FROM docker.io/library/alpine:3.21
RUN apk add --no-cache openssh-client bash ca-certificates ncurses su-exec shadow
RUN mkdir -p /home/ducklord/.ssh /home/ducklord/.ducklord
RUN printf '%s\n' \
  '#!/bin/sh' \
  'set -eu' \
  '' \
  'uid="${DUCKLORD_UID:-1000}"' \
  'gid="${DUCKLORD_GID:-1000}"' \
  'group="ducklord"' \
  'user="ducklord"' \
  '' \
  'if ! getent group "$gid" >/dev/null 2>&1; then' \
  '  addgroup -g "$gid" -S "$group" >/dev/null' \
  'fi' \
  'if ! getent passwd "$uid" >/dev/null 2>&1; then' \
  '  adduser -S -D -H -u "$uid" -G "$(getent group "$gid" | cut -d: -f1)" -h /home/ducklord "$user" >/dev/null' \
  'fi' \
  '' \
  'mkdir -p /home/ducklord' \
  'chown "$uid:$gid" /home/ducklord' \
  'export HOME=/home/ducklord' \
  'exec su-exec "$uid:$gid" /usr/local/bin/ducklord "$@"' \
  > /usr/local/bin/ducklord-entrypoint && chmod +x /usr/local/bin/ducklord-entrypoint
COPY --from=build /out/ducklord /usr/local/bin/ducklord
ENTRYPOINT ["ducklord-entrypoint"]
EOF

echo "[ducklord-podman] building image with $RUNTIME"
"$RUNTIME" build --build-arg "DUCKWAY_VERSION=$DUCKWAY_VERSION" -t "$IMAGE" -f "$WORK/Containerfile" "$ROOT" >/dev/null

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
run_args+=("${extra_run_args[@]}")

echo "[ducklord-podman] starting ducklord"
exec "$RUNTIME" run "${run_args[@]}" "$IMAGE" "${ducklord_args[@]}"
