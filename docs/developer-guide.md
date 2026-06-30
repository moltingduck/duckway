# Duckway Developer Guide

For developers working on Duckway itself, adding new services, debugging the proxy flow, or running tests.

For installation, configuration, and daily operations, see [user-guide.md](user-guide.md).

## Contents

- [Architecture](#architecture)
- [Code layout](#code-layout)
- [Phantom token swap — the proxy flow](#phantom-token-swap--the-proxy-flow)
- [Header stripping](#header-stripping)
- [Refreshable (OAuth) tokens](#refreshable-oauth-tokens)
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

## Control Channels (Discord)

CC v2: a Control Channel binds **one client to one Discord category** via a bot. Channels under the category are either:

- a single **management channel** (auto-created on CC create, named `<client>-control`, parses `!`-prefix commands server-side), or
- **task channels** (one per agent session — created via `!new` or the `discord_create_task_channel` MCP tool).

When a human posts in a task channel, the gateway forwards the event over SSE to the on-machine `duckway cc watch` daemon. For `claude_code`, the daemon drives a long-lived Claude TUI in tmux when tmux is installed, falling back to the headless print runner. For `codex`, it runs `codex exec --json` inside a per-channel tmux session when tmux is installed, falling back to headless `codex exec --json`; follow-up turns use `codex exec resume <thread_id>`. The daemon posts the final agent message back to the channel.

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
internal/client/mcp.go + mcp_tools.go        Stdio MCP server (JSON-RPC 2.0) — 10 discord_* tools +
                                             duckway_list_local_sessions / duckway_bind_session
internal/client/cc_watch.go                  SSE consumer + reconnect loop
internal/client/cc_runner.go                 Per-channel FIFO queue + agent exec wrapper
internal/client/cc_codex.go                  `codex exec --json` wrappers for headless + tmux
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
- Daemon trust boundary is the Discord category: anyone in the category can drive the selected agent. `claude_code` currently runs with `--dangerously-skip-permissions`; `codex` runs with `--sandbox workspace-write`. When tmux is installed, both supported agents get per-channel attachable sessions named `duckway-<handle>`.

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
