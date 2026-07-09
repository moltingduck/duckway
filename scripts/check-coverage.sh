#!/bin/sh
set -eu

# Keep a small buffer below the current rounded coverage so unrelated
# statement-count churn does not break local commits. Raise this intentionally
# when coverage meaningfully improves.
min_coverage="${DUCKWAY_COVERAGE_MIN:-37.5}"
coverprofile="$(mktemp "${TMPDIR:-/tmp}/duckway-coverage.XXXXXX")"
trap 'rm -f "$coverprofile"' EXIT

echo "[coverage] go test ./... -covermode=atomic -coverprofile"
go test ./... -covermode=atomic -coverprofile="$coverprofile"

total="$(go tool cover -func="$coverprofile" | awk '/^total:/ { sub(/%$/, "", $3); print $3 }')"
if [ -z "$total" ]; then
  echo "[coverage] failed to read total coverage" >&2
  exit 1
fi

awk -v total="$total" -v min="$min_coverage" 'BEGIN {
  if ((total + 0) + 0.000001 < (min + 0)) {
    printf("[coverage] total %.1f%% is below required %.1f%%\n", total, min) > "/dev/stderr"
    exit 1
  }
  printf("[coverage] total %.1f%% >= %.1f%%\n", total, min)
}'
