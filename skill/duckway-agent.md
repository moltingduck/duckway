# Duckway Agent Skill

You are working behind a **Duckway API proxy**. All API calls should be routed through the Duckway proxy instead of directly to upstream APIs.

## How It Works

Your environment has placeholder API keys that look like real keys but contain `dw_` as a marker. When you make API calls through the Duckway proxy, it replaces your placeholder key with the real API key.

## Making API Calls

Instead of calling APIs directly, route through the Duckway proxy:

```
# Instead of:
POST https://api.openai.com/v1/chat/completions

# Use:
POST http://<duckway-server>/proxy/openai/v1/chat/completions
```

Include your client token in every request:
```
X-Duckway-Token: <your-client-token>
```

The proxy handles authentication with the upstream API automatically.

## Available Environment Variables

Your placeholder keys are in `~/.duckway/keys.env`. Load them with:
```bash
eval $(duckway env)
```

Each key follows the format:
```
OPENAI_API_KEY=sk-proj-dw_abc123...
ANTHROPIC_API_KEY=sk-ant-dw_def456...
GITHUB_TOKEN=ghp_dw_789abc...
```

## Client Commands

```bash
duckway status    # Check connection and available keys
duckway sync      # Refresh placeholder keys from server
duckway env       # Print keys as shell exports
duckway proxy     # Start local proxy on port 18080
```

## Using the Local Proxy

If `duckway proxy` is running, set your proxy environment:
```bash
source ~/.duckway/proxy-env.sh
```

Then all HTTP requests route through the Duckway proxy automatically.

## Approval Flow

Some keys require admin approval on first use. If you get a `403` response with `duckway_approval_pending`, wait for the admin to approve your request, then retry.

## Permissions

Your placeholder key may have restricted permissions (e.g., only certain endpoints or models). If you get a `403` with a permission error, check with the admin about your access level.
