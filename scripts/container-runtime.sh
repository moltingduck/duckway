#!/usr/bin/env bash

# Shared Docker/Podman selection for operational scripts. Source this file,
# then call duckway_init_container_runtime or duckway_init_compose_runtime.

duckway_init_container_runtime() {
  if [ -n "${CONTAINER_RUNTIME:-}" ]; then
    if ! command -v "$CONTAINER_RUNTIME" >/dev/null 2>&1; then
      echo "Error: configured container runtime is not installed: $CONTAINER_RUNTIME" >&2
      return 1
    fi
    export CONTAINER_RUNTIME
    return
  fi

  if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    CONTAINER_RUNTIME=docker
  elif command -v podman >/dev/null 2>&1 && podman info >/dev/null 2>&1; then
    CONTAINER_RUNTIME=podman
  else
    echo "Error: no usable Docker or Podman runtime found." >&2
    echo "Set CONTAINER_RUNTIME=docker or CONTAINER_RUNTIME=podman to select one explicitly." >&2
    return 1
  fi
  export CONTAINER_RUNTIME
}

duckway_init_compose_runtime() {
  duckway_init_container_runtime || return
  if "$CONTAINER_RUNTIME" compose version >/dev/null 2>&1; then
    DUCKWAY_COMPOSE=("$CONTAINER_RUNTIME" compose)
    return
  fi
  if [ "$CONTAINER_RUNTIME" = "podman" ] && command -v podman-compose >/dev/null 2>&1; then
    DUCKWAY_COMPOSE=(podman-compose)
    return
  fi
  echo "Error: $CONTAINER_RUNTIME is available, but no Compose provider was found." >&2
  if [ "$CONTAINER_RUNTIME" = "podman" ]; then
    echo "Install podman-compose (or a Docker Compose provider), then retry." >&2
  else
    echo "Install the Docker Compose plugin, then retry." >&2
  fi
  return 1
}
