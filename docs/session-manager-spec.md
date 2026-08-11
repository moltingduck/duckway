# Duckway Session Manager Spec

Status: draft for TDD implementation.

## Goals

Duckway will add a local PTY session manager for operator-driven terminal agents while keeping Discord Control Channel (CC) agent sessions independent by default.

The first implementation is client-local only:

- No new server API.
- No web UI dependency.
- No shared live PTY between terminal and CC.
- Terminal sessions and CC sessions may use the same workspace, but they have separate PTY processes and queues.

## Non-Goals

- Do not let terminal and CC write to the same live agent PTY in this phase.
- Do not move CC runner behavior onto the new manager yet.
- Do not expose remote terminal control through Duckway server.
- Do not store secrets, prompts, or full terminal scrollback in the session state file.

## User Model

Terminal sessions are local operator sessions:

```bash
duckway session start --name review --agent codex --cwd /repo -- codex exec
duckway session list
duckway session send review "review the current diff"
duckway session read review --lines 120
duckway session attach review
duckway session stop review
```

CC sessions keep the existing model:

```text
Discord task channel -> independent CC runner -> ducklion PTY session
```

Both session types can inspect the same filesystem and coordinate through files, git status, logs, and docs.

## State

The local session manager persists metadata at:

```text
~/.duckway/agent-sessions.json
```

Shape:

```json
{
  "version": 1,
  "sessions": [
    {
      "id": "sess_...",
      "name": "review",
      "kind": "terminal",
      "agent_type": "codex",
      "cwd": "/repo",
      "backend": "pty",
      "pty_session": "duckway-term-review",
      "tmux_session": "",
      "created_at": "2026-07-30T00:00:00Z",
      "updated_at": "2026-07-30T00:00:00Z",
      "status": "running"
    }
  ]
}
```

Rules:

- State file is versioned from day one.
- Writes are atomic.
- State does not include prompts, stdout, API keys, environment dumps, or Discord message content.
- `status` is best-effort metadata; `duckway session list` should reconcile it with backend process/socket existence.

## Session Naming

Terminal sessions use:

```text
duckway-term-<safe-name>
```

Legacy tmux CC sessions use:

```text
<channel_handle>-duckway
```

This makes terminal and CC sessions intentionally non-colliding.

Session names must be shell/backend safe:

- allowed: ASCII letters, digits, `_`, `-`
- disallowed: empty names, `.`, `/`, `\`, `:`, whitespace, NUL, control characters, and overlong names

## Migration

Old clients do not have `agent-sessions.json`; new clients must treat a missing file as an empty version-1 state.

Existing files are not rewritten unless a session command mutates state.

Existing CC state remains untouched:

- `~/.duckway/cc-sessions.json`
- `~/.duckway/cc-watch/<handle>/...`
- legacy CC tmux migration from `duckway-<handle>` to `<handle>-duckway`

The session manager must not import or rename CC sessions during phase 1.

Forward compatibility:

- unknown future state versions must return a clear error instead of silently rewriting.
- unknown fields in version 1 are tolerated by JSON decoding.

## CLI Contract

```bash
duckway session list
duckway session start --name <name> [--agent <codex|claude_code|openclaw|shell>] [--cwd <dir>] [--tmux] -- <command> ...
duckway session attach <name>
duckway session send <name> <text>
duckway session read <name> [--lines N]
duckway session stop <name>
```

Behavior:

- `start` uses Duckway's PTY supervisor by default.
- `start --tmux` uses the legacy tmux backend.
- `start` fails if the name already exists and the backend session is still alive.
- `start` may replace a stale record whose backend session no longer exists.
- `start --cwd` resolves symlinks and stores the evaluated absolute directory.
- `send` appends Enter after the provided text.
- `read` captures recent pane output without mutating the session.
- `attach` execs the backend attach command, usually `ducklion attach <pty_session>`.
- `stop` kills the backend session if present and marks state as stopped.

## TDD Acceptance Tests

Unit tests first:

- missing state loads as version-1 empty state.
- unknown future state version is rejected.
- start writes one terminal session record with `backend=pty` and `duckway-term-` PTY name.
- terminal backend names do not collide with legacy CC `<handle>-duckway` tmux names.
- duplicate live start is rejected.
- stale record can be replaced.
- send/read/stop target the backend session from state.
- invalid session names and cwd paths are rejected.

Integration tests later:

- PTY smoke test for start/send/read/stop with standalone `ducklion`.
- legacy tmux smoke test for `--tmux` when tmux is installed.
- CLI smoke for `duckway session list` with missing state.
- migration smoke with existing `cc-sessions.json` and CC tmux sessions proving they are left untouched.

## Security Notes

- This is local-only in phase 1. Remote server-triggered terminal control is out of scope.
- Do not expose `duckway session` through MCP tools, CC commands, or server/control-plane APIs in phase 1.
- Commands are passed as argv after `--`; do not parse them through a shell unless the user explicitly starts `shell`.
- Generic terminal sessions do not automatically inject Duckway key/proxy environment. They inherit the local operator environment only.
- State files use `0600`; directories use `0700`.
- Logs should include session names and backend session names, not command contents or environment.
