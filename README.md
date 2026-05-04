# Duckway

API proxy that manages real API keys centrally. AI agents use **phantom tokens** — fake keys that look identical to real ones but get swapped by the proxy. Agents never see real keys.

## Features

- **Reverse proxy** with API key injection (`/proxy/{service}/...`)
- **HTTPS MITM proxy** — transparent interception via CONNECT tunnels
- **Three-layer ACL** — 22 pre-built templates (OpenAI, Anthropic, GitHub, Discord, Telegram) + custom JSON rules. Each layer can only narrow, never widen.
- **Approval workflow** — admin approves via Discord reactions / Telegram buttons / web panel, configurable TTL per key
- **Key groups** — round-robin, least-used, failover to avoid rate limits
- **16 canary token types** — auto-deployed honeypots via [canarytokens.org](https://canarytokens.org), per-client Gmail `+` tagging
- **Admin panel** — dark theme, Go templates + HTMX, search/filter/pagination, live ACL preview
- **Discord Gateway WSS + Telegram polling** — interactive approval without public endpoints
- **Control Channels (Discord)** — agents talk to humans in a Discord category via MCP tools; real channel IDs / guild IDs / bot tokens never leave the server
- **190 E2E tests** + unit tests

## Documentation

- **[User Guide](docs/user-guide.md)** — installation, configuration, daily operations, production deployment, troubleshooting
- **[Developer Guide](docs/developer-guide.md)** — architecture, code layout, proxy flow, header stripping, adding services, testing

## Quick Start

### Development

```bash
# Clone
git clone git@github.com:moltingduck/duckway.git
cd duckway

# Configure dev secrets (optional — Discord bot for notifications)
cp .env.example .dev.env
# Edit .dev.env

# Start (combined mode, admin + gateway on one port)
./scripts/dev.sh up

# Admin panel: http://localhost:9090/admin/
# Username: duckway  Password: duckway
```

### Production

`scripts/prod.sh` reads `.prod.env` and dispatches to a docker-compose profile:

| `DUCKWAY_PROD_MODE` | `DUCKWAY_TAILSCALE` | Profile used | Ports |
|---|---|---|---|
| `split` (default) | `true` (default) | `tailscale` | `:80` inside tailnet on `duckway-admin` and `duckway-gw` |
| `combined` | `true` | `tailscale-combined` | `:80` inside tailnet on `duckway` |
| `split` | `false` | `prod-split` | none — put a reverse proxy in front |
| `combined` | `false` | `prod` | none — put a reverse proxy in front |

#### With Tailscale (recommended)

```bash
cp .env.example .prod.env
# Edit .prod.env:
#   TS_AUTHKEY=tskey-auth-xxxxx        (Tailscale admin)
#   TS_HOSTNAME=duckway
#   DUCKWAY_PROD_MODE=split            (default)
# Optional for headscale:
#   TS_EXTRA_ARGS=--login-server=https://hs.example.com

./scripts/prod.sh up

# Access from any node on your tailnet:
#   Admin:   http://duckway-admin/
#   Gateway: http://duckway-gw/
```

The first-run admin password prints once; recover later with:

```bash
./scripts/prod.sh password           # tries to grep it from logs
./scripts/reset-password.sh          # generates a fresh random password
```

> **Tailscale ports note**: services bind directly to `:80` inside the
> Tailscale node's network namespace. `tailscale serve` HTTPS is not used
> because headscale doesn't issue HTTPS certs, and the tailnet is already
> private — no need to MITM-terminate again.

#### Without Tailscale (behind a reverse proxy)

```bash
# .prod.env
DUCKWAY_TAILSCALE=false
DUCKWAY_PROD_MODE=split

./scripts/prod.sh up
# Containers expose nothing — point your reverse proxy at:
#   duckway-admin:9091
#   duckway-gateway:8080
```

#### Split-mode required setting

In split mode, the **gateway** serves `/install.sh` and `/proxy/*`; the
**admin** does not. Set the gateway URL in **Settings → Gateway URL**
(e.g. `http://duckway-gw`) so the Quick Install command on the Clients
page and the registration token modal point agents at the right host.

### Client Setup (on agent machines)

```bash
# One-liner install + register
curl -fsSL http://duckway-gw/install.sh | sh
duckway init        # interactive: server URL, client name, paste token

# Or copy the full setup command from admin panel → Clients →
# Register Client → "Full Setup" — installs and registers in one shot.
```

For more, see the [User Guide](docs/user-guide.md).

## Architecture

```
┌─────────────────────┐                ┌──────────────────────────────────┐
│  Agent Machine      │                │  Duckway prod (split + Tailscale) │
│                     │                │                                  │
│  AI Agent           │ HTTPS_PROXY    │  ┌─ tailscale-admin ─────────┐  │
│  └→ duckway proxy   │───────────────→│  │ duckway-admin :80         │  │
│     (MITM, CONNECT) │                │  │ Web panel + management API│  │
│                     │                │  └───────────────────────────┘  │
│  ~/.duckway/        │ /proxy/{svc}   │  ┌─ tailscale-gateway ───────┐  │
│  ├── config.yaml    │───────────────→│  │ duckway-gateway :80       │  │
│  ├── keys.env       │                │  │ Proxy + client API + dl   │  │
│  ├── ca.pem         │                │  └───────────────────────────┘  │
│  └── canary files   │                │            │                    │
│                     │                │   ┌────────▼─────────┐          │
└─────────────────────┘                │   │ SQLite + AES-256 │          │
                                       │   └──────────────────┘          │
                                       └──────────────────────────────────┘
```

### Split Mode (recommended)

| Container | Port (internal) | Reachable from | Purpose |
|---|---|---|---|
| `duckway-admin` | `:80` (or `:9091` non-Tailscale) | admins only | Web panel, management API |
| `duckway-gateway` | `:80` (or `:8080` non-Tailscale) | agents | Proxy, client API, downloads, `/install.sh` |

Agents **cannot** reach the admin panel. With Tailscale, enforce by ACL on the admin tailnet hostname. Without Tailscale, put a reverse proxy in front and route only `duckway-gw` to public agents.

## Scripts

| Script | Purpose |
|--------|---------|
| `./scripts/dev.sh up` | Build + start dev containers + seed test data |
| `./scripts/dev.sh nuke` | Wipe data + containers |
| `./scripts/prod.sh up` | Production start (Tailscale or `prod` profile via `.prod.env`) |
| `./scripts/prod.sh status` | Container + Tailscale node status |
| `./scripts/prod.sh logs` | Follow logs |
| `./scripts/prod.sh password` | Show admin password (if still in logs) |
| `./scripts/reset-password.sh` | Generate a fresh random admin password (prod) |
| `./scripts/e2e-test.sh` | Run the full E2E suite (190 tests) |
| `./scripts/phantom-proxy-test.sh` | Real-API test against OpenAI/Anthropic/GitHub/Discord |

## Environment Files

| File | Purpose | In git? |
|------|---------|---------|
| `.env.example` | Template | Yes |
| `.dev.env` | Dev secrets | No |
| `.prod.env` | Prod secrets (Tailscale auth key) | No |

## Docker Compose Profiles

A single `docker-compose.yml` with profiles for every deployment mode:

| Profile | Containers | Tailscale | Ports exposed |
|---|---|---|---|
| `combined` | `duckway-server` | No | `127.0.0.1:8080` (dev) |
| `split` | `duckway-admin` + `duckway-gateway` | No | `127.0.0.1:9091` + `127.0.0.1:8080` (dev) |
| `prod` | `duckway-server` | No | none — reverse proxy in front |
| `prod-split` | `duckway-admin` + `duckway-gateway` | No | none — reverse proxy in front |
| `tailscale-combined` | `duckway-server` + Tailscale sidecar | Yes | none, `:80` inside tailnet |
| `tailscale` | `duckway-admin` + `duckway-gateway` + 2 Tailscale sidecars | Yes | none, `:80` each inside tailnet |

```bash
docker compose --profile tailscale up -d              # what prod.sh does by default
docker compose --profile prod-split up -d             # split, no Tailscale, no exposed ports
```

## Proxy Modes

### HTTPS Proxy (transparent)

```bash
export HTTPS_PROXY=http://localhost:18080
curl https://api.openai.com/v1/chat/completions ...
# → duckway client MITM → gateway → key injection → upstream
```

### Direct URL (lightweight)

```bash
curl http://duckway-gw/proxy/openai/v1/chat/completions \
  -H "X-Duckway-Token: <token>" ...
```

## ACL System

Three layers, each can only narrow access:

```
Service default_acl (widest)
  ∩ API Key acl
    ∩ Phantom Token permission_config (narrowest)
```

22 pre-built templates:

| Service | Templates |
|---------|-----------|
| OpenAI | allow-all, chat-only, chat-embeddings, inference-all, no-admin |
| Anthropic | allow-all, messages-only, no-batches |
| GitHub | allow-all, read-only, repo-read, issues-prs, no-destructive, gists-only |
| Discord | allow-all, webhook-only, messages-only, read-only |
| Telegram | allow-all, send-only, read-only, no-admin |

## Canary Tokens

16 types auto-deployed on client machines:

| Type | Path | Mode |
|------|------|------|
| AWS Credentials | `~/.aws/credentials` | merge |
| Kubernetes Config | `~/.kube/config.bak` | create |
| WireGuard Config | `~/.config/wireguard/wg-company.conf` | create |
| GitHub Token | `~/.git-credentials` | merge |
| .env File | `~/.env.production.bak` | create |
| SSH Private Key | `~/.ssh/id_deploy` | create |
| Bash History | `~/.bash_history` | merge |
| PostgreSQL .pgpass | `~/.pgpass` | merge |
| .bashrc Exports | `~/.bashrc` | merge |
| + 7 more optional types | | |

Per-client email tagging: `admin+shortid@gmail.com` identifies which machine was compromised.

## Notifications

| Channel | Interactive Approval |
|---------|---------------------|
| Telegram Bot | Inline buttons via getUpdates polling |
| Discord Bot | ✅/❌ reactions via Gateway WSS |
| Discord Webhook | No (fire-and-forget) |
| Generic Webhook | No (fire-and-forget) |

No public endpoints needed — all outbound connections.

## Control Channels (Discord)

A **Control Channel (CC)** = `{bot, guild, category}`. Each assigned client gets a home text channel under the category, plus the ability to create per-task channels at runtime — all driven through MCP tools the agent's Claude Code session sees automatically.

```
Admin → New CC (Discord bot + guild_id + category_id)
      → Assign CC to client + agent_type           ──→ Discord auto-creates channel
                                                       phantom token issued to client

Agent  duckway sync                                ──→ ~/.duckway/cc.json
                                                       ~/.claude/mcp.json (writes "duckway-cc")
       claude                                       ──→ launches `duckway mcp serve`
       (model calls discord_post / discord_create_task_channel / …)
```

Real Discord IDs (channel_id, guild_id, category_id) **never leave the server** — agents only ever hold opaque `dwch_…` handles. Two-layer ACL: the client must be assigned to the CC, AND any handle in a path must belong to that CC. A server-side gateway WSS connection per bot writes `MESSAGE_CREATE` events into a SQLite inbox; `discord_wait_for_message` long-polls it.

Bot token = security boundary. Two CCs sharing one bot can technically reach each other's channels — use a different bot to isolate teams. See the **Control Channels** section in `/admin/docs` for the full walkthrough.

## Tech Stack

- **Server**: Go, single binary (~14MB)
- **Client**: Go, single binary (~7.5MB)
- **Database**: SQLite (embedded, no external DB)
- **Admin panel**: Go `html/template` + vendored HTMX + CSS
- **Encryption**: AES-256-GCM for API keys at rest
- **Proxy**: Go `net/http` + `crypto/tls` for MITM
- **Zero npm, zero runtime dependencies**

## CLI Reference

### Server

```bash
duckway-server --port 8080 --data /path/to/data
duckway-admin --port 9091 --data /path/to/data
duckway-gateway --port 8080 --data /path/to/data
# Env: DUCKWAY_LISTEN, DUCKWAY_DATA_DIR, DUCKWAY_DEV=1
```

### Client

```bash
duckway init                # Register + download CA + sync
duckway sync                # Refresh keys + canary tokens + Claude config + CC state
duckway env                 # Print keys as shell exports
duckway proxy [--port N]    # Start HTTPS MITM proxy (foreground)
duckway proxy -d            # Start as background daemon
duckway proxy stop          # Stop the running daemon
duckway proxy status        # Show daemon status
duckway status              # Server, keys, heartbeat, proxy, CA
duckway mcp serve           # Stdio MCP server for Control Channels
duckway update              # Compare with server, replace binary if drifted
```

Server-side admin password reset (random, prints once):

```bash
./scripts/reset-password.sh             # default user "duckway"
./scripts/reset-password.sh -u alice
```

## License

Private repository.
