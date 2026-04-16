#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

cd "$PROJECT_DIR"

case "${1:-}" in
  docker|compose)
    echo "Starting Duckway with Docker Compose..."
    docker compose up --build -d
    sleep 3
    echo ""
    echo "Server: http://localhost:${DUCKWAY_PORT:-8080}/admin/"
    echo "Logs:   docker compose logs -f server"
    echo ""
    docker compose logs server 2>&1 | grep "Password:" || true
    ;;

  down|stop)
    docker compose down
    ;;

  logs)
    docker compose logs -f server
    ;;

  *)
    echo "Building Duckway..."
    go build -o "$PROJECT_DIR/server" ./cmd/server/
    go build -o "$PROJECT_DIR/client" ./cmd/client/
    echo ""
    exec "$PROJECT_DIR/server" "$@"
    ;;
esac
