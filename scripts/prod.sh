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

# Validate required vars
if [ -z "$TS_AUTHKEY" ]; then
  echo "Error: TS_AUTHKEY not set in .env"
  echo "  Get one from https://login.tailscale.com/admin/settings/keys"
  exit 1
fi

MODE="${DUCKWAY_PROD_MODE:-split}"
case "$MODE" in
  split)    COMPOSE="docker compose -f docker-compose.prod.yml" ;;
  combined) COMPOSE="docker compose -f docker-compose.combined.yml" ;;
  *) echo "Error: DUCKWAY_PROD_MODE must be 'split' or 'combined'"; exit 1 ;;
esac

case "${1:-up}" in
  up|start)
    echo "Starting Duckway production ($MODE mode)..."
    $COMPOSE up --build -d
    sleep 5

    echo ""
    if [ "$MODE" = "split" ]; then
      echo "Admin:   https://${TS_HOSTNAME:-duckway}-admin (Tailscale HTTPS)"
      echo "Gateway: https://${TS_HOSTNAME:-duckway}-gw (Tailscale HTTPS)"
    else
      echo "Server:  https://${TS_HOSTNAME:-duckway} (Tailscale HTTPS)"
    fi
    echo ""
    echo "Admin password (first run only):"
    $COMPOSE logs 2>&1 | grep "Password:" | tail -1 || echo "  (check logs: $0 logs)"
    echo ""
    echo "Tailscale nodes:"
    $COMPOSE ps --format "table {{.Name}}\t{{.Status}}" | grep -E "NAME|ts-|tailscale"
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
      docker exec duckway-ts-admin tailscale status 2>/dev/null || echo "  ts-admin not running"
      docker exec duckway-ts-gateway tailscale status 2>/dev/null || echo "  ts-gateway not running"
    else
      docker exec duckway-tailscale tailscale status 2>/dev/null || echo "  tailscale not running"
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
    echo "  up        Build and start all services"
    echo "  down      Stop all services (keep data)"
    echo "  restart   Rebuild and restart"
    echo "  nuke      Stop and delete all data + Tailscale state"
    echo "  logs      Follow logs (optional: service name)"
    echo "  status    Show container + Tailscale status"
    echo "  password  Show admin password from logs"
    echo ""
    echo "Mode: $MODE (set DUCKWAY_PROD_MODE in .env)"
    echo "  split    — admin + gateway on separate Tailscale nodes"
    echo "  combined — everything on one Tailscale node"
    exit 1
    ;;
esac
