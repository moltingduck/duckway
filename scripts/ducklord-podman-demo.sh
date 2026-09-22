#!/usr/bin/env bash
set -euo pipefail
# The regular demo keeps ducklord-dev/ducklord-demo by default. Set
# DUCKLORD_DEMO_NAME_PREFIX when a caller needs an isolated fixture namespace.

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/container-runtime.sh"
source "$ROOT/scripts/ducklord-demo-common.sh"
duckway_init_container_runtime
REQUIRE_OWNED_NETWORK=0
for arg in "$@"; do
  case "$arg" in
    --require-owned-network) REQUIRE_OWNED_NETWORK=1 ;;
    *) echo "[ducklord-demo] unknown option: $arg" >&2; exit 2 ;;
  esac
done
CODEX_AUTH="$ROOT/live-credentials/codex-auth.json"
CLAUDE_AUTH="$ROOT/live-credentials/claude-credentials.json"
WORK_PARENT="${WORK:-${TMPDIR:-/tmp}}"
RUNTIME="$CONTAINER_RUNTIME"
IMAGE="${IMAGE:-duckway-ducklord-demo:local}"
NAME_PREFIX="${DUCKLORD_DEMO_NAME_PREFIX:-}"
OWNER_TOKEN="${DUCKLORD_DEMO_OWNER_TOKEN:-ducklord-demo}"
case "$NAME_PREFIX" in
  *[!A-Za-z0-9_.-]*) echo "invalid DUCKLORD_DEMO_NAME_PREFIX=$NAME_PREFIX" >&2; exit 2 ;;
esac
if [ -n "$NAME_PREFIX" ]; then
  NAME_PREFIX="${NAME_PREFIX}-"
fi
demo_name() { printf '%s%s' "$NAME_PREFIX" "$1"; }
DEV_CONTAINER="$(demo_name ducklord-dev)"
CLIENT_A="$(demo_name ducklion-client-a)"
CLIENT_B="$(demo_name ducklion-client-b)"
CLIENT_C="$(demo_name ducklion-client-c)"
CLIENT_D="$(demo_name ducklion-client-d)"
NET_WAS_SUPPLIED=0
if [ "${NET+x}" = x ]; then
  NET_WAS_SUPPLIED=1
fi
NET="${NET:-${NAME_PREFIX}ducklord-demo}"
CREDENTIALS="${DUCKLORD_DEMO_AGENT_CREDENTIALS:-all}"
CREDENTIAL_CLIENTS="${DUCKLORD_DEMO_CREDENTIAL_CLIENTS:-all}"
INCLUDE_CLIENT_C="${DUCKLORD_DEMO_INCLUDE_CLIENT_C:-1}"
INCLUDE_CLIENT_D="${DUCKLORD_DEMO_INCLUDE_CLIENT_D:-1}"

case "$CREDENTIALS" in all|codex|claude|none) ;; *) echo "invalid DUCKLORD_DEMO_AGENT_CREDENTIALS=$CREDENTIALS" >&2; exit 2;; esac
case "$CREDENTIAL_CLIENTS" in all|client-a|client-b|client-c|client-d) ;; *) echo "invalid DUCKLORD_DEMO_CREDENTIAL_CLIENTS=$CREDENTIAL_CLIENTS" >&2; exit 2;; esac
case "$INCLUDE_CLIENT_D" in 0|1) ;; *) echo "invalid DUCKLORD_DEMO_INCLUDE_CLIENT_D=$INCLUDE_CLIENT_D" >&2; exit 2;; esac
if [ "$CREDENTIALS" = all ] || [ "$CREDENTIALS" = codex ]; then ducklord_require_demo_secret "$CODEX_AUTH"; fi
if [ "$CREDENTIALS" = all ] || [ "$CREDENTIALS" = claude ]; then ducklord_require_demo_secret "$CLAUDE_AUTH"; fi

if [ "${DUCKLORD_DEMO_LOCK_HELD:-0}" != 1 ]; then
	ducklord_reexec_with_demo_lock "$0" "$@"
fi

[ -d "$WORK_PARENT" ] && [ ! -L "$WORK_PARENT" ] || { echo "demo work parent must be a non-symlink directory: $WORK_PARENT" >&2; exit 2; }
WORK="$(mktemp -d "$WORK_PARENT/ducklord-podman-demo.XXXXXX")"
cleanup_work() { rm -rf -- "$WORK"; }
trap cleanup_work EXIT

cleanup_existing() {
  for resource in "$DEV_CONTAINER" "$CLIENT_A" "$CLIENT_B" "$CLIENT_C" "$CLIENT_D"; do
    if [ "$("$RUNTIME" container inspect -f '{{ index .Config.Labels "ducklord.demo.owner" }}' "$resource" 2>/dev/null || true)" = "$OWNER_TOKEN" ]; then
      "$RUNTIME" rm -f "$resource" >/dev/null 2>&1 || true
    fi
  done
}

echo "[ducklord-demo] building local binaries"
CGO_ENABLED=0 go build -o "$WORK/ducklord" "$ROOT/cmd/ducklord"
CGO_ENABLED=0 go build -o "$WORK/duckway" "$ROOT/cmd/client"
CGO_ENABLED=0 go build -o "$WORK/ducklion" "$ROOT/cmd/ducklion"

ssh-keygen -q -t ed25519 -N '' -f "$WORK/id_ed25519"
printf '%s\n' '{"hasCompletedOnboarding":true,"theme":"dark","installMethod":"global"}' >"$WORK/claude-demo-state.json"

cat >"$WORK/Containerfile" <<'EOF'
FROM alpine:3.21
RUN apk add --no-cache openssh openssh-client bash zsh vim ca-certificates ncurses nodejs npm \
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
# A caller may own NET (the TUI E2E does). Inspect first and only create when
# absent; never remove or recreate a supplied network. For the default managed
# network, refuse to adopt an existing network with a different owner. The
# inspect/create pair is safe under a concurrent creator because a failed
# create is accepted only after a successful re-inspect.
if ! "$RUNTIME" network inspect "$NET" >/dev/null 2>&1; then
  if ! "$RUNTIME" network create --label "ducklord.demo.owner=$OWNER_TOKEN" "$NET" >/dev/null 2>&1; then
    if { [ "$NET_WAS_SUPPLIED" = 0 ] || [ "$REQUIRE_OWNED_NETWORK" = 1 ]; } && [ "$("$RUNTIME" network inspect -f '{{ index .Labels "ducklord.demo.owner" }}' "$NET" 2>/dev/null || true)" != "$OWNER_TOKEN" ]; then
      echo "[ducklord-demo] network $NET was created by another owner" >&2
      exit 1
    fi
  fi
elif { [ "$NET_WAS_SUPPLIED" = 0 ] || [ "$REQUIRE_OWNED_NETWORK" = 1 ]; } && [ "$("$RUNTIME" network inspect -f '{{ index .Labels "ducklord.demo.owner" }}' "$NET" 2>/dev/null || true)" != "$OWNER_TOKEN" ]; then
  echo "[ducklord-demo] default network $NET is owned by another owner; set NET to use a caller-supplied shared network" >&2
  exit 1
fi

echo "[ducklord-demo] starting remote clients"
"$RUNTIME" run -d --label "ducklord.demo.owner=$OWNER_TOKEN" --name "$CLIENT_A" --hostname client-a --network "$NET" "$IMAGE" >/dev/null
"$RUNTIME" run -d --label "ducklord.demo.owner=$OWNER_TOKEN" --name "$CLIENT_B" --hostname client-b --network "$NET" "$IMAGE" >/dev/null
"$RUNTIME" run -d --label "ducklord.demo.owner=$OWNER_TOKEN" --name "$CLIENT_C" --hostname client-c --network "$NET" "$IMAGE" >/dev/null
if [ "$INCLUDE_CLIENT_D" = 1 ]; then
  "$RUNTIME" run -d --label "ducklord.demo.owner=$OWNER_TOKEN" --name "$CLIENT_D" --hostname client-d --network "$NET" "$IMAGE" >/dev/null
  "$RUNTIME" exec "$CLIENT_D" rm /usr/local/bin/ducklion
  "$RUNTIME" exec -u duck "$CLIENT_D" sh -lc 'test ! -e /usr/local/bin/ducklion && test ! -e "$HOME/.duckway/ducklion/management.json"'
fi
for container in "$CLIENT_A" "$CLIENT_B" "$CLIENT_C"; do
  "$RUNTIME" exec -d -u duck "$container" sh -lc 'mkdir -p $HOME/.duckway; nohup ducklion daemon >$HOME/.duckway/ducklion-daemon.log 2>&1 </dev/null & echo $! >$HOME/.duckway/ducklion-daemon.pid' >/dev/null
done
if [ "$CREDENTIALS" != none ] && { [ -f "$CODEX_AUTH" ] || [ -f "$CLAUDE_AUTH" ]; }; then
  echo "[ducklord-demo] injecting available local agent credentials into remote demo users"
  for container in "$CLIENT_A" "$CLIENT_B" "$CLIENT_C" "$CLIENT_D"; do
    if [ "$container" = "$CLIENT_D" ] && [ "$INCLUDE_CLIENT_D" != 1 ]; then continue; fi
    if [ "$CREDENTIAL_CLIENTS" != all ] && [ "$container" != "$(demo_name "ducklion-$CREDENTIAL_CLIENTS")" ]; then
      continue
    fi
    "$RUNTIME" exec -u duck "$container" sh -lc 'mkdir -p $HOME/.codex $HOME/.claude && chmod 700 $HOME/.codex $HOME/.claude'
    if { [ "$CREDENTIALS" = all ] || [ "$CREDENTIALS" = codex ]; } && [ -f "$CODEX_AUTH" ]; then
      "$RUNTIME" cp "$CODEX_AUTH" "$container":/home/duck/.codex/auth.json
      "$RUNTIME" exec "$container" chown duck:duck /home/duck/.codex/auth.json
      "$RUNTIME" exec "$container" chmod 600 /home/duck/.codex/auth.json
      "$RUNTIME" cp "$ROOT/scripts/fixtures/ducklord-live-codex-config.toml" "$container":/home/duck/.codex/config.toml
      "$RUNTIME" exec "$container" chown duck:duck /home/duck/.codex/config.toml
      "$RUNTIME" exec "$container" chmod 600 /home/duck/.codex/config.toml
    fi
    if { [ "$CREDENTIALS" = all ] || [ "$CREDENTIALS" = claude ]; } && [ -f "$CLAUDE_AUTH" ]; then
      "$RUNTIME" cp "$CLAUDE_AUTH" "$container":/home/duck/.claude/.credentials.json
      "$RUNTIME" exec "$container" chown duck:duck /home/duck/.claude/.credentials.json
      "$RUNTIME" exec "$container" chmod 600 /home/duck/.claude/.credentials.json
      "$RUNTIME" cp "$WORK/claude-demo-state.json" "$container":/tmp/claude-demo-state.json
      "$RUNTIME" exec "$container" install -o duck -g duck -m 600 /tmp/claude-demo-state.json /home/duck/.claude.json
      "$RUNTIME" exec "$container" rm -f /tmp/claude-demo-state.json
    fi
  done
fi
if [ "$CREDENTIALS" = all ] && [ "$CREDENTIAL_CLIENTS" = all ]; then
  for container in "$CLIENT_A" "$CLIENT_B" "$CLIENT_C" "$CLIENT_D"; do
    if [ "$container" = "$CLIENT_D" ] && [ "$INCLUDE_CLIENT_D" != 1 ]; then continue; fi
    "$RUNTIME" exec "$container" sh -lc 'test "$(stat -c %U:%G:%a /home/duck/.codex/auth.json)" = duck:duck:600 && test "$(stat -c %U:%G:%a /home/duck/.claude/.credentials.json)" = duck:duck:600'
  done
fi
for container in "$CLIENT_A" "$CLIENT_B" "$CLIENT_C"; do
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
"$RUNTIME" run -d --label "ducklord.demo.owner=$OWNER_TOKEN" --name $DEV_CONTAINER --hostname dev-laptop --network "$NET" "$IMAGE" sleep infinity >/dev/null

# Keep Ducklord's managed repository separate from agent-loaded paths. These
# small fixtures make push and pull behaviour visible in every demo.
"$RUNTIME" exec "$DEV_CONTAINER" install -d -m 700 /root/.ducklord/skills/ducklord-quack
"$RUNTIME" cp "$ROOT/scripts/fixtures/ducklord-demo-skills/ducklord-quack/SKILL.md" "$DEV_CONTAINER":/root/.ducklord/skills/ducklord-quack/SKILL.md
"$RUNTIME" exec "$DEV_CONTAINER" chmod 600 /root/.ducklord/skills/ducklord-quack/SKILL.md
"$RUNTIME" exec -u duck "$CLIENT_A" sh -lc 'install -d -m 700 "$HOME/.codex/skills/client-a-meow"'
"$RUNTIME" cp "$ROOT/scripts/fixtures/ducklord-demo-skills/client-a-meow/SKILL.md" "$CLIENT_A":/home/duck/.codex/skills/client-a-meow/SKILL.md
"$RUNTIME" exec "$CLIENT_A" chown duck:duck /home/duck/.codex/skills/client-a-meow/SKILL.md
"$RUNTIME" exec "$CLIENT_A" chmod 600 /home/duck/.codex/skills/client-a-meow/SKILL.md
"$RUNTIME" cp "$WORK/id_ed25519" $DEV_CONTAINER:/root/.ssh/id_ed25519
"$RUNTIME" exec $DEV_CONTAINER sh -lc 'chmod 600 /root/.ssh/id_ed25519 && cat >/root/.ssh/config <<EOF
Host client-a client-b client-c client-d
  User duck

Host *
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
EOF'
"$RUNTIME" exec $DEV_CONTAINER sh -lc 'umask 077; cat >/root/.ducklord/config.yaml <<EOF
name: dev-laptop
hosts:
  - name: client-a
    host: client-a
    user: duck
    group: lab
    skill_targets:
      - id: codex
        path: /home/duck/.codex/skills
    selected_skills:
      - ducklord-quack
  - name: client-b
    host: client-b
    user: duck
    group: lab
EOF'
if [ "$INCLUDE_CLIENT_C" = 1 ]; then
  "$RUNTIME" exec $DEV_CONTAINER sh -lc 'cat >>/root/.ducklord/config.yaml <<EOF
  - name: client-c
    host: client-c
    user: duck
    group: lab
EOF'
fi

echo "[ducklord-demo] verifying managed skill fixtures"
"$RUNTIME" exec "$DEV_CONTAINER" grep -F '呱呱' /root/.ducklord/skills/ducklord-quack/SKILL.md >/dev/null
"$RUNTIME" exec -u duck "$CLIENT_A" grep -F '喵喵' /home/duck/.codex/skills/client-a-meow/SKILL.md >/dev/null

echo "[ducklord-demo] creating sample remote sessions"
if [ "$INCLUDE_CLIENT_D" = 1 ]; then
  "$RUNTIME" exec $DEV_CONTAINER sh -lc 'ssh client-d "command -v ducklion >/dev/null 2>&1"' && {
    echo "[ducklord-demo] client-d unexpectedly has Ducklion installed" >&2
    exit 1
  }
fi
"$RUNTIME" exec -u duck $CLIENT_A sh -lc 'mkdir -p /home/duck/projects/alpha && ducklion bookmarks --add /home/duck/projects/alpha --name alpha-project' >/dev/null
"$RUNTIME" exec -u duck $CLIENT_B sh -lc 'mkdir -p /home/duck/projects/beta && ducklion bookmarks --add /home/duck/projects/beta --name beta-project' >/dev/null
"$RUNTIME" exec -u duck $CLIENT_C sh -lc 'mkdir -p /home/duck/projects/gamma && ducklion bookmarks --add /home/duck/projects/gamma --name gamma-project' >/dev/null
"$RUNTIME" exec $DEV_CONTAINER ducklord start client-a --name alpha --kind shell --cwd /home/duck -- bash >/dev/null
"$RUNTIME" exec $DEV_CONTAINER ducklord start client-a --name bash --kind shell --cwd /home/duck -- bash >/dev/null
"$RUNTIME" exec $DEV_CONTAINER ducklord start client-a --name build --kind shell --cwd /home/duck -- bash >/dev/null
"$RUNTIME" exec $DEV_CONTAINER ducklord start client-b --name beta --kind shell --cwd /home/duck -- bash >/dev/null
"$RUNTIME" exec $DEV_CONTAINER ducklord start client-b --name zsh --kind shell --cwd /home/duck -- zsh >/dev/null
if [ "$INCLUDE_CLIENT_C" = 1 ]; then
  "$RUNTIME" exec $DEV_CONTAINER ducklord start client-c --name sh --kind shell --cwd /home/duck -- sh >/dev/null
fi
"$RUNTIME" exec $DEV_CONTAINER ducklord send client-a alpha 'i=0; while :; do i=$((i+1)); echo client-a alpha tick $i; sleep 4; done' >/dev/null
"$RUNTIME" exec $DEV_CONTAINER ducklord send client-a build 'i=0; while :; do i=$((i+1)); echo client-a build output $i; sleep 6; done' >/dev/null
"$RUNTIME" exec $DEV_CONTAINER ducklord send client-b beta 'i=0; while :; do i=$((i+1)); echo client-b beta tick $i; sleep 5; done' >/dev/null

echo "[ducklord-demo] verifying daemon inventory, PTY input, and recovery"
agents_ready=false
for _ in $(seq 1 20); do
  agents="$($RUNTIME exec $DEV_CONTAINER ducklord agents client-a /home/duck --config /root/.ducklord/config.yaml 2>/dev/null)" || agents=""
  if grep -q '^shell' <<<"$agents"; then agents_ready=true; break; fi
  sleep .25
done
if [ "$agents_ready" != true ]; then
  echo "[ducklord-demo] remote agent discovery did not report the interactive shell" >&2
  exit 1
fi
sessions="$($RUNTIME exec $DEV_CONTAINER ducklord sessions client-a --config /root/.ducklord/config.yaml)"
grep -q 'alpha.*running' <<<"$sessions"
grep -q 'bash.*running' <<<"$sessions"
grep -q 'build.*running' <<<"$sessions"
"$RUNTIME" exec $DEV_CONTAINER ducklord sessions client-b --config /root/.ducklord/config.yaml | grep -q 'zsh.*running'
if [ "$INCLUDE_CLIENT_C" = 1 ]; then
  "$RUNTIME" exec $DEV_CONTAINER ducklord sessions client-c --config /root/.ducklord/config.yaml | grep -q 'sh.*running'
fi
"$RUNTIME" exec $DEV_CONTAINER ducklord send client-a bash 'printf ducklord-e2e-ready' --config /root/.ducklord/config.yaml >/dev/null
ready=false
for _ in $(seq 1 50); do
  if "$RUNTIME" exec $DEV_CONTAINER ducklord read client-a bash --lines 20 --config /root/.ducklord/config.yaml | grep -q ducklord-e2e-ready; then
    ready=true
    break
  fi
  sleep 0.05
done
if [ "$ready" != true ]; then
  echo "[ducklord-demo] PTY input/output assertion failed" >&2
  exit 1
fi
"$RUNTIME" exec -u duck $CLIENT_A sh -lc 'kill "$(cat $HOME/.duckway/ducklion-daemon.pid)"'
"$RUNTIME" exec -d -u duck $CLIENT_A sh -lc 'mkdir -p $HOME/.duckway; nohup ducklion daemon >$HOME/.duckway/ducklion-daemon.log 2>&1 </dev/null & echo $! >$HOME/.duckway/ducklion-daemon.pid' >/dev/null
recovered=false
for _ in $(seq 1 100); do
  if "$RUNTIME" exec $DEV_CONTAINER ducklord sessions client-a --config /root/.ducklord/config.yaml 2>/dev/null | grep -q 'bash.*running'; then
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
"$RUNTIME" exec $DEV_CONTAINER ducklord start client-a --name lifecycle --kind shell --cwd /home/duck -- bash >/dev/null
restart_output="$("$RUNTIME" exec $DEV_CONTAINER ducklord restart client-a lifecycle --config /root/.ducklord/config.yaml)"
grep -q 'Restarted .* (generation 2)' <<<"$restart_output"
if force_output="$("$RUNTIME" exec $DEV_CONTAINER ducklord restart client-a lifecycle --force --config /root/.ducklord/config.yaml 2>&1)"; then
  echo "[ducklord-demo] shell restart unexpectedly accepted --force" >&2
  exit 1
fi
grep -q 'immediate.*do not accept wait or force' <<<"$force_output"
"$RUNTIME" exec $DEV_CONTAINER ducklord send client-a lifecycle 'printf "ducklord-lifecycle-generation-2\\n"' --config /root/.ducklord/config.yaml >/dev/null
lifecycle_ready=false
for _ in $(seq 1 50); do
  if "$RUNTIME" exec $DEV_CONTAINER ducklord read client-a lifecycle --lines 20 --config /root/.ducklord/config.yaml | grep -q ducklord-lifecycle-generation-2; then
    lifecycle_ready=true
    break
  fi
  sleep 0.05
done
if [ "$lifecycle_ready" != true ]; then
  echo "[ducklord-demo] restarted shell did not accept PTY input" >&2
  exit 1
fi
"$RUNTIME" exec $DEV_CONTAINER ducklord end client-a lifecycle --config /root/.ducklord/config.yaml | grep -q 'Ended '
if "$RUNTIME" exec $DEV_CONTAINER ducklord sessions client-a --config /root/.ducklord/config.yaml | grep -q 'lifecycle'; then
  echo "[ducklord-demo] ended shell remains in active inventory" >&2
  exit 1
fi
if ! "$RUNTIME" exec $DEV_CONTAINER ducklord retained client-a --config /root/.ducklord/config.yaml | grep -q 'lifecycle'; then
  echo "[ducklord-demo] ended shell is missing from retained diagnostics" >&2
  exit 1
fi
if ! "$RUNTIME" exec -u duck $CLIENT_A sh -lc 'find "$HOME/.duckway/ducklion/sessions" -name "output.*" -type f -exec stat -c %a {} \; | grep -q . && ! find "$HOME/.duckway/ducklion/sessions" -name "output.*" -type f -exec stat -c %a {} \; | grep -vx 600'; then
  echo "[ducklord-demo] retained output files are missing or not mode 0600" >&2
  exit 1
fi

cat <<EOF
[ducklord-demo] ready

client-d is reachable over SSH but has no Ducklion and is not yet in Ducklord's host list.
In the TUI press a, choose Standalone, then select client-d to test installation.
For Duckway proxy integration, configure Duckway on client-d first, then run
  $RUNTIME exec -it -u duck $CLIENT_D sh -lc 'duckway integrate ducklion'

Open the dev laptop TUI:
  $RUNTIME exec -it $DEV_CONTAINER ducklord tui --config /root/.ducklord/config.yaml

Useful checks:
  $RUNTIME exec $DEV_CONTAINER ducklord clients --config /root/.ducklord/config.yaml
  $RUNTIME exec $DEV_CONTAINER ducklord ssh-hosts
  $RUNTIME exec $DEV_CONTAINER ducklord probe client-a --config /root/.ducklord/config.yaml
  $RUNTIME exec $DEV_CONTAINER ducklord sessions client-a --config /root/.ducklord/config.yaml
  $RUNTIME exec $DEV_CONTAINER ducklord bookmarks client-a --config /root/.ducklord/config.yaml
  $RUNTIME exec $DEV_CONTAINER cat /root/.ducklord/skills/ducklord-quack/SKILL.md
  $RUNTIME exec -u duck $CLIENT_A cat /home/duck/.codex/skills/client-a-meow/SKILL.md
  $RUNTIME exec $DEV_CONTAINER ducklord read client-a alpha --lines 20 --config /root/.ducklord/config.yaml
  $RUNTIME exec -it $DEV_CONTAINER ducklord attach-host client-a --config /root/.ducklord/config.yaml

Inside the TUI:
  j/k or arrow keys: move through the quick Session list pane
  t: cycle Session sort (event time / importance / Host / type); Ctrl-T: reverse event-time direction
  b: move keyboard focus to the Project pane; e creates a Project, p adds a Session pane
  i (while Project pane focused): toggle notification focus for the visible Project
  l: open the detailed Session list; / searches, f filters, Enter focuses, g jumps to its Project
  Enter: focus the selected Session pane (through Ducklion writer control)
  a: add another ducklion host from ~/.ssh/config
  c: create a shell-first Session; launch Codex or Claude inside that shell
  m: open the centered action menu for attach, yield, notifications, and lifecycle
  n: configure notifications for the selected session
  E / R / X: end, restart, or destroy the selected session (with confirmation)
  Terminal area: one Project's tabs and split Session panes
  Ctrl-B then arrows: focus a neighboring pane; Ctrl-B then PageUp/PageDown: switch tabs
  Ctrl-]: return keyboard focus to the current navigation pane
  ?: pinned searchable shortcut help
  q: quit

Clean up:
  $RUNTIME rm -f $DEV_CONTAINER $CLIENT_A $CLIENT_B $CLIENT_C $CLIENT_D
  Remove network $NET only if this demo created it and it is dedicated to
  this run; keep a caller supplied or shared network.
EOF
