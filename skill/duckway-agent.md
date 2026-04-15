# Duckway Agent Skill

You are working behind a **Duckway API proxy**. All API calls are routed through the proxy which manages real API keys on your behalf. You never see or need real keys.

## Setup (already done for you)

Your environment has been configured with:
- `HTTPS_PROXY` and `HTTP_PROXY` pointing to the local Duckway proxy
- The Duckway CA certificate installed in the system trust store
- Placeholder API keys in `~/.duckway/keys.env`

## How to Make API Calls

### Method 1: HTTPS Proxy (recommended, transparent)

Just call APIs normally. The proxy intercepts HTTPS traffic automatically:

```bash
# These work transparently — the proxy handles authentication
curl https://api.openai.com/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}'

curl https://api.github.com/user

curl https://api.anthropic.com/v1/messages \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}'
```

The proxy:
1. Intercepts the HTTPS connection
2. Finds your placeholder key for this service
3. Replaces it with the real API key
4. Forwards to the upstream API
5. Returns the response to you

### Method 2: Direct Proxy URL (lightweight, no HTTPS proxy needed)

For environments without the proxy running, call the Duckway server directly:

```bash
# POST http://<duckway-server>/proxy/<service>/<path>
curl http://duckway-server:9090/proxy/openai/v1/chat/completions \
  -H "X-Duckway-Token: <client-token>" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}'
```

This mode is useful for:
- Using Duckway as an OpenAI-compatible API endpoint
- Environments where you can't install a CA certificate
- Simple scripts that only need one service

## Available Environment Variables

```bash
eval $(duckway env)
# Exports: OPENAI_API_KEY, ANTHROPIC_API_KEY, GITHUB_TOKEN, etc.
```

These are placeholder keys (contain `dw_` marker). They work through the proxy but are not real API keys.

## Commands

```bash
duckway status    # Check connection, keys, heartbeat
duckway sync      # Refresh keys and canary tokens
duckway env       # Print keys as shell exports
duckway proxy     # Start HTTPS proxy (handles CONNECT tunnels)
```

## Approval Flow

Some keys require admin approval on first use. If you get a `403` with `duckway_approval_pending`, wait and retry — the admin will be notified.

## Permissions

Your placeholder key may have restricted permissions. If you get a `permission denied` error, the admin has limited which endpoints or models you can use.
