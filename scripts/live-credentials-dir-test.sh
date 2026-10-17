#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$ROOT/scripts/live-credentials-dir.sh"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/duckway-live-creds-test.XXXXXX")"
cleanup() {
  git -C "$MAIN" worktree remove --force "$LINKED" >/dev/null 2>&1 || true
  git -C "$MAIN" worktree prune >/dev/null 2>&1 || true
  rm -rf "$TEST_ROOT"
}
trap cleanup EXIT INT TERM
MAIN="$TEST_ROOT/main"
LINKED="$TEST_ROOT/linked"
SEPARATE_MAIN="$TEST_ROOT/separate-main"
SEPARATE_GIT="$TEST_ROOT/separate-git"
SEPARATE_LINKED="$TEST_ROOT/separate-linked"
mkdir -p "$MAIN" "$MAIN/shared" "$MAIN/env-dir" "$MAIN/config-dir"
git -C "$MAIN" init -q
git -C "$MAIN" -c user.name=test -c user.email=test@example.invalid commit --allow-empty -qm init

actual="$(env -u DUCKWAY_LIVE_CREDENTIALS_DIR bash -c 'source "$1/scripts/live-credentials-dir.sh"; duckway_resolve_live_credentials_dir "$2"' _ "$ROOT" "$MAIN")"
expected="$MAIN/live-credentials"
[ "$actual" = "$expected" ] || { echo "fallback mismatch" >&2; exit 1; }

git -C "$MAIN" config duckway.liveCredentialsDir config-dir
actual="$(DUCKWAY_LIVE_CREDENTIALS_DIR=env-dir bash -c 'source "$1/scripts/live-credentials-dir.sh"; duckway_resolve_live_credentials_dir "$2"' _ "$ROOT" "$MAIN")"
[ "$actual" = "$MAIN/env-dir" ] || { echo "environment override mismatch" >&2; exit 1; }
actual="$(env -u DUCKWAY_LIVE_CREDENTIALS_DIR bash -c 'source "$1/scripts/live-credentials-dir.sh"; duckway_resolve_live_credentials_dir "$2"' _ "$ROOT" "$MAIN")"
[ "$actual" = "$MAIN/config-dir" ] || { echo "git config mismatch" >&2; exit 1; }

git -C "$MAIN" config duckway.liveCredentialsDir './shared/../shared'
actual="$(env -u DUCKWAY_LIVE_CREDENTIALS_DIR bash -c 'source "$1/scripts/live-credentials-dir.sh"; duckway_resolve_live_credentials_dir "$2"' _ "$ROOT" "$MAIN")"
[ "$actual" = "$MAIN/shared" ] || { echo "relative path canonicalization mismatch" >&2; exit 1; }

# The config value is read from the common repository and anchored to its root,
# even when the resolver is invoked from a linked worktree.
git -C "$MAIN" worktree add -q -b test-linked "$LINKED" HEAD
actual="$(env -u DUCKWAY_LIVE_CREDENTIALS_DIR bash -c 'source "$1/scripts/live-credentials-dir.sh"; duckway_resolve_live_credentials_dir "$2"' _ "$ROOT" "$LINKED")"
[ "$actual" = "$MAIN/shared" ] || { echo "linked worktree root mismatch" >&2; exit 1; }

# A separate git directory is not located beside its primary checkout, so
# deriving the worktree root from --git-common-dir would choose the wrong base.
git init -q --separate-git-dir="$SEPARATE_GIT" "$SEPARATE_MAIN"
mkdir -p "$SEPARATE_MAIN/shared"
git -C "$SEPARATE_MAIN" -c user.name=test -c user.email=test@example.invalid commit --allow-empty -qm init
git -C "$SEPARATE_MAIN" config duckway.liveCredentialsDir shared
git -C "$SEPARATE_MAIN" worktree add -q -b separate-linked "$SEPARATE_LINKED" HEAD
actual="$(env -u DUCKWAY_LIVE_CREDENTIALS_DIR bash -c 'source "$1/scripts/live-credentials-dir.sh"; duckway_resolve_live_credentials_dir "$2"' _ "$ROOT" "$SEPARATE_MAIN")"
[ "$actual" = "$SEPARATE_MAIN/shared" ] || { echo "separate-git-dir primary root mismatch" >&2; exit 1; }
# Current Git versions may list the separate administrative directory as the
# primary worktree. From a linked worktree the checkout root cannot be derived
# through Git's worktree list, so require an explicit error rather than
# resolving relative credentials beside the git directory.
if output="$(env -u DUCKWAY_LIVE_CREDENTIALS_DIR bash -c 'source "$1/scripts/live-credentials-dir.sh"; duckway_resolve_live_credentials_dir "$2"' _ "$ROOT" "$SEPARATE_LINKED" 2>&1)"; then
  echo "separate-git-dir linked worktree unexpectedly resolved an ambiguous relative path" >&2
  exit 1
fi
case "$output" in
  *"cannot determine primary worktree root for separate-git-dir repository"*) ;;
  *) echo "separate-git-dir linked worktree error was unclear: $output" >&2; exit 1 ;;
esac

# Explicit per-agent overrides must continue to win in the child preflight.
# The missing mock paths contain no credential material; preflight exits before
# any container operation or live gate can run.
MOCK_BIN="$TEST_ROOT/mock-bin"
mkdir -p "$MOCK_BIN"
cat >"$MOCK_BIN/mock-runtime" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod 700 "$MOCK_BIN/mock-runtime"
for pair in codex:CODEX_AUTH claude:CLAUDE_AUTH; do
  agent="${pair%%:*}"
  variable="${pair#*:}"
  override="$TEST_ROOT/explicit-$agent-auth.json"
  if output="$(env DUCKLORD_DEMO_LOCK_HELD=1 CONTAINER_RUNTIME=mock-runtime \
    DUCKLORD_DEMO_AGENT_CREDENTIALS="$agent" PATH="$MOCK_BIN:$PATH" "$variable=$override" \
    "$ROOT/scripts/ducklord-podman-demo.sh" 2>&1)"; then
    echo "explicit $variable unexpectedly passed missing-file preflight" >&2
    exit 1
  fi
  case "$output" in
    *"$override"*) ;;
    *) echo "explicit $variable was not used by child preflight" >&2; exit 1 ;;
  esac
done

echo "live credential directory regression checks passed"
