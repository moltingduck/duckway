# Native Codex remote control through Duckway

Run `duckway proxy serve` and, in another terminal, run
`duckway proxy exec -- codex remote-control --no-daemon`.
Keep that foreground host running. Its backend inherits Duckway's proxy,
CA and synced credential environment. `duckway proxy exec -- codex` also selects
foreground mode automatically; resume and fork use the same path. Existing
CODEX_HOME, history and unrelated shared daemon sessions are preserved.

The installed CLI must advertise `--no-daemon`; Duckway checks local help with
a three-second timeout before adding the root flag. Upgrade Codex if this check
fails. Codex exec, login and other independent subcommands retain their arguments.
Shared daemon lifecycle operations and remote-control start/stop/pair cannot
operate on this private foreground host and return an actionable error. Do not
restart a shared daemon just to change Duckway settings.

For an explicit manual pairing connection, run
`duckway proxy exec -- codex app-server --listen stdio://` and keep stdin open.
Send these newline-delimited JSON messages, waiting for each numbered response
before sending the next request (send `initialized` after response 1):

```json
{"id":1,"method":"initialize","params":{"clientInfo":{"name":"duckway","version":"1"},"capabilities":{"experimentalApi":true}}}
{"method":"initialized"}
{"id":2,"method":"remoteControl/enable","params":{"ephemeral":true}}
{"id":3,"method":"remoteControl/pairing/start","params":{"manualCode":true}}
```

Response 3 contains `pairingCode`, `manualPairingCode`, `environmentId` and
`expiresAt` in `result`. Use the returned code in the native controller.
This stdio connection directly owns its app-server; it does not use the shared
daemon socket. The opt-in real CLI fixture verifies enrollment, pairing and
relay ping/pong with dummy credentials. Mobile steering remains untested.

Duckway accepts only enrollment/refresh, pairing/status and the server WebSocket
at the fixed ChatGPT origin. Enrollment/refresh resolve the client's assigned
Codex OAuth credential server-side. The returned provider host bearer is
encrypted in the database and replaced with a random Duckway capability.
Pairing and relay resolve that capability back to its client, placeholder,
API key, account and host. Revoked assignments, changed accounts, expiry and
rotation invalidate use. Open relays close at expiry; other authorization changes
are revalidated on a one-second polling interval with a one-second database
lookup timeout; a stalled lookup can therefore delay revocation closure by
roughly two seconds. Pairing has no installation header in the native
protocol; the relay must match both installation and server headers.
Remote payloads and pairing secrets are excluded from request details and usage
metering. Only request metadata is logged.

Enrollment and refresh are serialized by installation using a bounded set of
locks shared by the server query instance. This deployment supports one Duckway
server process writing this database; multiple independent writers are unsupported.

## Protocol evidence

Pinned upstream: OpenAI Codex rust-v0.164.0, commit
`6aaac694803004a63c6f393d153c96d22521a6b9` (annotated tag
`fe3b29455488c2e189e43ad5526a107900c5e1b7`). No inferred endpoint schema.

* [Request/response types and URL construction](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/app-server-transport/src/transport/remote_control/protocol.rs#L20): POST enroll/refresh/pair/pair/status, GET server WebSocket.
* [Enrollment and refresh OAuth](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/app-server-transport/src/transport/remote_control/server_api.rs#L93).
* [Pair host bearer](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/app-server-transport/src/transport/remote_control/enroll.rs#L67) and [status](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/app-server-transport/src/transport/remote_control/enroll.rs#L172).
* [Relay bearer and protocol version3 headers](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/app-server-transport/src/transport/remote_control/websocket.rs#L1305).
* [Foreground syntax and subcommand restriction](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/cli/src/remote_control_cmd.rs#L75), [private foreground server](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/cli/src/remote_control_cmd.rs#L180).
* [Shared daemon environment behavior](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/app-server-daemon/README.md).
* [Pairing RPC shape](https://github.com/openai/codex/blob/6aaac694803004a63c6f393d153c96d22521a6b9/codex-rs/app-server-daemon/src/remote_control_client.rs#L58).

Local regressions: `go test ./internal/client ./internal/server/handlers ./cmd/client
-run 'TestProxyExec|TestCodexRemoteCredentialIsolation' -count=1`.
Live enrollment/relay, current CLI integration and mobile control are separate
verification claims; a synthetic fixture does not establish provider acceptance.

Relay monitoring uses one cancellable database snapshot of the capability,
client and placeholder binding/activity, service identity/activity, API key
activity, direct assignment or current group membership, and unexpired approval.
It then checks current OAuth metadata and source account plus all three ACL
layers. It does not rotate group selection or write usage counters. Endpoint
policy changes apply to established relays; rate limits are charged on the initial
request, not each monitor tick. Database lookup failure or timeout closes the
relay. Cancellation and capability expiry independently close the socket while
the bounded checker exits; stopping the watcher joins that checker.
