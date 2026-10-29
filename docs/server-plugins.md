# Server-managed client plugins

Only administrators publish plugin releases. Registering a source checks its
manifest and stores an available candidate. Publishing explicitly approves that
exact candidate: the server downloads **every** artifact, verifies declared sizes
and SHA-256 hashes, then atomically activates the immutable release. Review the
source and publisher before publishing. The server never runs plugins.
Client catalog, manifest and binary requests use the local cache only.

The authenticated admin API provides:

- `GET /api/plugins`: configured sources, enabled status, current version, description, current artifact platform/size/hash details and release count.
- `POST /api/plugins/import`: `{"name":"duckllo","manifest_url":"https://publisher.example/plugin-manifest.json"}`.
- `POST /api/plugins/duckllo/check`: `{}`; fetch manifest metadata only.
- `POST /api/plugins/duckllo/publish`: `{"version":"sha256-<64 lowercase hex>","manifest_digest":"<64 lowercase hex>"}`; publish the checked available version and enable it.
- `POST /api/plugins/duckllo/disable` or `/enable`: `{}`; enable requires a previously published release.
- `GET /api/plugins/duckllo/clients`: reported installation and update status for every known client.

These use the existing `duckway_session` cookie. Mutations require
`Content-Type: application/json` and an `Origin` matching the admin request origin,
or a same-origin `Referer` when `Origin` is absent. Nonbrowser operators explicitly
supply that Origin with their session cookie. Bodies are limited to 8 KiB, reject
unknown fields, and contain exactly one JSON object. The first registration fixes the
source URL for that plugin name; subsequent registrations must use the identical URL. Registration checks only the manifest; it never downloads binaries or changes the published version. A failed initial check retains the registered source and a sanitized error for retry.
There is deliberately no source-switch API. To migrate publishers, use a new
plugin name and review it as a new source. Do not hand-edit the cache catalog.

Clients authenticate with existing ClientAuth (`X-Duckway-Token`) on:

- `GET /client/plugins`: an array of enabled current API-v1 manifests (`[]` when empty).
- `GET /client/plugins/{name}/manifest`: that enabled current manifest.
- `GET /client/plugins/{name}/releases/{version}/{os}/{arch}`: verified cached bytes.

Client manifests use only `/client/plugins/.../releases/...` artifact paths. No
publisher URL is exposed to clients. All enabled plugins are available to all
authenticated clients; this API does not add per-client plugin entitlements.
Old immutable releases remain cached and downloadable while the plugin is enabled,
so installs using an older fetched manifest remain consistent after an update.
Disabling hides catalog/manifest/binaries, including old versions. A download
already in progress can finish. Disable does not revoke a binary already installed
on a client or prevent that client from running its local copy.

`DataDir/plugins` holds private `catalog.json`, `.lock`, and immutable release
files. Admin and gateway deployments must mount **the same persistent DataDir**;
per-plugin flock locks serialize source work while short global catalog locks
and atomic rename synchronize separate processes. Upstream HTTP and artifact
verification never hold the global lock, so cached reads, client reports and
disable actions remain available during slow source requests. Publication uses
a final catalog comparison and aborts if an enable/disable action changed its
starting state. Use a local filesystem with flock/rename/fsync semantics. Back up the whole directory
consistently; keep it accessible only to the service user. Every ancestor must be
owned by root or the service user. Symlink ancestors,
writable ancestors (except trusted sticky directories such as
`/tmp`), and nonprivate cache directories/files are rejected. The cache root must
be owned by the service user; its opened directory descriptor is checked again
for identity, ownership and private permissions. Failed checks and publishes
preserve the previous active release. No database migration or public `/download` entry is used.
Old releases are retained indefinitely; operators must provision disk capacity.
Interrupted imports may leave unreferenced files; they are never catalog-visible.

Upstream defaults to HTTPS with public DNS destinations. URLs cannot contain
credentials, query, fragment or noncanonical paths. Artifacts must be canonical
absolute paths at the manifest's origin. Redirects, ambient HTTP proxies, and
compressed transfers are disabled. Each dial resolves DNS, validates every answer,
and connects directly to a validated IP, preserving TLS hostname validation.
Client cookies/tokens are never used in upstream requests. Imports have a three
minute total deadline, two minute request bounds, bounded headers, at most 32
artifacts, at most 512 MiB each and at most 1 GiB aggregate. Manifests are capped
at 1 MiB and the catalog at 256 plugin names. Responses and errors do not expose upstream details to clients.

For a deliberately internal publisher, configure the **server process** environment:

```sh
DUCKWAY_PLUGIN_ALLOWED_ORIGINS=http://127.0.0.1:18080,https://duckllo.internal.example
```

This comma-separated allowlist matches exact normalized scheme/host/port origins;
it is never accepted from an API request. It permits private/loopback destinations
for those origins only. Plain HTTP requires such an origin and private/loopback
DNS answers; public plaintext HTTP stays disallowed. Link-local, cloud metadata,
multicast, unspecified, carrier-grade NAT and reserved address ranges stay blocked,
even for an allowed origin. Configure the variable on the admin process that imports;
the gateway never contacts publishers. Restart after changing the policy.


## Checking and publication

The admin process checks registered manifests every six hours by default. Set
`DUCKWAY_PLUGIN_CHECK_INTERVAL` to a Go duration between `1m` and `168h` to change
it. The combined server and admin-only server run this scheduler; gateway-only
processes never check publishers. There is no startup check. The scheduler uses a polling timer of at most one minute (independent of the
check interval), preventing a source registered between ticks from
waiting another full interval. Per-plugin filesystem locks prevent duplicate checks
from multiple admin processes using the shared volume. Shutdown cancels in-flight
HTTP and waits for the checker to exit.

`GET /api/plugins` additionally reports `available_version`,
`available_manifest_digest`, `available_description`, `available_artifacts`,
`last_checked_at`,
`last_successful_check_at`, `next_check_at`, and a sanitized `check_error`.
`version` remains the published version or empty before first publication.
A version is a content digest; “available” means different, with no semantic
version ordering. A failed check retains the last successfully observed candidate,
but publication requires another successful check. A successful check never
activates binaries or changes enabled state.

Publish includes the exact checked `version` and the server-computed
`available_manifest_digest`. This digest covers every manifest field and binds
approval to the data reviewed in the admin panel, including artifact hashes,
sizes, URLs and description; a reused publisher version cannot reuse approval. The server refetches the manifest
before and after downloading artifacts and requires the whole manifest to match
the checked candidate. If it changes or becomes unavailable, publication stops
and a new check is required. Previously published versions remain immutable and
available while enabled. Client updates are manual by default. Publication does
not start, stop, or replace any running client process.

## Client reporting

`POST /client/plugins/report`, behind existing ClientAuth, accepts this full
snapshot, bounded to 256 KiB and 256 distinct plugin names:

```json
{"api_version":1,"plugins":[{"name":"duckllo","version":"sha256-<64 lowercase hex>","last_update":{"id":"<32 lowercase hex>","operation":"update","status":"failed","error_code":"download_failed"}}]}
```

`last_update` is optional; operation is `install` or `update`, status is
`succeeded` or `failed`. Failures use only `download_failed`,
`verification_failed`, `server_changed`, `local_error`, `canceled`, or
`operation_failed`. Success has an empty error code. An empty version is allowed
only for a failed first install. `plugins: []` reports an empty local inventory.
Client IDs, client timestamps, URLs and free-form errors are rejected. The server
binds every report to the authenticated client's database identity. Report files
are private and atomic within the shared plugin volume; no database migration is
needed.

The server sets `seen_at` on receipt and `last_update_at` on a new attempt ID.
Repeating the same attempt preserves its original timestamp; changing that
attempt's content is rejected. Fleet rows include `client_id`, `client_name`,
`seen_at`, `last_update_at`, `installed_version`, `published_version`, `status`,
and optional `last_update`. Status is `unknown` without a report, `stale` after
24 hours, `update_failed`, `not_installed`, `unpublished`, `up_to_date`, or
`different`. These are client-reported facts, not binary attestation. Unknown and
stale clients are never assumed current. Deleted clients' old report files do not
appear in the fleet view; operators should include them in cache capacity and
backup planning.

## Isolated inspection demo

From the candidate checkout, run `scripts/plugin-isolated-demo.sh` with Podman
and curl installed. It builds only the required source directories in the pinned
Go image, starts a fresh server at `http://127.0.0.1:18880/admin/plugins`, and
prints the exact command for viewing its fresh randomly generated admin password.
The empty database has no provider keys, channels, plugins or live credentials;
external gateway integrations remain unconfigured. Neither the primary checkout's
`.runtime` nor any user credential/configuration directory is mounted.

Set `DUCKWAY_PLUGIN_DEMO_OWNER` to a unique lowercase owner token and
`DUCKWAY_PLUGIN_DEMO_PORT` to an unused loopback port if needed. An explicitly
set `DUCKWAY_PLUGIN_ALLOWED_ORIGINS` is passed only as server operator
configuration, so an isolated internal publisher can be tested deliberately. The launcher
refuses an existing owner container or temporary root. Its private database,
keys, build outputs and logs live under ignored `tmp/<owner>`; Ctrl-C or exit
removes both named containers and this root. Keep the launcher running only for
an explicitly retained inspection session. This demo creates no installed client,
model-provider calls, production service restarts or deployment changes.
