# Ducklion PTY transport implementation

This document describes the implemented transport underneath the converged
[CC/terminal handoff specification](./cc-terminal-pty-handoff-spec.md). The
specification remains authoritative for product behavior.

## Process and trust boundaries

```text
Ducklord TUI
    │ one long-lived SSH stdio bridge per configured host
    ▼
ducklion bridge --stdio
    │ opaque bytes; no parsing or logging
    ▼
~/.duckway/ducklion/ducklion.sock (0600, same-EUID peer check)
    ▼
Ducklion daemon ───── SQLite state
    │ recovery/output socket       │ control socket
    ▼                              ▼
independent PTY supervisor ───── managed PTY
```

`ducklion bridge --stdio` is deliberately a transport shim. It writes no
banner, diagnostic, prompt, or log record to stdout. Stdout is reserved for
protocol bytes; process errors go to stderr. The bridge does not accept an
owner argument and does not inspect PTY content. Ducklord supplies its
user-managed owner name in the protocol handshake, as required by the spec.
The bridge function owns and closes its stdin. Its production lifetime is the
remote command process lifetime; callers must cancel the context and close the
transport to terminate blocked copies. It is not a reusable in-process stream
pool.

Ducklion validates the local bridge process with Unix peer credentials. The
socket, database, and lock live below `~/.duckway/ducklion`, whose permissions
are restricted to the Duckway user. No TCP listener is opened.

The remote Unix account is the authorization boundary. The Ducklord owner name
is a user-managed, case-sensitive collision label rather than a separate
credential: it uses 1–64 ASCII letters, digits, `.`, `_`, or `-`. Ducklion
allows only one live bridge for a given name and releases that registration
when the connection closes. A user who can authenticate as the same remote
Unix account can assert any valid owner name; deployments needing stronger
device isolation must use distinct operating-system accounts or SSH policy.

## Framing and negotiation

Frames use a four-byte big-endian length followed by JSON. A frame is capped at
1 MiB. Both peers negotiate a major/minor protocol version and explicit
capabilities. Connection setup requires only the `status` capability; APIs
check their own capability before use, allowing a clear unsupported-operation
error instead of failing an otherwise compatible connection.

Generic stdio/SSH streams do not provide socket deadlines. Client negotiation
therefore also accepts a context; expiry closes the stream so a remote bridge
that never answers cannot hang Ducklord startup indefinitely.

After negotiation the connection is fully multiplexed. Requests carry unique
IDs and may be in flight while zero or more PTY output subscriptions emit
events. Ducklord has one reader loop that demultiplexes responses by request ID
and output by subscription ID; writes are serialized independently. This keeps
session inventory, input, resize, and multiple raw-output streams on the same
SSH process. A bounded pre-registration event buffer covers the narrow race
between the subscribe response and local subscription installation without
allowing arbitrary event IDs to grow memory indefinitely.

Closing an output view sends `session.output_unsubscribe`; it does not close
the host bridge. Ducklion cancels only that OutputHub subscriber, sends a
terminal event when appropriate, and keeps processing other requests and
subscriptions. Slow subscribers remain isolated by bounded queues.

RPC calls have bounded client contexts. A timeout interrupts both waiting for
the connection writer and a blocked frame write by closing the affected host
bridge. For mutations this deliberately reports an unknown outcome and never
retries input implicitly. Ducklion preserves request arrival order in a
bounded connection worker while its reader continues admitting independent
subscribe and unsubscribe operations. Server writes have a five-second socket
deadline, so a peer that stops reading loses its bridge rather than freezing
all streams indefinitely.

## Supervisor channels

The supervisor uses two independently authenticated Unix connections:

- recovery/output carries registration, liveness, and ordered output chunks;
- control carries daemon-to-supervisor input and resize requests.

Both connections prove possession of the per-runtime Ed25519 recovery key.
Proofs bind the Ducklion instance ID, session ID, runtime generation, nonce,
and negotiated protocol version. A successful registration receives a random
runtime lease. Every later operation is checked against the current lease and
generation.

The split prevents asynchronous control requests from being confused with
output acknowledgements and guarantees one codec reader and writer at a time.
Control reconnect does not reset the runtime-scoped input sequence.

## Session creation and daemon restart

Ducklord creates sessions with the negotiated `session_create` RPC. The request
contains a Unicode display handle, session kind, agent type, absolute working
directory, argv array, and initial PTY dimensions. Commands are never joined or
reparsed through a shell by Ducklion. The authenticated Ducklord principal
becomes the initial writer of an agent session; shell sessions remain
shared-writer as specified.

Ducklion allocates the six-character Crockford session ID and an Ed25519
recovery key. Public state is committed to SQLite. The private key and immutable
runtime launch description are stored below
`~/.duckway/ducklion/sessions/<session-id>/` with mode `0600`; the directory is
private to the Duckway Unix account. Ducklion then starts the same installed
binary using its private `__ducklion_runtime_v1` entry point. That process is a
new session leader and owns both the PTY child and its in-memory output ring.
The create RPC normally waits for matching recovery/output and control leases,
so the common successful response is immediately attachable. If startup takes
longer than the bounded readiness window, it returns the stable session ID with
`recovering` status rather than claiming failure or spawning a duplicate;
inventory then exposes the eventual transition to `running` or `stopped`.

Managed PTYs do not inherit arbitrary daemon-only credentials. Ducklion passes
only the basic login/terminal environment (`HOME`, `PATH`, terminal and locale,
user/shell, and temporary-directory fields). An interactive shell remains
responsible for its own startup files; a later launch-config feature may add
explicit per-session variables.

The supervisor is not a child-lifetime resource of the daemon. If Ducklion is
restarted, the Unix connections close, the supervisor keeps the PTY alive, and
it retries authenticated registration against the recreated socket. SQLite
temporarily reports `recovering`, then returns to `running` after the new daemon
accepts the same recovery key and runtime generation. This path is exercised by
an automated test that writes to the same PTY after a daemon close/open cycle.
When the PTY exits, the supervisor first drains final output and sends an
authenticated `supervisor.exited` record. Ducklion durably marks the session
`stopped` instead of mistaking an intentional exit for a reconnectable network
loss. `session.stop` is owner- and generation-fenced, terminates the supervised
process group, and waits for that stopped record before replying.

Ducklion schema v2 adds the bounded exit success/reason fields exposed in
session summaries. Opening an existing v1 database creates a timestamped
mode-`0600` SQLite backup before applying the transactional migration; a fresh
install goes through the same ordered v1→v2 migration path. No manual migration
command is required.

## Input and resize safety

Ducklord cannot provide an authoritative owner in an input body. Ducklion
derives `terminal:<name>` from the connection handshake and compares it with
the persisted writer for agent sessions. It then checks instance ID, session
ID, ownership epoch, runtime generation, runtime lease, status, and adapter
health before forwarding bytes. The supervisor checks epoch and generation
again immediately before the PTY write.

Input is limited to 64 KiB per frame and serialized through a bounded pump.
Queued input is rejected after shutdown. A partial PTY write poisons that
runtime's input stream until restart. A response timeout is reported as an
unknown outcome and must never be automatically retried.

Resize is limited to 5–200 rows and 20–500 columns and is fenced at both daemon
and supervisor. Shell-session multi-writer behavior is intentional and follows
the specification's tmux-like model.

## Output and replay

The supervisor owns a bounded in-memory output ring (managed sessions currently
use 1 MiB). Output is published
in chunks no larger than 64 KiB, with contiguous byte offsets. Ducklion fans it
out through bounded per-viewer queues; a slow viewer never blocks PTY capture.

Subscription metadata and every event carry instance ID, session ID, runtime
generation, runtime lease ID, and subscription ID. Replay is split into 64 KiB
events, so Base64 expansion cannot exceed the 1 MiB frame limit. Stream end
distinguishes runtime disconnect from subscriber lag. Raw terminal bytes must
be applied to Ducklord's in-memory terminal model; they must not be printed
directly into the outer TUI terminal.

The transitional Ducklord attach path requests at most the newest 256 KiB from
the raw ring and then follows live output. Its visible text cache is bounded by
both 120 lines and 1 MiB, including newline-free output. Explicit detach wakes
blocked readers locally before the bounded best-effort unsubscribe RPC, while
the underlying host bridge remains available for other views and control.

## Current integration boundary

The daemon-backed create path, independent supervisor recovery, output
subscription, owner-fenced input, resize, and stdio bridge are implemented and
covered by socket, real-PTY, daemon-restart, and Podman tests. Ducklord uses the
authenticated bridge for inventory, creation, read, send, and streaming attach.
Changing owner closes and renegotiates retained bridges.

The remaining integration work includes yield/lifecycle RPCs, the complete
terminal framebuffer and resize signal wiring, durable activity subscriptions,
and Discord CC binding. Legacy CLI session state remains available only during
this staged cutover and must not be mixed with daemon inventory.

`scripts/ducklord-podman-demo.sh` is both a demo provisioner and a release
smoke: before printing the interactive TUI command it asserts daemon inventory,
PTY input/output, and same-session recovery across a real daemon restart in
separate containers.
