# Duckway shell completion

Generate a completion script using the installed `duckway` executable.

Bash (add this line to `~/.bashrc`):

```bash
source <(duckway completion bash)
```

Zsh (after `autoload -Uz compinit; compinit` in `~/.zshrc`):

```zsh
source <(duckway completion zsh)
```

Fish:

```fish
mkdir -p ~/.config/fish/completions
duckway completion fish > ~/.config/fish/completions/duckway.fish
```

Completion suggests public Duckway commands, their flags, fixed option values,
and assigned `OWNER/REPO` names after `duckway git clone`. Free-form arguments
such as session names, paths, server URLs, and commands after `--` have no
Duckway suggestions. The `ducklion` compatibility wrapper delegates to another
executable and does not provide nested completion.

Repository hints come from `~/.duckway/git-completion.json`, a secret-free local
snapshot refreshed by normal `duckway sync`. Run `duckway sync` once after
upgrading to populate it. The snapshot contains only repository names whose
GitHub assignment permits cloning; removed assignments disappear after the next
successful sync. Missing or invalid snapshots produce no repository suggestions.
A stale hint does not grant access: `git clone` still checks the current server
assignment.

Pressing Tab never contacts the server, starts a daemon, synchronizes keys, or
changes configuration. Suggestions are passed as literal shell arguments, never
executed as shell code. Internal runtime commands and update verification flags
are excluded.
