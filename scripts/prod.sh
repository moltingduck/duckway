#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
. "$SCRIPT_DIR/container-runtime.sh"

cd "$PROJECT_DIR"

# Load .prod.env
if [ ! -f "$PROJECT_DIR/.prod.env" ]; then
  echo "Error: .prod.env not found. Copy .env.example to .prod.env and fill in values."
  echo "  cp .env.example .prod.env"
  exit 1
fi

set -a
. "$PROJECT_DIR/.prod.env"
set +a

MODE="${DUCKWAY_PROD_MODE:-split}"
USE_TAILSCALE="${DUCKWAY_TAILSCALE:-true}"
TS_HOSTNAME="${TS_HOSTNAME:-duckway}"
export TS_HOSTNAME

if [ "$USE_TAILSCALE" = "true" ]; then
  case "$MODE" in
    split)
      if { [ -z "${TS_AUTHKEY_ADMIN:-}" ] || [ -z "${TS_AUTHKEY_GATEWAY:-}" ]; } && [ -z "${TS_AUTHKEY:-}" ]; then
        echo "Error: set both TS_AUTHKEY_ADMIN and TS_AUTHKEY_GATEWAY, or set fallback TS_AUTHKEY, in .prod.env"
        exit 1
      fi
      PROFILES="--profile tailscale"
      ;;
    combined)
      if [ -z "${TS_AUTHKEY_SERVER:-}" ] && [ -z "${TS_AUTHKEY:-}" ]; then
        echo "Error: set TS_AUTHKEY_SERVER or fallback TS_AUTHKEY in .prod.env"
        exit 1
      fi
      PROFILES="--profile tailscale-combined"
      ;;
    *) echo "Error: DUCKWAY_PROD_MODE must be 'split' or 'combined'"; exit 1 ;;
  esac
else
  case "$MODE" in
    split)    PROFILES="--profile prod-split" ;;
    combined) PROFILES="--profile prod" ;;
    *) echo "Error: DUCKWAY_PROD_MODE must be 'split' or 'combined'"; exit 1 ;;
  esac
fi

duckway_init_compose_runtime
BASE_COMPOSE=("${DUCKWAY_COMPOSE[@]}" -f docker-compose.yml)
read -r -a PROFILE_ARGS <<< "$PROFILES"
BASE_COMPOSE+=("${PROFILE_ARGS[@]}")
COMPOSE=("${BASE_COMPOSE[@]}")
DATABASE_BACKEND="${DUCKWAY_DATABASE:-sqlite}"
if [ "$DATABASE_BACKEND" = "postgres" ]; then
  COMPOSE=("${DUCKWAY_COMPOSE[@]}" -f docker-compose.yml -f docker-compose.postgres.yml "${PROFILE_ARGS[@]}")
elif [ "$DATABASE_BACKEND" != "sqlite" ]; then
  echo "Error: DUCKWAY_DATABASE must be 'sqlite' or 'postgres'"
  exit 1
fi

# Stamp builds with the current git revision so `duckway version` reports it.
export DUCKWAY_VERSION="$(git -C "$PROJECT_DIR" describe --tags --always --dirty 2>/dev/null || echo container)"

ui_service() {
  if [ "$MODE" = "split" ]; then
    if [ "$USE_TAILSCALE" = "true" ]; then
      echo "admin-tailscale"
    else
      echo "admin-prod"
    fi
  else
    if [ "$USE_TAILSCALE" = "true" ]; then
      echo "server-tailscale"
    else
      echo "server-prod"
    fi
  fi
}

app_services() {
  if [ "$MODE" = "split" ]; then
    if [ "$USE_TAILSCALE" = "true" ]; then
      echo "admin-tailscale gateway-tailscale"
    else
      echo "admin-prod gateway-prod"
    fi
  else
    if [ "$USE_TAILSCALE" = "true" ]; then
      echo "server-tailscale"
    else
      echo "server-prod"
    fi
  fi
}

# ---------------------------------------------------------------------------
# upgrade / rollback helpers
# ---------------------------------------------------------------------------
BACKUP_ROOT="$PROJECT_DIR/backups"
SMOKE_INTERVAL="${DUCKWAY_UPGRADE_SMOKE_INTERVAL:-2}"
SMOKE_ATTEMPTS="${DUCKWAY_UPGRADE_SMOKE_ATTEMPTS:-30}"

# Service name -> container / binary / listen port, for the active mode.
svc_container() {
  case "$1" in
    admin-*) echo duckway-admin ;;
    gateway-*) echo duckway-gateway ;;
    server-*) echo duckway-server ;;
  esac
}
svc_binary() { svc_container "$1"; }
svc_port() {
  case "$1" in
    admin-*) echo 9091 ;;
    *) echo 8080 ;;
  esac
}
# The first app service owns the SQLite file and the admin UI.
primary_service() {
  local first
  for first in $(app_services); do
    echo "$first"
    return
  done
}
app_services_containers() {
  local s
  for s in $(app_services); do svc_container "$s"; done
}

json_str() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g'; }
img_repo() {
  case "${1##*/}" in
    *:*) echo "${1%:*}" ;;
    *) echo "$1" ;;
  esac
}
rt_running() {
  [ "$("$CONTAINER_RUNTIME" inspect --format '{{.State.Running}}' "$1" 2>/dev/null || true)" = "true" ]
}
file_sha256() { sha256sum "$1" | awk '{print $1}'; }

# with_timeout SECS CMD...: bound a host-side command when `timeout` exists.
with_timeout() {
  local secs="$1"
  shift
  if command -v timeout >/dev/null 2>&1; then timeout "$secs" "$@"; else "$@"; fi
}
# Input validation for values that reach runtime/CLI arguments or paths.
valid_stamp() { printf '%s' "$1" | grep -qE '^[0-9]{8}T[0-9]{6}Z$'; }
valid_image_ref() {
  case "$1" in -*|'') return 1 ;; esac
  printf '%s' "$1" | grep -qE '^[A-Za-z0-9._/:@-]+$'
}
valid_sha256() { printf '%s' "$1" | grep -qE '^[0-9a-f]{64}$'; }
# redact_url URL: drop any user:password@ part before printing.
redact_url() { printf '%s' "$1" | sed -E 's#^([A-Za-z][A-Za-z0-9+.-]*://)[^/@]*@#\1#'; }
file_bytes() { wc -c < "$1" | tr -d ' '; }

# http_probe CONTAINER URL: wget -S inside the container; prints status line and headers.
http_probe() {
  with_timeout 30 "$CONTAINER_RUNTIME" exec "$1" wget -T 5 -S -q -O /dev/null "$2" 2>&1 || true
}
probe_status() { printf '%s\n' "$1" | awk '/^ *HTTP\//{s=$2} END{print s}'; }
probe_has_header() { printf '%s\n' "$1" | grep -qi "^ *$2:"; }

# smoke_service SERVICE [strict|lenient]
smoke_service() {
  local svc="$1" strict="${2:-strict}" c port out="" st="" i
  c="$(svc_container "$svc")"
  port="$(svc_port "$svc")"
  case "$svc" in
    gateway-*)
      for i in $(seq 1 "$SMOKE_ATTEMPTS"); do
        out="$(http_probe "$c" "http://127.0.0.1:$port/healthz")"
        st="$(probe_status "$out")"
        if [ "$st" = "200" ]; then
          echo "  smoke ok: $c /healthz 200"
          return 0
        fi
        sleep "$SMOKE_INTERVAL"
      done
      echo "  smoke FAILED: $c /healthz returned '${st:-no response}'" >&2
      return 1
      ;;
  esac
  for i in $(seq 1 "$SMOKE_ATTEMPTS"); do
    out="$(http_probe "$c" "http://127.0.0.1:$port/admin/login")"
    st="$(probe_status "$out")"
    [ "$st" = "200" ] && break
    sleep "$SMOKE_INTERVAL"
  done
  if [ "$st" != "200" ]; then
    echo "  smoke FAILED: $c /admin/login returned '${st:-no response}' (want 200)" >&2
    return 1
  fi
  echo "  smoke ok: $c /admin/login 200"
  if [ "$strict" = "strict" ]; then
    if ! probe_has_header "$out" "Content-Security-Policy"; then
      echo "  smoke FAILED: $c /admin/login has no Content-Security-Policy header" >&2
      return 1
    fi
    echo "  smoke ok: Content-Security-Policy header present"
    out="$(http_probe "$c" "http://127.0.0.1:$port/api/events")"
    st="$(probe_status "$out")"
    case "$st" in
      401|403) echo "  smoke ok: /api/events requires auth ($st)" ;;
      ""|404|5??)
        echo "  smoke FAILED: /api/events returned '${st:-no response}' (want 401)" >&2
        return 1
        ;;
      *) echo "  smoke note: /api/events returned $st (expected 401)" ;;
    esac
  fi
}

# upgrade_check: strictly read-only (runtime inspect/exec-read, git, df, curl GET).
# Returns non-zero only for blocking problems.
upgrade_check() {
  local blockers=0 svc c head need_kb data_kb free_kb data_path ver url hdrs csp disk_dir
  echo "=== Duckway upgrade check (read-only) ==="
  echo "Mode:      $MODE"
  echo "Database:  $DATABASE_BACKEND"
  echo "Tailscale: $USE_TAILSCALE"
  head="$(git -C "$PROJECT_DIR" rev-parse HEAD 2>/dev/null || echo unknown)"
  echo "Target commit: $head"
  if [ -n "$(git -C "$PROJECT_DIR" status --porcelain 2>/dev/null)" ]; then
    echo "WARNING: working tree has uncommitted changes; the new images will include them and the commit above will not describe them."
  fi
  echo ""
  echo "--- Running services ---"
  for svc in $(app_services); do
    c="$(svc_container "$svc")"
    if rt_running "$c"; then
      echo "$c: image $("$CONTAINER_RUNTIME" inspect --format '{{.Image}}' "$c" 2>/dev/null) ($("$CONTAINER_RUNTIME" inspect --format '{{.Config.Image}}' "$c" 2>/dev/null))"
      ver="$("$CONTAINER_RUNTIME" exec "$c" "$(svc_binary "$svc")" --version 2>/dev/null | head -1 || true)"
      echo "  version: ${ver:-unknown}"
      echo "  labels: $("$CONTAINER_RUNTIME" inspect --format '{{json .Config.Labels}}' "$c" 2>/dev/null || echo '{}')"
    else
      echo "BLOCKER: $c is not running; there is nothing to back up. Start it with: $0 up"
      blockers=$((blockers + 1))
    fi
  done
  echo ""
  echo "--- Disk ---"
  disk_dir="$BACKUP_ROOT"
  [ -d "$disk_dir" ] || disk_dir="$PROJECT_DIR"
  free_kb="$(df -Pk "$disk_dir" 2>/dev/null | awk 'NR==2{print $4}')"
  data_kb=""
  if [ "$DATABASE_BACKEND" = "postgres" ]; then
    c=duckway-postgres
    data_path=/var/lib/postgresql/data
  else
    c="$(svc_container "$(primary_service)")"
    data_path=/data
  fi
  if rt_running "$c"; then
    data_kb="$("$CONTAINER_RUNTIME" exec "$c" du -sk "$data_path" 2>/dev/null | awk '{print $1}' || true)"
  fi
  case "$data_kb" in
    ''|*[!0-9]*)
      echo "Data size: unknown ($c not reachable)"
      if [ "$DATABASE_BACKEND" = "postgres" ] && ! rt_running duckway-postgres; then
        echo "BLOCKER: duckway-postgres is not running; the pg_dump backup is impossible."
        blockers=$((blockers + 1))
      fi
      ;;
    *)
      need_kb=$((data_kb * 2 + 51200))
      echo "Data size: ${data_kb} KB in $c:$data_path; estimated backup need: ${need_kb} KB (2x data + 50 MB)"
      echo "Free in $disk_dir: ${free_kb:-unknown} KB"
      if [ -n "$free_kb" ] && [ "$free_kb" -lt "$need_kb" ]; then
        echo "BLOCKER: insufficient disk space for the backup."
        blockers=$((blockers + 1))
      fi
      ;;
  esac
  echo ""
  echo "--- Schema migrations ---"
  echo "Migrations are append-only by policy: a new image only adds schema, so the"
  echo "previous image keeps working against the upgraded database. 'rollback --keep-data'"
  echo "relies on this; a full rollback restores the pre-upgrade backup instead."
  echo ""
  echo "--- Reverse proxy ---"
  url="${DUCKWAY_ADMIN_PUBLIC_URL:-}"
  if [ -z "$url" ]; then
    echo "DUCKWAY_ADMIN_PUBLIC_URL is not set; skipping proxy probe."
  elif ! command -v curl >/dev/null 2>&1; then
    echo "curl not installed; skipping proxy probe of $(redact_url "$url")"
  else
    hdrs="$(curl -sS -m 10 --proto '=http,https' -o /dev/null -D - -- "${url%/}/admin/login" 2>&1 || true)"
    if [ -z "$hdrs" ]; then
      echo "WARNING: no response from $(redact_url "${url%/}")/admin/login"
    else
      csp="$(printf '%s\n' "$hdrs" | tr -d '\r' | awk 'tolower($0) ~ /^content-security-policy:/{sub(/^[^:]*: */,""); print; exit}')"
      if [ -z "$csp" ]; then
        echo "WARNING: $(redact_url "$url") sends no Content-Security-Policy; a proxy is stripping or overriding Duckway's CSP."
      elif ! printf '%s' "$csp" | grep -q "'nonce-" || ! printf '%s' "$csp" | grep -q "strict-dynamic"; then
        echo "WARNING: Content-Security-Policy at $(redact_url "$url") differs from Duckway's (no nonce/strict-dynamic); the proxy is probably overriding it. Do not override CSP."
      else
        echo "CSP at $(redact_url "$url") matches Duckway's nonce policy."
      fi
    fi
    echo "Reminder: live updates use SSE at /api/events. The proxy must not buffer it"
    echo "(nginx: proxy_buffering off; Caddy: flush_interval -1) and needs a long read timeout."
  fi
  echo ""
  if [ "$blockers" -gt 0 ]; then
    echo "Check FAILED: $blockers blocking problem(s)."
    return 1
  fi
  echo "Check passed."
}

# backup_sqlite DIR: runs the NEW image's binary in a one-off container on the
# data volume (the running binary may predate the `backup` subcommand, and
# invoking an unknown subcommand on it would start a second server). The copy
# is streamed to the host through stdout; nothing is left in the container.
backup_sqlite() {
  local dir="$1" svc bin probe part errf want got
  svc="$(primary_service)"
  bin="$(svc_binary "$svc")"
  part="$dir/duckway.db.partial"
  errf="$dir/backup.stderr.partial"
  echo "Checking that the new $bin image supports 'backup'..."
  probe="$(with_timeout 120 "${COMPOSE[@]}" run --rm -T --no-deps --entrypoint sh "$svc" -c "$bin backup --help 2>&1")" \
    || { echo "The new image's '$bin backup --help' failed (exit non-zero)." >&2; return 1; }
  printf '%s\n' "$probe" | grep -q -- '--out' \
    || { echo "The new image's '$bin backup --help' does not mention --out; refusing to rely on it." >&2; return 1; }
  echo "Backing up SQLite via VACUUM INTO using the new image on the data volume (services keep running)..."
  if ! with_timeout 900 "${COMPOSE[@]}" run --rm -T --no-deps --entrypoint sh "$svc" -c \
      "umask 077; f=/tmp/duckway-upgrade-backup.db; trap 'rm -f \"\$f\"' EXIT INT TERM; $bin backup --out \"\$f\" >&2 && sha256sum \"\$f\" | awk '{print \"DUCKWAY_BACKUP_SHA256=\" \$1}' >&2 && cat \"\$f\"" \
      > "$part" 2> "$errf"; then
    cat "$errf" >&2 2>/dev/null || true
    rm -f "$part" "$errf"
    return 1
  fi
  want="$(sed -n 's/^DUCKWAY_BACKUP_SHA256=\([0-9a-f]\{64\}\)$/\1/p' "$errf" | tail -1)"
  grep -v '^DUCKWAY_BACKUP_SHA256=' "$errf" >&2 2>/dev/null || true
  rm -f "$errf"
  [ -s "$part" ] || { echo "backup file is empty" >&2; rm -f "$part"; return 1; }
  [ "$(head -c 15 "$part")" = "SQLite format 3" ] || { echo "backup is not a SQLite database" >&2; rm -f "$part"; return 1; }
  got="$(file_sha256 "$part")"
  if [ -z "$want" ] || [ "$want" != "$got" ]; then
    echo "backup stream failed verification (container sha256 '${want:-none}' vs host '$got')" >&2
    rm -f "$part"
    return 1
  fi
  chmod 600 "$part"
  mv "$part" "$dir/duckway.db"
}

# Keys, CA material and anything else in /data except the live DB files.
backup_data_files() {
  local dir="$1" c
  c="$(svc_container "$(primary_service)")"
  echo "Archiving key and CA files from $c:/data (excluding live database files)..."
  "$CONTAINER_RUNTIME" exec "$c" tar -cf - -C /data --exclude './duckway.db*' . > "$dir/data-files.tar.partial" \
    || { rm -f "$dir/data-files.tar.partial"; return 1; }
  tar -tf "$dir/data-files.tar.partial" >/dev/null || { echo "data-files.tar is not a valid archive" >&2; rm -f "$dir/data-files.tar.partial"; return 1; }
  chmod 600 "$dir/data-files.tar.partial"
  mv "$dir/data-files.tar.partial" "$dir/data-files.tar"
}

backup_postgres() {
  local dir="$1"
  echo "Backing up PostgreSQL with pg_dump inside duckway-postgres..."
  "$CONTAINER_RUNTIME" exec duckway-postgres sh -c 'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc' > "$dir/duckway.pgdump.partial" \
    || { rm -f "$dir/duckway.pgdump.partial"; return 1; }
  [ -s "$dir/duckway.pgdump.partial" ] || { echo "pg_dump produced no data" >&2; rm -f "$dir/duckway.pgdump.partial"; return 1; }
  "$CONTAINER_RUNTIME" exec -i duckway-postgres pg_restore -l < "$dir/duckway.pgdump.partial" >/dev/null \
    || { echo "pg_dump archive failed verification" >&2; rm -f "$dir/duckway.pgdump.partial"; return 1; }
  chmod 600 "$dir/duckway.pgdump.partial"
  mv "$dir/duckway.pgdump.partial" "$dir/duckway.pgdump"
}

# write_manifest DIR STATUS (uses the UP_* globals). One image object per line so
# rollback can parse it without jq.
write_manifest() {
  local dir="$1" status="$2" out f first=1 i n="${#UP_SVCS[@]}"
  out="$dir/manifest.json.tmp"
  {
    echo "{"
    echo "  \"schema\": 1,"
    echo "  \"status\": \"$status\","
    echo "  \"timestamp\": \"$STAMP\","
    echo "  \"mode\": \"$MODE\","
    echo "  \"database_backend\": \"$DATABASE_BACKEND\","
    echo "  \"tailscale\": \"$USE_TAILSCALE\","
    echo "  \"git_commit_before\": \"$(json_str "$UP_COMMIT_BEFORE")\","
    echo "  \"git_commit_after\": \"$(json_str "$UP_COMMIT_AFTER")\","
    echo "  \"git_dirty\": $UP_DIRTY,"
    echo "  \"version_before\": \"$(json_str "$UP_VERSION_BEFORE")\","
    echo "  \"images\": ["
    for ((i = 0; i < n; i++)); do
      printf '    {"service":"%s","container":"%s","image_name":"%s","previous_image_id":"%s","rollback_tag":"%s"}' \
        "$(json_str "${UP_SVCS[$i]}")" "$(json_str "${UP_CONTAINERS[$i]}")" "$(json_str "${UP_NAMES[$i]}")" \
        "$(json_str "${UP_IDS[$i]}")" "$(json_str "${UP_TAGS[$i]}")"
      if [ $((i + 1)) -lt "$n" ]; then echo ","; else echo; fi
    done
    echo "  ],"
    echo "  \"backup_dir\": \"$(json_str "$(basename "$dir")")\","
    echo "  \"backup_files\": ["
    for f in duckway.db duckway.pgdump data-files.tar; do
      [ -f "$dir/$f" ] || continue
      if [ "$first" -eq 0 ]; then echo ","; fi
      first=0
      printf '    {"name":"%s","sha256":"%s","bytes":%s}' "$f" "$(file_sha256 "$dir/$f")" "$(file_bytes "$dir/$f")"
    done
    echo
    echo "  ]"
    echo "}"
  } > "$out"
  chmod 600 "$out"
  mv "$out" "$dir/manifest.json"
}

upgrade_fail_after_recreate() {
  echo "" >&2
  echo "UPGRADE FAILED after app services were recreated: $1" >&2
  echo "Roll back with:" >&2
  echo "  $0 rollback --to backups/$(basename "$UP_DIR")" >&2
  echo "(add --keep-data to roll back images only and keep the current database)" >&2
  write_manifest "$UP_DIR" failed || true
  exit 1
}

do_upgrade() {
  local assume_yes=false svc c cand confirm name id tag admin_svc gw_svc
  shift
  while [ $# -gt 0 ]; do
    case "$1" in
      --check) upgrade_check; exit $? ;;
      --yes|-y) assume_yes=true ;;
      *) echo "Unknown option for upgrade: $1" >&2; exit 2 ;;
    esac
    shift
  done
  if ! upgrade_check; then
    echo "Aborting: nothing was changed." >&2
    exit 1
  fi
  if [ "$assume_yes" != "true" ]; then
    echo ""
    if ! read -r -p "Tag current images, build, back up, and recreate the app services now? [y/N] " confirm; then
      echo ""
      echo "Cancelled (no input); nothing was changed."
      exit 1
    fi
    case "$confirm" in y|Y|yes|YES) ;; *) echo "Cancelled; nothing was changed."; exit 1 ;; esac
  fi

  STAMP="${DUCKWAY_UPGRADE_STAMP:-$(date -u +%Y%m%dT%H%M%SZ)}"
  if ! valid_stamp "$STAMP"; then
    echo "Error: DUCKWAY_UPGRADE_STAMP must look like 20260102T030405Z (got '$STAMP')" >&2
    exit 2
  fi
  umask 077
  UP_DIR="$BACKUP_ROOT/upgrade-$STAMP"
  mkdir -p "$BACKUP_ROOT"
  chmod 700 "$BACKUP_ROOT"
  if [ -e "$UP_DIR" ]; then
    echo "Error: $UP_DIR already exists" >&2
    exit 1
  fi
  mkdir -m 700 "$UP_DIR"

  UP_SVCS=(); UP_CONTAINERS=(); UP_NAMES=(); UP_IDS=(); UP_TAGS=()
  UP_COMMIT_AFTER="$(git -C "$PROJECT_DIR" rev-parse HEAD 2>/dev/null || echo unknown)"
  UP_DIRTY=false
  if [ -n "$(git -C "$PROJECT_DIR" status --porcelain 2>/dev/null)" ]; then
    UP_DIRTY=true
  fi
  svc="$(primary_service)"
  UP_VERSION_BEFORE="$("$CONTAINER_RUNTIME" exec "$(svc_container "$svc")" "$(svc_binary "$svc")" --version 2>/dev/null | head -1 || true)"
  UP_COMMIT_BEFORE=unknown
  cand="$(printf '%s' "$UP_VERSION_BEFORE" | grep -oE '[0-9a-f]{7,40}' | tail -1 || true)"
  if [ -n "$cand" ]; then
    UP_COMMIT_BEFORE="$(git -C "$PROJECT_DIR" rev-parse --verify --quiet "$cand^{commit}" 2>/dev/null || echo "$cand")"
  fi

  # Partial backup files never outlive a failed or interrupted run.
  trap 'rm -f "$UP_DIR"/*.partial' EXIT INT TERM

  # Step 1: tag the running images so they survive the rebuild (the build
  # replaces the tags the running containers were created from).
  echo ""
  echo "=== Step 1/5: tag current images for rollback ==="
  for svc in $(app_services); do
    c="$(svc_container "$svc")"
    name="$("$CONTAINER_RUNTIME" inspect --format '{{.Config.Image}}' "$c" 2>/dev/null || true)"
    id="$("$CONTAINER_RUNTIME" inspect --format '{{.Image}}' "$c" 2>/dev/null || true)"
    if [ -z "$name" ] || [ -z "$id" ]; then
      echo "Cannot determine the running image of $c; nothing was changed." >&2
      exit 1
    fi
    tag="$(img_repo "$name"):rollback-$STAMP"
    if ! valid_image_ref "$name" || ! valid_image_ref "$id" || ! valid_image_ref "$tag"; then
      echo "Unexpected image reference for $c ('$name' / '$id'); nothing was changed." >&2
      exit 1
    fi
    "$CONTAINER_RUNTIME" tag -- "$id" "$tag" || { echo "Tagging the $c image failed; nothing was changed." >&2; exit 1; }
    echo "  $c: $id -> $tag"
    UP_SVCS+=("$svc"); UP_CONTAINERS+=("$c"); UP_NAMES+=("$name"); UP_IDS+=("$id"); UP_TAGS+=("$tag")
  done
  write_manifest "$UP_DIR" prepared
  echo "Manifest: $UP_DIR/manifest.json"

  # Step 2: build before touching anything that is running.
  echo ""
  echo "=== Step 2/5: build new images (running services untouched) ==="
  if ! "${COMPOSE[@]}" build $(app_services); then
    echo "Build failed; running services were not touched. Rollback tags remain (harmless)." >&2
    write_manifest "$UP_DIR" build-failed || true
    exit 1
  fi

  # Step 3: backup. SQLite uses the freshly built image's binary, so a first
  # upgrade from an image that predates the `backup` subcommand works. Any
  # failure here still leaves running services untouched.
  echo ""
  echo "=== Step 3/5: consistent backup into $UP_DIR ==="
  if [ "$DATABASE_BACKEND" = "postgres" ]; then
    backup_postgres "$UP_DIR" || { echo "Backup failed; nothing was changed. Partial files are in $UP_DIR" >&2; write_manifest "$UP_DIR" backup-failed || true; exit 1; }
  else
    backup_sqlite "$UP_DIR" || { echo "Backup failed; nothing was changed. Partial files are in $UP_DIR" >&2; write_manifest "$UP_DIR" backup-failed || true; exit 1; }
  fi
  backup_data_files "$UP_DIR" || { echo "Backup of data files failed; nothing was changed. Partial files are in $UP_DIR" >&2; write_manifest "$UP_DIR" backup-failed || true; exit 1; }
  write_manifest "$UP_DIR" prepared
  echo "Manifest with backup checksums: $UP_DIR/manifest.json"

  # Step 4: recreate (split: admin first, smoke it, then the gateway).
  echo ""
  echo "=== Step 4/5: recreate app services ==="
  if [ "$MODE" = "split" ]; then
    set -- $(app_services)
    admin_svc="$1"; gw_svc="$2"
    "${COMPOSE[@]}" up -d --no-deps "$admin_svc" || upgrade_fail_after_recreate "recreating $admin_svc failed"
    echo "Smoke-testing admin before touching the gateway..."
    smoke_service "$admin_svc" strict || upgrade_fail_after_recreate "admin smoke test failed; the gateway was NOT recreated"
    "${COMPOSE[@]}" up -d --no-deps "$gw_svc" || upgrade_fail_after_recreate "recreating $gw_svc failed"
  else
    "${COMPOSE[@]}" up -d --no-deps $(app_services) || upgrade_fail_after_recreate "recreating $(app_services) failed"
  fi

  echo ""
  echo "=== Step 5/5: smoke tests ==="
  for svc in $(app_services); do
    if [ "$MODE" = "split" ] && [ "$svc" = "${UP_SVCS[0]}" ]; then
      continue # admin was already smoke-tested before the gateway was recreated
    fi
    smoke_service "$svc" strict || upgrade_fail_after_recreate "smoke test failed for $svc"
  done
  write_manifest "$UP_DIR" complete
  echo ""
  echo "Upgrade complete. Backup and manifest: $UP_DIR"
  echo "To roll back (restores the pre-upgrade database and images; later writes are lost."
  echo "data-files.tar (keys/CA) is NOT restored automatically; it is kept for manual recovery):"
  echo "  $0 rollback --to backups/upgrade-$STAMP"
  echo "Images only (keep current data):"
  echo "  $0 rollback --to backups/upgrade-$STAMP --keep-data"
}

# Prints: service|container|image_name|previous_image_id|rollback_tag
manifest_images() {
  sed -n 's/^ *{"service":"\([^"]*\)","container":"\([^"]*\)","image_name":"\([^"]*\)","previous_image_id":"\([^"]*\)","rollback_tag":"\([^"]*\)"}.*$/\1|\2|\3|\4|\5/p' "$1"
}
# Prints: name|sha256|bytes
manifest_files() {
  sed -n 's/^ *{"name":"\([^"]*\)","sha256":"\([^"]*\)","bytes":\([0-9]*\)}.*$/\1|\2|\3/p' "$1"
}
manifest_field() {
  sed -n "s/^  \"$2\": \"\\([^\"]*\\)\".*\$/\\1/p" "$1" | head -1
}

do_rollback() {
  local dir="" keep_data=false assume_yes=false force=false m_mode m_backend m_status images dbfile typed svc c name tag id fname fsha fbytes
  shift
  while [ $# -gt 0 ]; do
    case "$1" in
      --to) dir="${2:-}"; shift ;;
      --keep-data) keep_data=true ;;
      --yes|-y) assume_yes=true ;;
      --force) force=true ;;
      *) echo "Unknown option for rollback: $1" >&2; exit 2 ;;
    esac
    shift
  done
  if [ -z "$dir" ]; then
    echo "Usage: $0 rollback --to backups/upgrade-<UTC timestamp> [--keep-data] [--yes] [--force]" >&2
    exit 2
  fi
  case "$dir" in
    /*) ;;
    backups/*|./backups/*) dir="$PROJECT_DIR/${dir#./}" ;;
    *) dir="$BACKUP_ROOT/$dir" ;;
  esac
  dir="${dir%/}"
  if [ ! -f "$dir/manifest.json" ]; then
    echo "Error: $dir/manifest.json not found" >&2
    exit 1
  fi
  m_mode="$(manifest_field "$dir/manifest.json" mode)"
  m_backend="$(manifest_field "$dir/manifest.json" database_backend)"
  if [ "$m_mode" != "$MODE" ] || [ "$m_backend" != "$DATABASE_BACKEND" ]; then
    echo "Error: manifest was made in mode=$m_mode backend=$m_backend but .prod.env now says mode=$MODE backend=$DATABASE_BACKEND" >&2
    exit 1
  fi
  m_status="$(manifest_field "$dir/manifest.json" status)"
  case "$m_status" in
    complete|failed) ;;
    *)
      if [ "$force" != "true" ]; then
        echo "Error: manifest status is '${m_status:-unknown}' (want complete or failed); the upgrade never finished its backup/recreate steps. Use --force to override." >&2
        exit 1
      fi
      echo "WARNING: --force: manifest status is '${m_status:-unknown}'." >&2
      ;;
  esac
  images="$(manifest_images "$dir/manifest.json")"
  if [ -z "$images" ]; then
    echo "Error: manifest lists no images" >&2
    exit 1
  fi
  while IFS='|' read -r _ _ name id tag; do
    if ! valid_image_ref "$name" || ! valid_image_ref "$id" || ! valid_image_ref "$tag"; then
      echo "Error: manifest contains an invalid image reference ('$name' '$id' '$tag'); nothing was changed." >&2
      exit 1
    fi
  done <<< "$images"
  # Verify every backup file (checksum and size) before anything destructive.
  if [ "$keep_data" != "true" ]; then
    dbfile=duckway.db
    [ "$DATABASE_BACKEND" = "postgres" ] && dbfile=duckway.pgdump
    if ! manifest_files "$dir/manifest.json" | grep -q "^$dbfile|"; then
      echo "Error: manifest does not list $dbfile; cannot restore" >&2
      exit 1
    fi
    while IFS='|' read -r fname fsha fbytes; do
      case "$fname" in
        duckway.db|duckway.pgdump|data-files.tar) ;;
        *) echo "Error: manifest lists unexpected backup file '$fname'; nothing was changed." >&2; exit 1 ;;
      esac
      if [ ! -f "$dir/$fname" ]; then
        echo "Error: backup file $dir/$fname is missing; nothing was changed." >&2
        exit 1
      fi
      if ! valid_sha256 "$fsha" || [ "$(file_sha256 "$dir/$fname")" != "$fsha" ] || [ "$(file_bytes "$dir/$fname")" != "$fbytes" ]; then
        echo "Error: backup file $dir/$fname does not match the manifest checksum/size; refusing to roll back. Nothing was changed." >&2
        exit 1
      fi
    done < <(manifest_files "$dir/manifest.json")
    echo "Backup files verified against manifest checksums."
    if [ ! -s "$dir/$dbfile" ]; then
      echo "Error: backup file $dir/$dbfile is empty" >&2
      exit 1
    fi
  fi
  echo "Rollback target: $dir"
  echo "  mode=$m_mode backend=$m_backend commit_before=$(manifest_field "$dir/manifest.json" git_commit_before)"
  echo "  App images will be re-tagged to the pre-upgrade images and the services recreated."
  if [ "$keep_data" = "true" ]; then
    echo "  --keep-data: the database is NOT restored (relies on append-only migrations)."
  else
    echo "  WARNING: the database will be restored from this backup. Every write made after"
    echo "  $(manifest_field "$dir/manifest.json" timestamp) (approvals, logs, new keys and clients) will be LOST."
  fi
  if [ "$assume_yes" != "true" ]; then
    typed=""
    read -r -p "Type the backup directory name ($(basename "$dir")) to continue: " typed || echo ""
    if [ "$typed" != "$(basename "$dir")" ]; then
      echo "Confirmation did not match; nothing was changed."
      exit 1
    fi
  fi

  while IFS='|' read -r _ _ _ _ tag; do
    "$CONTAINER_RUNTIME" image inspect -- "$tag" >/dev/null 2>&1 || { echo "Error: rollback image $tag no longer exists; nothing was changed." >&2; exit 1; }
  done <<< "$images"

  if [ "$keep_data" != "true" ]; then
    echo "Stopping app services..."
    for c in $(app_services_containers); do
      "$CONTAINER_RUNTIME" stop "$c" >/dev/null || { echo "Failed to stop $c; nothing restored." >&2; exit 1; }
    done
    if [ "$DATABASE_BACKEND" = "postgres" ]; then
      echo "Restoring PostgreSQL from $dir/duckway.pgdump ..."
      "$CONTAINER_RUNTIME" exec -i duckway-postgres sh -c 'pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --single-transaction --clean --if-exists --no-owner' < "$dir/duckway.pgdump" \
        || { echo "PostgreSQL restore FAILED (single transaction, database unchanged). App services are stopped." >&2; exit 1; }
    else
      echo "Restoring SQLite database from $dir/duckway.db ..."
      svc="$(primary_service)"
      "${COMPOSE[@]}" run --rm --no-deps -v "$dir:/restore:ro" --entrypoint sh "$svc" -c \
        'cp /restore/duckway.db /data/duckway.db.restore && rm -f /data/duckway.db-journal /data/duckway.db-wal /data/duckway.db-shm && mv /data/duckway.db.restore /data/duckway.db' \
        || { echo "SQLite restore FAILED. App services are stopped; retry or restore $dir/duckway.db manually." >&2; exit 1; }
    fi
  fi

  echo "Re-tagging previous images..."
  while IFS='|' read -r _ _ name _ tag; do
    "$CONTAINER_RUNTIME" tag -- "$tag" "$name" || { echo "Re-tagging $tag failed." >&2; exit 1; }
    echo "  $tag -> $name"
  done <<< "$images"

  echo "Recreating app services from the previous images..."
  if [ "$MODE" = "split" ]; then
    set -- $(app_services)
    "${COMPOSE[@]}" up -d --no-deps "$1" || { echo "Recreating $1 failed." >&2; exit 1; }
    "${COMPOSE[@]}" up -d --no-deps "$2" || { echo "Recreating $2 failed." >&2; exit 1; }
  else
    "${COMPOSE[@]}" up -d --no-deps $(app_services) || { echo "Recreating app services failed." >&2; exit 1; }
  fi
  for svc in $(app_services); do
    smoke_service "$svc" lenient || { echo "Smoke test failed after rollback for $svc; inspect with: $0 logs" >&2; exit 1; }
  done
  echo "Rollback complete."
}

case "${1:-up}" in
  up|start)
    echo "Building images..."
    "${COMPOSE[@]}" build
    echo "Starting Duckway production ($MODE mode + Tailscale)..."
    "${COMPOSE[@]}" up -d
    echo ""
    if [ "$USE_TAILSCALE" = "true" ]; then
      if [ "$MODE" = "split" ]; then
        echo "Admin:   http://$TS_HOSTNAME-admin/admin/  (port 80, tailnet only)"
        echo "Gateway: http://$TS_HOSTNAME-gw/     (port 80, tailnet only)"
      else
        echo "Server:  http://$TS_HOSTNAME/        (port 80, tailnet only)"
      fi
    else
      echo "No ports exposed. Use a reverse proxy to reach the containers."
      if [ "$MODE" = "split" ]; then
        echo "Admin:   duckway-admin:9091"
        echo "Gateway: duckway-gateway:8080"
      else
        echo "Server:  duckway-server:8080"
      fi
    fi
    echo ""
    echo "Admin password (first run only):"
    "${COMPOSE[@]}" logs 2>&1 | grep "Password:" | tail -1 || echo "  (check: $0 logs)"
    echo ""
    echo "Tailscale nodes:"
    "${COMPOSE[@]}" ps --format "table {{.Name}}\t{{.Status}}" 2>/dev/null | grep -E "NAME|ts-|tailscale|postgres" || true
    ;;

  down|stop)
    echo "Stopping..."
    "${COMPOSE[@]}" down
    ;;

  restart)
    if [ "${2:-}" = "--minimal" ]; then
      services="$(app_services)"
      echo "Building app services before touching running containers ($MODE mode): $services"
      "${COMPOSE[@]}" build $services
      echo "Recreating app services only (dependencies/sidecars are left running)..."
      "${COMPOSE[@]}" up -d --no-deps $services
    else
      echo "Building new images before touching running containers ($MODE mode)..."
      "${COMPOSE[@]}" build
      echo "Recreating containers with new images..."
      "${COMPOSE[@]}" up -d --remove-orphans
    fi
    ;;

  ui|restart-ui)
    svc="$(ui_service)"
    if [ "$MODE" = "combined" ]; then
      echo "Combined mode embeds UI in the server; recreating only $svc."
    else
      echo "Split mode embeds UI in admin only; gateway will not be restarted."
    fi
    echo "Building $svc before touching the running container..."
    "${COMPOSE[@]}" build "$svc"
    echo "Recreating $svc..."
    "${COMPOSE[@]}" up -d --no-deps "$svc"
    ;;

  nuke)
    echo "Removing everything including data and Tailscale state..."
    read -p "Are you sure? This deletes all data. [y/N] " confirm
    if [ "$confirm" = "y" ] || [ "$confirm" = "Y" ]; then
      "${COMPOSE[@]}" down -v
      echo "Done."
    else
      echo "Cancelled."
    fi
    ;;

  logs)
    if [ -n "${2:-}" ]; then
      "${COMPOSE[@]}" logs -f "$2"
    else
      "${COMPOSE[@]}" logs -f
    fi
    ;;

  status)
    echo "=== Containers ==="
    "${COMPOSE[@]}" ps
    echo ""
    echo "=== Tailscale Status ==="
    if [ "$MODE" = "split" ]; then
      "$CONTAINER_RUNTIME" exec duckway-tailscale-admin tailscale status 2>/dev/null || echo "  tailscale-admin not running"
      "$CONTAINER_RUNTIME" exec duckway-tailscale-gateway tailscale status 2>/dev/null || echo "  tailscale-gateway not running"
    else
      "$CONTAINER_RUNTIME" exec duckway-tailscale-server tailscale status 2>/dev/null || echo "  tailscale-server not running"
    fi
    ;;

  password)
    "${COMPOSE[@]}" logs 2>&1 | grep "Password:" | tail -1
    ;;

  migrate-postgres)
    if [ "$DATABASE_BACKEND" = "postgres" ]; then
      echo "PostgreSQL is already the configured database."
      exit 0
    fi
    configured_secret="${DUCKWAY_POSTGRES_PASSWORD_FILE:-}"
    secret_file="${configured_secret:-./.secrets/postgres-password}"
    case "$secret_file" in
      /*) ;;
      *) secret_file="$PROJECT_DIR/${secret_file#./}" ;;
    esac
    if [ -z "$configured_secret" ] || [ "$configured_secret" = "./.secrets/postgres-password" ]; then
      mkdir -p "$(dirname "$secret_file")"
      chmod 700 "$(dirname "$secret_file")"
    elif [ ! -d "$(dirname "$secret_file")" ]; then
      echo "Error: PostgreSQL password directory does not exist: $(dirname "$secret_file")" >&2
      exit 1
    fi
    mkdir -p "$PROJECT_DIR/backups"
    chmod 700 "$PROJECT_DIR/backups"
    if [ -L "$secret_file" ] || { [ -e "$secret_file" ] && [ ! -f "$secret_file" ]; }; then
      echo "Error: PostgreSQL password path must be a regular, non-symlink file" >&2
      exit 1
    fi
    if [ ! -f "$secret_file" ]; then
      umask 077
      openssl rand -base64 36 > "$secret_file"
    fi
    chmod 600 "$secret_file"
    if [ ! -s "$secret_file" ] || [ "$(wc -l < "$secret_file")" -ne 1 ]; then
      echo "Error: PostgreSQL password file must contain exactly one non-empty line" >&2
      exit 1
    fi
    export DUCKWAY_POSTGRES_PASSWORD_FILE="$secret_file"
    PG_COMPOSE=("${DUCKWAY_COMPOSE[@]}" -f docker-compose.yml -f docker-compose.postgres.yml "${PROFILE_ARGS[@]}" --profile postgres-tools)
    postgres_preexisting=false
    if "$CONTAINER_RUNTIME" inspect duckway-postgres >/dev/null 2>&1; then
      postgres_preexisting=true
    fi
    services="$(app_services)"
    stamp="$(date -u +%Y%m%dT%H%M%SZ)"
    backup="duckway-sqlite-$stamp.tar.gz"
    echo "Building PostgreSQL and migration images..."
    "${PG_COMPOSE[@]}" build postgres-migrator
    echo "Pulling PostgreSQL before stopping Duckway..."
    "${PG_COMPOSE[@]}" pull postgres
    echo "Stopping every Duckway writer before the SQLite snapshot..."
    for container in duckway-server duckway-admin duckway-gateway; do
      "$CONTAINER_RUNTIME" stop "$container" >/dev/null 2>&1 || true
    done
    migration_ok=false
    env_backup=""
    cleanup_migration() {
      if [ "$migration_ok" != "true" ]; then
        if [ -n "$env_backup" ] && [ -f "$env_backup" ]; then
          install -m 600 "$env_backup" "$PROJECT_DIR/.prod.env"
        fi
        echo "Migration failed; PostgreSQL was not enabled. Restarting SQLite services." >&2
        if [ "$postgres_preexisting" != "true" ]; then
          "$CONTAINER_RUNTIME" stop duckway-postgres >/dev/null 2>&1 || true
        fi
        "${BASE_COMPOSE[@]}" up -d $services || true
      fi
    }
    trap cleanup_migration EXIT
    echo "Creating offline SQLite and key backup: backups/$backup"
    "${PG_COMPOSE[@]}" run --rm sqlite-backup -czf "/backup/$backup" -C /data .
    chmod 600 "$PROJECT_DIR/backups/$backup"
    tar -tzf "$PROJECT_DIR/backups/$backup" >/dev/null
    for required_file in ./duckway.db ./encryption.key; do
      if ! tar -tzf "$PROJECT_DIR/backups/$backup" | grep -qx "$required_file"; then
        echo "Backup verification failed: missing $required_file" >&2
        exit 1
      fi
    done
    sha256sum "$PROJECT_DIR/backups/$backup" > "$PROJECT_DIR/backups/$backup.sha256"
    chmod 600 "$PROJECT_DIR/backups/$backup.sha256"
    echo "Starting PostgreSQL..."
    "${PG_COMPOSE[@]}" up -d postgres
    echo "Importing and validating SQLite data..."
    migrator_args=(--sqlite-data /data)
    if [ "${DUCKWAY_MIGRATION_SKIP_REQUEST_LOGS:-false}" = "true" ]; then
      echo "Request log history will be excluded from the PostgreSQL migration."
      migrator_args+=(--skip-request-logs)
    fi
    "${PG_COMPOSE[@]}" run --rm postgres-migrator "${migrator_args[@]}"
    env_backup="$PROJECT_DIR/backups/prod-env-before-postgres-$stamp"
    install -m 600 "$PROJECT_DIR/.prod.env" "$env_backup"
    env_tmp="$PROJECT_DIR/.prod.env.tmp.$$"
    awk '
      BEGIN { replaced=0 }
      /^DUCKWAY_DATABASE=/ { print "DUCKWAY_DATABASE=postgres"; replaced=1; next }
      { print }
      END { if (!replaced) print "DUCKWAY_DATABASE=postgres" }
    ' "$PROJECT_DIR/.prod.env" > "$env_tmp"
    chmod 600 "$env_tmp"
    mv "$env_tmp" "$PROJECT_DIR/.prod.env"
    echo "Starting Duckway on PostgreSQL..."
    "$0" restart
    running_services="$("${PG_COMPOSE[@]}" ps --status running --services)"
    for service in $services; do
      if ! printf '%s\n' "$running_services" | grep -qx "$service"; then
        echo "PostgreSQL cutover health check failed: $service is not running" >&2
        exit 1
      fi
    done
    if [ "$MODE" = "split" ]; then
      health_containers="duckway-admin duckway-gateway"
    else
      health_containers="duckway-server"
    fi
    for container in $health_containers; do
      healthy=false
      for _ in $(seq 1 24); do
        if [ "$("$CONTAINER_RUNTIME" inspect --format '{{.State.Health.Status}}' "$container" 2>/dev/null || true)" = "healthy" ]; then
          healthy=true
          break
        fi
        sleep 5
      done
      if [ "$healthy" != "true" ]; then
        echo "PostgreSQL cutover health check failed: $container is not healthy" >&2
        exit 1
      fi
    done
    migration_ok=true
    trap - EXIT
    echo "Cutover complete. SQLite backup retained at backups/$backup"
    ;;

  upgrade)
    do_upgrade "$@"
    ;;

  rollback)
    do_rollback "$@"
    ;;

  *)
    echo "Duckway Production Manager"
    echo ""
    echo "Usage: $0 {up|down|restart [--minimal]|ui|restart-ui|upgrade|rollback|migrate-postgres|nuke|logs|status|password}"
    echo ""
    echo "Commands:"
    echo "  up        Build and start with Tailscale"
    echo "  down      Stop (keep data)"
    echo "  restart   Build first, then recreate containers with minimal downtime"
    echo "            --minimal recreates app services only; dependencies/sidecars must already be healthy"
    echo "  ui        Rebuild/recreate only the UI-bearing service"
    echo "  nuke      Stop and delete all data (asks confirmation)"
    echo "  logs      Follow logs (optional: service name)"
    echo "  status    Show container + Tailscale status"
    echo "  password  Show admin password"
    echo "  upgrade --check"
    echo "            Read-only preflight: versions, mode, backend, disk, proxy headers"
    echo "  upgrade [--yes]"
    echo "            Tag rollback images, build, consistent backup (new image), recreate, smoke-test"
    echo "            (backups/upgrade-<UTC ts>/manifest.json records everything)"
    echo "  rollback --to backups/upgrade-<ts> [--keep-data] [--yes] [--force]"
    echo "            Restore previous images (and, unless --keep-data, the pre-upgrade database)"
    echo "  migrate-postgres  Offline backup, import, verify, and switch from SQLite"
    echo ""
    echo "Mode: $MODE (set DUCKWAY_PROD_MODE in .prod.env)"
    echo "  split    — admin + gateway on separate containers (default)"
    echo "  combined — everything on one container"
    echo ""
    echo "Tailscale: $USE_TAILSCALE (set DUCKWAY_TAILSCALE=false to disable)"
    echo "Database: $DATABASE_BACKEND (set DUCKWAY_DATABASE in .prod.env)"
    exit 1
    ;;
esac
