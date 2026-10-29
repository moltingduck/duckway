#!/usr/bin/env bash
# A foreground, disposable plugin-admin demo. No production config or credentials.
set -euo pipefail
umask 077
repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
owner=${DUCKWAY_PLUGIN_DEMO_OWNER:-duckllo-plugin-demo-$(date +%s)-$$}
port=${DUCKWAY_PLUGIN_DEMO_PORT:-18880}
[[ "$owner" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || { echo 'Invalid demo owner' >&2; exit 1; }
[[ "$port" =~ ^[0-9]{1,5}$ ]] && ((10#$port > 0 && 10#$port <= 65535)) || { echo 'Invalid demo port' >&2; exit 1; }
command -v podman >/dev/null
command -v curl >/dev/null
for name in "$owner-build" "$owner-server"; do
 if podman container exists "$name"; then echo "Demo container already exists: $name" >&2; exit 1; fi
done
root="$repo_dir/tmp/$owner"
mkdir -p -- "$repo_dir/tmp"
mkdir -- "$root"
cleanup() {
 trap - EXIT INT TERM
 podman rm -f "$owner-build" "$owner-server" >/dev/null 2>&1 || true
 rm -rf -- "$root"
}
trap cleanup EXIT INT TERM
mkdir -- "$root/bin" "$root/data"
image=docker.io/library/golang:1.26.10-bookworm@sha256:632dff7351799a5a9954a48dc06670ade72ee44e8c351cdc0e5a57af2fea21af
podman run --rm --name "$owner-build" --label "owner=$owner" -e GOTOOLCHAIN=local \
 -v "$repo_dir/go.mod:/src/go.mod:ro" -v "$repo_dir/go.sum:/src/go.sum:ro" \
 -v "$repo_dir/cmd/server:/src/cmd/server:ro" -v "$repo_dir/internal:/src/internal:ro" \
 -v "$repo_dir/web:/src/web:ro" -v "$repo_dir/skill:/src/skill:ro" \
 -v "$root/bin:/out:rw" -w /src "$image" go build -o /out/duckway-server ./cmd/server \
 >"$root/build.log" 2>&1
podman run --rm -d --name "$owner-server" --label "owner=$owner" \
 -p "127.0.0.1:$port:8080" -v "$root/bin:/bin-demo:ro" -v "$root/data:/data:rw" \
 -e DUCKWAY_PLUGIN_CHECK_INTERVAL=6h \
 -e "DUCKWAY_PLUGIN_ALLOWED_ORIGINS=${DUCKWAY_PLUGIN_ALLOWED_ORIGINS:-}" "$image" \
 /bin-demo/duckway-server --listen :8080 --data /data >/dev/null
ready=false
for ((attempt=0; attempt<80; attempt++)); do
 if curl --fail --silent "http://127.0.0.1:$port/healthz" >/dev/null; then ready=true; break; fi
 sleep 0.25
done
if [[ "$ready" != true ]]; then echo "Demo failed readiness; inspect podman logs $owner-server" >&2; exit 1; fi
printf 'Plugin admin demo: http://127.0.0.1:%s/admin/plugins\n' "$port"
printf 'Fresh random credentials: podman logs %s-server\n' "$owner"
printf 'Owner: %s. Stop with Ctrl-C; teardown removes its container and private data.\n' "$owner"
# Keep this process attached so the trap owns teardown, including build/setup failure.
while podman container exists "$owner-server"; do sleep 1; done
