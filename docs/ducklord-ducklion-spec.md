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

## Terminology

- Host entry: a Ducklord config item under `~/.ducklord/config.yaml` that
  describes one SSH-reachable remote host, its display name, group, SSH command,
  and Ducklion command path.
- Ducklion session: a remote PTY session owned by Ducklion on a host entry.
  These are listed with `ducklion list` and can be attached, read, started, or
  stopped.

Use `host entry` for Ducklord inventory records. Use `Ducklion session` for the
actual remote PTY process.

`ducklord` is the operator UI and SSH orchestrator. It does not store SSH
private keys and does not run remote commands through a shell locally.

`ducklion` is the remote command surface. The canonical entry point is the
standalone `ducklion` binary installed beside `duckway`. `duckway ducklion`
remains as a legacy compatibility wrapper, but it must not own supervisor
processes. Ducklion owns a self-managed PTY backend and returns typed JSON for
list/read/project operations.

## Configuration

MVP `ducklord` uses a local config file:

```yaml
name: dev-laptop
hosts:
  - name: vulns
    host: vulns.tailnet.example
    user: cjiso1117
    group: ctf
    ducklion: ducklion
```

Default path:

```text
~/.ducklord/config.yaml
```

`ducklord tui` does not require the file to exist. If it is missing, Ducklord
starts with an empty menu and lets the operator press `a` to add a host from
`~/.ssh/config` or a full SSH command. The config file is created when the first
host is saved. Non-TUI commands such as `ducklord sessions <client>` still need
a config entry because they must resolve `<client>`.

Override:

```bash
ducklord tui --config ./ducklord.yaml
```

Ducklord discovery is intentionally local and SSH-based:

```bash
ducklord ssh-hosts
ducklord import-ssh-hosts --config ~/.ducklord/config.yaml
```

Ducklord does not use Duckway server for discovery, authorization, or command
relay. The only security boundary is whether the operator can SSH to the
configured host.

## Installing Ducklord And Ducklion

The gateway `/install.sh` is interactive when run from a terminal. It shows a
checkbox menu:

```text
[x] Duckway client + Ducklion
[ ] Ducklord
```

Use `j/k` to move, `Space` to toggle, and `Enter` to continue. On a remote
agent host, keep `Duckway client + Ducklion` selected:

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

On a developer laptop that only needs the SSH TUI, select only `Ducklord`. That
installs the platform-specific `ducklord-*` binary as:

```text
/usr/local/bin/ducklord
```

or, for user-local installs:

```text
~/.local/bin/ducklord
```

Non-interactive `curl ... | sh` installs `Duckway client + Ducklion` by
default. To script other modes:

```bash
DUCKWAY_INSTALL_COMPONENT=ducklord DUCKWAY_INSTALL=user \
  sh -c "$(curl -fsSL http://your-duckway-gateway/install.sh)"
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

Ducklord can also install Ducklion over SSH when the developer laptop already
has a local `ducklion` binary:

```bash
ducklord import-ssh-hosts --config ~/.ducklord/config.yaml
ducklord install-ducklion vulns --source ./ducklion-linux-amd64 --config ~/.ducklord/config.yaml
ducklord probe vulns --config ~/.ducklord/config.yaml
```

By default the remote binary is written to `~/.local/bin/ducklion`. Use
`--dest /usr/local/bin/ducklion` only when the SSH user can write that path.
Ducklord stores the resolved remote path in its local config after the remote
`ducklion version` check succeeds.

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
`/root/.ducklord/config.yaml` in `ducklord-dev`, and creates sample PTY
sessions.

`client-a` and `client-b` are pre-registered in Ducklord config. `client-c`
only exists in `/root/.ssh/config`, so it can be used to test the TUI add-host
flow.

### 2. Inspect Remote Clients And Sessions

```bash
podman exec ducklord-dev ducklord clients --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord ssh-hosts
podman exec ducklord-dev ducklord probe client-a --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord projects client-a --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord sessions client-a --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord read client-a alpha --lines 20 --config /root/.ducklord/config.yaml
podman exec -it ducklord-dev ducklord attach-host client-a --config /root/.ducklord/config.yaml
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
podman exec -it ducklord-dev ducklord tui --config /root/.ducklord/config.yaml
```

Useful keys:

- `j` / `k` or arrow keys move the selection.
- Mouse click selects a row.
- `Enter` or right-click focuses the selected session in the right pane.
- `Ctrl-]` returns keyboard focus to the left menu.
- `a` adds a Ducklion host from `~/.ssh/config`; use `client-c` in the demo.
- `c` creates a new remote session with the wizard. Choose `agent` or `shell`,
  then follow the type-specific flow.
- `n` configures notification categories for the selected session.
- `E`, `R`, and `X` open confirmation views for end, restart, and destroy.
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
podman exec ducklord-dev ducklord read client-a bash --lines 20 --config /root/.ducklord/config.yaml
```

### 5. Add A Host Entry From The TUI

Inside the TUI:

1. Press `a`.
2. Choose `client-c` by number, type `client-c`, or paste a full SSH command
   such as `ssh -p 2222 -i ~/.ssh/id_ed25519 duck@client-c`.
3. Press `Enter`.

Ducklord probes the remote host over SSH. It checks `ducklion version`, runs
`ducklion list --json` to verify the session manager entry point, and records
the host entry in `/root/.ducklord/config.yaml`. If Ducklion is missing,
Ducklord attempts to install the local `ducklion` binary to
`~/.local/bin/ducklion` over SSH, probes again, then records the resolved remote
path. If installation is not possible because the local binary is missing or SSH
cannot write the target path, Ducklord still adds the host and shows a clear
status message so the operator can enable the remote entry point manually.

Press `d` on a selected row to remove that host entry from the current
`config.yaml`. Removing a host entry does not stop remote Ducklion sessions; it
only removes the host from Ducklord's local inventory.

For a host that is reachable over SSH but still missing Ducklion, install the
local binary explicitly:

```bash
podman exec ducklord-dev ducklord install-ducklion client-c \
  --source /usr/local/bin/ducklion \
  --config /root/.ducklord/config.yaml
```

Verify the config was updated:

```bash
podman exec ducklord-dev ducklord clients --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord probe client-c --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord projects client-c --config /root/.ducklord/config.yaml
```

### 6. Create A Remote Session From The TUI

Inside the TUI, press `c`, then follow the wizard:

```text
agent -> host -> configured project -> available agent -> handle
shell -> host -> configured project (or Ducklion default) -> handle
```

For example:

1. Choose `agent` or `shell`.
2. Choose `client-a` by number or name.
3. Choose `alpha-project` by number or name. Shell sessions may also choose
   `Ducklion default` (`~/.duckway/ducklion`). Arbitrary paths are deliberately
   not accepted.
4. For an agent session, choose Codex or Claude only when installed remotely.
5. Enter a Unicode display handle, or press Enter to use the folder name.

Ducklord fetches projects with `ducklion projects --json`, then revalidates the
directory and discovers available commands with
`ducklion agents --cwd <path> --json`. Discovery and final revalidation run in
cancellable workers so SSH latency never freezes navigation. A host generation
change discards stale results. Immediately before creation Ducklord re-reads the
project registry and runtime capabilities; stale choices return to their
relevant step without a partial session. It then starts asynchronously and
selects the exact returned six-character session ID, including for duplicate
handles.

Verify from the terminal:

```bash
podman exec ducklord-dev ducklord sessions client-a --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord read client-a shell-alpha --lines 20 --config /root/.ducklord/config.yaml
```

The same start operation is available from the CLI:

```bash
podman exec ducklord-dev ducklord start client-a \
  --name scratch \
  --kind shell \
  --cwd /home/duck \
  -- bash \
  --config /root/.ducklord/config.yaml
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
ducklord start <client> --name <name> [--kind shell | --agent <agent>] [--cwd <dir>] -- CMD [ARGS...]
ducklord stop <client> <session>
ducklord end <client> <session> [-w|--wait|-f|--force]
ducklord restart <client> <session> [-f|--force]
ducklord destroy <client> <session> [-w|--wait|-f|--force]
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
- `c` to create an agent or shell session with separate type-specific flows
- `n` to configure notification categories for the selected session
- `ducklord attach-host <client>` to open the same split-pane view scoped to
  one remote host and its advertised Ducklion sessions
- `r` to refresh immediately
- `E`, `R`, and `X` open end, restart, and destroy confirmation panels;
  lifecycle requests run asynchronously so SSH recovery never freezes the UI
- `q` to quit
- basic xterm mouse click selection when the terminal supports SGR mouse mode
- a changed marker when recent remote output changes since the previous refresh

The right pane uses a bounded VT framebuffer with primary/alternate screens,
cursor movement, common erase/edit operations, SGR styles, wide and combining
characters, and soft-wrap-aware resize. Remote escape sequences are interpreted
inside that model and never replayed directly into Ducklord's own terminal;
rendering emits only locally generated, allowlisted SGR sequences.

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
agent -> host -> configured project -> available agent -> handle
shell -> host -> configured project or Ducklion default -> handle
```

Projects come from:

```text
ducklord -> ssh -> ducklion projects --json
ducklord -> ssh -> ducklion agents --cwd <project> --json
```

`ducklion projects --json` reads the Duckway client project registry under
`~/.duckway/cc-projects.json` and returns each entry with
`source: "duckway-client"`. It also reports Ducklion's own directory as
`source: "ducklion-default"`; only the shell wizard presents that entry.

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

TUI creation starts when the operator presses `c`.

State transitions:

```text
normal mode
  -> c
  -> type step: agent or shell
  -> host step: configured Ducklord client number or name
  -> project step: a stable entry returned by the remote registry
  -> agent step: agent sessions only; only types reported by that host
  -> handle step: Unicode display handle; empty uses the project folder name
  -> Enter starts the remote PTY session
```

The wizard is local and does not execute through a local shell:

1. The host step resolves a connected Ducklord client by number or name.
2. The project step uses `ducklion projects --json` and rejects arbitrary paths.
   Shell sessions additionally expose `Ducklion default`.
3. Ducklion validates that cwd and resolves its own `PATH` and login shell.
   `ducklion agents --cwd ... --json` always reports `shell` and reports Codex
   or Claude only when its executable is available on the remote host.
4. The agent flow excludes `shell` from the agent step. The separate shell flow
   resolves the remote user's shell without presenting an agent choice.
5. The handle is an independent 1–128-code-point Unicode display name and may
   repeat. Empty input uses the final project directory name. The six-character
   session ID, not the handle, remains the mutation/routing identity.
6. Remote discovery is asynchronous and request-fenced by wizard request ID,
   host generation, and Ducklion instance ID. Escape cancels immediately;
   shutdown joins discovery workers. Final creation revalidates the exact
   `(source, name, path)` project tuple and runtime, then selects by returned ID.

The non-TUI `ducklord start` CLI accepts either `--kind shell` with exactly one
shell executable or `--agent <type>` with an agent command. It uses the same
validation as the wizard before connecting over SSH.

`ducklord` then starts the remote session asynchronously:

```text
goroutine:
  runner.Start(startCtx, client, startArgs)
    -> ssh
    -> ducklion start --name <name> [--kind shell | --agent <agent>] [--cwd <dir>] -- CMD
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

## Durable Agent Lifecycle

Discord task channels support `!end` and `!destroy` in three modes. With no
flag, an active or replying turn is rejected immediately and no barrier is
left behind. `-w`/`--wait` durably rejects new prompts, input, resize, and yield
requests while allowing the current final response and ACK to drain. There is
no wait timeout. `-f`/`--force` emits a failed cancellation event from the PTY
supervisor, fences later adapter output for that task, and terminates only after
the cancellation result is available for Discord delivery.

The request is identified by requester, request ID, session ID, operation,
mode, ownership epoch, and runtime generation. A different payload using the
same requester/request ID is an idempotency conflict. Ducklion reconstructs
workers from `pending_lifecycle_operations` after restart. Completed operations
are immutable entries in `lifecycle_outcomes`, so releasing a barrier never
loses replay safety. `end` archives Discord while retaining the stopped session
and binding; `destroy` hard-deletes both after runtime cleanup succeeds.

Agent `restart` waits for idle by default; `-f` first delivers a cancellation. It
retains the session ID, handle, writer, binding, working directory, and launch
command, then starts runtime generation `N+1`. Mode-0600 `runtime.json` remains
after process exit while the generation-specific recovery key is removed.
Restart creates a new key, atomically changes the session and lifecycle row to
`recovering`/`launching`, then starts the replacement supervisor. Ducklion
reconstructs `launching` work after daemon restart. A per-session
`runtime.lock`, held with a non-blocking OS file lock, makes repeated launch
attempts safe: at most one supervisor can own the PTY.

Shell sessions use the same lifecycle RPC from Ducklord only. Since shell PTYs
have no agent adapter or exclusive writer, restart/end/destroy are always
immediate: they terminate the current shell process directly and reject wait or
force modes. A retained shell launch spec contains one resolved executable and
the CWD, never arbitrary arguments or environment overrides. A definitive
replacement-launch failure returns the session to `stopped`, records an
immutable failed receipt, and releases the barrier so the operator may retry or
destroy it. Discord CC is never authorized to manage shell sessions.

Ducklord does not cancel an accepted durable lifecycle operation when its CLI
wait is interrupted or the TUI exits. The CLI reports that the request remains
durable, while the TUI permits normal navigation as soon as admission succeeds;
the eventual result is shown if that TUI remains open. A lifecycle cancellation
protocol is intentionally outside this version.

Schema v13 adds the immutable lifecycle failure text used by restart recovery.
No manual migration is required; Ducklion applies it transactionally at startup
and preserves the pre-migration database backup behavior.

## Security Boundaries

- SSH controls real connection permission.
- `ducklord` must validate client names, session names, users, and hosts before
  constructing SSH argv.
- `ducklord` must use `exec.Command` argv, not a local shell.
- Full SSH commands are split into argv and stored as the SSH executable plus
  options; the target host remains a separate validated field.
- `ducklord` disables SSH agent forwarding by default with SSH options such as
  `ForwardAgent=no` and `ClearAllForwardings=yes`.
- `ducklion` validates session names and delegates PTY socket/process
  construction to the PTY manager.
- `ducklion` strips `SSH_AUTH_SOCK` from supervised session environments by
  default. Broader session environment allowlisting remains future work.
- Remote session metadata must not contain secrets or prompts. Do not pass secrets
  in launch argv: agent argv and the canonical shell executable/CWD live in a
  mode-0600 retained runtime spec so restart can reproduce the process. The MVP
  stores only the newest 1 MiB of PTY output in generation-specific `0600`
  diagnostic logs. Logs expire after seven days by default, are capped at 32
  generations per session, and are removed with the session on destroy.
  Configure 1–3650 days with `pty_log_retention_days` in
  `~/.duckway/config.yaml`; restart Ducklion explicitly to apply it.
- Ducklord does not trust Duckway server metadata and does not require Duckway
  server registration. SSH host access is the authorization boundary.

## Migration

Old Duckway clients do not have `ducklion`. They remain compatible because:

- Existing `duckway` behavior and state files are unchanged.
- Existing `~/.duckway/agent-sessions.json` records remain owned by
  `duckway session` and are not reused by `ducklion`.
- New `ducklion` PTY state is stored under `~/.ducklion/`.
- Existing `~/.duckway/cc-sessions.json` records are not imported or renamed.
- `ducklord` shows a clear remote error when `ducklion` is missing.

Operators can migrate old hosts one at a time by installing Ducklion over SSH:

```bash
ducklord tui --config ~/.ducklord/config.yaml
ducklord import-ssh-hosts --config ~/.ducklord/config.yaml
ducklord install-ducklion <client> --source ./ducklion-linux-amd64 --config ~/.ducklord/config.yaml
```

## Podman Demo

The repository includes a demo script that creates:

- one `ducklord-dev` container
- three remote client containers, each running sshd and standalone `ducklion`
- a private podman network
- SSH keys/config for the dev container
- sample PTY sessions on pre-registered clients

Before presenting the TUI, the script also runs a non-interactive release
smoke through the real SSH stdio bridge: it restarts a native shell while
preserving its session ID and advancing to generation 2, proves the replacement
PTY accepts input, rejects shell `--force`, reads retained generation-2 output
after end, validates mode-0600 storage, then destroys the session.

Run:

```bash
scripts/ducklord-podman-demo.sh
podman exec ducklord-dev ducklord clients --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord ssh-hosts
podman exec ducklord-dev ducklord probe client-a --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord projects client-a --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord sessions client-a --config /root/.ducklord/config.yaml
podman exec ducklord-dev ducklord read client-a alpha --lines 20 --config /root/.ducklord/config.yaml
podman exec -it ducklord-dev ducklord tui --config /root/.ducklord/config.yaml
```

To run only Ducklord in Podman against real SSH host entries, without starting
the demo remote client containers:

```bash
scripts/ducklord-podman.sh
```

This runs a multi-stage Podman build, so the host only needs Podman. The build
stage compiles Ducklord inside a Go container, creates a local Ducklord runtime
image, and starts:

```bash
ducklord tui --config /home/ducklord/.ducklord/config.yaml
```

The runner mounts the developer's `~/.ssh` and `~/.ducklord` into the container.
Its entrypoint creates a matching container user for the current host UID/GID
and runs Ducklord as that user, so files written through the mounts keep the
developer's ownership instead of becoming root-owned. Useful overrides:

```bash
SSH_DIR=~/.ssh-lab DUCKLORD_DIR=~/.ducklord-lab scripts/ducklord-podman.sh
DUCKLORD_PODMAN_SSH_MOUNT=ro scripts/ducklord-podman.sh
scripts/ducklord-podman.sh version
```

Pass extra Podman `run` options before the Ducklord command and use `--` to
separate them from Ducklord arguments:

```bash
scripts/ducklord-podman.sh \
  --podman-volume "$PWD:/workspace:rw" \
  -- tui --config /home/ducklord/.ducklord/config.yaml
```

Remote Ducklion sessions read the remote host's Duckway client config. For
non-shell agent sessions, Ducklion injects `HTTP_PROXY`, `HTTPS_PROXY`,
`NO_PROXY`, and Duckway CA bundle environment variables when
`~/.duckway/config.yaml` exists and `~/.duckway/proxy.pid` points at a live
Duckway proxy process. Shell sessions are left unchanged. Explicit env values
passed by higher-level callers override these defaults.

Inside the TUI:

- `j` / `k` or arrow keys move selection
- mouse click selects a row
- `Enter` or right-click focuses the selected session in the right pane
- `Ctrl-]` returns keyboard focus to the left menu
- `a` adds a host entry from `~/.ssh/config`; use `client-c`
- `d` removes the selected host entry from the current `config.yaml`
- `c` creates an agent or shell session with the type-specific wizard
- `m` opens the centered session action menu; unavailable actions remain visible
  with their reason, and destructive lifecycle actions require confirmation
- `n` configures notifications for the selected session
- `E`, `R`, and `X` confirm end, restart, and destroy
- `q` exits
