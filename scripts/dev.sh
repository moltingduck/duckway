#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

cd "$PROJECT_DIR"

# Load .dev.env if exists
if [ -f "$PROJECT_DIR/.dev.env" ]; then
  set -a
  . "$PROJECT_DIR/.dev.env"
  set +a
fi

MODE="${DUCKWAY_MODE:-combined}"
COMPOSE="docker compose -f docker-compose.yml -f docker-compose.dev.yml --profile $MODE"

case "${1:-up}" in
  up|start)
    echo "Building and starting Duckway ($MODE mode) in Docker..."
    $COMPOSE up --build -d
    sleep 4

    # Auto-seed dev data
    if [ "$MODE" = "split" ]; then
      DUCKWAY_URL=http://127.0.0.1:9099 "$SCRIPT_DIR/seed-dev.sh"
    else
      "$SCRIPT_DIR/seed-dev.sh"
    fi

    echo ""
    if [ "$MODE" = "split" ]; then
      echo "Admin:   http://localhost:9099/admin/ (management only)"
      echo "Gateway: http://localhost:8080 (proxy + client API)"
    else
      echo "Server:  http://localhost:9090/admin/ (combined)"
    fi
    echo "Username: duckway"
    echo "Password: duckway"
    echo ""
    echo "Mode: $MODE (set DUCKWAY_MODE=split for separate admin/gateway)"
    echo "Logs:   $COMPOSE logs -f"
    echo "Client: docker exec -it duckway-client sh"
    ;;

  restart)
    echo "Rebuilding ($MODE mode)..."
    $COMPOSE up --build -d
    sleep 3
    echo "Done."
    ;;

  down|stop)
    $COMPOSE down
    ;;

  nuke)
    echo "Removing containers + volumes..."
    $COMPOSE down -v
    ;;

  logs)
    $COMPOSE logs -f
    ;;

  split)
    export DUCKWAY_MODE=split
    exec "$0" up
    ;;

  combined)
    export DUCKWAY_MODE=combined
    exec "$0" up
    ;;

  bare)
    shift
    echo "Running bare binary..."
    go build -o "$PROJECT_DIR/server" ./cmd/server/
    go build -o "$PROJECT_DIR/client" ./cmd/client/
    exec "$PROJECT_DIR/server" "$@"
    ;;

  *)
    echo "Usage: $0 {up|restart|down|nuke|logs|split|combined|bare}"
    echo ""
    echo "Modes:"
    echo "  combined  — admin + gateway on one port (default)"
    echo "  split     — admin (:9090) + gateway (:8080) on separate ports"
    echo ""
    echo "Set DUCKWAY_MODE=split in .env for persistent split mode"
    exit 1
    ;;
esac
