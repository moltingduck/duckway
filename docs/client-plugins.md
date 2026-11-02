# Server-managed client plugins (API v1)

Duckway server owns upstream plugin sources, downloaded releases, and the approved
version of each plugin. Clients install a named plugin exclusively from their
configured Duckway server. A client request never fetches from an upstream
publisher. Ducklion continues to own PTYs and session lifecycle.

`duckway plugin install NAME` remains the server-managed path: it requires the
configured Duckway server, installs only its approved catalog entry, and pins the
server origin. For local development testing, an explicit offline path is also
available:

```sh
duckway plugin install-local --dir /path/to/duckllo-package
duckway duckllo --help
```

The package directory contains `manifest.json` and each artifact at its declared
canonical absolute path with the leading slash removed (for example,
`/client/releases/VERSION/BINARY` maps to
`client/releases/VERSION/BINARY` beneath the package directory). The installer
reads only those local files, verifies the manifest API version, platform, size,
and SHA-256, and makes no network request or Duckway configuration/token change.
Treat a local package as executable code you trust. Local installs are marked
`local`, shown by `duckway plugin installed`, excluded from fleet reports, and
cannot replace a server-managed install. Repeating `install-local` may atomically
replace a prior local release while preserving plugin data.

Plugin API version 1 describes the manifest and foreground executable contract;
it is independent of Duckway core or server product versions. Duckllo still
requires a compatible Duckllo server for pairing and worker/task operations.

First initialize the Duckway client against your server, then install an approved
plugin and pair Duckllo using stdin:

```sh
duckway init
duckway plugin list
duckway plugin install duckllo && duckway duckllo pair --server https://duckllo.example --invite-stdin
```

The Duckllo origin is an example; use your actual deployment and supply its
invitation through stdin using your normal secret-input mechanism. The Duckway
administrator must import and approve a shipped Duckllo release before installation
is available. These examples do not establish a public plugin release or hosted
manifest. Installation does not pair or request an invitation.

```sh
duckway plugin list                         # Approved server catalog
duckway plugin installed                    # Local installed releases
duckway plugin update duckllo               # Current server-approved release
duckway plugin run duckllo -- whoami
duckway duckllo whoami
```

Remote operations require saved `server_url` and `token` from `duckway init`.
There is no client manifest-URL/network override or upstream-source
configuration. Server-managed records pin the normalized configured Duckway
server origin. Updates refuse to switch to another configured server or to
replace a local install; migration requires an explicit separately managed
procedure. Changing a token on the same server is supported. API v1 does not
provide an uninstall command.

After installing through Duckway, update with `duckway plugin update duckllo`.
Duckllo standalone self-update is for standalone installations. Host activation
records must stay consistent with the executable digest. Duckllo owns pairing and
credentials in its default OS user configuration directory. Invitations use stdin,
never CLI arguments, manifests, host logs, or shell interpolation.

## Client execution and registry

`plugin run NAME -- ARGS` and `duckway NAME ARGS` pass argv directly without a shell,
inherit stdin/stdout/stderr, and return the child's exit status. Signal exits return
`128 + signal`; host cancellation returns 130. Cancellation kills the foreground
plugin process group and waits for its direct child. Helpers must stay in that
group; plugins must not daemonize or escape the supervisor. Interactive terminal
ownership is handed to the child and restored when it exits.

When Duckway has local configuration, plugin invocation reuses `proxy exec`'s
local proxy and CA environment. Otherwise it preserves the inherited environment.
The host replaces inherited plugin metadata with `DUCKWAY_PLUGIN_API_VERSION=1`,
`DUCKWAY_PLUGIN_NAME`, `DUCKWAY_PLUGIN_SOURCE` (`managed` or `local`),
`DUCKWAY_PLUGIN_CONFIG_DIR` (optional plugin-owned host data), and
`DUCKWAY_PLUGIN_ORIGIN` (empty for local packages, otherwise the pinned Duckway
server origin). It never exports the saved Duckway client token to a plugin and
removes an inherited `DUCKWAY_TOKEN`.
Duckllo's own server origin and credential location remain connector-owned. A
plugin executes with the user's permissions and inherits other parent environment
variables; choose the approved executable and its environment accordingly.

The registry lives under `<Duckway config>/plugins` (default `~/.duckway/plugins`),
with private directories and owned regular files. Symlink path components,
foreign-owned ancestors, and nonsticky group/other-writable ancestors are rejected.
Ancestors owned by root or the current user are supported; public sticky parents
such as `/tmp` are supported with owned children. A lock serializes activation and
is cancellable while waiting. Downloads stage on the registry filesystem. The
client checks exact byte count and SHA-256, syncs the executable, and atomically
replaces `NAME/current.json`. Failed validation or download preserves the prior
activation and `NAME/data`. Old executable releases are reclaimed after activation;
a running Unix executable remains valid. Plugin data survives updates. The client
checks executable integrity again before launch.

## Authenticated server protocol

The client sends its existing `X-Duckway-Token` only to the configured Duckway
origin, using a direct HTTP transport that does not inherit ambient proxies.
Redirects are forbidden for catalog, manifest, and artifact requests. The server
protects all plugin routes with existing client authentication:

| Request | Response |
| --- | --- |
| `GET /client/plugins` | JSON array of approved `Manifest` objects; `[]` when empty |
| `GET /client/plugins/NAME/manifest` | One approved `Manifest` object |
| `GET /client/plugins/NAME/releases/VERSION/OS/ARCH` | Exact cached executable bytes |
| `POST /client/plugins/report` | Authenticated full installed inventory and latest manual results |

Clients require every artifact URL to exactly match the authenticated release route
for its manifest name, version, OS and architecture. They never accept upstream,
proxy, public core-download, or cross-origin artifact routes. Catalog and download
handlers serve only cached approved state, even if the publisher is unavailable.
The client catalog is capped at 16 MiB and 256 unique plugin names.

A server-delivered manifest looks like:

```json
{
  "api_version": 1,
  "name": "duckllo",
  "version": "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "description": "Duckllo client connector",
  "artifacts": [
    {
      "os": "linux",
      "arch": "arm64",
      "url": "/client/plugins/duckllo/releases/sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/linux/arm64",
      "sha256": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
      "size": 123456
    }
  ]
}
```

`description` is optional; the other fields are required and unknown JSON fields
are rejected. `version` is a publisher-defined content release identifier with
literal `sha256-` followed by 64 lowercase hex characters. Every artifact has its
own 64-character lowercase SHA-256 digest and positive byte size, capped at 512 MiB.
A manifest is capped at 1 MiB and 128 unique OS/architecture entries. Names match
`[a-z][a-z0-9-]{0,62}` and cannot shadow Duckway commands or aliases. The client
selects its current Go OS and architecture. Configured server URLs require HTTPS, with no credentials, query, fragment, or
path override. HTTP is allowed only for `localhost` or literal loopback addresses
for isolated development. Public or private-network plaintext HTTP servers are
rejected before the client token or executable request is sent.

The upstream publisher manifest uses the same schema and hashes, with canonical
same-origin absolute artifact paths such as `/client/releases/VERSION/BINARY`.
Upstream sources are configured and imported on the Duckway server, which validates
bytes before publishing a cached release and rewrites only artifact URLs for client
consumption. SHA-256 protects transfer integrity; administrative publisher approval
and upstream transport establish which executable is trusted.

See [Server plugin administration](server-plugins.md) for administrator import,
source restrictions, enable/disable, and server-only origin allowlisting.

## Fleet version and manual-update reports

After each manual install/update, the client saves its latest outcome separately
from executable activation and posts an authenticated full inventory snapshot to
`POST /client/plugins/report`. The existing control heartbeat and manual
`duckway sync` also refresh that snapshot. There is no additional daemon and
these reports never trigger plugin installation or update.

Local-only installs are excluded. When the registry has no server-managed
inventory or report history, sync and heartbeat skip the plugin-report request;
this keeps a client with only local test packages compatible with older Duckway
servers. If managed history exists but the current snapshot is empty, Duckway
sends the empty snapshot so the server can clear previously reported inventory.

Reports contain plugin names, activated installed versions, and optional latest
manual results (`install`/`update`, `succeeded`/`failed`, stable random attempt ID,
and a fixed error classification). They exclude tokens, publisher URLs,
invitations, raw error text, child arguments, and client-supplied timestamps. Only
records bound to the configured Duckway server are included. Empty installed
version means a failed first installation, never success. A registry scan failure
sends no replacement inventory. A completed empty snapshot means no plugins from
that server are installed; omitted records cease being current while the server
may retain their history. There is no uninstall command in this version.

The server derives client identity from authentication and timestamps receipt.
Replaying the same attempt ID preserves its update timestamp. Clients with no
report are unknown; stale/offline views are based on the server's last receipt,
not an invented success or client clock. Reporting failures print **not reported**
and retry on the next sync or heartbeat. They preserve local activation and a
successful install/update exit status. Failure to save the outcome locally has a
separate warning; inventory can still be refreshed, but that missing result is
not claimed to be durably queued.

An installed version describes the activated release for future invocations.
Already running native plugins continue executing their previous release after
an update; reports do not claim their running process has changed. Updates never
patch a live plugin or restart its Ducklion session.

## Existing session supervision

Use an ordinary Duckway session with the plugin as its foreground command:

```sh
duckway session start --name duckllo -- duckway duckllo whoami
```

For long-running connector work, replace `whoami` with its actual foreground command
and arguments. Existing Duckway/Ducklion session entry owns its PTY, output, stop,
and attach flow. API v1 provides generic foreground dispatch; it does not add
task-aware native Duckllo hooks, task assignment, or a second supervisor.

## Bounded acceptance and verification

API v1 covers the authenticated cached server catalog, install by name,
list/installed/update/run, fallback dispatch, server-origin pinning, transfer
integrity, private paths, failure-preserving activation, argument/stdio/status
forwarding, proxy/CA inheritance, cancellation, and existing-session composition.
Host tests use loopback Duckway fixtures, synthetic tokens, and helper executables.

```sh
go test ./internal/clientplugin ./cmd/client
go test -race ./internal/clientplugin ./cmd/client
go test -covermode=atomic -coverprofile=OWNED_PATH ./internal/clientplugin ./cmd/client
go vet ./internal/clientplugin ./cmd/client
```

Independent integration verification must exercise server import of a shipped
Duckllo artifact, client installation from the authenticated server, stdin pairing,
`whoami`, corrupt import/update rejection, and identity preservation after failure.
A fixture-origin round trip establishes this contract; production auth and
task-aware hooks need their own explicit evidence. Final feature gates are in
`agent-workflow.md`.
