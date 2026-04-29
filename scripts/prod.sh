#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

cd "$PROJECT_DIR"

# Load .prod.env
if [ ! -f "$PROJECT_DIR/.prod.env" ]; then
  echo "Error: .prod.env not found. Copy .env.example to .prod.env and fill in values."
  echo "  cp .env.example .prod.env"
  exit 1
fi

set -a
. "$PROJECT_DIR/.prod.env"
set +a

MODE="${DUCKWAY_PROD_MODE:-split}"
USE_TAILSCALE="${DUCKWAY_TAILSCALE:-true}"
TS_HOSTNAME="$TS_HOSTNAME"

if [ "$USE_TAILSCALE" = "true" ]; then
  if [ -z "$TS_AUTHKEY" ]; then
    echo "Error: TS_AUTHKEY not set in .prod.env"
    echo "  Get one from https://login.tailscale.com/admin/settings/keys"
    echo "  Or set DUCKWAY_TAILSCALE=false to run without Tailscale"
    exit 1
  fi
  case "$MODE" in
    split)    PROFILES="--profile tailscale" ;;
    combined) PROFILES="--profile tailscale-combined" ;;
    *) echo "Error: DUCKWAY_PROD_MODE must be 'split' or 'combined'"; exit 1 ;;
  esac
else
  case "$MODE" in
    split)    PROFILES="--profile prod-split" ;;
    combined) PROFILES="--profile prod" ;;
    *) echo "Error: DUCKWAY_PROD_MODE must be 'split' or 'combined'"; exit 1 ;;
  esac
fi

COMPOSE="docker compose -f docker-compose.yml $PROFILES"

# Stamp builds with the current git revision so `duckway version` reports it.
export DUCKWAY_VERSION="$(git -C "$PROJECT_DIR" describe --tags --always --dirty 2>/dev/null || echo docker)"

case "${1:-up}" in
  up|start)
    echo "Starting Duckway production ($MODE mode + Tailscale)..."
    $COMPOSE up --build -d
    sleep 5

    echo ""
    if [ "$USE_TAILSCALE" = "true" ]; then
      if [ "$MODE" = "split" ]; then
        echo "Admin:   http://$TS_HOSTNAME-admin/  (port 80, tailnet only)"
        echo "Gateway: http://$TS_HOSTNAME-gw/     (port 80, tailnet only)"
      else
        echo "Server:  http://$TS_HOSTNAME/        (port 80, tailnet only)"
      fi
    else
      echo "No ports exposed. Use a reverse proxy to reach the containers."
      if [ "$MODE" = "split" ]; then
        echo "Admin:   duckway-admin:9091"
        echo "Gateway: duckway-gateway:8080"
      else
        echo "Server:  duckway-server:8080"
      fi
    fi
    echo ""
    echo "Admin password (first run only):"
    $COMPOSE logs 2>&1 | grep "Password:" | tail -1 || echo "  (check: $0 logs)"
    echo ""
    echo "Tailscale nodes:"
    $COMPOSE ps --format "table {{.Name}}\t{{.Status}}" 2>/dev/null | grep -E "NAME|ts-|tailscale" || true
    ;;

  down|stop)
    echo "Stopping..."
    $COMPOSE down
    ;;

  restart)
    echo "Rebuilding ($MODE mode)..."
    $COMPOSE up --build -d
    ;;

  nuke)
    echo "Removing everything including data and Tailscale state..."
    read -p "Are you sure? This deletes all data. [y/N] " confirm
    if [ "$confirm" = "y" ] || [ "$confirm" = "Y" ]; then
      $COMPOSE down -v
      echo "Done."
    else
      echo "Cancelled."
    fi
    ;;

  logs)
    $COMPOSE logs -f "${2:-}"
    ;;

  status)
    echo "=== Containers ==="
    $COMPOSE ps
    echo ""
    echo "=== Tailscale Status ==="
    if [ "$MODE" = "split" ]; then
      docker exec duckway-tailscale-admin tailscale status 2>/dev/null || echo "  tailscale-admin not running"
      docker exec duckway-tailscale-gateway tailscale status 2>/dev/null || echo "  tailscale-gateway not running"
    else
      docker exec duckway-tailscale-server tailscale status 2>/dev/null || echo "  tailscale-server not running"
    fi
    ;;

  password)
    $COMPOSE logs 2>&1 | grep "Password:" | tail -1
    ;;

  *)
    echo "Duckway Production Manager"
    echo ""
    echo "Usage: $0 {up|down|restart|nuke|logs|status|password}"
    echo ""
    echo "Commands:"
    echo "  up        Build and start with Tailscale"
    echo "  down      Stop (keep data)"
    echo "  restart   Rebuild and restart"
    echo "  nuke      Stop and delete all data (asks confirmation)"
    echo "  logs      Follow logs (optional: service name)"
    echo "  status    Show container + Tailscale status"
    echo "  password  Show admin password"
    echo ""
    echo "Mode: $MODE (set DUCKWAY_PROD_MODE in .prod.env)"
    echo "  split    — admin + gateway on separate containers (default)"
    echo "  combined — everything on one container"
    echo ""
    echo "Tailscale: $USE_TAILSCALE (set DUCKWAY_TAILSCALE=false to disable)"
    exit 1
    ;;
esac
