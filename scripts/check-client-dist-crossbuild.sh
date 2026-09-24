#!/usr/bin/env bash
set -euo pipefail

# Compile every binary and target used by Dockerfile's client-dist stage.
# This is a compile-only portability check: no cross-compiled artifacts are kept.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

for target in \
	"linux amd64" \
	"linux arm64" \
	"darwin amd64" \
	"darwin arm64"; do
	read -r goos goarch <<<"$target"
	for package in ./cmd/client ./cmd/ducklion ./cmd/ducklord; do
		echo "checking $goos/$goarch $package"
		GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
			go build -buildvcs=false -o /dev/null "$package"
	done
done
