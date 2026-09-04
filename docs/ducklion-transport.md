# Ducklion PTY transport implementation

This document describes the implemented transport underneath the converged
[CC/terminal handoff specification](./cc-terminal-pty-handoff-spec.md). The
specification remains authoritative for product behavior.

## Process and trust boundaries

```text
Ducklord TUI
    │ one long-lived SSH stdio bridge (target architecture)
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

## Framing and negotiation

Frames use a four-byte big-endian length followed by JSON. A frame is capped at
1 MiB. Both peers negotiate a major/minor protocol version and explicit
capabilities. Connection setup requires only the `status` capability; APIs
check their own capability before use, allowing a clear unsupported-operation
error instead of failing an otherwise compatible connection.

Generic stdio/SSH streams do not provide socket deadlines. Client negotiation
therefore also accepts a context; expiry closes the stream so a remote bridge
that never answers cannot hang Ducklord startup indefinitely.

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

The supervisor owns a bounded 4 MiB in-memory output ring. Output is published
in chunks no larger than 64 KiB, with contiguous byte offsets. Ducklion fans it
out through bounded per-viewer queues; a slow viewer never blocks PTY capture.

Subscription metadata and every event carry instance ID, session ID, runtime
generation, runtime lease ID, and subscription ID. Replay is split into 64 KiB
events, so Base64 expansion cannot exceed the 1 MiB frame limit. Stream end
distinguishes runtime disconnect from subscriber lag. Raw terminal bytes must
be applied to Ducklord's in-memory terminal model; they must not be printed
directly into the outer TUI terminal.

## Current integration boundary

The daemon, supervisor recovery, output subscription, owner-fenced input,
resize, and stdio bridge are implemented and covered by socket and real-PTY
tests. The remaining integration work is the single-SSH multiplexing layer,
Ducklord terminal renderer/TUI wiring, yield/lifecycle RPCs, durable event
subscriptions, and the final process-level release E2E. Until that cutover is
complete, legacy CLI session commands remain present and must not be treated as
evidence that the final architecture is complete.
