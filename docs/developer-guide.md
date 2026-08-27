# Duckway Developer Guide

For developers working on Duckway itself, adding new services, debugging the proxy flow, or running tests.

For installation, configuration, and daily operations, see [user-guide.md](user-guide.md).

## Contents

- [Architecture](#architecture)
- [Code layout](#code-layout)
- [Phantom token swap — the proxy flow](#phantom-token-swap--the-proxy-flow)
- [Header stripping](#header-stripping)
- [Refreshable (OAuth) tokens](#refreshable-oauth-tokens)
- [Data dependencies and delete behavior](#data-dependencies-and-delete-behavior)
- [Control Channels (Discord)](#control-channels-discord)
- [Adding a new service](#adding-a-new-service)
- [Testing](#testing)
- [Build and release](#build-and-release)

---

## Architecture

Three binaries from one Go module, sharing the `internal/server` package:

| Binary | Routes | Use |
|---|---|---|
| `duckway-server` | admin + gateway routes on one mux | combined mode |
| `duckway-admin` | admin panel + management API only | split mode admin node |
| `duckway-gateway` | proxy + client API + public endpoints | split mode gateway node |

A separate `duckway` (client) binary runs on agent machines to do `init` / `sync` / `proxy` / `status`.

Storage: SQLite via `modernc.org/sqlite` (pure Go, no CGO). One DB file per server, optional WAL mode. AES-256-GCM at rest for keys.

```
Agent machine                  Duckway gateway                    Upstream
─────────────                  ───────────────                    ────────

  curl api.openai.com          POST /proxy/openai/v1/messages
       ↓ HTTPS_PROXY=:18080    (X-Duckway-Token: <client-token>)
  duckway-client                       ↓
   MITM intercept                resolver: client+service → API key ID
   (known host)                       ↓
       ↓                          decrypt ─────────────────► api.openai.com
  POST .../proxy/openai/        inject Authorization: Bearer  (real key)
       ↓                              ↓
                                stream response back
                                       ↓
  back to curl ◄──────────────────────────
```

The agent never sees the real key. The duckway-client never sees it either — only the gateway holds the encryption key for the at-rest storage.

---

## Code layout

```
cmd/
  server/        duckway-server entry point
  admin/         duckway-admin entry point
  gateway/       duckway-gateway entry point
  client/        duckway client CLI (init, sync, proxy, status, ...)

internal/
  database/
    migrations.go              schema + ALTER TABLE migrations
    queries/                   one file per table — typed CRUD helpers
  models/
    models.go                  shared structs

  server/
    server.go                  setup, shared services, admin user bootstrap
    routes.go                  SetupAdminRoutes + SetupGatewayRoutes
    config.go                  flags + env + defaults

    middleware/
      admin_auth.go            session-cookie middleware for /api/* and /admin/*
      client_auth.go           X-Duckway-Token middleware for /proxy/*

    handlers/
      proxy.go                 the gateway’s core: phantom→real swap
      api_keys.go              CRUD + ACL templates + masked previews
      services.go              service CRUD + ACL templates
      clients.go               client CRUD
      placeholders.go          phantom token CRUD
      groups.go                key-group CRUD
      oauth.go                 refreshable-token upload + client credentials endpoint
      approvals.go             approval workflow
      notifications.go         notification channel CRUD + test
      canary.go                canary token settings + per-client gen
      admin.go                 page renderers (Go html/template)

    services/
      key_resolver.go          phantom → real key resolution
      crypto.go                AES-256-GCM wrapper
      oauth_refresh.go         background refresh job
      ca_manager.go            ECDSA P-256 CA + per-host leaf certs (MITM)
      acl_templates.go         22 pre-built ACL templates
      permission_checker.go    runtime ACL evaluation
      generators.go            generate phantom strings / IDs / passwords
      notifier.go              dispatch to telegram/discord/webhook
      discord_gateway.go       Discord WSS for reaction-based approvals
      telegram_poller.go       Telegram getUpdates polling

  client/
    config.go                  ~/.duckway/config.yaml read/write
    api.go                     thin Go HTTP client for the gateway
    sync.go                    SyncKeys, SyncClaudeCredentials, SyncCanaries
    https_proxy.go             local MITM proxy (CONNECT + per-host certs)
    proxy.go                   simple HTTP forwarder (legacy)
    tls.go                     CA install to system trust store

web/
  templates/                   Go html/template files (one per page)
  static/                      CSS + vendored htmx.min.js

scripts/
  dev.sh                       local dev container management
  prod.sh                      production with profile selection
  e2e-test.sh                  104+ tests against an ephemeral server
  phantom-proxy-test.sh        real-API end-to-end test
  reset-password.sh            CLI password reset for prod containers
  seed-dev.sh                  populate dev with realistic data

examples/
  docker-compose.claude.yml         full Claude E2E with proxy + Tailscale
  docker-compose.claude-test.yml    manual credential test (no proxy)
  docker-compose.agent.yml          generic agent + proxy sidecar

docs/
  user-guide.md
  developer-guide.md
```

---

## Phantom token swap — the proxy flow

### Client-side routing policy

The local HTTPS proxy routes in two stages because an HTTPS `CONNECT` exposes
the destination authority but not credentials inside TLS:

```text
canonical host
  -> unknown host: transparent tunnel
  -> explicit tunnel-only host: transparent tunnel
  -> managed host: terminate TLS, then inspect the HTTP request
       -> non-phantom credential (or no credential): direct provider request
       -> phantom credential + assigned service: Duckway server gateway
       -> phantom credential + unassigned service: local 403
       -> phantom credential + unknown assignment state: local 503
```

The corresponding decision matrix is:

| Host policy | Assignment | Credential | Result |
|---|---|---|---|
| Unknown | Not checked | Any | Transparent tunnel |
| Tunnel-only | Not checked | Any | Transparent tunnel |
| Managed | Unassigned | None or real | Direct provider request |
| Managed | Unassigned | Phantom | Local 403; never send the phantom upstream |
| Managed | Assigned | None or real | Direct provider request; never replace the real credential |
| Managed | Assigned | Phantom | Forward to the Duckway server gateway |
| Managed | Unknown | Phantom | Local 503 (fail closed) |

`/client/sync` is client-authenticated and returns one revisioned snapshot with
both the client's phantom keys and the full active service routing catalog. Each
service has assignment metadata for that client only, including unassigned
services so stale local phantoms cannot be mistaken for ordinary credentials on
an unknown host. The sidecar atomically writes this snapshot before replacing
its in-memory host map. `/client/keys` and `/client/services` remain available
for older clients.

The server remains authoritative and revalidates the phantom's client/service
binding on every gateway request. Stale metadata therefore fails safely: a
removed assignment may reach the server and be rejected there, while a newly
added assignment may receive a local error until the host map reloads.

Assignment state never decides whether a managed HTTPS host is intercepted at
`CONNECT` time: the sidecar must first decrypt TLS to distinguish a real
credential from a Duckway phantom. Explicit tunnel-only entries must be exact,
port-restricted destinations such as `gateway.discord.gg:443`. OAuth refresh
phantoms in structured request bodies are classified the same way as phantom
credentials in headers.

Direct provider requests do not use Duckway key ACLs, audit logging, or the
assigned token's upstream proxy. Those controls apply only after Duckway
resolves a phantom credential. The sidecar caches the last successful service
and assignment response; if neither the server nor that cache is available,
known managed hosts reject phantom traffic with `503` instead of leaking it to
the provider.

Only the phantom branch enters the Duckway server data path described below.

Implemented in `internal/server/handlers/proxy.go`, the `Handle` method:

```
POST /proxy/openai/v1/messages
  X-Duckway-Token: <client-token>
  Content-Type: application/json
  Authorization: Bearer sk-proj-dw_<phantom>     ← will be stripped
  body: {"model":"gpt-4","messages":[...]}
```

### Step 1 — Path parsing

```go
remainder := strings.TrimPrefix(path, "/proxy/")
parts := strings.SplitN(remainder, "/", 2)
serviceName := parts[0]                 // "openai"
upstreamPath := "/" + parts[1]          // "/v1/messages"
svc := h.services.GetByName(serviceName)
```

### Step 2 — Client authentication

`middleware.ClientAuth` (run before `Handle`) reads `X-Duckway-Token`, hashes it, looks up `clients.token_hash`, and stores the client on the request context:

```go
client := middleware.GetClient(r)       // *models.Client
```

### Step 3 — Resolve the phantom binding

`KeyResolver.ResolveForService(client.ID, svc.ID)` in `internal/server/services/key_resolver.go`:

1. Look up `placeholder_keys` row matching `client_id + service_id` — yields the placeholder
2. Look up the `api_keys` row referenced by the placeholder
3. Decrypt `api_keys.key_encrypted` using the server-side AES-256-GCM key
4. Return `ResolveResult{RealKey, IsRefreshable, PermissionConfig, APIKeyACL, ...}`

`IsRefreshable = (apiKey.RefreshToken != "")` — important for OAuth handling below.

### Step 4 — Approval gate

If the placeholder has `requires_approval = 1` and there's no valid approval row in `approvals`, the handler:

1. Creates a pending approval record
2. Calls `notifier.NotifyApprovalNeeded()` which fans out to active notification channels (Discord WSS, Telegram polling, webhooks)
3. Returns `403 duckway_approval_pending` so the agent retries later

### Step 5 — ACL evaluation (three-layer narrow-only)

For each non-empty layer (service.default_acl, api_key.acl, placeholder.permission_config), `PermissionChecker.Check` evaluates the JSON ACL against the request method, path, and body. **All non-empty layers must pass** — each can only narrow, never widen.

### Step 6 — Build upstream request

```go
upstreamURL := strings.TrimRight(svc.UpstreamURL, "/") + upstreamPath  // https://api.openai.com/v1/messages
upstreamReq, _ := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, bodyReader)
```

### Step 7 — Header copying with strip filter

```go
for key, values := range r.Header {
    if shouldStripHeader(key) { continue }
    for _, v := range values {
        upstreamReq.Header.Add(key, v)
    }
}
```

`shouldStripHeader` is the single source of truth for what never leaks upstream — see [Header stripping](#header-stripping) below.

### Step 8 — Inject the real key

```go
authType := svc.AuthType
authHeader := svc.AuthHeader
authPrefix := svc.AuthPrefix
if result.IsRefreshable {
    authType = "bearer"
    authHeader = "Authorization"
    authPrefix = "Bearer "
}

switch authType {
case "bearer": upstreamReq.Header.Set(authHeader, authPrefix + result.RealKey)
case "header": upstreamReq.Header.Set(authHeader, result.RealKey)
case "query":  q := upstreamReq.URL.Query(); q.Set(authHeader, result.RealKey); ...
}
```

The override for refreshable keys is critical: Anthropic's OAuth tokens require `Authorization: Bearer`, while raw API keys go in `x-api-key`. The same service config can serve both.

### Step 9 — Forward and stream back

```go
resp, _ := h.httpClient.Do(upstreamReq)
h.requestLog.Log(client.ID, result.PlaceholderID, serviceName, r.Method, upstreamPath, resp.StatusCode)
for k, vs := range resp.Header { for _, v := range vs { w.Header().Add(k, v) } }
w.WriteHeader(resp.StatusCode)
io.Copy(w, resp.Body)
```

The response is proxied back unchanged — no response-side filtering currently. (Future improvement: filter `Set-Cookie` or audit-log certain response patterns.)

### Diagram

```
                         /proxy/{svc}/{path}
                               │
 ClientAuth middleware ────────┤  validates X-Duckway-Token, attaches Client
                               │
 ProxyHandler.Handle ──────────┤
                               │
  GetByName(svc)               │  ─► not found → 404
                               │
  ResolveForService(...)       │  ─► no binding / inactive → 403
                               │
  approval check               │  ─► pending → 403 + notification
                               │
  ACL layers (3)               │  ─► denied → 403
                               │
  build upstream req           │
  copy headers (strip)         │
  inject real auth             │
                               │
  httpClient.Do() ─────────────┤  ─► transport error → 502
                               │
  log + stream back            │
                               ▼
                        agent receives raw upstream response
```

---

## Header stripping

`shouldStripHeader(name string) bool` in `internal/server/handlers/proxy.go` strips:

| Pattern | Reason |
|---|---|
| `X-Duckway-*` (any) | Internal — token, key, future ones — must never leak |
| `Authorization` | Carried the phantom; we inject the real one |
| `X-Api-Key` | Same |
| `Host` | Go fills this from the upstream URL |
| Hop-by-hop (RFC 7230 §6.1): `Connection`, `Keep-Alive`, `Proxy-Authenticate`, `Proxy-Authorization`, `Te`, `Trailer`, `Transfer-Encoding`, `Upgrade` | Per-hop, not end-to-end |

If you add a new internal header, just prefix it `X-Duckway-` and `shouldStripHeader` already covers it.

---

## Refreshable (OAuth) tokens

Stored in the same `api_keys` table as raw keys, with these extra columns populated:

```sql
refresh_token      TEXT      -- AES-encrypted refresh token
expires_at         INTEGER   -- Unix ms; 0 = never expires
token_endpoint     TEXT      -- e.g. https://console.anthropic.com/v1/oauth/token
subscription_info  TEXT      -- JSON: subscriptionType, rateLimitTier, scopes, displayName
```

A row is "refreshable" when `refresh_token != ""`. Detected at scan time:

```go
k.IsRefreshable = k.RefreshToken != ""
```

### Upload payload formats

Admin upload accepts provider-specific token shapes in the UI, then normalizes them before calling `POST /api/oauth/upload`:

- Claude Code: paste `~/.claude/.credentials.json`; the UI reads `claudeAiOauth.accessToken`, `refreshToken`, `expiresAt`, `subscriptionType`, `rateLimitTier`, and `scopes`.
- Generic OAuth: paste a token response with `access_token`, `refresh_token`, and either `expires_in` or `expires_at`.
- Manual entry: provide `access_token`, `refresh_token`, `token_endpoint`, optional `expires_at` in Unix milliseconds, and optional `subscription_info` JSON.

The API body remains normalized:

```json
{
  "name": "Claude OAuth",
  "service_id": "...",
  "access_token": "...",
  "refresh_token": "...",
  "expires_at": 1760000000000,
  "token_endpoint": "https://console.anthropic.com/v1/oauth/token",
  "subscription_info": "{\"subscriptionType\":\"max\"}"
}
```

Deletion is two-phase through `DELETE /api/oauth/{id}`. Without `{"confirm":true}`, the handler returns an impact preview listing Key Suite entries, client placeholders, and Control Channels using the refreshable key. With confirmation, it removes suite entries, deletes direct/suite client placeholders, detaches historical request logs, removes key-group memberships, and disables affected Control Channels. If a Control Channel still references the key, the API key row is retained as inactive and non-refreshable so the CC can show "reassign key" instead of being cascaded away.

### Background refresh

`internal/server/services/oauth_refresh.go` (`TokenRefresher`) runs a goroutine that wakes every 5 minutes:

1. `apiKeyQ.ListExpiring(10)` — keys expiring within 10 minutes
2. For each: decrypt `refresh_token`, POST to `token_endpoint` with `grant_type=refresh_token`
3. On success, encrypt the new `access_token` and call `UpdateTokens(id, encAccess, expiresAt)`. If the response includes a new `refresh_token` (rotation), `UpdateRefreshToken` too.

Started from `Server.startOAuthRefresher` on boot.

### Client delivery (Anthropic-specific)

`oauth.ClientGetCredentials` (`GET /client/claude-credentials`) returns a JSON envelope with two top-level fields:

```json
{
  "claudeAiOauth": { ...phantom tokens, scopes, subscriptionType... },
  "claudeConfig":  { ...oauthAccount with fake values, displayName, onboarding flags... }
}
```

The client splits this into three files in `internal/client/sync.go > SyncClaudeCredentials`:

- `~/.claude/.credentials.json`  ← `{claudeAiOauth: ...}` (always overwritten)
- `~/.claude.json`               ← `claudeConfig` (always overwritten — server controls oauthAccount)
- `~/.claude/settings.json`      ← `{"theme":"dark"}` (only if missing — preserves user prefs)

The phantom tokens look exactly like real Anthropic OAuth tokens (`sk-ant-dw_...` with the right length) so Claude Code accepts them locally without complaint and uses them as `Authorization: Bearer ...` in API calls.

---

## Data dependencies and delete behavior

Duckway keeps operational history even when admins delete clients, phantom tokens, keys, or suite entries. The schema therefore mixes real database cascades with explicit cleanup code in the query layer. Do not infer delete behavior from foreign keys alone.

### Core relationship graph

```mermaid
erDiagram
  SERVICES ||--o{ API_KEYS : owns
  SERVICES ||--o{ API_KEY_GROUPS : owns
  API_KEY_GROUPS ||--o{ API_KEY_GROUP_MEMBERS : has
  API_KEYS ||--o{ API_KEY_GROUP_MEMBERS : member

  CLIENTS ||--o{ PLACEHOLDER_KEYS : receives
  SERVICES ||--o{ PLACEHOLDER_KEYS : scopes
  API_KEYS ||--o{ PLACEHOLDER_KEYS : direct_key
  API_KEY_GROUPS ||--o{ PLACEHOLDER_KEYS : group_key

  KEY_SUITES ||--o{ KEY_SUITE_ENTRIES : bundles
  KEY_SUITES ||--o{ KEY_SUITE_ASSIGNMENTS : assigned_to
  CLIENTS ||--o{ KEY_SUITE_ASSIGNMENTS : assigned_suite
  KEY_SUITE_ENTRIES ||--o{ PLACEHOLDER_KEYS : materializes

  PLACEHOLDER_KEYS ||--o{ APPROVALS : approval_state
  PLACEHOLDER_KEYS ||--o{ REQUEST_LOG : nullable_reference
  CLIENTS ||--o{ REQUEST_LOG : nullable_reference
  REQUEST_LOG ||--o| REQUEST_LOG_DETAIL : capture_detail

  CLIENTS ||--o{ CONTROL_CHANNELS : controls_agent
  SERVICES ||--o{ CONTROL_CHANNELS : message_service
  API_KEYS ||--o{ CONTROL_CHANNELS : bot_key
  CONTROL_CHANNELS ||--o{ CC_CHANNELS : contains
  CONTROL_CHANNELS ||--o{ DISCORD_INBOX : buffers
  CONTROL_CHANNELS ||--o{ CC_AGENT_TESTS : tracks
```

`key_suite_assignments` is the durable source of truth for "this client is assigned to this suite". `placeholder_keys.suite_id` is only the materialized per-service token created from the current suite entries. This separation matters when a suite entry is removed: the service placeholder is deleted, but the client remains assigned to the suite so a later replacement entry can propagate back to that client.

### Delete and propagation rules

| Operation | What is deleted or changed | What is retained |
|---|---|---|
| Delete a client | Detaches `request_log.client_id` and affected `request_log.placeholder_id`; deletes the client's `control_channels`, `cc_channels`, `discord_inbox`, `canary_tokens`, `key_suite_assignments`, and `placeholder_keys`; then deletes `clients`. | `request_log` rows and `request_log_detail` history remain with nullable references cleared. |
| Delete a phantom token (`placeholder_keys`) | `PlaceholderQueries.Delete` first sets `request_log.placeholder_id = NULL`, then deletes the placeholder. `approvals` cascade from the placeholder. | Request history remains. |
| Delete a suite entry | `DeleteSuiteServicePlaceholders` detaches request logs for that suite/service and deletes matching suite-managed placeholders. | `key_suite_assignments` remain, so assigned clients stay bound to the suite even if service_count becomes 0. |
| Unassign a suite from a client | `UnassignClient` detaches request logs for that suite/client, deletes that client's suite-managed placeholders, and removes the `key_suite_assignments` row. | Individual non-suite placeholders on the client remain. The suite, entries, and API keys remain. |
| Add a suite entry | The handler lists `key_suite_assignments` and creates a suite-managed placeholder for each bound client unless that client has an individual non-suite placeholder conflict. | Existing individual placeholders win and are skipped. |
| Update a suite entry | `PropagateEntryUpdate` updates every active placeholder with matching `suite_id + service_id` to the new `api_key_id` or `group_id`; direct-key changes may regenerate placeholder token format. | Assignment records and request history remain. |
| Delete a suite | `key_suite_entries` and `key_suite_assignments` cascade from `key_suites`. Placeholder `suite_id` is `ON DELETE SET NULL`, so the client keeps usable placeholders but they stop being suite-managed. | Existing client placeholders remain as individual assignments. |
| Delete a refreshable API key | `DeleteRefreshableWithCleanup` removes suite entries, key group memberships, and client placeholders for the key; request logs are detached. Control Channels referencing the key are disabled and the API key row is retained as inactive/non-refreshable so the UI can tell the admin to reassign it. | Control Channel rows remain visible but inactive. Request history remains. |
| Delete a static API key | `DeleteWithControlChannelCleanup` disables referencing Control Channels before deleting the key when needed. | Control Channels remain visible but inactive. |
| Delete a Control Channel | The admin handler best-effort archives/deletes Discord resources first, then deletes the CC. `cc_channels`, `discord_inbox`, and `cc_agent_tests` cascade from `control_channels`. | Client and API key rows remain. |

### Rules of thumb

- Historical tables (`request_log`, `request_log_detail`, `conversation_usage`) should not disappear just because an admin removes a key/client/suite. Detach nullable references instead.
- UI-visible recovery states should prefer disabling rows over hard cascades. Control Channels are disabled when a bot key disappears so the admin can see and repair them.
- Suite assignment and suite materialization are different concepts. Use `key_suite_assignments` to find assigned clients; use `placeholder_keys.suite_id` to find currently materialized service tokens.
- When adding a new FK, decide whether the delete is part of business behavior or only referential safety. If history should survive, make the FK nullable and detach explicitly in a transaction.

---

## Control Channels (Discord)

CC v2: a Control Channel binds **one client to one Discord category** via a bot. Channels under the category are either:

- a single **management channel** (auto-created on CC create, named `<client>-control`, parses `!`-prefix commands server-side), or
- **task channels** (one per agent session — created via `!new` or the `discord_create_task_channel` MCP tool).

When a human posts in a task channel, the gateway forwards the event over SSE to the on-machine `duckway cc watch` daemon. For `claude_code`, the daemon runs `claude --print --output-format json` inside Duckway's PTY supervisor by default. For `codex`, it runs `codex exec --json` inside Duckway's PTY supervisor by default; follow-up turns use `codex exec resume <thread_id>`. The legacy tmux runner is still available with `duckway cc watch --tmux` or `DUCKWAY_CC_USE_TMUX=1`. For `openclaw`, it runs `openclaw agent --agent <id> --session-key duckway:<handle> --message-file <file> --json`; the agent id comes from `DUCKWAY_CC_OPENCLAW_AGENT` or defaults to `default`, and Duckway does not use OpenClaw's own channel bindings. The daemon posts the final agent message back to the channel.

Codex PTY turns intentionally do not use the legacy five-minute tmux timeout; long-running prompts such as `/goal` keep the per-channel runner occupied until Codex writes its completion event. The tmux runner still uses `in-flight.json` plus event files for startup recovery when explicitly enabled.

Tmux session names use `<handle>-duckway`. During upgrade, `migrateLegacyTmuxSession` renames the older `duckway-<handle>` convention to the current name when only the legacy session exists. If both names exist, it logs a warning and leaves both alone to avoid merging separate active turns.

### Tables (v2 — `client_cc` is gone)

| Table | Purpose |
|---|---|
| `control_channels` | One row per CC. Carries `client_id` (UNIQUE), `agent_type`, `placeholder_id`, plus the bot reference. `config` JSON holds `{guild_id, category_id}`. |
| `cc_channels` | Cache of every channel under a CC. `kind ∈ {management, task}`, plus `session_id` + `cwd` written by the daemon. |
| `discord_inbox` | Buffered gateway events for cold-start replay — long-polled by `/client/cc/inbox`. |

`placeholder_keys.env_name` for CC phantoms is `DUCKWAY_CC_<cc_id>` so the `UNIQUE(client_id, service_id, env_name)` triple doesn't collide.

### Components

```
internal/server/services/discord_bot.go      Thin REST client (Create/Archive/Delete channel, Post/Edit/Delete msg,
                                             AddReaction)
internal/server/services/cc_gateway.go       Per-bot WSS manager. Dispatches MESSAGE_CREATE/UPDATE/DELETE,
                                             CHANNEL_DELETE/UPDATE, MESSAGE_REACTION_ADD.
internal/server/services/cc_event_hub.go     In-process pub/sub. Per-client buffered chans, non-blocking publish.
internal/server/services/cc_commands.go      `!new` / `!end` / `!destroy` / `!reset` / `!list` / `!status` / `!help` parser
                                             plus `!sessions` / `!bind` which forward to the daemon
                                             via a `client_command` SSE event (agent owns the FS)
internal/server/services/cc_approvals.go     Reaction-vote registry for discord_request_approval
internal/server/handlers/control_channels.go Admin CRUD; Create provisions the management channel + phantom token
internal/server/handlers/cc_client.go        /client/cc/* — implicit cc_id (1:1), real IDs never returned

internal/client/sync_cc.go                   Writes ~/.duckway/cc.json + merges ~/.claude.json mcpServers entry
internal/client/mcp.go + mcp_tools.go        Stdio MCP server (JSON-RPC 2.0) — 11 discord_* tools +
                                             duckway_list_local_sessions / duckway_bind_session
internal/client/cc_watch.go                  SSE consumer + reconnect loop
internal/client/cc_runner.go                 Per-channel FIFO queue + agent exec wrapper
internal/client/cc_codex.go                  `codex exec --json` wrappers for headless + tmux
internal/client/cc_openclaw.go               `openclaw agent --message-file --json` adapter
internal/client/cc_session_store.go          ~/.duckway/cc-sessions.json persistence
internal/client/local_sessions.go            Scans ~/.claude/projects/*.jsonl for the session picker
internal/client/cc_client_commands.go        Daemon-side handlers for `!sessions` / `!bind`. Bind
                                             creates one task channel per session and writes the
                                             cc-sessions.json mapping. Reused by `duckway cc bind`.
cmd/client/main.go                           `duckway mcp serve` + `duckway cc watch` subcommands

examples/duckway-cc-watch.service            Sample systemd user unit
```

### Runtime flow (full round-trip)

```
Admin                     duckway-server                              agent machine
─────                     ──────────────                              ─────────────
GET /api/cc/discord/setup
                      ──→ decrypt bot token
                          Discord GET /users/@me + /users/@me/guilds
                          return invite_url + setup_invite_url + server picker data
POST /api/cc/discord/categories
                      ──→ Discord POST /guilds/{g}/channels type=4
                          Discord PUT /channels/{category}/permissions/{bot_user}
                          return category + permission status

POST /api/cc/discord/category-permissions
                      ──→ Discord GET /users/@me
                          Discord PUT /channels/{category}/permissions/{bot_user}

POST /api/cc          ──→ decrypt bot token
                          Discord POST /guilds/{g}/channels      ┐
                          issue phantom DUCKWAY_CC_<cc_id>       │   <-- CC create
                          persist control_channels + cc_channels ┘

                                                                 ┌─→  duckway sync
                                                                 │      ~/.duckway/cc.json
                                                                 │      ~/.claude.json (mcpServers entry)
                                                                 │
                                                                 └─→  duckway cc watch
                                                                        SSE: GET /client/cc/events

                          (Discord gateway WSS)
human types in task ────→ MESSAGE_CREATE (Discord WSS)
                          cc_gateway looks up cc_channels      ──→ SSE message_create event
                          publishes to hub + writes inbox           ▼
                                                                  cc_runner enqueue → selected agent resume "msg"
                                                                  (in cwd from cc_channels.cwd or default)
                                                                  parse JSON output → SessionStore.Set(handle, sess-X')
                          ◀── POST /client/cc/.../messages ────  post result back
                          bot.PostMessage → Discord

human deletes channel ──→ CHANNEL_DELETE
                          cc_channels row dropped
                          channel_delete event → SSE             ──→ cc_runner.Stop + SessionStore.Drop(handle)
```

For the `!cmd` flow, `routeMessageEvent` peels `!`-prefix messages off before they reach the inbox or SSE — they're handled in-process by `cc_commands.Handle` and the bot replies directly.

For `discord_request_approval`, the server posts the question + reactions, registers `(message_id → resultChan)` in `CCApprovalRegistry`, and blocks on the chan until the gateway routes a `MESSAGE_REACTION_ADD` to `Resolve()`.

### Security boundary

- Bot token = the only real boundary. Different teams → different bots.
- `cc_client.go` enforces two ACL layers: client must be bound to the CC (1:1), and every `{handle}` in a URL must belong to that CC.
- Daemon trust boundary is the Discord category: anyone in the category can drive the selected agent. `claude_code` currently runs with `--dangerously-skip-permissions`; `codex` runs with a per-CC sandbox enum (`workspace-write`, `read-only`, `danger-full-access`, or `none`) normalized by the server and sanitized again by the client; `openclaw` uses the local OpenClaw configuration and selected agent id. Claude/Codex use Duckway PTY sessions by default; legacy per-channel tmux sessions named `<handle>-duckway` are used only when `duckway cc watch --tmux` or `DUCKWAY_CC_USE_TMUX=1` is enabled.
- Agent options are not a generic argument channel. The server stores provider-specific `config.agent_options`, strips unsupported options for other agents, and rejects unknown Codex sandbox values. The client treats server state as untrusted, re-validates the same enum, and constructs `exec.Command` argv or a quoted tmux launch script from fixed argument positions. Do not add a free-form `args` or `flags` field to Control Channels; add a typed option and validate it on both sides.

### Test hooks

- `DUCKWAY_CC_DISABLE_GATEWAY=1` skips the WSS dial (REST + provisioning + SSE still work).
- `DUCKWAY_CC_DEBUG_INJECT=1` exposes `POST /api/cc/{id}/inject_event` so e2e can publish synthetic CCEvents (or resolve approvals via `type=reaction_add`) without a real Discord WSS.
- The runner tests use a fake `claude` shell script that prints deterministic JSON; `cc_session_store_test.go` covers persistence + atomic writes.

### Test environment

Set `DUCKWAY_CC_DISABLE_GATEWAY=1` to skip the WSS dial (REST + provisioning still work). Set `DUCKWAY_DISCORD_BASE_URL=http://...` to point the bot client at a mock server — the e2e suite uses both.

---

## Adding a new service

### 1. Seed (or admin-create) the service row

In `internal/server/server.go > seedDefaultServices` (only runs if no services exist) or via the admin panel **Services → Add Service**:

```go
{Name: "mistral", DisplayName: "Mistral AI",
 UpstreamURL: "https://api.mistral.ai", HostPattern: "api.mistral.ai",
 AuthType: "bearer", AuthHeader: "Authorization", AuthPrefix: "Bearer ",
 KeyPrefix: "", KeyLength: 32, KeyDirectory: ".config/mistral",
 IsActive: true},
```

### 2. (Optional) Add ACL templates

In `internal/server/services/acl_templates.go`, add an entry for `"mistral"` with one or more pre-built JSON ACL configs.

### 3. Test it

```bash
# In the admin panel: add an API key for mistral
# Register a client, bind a phantom for client+mistral
# Run from any tailnet client:
curl http://duckway-gw/proxy/mistral/v1/chat/completions \
  -H "X-Duckway-Token: <client-token>" \
  -H "Content-Type: application/json" \
  -d '{"model":"mistral-small","messages":[{"role":"user","content":"hi"}]}'
```

A `200` proves the swap. If you get `401`, check the `auth_type` / `auth_header` / `auth_prefix` in the service row.

### 4. Add to phantom-proxy-test.sh

Add another `run_test` line so the new service is included in the real-API test:

```bash
run_test "mistral" "$MISTRAL_ID" "$MISTRAL_API_KEY" \
  "GET" "/v1/models"
```

---

## Testing

### Unit + E2E (`scripts/e2e-test.sh`)

A single bash script that:

1. Builds all four binaries
2. Uses the opt-in docker-compose `client` profile or builds the duckway-client Docker image
3. Starts an ephemeral server on `127.0.0.1:19090` with a fresh data dir
4. Runs ~105 assertions across 15 categories: server health, auth, default services, key/client/placeholder CRUD, docker-client sync, proxy chain, key injection, approvals, ACL templates, ACL layering, canaries, admin pages, unit tests
5. Tears down

Run it from a clean working tree before any commit that touches the server:

```bash
./scripts/e2e-test.sh
```

Each test prints `PASS` / `FAIL` with the assertion. Any FAIL exits non-zero.

### Phantom proxy test script (`scripts/phantom-proxy-test.sh`)

Real-API end-to-end test, separate because it needs real keys. Auto-loads `scripts/phantom-test.env` if present (gitignored).

The flow per service (e.g. OpenAI):

1. Login as admin → session cookie
2. `POST /api/keys` with the real OpenAI key → server encrypts, returns key ID
3. `POST /api/clients` with a unique name → returns one-time client token
4. `POST /api/placeholders` binding client + service + key, `requires_approval: false`
5. **The actual test**: `GET http://duckway/proxy/openai/v1/models -H "X-Duckway-Token: <client-token>"` — note that no real key is in this request
6. Verify status `200`. Parse the upstream response and print a distinctive field (`models=42`) so you can see the call really went through to OpenAI.
7. `DELETE /api/clients/<id>` and `DELETE /api/keys/<id>` — cleanup

A passing run proves the full chain: auth → resolve → decrypt → inject correct header → real upstream returns success. A failure on any step bubbles up as the upstream's error body, helping diagnose where the chain broke.

The script never sends the real API key in the test request itself — the key is only sent once during the upload step, and the test request only carries the Duckway client token. So if `/proxy/...` returns 200, the only path that could have produced that result is Duckway resolving and injecting the real key correctly.

### Ducklord / Ducklion smoke test

Ducklord has a Podman demo that creates one developer laptop container and
multiple SSH-reachable remote clients. Use it when changing Ducklord, Ducklion,
remote PTY handling, SSH config parsing, project registry integration, or
standalone `ducklion` installation.

```bash
scripts/ducklord-podman-demo.sh
podman exec ducklord-dev ducklord clients --config /root/.ducklord/config.json
podman exec ducklord-dev ducklord ssh-hosts
podman exec ducklord-dev ducklord import-ssh-hosts --config /root/.ducklord/config.json
podman exec ducklord-dev ducklord probe client-a --config /root/.ducklord/config.json
podman exec ducklord-dev ducklord projects client-a --config /root/.ducklord/config.json
podman exec ducklord-dev ducklord sessions client-a --config /root/.ducklord/config.json
podman exec ducklord-dev ducklord read client-a alpha --lines 20 --config /root/.ducklord/config.json
podman exec -it ducklord-dev ducklord tui --config /root/.ducklord/config.json
podman exec -it ducklord-dev ducklord attach-host client-a --config /root/.ducklord/config.json
```

Inside the TUI:

- `a` adds a host entry from `/root/.ssh/config`; try `client-c`. The prompt
  also accepts a full SSH command such as
  `ssh -p 2222 -i ~/.ssh/id_ed25519 duck@client-c`.
- `d` removes the selected host entry from the current `config.json`; it does
  not stop remote Ducklion sessions.
- `n` creates a remote session with `agent -> host -> project`.
- `Enter` or right-click focuses the selected PTY session.
- `Ctrl-]` returns focus to the left menu.
- `ducklord attach-host client-a` opens the split-pane UI scoped to one remote
  host and disables add/new shortcuts.

For install-path behavior, the gateway `/install.sh` has an interactive
checkbox component menu:

```text
[x] Duckway client + Ducklion
[ ] Ducklord
```

Use `j/k` to move, `Space` to toggle, and `Enter` to continue. Remote agent
hosts should install `Duckway client + Ducklion`. Developer laptops can install
only `Ducklord`. Manual remote hosts must expose either
`ducklion` in `PATH` or `duckway ducklion` as a compatibility wrapper before
Ducklord can manage PTY sessions.

Non-interactive install defaults to `Duckway client + Ducklion`. To install
only Ducklord in user-local mode:

```bash
DUCKWAY_INSTALL_COMPONENT=ducklord DUCKWAY_INSTALL=user \
  sh -c "$(curl -fsSL http://your-duckway-gateway/install.sh)"
```

Ducklord itself is independent of Duckway server. Remote discovery and installs
use only the operator's local SSH config and SSH permissions:

```bash
ducklord import-ssh-hosts --config ~/.ducklord/config.json
ducklord install-ducklion <client> --source ./ducklion-linux-amd64 --config ~/.ducklord/config.json
ducklord attach-host <client> --config ~/.ducklord/config.json
```

When a host is added from the TUI, Ducklord probes `ducklion version` and
`ducklion list --json`. If Ducklion is missing, it attempts to install the local
Ducklion binary over SSH to `~/.local/bin/ducklion`, then probes again before
saving the resolved command path.

Clean up:

```bash
podman rm -f ducklord-dev ducklion-client-a ducklion-client-b ducklion-client-c
podman network rm ducklord-demo
```

See [Ducklord / Ducklion Remote Agent Control](ducklord-ducklion-spec.md) for
the complete terminal walkthrough and architecture notes.

### Ducklord-only Podman runner

Use this when you want to run only the developer laptop TUI in a container,
against your own SSH hosts, without starting the demo Ducklion client
containers:

```bash
scripts/ducklord-podman.sh
```

The script runs a multi-stage Podman build, so the host only needs Podman. The
build stage compiles `ducklord` inside a Go container, then the runtime stage
creates a small Alpine image. It mounts `~/.ssh` to `/home/ducklord/.ssh`,
mounts `~/.ducklord` to `/home/ducklord/.ducklord`, and starts:

```bash
ducklord tui --config /home/ducklord/.ducklord/config.json
```

The entrypoint creates a matching container user for the current host UID/GID
and runs Ducklord as that user. This keeps files written to `~/.ducklord` or
`~/.ssh` owned by the developer instead of root, while still giving OpenSSH a
valid home directory. Use `DUCKLORD_PODMAN_UID` and `DUCKLORD_PODMAN_GID` only
when you intentionally want different ownership. Use
`DUCKLORD_PODMAN_SSH_MOUNT=ro` if the container should not write `known_hosts`
or other SSH files.

Useful overrides:

```bash
SSH_DIR=~/.ssh-lab DUCKLORD_DIR=~/.ducklord-lab scripts/ducklord-podman.sh
DUCKLORD_PODMAN_NETWORK_ARGS="--network bridge" scripts/ducklord-podman.sh
DUCKLORD_PODMAN_SSH_MOUNT=ro scripts/ducklord-podman.sh
scripts/ducklord-podman.sh version
scripts/ducklord-podman.sh clients --config /home/ducklord/.ducklord/config.json
```

Pass extra Podman `run` options before the Ducklord command. Use `--` to
separate runner options from the Ducklord arguments:

```bash
scripts/ducklord-podman.sh \
  --podman-volume "$PWD:/workspace:rw" \
  --podman-env "DUCKLORD_EXPERIMENT=1" \
  -- tui --config /home/ducklord/.ducklord/config.json

scripts/ducklord-podman.sh \
  --podman-arg --add-host \
  --podman-arg "lab.local:10.0.0.5" \
  -- version
```

If `SSH_AUTH_SOCK` is set, the script mounts the agent socket into the
container. Ducklord still disables remote SSH agent forwarding when connecting
to host entries.

When Ducklord starts a non-shell agent through a remote Ducklion, Ducklion reads
the remote host's Duckway client config. If `~/.duckway/config.yaml` exists and
`~/.duckway/proxy.pid` points at a live proxy process, Ducklion injects the
Duckway proxy and CA environment into the agent session. Shell sessions are not
modified, and explicit env values from higher-level callers override the
defaults.

### Live GitHub App minter test

GitHub App installation token minting has an opt-in live test. It is skipped by default so ordinary test runs and CI do not need real credentials.

Store the credential in the ignored `secrets/` directory:

```bash
mkdir -p secrets
chmod 700 secrets
$EDITOR secrets/github-app-live.json
chmod 600 secrets/github-app-live.json
```

`secrets/github-app-live.json`:

```json
{
  "app_id": 4244336,
  "installation_id": 12345678,
  "private_key": "-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----",
  "repository": "OWNER/REPO"
}
```

Optional for GitHub Enterprise or a local mock:

```json
{
  "base_url": "https://github.example.com/api/v3"
}
```

Run the live test explicitly:

```bash
DUCKWAY_TEST_GITHUB_APP_LIVE=1 go test ./internal/server/handlers -run TestGitHubAppMinterLive -count=1 -v
```

By default, the test searches upward from the package test directory for `secrets/github-app-live.json`. Set `DUCKWAY_GITHUB_APP_LIVE_CONFIG=/absolute/path/to/github-app-live.json` to use a different location.

The test mints a short-lived installation token for `repository`, verifies GitHub grants `contents: read`, and asserts the handler response does not contain the `ghs_` token or private key. Do not commit files under `secrets/`; the directory is ignored by `.gitignore`.

To test the full Duckway client Git path against the same repository:

```bash
DUCKWAY_TEST_GITHUB_GIT_LIVE=1 go test ./internal/client -run 'Test(GitHubAppPhantomGitPullLive|DuckwayGitCloneLive)$' -count=1 -v
```

This starts an in-process Duckway server proxy, an in-process Duckway client MITM proxy, writes a phantom `GITHUB_TOKEN` to a temporary Git credential store, runs `git ls-remote https://github.com/OWNER/REPO.git HEAD`, then runs `duckway git clone OWNER/REPO` and verifies native `git ls-remote origin HEAD` works through the repo-local proxy config. The real GitHub App installation token is minted and used only inside Duckway. This test is also skipped unless `DUCKWAY_TEST_GITHUB_GIT_LIVE=1` is set.

To verify one GitHub App installation can mint and clone more than one assigned repository, run:

```bash
DUCKWAY_TEST_GITHUB_GIT_LIVE=1 go test ./internal/client -run TestDuckwayGitCloneMultipleReposLive -count=1 -v
```

The multi-repo test asks GitHub for the repositories visible to the configured App installation token and clones the first two returned repositories. If the installation lists fewer than two repositories, the test fails with a setup error; add at least two test repositories to the GitHub App installation before rerunning it.

To seed the live repository with deterministic random benchmark data, the GitHub App installation must have **Contents: read/write** permission and the repository assignment must allow `git-receive-pack`. The seed test is separately gated because it pushes to the configured repository:

```bash
DUCKWAY_TEST_GITHUB_GIT_SEED_LIVE=1 go test ./internal/client -run TestDuckwayGitCloneLiveSeedRepository -count=1 -v
```

By default this creates `.duckway-live-benchmark/seed-32mb-256files/` once. Override the fixture size with:

```bash
DUCKWAY_GITHUB_GIT_LIVE_SEED_MB=128 \
DUCKWAY_GITHUB_GIT_LIVE_SEED_FILES=1024 \
DUCKWAY_TEST_GITHUB_GIT_SEED_LIVE=1 \
go test ./internal/client -run TestDuckwayGitCloneLiveSeedRepository -count=1 -v
```

To benchmark the actual `duckway git clone` path against the live repository:

```bash
DUCKWAY_TEST_GITHUB_GIT_BENCH_LIVE=1 go test ./internal/client -run '^$' -bench BenchmarkDuckwayGitCloneLive -benchtime=3x -count=1 -benchmem -v
```

Add `DUCKWAY_TEST_GITHUB_GIT_SEED_LIVE=1` to the benchmark command if you want it to ensure the seed fixture exists before measuring.

### Live OAuth credentials

Real OAuth refresh credentials live under `live-credentials/` during local testing. The repository commits only `live-credentials/.gitkeep`; `.gitignore` ignores every other file in that directory, plus root-level `auth.json` and `test_auth*.json`.

Use these standard file names:

```bash
mkdir -p live-credentials
chmod 700 live-credentials
install -m 600 ~/.claude/.credentials.json live-credentials/claude-credentials.json
install -m 600 ~/.codex/auth.json live-credentials/codex-auth.json
```

Do not copy these files into tracked testdata. `go test` includes `TestLiveCredentialsAreNotTracked`, which fails if a credential-looking file is force-added to git.

### Live Claude/Codex OAuth refresh tests

Claude Code and Codex refresh tokens rotate. These tests are skipped by default, even if the credential files exist, so normal `go test ./...` does not contact real providers or consume refresh tokens.

There are two levels:

- Direct provider probes call only the provider token endpoint and prove the credential file can refresh.
- Duckway E2E tests import the live credential into a temp Duckway DB, run `POST /api/oauth/validate`, `POST /api/oauth/upload`, client phantom credential delivery, and `POST /api/oauth/{id}/refresh`, then write the rotated credential from Duckway storage back to the ignored live credential file.

Run Claude Code OAuth refresh explicitly:

```bash
DUCKWAY_TEST_CLAUDE_OAUTH_LIVE=1 \
go test ./internal/server/services -run TestClaudeCodeOAuthLiveRefreshIfCredentialsExist -count=1 -v
```

Run Codex OAuth refresh explicitly:

```bash
DUCKWAY_TEST_CODEX_OAUTH_LIVE=1 \
go test ./internal/server/services -run TestCodexOAuthLiveRefreshIfCredentialsExist -count=1 -v
```

Run Duckway upload/refresh E2E for Claude:

```bash
DUCKWAY_TEST_CLAUDE_OAUTH_LIVE=1 DUCKWAY_LIVE_CREDENTIALS_STRICT=1 \
go test ./internal/server/services -run TestClaudeCodeOAuthLiveDuckwayUploadRefreshE2EIfCredentialsExist -count=1 -v
```

Run Duckway upload/refresh E2E for Codex:

```bash
DUCKWAY_TEST_CODEX_OAUTH_LIVE=1 DUCKWAY_LIVE_CREDENTIALS_STRICT=1 \
go test ./internal/server/services -run TestCodexOAuthLiveDuckwayUploadRefreshE2EIfCredentialsExist -count=1 -v
```

Run all live OAuth tests only when you intentionally want to consume and rotate each provider credential more than once:

```bash
DUCKWAY_TEST_OAUTH_LIVE=1 \
go test ./internal/server/services -run 'Test(ClaudeCode|Codex)OAuthLive.*IfCredentialsExist' -count=1 -v
```

Default paths:

| Provider | Default file | Override |
|---|---|---|
| Claude Code | `live-credentials/claude-credentials.json` | `DUCKWAY_CLAUDE_LIVE_CREDENTIALS=/absolute/path` |
| Codex | `live-credentials/codex-auth.json` | `DUCKWAY_CODEX_LIVE_AUTH=/absolute/path` |

The files must be mode `600`. Each test creates a sibling `.lock` file before sending a real refresh request, then writes the rotated tokens back to the same ignored credential file on success. If the provider reports a permanent stale-token error such as `refresh_token_invalidated`, the test skips by default and tells you to sign in again. Set `DUCKWAY_LIVE_CREDENTIALS_STRICT=1` when you want stale live credentials to fail CI-style instead of skipping.

### Live Codex OAuth E2E

Codex OAuth phantom-token testing is script-based because it needs a real `~/.codex/auth.json`-style credential and runs Codex inside an isolated podman container.

Store the credential in the ignored `live-credentials/` directory:

```bash
mkdir -p live-credentials
install -m 600 ~/.codex/auth.json live-credentials/codex-auth.json
```

Check that the token file is readable and shaped correctly:

```bash
CODEX_AUTH=live-credentials/codex-auth.json ./scripts/codex-oauth-live-e2e.sh --check-token
```

Run the full live E2E:

```bash
CODEX_AUTH=live-credentials/codex-auth.json ./scripts/codex-oauth-live-e2e.sh
```

Run the optional control-channel watch path:

```bash
CODEX_AUTH=live-credentials/codex-auth.json ./scripts/codex-oauth-live-e2e.sh --cc-watch
```

The script builds Duckway in a throwaway podman container, starts a fresh server, uploads the real Codex OAuth tokens as a refreshable key, creates a client phantom, runs `duckway sync` + `duckway proxy`, then runs `codex exec` with the default prompt `hello?` through Duckway. Set `DUCKWAY_CODEX_PROMPT='your prompt'` to override the prompt. The token file must be mode `600`.

### Where to add new tests

- Pure-Go unit tests next to the code: `*_test.go` in the relevant package
- Integration via HTTP: extend `scripts/e2e-test.sh`. Each test category is a numbered section; copy an existing one and adjust.
- Real-API: extend `scripts/phantom-proxy-test.sh` with another `run_test` line.

---

## Build and release

### Build all binaries

```bash
go build ./...                                 # current platform
go build -o /tmp/duckway-server ./cmd/server/
go build -o /tmp/duckway-admin  ./cmd/admin/
go build -o /tmp/duckway-gateway ./cmd/gateway/
go build -o /tmp/duckway        ./cmd/client/
```

### Cross-compile clients (already done in the Docker build)

The server image's `Dockerfile` builds linux-amd64, linux-arm64, darwin-amd64, darwin-arm64 client binaries into `/srv/downloads/`. The gateway serves them at `GET /download/<name>`.

To rebuild the docker image after a code change:

```bash
docker compose -f docker-compose.yml build server gateway admin client
# or just bounce the dev env
./scripts/dev.sh restart
```

For production, prefer the scripted restart modes:

```bash
./scripts/prod.sh restart             # build first, then recreate active profile
./scripts/prod.sh restart --minimal   # app containers only; leaves sidecars/deps running
./scripts/prod.sh ui                  # UI-bearing service only
```

`restart --minimal` avoids touching Tailscale sidecars and other dependency containers. Use the full `restart` when dependencies may be unhealthy or compose wiring changed. The `client` compose service is opt-in for tests/debugging and is not part of prod runtime:

```bash
docker compose --profile client up -d client
```

### Embedded web assets

Templates and static files in `web/` are embedded into the binary via `web/embed.go` (`//go:embed`). To live-reload during dev:

```bash
DUCKWAY_WEB_DIR=$PWD/web ./duckway-server --port 8080 --data /tmp/dwdata
```

When `DUCKWAY_WEB_DIR` is set, the server uses `os.DirFS` for templates and static — refresh the browser, see the change. The dev compose profile sets this automatically.

### Database migrations

In `internal/database/migrations.go`, append a new entry to the migrations slice. They run idempotently with `CREATE TABLE IF NOT EXISTS` and safe `ALTER TABLE ADD COLUMN` (catches "duplicate column" errors). Never edit a previous migration — only append.

For a column that needs a backfill, add the `ALTER TABLE` followed by a separate `UPDATE ... WHERE column IS NULL` in the same migrations slice.
