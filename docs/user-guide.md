# Duckway User Guide

For administrators running Duckway and operators managing keys, clients, and agents.

For internals, code layout, or how the phantom-token swap works under the hood, see [developer-guide.md](developer-guide.md).

## Contents

- [What is Duckway?](#what-is-duckway)
- [Installation](#installation)
- [First-time setup](#first-time-setup)
- [Daily operations](#daily-operations)
- [Production deployment](#production-deployment)
- [Refreshable tokens (Claude OAuth, etc.)](#refreshable-tokens-claude-oauth-etc)
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
duckway env      # show shell exports
duckway proxy -d # start HTTPS MITM proxy in background

# Configure the agent's HTTPS_PROXY
export HTTPS_PROXY=http://localhost:18080
export HTTP_PROXY=http://localhost:18080
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
./scripts/prod.sh restart       # rebuild + restart
./scripts/prod.sh logs          # follow logs
./scripts/prod.sh status        # container + Tailscale status
./scripts/prod.sh password      # show first-run admin password (if still in logs)
./scripts/prod.sh nuke          # asks confirmation, deletes all data
```

### Split-mode setting

In split mode, the **admin** and **gateway** run on different containers. Agents talk to the **gateway** (which serves `/proxy/*`, `/install.sh`, `/client/*`). The admin panel is a separate host that does not serve `/install.sh`.

You **must** set the gateway URL in **Settings → Gateway URL** (e.g. `http://duckway-gw`) so the Quick Install command and client init scripts point agents at the right host.

If you don't set it, the clients page warns:

> Gateway URL not configured. Set it in Settings (this admin panel does not serve `/install.sh` in split mode).

---

## Refreshable tokens (Claude OAuth, etc.)

Some services use OAuth access tokens that expire and need refreshing. Duckway handles this automatically.

### Upload a Claude OAuth token

1. On a machine where you've already done `claude login`, copy `~/.claude/.credentials.json`
2. In the admin panel: **Refreshable Tokens** → **Upload Token**
3. Paste the JSON into the auto-fill box — fields populate automatically
4. Set **Agent Display Name** (this is what agents see in their fake `~/.claude.json`, e.g. `"CI Agent Bot"`)
5. **Upload**

Duckway stores both access and refresh tokens encrypted, refreshes the access token automatically before expiry (background job runs every 5 minutes), and never shows the real tokens again.

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

Lets a Claude Code agent talk to humans inside a Discord category — one channel per task — without seeing the bot token or any real Discord IDs.

### Admin one-time setup

1. Discord developer portal → make a bot, enable **Message Content Intent** if you want the agent to read replies.
2. Invite the bot to your guild with `Manage Channels` + `Send Messages` on the target category.
3. Admin panel:
   - **API Keys** → add the bot token under service `discord`
   - **Control Channels** → New CC → name it, pick the bot, fill `guild_id` + `category_id`

### Per-client wire-up

1. **Clients** → open the client → **Assign CC** → pick the CC + agent type (`claude_code`).
   Server calls Discord, creates a home channel named after the client, issues a phantom token bound to (client, bot, CC), and stores the binding.
2. On the agent machine: `duckway sync` writes `~/.duckway/cc.json` and merges a `duckway-cc` entry into `~/.claude/mcp.json`.
3. Next `claude` session sees these tools (only when at least one CC is assigned):
   `discord_list_assigned_ccs`, `discord_list_channels`, `discord_create_task_channel`, `discord_archive_channel`, `discord_post`, `discord_edit_message`, `discord_delete_message`, `discord_read_recent`, `discord_wait_for_message`.

If the client has only one CC, `cc_id` can be omitted on tool calls.

### Security boundary

- The **bot token** is the only real boundary. Two CCs that share a bot can reach each other's channels — use **different bots** to isolate teams.
- The agent never sees `channel_id`, `guild_id`, or `category_id` — only opaque `dwch_…` handles.
- A client can only operate within CCs it's assigned to (HTTP 403 otherwise) AND any handle in a path is checked to belong to that CC.

### Inbox tuning (Settings page)

| Key | Default |
|-----|---------|
| `cc_inbox_retention_hours` | 24 |
| `cc_inbox_max_per_channel` | 1000 |
| `cc_inbox_cleanup_interval_minutes` | 10 |

`DUCKWAY_CC_DISABLE_GATEWAY=1` skips the Discord WSS connection at server startup (REST + provisioning still work) — useful in test environments.

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
