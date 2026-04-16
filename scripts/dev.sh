#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

cd "$PROJECT_DIR"

COMPOSE="docker compose -f docker-compose.yml -f docker-compose.dev.yml"

case "${1:-up}" in
  up|start)
    echo "Building and starting Duckway (dev mode) in Docker..."
    $COMPOSE up --build -d
    sleep 3
    echo ""
    echo "Server:   http://localhost:9090/admin/"
    echo "Username: duckway"
    echo "Password: duckway"
    echo ""
    echo "Logs:   $COMPOSE logs -f server"
    echo "Client: docker exec -it duckway-client sh"
    ;;

  restart)
    echo "Rebuilding and restarting..."
    $COMPOSE up --build -d
    sleep 3
    echo "Done."
    ;;

  down|stop)
    $COMPOSE down
    ;;

  nuke)
    echo "Removing containers + volumes (wipe data)..."
    $COMPOSE down -v
    ;;

  logs)
    $COMPOSE logs -f server
    ;;

  bare)
    # Old bare-metal mode for fast debugging
    shift
    echo "Running bare binary..."
    go build -o "$PROJECT_DIR/server" ./cmd/server/
    go build -o "$PROJECT_DIR/client" ./cmd/client/
    exec "$PROJECT_DIR/server" "$@"
    ;;

  *)
    echo "Usage: $0 {up|restart|down|nuke|logs|bare}"
    exit 1
    ;;
esac
