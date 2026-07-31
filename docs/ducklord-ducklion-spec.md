# Ducklord / Ducklion Remote Agent Control MVP

## Goal

Duckway will add a developer-facing remote agent control plane that lives in the
Duckway repository but is delivered as independent binaries:

- `ducklord`: runs on a developer laptop and presents the management UI.
- `ducklion`: runs on a remote agent host and owns local session operations.

The first MVP assumes the developer can reach remote hosts over ordinary SSH.
It does not require Tailscale SSH, but it works well inside a Tailscale network
because MagicDNS names and private addresses can be used as SSH targets.

## Non-Goals For MVP

- Do not relay terminal bytes through the Duckway server.
- Do not add Duckway server discovery APIs yet.
- Do not add `ducklord` or `ducklion` to the `duckway-client-*` update artifact
  pipeline yet.
- Do not merge Discord CC sessions with Ducklord sessions.
- Do not require the remote host to run the normal `duckway` proxy/client daemon.

## Architecture

```text
developer laptop
  ducklord tui
    |
    | ordinary SSH, using the user's SSH config/keys/agent
    v
remote agent host
  ducklion
    |
    | local tmux/session manager
    v
  agent process / shell / codex / claude
```

`ducklord` is the operator UI and SSH orchestrator. It does not store SSH
private keys and does not run remote commands through a shell locally.

`ducklion` is the remote command surface. It wraps the existing local
tmux-backed session manager and returns typed JSON for list/read operations.

## Configuration

MVP `ducklord` uses a local config file:

```json
{
  "clients": [
    {
      "name": "vulns",
      "host": "vulns.tailnet.example",
      "user": "cjiso1117",
      "group": "ctf",
      "ducklion": "ducklion"
    }
  ]
}
```

Default path:

```text
~/.ducklord/config.json
```

Override:

```bash
ducklord tui --config ./ducklord.json
```

Future server-backed discovery can replace this file with:

```text
ducklord -> Duckway server -> registered client metadata
```

The SSH data path should remain direct unless a future server-relay mode is
explicitly enabled.

## CLI Contract

Remote host:

```bash
ducklion list --json [--tail-lines N]
ducklion start --name <name> [--agent <agent>] [--cwd <dir>] -- CMD [ARGS...]
ducklion read <name> [--lines N] [--json]
ducklion send <name> <text>
ducklion attach <name>
ducklion stop <name>
ducklion version
```

Developer laptop:

```bash
ducklord clients [--config <path>]
ducklord sessions <client> [--config <path>]
ducklord tui [--config <path>] [--refresh 2s]
ducklord attach <client> <session> [--config <path>]
ducklord read <client> <session> [--lines N] [--config <path>]
ducklord send <client> <session> <text> [--config <path>]
ducklord start <client> --name <name> [--agent <agent>] [--cwd <dir>] -- CMD [ARGS...]
ducklord stop <client> <session>
ducklord version
```

## TUI MVP

The first TUI supports:

- left-side grouped session list
- keyboard navigation with arrow keys or `j` / `k`
- `Enter` to attach the selected session through SSH
- `r` to refresh immediately
- `q` to quit
- basic xterm mouse click selection when the terminal supports SGR mouse mode
- a changed marker when recent remote output changes since the previous refresh

The MVP TUI does not embed a full terminal pane. Attach uses SSH to hand the
terminal to the remote `ducklion attach <session>` command. Detaching from tmux
returns to the TUI.

## Notifications

MVP notification is intentionally simple:

- `ducklion list --json --tail-lines N` returns a tail hash and last non-empty
  line for each session.
- `ducklord tui` polls remote clients on a refresh interval.
- If a running session's tail hash changes, the TUI marks it as changed and can
  ring the terminal bell.
- If a session changes from `running` to `stale` or `stopped`, the TUI marks it
  as changed.

Future notification upgrades:

- remote `ducklion watch --json` event stream over SSH
- server-side out-of-band events for "done", "blocked", and "needs input"
- desktop notifications from the local `ducklord` process

## Security Boundaries

- SSH controls real connection permission.
- `ducklord` must validate client names, session names, users, and hosts before
  constructing SSH argv.
- `ducklord` must use `exec.Command` argv, not a local shell.
- `ducklion` validates session names and delegates tmux target construction to
  the session manager.
- Remote session state must not contain secrets, prompts, or full scrollback.
- Duckway server visibility, when added, is a discovery/policy boundary, not the
  SSH login boundary unless a future relay mode is introduced.

## Migration

Old Duckway clients do not have `ducklion`. They remain compatible because:

- Existing `duckway` behavior and state files are unchanged.
- Existing `~/.duckway/agent-sessions.json` records are reused by `ducklion`.
- Existing `~/.duckway/cc-sessions.json` records are not imported or renamed.
- `ducklord` shows a clear remote error when `ducklion` is missing.

Future migration from file config to server discovery should support both:

```bash
ducklord tui --config ~/.ducklord/config.json
ducklord tui --server https://duckway.example
```

## Podman Demo

The repository includes a demo script that creates:

- one `ducklord-dev` container
- two remote client containers, each running sshd and `ducklion`
- a private podman network
- SSH keys/config for the dev container
- sample tmux sessions on both clients

Run:

```bash
scripts/ducklord-podman-demo.sh
podman exec -it ducklord-dev ducklord tui --config /root/.ducklord/config.json
```

Inside the TUI:

- `j` / `k` or arrow keys move selection
- mouse click selects a row
- `Enter` attaches to the remote tmux session
- `Ctrl-b d` detaches from tmux and returns to Ducklord
- `q` exits
