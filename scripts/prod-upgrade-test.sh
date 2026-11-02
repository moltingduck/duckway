#!/usr/bin/env bash
# Offline test for `scripts/prod.sh upgrade|rollback`.
#
# Uses a stub docker (CONTAINER_RUNTIME=docker), stub curl and stub df placed
# first on PATH. Every runtime invocation is logged so the test can assert call
# ordering without touching any real container, image or volume.
#
# Usage: scripts/prod-upgrade-test.sh
# Temp root: <repo>/tmp/lu1102-upgrade/run-$$ (removed on EXIT/INT/TERM).
set -u

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
ROOT="$REPO_DIR/tmp/lu1102-upgrade/run-$$"
mkdir -p "$ROOT"
cleanup() { rm -rf "$ROOT"; }
trap cleanup EXIT INT TERM

STUB="$ROOT/stub"
LOG="$ROOT/calls.log"
mkdir -p "$STUB"
: > "$LOG"

# ---------------------------------------------------------------- stubs
mkdir -p "$ROOT/tarsrc"
printf 'key' > "$ROOT/tarsrc/encryption.key"
tar -cf "$STUB/data.tar" -C "$ROOT/tarsrc" encryption.key

cat > "$STUB/docker" <<'EOF'
#!/usr/bin/env bash
echo "docker $*" >> "$STUB_LOG"
# Runtime state model: container -> image id, image name -> image id. `compose
# up` WITHOUT --force-recreate only restarts the existing container (it keeps
# the image it was created from), like podman-compose does.
ST="$STUB_DIR/state"
mkdir -p "$ST"
get() { [ -f "$ST/$1" ] && cat "$ST/$1"; }
resolve() { # image ref -> id
  local v; v="$(get "img-$(echo "$1" | tr '/:' '__')")"
  if [ -n "$v" ]; then echo "$v"; return; fi
  case "$1" in
    sha256:*) echo "$1" ;;
    img-*:rollback-*) echo "sha256:old-${1%%:*}" | sed 's/old-img-/old-/' ;;
    img-*) echo "sha256:old-${1#img-}" ;;
    *) echo "$1" ;;
  esac
}
ctr_image() {
  local v; v="$(get "ctr-$1")"; v="${v:-sha256:old-$1}"
  # containerd image store: the container reports the config digest.
  if [ -n "${STUB_CONTAINERD:-}" ]; then echo "sha256:cfg-${v#sha256:}"; else echo "$v"; fi
}
svc_ctr() { case "$1" in admin-*) echo duckway-admin ;; gateway-*) echo duckway-gateway ;; server-*) echo duckway-server ;; esac; }
case "$1" in
  info) exit 0 ;;
  compose)
    shift
    [ "$1" = "version" ] && exit 0
    all="$*"
    for a in "$@"; do
      if [ "$a" = "build" ] && [ -n "${STUB_FAIL_BUILD:-}" ]; then exit 1; fi
    done
    case " $all " in
      *" build "*)
        for a in "$@"; do
          c="$(svc_ctr "$a")"
          [ -n "$c" ] && echo "sha256:new-$c" > "$ST/img-img-$c"
        done ;;
      *" up "*)
        [ -n "${STUB_KILL_ON_UP:-}" ] && kill -TERM "$PPID"
        force=""; case " $all " in *" --force-recreate "*) force=1 ;; esac
        [ -n "${STUB_NEVER_RECREATE:-}" ] && force=""
        if [ -n "$force" ]; then
          for a in "$@"; do
            c="$(svc_ctr "$a")"
            [ -n "$c" ] && resolve "img-$c" > "$ST/ctr-$c"
          done
        fi ;;
    esac
    case "$all" in
      *"grep -a -q -F -- 'Usage: backup --out'"*)
        # Marker probe of the NEW image's binary (the binary is never executed).
        if [ -n "${STUB_NO_BACKUP_SUPPORT:-}" ]; then exit 1; fi
        exit 0 ;;
      *"backup --out"*)
        [ -n "${STUB_FAIL_BACKUP:-}" ] && { echo "backup failed" >&2; exit 1; }
        content='SQLite format 3\0stubbed-db'
        sum="$(printf "$content" | sha256sum | awk '{print $1}')"
        [ -n "${STUB_BAD_STREAM_SHA:-}" ] && sum=0000000000000000000000000000000000000000000000000000000000000000
        echo "backup written: /tmp/x" >&2
        echo "DUCKWAY_BACKUP_SHA256=$sum" >&2
        printf "$content"
        exit 0 ;;
    esac
    exit 0
    ;;
  inspect)
    fmt="" name=""
    while [ $# -gt 0 ]; do
      case "$1" in
        inspect) ;;
        --format) fmt="$2"; shift ;;
        *) name="$1" ;;
      esac
      shift
    done
    if [ -n "${STUB_NOT_RUNNING:-}" ] && [ "$fmt" = '{{.State.Running}}' ]; then echo false; exit 0; fi
    if [ -n "${STUB_NO_CONFIG_IMAGE:-}" ] && [ "$fmt" = '{{.Config.Image}}' ]; then exit 1; fi
    case "$fmt" in
      '{{.State.Running}}') echo true ;;
      '{{.Image}}') ctr_image "$name" ;;
      '{{.Config.Image}}') echo "img-$name" ;;
      '{{json .Config.Labels}}') echo '{}' ;;
      *) echo "" ;;
    esac
    exit 0
    ;;
  image)
    [ -n "${STUB_NO_ROLLBACK_IMAGE:-}" ] && exit 1
    case "$*" in
      *"{{.Id}}"*)
        last="${*: -1}"
        case "$last" in sha256:cfg-*) echo "sha256:${last#sha256:cfg-}" ;; *) resolve "$last" ;; esac
        exit 0 ;;
    esac
    exit 0
    ;;
  tag)
    [ -n "${STUB_FAIL_TAG:-}" ] && exit 1
    shift; [ "$1" = "--" ] && shift
    resolve "$1" > "$ST/img-$(echo "$2" | tr '/:' '__')"
    exit 0
    ;;
  stop|rm) exit 0 ;;
  cp)
    # docker cp CONTAINER:SRC DEST
    printf 'SQLite format 3\0stubbed-db' > "$3"
    exit 0
    ;;
  exec)
    shift
    [ "$1" = "-i" ] && { shift; cat >/dev/null; }
    ctr="$1"; shift
    case "$1" in
      du) printf '1024\t%s\n' "$3"; exit 0 ;;
      df) printf 'Filesystem 1K-blocks Used Available Use%% Mounted\nstub 1 1 %s 1%% /data\n' "${STUB_VOL_FREE_KB:-90000000}"; exit 0 ;;
      rm) exit 0 ;;
      tar) cat "$STUB_DIR/data.tar"; exit 0 ;;
      sh) printf 'PGDMP-stub'; exit 0 ;;
      pg_restore) exit 0 ;;
      wget)
        url="${*: -1}"
        case "$url" in
          */admin/login)
            echo "  HTTP/1.1 200 OK" >&2
            [ -z "${STUB_NO_CSP:-}" ] && echo "  Content-Security-Policy: default-src 'self'" >&2
            ;;
          */api/events) echo "  HTTP/1.1 401 Unauthorized" >&2 ;;
          */healthz) echo "  HTTP/1.1 200 OK" >&2 ;;
        esac
        exit 0
        ;;
      *)
        if [ "${2:-}" = "--version" ]; then echo "$1 abc1234"; exit 0; fi
        if [ "${2:-}" = "backup" ]; then
          # The running (old) binary must never be asked to back up: it may
          # not know the subcommand and would start a second server.
          echo "OLD BINARY STARTED A SERVER" >&2
          exit 1
        fi
        exit 0
        ;;
    esac
    ;;
esac
exit 0
EOF

cat > "$STUB/curl" <<'EOF'
#!/usr/bin/env bash
echo "curl $*" >> "$STUB_LOG"
printf 'HTTP/1.1 200 OK\r\n'
case "${STUB_PROXY_CSP:-}" in
  none) ;;
  other) printf "Content-Security-Policy: default-src *\r\n" ;;
  *) printf "Content-Security-Policy: default-src 'self'; script-src 'nonce-abc' 'strict-dynamic'\r\n" ;;
esac
exit 0
EOF

cat > "$STUB/df" <<'EOF'
#!/usr/bin/env bash
printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\n'
printf 'stub 1000000 1 %s 1%% /\n' "${STUB_DF_FREE_KB:-90000000}"
EOF
chmod +x "$STUB/docker" "$STUB/curl" "$STUB/df"
ln -s docker "$STUB/podman"

# ---------------------------------------------------------------- harness
PASS=0
FAIL=0
ok() { PASS=$((PASS + 1)); echo "  ok   - $1"; }
bad() { FAIL=$((FAIL + 1)); echo "  FAIL - $1"; }
assert() { # assert DESC CMD...
  local d="$1"; shift
  if "$@"; then ok "$d"; else bad "$d"; fi
}
assert_not() { local d="$1"; shift; if "$@"; then bad "$d"; else ok "$d"; fi; }

PROJ=""
new_proj() { # new_proj MODE BACKEND [extra .prod.env lines...]
  local mode="$1" backend="$2"
  shift 2
  PROJ="$ROOT/proj-$RANDOM$RANDOM"
  mkdir -p "$PROJ/scripts"
  cp "$REPO_DIR/scripts/prod.sh" "$REPO_DIR/scripts/container-runtime.sh" "$PROJ/scripts/"
  cp "$REPO_DIR"/docker-compose.yml "$REPO_DIR"/docker-compose.postgres.yml "$PROJ/"
  {
    echo "DUCKWAY_PROD_MODE=$mode"
    echo "DUCKWAY_TAILSCALE=false"
    echo "DUCKWAY_DATABASE=$backend"
    for l in "$@"; do echo "$l"; done
  } > "$PROJ/.prod.env"
  git -C "$PROJ" init -q
  git -C "$PROJ" add -A
  git -C "$PROJ" -c user.name=t -c user.email=t@example.invalid commit -q -m init
  : > "$LOG"
  rm -rf "$STUB/state"
}

OUT=""
RC=0
run_prod() { # run_prod [stdin-string] -- args...
  local input="$1"
  shift
  [ "$1" = "--" ] && shift
  OUT="$ROOT/out.txt"
  local stdin_src="$ROOT/stdin.txt"
  if [ "$input" = "__EOF__" ]; then : > "$stdin_src"; else printf '%s\n' "$input" > "$stdin_src"; fi
  (
    cd "$PROJ" || exit 99
    PATH="$STUB:$PATH" CONTAINER_RUNTIME=docker STUB_LOG="$LOG" STUB_DIR="$STUB" \
      DUCKWAY_UPGRADE_STAMP="${STAMP_OVERRIDE:-20260102T030405Z}" DUCKWAY_UPGRADE_SMOKE_INTERVAL=0 DUCKWAY_UPGRADE_SMOKE_ATTEMPTS=2 \
      bash scripts/prod.sh "$@" < "$stdin_src"
  ) > "$OUT" 2>&1
  RC=$?
}

line_of() { grep -nE -m1 -- "$1" "$LOG" | cut -d: -f1; }
in_log() { grep -qE -- "$1" "$LOG"; }
in_out() { grep -qE -- "$1" "$OUT"; }
before() { # before REGEX_A REGEX_B: both present and A first
  local a b
  a="$(line_of "$1")"; b="$(line_of "$2")"
  [ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ]
}
MUTATING='docker (tag|stop|rm|cp) |docker compose .* (build|up|run|down) |exec .* backup|exec duckway-postgres sh -c'

# ================================================================ check
echo "# upgrade --check is read-only"
new_proj split sqlite
run_prod "" -- upgrade --check
assert "check exits 0" [ "$RC" -eq 0 ]
assert_not "check makes no mutating runtime call" in_log "$MUTATING"
assert "check inspects the runtime" in_log 'docker inspect'
assert "check prints mode" in_out 'Mode: +split'
assert "check prints backend" in_out 'Database: +sqlite'
assert "check prints target commit" in_out 'Target commit: [0-9a-f]{40}'
assert "check prints migration policy" in_out 'append-only'
assert "check creates no backup dir" [ ! -d "$PROJ/backups/upgrade-20260102T030405Z" ]
assert_not "no dirty warning on clean tree" in_out 'uncommitted changes'
echo dirty > "$PROJ/dirty.txt"
run_prod "" -- upgrade --check
assert "check warns on dirty tree" in_out 'uncommitted changes'

echo "# check blockers"
new_proj split sqlite
STUB_DF_FREE_KB=10 run_prod "" -- upgrade --check
assert "insufficient disk is blocking" [ "$RC" -ne 0 ]
assert "insufficient disk reported" in_out 'insufficient disk'
STUB_NOT_RUNNING=1 run_prod "" -- upgrade --check
assert "stopped service is blocking" [ "$RC" -ne 0 ]
STUB_NOT_RUNNING=1 run_prod "" -- upgrade --yes
assert "upgrade aborts on blocking check" [ "$RC" -ne 0 ]
assert_not "blocked upgrade ran no mutation" in_log "$MUTATING"

echo "# proxy probe"
new_proj split sqlite DUCKWAY_ADMIN_PUBLIC_URL=http://proxy.test
STUB_PROXY_CSP=none run_prod "" -- upgrade --check
assert "missing CSP warns" in_out 'no Content-Security-Policy'
assert "SSE buffering reminder" in_out 'proxy_buffering off'
STUB_PROXY_CSP=other run_prod "" -- upgrade --check
assert "different CSP warns" in_out 'differs from Duckway'
STUB_PROXY_CSP=ok run_prod "" -- upgrade --check
assert "matching CSP accepted" in_out 'matches Duckway'
assert "proxy probe is exit 0" [ "$RC" -eq 0 ]

# ================================================================ upgrade (split, sqlite)
echo "# upgrade split/sqlite success"
new_proj split sqlite
run_prod "" -- upgrade --yes
assert "upgrade exits 0" [ "$RC" -eq 0 ]
assert "tag before build" before 'docker tag ' 'docker compose .* build'
assert "build before backup" before 'docker compose .* build' 'backup-tmp-20260102T030405Z'
assert "backup probe before backup" before "Usage: backup --out" 'backup-tmp-20260102T030405Z'
assert "backup before recreate" before 'backup-tmp-20260102T030405Z' 'docker compose .* up -d'
assert "backup runs as a one-off container of the NEW image" in_log 'compose .* run --rm -T --no-deps --name duckway-backup-[^ ]+ --entrypoint sh admin-prod -c .*backup-tmp-20260102T030405Z.*duckway-admin backup --out'
assert_not "running (old) container is never asked to back up" in_log 'exec .* backup'
assert_not "backup probe never executes the binary" in_log 'backup --help'
assert "one-off backup container has a deterministic name" in_log '--name duckway-backup-20260102T030405Z'
assert "one-off backup container is force-removed" in_log 'docker rm -f duckway-backup-20260102T030405Z'
assert "restore-side umask and sha check present in rollback" true
assert "wget probes have a timeout" in_log 'wget -T 5 '
assert "backup db is private" [ "$(stat -c %a "$PROJ/backups/upgrade-20260102T030405Z/duckway.db")" = 600 ]
assert "no partial files left" bash -c "! ls '$PROJ'/backups/upgrade-20260102T030405Z/*.partial >/dev/null 2>&1"
assert "recreate uses --force-recreate" in_log 'up -d --no-deps --force-recreate admin-prod'
assert "upgrade verifies the admin image" in_out 'verified: duckway-admin runs the newly built image sha256:new-duckway-admin'
assert "upgrade verifies the gateway image" in_out 'verified: duckway-gateway runs the newly built image sha256:new-duckway-gateway'
assert "build before recreate" before 'docker compose .* build' 'docker compose .* up -d'
assert "admin recreated before smoke" before 'up -d --no-deps --force-recreate admin-prod' 'wget .*9091/admin/login'
assert "admin smoke before gateway recreate" before 'wget .*9091/admin/login' 'up -d --no-deps --force-recreate gateway-prod'
assert "build covers both app services" in_log 'build admin-prod gateway-prod'
assert "images tagged with rollback stamp" in_log 'docker tag -- sha256:old-duckway-admin img-duckway-admin:rollback-20260102T030405Z'
assert "gateway image tagged" in_log 'docker tag -- sha256:old-duckway-gateway img-duckway-gateway:rollback-20260102T030405Z'
assert "CSP smoke checked" in_out 'Content-Security-Policy header present'
assert "events smoke checked" in_out '/api/events requires auth \(401\)'
assert "rollback command printed" in_out 'rollback --to backups/upgrade-20260102T030405Z'
D="$PROJ/backups/upgrade-20260102T030405Z"
assert "backup db exists" [ -s "$D/duckway.db" ]
assert "data files archive exists" [ -s "$D/data-files.tar" ]
assert "manifest exists" [ -s "$D/manifest.json" ]
if command -v python3 >/dev/null 2>&1; then
  python3 -I - "$D/manifest.json" "$D" <<'PY' && ok "manifest fields valid" || bad "manifest fields valid"
import hashlib, json, os, sys
m = json.load(open(sys.argv[1]))
d = sys.argv[2]
for k in ("schema", "status", "timestamp", "mode", "database_backend", "git_commit_before",
          "git_commit_after", "git_dirty", "version_before", "images", "backup_dir", "backup_files"):
    assert k in m, k
assert m["status"] == "complete", m["status"]
assert m["mode"] == "split" and m["database_backend"] == "sqlite"
assert len(m["git_commit_after"]) == 40
assert len(m["images"]) == 2
for i in m["images"]:
    for k in ("service", "container", "image_name", "previous_image_id", "rollback_tag"):
        assert i[k], k
    assert i["rollback_tag"].endswith(":rollback-20260102T030405Z")
names = {f["name"] for f in m["backup_files"]}
assert {"duckway.db", "data-files.tar"} <= names, names
for f in m["backup_files"]:
    h = hashlib.sha256(open(os.path.join(d, f["name"]), "rb").read()).hexdigest()
    assert h == f["sha256"], f["name"]
    assert f["bytes"] == os.path.getsize(os.path.join(d, f["name"]))
PY
else
  echo "  skip - python3 not installed; manifest JSON validation skipped"
fi
assert "manifest names previous image id" grep -q '"previous_image_id":"sha256:old-duckway-admin"' "$D/manifest.json"

# ================================================================ rollback (uses $D)
echo "# rollback confirmation and data handling"
: > "$LOG"
run_prod "wrong-name" -- rollback --to "backups/upgrade-20260102T030405Z"
assert "rollback with wrong confirmation exits non-zero" [ "$RC" -ne 0 ]
assert_not "wrong confirmation changes nothing" in_log "docker (tag|stop) |compose .* (up|run) "
assert "rollback warns about lost writes" in_out 'will be LOST'
: > "$LOG"
run_prod "__EOF__" -- rollback --to "backups/upgrade-20260102T030405Z"
assert "rollback with empty stdin exits non-zero" [ "$RC" -ne 0 ]
assert_not "empty stdin changes nothing" in_log "docker (tag|stop) |compose .* (up|run) "
: > "$LOG"
run_prod "upgrade-20260102T030405Z" -- rollback --to "backups/upgrade-20260102T030405Z"
assert "typed confirmation proceeds" [ "$RC" -eq 0 ]
assert "rollback verifies the previous admin image is running" in_out 'verified: duckway-admin runs the previous image sha256:old-duckway-admin'
assert "rollback verifies the previous gateway image is running" in_out 'verified: duckway-gateway runs the previous image sha256:old-duckway-gateway'
assert "rollback recreates with --force-recreate" in_log 'up -d --no-deps --force-recreate gateway-prod'
assert "stop before DB restore" before 'docker stop ' 'compose .* run --rm --no-deps -v .*restore'
assert "DB restore before re-tag" before 'compose .* run --rm' 'docker tag -- img-duckway-admin:rollback'
assert "re-tag before recreate" before 'docker tag -- img-duckway-admin:rollback' 'up -d --no-deps --force-recreate admin-prod'
assert "admin recreated before gateway" before 'up -d --no-deps --force-recreate admin-prod' 'up -d --no-deps --force-recreate gateway-prod'
assert "re-tag restores original image name" in_log 'docker tag -- img-duckway-admin:rollback-20260102T030405Z img-duckway-admin$'
: > "$LOG"
run_prod "" -- rollback --to "backups/upgrade-20260102T030405Z" --keep-data --yes
assert "--keep-data --yes proceeds" [ "$RC" -eq 0 ]
assert_not "--keep-data does not stop services" in_log 'docker stop '
assert_not "--keep-data skips DB restore" in_log 'compose .* run '
assert_not "--keep-data skips pg_restore" in_log 'pg_restore'
assert "--keep-data still re-tags images" in_log 'docker tag -- img-duckway-admin:rollback'
assert "--keep-data still recreates" in_log 'up -d --no-deps --force-recreate admin-prod'
: > "$LOG"
STUB_NO_ROLLBACK_IMAGE=1 run_prod "" -- rollback --to "backups/upgrade-20260102T030405Z" --yes
assert "missing rollback image fails" [ "$RC" -ne 0 ]
assert_not "missing rollback image changes nothing" in_log 'docker (tag|stop) |compose .* (up|run) '
run_prod "" -- rollback --to "backups/upgrade-does-not-exist" --yes
assert "unknown backup dir fails" [ "$RC" -ne 0 ]
run_prod "" -- rollback
assert "rollback without --to is a usage error" [ "$RC" -ne 0 ]

echo "# rollback mode mismatch"
cp -r "$D" "$ROOT/saved-manifest"
new_proj combined sqlite
mkdir -p "$PROJ/backups"
cp -r "$ROOT/saved-manifest" "$PROJ/backups/upgrade-20260102T030405Z"
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "mode mismatch refused" [ "$RC" -ne 0 ]
assert_not "mode mismatch changes nothing" in_log 'docker (tag|stop) |compose .* (up|run) '

echo "# rollback integrity: checksum, status, image refs"
restore_proj() { # restore_proj: fresh split project holding a pristine copy of the saved backup
  new_proj split sqlite
  mkdir -p "$PROJ/backups"
  cp -r "$ROOT/saved-manifest" "$PROJ/backups/upgrade-20260102T030405Z"
  RB="$PROJ/backups/upgrade-20260102T030405Z"
}
restore_proj
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "pristine copy rolls back" [ "$RC" -eq 0 ]
assert "rollback verifies checksums" in_out 'verified against manifest'
restore_proj
printf 'tampered' >> "$RB/duckway.db"
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "tampered db refused" [ "$RC" -ne 0 ]
assert "tampered db reports mismatch" in_out 'does not match the manifest'
assert_not "tampered db changes nothing" in_log 'docker (tag|stop) |compose .* (up|run) '
restore_proj
printf 'tampered' >> "$RB/data-files.tar"
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "tampered data archive refused" [ "$RC" -ne 0 ]
assert_not "tampered archive changes nothing" in_log 'docker (tag|stop) |compose .* (up|run) '
restore_proj
sed -i 's/"status": "complete"/"status": "prepared"/' "$RB/manifest.json"
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "unfinished manifest status refused" [ "$RC" -ne 0 ]
assert "unfinished status explains --force" in_out '\-\-force'
assert_not "unfinished status changes nothing" in_log 'docker (tag|stop) |compose .* (up|run) '
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes --force
assert "--force overrides unfinished status" [ "$RC" -eq 0 ]
restore_proj
sed -i 's/"status": "complete"/"status": "failed"/' "$RB/manifest.json"
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "failed status is rollbackable" [ "$RC" -eq 0 ]
restore_proj
sed -i 's|"rollback_tag":"img-duckway-admin:rollback-20260102T030405Z"|"rollback_tag":"-evil --flag"|' "$RB/manifest.json"
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "invalid image ref in manifest refused" [ "$RC" -ne 0 ]
assert_not "invalid image ref changes nothing" in_log 'docker (tag|stop) |compose .* (up|run) '

echo "# proxy probe hardening"
new_proj split sqlite DUCKWAY_ADMIN_PUBLIC_URL=http://user:secretpw@proxy.test
run_prod "" -- upgrade --check
assert "curl restricted to http(s) and uses --" in_log "curl .*--proto =http,https .* -- http://user:secretpw@proxy.test/admin/login"
assert_not "userinfo never printed" in_out 'secretpw'

echo "# rollback hardening: restore umask/sha, names, paths"
restore_proj
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "restore runs with umask 077 and chmod 600" in_log "umask 077; echo .*sha256sum -c .*chmod 600 /data/duckway.db.restore"
restore_proj
sed -i 's|"image_name":"img-duckway-admin"|"image_name":"evil/other:latest"|' "$RB/manifest.json"
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "manifest image name differing from the current one refused" [ "$RC" -ne 0 ]
assert "name mismatch explained" in_out 'refusing to retag an arbitrary image'
assert_not "name mismatch changes nothing" in_log 'docker (tag|stop) |compose .* (up|run) '
restore_proj
sed -i 's|"container":"duckway-admin"|"container":"some-other-container"|' "$RB/manifest.json"
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "manifest container outside the deployment refused" [ "$RC" -ne 0 ]
run_prod "" -- rollback --to "backups/a:b" --yes
assert "--to with colon rejected" [ "$RC" -eq 2 ]
run_prod "" -- rollback --to "backups/a,b" --yes
assert "--to with comma rejected" [ "$RC" -eq 2 ]
restore_proj
sed -i 's|"status": "complete"|"status": "build-failed"|' "$RB/manifest.json"
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "build-failed status still needs --force" [ "$RC" -ne 0 ]

echo "# newline-bearing values are rejected"
new_proj split sqlite
STAMP_OVERRIDE=$'20260102T030405Z\nextra' run_prod "" -- upgrade --yes
assert "stamp with newline rejected" [ "$RC" -eq 2 ]
assert_not "stamp with newline changes nothing" in_log "$MUTATING"

echo "# interrupted recreate leaves a rollbackable manifest"
new_proj split sqlite
STUB_KILL_ON_UP=1 run_prod "" -- upgrade --yes
assert "interrupted upgrade exits non-zero" [ "$RC" -ne 0 ]
assert "manifest status is recreating" grep -q '"status": "recreating"' "$PROJ/backups/upgrade-20260102T030405Z/manifest.json"
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "rollback after interruption needs no --force" [ "$RC" -eq 0 ]

echo "# containerd-style digest kinds do not false-fail"
new_proj split sqlite
STUB_CONTAINERD=1 run_prod "" -- upgrade --yes
assert "upgrade passes with differing digest kinds" [ "$RC" -eq 0 ]
new_proj split sqlite
STUB_CONTAINERD=1 STUB_NEVER_RECREATE=1 run_prod "" -- upgrade --yes
assert "stale image still fails hard with differing digest kinds" [ "$RC" -ne 0 ]

echo "# data volume hygiene and preflight"
new_proj split sqlite
run_prod "" -- upgrade --yes
assert "data-files archive excludes backup temp dirs" in_log "--exclude ./.backup-tmp-"
assert "stale backup temp dirs removed before the first mutation" before 'rm -rf /data/.backup-tmp-\*' 'docker tag '
assert "temp dir cleaned after the backup" in_log 'exec duckway-admin rm -rf /data/.backup-tmp-20260102T030405Z'
new_proj split sqlite
STUB_VOL_FREE_KB=10 run_prod "" -- upgrade --check
assert "tiny data volume is blocking" [ "$RC" -ne 0 ]
assert "volume free space reported" in_out 'Data volume free space: 10 KB'
new_proj split sqlite
STUB_FAIL_BACKUP=1 run_prod "" -- upgrade --yes
assert "failed backup cleans the temp dir" in_log 'exec duckway-admin rm -rf /data/.backup-tmp-20260102T030405Z'

echo "# rollback: tag identity, missing container, lock"
restore_proj
sed -i 's|"rollback_tag":"img-duckway-admin:rollback-20260102T030405Z"|"rollback_tag":"img-duckway-gateway:rollback-20260102T030405Z"|' "$RB/manifest.json"
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "rollback tag resolving to another image refused" [ "$RC" -ne 0 ]
assert "wrong tag explained" in_out 'refusing to retag'
assert_not "wrong tag changes nothing" in_log 'docker (tag|stop) |compose .* (up|run) '
restore_proj
STUB_NO_CONFIG_IMAGE=1 run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "rollback proceeds when containers no longer exist" [ "$RC" -eq 0 ]
assert "missing container noted" in_out 'does not exist; accepting manifest image'
restore_proj
sed -i 's|"image_name":"img-duckway-admin"|"image_name":"evil/other"|' "$RB/manifest.json"
STUB_NO_CONFIG_IMAGE=1 run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "missing container with foreign image name refused" [ "$RC" -ne 0 ]
restore_proj
mkdir -p "$PROJ/backups/.upgrade.lock"; echo "$$" > "$PROJ/backups/.upgrade.lock/pid"
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "rollback refused while a live lock is held" [ "$RC" -ne 0 ]
assert "live lock explained" in_out 'already running'
assert_not "live lock changes nothing" in_log 'docker (tag|stop) |compose .* (up|run) '
run_prod "" -- upgrade --yes
assert "upgrade refused while a live lock is held" [ "$RC" -ne 0 ]
echo 999999999 > "$PROJ/backups/.upgrade.lock/pid"
run_prod "" -- upgrade --yes
assert "stale lock refused with guidance" in_out 'stale lock'
assert "stale lock guidance names the removal command" in_out "rm -rf"
assert "lock directory kept for the operator" [ -d "$PROJ/backups/.upgrade.lock" ]
rm -rf "$PROJ/backups/.upgrade.lock"
STAMP_OVERRIDE=20270101T000000Z run_prod "" -- upgrade --yes
assert "upgrade runs once the lock is gone" [ "$RC" -eq 0 ]
assert "lock released after the run" [ ! -e "$PROJ/backups/.upgrade.lock" ]

# ================================================================ failure paths
echo "# failed backup (after tag+build) aborts before any recreate"
new_proj split sqlite
STUB_FAIL_BACKUP=1 run_prod "" -- upgrade --yes
assert "failed backup exits non-zero" [ "$RC" -ne 0 ]
assert "failed backup happened after tag and build" before 'docker compose .* build' 'backup-tmp-20260102T030405Z'
assert_not "failed backup: no recreate" in_log 'compose .* up '
assert "failed backup says nothing was changed" in_out 'nothing was changed'
assert "failed backup reports the kept directory" in_out 'safe to delete'
assert "failed backup lists directory contents" in_out 'manifest.json'
assert "failed backup marks manifest" grep -q '"status": "backup-failed"' "$PROJ/backups/upgrade-20260102T030405Z/manifest.json"
assert "failed backup leaves no partial files" bash -c "! ls '$PROJ'/backups/upgrade-20260102T030405Z/*.partial >/dev/null 2>&1"

echo "# new image without backup support aborts before recreate"
new_proj split sqlite
STUB_NO_BACKUP_SUPPORT=1 run_prod "" -- upgrade --yes
assert "unsupported backup exits non-zero" [ "$RC" -ne 0 ]
assert_not "unsupported backup: no real backup attempted" in_log 'umask 077; d=/data/.backup-tmp-'
assert "unsupported backup: probe container removed" in_log 'docker rm -f duckway-backup-20260102T030405Z'
assert_not "unsupported backup: no recreate" in_log 'compose .* up '
assert_not "unsupported backup: running container not asked" in_log 'exec .* backup'

echo "# corrupted backup stream is rejected"
new_proj split sqlite
STUB_BAD_STREAM_SHA=1 run_prod "" -- upgrade --yes
assert "bad stream exits non-zero" [ "$RC" -ne 0 ]
assert_not "bad stream: no recreate" in_log 'compose .* up '
assert "bad stream: no db kept" [ ! -e "$PROJ/backups/upgrade-20260102T030405Z/duckway.db" ]

echo "# runtime that only restarts containers (no image swap) is detected"
new_proj split sqlite
STUB_NEVER_RECREATE=1 run_prod "" -- upgrade --yes
assert "upgrade without image swap exits non-zero" [ "$RC" -ne 0 ]
assert "upgrade reports the stale image" in_out 'not running the newly built image'
assert "upgrade marks manifest failed" grep -q '"status": "failed"' "$PROJ/backups/upgrade-20260102T030405Z/manifest.json"
assert_not "upgrade does not claim completion" in_out 'Upgrade complete'
assert_not "gateway untouched after admin image mismatch" in_log 'up -d --no-deps --force-recreate gateway-prod'
restore_proj
mkdir -p "$STUB/state"
for c in duckway-admin duckway-gateway; do echo "sha256:new-$c" > "$STUB/state/ctr-$c"; done # containers run the upgraded images
STUB_NEVER_RECREATE=1 run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "rollback without image swap exits non-zero" [ "$RC" -ne 0 ]
assert "rollback fails loudly" in_out 'ROLLBACK FAILED'
assert "rollback gives manual guidance" in_out 'force-recreate'
assert_not "rollback does not claim completion" in_out 'Rollback complete'

echo "# early abort leaves no empty backup directory"
new_proj split sqlite
STUB_FAIL_TAG=1 run_prod "" -- upgrade --yes
assert "tag failure exits non-zero" [ "$RC" -ne 0 ]
assert "empty run directory removed" [ ! -e "$PROJ/backups/upgrade-20260102T030405Z" ]
new_proj split sqlite
STUB_FAIL_BUILD=1 run_prod "" -- upgrade --yes
assert "directory with a manifest is kept" [ -s "$PROJ/backups/upgrade-20260102T030405Z/manifest.json" ]

echo "# failed build leaves running services untouched"
new_proj split sqlite
STUB_FAIL_BUILD=1 run_prod "" -- upgrade --yes
assert "failed build exits non-zero" [ "$RC" -ne 0 ]
assert "failed build happened after tag" before 'docker tag ' 'compose .* build'
assert_not "failed build: no backup" in_log 'backup-tmp-20260102T030405Z'
assert_not "failed build: no recreate" in_log 'compose .* up '

echo "# invalid stamp and EOF confirmation"
new_proj split sqlite
STAMP_OVERRIDE='bad stamp; rm' run_prod "" -- upgrade --yes
assert "invalid stamp rejected" [ "$RC" -eq 2 ]
assert_not "invalid stamp changes nothing" in_log "$MUTATING"
run_prod "__EOF__" -- upgrade
assert "EOF at confirmation exits non-zero" [ "$RC" -ne 0 ]
assert "EOF at confirmation says Cancelled" in_out 'Cancelled'
assert_not "EOF at confirmation changes nothing" in_log "$MUTATING"

echo "# post-recreate smoke failure prints rollback, gateway untouched"
new_proj split sqlite
STUB_NO_CSP=1 run_prod "" -- upgrade --yes
assert "smoke failure exits non-zero" [ "$RC" -ne 0 ]
assert "admin was recreated" in_log 'up -d --no-deps --force-recreate admin-prod'
assert_not "gateway not recreated after admin smoke failure" in_log 'up -d --no-deps --force-recreate gateway-prod'
assert "exact rollback command printed" in_out 'rollback --to backups/upgrade-20260102T030405Z'
assert "failed smoke says to keep the directory" in_out 'Keep it'
assert "manifest marked failed" grep -q '"status": "failed"' "$PROJ/backups/upgrade-20260102T030405Z/manifest.json"

echo "# upgrade without --yes needs confirmation"
new_proj split sqlite
run_prod "n" -- upgrade
assert "declined confirmation exits non-zero" [ "$RC" -ne 0 ]
assert_not "declined confirmation changes nothing" in_log "$MUTATING"

# ================================================================ combined
echo "# combined mode"
new_proj combined sqlite
run_prod "" -- upgrade --yes
assert "combined upgrade exits 0" [ "$RC" -eq 0 ]
assert "combined builds server only" in_log 'build server-prod$'
assert "combined recreates server" in_log 'up -d --no-deps --force-recreate server-prod'
assert_not "combined never touches gateway" in_log 'gateway'
assert "combined backup runs as one-off server-prod container" in_log 'run --rm -T --no-deps --name duckway-backup-[^ ]+ --entrypoint sh server-prod -c .*backup-tmp-20260102T030405Z.*duckway-server backup --out'

# ================================================================ postgres
echo "# postgres backend"
new_proj split postgres
run_prod "" -- upgrade --yes
assert "postgres upgrade exits 0" [ "$RC" -eq 0 ]
assert "pg_dump runs in postgres container" in_log 'exec duckway-postgres sh -c'
assert_not "no sqlite backup command on postgres" in_log 'Usage: backup --out'
assert "tag before pg dump" before 'docker tag ' 'duckway-postgres sh -c'
assert "build before pg dump" before 'compose .* build' 'duckway-postgres sh -c'
PD="$PROJ/backups/upgrade-20260102T030405Z"
assert "pgdump file stored" [ -s "$PD/duckway.pgdump" ]
assert "manifest lists pgdump" grep -q '"name":"duckway.pgdump"' "$PD/manifest.json"
assert "postgres compose overlay used" in_log 'docker-compose.postgres.yml'
: > "$LOG"
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes
assert "postgres rollback exits 0" [ "$RC" -eq 0 ]
assert "postgres rollback restores via pg_restore" in_log 'exec -i duckway-postgres sh -c'
assert "postgres restore after stop" before 'docker stop ' 'duckway-postgres sh -c'
: > "$LOG"
run_prod "" -- rollback --to backups/upgrade-20260102T030405Z --yes --keep-data
assert "postgres --keep-data skips restore" bash -c "! grep -q 'duckway-postgres' '$LOG'"

# ================================================================ static checks
echo "# static checks"
assert "bash -n prod.sh" bash -n "$REPO_DIR/scripts/prod.sh"
assert "bash -n prod-upgrade-test.sh" bash -n "$REPO_DIR/scripts/prod-upgrade-test.sh"
if command -v shellcheck >/dev/null 2>&1; then
  assert "shellcheck prod-upgrade-test.sh" shellcheck -S warning "$REPO_DIR/scripts/prod-upgrade-test.sh"
  assert "shellcheck prod.sh" shellcheck -S error "$REPO_DIR/scripts/prod.sh"
else
  echo "  skip - shellcheck not installed"
fi

echo ""
echo "passed=$PASS failed=$FAIL"
[ "$FAIL" -eq 0 ]
