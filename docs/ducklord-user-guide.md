# Ducklord quick guide

Start the TUI:

```bash
ducklord tui
```

The session list is on the left and the selected PTY is on the right. A bright
border shows which pane owns keyboard input.

## Everyday controls

| Key | Action |
| --- | --- |
| `j` / `k`, `↑` / `↓` | Select a session |
| `Enter` | Focus the selected PTY |
| `Ctrl+]` | Return from the PTY to the session list |
| `v` | Freeze the screen and enter copy mode |
| `/` | Search sessions |
| `c` | Create a session |
| `m` | Open session actions |
| `n` | Configure notifications |
| `o` / `g` | Organize sessions / manage groups |
| `y` / `Y` | Yield now / wait until idle |
| `E` / `R` / `X` | End / restart / destroy |
| `q` | Quit from the session list |

Central dialogs use the same rules: `↑`/`↓` selects, `Enter` continues, `Esc`
goes back, and `Ctrl+C` closes the dialog. Left and right arrows do nothing, so
they cannot accidentally dismiss a dialog.

## Copy PTY text

1. Return to the session list with `Ctrl+]` if the PTY is focused.
2. Press `v`. Ducklord freezes redraws and releases mouse selection.
3. Drag over the PTY text and use the terminal application's normal Copy action
   (`Ctrl+Shift+C` on many Linux terminals, `Cmd+C` on macOS).
4. Press `Esc`, `q`, `v`, or `Ctrl+C` to resume Ducklord.

Copy mode only changes local viewing. It does not detach the PTY or yield its
writer ownership.

## Create a session

Press `c`, then choose the session type, host, project, and runtime. If the host
has no saved project, type a remote path and choose a suggestion. The first
`Enter` completes the path; the second confirms it. You can save the path as a
Duckway project or use it once. An empty handle uses the light-colored default
shown in the input box.

Shell sessions are writable like tmux. Agent sessions have one writer; a
read-only session must be yielded to this Ducklord before it accepts input.

## Ownership and Discord

The writer shown in the session list is authoritative. A Discord-controlled
agent is read-only in Ducklord until you press `y`; Ducklion transfers it only
when the agent is idle. Press `Y` to wait and transfer immediately after the
current turn finishes. To return control from Discord, send `!yield` in that
session's channel. Shell sessions remain multi-writer and cannot be controlled
from Discord.

## Manage sessions

Press `m` for the complete action menu. `E` stops the current process but keeps
the session, `R` starts a new runtime generation, and `X` permanently removes
the session and retained PTY logs. Destructive actions always use the same
centered confirmation dialog.

Press `n` to enable or disable completion, failure, and terminal-attention
notifications for the selected session. A dot marks unread activity; selecting
that session clears it. A group also shows a dot while any child session is
unread.

Use `o` to assign a session to one custom group and change its order. Use `g` to
create, rename, reorder, or remove groups. These settings live locally under
`~/.ducklord`; they do not rename remote Ducklion sessions.

## Hosts and configuration

Ducklord reads `~/.ducklord/config.yaml` and uses your normal SSH configuration,
keys, and agent. Press `a` to add a host discovered from `~/.ssh/config`.
Configuration changes take effect after restarting Ducklord; it never restarts
itself automatically.

## Agent sign-in

Ducklion uses the agent's normal files (`~/.codex/auth.json` and
`~/.claude/.credentials.json`); Ducklord never displays or stores them. Complete
the agent's per-project trust screen inside the PTY. The demo preconfigures only
Claude's non-sensitive theme/onboarding state; real hosts keep their own CLI
preferences. If an agent returns to its login screen, refresh that credential
on the remote host and restart the session.

Developers can run an isolated integration check by placing mode-`600` copies
in `live-credentials/` and running:

```bash
CONTAINER_RUNTIME=podman scripts/ducklord-agent-live-e2e.sh
```

Use `CONTAINER_RUNTIME=docker` for Docker, or add `--codex-only` /
`--claude-only` while diagnosing one runtime. This developer check sends one
real prompt per selected runtime, can incur provider usage, and may take up to
three minutes per runtime. Never commit files from `live-credentials/`.
