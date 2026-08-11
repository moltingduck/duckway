# Ducklord / Ducklion Remote Agent Control MVP

## Goal

Duckway will add a developer-facing remote agent control plane that lives in the
Duckway repository but is delivered as independent binaries:

- `ducklord`: runs on a developer laptop and presents the management UI.
- `ducklion`: runs on a remote agent host and owns local session operations.
- `duckway ducklion`: compatibility wrapper that finds and executes the
  standalone `ducklion` binary.

The first MVP assumes the developer can reach remote hosts over ordinary SSH.
It does not require Tailscale SSH, but it works well inside a Tailscale network
because MagicDNS names and private addresses can be used as SSH targets.

## Non-Goals For MVP

- Do not relay terminal bytes through the Duckway server.
- Do not add Duckway server discovery APIs yet.
- Do not add `ducklord` to the `duckway-client-*` update artifact pipeline yet.
  `ducklion` is delivered as the companion PTY binary for client installs.
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
    | local PTY supervisor
    v
  agent process / shell / codex / claude
```

`ducklord` is the operator UI and SSH orchestrator. It does not store SSH
private keys and does not run remote commands through a shell locally.

`ducklion` is the remote command surface. The canonical entry point is the
standalone `ducklion` binary installed beside `duckway`. `duckway ducklion`
remains as a legacy compatibility wrapper, but it must not own supervisor
processes. Ducklion owns a self-managed PTY backend and returns typed JSON for
list/read/project operations.

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

## Installing Ducklion

Normal Duckway client installs fetch both binaries:

```bash
curl -fsSL http://your-duckway-gateway/install.sh | sh
```

The installer downloads the platform-specific `duckway-client-*` binary and the
matching `ducklion-*` binary, then installs them into the same directory:

```text
/usr/local/bin/duckway
/usr/local/bin/ducklion
```

or, for user-local installs:

```text
~/.local/bin/duckway
~/.local/bin/ducklion
```

For manual installs, `ducklord` expects the remote host to expose either:

```bash
ducklion version
```

or the compatibility wrapper:

```bash
duckway ducklion version
```

The standalone `ducklion` binary should be preferred because CC PTY runners and
Ducklord both look for a real `ducklion` executable first. When `duckway` and
`ducklion` are installed side by side, Duckway client PTY runners can still find
the companion binary even if the service `PATH` does not include that directory.

## Operator Tutorial

This walkthrough creates a reproducible local demo with one developer laptop
container and three remote agent containers.

### 1. Build And Start The Demo

```bash
scripts/ducklord-podman-demo.sh
```

The script builds local `ducklord`, `duckway`, and `ducklion` binaries, creates
a private podman network, starts `ducklord-dev`, `ducklion-client-a`,
`ducklion-client-b`, and `ducklion-client-c`, writes
`/root/.ducklord/config.json` in `ducklord-dev`, and creates sample PTY
sessions.

`client-a` and `client-b` are pre-registered in Ducklord config. `client-c`
only exists in `/root/.ssh/config`, so it can be used to test the TUI add-host
flow.

### 2. Inspect Remote Clients And Sessions

```bash
podman exec ducklord-dev ducklord clients --config /root/.ducklord/config.json
podman exec ducklord-dev ducklord ssh-hosts
podman exec ducklord-dev ducklord probe client-a --config /root/.ducklord/config.json
podman exec ducklord-dev ducklord projects client-a --config /root/.ducklord/config.json
podman exec ducklord-dev ducklord sessions client-a --config /root/.ducklord/config.json
podman exec ducklord-dev ducklord read client-a alpha --lines 20 --config /root/.ducklord/config.json
podman exec -it ducklord-dev ducklord attach-host client-a --config /root/.ducklord/config.json
```

Expected shape:

```text
CLIENT       SESSION            STATUS     AGENT        LAST
client-a     alpha              running    shell        ...
client-a     bash               running    shell        client-a:~$
client-a     build              running    shell        ...
```

`ducklord ssh-hosts` should include `client-a`, `client-b`, and `client-c`.
`ducklord probe client-a` should report `ducklion: available`. `ducklord
projects client-a` should show the remote Duckway project registry, including
`alpha-project`.

### 3. Open The TUI

```bash
podman exec -it ducklord-dev ducklord tui --config /root/.ducklord/config.json
```

Useful keys:

- `j` / `k` or arrow keys move the selection.
- Mouse click selects a row.
- `Enter` or right-click focuses the selected session in the right pane.
- `Ctrl-]` returns keyboard focus to the left menu.
- `a` adds a Ducklion host from `~/.ssh/config`; use `client-c` in the demo.
- `n` creates a new remote session with the wizard:
  `agent -> host -> project`.
- `r` refreshes immediately.
- `q` quits.

### 4. Interact With A Bash Session

1. Select `client-a / bash`.
2. Press `Enter`.
3. Type:

```bash
echo hello-from-ducklord
```

4. Press `Ctrl-]`.
5. Press `q`.

Verify the command reached the remote PTY:

```bash
podman exec ducklord-dev ducklord read client-a bash --lines 20 --config /root/.ducklord/config.json
```

### 5. Add A Ducklion Host From The TUI

Inside the TUI:

1. Press `a`.
2. Choose `client-c` by number or type `client-c`.
3. Press `Enter`.

Ducklord probes the remote host over SSH. If `ducklion` or `duckway ducklion`
is available, Ducklord records the working command in
`/root/.ducklord/config.json`. If Ducklion is missing, Ducklord still adds the
host but shows a clear status message so the operator can enable the remote
entry point.

Verify the config was updated:

```bash
podman exec ducklord-dev ducklord clients --config /root/.ducklord/config.json
podman exec ducklord-dev ducklord probe client-c --config /root/.ducklord/config.json
podman exec ducklord-dev ducklord projects client-c --config /root/.ducklord/config.json
```

### 6. Create A Remote Session From The TUI

Inside the TUI, press `n`, then follow the wizard:

```text
agent -> host -> project
```

For example:

1. Enter `shell` or `1`.
2. Choose `client-a` by number or name.
3. Choose `alpha-project` by number or name.

Ducklord fetches projects with `ducklion projects --json`, builds a safe
session name such as `shell-alpha`, starts the session asynchronously, refreshes
the session list, selects the new row, and shows its output preview.

Verify from the terminal:

```bash
podman exec ducklord-dev ducklord sessions client-a --config /root/.ducklord/config.json
podman exec ducklord-dev ducklord read client-a shell-alpha --lines 20 --config /root/.ducklord/config.json
```

The same start operation is available from the CLI:

```bash
podman exec ducklord-dev ducklord start client-a \
  --name scratch \
  --agent shell \
  --cwd /home/duck \
  -- bash \
  --config /root/.ducklord/config.json
```

### 7. Clean Up

```bash
podman rm -f ducklord-dev ducklion-client-a ducklion-client-b ducklion-client-c
podman network rm ducklord-demo
```

## CLI Contract

Remote host:

```bash
ducklion list --json [--tail-lines N]
ducklion projects --json
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
ducklord ssh-hosts
ducklord probe <client> [--config <path>]
ducklord sessions <client> [--config <path>]
ducklord projects <client> [--config <path>]
ducklord tui [--config <path>] [--refresh 2s]
ducklord attach-host <client> [--config <path>]
ducklord attach <client> <session> [--config <path>]
ducklord read <client> <session> [--lines N] [--config <path>]
ducklord send <client> <session> <text> [--config <path>]
ducklord start <client> --name <name> [--agent <agent>] [--cwd <dir>] -- CMD [ARGS...]
ducklord stop <client> <session>
ducklord version
```

## TUI MVP

The current TUI supports:

- left-side grouped session list
- right-side selected session output preview
- keyboard navigation with arrow keys or `j` / `k`
- `Enter` or right-click to focus the selected session in the right pane
- keyboard input routing to the focused remote PTY session
- `Ctrl-]` to return focus to the left menu
- `n` to create a new remote session with `agent -> host -> project`
- `ducklord attach-host <client>` to open the same split-pane view scoped to
  one remote host and its advertised Ducklion sessions
- `r` to refresh immediately
- `q` to quit
- basic xterm mouse click selection when the terminal supports SGR mouse mode
- a changed marker when recent remote output changes since the previous refresh

The right pane is not a full VT terminal emulator. It renders a bounded,
sanitized output view and handles common interactive echoes such as backspace.
Remote terminal-global escape sequences are not allowed to control the local
TUI. Future work should replace the simplified renderer with a bounded VT
screen model.

## Target TUI Flow

The next TUI flow is two-stage:

1. Add or enable a Ducklion host.
2. Start a shell or agent session on one enabled host and one project.

Stage 1 uses local SSH metadata:

```text
ducklord -> ~/.ssh/config -> concrete Host choices
ducklord -> ssh <host> 'sh -lc <ducklion probe script>'
```

The probe distinguishes:

- `ducklion`: use the standalone binary
- `duckway ducklion`: legacy wrapper found on older hosts
- missing: ask the operator whether to install or enable Ducklion

Stage 2 asks for:

```text
shell/agent -> host -> project
```

Projects come from:

```text
ducklord -> ssh -> ducklion projects --json
```

`ducklion projects --json` reads the Duckway client project registry under
`~/.duckway/cc-projects.json` and returns each entry with
`source: "duckway-client"`. The TUI should still offer a custom path for hosts
without a saved project registry.

## Technical Details

### Listing And Preview

`ducklord tui` polls each configured client:

```text
ducklord -> ssh -> ducklion list --json --tail-lines N
```

`ducklord attach-host <client>` uses the same polling and preview path, but it
first narrows the loaded config to the selected client. The host-scoped TUI
keeps the left menu visible, opens focused sessions with `Enter` or right-click,
and disables add-host / new-session shortcuts so it acts as an attach surface
for the remote daemon's advertised sessions.

`ducklion list` returns records with status, agent type, last non-empty line,
and a hash of recent output. `ducklord` overwrites the remote `client` and
`group` fields with local config metadata, sanitizes remote display text, sorts
by group/client/session, and renders the left menu.

When a row is selected, `ducklord` reads a longer preview:

```text
ducklord -> ssh -> ducklion read <session> --lines 80
```

The passive `ducklord sessions` command also sanitizes remote last-line output
before printing it to the local terminal.

### Focusing A Session And Sending Input

When the operator presses `Enter` or right-clicks a session, `ducklord` starts
a streaming attach over SSH:

```text
ducklord tui
  -> exec.CommandContext("ssh", SSHArgs(..., "ducklion attach <session>"))
  -> stdin/stdout pipes
  -> remote ducklion attach
  -> local ducklion Unix socket
  -> session PTY
```

Input flow:

1. `readInput()` reads raw bytes from local `os.Stdin`.
2. `nextInputEvent()` splits coalesced input into key and mouse events.
3. In focused mode, `Ctrl-]` is intercepted by `ducklord` to return to the menu.
4. All other bytes are written directly to the attach process stdin.
5. SSH delivers those bytes to `ducklion attach`.
6. `ducklion attach` sends an attach request to the session control socket.
7. The supervisor handles `attach` with `io.Copy(sessionPTY, conn)`.

Output flow:

```text
remote process PTY
  -> supervisor capture goroutine
  -> per-session 0600 log
  -> attached Unix socket listeners
  -> ducklion attach stdout
  -> ssh stdout
  -> ducklord right pane renderer
```

The attach stream is generation-tagged inside `ducklord`, so stale output or
completion from an older attach cannot cancel or pollute a newer focused
session. `AttachSession.Done` is observed so SSH or remote attach errors are
shown instead of being mistaken for a clean EOF.

### Creating A Remote Session

TUI creation starts when the operator presses `n`.

State transitions:

```text
normal mode
  -> n
  -> agent step: shell / codex / claude
  -> host step: configured Ducklord client number or name
  -> project step: remote Duckway project number/name/path, or custom cwd
  -> Enter starts the remote PTY session
```

The wizard is local and does not execute through a local shell:

1. `parseCreateAgentChoice()` maps `shell`, `codex`, and `claude` to an agent
   type and default command.
2. The host step resolves a configured Ducklord client by number or name.
3. The project step uses `ducklion projects --json`; if no registry is present,
   the operator can enter a custom cwd path.
4. `createSessionName()` derives a safe session name from agent and project
   path, for example `shell-alpha`.
5. `buildStartArgs()` validates session and agent names as safe identifiers.

The older non-TUI `ducklord start` CLI still accepts explicit
`--name`/`--agent`/`--cwd`/`-- CMD` arguments and uses the same start-argument
builder before connecting over SSH.

`ducklord` then starts the remote session asynchronously:

```text
goroutine:
  runner.Start(startCtx, client, startArgs)
    -> ssh
    -> ducklion start --name <name> [--agent <agent>] [--cwd <dir>] -- CMD
```

The TUI remains responsive while SSH start is in progress. `Esc`, `Ctrl-C`, or
`q` cancels the start context. On success, the main loop receives a
`startDoneEvent`, refreshes sessions, selects the new row, and reads its output.
On failure, the prompt remains open and shows the error.

The CLI path uses the same start-argument builder before connecting over SSH, so
invalid names, invalid agents, unknown options, and missing commands are
rejected locally.

### Ducklion PTY Supervisor

`ducklion start` creates state under `~/.ducklion/`:

```text
~/.ducklion/
  sessions.json
  sessions/<name>/
    control.sock
    output.log
    supervisor.err
```

The supervisor process is launched as:

```text
ducklion __supervise --name <name> --agent <agent> --cwd <dir> \
  --socket <socket> --log <log> -- CMD [ARGS...]
```

Inside `RunSupervisor`, `pty.StartWithSize(cmd, 40x120)` creates the child PTY
after the supervisor opens its `0600` Unix socket. The supervisor listens for:

- `send`: writes one command line to the PTY
- `attach`: streams bidirectional bytes between the socket and PTY
- `stop`: terminates the process group and closes the PTY

The capture path writes logs first, snapshots listener sockets while holding the
mutex, then writes to listeners outside the lock with deadlines. A slow attach
consumer therefore cannot block log capture or the whole PTY supervisor.

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
- `ducklord` disables SSH agent forwarding by default with SSH options such as
  `ForwardAgent=no` and `ClearAllForwardings=yes`.
- `ducklion` validates session names and delegates PTY socket/process
  construction to the PTY manager.
- `ducklion` strips `SSH_AUTH_SOCK` from supervised session environments by
  default. Broader session environment allowlisting remains future work.
- Remote session state must not contain secrets or prompts. The MVP stores PTY
  output in per-session `0600` logs for `read`/notification polling; future
  releases should add size caps, rotation, and retention controls.
- Duckway server visibility, when added, is a discovery/policy boundary, not the
  SSH login boundary unless a future relay mode is introduced.

## Migration

Old Duckway clients do not have `ducklion`. They remain compatible because:

- Existing `duckway` behavior and state files are unchanged.
- Existing `~/.duckway/agent-sessions.json` records remain owned by
  `duckway session` and are not reused by `ducklion`.
- New `ducklion` PTY state is stored under `~/.ducklion/`.
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
- three remote client containers, each running sshd and standalone `ducklion`
- a private podman network
- SSH keys/config for the dev container
- sample PTY sessions on pre-registered clients

Run:

```bash
scripts/ducklord-podman-demo.sh
podman exec ducklord-dev ducklord clients --config /root/.ducklord/config.json
podman exec ducklord-dev ducklord ssh-hosts
podman exec ducklord-dev ducklord probe client-a --config /root/.ducklord/config.json
podman exec ducklord-dev ducklord projects client-a --config /root/.ducklord/config.json
podman exec ducklord-dev ducklord sessions client-a --config /root/.ducklord/config.json
podman exec ducklord-dev ducklord read client-a alpha --lines 20 --config /root/.ducklord/config.json
podman exec -it ducklord-dev ducklord tui --config /root/.ducklord/config.json
```

Inside the TUI:

- `j` / `k` or arrow keys move selection
- mouse click selects a row
- `Enter` or right-click focuses the selected session in the right pane
- `Ctrl-]` returns keyboard focus to the left menu
- `a` adds a Ducklion host from `~/.ssh/config`; use `client-c`
- `n` creates a new remote session with `agent -> host -> project`
- `q` exits
