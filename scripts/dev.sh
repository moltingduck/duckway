#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

cd "$PROJECT_DIR"

echo "Building Duckway..."
go build -o "$PROJECT_DIR/server" ./cmd/server/
go build -o "$PROJECT_DIR/client" ./cmd/client/

echo ""
exec "$PROJECT_DIR/server" "$@"
