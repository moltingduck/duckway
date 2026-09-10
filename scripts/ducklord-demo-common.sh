#!/usr/bin/env bash

ducklord_demo_lock_file() {
  local runtime_dir="${XDG_RUNTIME_DIR:-${HOME}/.duckway/run}"
  if [ -L "$runtime_dir" ]; then
    echo "refusing symlink demo runtime directory: $runtime_dir" >&2
    return 1
  fi
  mkdir -p "$runtime_dir"
  chmod 700 "$runtime_dir"
  printf '%s/ducklord-demo.lock\n' "$runtime_dir"
}

ducklord_reexec_with_demo_lock() {
	local lock_file
	lock_file="$(ducklord_demo_lock_file)" || return
	if [ -e "$lock_file" ]; then
		[ -f "$lock_file" ] && [ ! -L "$lock_file" ] || {
			echo "refusing unsafe demo lock: $lock_file" >&2
			return 1
		}
	else
		(umask 077; set -o noclobber; : >"$lock_file") || return
	fi
	chmod 600 "$lock_file"
	# flock remains the lock owner while --close prevents Podman/Docker and
	# their long-lived monitor processes from inheriting the descriptor.
	exec flock -n --close "$lock_file" env DUCKLORD_DEMO_LOCK_HELD=1 "$@"
}

ducklord_require_demo_secret() {
  local path="$1" mode
  [ -f "$path" ] && [ ! -L "$path" ] && [ -r "$path" ] || {
    echo "credential must be a readable regular non-symlink file: $path" >&2
    return 1
  }
  mode="$(stat -c '%a' "$path" 2>/dev/null || stat -f '%Lp' "$path" 2>/dev/null || true)"
  [ "$mode" = 600 ] || {
    echo "refusing credential mode $mode for $path; expected 600" >&2
    return 1
  }
}
