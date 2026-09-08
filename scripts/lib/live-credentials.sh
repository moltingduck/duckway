#!/usr/bin/env bash

# Validate a local live credential without reading or printing its contents.
# Callers remain responsible for mounting it only into the process that needs it.
duckway_live_credential() {
  local label="$1"
  local path="$2"

  if [ ! -e "$path" ]; then
    return 1
  fi
  if [ -L "$path" ] || [ ! -f "$path" ]; then
    echo "Refusing $label credentials that are not a regular non-symlink file: $path" >&2
    return 2
  fi
  local mode
  mode="$(stat -c '%a' "$path")"
  if [ "$mode" != "600" ]; then
    echo "Refusing $label credentials with permissions $mode; run: chmod 600 '$path'" >&2
    return 2
  fi
  return 0
}
