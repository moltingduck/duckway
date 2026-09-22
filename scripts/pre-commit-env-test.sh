#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d -t duckway-precommit-env-test-XXXXXX)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/work/bin"
parent_config_before="$(git config --global --list --show-origin 2>/dev/null || true)"
git -C "$TMP/work" init -q
parent_config_after="$(git config --global --list --show-origin 2>/dev/null || true)"
[[ "$parent_config_before" == "$parent_config_after" ]]
cat >"$TMP/work/bin/go" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ -z "${GIT_DIR:-}" && -z "${GIT_WORK_TREE:-}" && -z "${GIT_INDEX_FILE:-}" ]]
printf '%s\n' "${PWD}" >"${DUCKWAY_PRECOMMIT_TEST_ROOT:?}/go-pwd"
EOF
chmod +x "$TMP/work/bin/go"

# Use a fixture repository and a fake go command. The fake command proves the
# environment cleanup is visible to the child process and that the hook keeps
# the fixture as its working tree.
DUCKWAY_PRECOMMIT_TEST_ROOT="$TMP" PATH="$TMP/work/bin:$PATH" \
  bash -c '
    cd "$1"
    export GIT_DIR="$3" GIT_INDEX_FILE="$4"
    if "$PWD/bin/go" test; then
      echo "fake go unexpectedly passed before hook cleanup" >&2
      exit 1
    fi
    . "$2/scripts/pre-commit-env.sh"
    duckway_prepare_precommit
    go test
  ' bash "$TMP/work" "$ROOT" "$TMP/work/.git" "$TMP/missing.index"

[[ "$(cat "$TMP/go-pwd")" == "$TMP/work" ]]
echo "pre-commit environment PASS"
