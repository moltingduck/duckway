#!/bin/bash
# Reset the Duckway admin password to a fresh random password.
# Always generates a strong random password — never accepts custom passwords
# (which would leak via shell history, ps output, or log files).
#
# Usage:
#   ./scripts/reset-password.sh             # reset 'duckway' user
#   ./scripts/reset-password.sh -u alice    # reset 'alice' user
#
# The new password is printed ONCE to stdout. Save it immediately.
set -e

USERNAME="duckway"

while [ $# -gt 0 ]; do
  case "$1" in
    -u|--username)
      USERNAME="$2"; shift 2 ;;
    -h|--help)
      sed -n '2,11p' "$0" | sed 's/^# \?//'
      exit 0 ;;
    *)
      echo "Unknown option: $1" >&2
      echo "Usage: $0 [-u username]" >&2
      exit 1 ;;
  esac
done

# Detect which container holds the database
CONTAINER=""
BINARY=""
if docker ps --format '{{.Names}}' | grep -qx "duckway-server"; then
  CONTAINER="duckway-server"
  BINARY="duckway-server"
elif docker ps --format '{{.Names}}' | grep -qx "duckway-admin"; then
  CONTAINER="duckway-admin"
  BINARY="duckway-admin"
else
  echo "Error: neither duckway-server nor duckway-admin is running." >&2
  echo "Start prod first: ./scripts/prod.sh up" >&2
  exit 1
fi

echo "Resetting password for '$USERNAME' in container '$CONTAINER'..."
docker exec "$CONTAINER" "$BINARY" --reset-password --reset-username "$USERNAME" --data /data
