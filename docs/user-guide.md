# Duckway User Guide

For administrators running Duckway and operators managing keys, clients, and agents.

For internals, code layout, or how the phantom-token swap works under the hood, see [developer-guide.md](developer-guide.md).

## Contents

- [What is Duckway?](#what-is-duckway)
- [Installation](#installation)
- [First-time setup](#first-time-setup)
- [Daily operations](#daily-operations)
- [Production deployment](#production-deployment)
- [Refreshable tokens (Claude, OpenAI/Codex, generic OAuth)](#refreshable-tokens-claude-openaicodex-generic-oauth)
- [Setting up agents](#setting-up-agents)
- [Control Channels (Discord-as-comms)](#control-channels-discord-as-comms)
- [Common tasks](#common-tasks)
- [Troubleshooting](#troubleshooting)

---

## What is Duckway?

Duckway is an API key proxy. Real API keys live encrypted on the Duckway server. Agents (Claude Code, scripts, CI runners) only ever see **phantom tokens** — strings that look identical to real keys (`sk-...`, `ghp_...`) but are useless to the upstream API. The Duckway proxy swaps phantom → real on its way to the upstream and strips the real key from any response back to the agent.

**Why use it?**

- Compromised agents leak phantom tokens, which you can revoke without rotating real keys
- Per-agent ACLs limit which endpoints / models / methods each agent can call
- Per-request approval gates dangerous calls (admin must click ✓ in Discord/Telegram)
- One real API key serves many agents; rotate centrally when needed

---

## Installation

### Server (Docker, recommended)

```bash
git clone git@github.com:moltingduck/duckway.git
cd duckway
./scripts/dev.sh up        # combined mode, http://localhost:9090/admin/
```

Default credentials are printed on first run and also visible via:

```bash
./scripts/dev.sh password
```

### Client (any machine that runs an agent)

The Duckway server hosts an installer at `/install.sh`. From the agent machine:

```bash
curl -fsSL http://your-duckway-host/install.sh | sh
```

Or use the prebuilt binaries from the `/srv/downloads/` route on the server:
- `duckway-client-linux-amd64`
- `duckway-client-linux-arm64`
- `duckway-client-darwin-amd64`
- `duckway-client-darwin-arm64`

---

## First-time setup

### 1. Add a service

Services are pre-seeded for OpenAI, Anthropic, GitHub, Discord, Telegram, and the internal heartbeat. To add a new one:

1. Admin panel → **Services** → **Add Service**
2. Fill in `name` (slug like `mistral`), `upstream_url` (`https://api.mistral.ai`), auth type and header, expected key prefix and length.

Click any service name to view full details and edit.

### 2. Add a real API key

1. Admin panel → **API Keys** → **Add API Key**
2. Pick the service, give the key a label, paste the real key
3. Duckway encrypts it (AES-256-GCM) and stores it; the plain key is never visible again
4. Click the key name in the list to see a masked preview (`sk-pro...7890`) for verification

### 3. Register a client

1. Admin panel → **Clients** → **Register Client**
2. Pick a name (`my-laptop`, `ci-runner-01`)
3. Copy the one-time client token shown — you cannot retrieve it later

### 4. Bind a phantom token

1. Admin panel → **Phantom Tokens** → **Generate Phantom Token**
2. Pick service, client, and the API key to back it
3. Toggle "Require admin approval" if you want first-use to need approval

### 5. Set up the agent machine

```bash
# On the agent machine
duckway init
# Follow prompts: server URL, client name, paste the client token

duckway sync     # pull phantom tokens from server
duckway proxy -d # start HTTPS MITM proxy in background

# `duckway env` prints both the keys AND the HTTP(S)_PROXY exports, so a single
# eval configures the agent's keys and routes its traffic through the proxy:
eval "$(duckway env)"
```

The agent now talks to `api.openai.com` / `api.anthropic.com` etc. as normal — Duckway intercepts, swaps phantom → real, and forwards.

---

## Daily operations

The admin panel uses the **same pattern across all pages**: click a row's name to open a detail modal showing all fields, then click **Edit** to switch the same modal to an edit form. Changes are saved with the **Save** button.

| Page | What's there | Detail / Edit fields |
|---|---|---|
| **Services** | Upstreams, key formats, default ACLs | Name, URL, host pattern, auth type/header/prefix, key format, ACL |
| **API Keys** | Real keys (encrypted) | Name, masked key preview, ACL, refreshable flag, usage |
| **Refreshable Tokens** | OAuth-style keys with refresh | Name, token endpoint, subscription info, agent display name |
| **Clients** | Registered agent machines | Name, status, canary, last seen — plus expandable phantom + canary tables |
| **Phantom Tokens** | Bindings between client + API key | Client, API key, service, env name, key path, approval, ACL |
| **Key Groups** | Pools of keys (round-robin / least-used / failover) | Name, strategy, members |
| **Approvals** | Pending requests | Approve / reject |
| **Notifications** | Telegram / Discord channels | Type, name, config + Test button |
| **Canary Tokens** | Honeypot settings | Email, types enabled |
| **Logs** | Recent proxy requests | Client, service, method, path, status |
| **Settings** | Gateway URL + proxy port | (Required for split mode — see below) |

---

## Production deployment

`scripts/prod.sh` reads from `.prod.env` and dispatches to the right docker-compose profile.

### Profiles

| Profile | Containers | Ports exposed | Use case |
|---|---|---|---|
| `combined` | `duckway-server` | `127.0.0.1:8080` | dev |
| `split` | `duckway-admin` + `duckway-gateway` | `127.0.0.1:9091` + `127.0.0.1:8080` | dev |
| `prod` | `duckway-server` | none | behind reverse proxy |
| `prod-split` | `duckway-admin` + `duckway-gateway` | none | behind reverse proxy |
| `tailscale-combined` | `duckway-server` + Tailscale sidecar | none, `:80` inside tailnet | prod |
| `tailscale` | `duckway-admin` + `duckway-gateway` + 2 Tailscale sidecars | none, `:80` each inside tailnet | prod, recommended |
| `client` | `duckway-client` test shell | none | opt-in sidecar/debug container; not part of prod runtime |

### .prod.env

```bash
# Tailscale auth key (or headscale pre-auth key)
TS_AUTHKEY=tskey-auth-...
TS_HOSTNAME=duckway

# For headscale (or other custom control servers):
TS_EXTRA_ARGS=--login-server=https://hs.example.com

# Mode
DUCKWAY_PROD_MODE=split           # split (recommended) or combined
DUCKWAY_TAILSCALE=true            # false to use prod / prod-split (no Tailscale)

# Optional
DISCORD_BOT_TOKEN=                # for Discord-bot approval channel
DISCORD_CHANNEL_ID=
```

### Commands

```bash
./scripts/prod.sh up            # build and start
./scripts/prod.sh down          # stop, keep data
./scripts/prod.sh restart       # build first, then recreate the active profile
./scripts/prod.sh restart --minimal  # rebuild/recreate app containers only; leave sidecars/deps running
./scripts/prod.sh ui            # rebuild/recreate only the UI-bearing service
./scripts/prod.sh logs          # follow logs
./scripts/prod.sh status        # container + Tailscale status
./scripts/prod.sh password      # show first-run admin password (if still in logs)
./scripts/prod.sh nuke          # asks confirmation, deletes all data
```

Use `restart --minimal` when only Duckway app images changed and dependency containers are already healthy. In split mode it recreates only admin + gateway; with Tailscale it leaves the Tailscale sidecars running. Use plain `restart` when you need Compose to reconcile the full dependency graph.

Use `ui` for template/static-only changes. In split mode this only rebuilds/recreates admin, so gateway traffic keeps flowing. In combined mode UI is embedded in the server binary, so `ui` recreates the combined server container.

The `duckway-client` Docker service is no longer part of prod startup. It is an opt-in test/debug shell:

```bash
docker compose --profile client up -d client
docker exec -it duckway-client sh
```

### Split-mode setting

In split mode, the **admin** and **gateway** run on different containers. Agents talk to the **gateway** (which serves `/proxy/*`, `/install.sh`, `/client/*`). The admin panel is a separate host that does not serve `/install.sh`.

You **must** set the gateway URL in **Settings → Gateway URL** (e.g. `http://duckway-gw`) so the Quick Install command and client init scripts point agents at the right host.

If you don't set it, the clients page warns:

> Gateway URL not configured. Set it in Settings (this admin panel does not serve `/install.sh` in split mode).

---

## Refreshable tokens (Claude, OpenAI/Codex, generic OAuth)

Some services use OAuth access tokens that expire and need refreshing. Duckway handles this automatically.

Refreshable token formats are provider-specific. Duckway stores the real access and refresh tokens encrypted, then issues provider-shaped phantom tokens to clients.

### Upload a Claude Code OAuth token

1. On a machine where you've already done `claude login`, copy `~/.claude/.credentials.json`
2. In the admin panel: **Refreshable Tokens** → **Upload Token**
3. Paste the JSON into the auto-fill box — fields populate automatically
4. Set **Agent Display Name** (this is what agents see in their fake `~/.claude.json`, e.g. `"CI Agent Bot"`)
5. **Upload**

Commands:

```bash
claude login
jq '.claudeAiOauth | {accessToken, refreshToken, expiresAt, subscriptionType, rateLimitTier, scopes}' ~/.claude/.credentials.json
jq -r '.claudeAiOauth | [.accessToken, .refreshToken, (.expiresAt // 0)] | @tsv' ~/.claude/.credentials.json
```

Expected Claude format:

```json
{
  "claudeAiOauth": {
    "accessToken": "sk-ant-oat01-...",
    "refreshToken": "sk-ant-ort01-...",
    "expiresAt": 1760000000000,
    "subscriptionType": "max",
    "rateLimitTier": "...",
    "scopes": ["user:inference"]
  }
}
```

Duckway refreshes the access token automatically before expiry (background job runs every 5 minutes), and never shows the real tokens again.

### Upload an OpenAI / Codex refreshable token

Codex stores ChatGPT-login credentials in `~/.codex/auth.json` after `codex login`. Select the OpenAI service in **Refreshable Tokens** → **Upload Token**, then paste the whole file into the auto-fill box.

Commands:

```bash
codex login
jq '{auth_mode, tokens: {access_token: .tokens.access_token, refresh_token: .tokens.refresh_token, account_id: .tokens.account_id}, last_refresh}' ~/.codex/auth.json
jq -r '.tokens | [.access_token, .refresh_token, (.account_id // "")] | @tsv' ~/.codex/auth.json
```

Expected Codex format:

```json
{
  "auth_mode": "chatgpt",
  "OPENAI_API_KEY": null,
  "tokens": {
    "id_token": "...",
    "access_token": "...",
    "refresh_token": "rt.1....",
    "account_id": "14d5c9cf-..."
  },
  "last_refresh": "2026-06-30T00:00:00Z"
}
```

The upload page derives **Expires At** from the `exp` claim inside `tokens.access_token`. Set **Token Endpoint** to `https://auth.openai.com/oauth/token` unless your Codex deployment uses a custom OAuth server.

### Upload a generic OAuth token

For non-Claude services, paste the token endpoint response or enter the fields manually. Common OAuth responses use snake_case, not Claude's camelCase:

```json
{
  "access_token": "ya29....",
  "refresh_token": "1//...",
  "expires_in": 3600,
  "scope": "read write",
  "token_type": "Bearer"
}
```

Set **Token Endpoint** to the provider's refresh endpoint. Duckway sends the stored refresh token there with `grant_type=refresh_token`. If the provider returns `expires_at` as Unix seconds, the upload page converts it to Duckway's Unix milliseconds format; `expires_in` is converted relative to the current browser time.

To extract values from a saved JSON response:

```bash
jq '{access_token, refresh_token, expires_in, expires_at, scope, token_type}' token-response.json
jq -r '[.access_token, .refresh_token, (.expires_in // .expires_at // 0)] | @tsv' token-response.json
```

If the provider only exposes tokens through a CLI, ask the CLI for JSON output when available, then paste that response. Otherwise copy the access token, refresh token, refresh endpoint, and expiry into the fields manually.

### File-based delivery (NOT environment variable)

Anthropic refreshable tokens are delivered to the agent as files, not env vars:

```
~/.claude/.credentials.json    # phantom OAuth tokens — overwritten on every sync
~/.claude.json                 # oauthAccount + onboarding flags — overwritten
~/.claude/settings.json        # default theme — only created if missing
```

`duckway sync` writes all three. The phantom tokens look exactly like real Claude OAuth tokens but only Duckway can swap them. Run `claude` and it picks them up automatically — no login flow.

---

## Setting up agents

### General agent (non-Claude)

```bash
# On the agent machine, after duckway init + sync
export HTTPS_PROXY=http://localhost:18080
export HTTP_PROXY=http://localhost:18080
duckway proxy -d              # background daemon

# The agent uses phantom keys from keys.env
source ~/.duckway/keys.env    # OPENAI_API_KEY=sk-... (phantom)
```

### Claude Code

See `examples/docker-compose.claude.yml` (full E2E with proxy + Tailscale) and `examples/docker-compose.claude-test.yml` (manual credential test).

Key points:
- The Duckway CA cert must be trusted by Node.js: `NODE_EXTRA_CA_CERTS=/path/to/ca.pem`
- `~/.claude/.credentials.json` is auto-generated by `duckway sync`
- Set `HTTPS_PROXY=http://duckway-proxy:18080` so all API calls hit the MITM proxy

---

## Control Channels (Discord-as-comms)

A **Control Channel (CC)** binds **one client to one Discord category** via a bot. Inside that category every text channel maps 1:1 to an agent session — every message a human types triggers Claude Code or Codex on the agent's machine and the result is posted back. A separate management channel accepts `!new` / `!end` / `!destroy` / `!reset` / `!list` / `!status` / `!help` text commands.

### Discord bot setup (first time, ~10 min)

This is the part that's not duckway-specific — you do it once at <https://discord.com/developers/applications/>.

**1. Create the application + bot**

- Open <https://discord.com/developers/applications> → **New Application** → name it (e.g. "Duckway Agent").
- Left sidebar **Bot** → Discord auto-creates one. Click **Reset Token** → copy the token NOW (it's shown once). This is the value you'll paste into duckway later. Treat like any password.

**2. Flip the privileged intents**

Same **Bot** page, scroll down to *Privileged Gateway Intents*:

- ✅ **Message Content Intent** — REQUIRED. Without it the bot sees every event but the message body arrives empty, so the daemon has nothing to forward to claude.
- ✅ **Server Members Intent** — OPTIONAL. Only flip if you'll use `discord_request_approval` with `required_reactors` (whitelist of who can decide).
- ✅ **Presence Intent** — leave OFF unless you have a separate need.

Save. ⚠️ If your app is verified (in 100+ servers) you'd need to apply for these — small / personal bots get them by toggling.

**3. Build the invite link**

Left sidebar **OAuth2** → **URL Generator**:

- *Scopes*: tick **`bot`** and **`applications.commands`**.
- *Bot Permissions*: tick:
  - **Manage Channels** (create / archive / move task channels)
  - **Send Messages**
  - **Add Reactions** (for `discord_request_approval` votes)
  - **Read Message History** (so the agent can read the channel above the latest message)

Copy the generated URL at the bottom, paste into a browser, pick the guild you want the bot in, and click **Authorize**.

**4. Per-category permission lock-down (recommended)**

The OAuth link above grants `Manage Channels` guild-wide. To narrow that to one category:

- In your guild: right-click the bot's role → **Edit Role** → uncheck **Manage Channels** in the global permissions.
- Right-click the **target category** → **Edit Category** → **Permissions** tab → **+ Add member or role** → pick the bot → grant **Manage Channels** + **Send Messages** + **Add Reactions** + **Read Message History** ONLY here.

Now if a CC's bot token leaks, blast radius is one category, not the whole guild.

**5. Find your `guild_id` and `category_id`**

Discord IDs aren't shown in the UI by default. Turn on Developer Mode first:

- Discord client → **User Settings** (cog icon) → **Advanced** → toggle **Developer Mode** on.

Then:

- *guild_id*: right-click your guild (the icon in the top-left server list) → **Copy Server ID**.
- *category_id*: right-click the category header (collapsible group above channels) → **Copy Channel ID**. Yes, "Channel ID" — Discord categories ARE channels internally with `type=4`.

Duckway can now discover servers and categories from the saved bot token, so you normally do not need to copy Discord IDs by hand.

### Admin one-time setup (in duckway)

1. **API Keys** → add the bot token from step 1 above. Pick service `discord`, paste the token.
2. **Control Channels** → **New CC** →
   - **Name** — anything you'll recognise (e.g. "Project Alpha CC").
   - **Client** — the duckway client this CC belongs to.
   - **Agent type** — `claude_code` or `codex` (when tmux is installed, both use attachable `duckway-<handle>` sessions; `--no-tmux` forces headless mode).
   - **Service** — `discord`.
   - **Bot Token** — the API key you uploaded in (1).
   - **Discord setup** — click **Load Discord setup**. If the bot is not in the server yet, click **Invite bot**, select the server in Discord, then refresh. Pick an existing category or create a new `duckway` category from the form.

   On save, duckway calls Discord to create `<client>-control` under the category and issues a phantom bot token bound to the client. If anything fails (bot lacks permission, wrong category id, etc.) the create rolls back and tells you which step failed.

3. Click into the CC → **Test (create + delete channel)** to verify connectivity end-to-end before assigning real workloads. Each step is reported individually so you know whether the failure is the token, the guild, the category, or the bot's perms.

### Per-client wire-up (agent machine)

```bash
duckway sync                                  # writes ~/.duckway/cc.json + ~/.claude.json mcp entry

# Run the watcher daemon in foreground to test:
duckway cc watch

# Or install the systemd user unit (Linux):
mkdir -p ~/.config/systemd/user/
cp examples/duckway-cc-watch.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now duckway-cc-watch
journalctl --user -u duckway-cc-watch -f
```

The daemon needs the selected agent binary (`claude` or `codex`) in `$PATH`. `duckway sync` also writes `~/.codex/config.toml` so Codex uses the `duckway-openai` provider (`OPENAI_API_KEY` from `~/.duckway/keys.env`, routed through the local proxy). Per-channel `cwd` defaults to `~/.duckway/cc-workspace/<handle>/` (auto-created); override with `!new --cwd /path` from the management channel.

### Inside an agent session, the model sees these MCP tools

`discord_get_my_cc`, `discord_list_channels`, `discord_create_task_channel`, `discord_archive_channel`, `discord_post`, `discord_edit_message`, `discord_delete_message`, `discord_read_recent`, `discord_wait_for_message`, `discord_request_approval` (reaction-vote — blocks until ✅/❌), `duckway_list_local_sessions`, `duckway_bind_session`.

### Attaching to a pre-existing Claude session

If you've already been chatting with Claude locally on the agent box (a session stored in `~/.claude/projects/`) and want a Discord channel to **continue** that conversation, do this from the management channel:

1. Send any message in `<client>-control` so the daemon spawns Claude — this first turn is a throwaway picker.
2. Ask the agent: *"list my local sessions"* → it calls `duckway_list_local_sessions` and posts the unbound sessions (newest first, with cwd + first-message preview).
3. Pick one: *"bind to the duckway one"* → the agent calls `duckway_bind_session(session_id)` (channel handle is auto-picked from the current channel env).
4. Send your next message → the daemon does `claude --resume <sid>` and the full prior history is restored.

The binding only takes effect on the **next** inbound message; the picker turn itself stays in the throwaway session. Run `!reset` afterwards if you want to forget the throwaway.

### Management channel commands

In `<client>-control`:
- `!new <slug> [--cwd <path>] [--topic "…"]` → create a task channel + register a session for it
- `!list` → table of task channels + which have running sessions
- `!status` → daemon up? agent type? counts?
- `!sessions [<cwd-filter>]` → list local Claude sessions on the agent that aren't yet bound to any CC channel
- `!bind <session_id> [<session_id> …]` → for each id, create a task channel (named after `basename(cwd)`) and attach the session — next message in the new channel resumes the existing conversation
- `!help`

`!sessions` / `!bind` run on the agent (the daemon owns the filesystem), so the cc-watch daemon must be up. The server posts an "offline" error if it isn't.

### Attaching from the CLI

`duckway cc bind` on the agent box does the same thing without going through Discord. With no args it prints a numbered table and reads selections from stdin — accept `1,3,5`, `1-3`, or `all`, and an empty line cancels. Each selection becomes its own task channel. For scripts: `duckway cc bind --session <id> [--session <id> …] [--cwd <substr>]`.

In any task channel:
- `!end` → end the current agent session and **archive** the Discord channel (history kept, channel renamed and removed from the category)
- `!destroy` → end the current agent session and **hard-delete** the Discord channel (history gone — useful for one-shot experiments)

The management channel itself also accepts plain text — the message is forwarded to the agent with a system note nudging it to spawn a dedicated task channel via `discord_create_task_channel` for any sustained work, instead of holding a long conversation inline.

### Security boundary

- The **bot token** is the only real boundary. Two CCs sharing a bot can reach each other's channels — use **different bots** to isolate teams.
- The agent never sees `channel_id`, `guild_id`, or `category_id` — only opaque `dwch_…` handles.
- A client can only operate within its own CC (HTTP 403 otherwise) AND any handle in a path is checked to belong to that CC.
- For `claude_code`, the daemon spawns claude with `--dangerously-skip-permissions`. For `codex`, the daemon uses `codex exec --json --sandbox workspace-write`; when tmux is installed, the command runs inside `duckway-<handle>` and leaves the pane open for inspection. Anyone in the Discord category can make the agent act. Trust the channel.

### Inbox tuning (Settings page)

| Key | Default |
|-----|---------|
| `cc_inbox_retention_hours` | 24 |
| `cc_inbox_max_per_channel` | 1000 |
| `cc_inbox_cleanup_interval_minutes` | 10 |

`DUCKWAY_CC_DISABLE_GATEWAY=1` skips the Discord WSS connection at server startup (REST + provisioning still work) — useful in test environments. `DUCKWAY_CC_DEBUG_INJECT=1` exposes a synthetic event endpoint for e2e (off by default in prod).

---

## Common tasks

### Reset admin password (forgotten password)

```bash
./scripts/reset-password.sh
# Generates a fresh random 16-char password, prints once
```

For a non-default username:

```bash
./scripts/reset-password.sh -u alice
```

The script never accepts a custom password — random only — to avoid leaks via shell history / `ps` / log files.

### Run the proxy as a daemon

```bash
duckway proxy -d              # start in background, returns immediately
duckway proxy status          # check if running
duckway proxy stop            # send SIGTERM, wait up to 2s

# Logs:
tail -f ~/.duckway/proxy.log
```

### Test phantom-token plumbing against real APIs

```bash
# 1. Drop your real keys into a gitignored env file:
cat > scripts/phantom-test.env <<'EOF'
export OPENAI_API_KEY=sk-...
export ANTHROPIC_API_KEY=sk-ant-api03-...
export GITHUB_TOKEN=ghp_...
export DISCORD_BOT_TOKEN=...
EOF
chmod 600 scripts/phantom-test.env

# 2. Run
./scripts/phantom-proxy-test.sh
```

The script auto-loads `scripts/phantom-test.env`, uploads each key, registers a fresh test client, calls the real upstream API through Duckway's proxy, and verifies the upstream returns 200. Cleanup runs automatically.

See [developer-guide.md](developer-guide.md#phantom-proxy-test-script) for the detailed flow.

### Add a new client and have an agent come online in one command

After registering the client in the admin panel, the modal shows a **"Full Setup"** copyable command:

```bash
curl -fsSL http://duckway-gw/install.sh | sh && \
  printf "http://duckway-gw\nmy-laptop\n1\n<token>\n" | duckway init
```

This installs the binary and runs `init` non-interactively in one shot.

### Use a custom Tailscale control server (headscale)

In `.prod.env`:

```bash
TS_AUTHKEY=hskey-auth-...                          # from `headscale preauthkeys create`
TS_EXTRA_ARGS=--login-server=https://hs.example.com
TS_HOSTNAME=duckway
```

Then `./scripts/prod.sh up`. Note: headscale doesn't issue HTTPS certs by default, so the Tailscale `tailscale serve` HTTPS feature won't work. Duckway handles this by binding the admin and gateway services directly to port 80 inside their tailnet network namespace. Access them as `http://duckway-admin/` and `http://duckway-gw/` (no port suffix).

---

## Troubleshooting

### "SSL certificate verification failed" from Claude Code

Claude (Node.js) doesn't trust the Duckway MITM CA. Set:

```bash
export NODE_EXTRA_CA_CERTS=/path/to/duckway/ca.pem
```

The CA cert is at `~/.duckway/ca.pem` after `duckway init`. The Docker examples (`examples/docker-compose.claude.yml`) wire this up automatically via a shared volume.

### "could not install CA to system trust store: could not find system CA directory"

The Duckway client tries `update-ca-certificates` (Debian/Ubuntu/Alpine), `update-ca-trust` (RHEL/Fedora), and `trust extract-compat` (Arch). If none of these binaries are in PATH, install the `ca-certificates` package on your distro and re-run `duckway init`.

### Quick Install command on the clients page is broken

If you're in **split mode**, the gateway URL setting is required. Go to **Settings → Gateway URL** and set it to your gateway's URL (e.g. `http://duckway-gw`). The clients page now reads from this setting; without it, the curl command would point at the admin host, which doesn't serve `/install.sh`.

### Tailscale containers say "Logged out"

Auth key expired or one-time-use already redeemed. Generate a new **reusable** key:
- Tailscale: https://login.tailscale.com/admin/settings/keys → check "Reusable"
- Headscale: `headscale preauthkeys create --user <name> --reusable`

Then update `TS_AUTHKEY` in `.prod.env` and run `./scripts/prod.sh restart`.

### `duckway proxy -d` says "already running" but no daemon is alive

Stale PID file. Run:

```bash
rm ~/.duckway/proxy.pid
duckway proxy -d
```

Normally this is handled automatically — `duckway proxy stop` and the SIGTERM handler clean it up — but a hard kill (`kill -9`) skips cleanup.

### Phantom proxy test fails with 401

The real API key you uploaded is invalid or expired (the upstream rejected it). Verify the key works directly first:

```bash
curl -H "Authorization: Bearer $OPENAI_API_KEY" https://api.openai.com/v1/models
```

If the direct call fails too, the key itself is bad. If only the Duckway path fails, see [developer-guide.md](developer-guide.md#proxy-flow) for the request flow and where to check logs.
