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
- **104 E2E tests** + unit tests

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

```bash
# Configure
cp .env.example .prod.env
# Edit .prod.env:
#   TS_AUTHKEY=tskey-auth-xxxxx  (from Tailscale admin)
#   TS_HOSTNAME=duckway

# Start (split mode with Tailscale)
./scripts/prod.sh up

# Access via Tailscale HTTPS:
#   Admin:   https://duckway-admin.your-tailnet.ts.net
#   Gateway: https://duckway-gw.your-tailnet.ts.net
```

### Client Setup (on agent machines)

```bash
# One-liner install + register
curl -fsSL https://duckway-gw.tailnet/install.sh | sh
duckway init

# Or use the full setup command from admin panel → Clients → Register
```

## Architecture

```
┌─────────────────────┐                  ┌──────────────────────────────┐
│   Agent Machine      │                  │   Duckway (Docker)            │
│                      │                  │                               │
│  AI Agent            │  HTTPS_PROXY     │  ┌─── ts-admin ────────────┐ │
│  └→ duckway proxy    │─────────────────→│  │ Tailscale HTTPS → :9091 │ │
│     (MITM, CONNECT)  │                  │  │ Admin Panel + API       │ │
│                      │                  │  └─────────────────────────┘ │
│  ~/.duckway/         │  /proxy/{svc}    │  ┌─── ts-gateway ─────────┐ │
│  ├── config.yaml     │─────────────────→│  │ Tailscale HTTPS → :8080 │ │
│  ├── keys.env        │                  │  │ Proxy + Client API      │ │
│  ├── ca.pem          │                  │  └─────────────────────────┘ │
│  └── canary files    │                  │          │                   │
│                      │                  │  ┌───────▼──────────┐       │
└─────────────────────┘                  │  │ SQLite + AES-256  │       │
                                          │  └──────────────────┘       │
                                          └──────────────────────────────┘
```

### Split Mode (recommended)

| Service | Port | Access | Purpose |
|---------|------|--------|---------|
| `duckway-admin` | 9091 | Tailscale (admins only) | Web panel, management API |
| `duckway-gateway` | 8080 | Tailscale (agents) | Proxy, client API, downloads |

Agents **cannot** access the admin panel. Enforced by Tailscale ACL + separate Tailscale nodes.

## Scripts

| Script | Purpose |
|--------|---------|
| `./scripts/dev.sh up` | Build + start dev containers + seed test data |
| `./scripts/dev.sh split` | Dev in split mode (admin :9099, gateway :8080) |
| `./scripts/dev.sh nuke` | Wipe data + containers |
| `./scripts/prod.sh up` | Production start with Tailscale |
| `./scripts/prod.sh status` | Container + Tailscale node status |
| `./scripts/prod.sh logs` | Follow logs |
| `./scripts/prod.sh password` | Show admin password |
| `./scripts/e2e-test.sh` | Run 104 E2E tests |

## Environment Files

| File | Purpose | In git? |
|------|---------|---------|
| `.env.example` | Template | Yes |
| `.dev.env` | Dev secrets | No |
| `.prod.env` | Prod secrets (Tailscale auth key) | No |

## Docker Compose Files

| File | Mode | Tailscale | Ports |
|------|------|-----------|-------|
| `docker-compose.prod.yml` | Split | Yes (2 sidecars) | None (Tailscale only) |
| `docker-compose.combined.yml` | Combined | Yes (1 sidecar) | None (Tailscale only) |
| `docker-compose.yml` + `.dev.yml` | Dev | No | 127.0.0.1 only |

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
duckway init              # Register + download CA + sync
duckway sync              # Refresh keys + canary tokens
duckway env               # Print keys as shell exports
duckway proxy [--port N]  # Start HTTPS MITM proxy
duckway status            # Server, keys, heartbeat, proxy, CA
```

## License

Private repository.
