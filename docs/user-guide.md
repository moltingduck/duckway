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
- [Managing remote agent sessions with Ducklord](#managing-remote-agent-sessions-with-ducklord)
- [Control Channels (Discord-as-comms)](#control-channels-discord-as-comms)
- [Common tasks](#common-tasks)
- [Troubleshooting](#troubleshooting)

---

## What is Duckway?

Duckway is an API key proxy. Real API keys live encrypted on the Duckway server. Agents (Claude Code, scripts, CI runners) only ever see **phantom tokens** — strings that look identical to real keys (`sk-...`, GitHub `github_pat_...` / `ghp_...` / `gho_...` / `ghu_...` / `ghs_...` / `ghr_...`) but are useless to the upstream API. The Duckway proxy swaps phantom → real on its way to the upstream and strips the real key from any response back to the agent.

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
./scripts/dev.sh up        # combined mode, http://127.0.0.1:9090/admin/
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

By default this installs to `/usr/local/bin/duckway` and uses `sudo` when that
directory is not writable. When run from an interactive terminal, the installer
asks where to install:

- `System-wide` → `/usr/local/bin/duckway`
- `User-local` → `~/.local/bin/duckway`
- `Custom path`

To install without sudo, choose `User-local`, then run:

```bash
~/.local/bin/duckway init
```

For non-interactive automation, you can still pin a mode or path:

```bash
curl -fsSL http://your-duckway-host/install.sh | DUCKWAY_INSTALL=user sh
curl -fsSL http://your-duckway-host/install.sh | DUCKWAY_INSTALL_PATH="$HOME/bin/duckway" sh
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

Client tokens do not expire automatically. If a token is lost, open the client in the admin panel and select **Rotate Token**. Duckway replaces only the registered token hash: the client ID, assignments, phantom tokens, usage history, and settings remain unchanged. The old token stops authenticating new requests immediately. The replacement token is shown once together with the same setup commands used during registration; run one to reconfigure the existing Duckway client.

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

For Grok Build, bind an xAI key to the client with service `xai` and env name `XAI_API_KEY`. `duckway sync` writes that phantom token into `~/.grok/config.toml` under `[model."grok-4.5"]`, preserving the rest of the Grok config. Start `duckway proxy -d` before running `grok` so calls to `cli-chat-proxy.grok.com` are swapped server-side.

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

## Managing remote agent sessions with Ducklord

Ducklord/Ducklion is the SSH-based remote session controller for developers who
want a local TUI that can manage shells or agent processes on remote hosts. It
is independent from the normal Duckway client daemon.

See [Ducklord / Ducklion Remote Agent Control](ducklord-ducklion-spec.md) for:

- a terminal-only Podman walkthrough
- TUI controls, including `Enter`, right-click, `Ctrl-]`, and `n` new session
- remote session creation examples
- the SSH, PTY, attach stream, and session creation technical details

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

### Delete a refreshable token

Use **Refreshable Tokens** → **Delete**. Duckway shows every affected Key Suite, client phantom token, and Control Channel before it deletes anything.

When you confirm, Duckway removes Key Suite entries that pointed at the token and unassigns the affected client phantom tokens. Control Channels are not deleted; they are disabled and marked in red until you edit the Control Channel and assign a replacement bot token.

Before uploading, click **Test** in the upload modal to validate the pasted token format without storing it or calling the provider refresh endpoint. After upload, use the **Refresh** button in the token list or **Refresh Now** in token details to force an immediate refresh.

### Upload a Codex OAuth token

Codex has two different credential modes in Duckway:

- **OpenAI Platform API key** — add this in **API Keys**. Duckway exposes it as `OPENAI_API_KEY` and routes Codex through the local Duckway proxy. This key must allow the Responses API, including `api.responses.write`.
- **Codex OAuth token** — add this in **Refreshable Tokens**. Duckway writes a fake `~/.codex/auth.json` on clients, intercepts Codex refresh calls to `auth.openai.com`, and swaps fake OAuth tokens for real OAuth tokens only inside the gateway. Codex API calls still use the `duckway-openai` provider and `OPENAI_API_KEY` placeholder through the local proxy; when the assigned OpenAI key is OAuth/JWT-shaped, Duckway issues a JWT-shaped phantom so Codex sees an OAuth-style token locally.

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

The upload page derives **Expires At** from the `exp` claim inside `tokens.access_token` and marks the credential as `codex_oauth`. Set **Token Endpoint** to `https://auth.openai.com/oauth/token` unless your Codex deployment uses a custom OAuth server. Clients never receive the uploaded real `access_token`, `refresh_token`, or `id_token`; `duckway sync` generates fake JWT-shaped values for `~/.codex/auth.json` and, when assigned as `OPENAI_API_KEY`, a JWT-shaped phantom in `~/.duckway/keys.env`.

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
export HTTPS_PROXY=http://127.0.0.1:18080
export HTTP_PROXY=http://127.0.0.1:18080
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

A **Control Channel (CC)** binds **one client to one Discord category** via a bot. Inside that category every text channel maps 1:1 to an agent session — every message a human types triggers Claude Code or Codex on the agent's machine and the result is posted back. A separate management channel accepts `!new` / `!end` / `!destroy` / `!yield` / `!list` / `!status` / `!duckway-version` / `!duckway-doctor` / `!duckway-restart` / `!duckway-update` / `!help` text commands.

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
- *Bot Permissions* for the normal **Invite bot** URL:
  - **Manage Channels** (create / archive / move task channels)
  - **View Channel**
  - **Send Messages**
  - **Add Reactions** (for `discord_request_approval` votes)
  - **Read Message History** (so the agent can read the channel above the latest message)

Duckway's **Setup invite** URL adds **Manage Roles**. Use it only when you want Duckway to write the bot's category permission override automatically.

Copy the generated URL at the bottom, paste into a browser, pick the guild you want the bot in, and click **Authorize**. In normal use, Duckway generates both URLs for you from the saved bot token.

**4. Per-category permission lock-down (recommended)**

The normal invite grants `Manage Channels` guild-wide so Duckway can create categories and task channels. The setup invite also grants `Manage Roles` so Duckway can write a category-level permission override. In Duckway, use **Grant bot access** after selecting an existing category; newly created categories attempt this automatically.

To narrow the bot after setup:

- In your guild: right-click the bot's role → **Edit Role** → uncheck **Manage Channels** and, if you used setup invite, **Manage Roles** in the global permissions.
- In Duckway, select the target category and click **Grant bot access**. If Discord rejects it, right-click the **target category** → **Edit Category** → **Permissions** tab → **+ Add member or role** → pick the bot → grant **Manage Channels** + **View Channel** + **Send Messages** + **Add Reactions** + **Read Message History** ONLY here.

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
   - **Agent type** — `claude_code`, `codex`, or `openclaw`. Claude/Codex use Duckway's PTY runner by default. Start `duckway cc watch --tmux` only when you explicitly want the legacy tmux runner. OpenClaw runs through `openclaw agent --message-file --json` and uses `DUCKWAY_CC_OPENCLAW_AGENT` for the OpenClaw agent id.
   - **Codex sandbox** — shown only for `codex`. Allowed values are `workspace-write` (default), `read-only`, `danger-full-access`, and `none`. `danger-full-access` lets Codex use local files and commands without filesystem sandboxing, so only use it for trusted Discord categories and clients.
   - **Service** — `discord`.
   - **Bot Token** — the API key you uploaded in (1).
   - **Discord setup** — click **Load Discord setup**. If the bot is not in the server yet, click **Invite bot** to open Duckway's normal Discord OAuth URL, select the server in Discord, then refresh. Use **Setup invite** only if **Grant bot access** fails because the bot lacks permission to write the category override. Pick an existing category and click **Grant bot access**, or create a new category from the form and Duckway will grant access automatically. Then click **Check permissions** to verify create/send/react/read/delete access before saving.

   On save, duckway calls Discord to create `<client>-control` under the category and issues a phantom bot token bound to the client. If anything fails (bot lacks permission, wrong category id, etc.) the create rolls back and tells you which step failed.

3. Click into the CC → **Test (create + delete channel)** to verify Discord connectivity. Use **Test agent (hi)** to publish a synthetic `hi` message to the management channel; if `duckway cc watch` is connected on the client, it should start the selected agent and post the result back.

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

The daemon needs the selected agent binary (`claude`, `codex`, or `openclaw`) in `$PATH`. For Codex, `duckway sync` always writes the `duckway-openai` provider in `~/.codex/config.toml` and uses `OPENAI_API_KEY` from `~/.duckway/keys.env` through the local proxy. If the assigned OpenAI key is a Codex OAuth refreshable key, Duckway writes OAuth-shaped phantom tokens locally and intercepts `auth.openai.com/oauth/token` refreshes so only the gateway sees the real refresh token. If you use an OpenAI Platform API key instead, that key must allow OpenAI's Responses API, including `api.responses.write`. For OpenClaw, set `DUCKWAY_CC_OPENCLAW_AGENT=<agent-id>` on the client if you do not want the default `openclaw` agent id `default`; Duckway does not configure OpenClaw's own channel bindings. Per-channel `cwd` defaults to `~/.duckway/cc-workspace/<handle>/` (auto-created); override with `!new --cwd /path` from the management channel.

For agent launch debugging, run `duckway cc watch --debug` or `DUCKWAY_CC_DEBUG=1 duckway cc watch -d`. The log includes `agent_type`, `runner_mode`, sandbox/permission fields, and sanitized CLI argv; prompt text is summarized as first 5 characters + last 5 characters only.

### Inside an agent session, the model sees these MCP tools

`discord_get_my_cc`, `discord_list_channels`, `discord_create_task_channel`, `discord_archive_channel`, `discord_post`, `discord_post_file` (text + one local file/image attachment in one Discord message), `discord_edit_message`, `discord_delete_message`, `discord_read_recent`, `discord_wait_for_message`, `discord_request_approval` (reaction-vote — blocks until ✅/❌), `duckway_list_local_sessions`, `duckway_bind_session`.

### Binding a managed Ducklion session

`!sessions` in the management channel lists live, unbound Ducklion agent PTYs.
Run `!bind <six-character-session-id>` to create a one-to-one Discord task
channel without changing its current writer. Use `!yield` in that task channel
to request Discord ownership when the adapter reports the session idle.

### Management channel commands

In `<client>-control`:
- `!new <slug> [--cwd <path>|--project <name|number>] [--topic "…"]` → create a task channel + register a session for it
- `!new-confirm <token>` → confirm creation of a missing `--cwd` folder; Duckway creates it on the agent machine, saves it as a project, then opens the task channel
- `!list` → table of task channels + which have running sessions
- `!status` → daemon up? agent type? counts?
- `!sessions` → list live Ducklion agent PTYs that aren't bound to a Discord channel
- `!bind <session_id> [<session_id> …]` → create one task channel for each live Ducklion session without changing ownership
- `!yield [-w|--wait]` → in a bound task channel, request Discord ownership now or after the active task completes
- `!projects [<filter>]` → list saved project folders from the agent machine
- `!duckway-version` → show the local Duckway version on the client
- `!duckway-restart` → restart local Duckway daemons on the client
- `!duckway-update [--restart]` → update the local Duckway binary; optionally restart daemons after a successful update
- `!! <command>` → run a shell command directly on the client in the current channel's working directory; stdout/stderr are posted back and the agent session is not touched

Agent prompts, direct `!!` shell commands, and daemon-side `!` commands use three independent bounded queues. Each queue remains FIFO for a channel, but work in different queues can run concurrently, so a long agent turn does not delay operational commands and replies may interleave.

Project folders are saved on the client machine, not browsed from the Duckway server. Add them with:

```bash
duckway projects add ~/duckway
duckway projects add ./api-server
duckway projects add ~/projects/*        # glob expands now; matching files are skipped
duckway projects add --name api ~/work/backend
duckway projects list
duckway projects remove api
duckway projects clear               # clears saved project registry only; folders are not deleted
```

Relative paths are resolved from the directory where you run `duckway projects add`; `~/...` is expanded to your home directory. Globs are expanded only when you add projects, then stored as concrete directories in `~/.duckway/cc-projects.json`. From Discord:

```text
!projects duck
!new fix-login --project duckway
!new fix-login --project 1
!new spike --cwd ~/work/new-spike     # asks for !new-confirm if the folder does not exist
```
- `!help`

`!sessions` / `!bind` run on the agent (the daemon owns the filesystem), so the cc-watch daemon must be up. The server posts an "offline" error if it isn't.

### Attaching from the CLI

`duckway cc bind` on the agent box does the same thing without going through Discord. With no args it prints a numbered table and reads selections from stdin — accept `1,3,5`, `1-3`, or `all`, and an empty line cancels. Each selection becomes its own task channel. For scripts: `duckway cc bind --session <id> [--session <id> …] [--cwd <substr>]`.

In any task channel:
- `!end` → end the current agent session and **archive** the Discord channel (history kept, channel renamed and removed from the category)
- `!destroy` → end the current agent session and **hard-delete** the Discord channel (history gone — useful for one-shot experiments)

The management channel itself also accepts plain text — the message is forwarded to the agent with a system note nudging it to spawn a dedicated task channel via `discord_create_task_channel` for any sustained work, instead of holding a long conversation inline.

For ordinary task messages, the client keeps status quiet with reactions: `🦆` means the client received the message, `⏳` means the agent is still running, `✅` means the turn completed, and `⚠️` means the turn failed or was dropped. Agent replies are Discord replies to the triggering message so back-to-back prompts stay distinguishable.

Codex PTY turns do not fail just because they run longer than five minutes. Long-running prompts such as `/goal` keep the task channel busy and complete when Codex writes its final event. The legacy tmux runner remains available with `duckway cc watch --tmux`.

When `duckway cc watch --tmux` is used, existing legacy tmux sessions named `duckway-<handle>` are automatically renamed to the current `<handle>-duckway` convention the next time `cc watch` uses that channel, unless both old and new sessions already exist.

### Local terminal agent sessions

Duckway can also manage local PTY-backed terminal agent sessions that are independent from Discord CC sessions:

```bash
duckway session start --name review --agent codex --cwd /repo -- codex exec
duckway session list
duckway session send review "review the current diff"
duckway session read review --lines 120
duckway session attach review
duckway session stop review
```

Use `duckway session start --tmux ...` only when you explicitly want the legacy tmux backend.

These sessions are local-only in the first version. They are not exposed through the server, MCP tools, or Discord commands. Metadata is stored in `~/.duckway/agent-sessions.json`; old clients that do not have this file load an empty session list, and existing CC files such as `cc-sessions.json` and `cc-watch/` are left untouched.

### Security boundary

- The **bot token** is the only real boundary. Two CCs sharing a bot can reach each other's channels — use **different bots** to isolate teams.
- The agent never sees `channel_id`, `guild_id`, or `category_id` — only opaque `dwch_…` handles.
- A client can only operate within its own CC (HTTP 403 otherwise) AND any handle in a path is checked to belong to that CC.
- For `claude_code`, the daemon spawns claude with `--dangerously-skip-permissions`. For `codex`, the daemon uses the CC's controlled sandbox enum: new sessions pass `--sandbox <value>`, and resumed sessions pass the equivalent `sandbox_mode` config override because `codex exec resume` does not accept `--sandbox`; choosing `none` passes neither. When tmux is installed, the command runs inside `<handle>-duckway` and leaves the pane open for inspection. For `openclaw`, the daemon runs `openclaw agent --agent <id> --session-key duckway:<handle> --message-file <file> --json`; it does not use OpenClaw's own Discord/channel integration. The server and client both validate agent options, and the client never accepts arbitrary CLI arguments from the server. Anyone in the Discord category can make the selected agent act. Trust the channel.

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
export GITHUB_TOKEN=github_pat_...
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
TS_AUTHKEY_ADMIN=hskey-auth-...                    # short-lived key tagged for admin
TS_AUTHKEY_GATEWAY=hskey-auth-...                  # short-lived key tagged for gateway
TS_EXTRA_ARGS=--login-server=https://hs.example.com
TS_HOSTNAME=duckway
```

Then `./scripts/prod.sh up`. Headscale doesn't issue Tailscale HTTPS certificates by default, so Duckway uses unprivileged userspace Tailscale to forward tailnet TCP port 80 directly to each app's loopback listener. This deliberately avoids certificate-domain and HTTP Host matching. The profile publishes no host ports and needs no TUN device or added Linux capability. Use separately tagged admin and gateway pre-auth keys plus a deny-by-default Headscale policy to control who can reach each node; hostnames are not security identities. Access them as `http://${TS_HOSTNAME}-admin/admin/` and `http://${TS_HOSTNAME}-gw/`.

### Embedded DERP reconnects every minute

Repeated `derp.Recv: EOF` messages at almost exactly 60-second intervals typically mean that the reverse proxy in front of Headscale is closing the upgraded DERP connection at its idle/read timeout. For Nginx, preserve the upgrade headers, disable buffering, and set long read/send timeouts on the Headscale location:

```nginx
# Place this map in the http context, outside server/location blocks.
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

location / {
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $connection_upgrade;
    proxy_set_header Host $host;
    proxy_buffering off;
    proxy_read_timeout 24h;
    proxy_send_timeout 24h;
    proxy_pass http://headscale;
}
```

The embedded DERP server also requires public `tcp/443` and STUN on `udp/3478`; do not send UDP through an HTTP reverse proxy. Cloudflare Proxy and Cloudflare Tunnel are not compatible with Headscale's custom POST-based protocol.

After changing the Headscale edge, verify from each Duckway Tailscale sidecar:

```bash
docker exec duckway-tailscale-admin tailscale netcheck
docker exec duckway-tailscale-admin tailscale debug derp headscale
docker exec duckway-tailscale-gateway tailscale netcheck
docker exec duckway-tailscale-gateway tailscale debug derp headscale
```

The DERP debug commands should remain connected without a new EOF every minute. Use `tailscale ping <peer>` to confirm whether normal traffic is direct or relayed.

---

## Troubleshooting

### Quick client diagnosis

Run doctor on the agent machine to see what the current client supports and
what is missing:

```bash
duckway doctor
```

The report checks the current client's config, server token, update status,
proxy daemon, local proxy port, CC assignment/sync state, `cc watch`, companion
`ducklion`, supported agent binaries, saved projects, CA certificate, and proxy
port.

From a CC management channel, run the same current-client diagnosis remotely:

```text
!duckway-doctor
```

### "SSL certificate verification failed" from Claude Code or Python/httpx tools

Duckway's local proxy signs intercepted HTTPS with the client CA at `~/.duckway/ca.pem`. `duckway cc watch` automatically prepares `~/.duckway/agent-ca-bundle.pem` and passes these trust settings to launched agents. If you run a tool manually, set the CA environment yourself:

```bash
export NODE_EXTRA_CA_CERTS="$HOME/.duckway/agent-ca-bundle.pem"
export SSL_CERT_FILE="$HOME/.duckway/agent-ca-bundle.pem"
export REQUESTS_CA_BUNDLE="$HOME/.duckway/agent-ca-bundle.pem"
export CURL_CA_BUNDLE="$HOME/.duckway/agent-ca-bundle.pem"
```

Restart `duckway cc watch` after `duckway init` so the bundle exists. The Docker examples (`examples/docker-compose.claude.yml`) wire the CA up automatically via a shared volume.

### "could not install CA to system trust store: could not find system CA directory"

The Duckway client tries `update-ca-certificates` (Debian/Ubuntu/Alpine), `update-ca-trust` (RHEL/Fedora), and `trust extract-compat` (Arch). If none of these binaries are in PATH, install the `ca-certificates` package on your distro and re-run `duckway init`.

### Quick Install command on the clients page is broken

If you're in **split mode**, the gateway URL setting is required. Go to **Settings → Gateway URL** and set it to your gateway's URL (e.g. `http://duckway-gw`). The clients page now reads from this setting; without it, the curl command would point at the admin host, which doesn't serve `/install.sh`.

### Tailscale containers say "Logged out"

Auth key expired or one-time-use already redeemed. Generate a new **reusable** key:
- Tailscale: https://login.tailscale.com/admin/settings/keys → check "Reusable"
- Headscale: `headscale preauthkeys create --user <name> --reusable`

Then update `TS_AUTHKEY` in `.prod.env` and run `./scripts/prod.sh restart`.

### Gateway crashes in `_walIndexAppend` with SIGBUS

Linux ARM64 deployments using modernc SQLite WAL mode can crash while updating
the WAL shared-memory index. Duckway defaults to the safer rollback journal:

```env
DUCKWAY_SQLITE_JOURNAL_MODE=DELETE
```

When upgrading an existing split deployment, stop every Duckway process before
the first restart so one process can checkpoint the old WAL and change modes:

```bash
docker compose down
docker run --rm -v duckway_duckway-data:/data -v "$PWD":/backup alpine \
  sh -c 'cp -a /data/duckway.db* /backup/'
./scripts/prod.sh up
```

Adjust the Docker volume name if the Compose project name is not `duckway`.
Do not delete `duckway.db-wal` before making this consistent backup; committed
transactions may still be present there. After restart, verify:

```bash
docker exec duckway-admin sh -c 'ls -l /data/duckway.db*'
docker logs --since 5m duckway-admin
docker logs --since 5m duckway-gateway
```

`DUCKWAY_SQLITE_JOURNAL_MODE=WAL` remains available for a validated local
filesystem and driver, but is not recommended for ARM64 split deployments.

### Migrate a production install to PostgreSQL

Production Compose deployments can run PostgreSQL while retaining `/data` for
encryption keys, CA material, and an offline SQLite rollback copy. Start from
the normal SQLite setting in `.prod.env`:

```env
DUCKWAY_DATABASE=sqlite
DUCKWAY_POSTGRES_DB=duckway
DUCKWAY_POSTGRES_USER=duckway
DUCKWAY_POSTGRES_PASSWORD_FILE=./.secrets/postgres-password
```

Then run the complete offline cutover:

```bash
./scripts/prod.sh migrate-postgres
```

To discard request history during the migration while preserving all other
data, run:

```bash
DUCKWAY_MIGRATION_SKIP_REQUEST_LOGS=true ./scripts/prod.sh migrate-postgres
```

This excludes only `request_log` and `request_log_detail` from PostgreSQL. The
offline SQLite backup still contains the original request history.

The command builds the migrator, stops all known Duckway writers, archives the
entire `/data` volume under `backups/`, starts a private PostgreSQL 17 container,
verifies the archive and writes its SHA-256 checksum, imports every table in one
transaction, verifies row counts and canonical content hashes, updates
`DUCKWAY_DATABASE=postgres`, and restarts the active split or combined profile.
Legacy rows whose nullable foreign key points to deleted data are preserved with
that relationship cleared. Rows with an invalid required relationship are
excluded from the migration snapshot with an explicit warning, while the
offline SQLite backup remains unchanged. Invalid UTF-8 found in legacy SQLite
TEXT values is replaced with the standard Unicode replacement character and
reported before PostgreSQL verification.
PostgreSQL does not publish port 5432. Its password is generated with OpenSSL
when missing and stored in the git-ignored `.secrets/` directory with mode 600.

Keep the printed SQLite backup. A rollback to it discards writes made after the
PostgreSQL cutover; Duckway does not perform reverse replication. Normal
commands remain unchanged after migration:

```bash
./scripts/prod.sh restart
./scripts/prod.sh status
./scripts/prod.sh logs
```

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
