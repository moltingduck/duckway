#!/usr/bin/env bash

# Resolve Duckway's shared live credential directory.
# Usage: duckway_resolve_live_credentials_dir SCRIPT_REPO_ROOT
# DUCKWAY_LIVE_CREDENTIALS_DIR wins over git config. Relative configured paths
# are anchored at the main worktree root; fallback stays in SCRIPT_REPO_ROOT.
duckway_resolve_live_credentials_dir() {
  local script_repo_root="$1"
  local configured="${DUCKWAY_LIVE_CREDENTIALS_DIR:-}"
  local worktree_list primary_worktree main_root common_dir current_git_dir

  if [ -z "$configured" ]; then
    configured="$(git -C "$script_repo_root" config --get duckway.liveCredentialsDir 2>/dev/null || true)"
  fi
  if [ -z "$configured" ]; then
    printf '%s/live-credentials\n' "$script_repo_root"
    return
  fi

  if [[ "$configured" = /* ]]; then
    (cd "$configured" 2>/dev/null && pwd -P) || {
      echo "live credential directory does not exist: $configured" >&2
      return 2
    }
    return
  fi

  worktree_list="$(git -C "$script_repo_root" worktree list --porcelain 2>/dev/null)" || {
    echo "cannot list git worktrees for $script_repo_root" >&2
    return 2
  }
  # `git worktree list` starts with the primary worktree in ordinary repos.
  # Resolve it through Git rather than interpreting a `.git` file ourselves.
  primary_worktree="${worktree_list#worktree }"
  primary_worktree="${primary_worktree%%$'\n'*}"
  [[ "$primary_worktree" != "$worktree_list" && -n "$primary_worktree" ]] || {
    echo "cannot determine primary worktree for $script_repo_root" >&2
    return 2
  }
  main_root="$(git -C "$primary_worktree" rev-parse --show-toplevel 2>/dev/null || true)"
  if [ -z "$main_root" ]; then
    # With --separate-git-dir, Git may report the git directory itself as the
    # primary worktree. If we're currently in that primary checkout, our own
    # top-level is reliable. From a linked checkout, the primary checkout root
    # cannot be established from Git's worktree list, so fail rather than
    # silently anchor a relative credential path to the git directory parent.
    common_dir="$(git -C "$script_repo_root" rev-parse --path-format=absolute --git-common-dir 2>/dev/null)" || {
      echo "cannot locate git common directory for $script_repo_root" >&2
      return 2
    }
    current_git_dir="$(git -C "$script_repo_root" rev-parse --absolute-git-dir 2>/dev/null)" || return 2
    if [ "$current_git_dir" = "$common_dir" ]; then
      main_root="$(git -C "$script_repo_root" rev-parse --show-toplevel 2>/dev/null)" || return 2
    else
      echo "cannot determine primary worktree root for separate-git-dir repository from linked worktree $script_repo_root" >&2
      return 2
    fi
  fi
  main_root="$(cd "$main_root" 2>/dev/null && pwd -P)" || {
    echo "primary worktree root is unavailable for $script_repo_root" >&2
    return 2
  }
  (cd "$main_root/$configured" 2>/dev/null && pwd -P) || {
    echo "live credential directory does not exist: $main_root/$configured" >&2
    return 2
  }
}
