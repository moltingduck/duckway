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
TS_HOSTNAME="${TS_HOSTNAME:-duckway}"
export TS_HOSTNAME

if [ "$USE_TAILSCALE" = "true" ]; then
  case "$MODE" in
    split)
      if { [ -z "${TS_AUTHKEY_ADMIN:-}" ] || [ -z "${TS_AUTHKEY_GATEWAY:-}" ]; } && [ -z "${TS_AUTHKEY:-}" ]; then
        echo "Error: set both TS_AUTHKEY_ADMIN and TS_AUTHKEY_GATEWAY, or set fallback TS_AUTHKEY, in .prod.env"
        exit 1
      fi
      PROFILES="--profile tailscale"
      ;;
    combined)
      if [ -z "${TS_AUTHKEY_SERVER:-}" ] && [ -z "${TS_AUTHKEY:-}" ]; then
        echo "Error: set TS_AUTHKEY_SERVER or fallback TS_AUTHKEY in .prod.env"
        exit 1
      fi
      PROFILES="--profile tailscale-combined"
      ;;
    *) echo "Error: DUCKWAY_PROD_MODE must be 'split' or 'combined'"; exit 1 ;;
  esac
else
  case "$MODE" in
    split)    PROFILES="--profile prod-split" ;;
    combined) PROFILES="--profile prod" ;;
    *) echo "Error: DUCKWAY_PROD_MODE must be 'split' or 'combined'"; exit 1 ;;
  esac
fi

BASE_COMPOSE=(docker compose -f docker-compose.yml)
read -r -a PROFILE_ARGS <<< "$PROFILES"
BASE_COMPOSE+=("${PROFILE_ARGS[@]}")
COMPOSE=("${BASE_COMPOSE[@]}")
DATABASE_BACKEND="${DUCKWAY_DATABASE:-sqlite}"
if [ "$DATABASE_BACKEND" = "postgres" ]; then
  COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.postgres.yml "${PROFILE_ARGS[@]}")
elif [ "$DATABASE_BACKEND" != "sqlite" ]; then
  echo "Error: DUCKWAY_DATABASE must be 'sqlite' or 'postgres'"
  exit 1
fi

# Stamp builds with the current git revision so `duckway version` reports it.
export DUCKWAY_VERSION="$(git -C "$PROJECT_DIR" describe --tags --always --dirty 2>/dev/null || echo docker)"

ui_service() {
  if [ "$MODE" = "split" ]; then
    if [ "$USE_TAILSCALE" = "true" ]; then
      echo "admin-tailscale"
    else
      echo "admin-prod"
    fi
  else
    if [ "$USE_TAILSCALE" = "true" ]; then
      echo "server-tailscale"
    else
      echo "server-prod"
    fi
  fi
}

app_services() {
  if [ "$MODE" = "split" ]; then
    if [ "$USE_TAILSCALE" = "true" ]; then
      echo "admin-tailscale gateway-tailscale"
    else
      echo "admin-prod gateway-prod"
    fi
  else
    if [ "$USE_TAILSCALE" = "true" ]; then
      echo "server-tailscale"
    else
      echo "server-prod"
    fi
  fi
}

case "${1:-up}" in
  up|start)
    echo "Building images..."
    "${COMPOSE[@]}" build
    echo "Starting Duckway production ($MODE mode + Tailscale)..."
    "${COMPOSE[@]}" up -d
    echo ""
    if [ "$USE_TAILSCALE" = "true" ]; then
      if [ "$MODE" = "split" ]; then
        echo "Admin:   http://$TS_HOSTNAME-admin/admin/  (port 80, tailnet only)"
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
    "${COMPOSE[@]}" logs 2>&1 | grep "Password:" | tail -1 || echo "  (check: $0 logs)"
    echo ""
    echo "Tailscale nodes:"
    "${COMPOSE[@]}" ps --format "table {{.Name}}\t{{.Status}}" 2>/dev/null | grep -E "NAME|ts-|tailscale|postgres" || true
    ;;

  down|stop)
    echo "Stopping..."
    "${COMPOSE[@]}" down
    ;;

  restart)
    if [ "${2:-}" = "--minimal" ]; then
      services="$(app_services)"
      echo "Building app services before touching running containers ($MODE mode): $services"
      "${COMPOSE[@]}" build $services
      echo "Recreating app services only (dependencies/sidecars are left running)..."
      "${COMPOSE[@]}" up -d --no-deps $services
    else
      echo "Building new images before touching running containers ($MODE mode)..."
      "${COMPOSE[@]}" build
      echo "Recreating containers with new images..."
      "${COMPOSE[@]}" up -d --remove-orphans
    fi
    ;;

  ui|restart-ui)
    svc="$(ui_service)"
    if [ "$MODE" = "combined" ]; then
      echo "Combined mode embeds UI in the server; recreating only $svc."
    else
      echo "Split mode embeds UI in admin only; gateway will not be restarted."
    fi
    echo "Building $svc before touching the running container..."
    "${COMPOSE[@]}" build "$svc"
    echo "Recreating $svc..."
    "${COMPOSE[@]}" up -d --no-deps "$svc"
    ;;

  nuke)
    echo "Removing everything including data and Tailscale state..."
    read -p "Are you sure? This deletes all data. [y/N] " confirm
    if [ "$confirm" = "y" ] || [ "$confirm" = "Y" ]; then
      "${COMPOSE[@]}" down -v
      echo "Done."
    else
      echo "Cancelled."
    fi
    ;;

  logs)
    if [ -n "${2:-}" ]; then
      "${COMPOSE[@]}" logs -f "$2"
    else
      "${COMPOSE[@]}" logs -f
    fi
    ;;

  status)
    echo "=== Containers ==="
    "${COMPOSE[@]}" ps
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
    "${COMPOSE[@]}" logs 2>&1 | grep "Password:" | tail -1
    ;;

  migrate-postgres)
    if [ "$DATABASE_BACKEND" = "postgres" ]; then
      echo "PostgreSQL is already the configured database."
      exit 0
    fi
    configured_secret="${DUCKWAY_POSTGRES_PASSWORD_FILE:-}"
    secret_file="${configured_secret:-./.secrets/postgres-password}"
    case "$secret_file" in
      /*) ;;
      *) secret_file="$PROJECT_DIR/${secret_file#./}" ;;
    esac
    if [ -z "$configured_secret" ] || [ "$configured_secret" = "./.secrets/postgres-password" ]; then
      mkdir -p "$(dirname "$secret_file")"
      chmod 700 "$(dirname "$secret_file")"
    elif [ ! -d "$(dirname "$secret_file")" ]; then
      echo "Error: PostgreSQL password directory does not exist: $(dirname "$secret_file")" >&2
      exit 1
    fi
    mkdir -p "$PROJECT_DIR/backups"
    chmod 700 "$PROJECT_DIR/backups"
    if [ -L "$secret_file" ] || { [ -e "$secret_file" ] && [ ! -f "$secret_file" ]; }; then
      echo "Error: PostgreSQL password path must be a regular, non-symlink file" >&2
      exit 1
    fi
    if [ ! -f "$secret_file" ]; then
      umask 077
      openssl rand -base64 36 > "$secret_file"
    fi
    chmod 600 "$secret_file"
    if [ ! -s "$secret_file" ] || [ "$(wc -l < "$secret_file")" -ne 1 ]; then
      echo "Error: PostgreSQL password file must contain exactly one non-empty line" >&2
      exit 1
    fi
    export DUCKWAY_POSTGRES_PASSWORD_FILE="$secret_file"
    PG_COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.postgres.yml "${PROFILE_ARGS[@]}" --profile postgres-tools)
    postgres_preexisting=false
    if docker inspect duckway-postgres >/dev/null 2>&1; then
      postgres_preexisting=true
    fi
    services="$(app_services)"
    stamp="$(date -u +%Y%m%dT%H%M%SZ)"
    backup="duckway-sqlite-$stamp.tar.gz"
    echo "Building PostgreSQL and migration images..."
    "${PG_COMPOSE[@]}" build postgres-migrator
    echo "Pulling PostgreSQL before stopping Duckway..."
    "${PG_COMPOSE[@]}" pull postgres
    echo "Stopping every Duckway writer before the SQLite snapshot..."
    for container in duckway-server duckway-admin duckway-gateway; do
      docker stop "$container" >/dev/null 2>&1 || true
    done
    migration_ok=false
    env_backup=""
    cleanup_migration() {
      if [ "$migration_ok" != "true" ]; then
        if [ -n "$env_backup" ] && [ -f "$env_backup" ]; then
          install -m 600 "$env_backup" "$PROJECT_DIR/.prod.env"
        fi
        echo "Migration failed; PostgreSQL was not enabled. Restarting SQLite services." >&2
        if [ "$postgres_preexisting" != "true" ]; then
          docker stop duckway-postgres >/dev/null 2>&1 || true
        fi
        "${BASE_COMPOSE[@]}" up -d $services || true
      fi
    }
    trap cleanup_migration EXIT
    echo "Creating offline SQLite and key backup: backups/$backup"
    "${PG_COMPOSE[@]}" run --rm sqlite-backup -czf "/backup/$backup" -C /data .
    chmod 600 "$PROJECT_DIR/backups/$backup"
    tar -tzf "$PROJECT_DIR/backups/$backup" >/dev/null
    for required_file in ./duckway.db ./encryption.key; do
      if ! tar -tzf "$PROJECT_DIR/backups/$backup" | grep -qx "$required_file"; then
        echo "Backup verification failed: missing $required_file" >&2
        exit 1
      fi
    done
    sha256sum "$PROJECT_DIR/backups/$backup" > "$PROJECT_DIR/backups/$backup.sha256"
    chmod 600 "$PROJECT_DIR/backups/$backup.sha256"
    echo "Starting PostgreSQL..."
    "${PG_COMPOSE[@]}" up -d postgres
    echo "Importing and validating SQLite data..."
    "${PG_COMPOSE[@]}" run --rm postgres-migrator --sqlite-data /data
    env_backup="$PROJECT_DIR/backups/prod-env-before-postgres-$stamp"
    install -m 600 "$PROJECT_DIR/.prod.env" "$env_backup"
    env_tmp="$PROJECT_DIR/.prod.env.tmp.$$"
    awk '
      BEGIN { replaced=0 }
      /^DUCKWAY_DATABASE=/ { print "DUCKWAY_DATABASE=postgres"; replaced=1; next }
      { print }
      END { if (!replaced) print "DUCKWAY_DATABASE=postgres" }
    ' "$PROJECT_DIR/.prod.env" > "$env_tmp"
    chmod 600 "$env_tmp"
    mv "$env_tmp" "$PROJECT_DIR/.prod.env"
    echo "Starting Duckway on PostgreSQL..."
    "$0" restart
    running_services="$("${PG_COMPOSE[@]}" ps --status running --services)"
    for service in $services; do
      if ! printf '%s\n' "$running_services" | grep -qx "$service"; then
        echo "PostgreSQL cutover health check failed: $service is not running" >&2
        exit 1
      fi
    done
    if [ "$MODE" = "split" ]; then
      health_containers="duckway-admin duckway-gateway"
    else
      health_containers="duckway-server"
    fi
    for container in $health_containers; do
      healthy=false
      for _ in $(seq 1 24); do
        if [ "$(docker inspect --format '{{.State.Health.Status}}' "$container" 2>/dev/null || true)" = "healthy" ]; then
          healthy=true
          break
        fi
        sleep 5
      done
      if [ "$healthy" != "true" ]; then
        echo "PostgreSQL cutover health check failed: $container is not healthy" >&2
        exit 1
      fi
    done
    migration_ok=true
    trap - EXIT
    echo "Cutover complete. SQLite backup retained at backups/$backup"
    ;;

  *)
    echo "Duckway Production Manager"
    echo ""
    echo "Usage: $0 {up|down|restart [--minimal]|ui|restart-ui|migrate-postgres|nuke|logs|status|password}"
    echo ""
    echo "Commands:"
    echo "  up        Build and start with Tailscale"
    echo "  down      Stop (keep data)"
    echo "  restart   Build first, then recreate containers with minimal downtime"
    echo "            --minimal recreates app services only; dependencies/sidecars must already be healthy"
    echo "  ui        Rebuild/recreate only the UI-bearing service"
    echo "  nuke      Stop and delete all data (asks confirmation)"
    echo "  logs      Follow logs (optional: service name)"
    echo "  status    Show container + Tailscale status"
    echo "  password  Show admin password"
    echo "  migrate-postgres  Offline backup, import, verify, and switch from SQLite"
    echo ""
    echo "Mode: $MODE (set DUCKWAY_PROD_MODE in .prod.env)"
    echo "  split    — admin + gateway on separate containers (default)"
    echo "  combined — everything on one container"
    echo ""
    echo "Tailscale: $USE_TAILSCALE (set DUCKWAY_TAILSCALE=false to disable)"
    echo "Database: $DATABASE_BACKEND (set DUCKWAY_DATABASE in .prod.env)"
    exit 1
    ;;
esac
