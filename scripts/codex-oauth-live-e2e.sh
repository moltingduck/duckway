#!/usr/bin/env bash
# Live Codex OAuth phantom-token end-to-end test.
#
# This test uses a real ~/.codex/auth.json-style file, but runs entirely inside
# a throwaway podman container:
#   1. build duckway-server and duckway client from the current checkout
#   2. start a fresh Duckway server with DUCKWAY_DEV=1
#   3. upload the real Codex OAuth access/refresh/id tokens as a refreshable key
#   4. create a fresh client and an OPENAI_API_KEY JWT-shaped phantom
#   5. run duckway sync + duckway proxy inside an isolated HOME
#   6. run `codex exec` with the prompt `hello?` through Duckway's proxy
#
# Required:
#   PODMAN        podman executable (default: podman)
#   CODEX_AUTH    path to the live Codex auth JSON
#
# Usage:
#   CODEX_AUTH=secrets/codex-auth-live.json ./scripts/codex-oauth-live-e2e.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
PODMAN="${PODMAN:-podman}"
CODEX_AUTH="${CODEX_AUTH:-$REPO_ROOT/secrets/codex-auth-live.json}"
IMAGE="${DUCKWAY_LIVE_IMAGE:-docker.io/library/golang:1.25-alpine}"
PROMPT="${DUCKWAY_CODEX_PROMPT:-hello?}"

if [ ! -r "$CODEX_AUTH" ]; then
  echo "Missing readable CODEX_AUTH file: $CODEX_AUTH" >&2
  echo "Create one with: install -m 600 ~/.codex/auth.json secrets/codex-auth-live.json" >&2
  exit 1
fi
CODEX_AUTH="$(cd "$(dirname "$CODEX_AUTH")" && pwd)/$(basename "$CODEX_AUTH")"

mode="$(stat -c '%a' "$CODEX_AUTH" 2>/dev/null || stat -f '%Lp' "$CODEX_AUTH" 2>/dev/null || true)"
if [ "$mode" != "600" ]; then
  echo "Refusing to use $CODEX_AUTH because permissions are $mode; run: chmod 600 '$CODEX_AUTH'" >&2
  exit 1
fi

cat <<'EOF'
============================================
  Codex OAuth phantom live E2E
  - podman isolated HOME
  - real token file mounted read-only
  - prompt: hello?
============================================
EOF

"$PODMAN" run --rm -i \
  -v "$REPO_ROOT":/workspace:ro \
  -v "$CODEX_AUTH":/run/secrets/codex-auth.json:ro \
  -w /workspace \
  "$IMAGE" \
  sh -s -- "$PROMPT" <<'CONTAINER_SCRIPT'
set -eu

PROMPT="$1"
export HOME=/tmp/duckway-home
export DUCKWAY_CONFIG_DIR="$HOME/.duckway"
export DUCKWAY_DEV=1
export DUCKWAY_DATA_DIR=/tmp/duckway-server-data
export DUCKWAY_LISTEN=127.0.0.1:19090
BASE=http://127.0.0.1:19090
PROXY_PORT=18080
RUN_ID="codex-oauth-live-$(date +%s)"
COOKIE=/tmp/duckway-cookies
SERVER_LOG=/tmp/duckway-server.log
PROXY_LOG=/tmp/duckway-proxy.log
CODEX_OUT=/tmp/codex-output.jsonl

cleanup() {
  if [ -n "${PROXY_PID:-}" ]; then kill "$PROXY_PID" >/dev/null 2>&1 || true; fi
  if [ -n "${SERVER_PID:-}" ]; then kill "$SERVER_PID" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

echo "Installing container test tools..."
apk add --no-cache bash curl jq nodejs npm ca-certificates python3 >/dev/null
npm install -g @openai/codex >/dev/null

echo "Building Duckway binaries..."
/usr/local/go/bin/go build -buildvcs=false -o /tmp/duckway-server ./cmd/server
/usr/local/go/bin/go build -buildvcs=false -o /tmp/duckway ./cmd/client

echo "Starting fresh Duckway server..."
mkdir -p "$HOME" "$DUCKWAY_CONFIG_DIR" "$DUCKWAY_DATA_DIR"
/tmp/duckway-server --data "$DUCKWAY_DATA_DIR" --listen "$DUCKWAY_LISTEN" >"$SERVER_LOG" 2>&1 &
SERVER_PID=$!

for i in $(seq 1 60); do
  if curl -fsS "$BASE/version" >/dev/null 2>&1; then break; fi
  if ! kill -0 "$SERVER_PID" >/dev/null 2>&1; then
    echo "Duckway server exited early" >&2
    tail -n 80 "$SERVER_LOG" >&2 || true
    exit 1
  fi
  sleep 0.5
done
curl -fsS "$BASE/version" >/dev/null

echo "Logging in to admin API..."
curl -fsS -c "$COOKIE" -X POST "$BASE/api/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"username":"duckway","password":"duckway"}' >/dev/null

OPENAI_SERVICE_ID="$(curl -fsS -b "$COOKIE" "$BASE/api/services" | jq -r '.[] | select(.name=="openai") | .id')"
if [ -z "$OPENAI_SERVICE_ID" ] || [ "$OPENAI_SERVICE_ID" = "null" ]; then
  echo "OpenAI service not found" >&2
  exit 1
fi

echo "Preparing Codex OAuth upload payload..."
python3 - "$OPENAI_SERVICE_ID" "$RUN_ID" >/tmp/codex-oauth-upload.json <<'PY'
import base64, json, sys, time

service_id, run_id = sys.argv[1], sys.argv[2]
with open("/run/secrets/codex-auth.json", "r", encoding="utf-8") as f:
    auth = json.load(f)

tokens = auth.get("tokens") if isinstance(auth.get("tokens"), dict) else auth
access = tokens.get("access_token") or auth.get("access_token")
refresh = tokens.get("refresh_token") or auth.get("refresh_token")
id_token = tokens.get("id_token") or auth.get("id_token")
if not access or not refresh or not id_token:
    raise SystemExit("auth file must include access_token, refresh_token, and id_token")

def jwt_claims(token: str) -> dict:
    try:
        payload = token.split(".")[1]
        payload += "=" * (-len(payload) % 4)
        return json.loads(base64.urlsafe_b64decode(payload.encode()))
    except Exception:
        return {}

access_claims = jwt_claims(access)
raw_scopes = access_claims.get("scp", access_claims.get("scope", []))
if isinstance(raw_scopes, str):
    scopes = set(raw_scopes.split())
elif isinstance(raw_scopes, list):
    scopes = {str(s) for s in raw_scopes}
else:
    scopes = set()
required_scopes = {"api.model.read", "api.responses.write"}
missing_scopes = sorted(required_scopes - scopes)
if missing_scopes:
    raise SystemExit(
        "Codex OAuth token is missing required scopes for live E2E: "
        + ", ".join(missing_scopes)
        + ". Present scopes: "
        + (", ".join(sorted(scopes)) if scopes else "(none)")
    )

def jwt_exp_ms(token: str) -> int:
    try:
        claims = jwt_claims(token)
        exp = int(claims.get("exp") or 0)
        return exp * 1000 if exp else int((time.time() + 3600) * 1000)
    except Exception:
        return int((time.time() + 3600) * 1000)

subscription = {
    "credential_kind": "codex_oauth",
    "auth_mode": auth.get("auth_mode") or "chatgpt",
    "source": "codex",
    "id_token": id_token,
}
for key in ("account_id", "last_refresh"):
    value = tokens.get(key) or auth.get(key)
    if value:
        subscription[key] = value

payload = {
    "service_id": service_id,
    "name": f"{run_id} codex oauth",
    "access_token": access,
    "refresh_token": refresh,
    "expires_at": jwt_exp_ms(access),
    "token_endpoint": "https://auth.openai.com/oauth/token",
    "subscription_info": json.dumps(subscription, separators=(",", ":")),
}
json.dump(payload, sys.stdout)
PY

echo "Validating and uploading Codex OAuth refreshable key..."
curl -fsS -b "$COOKIE" -X POST "$BASE/api/oauth/validate" \
  -H "Content-Type: application/json" \
  --data-binary @/tmp/codex-oauth-upload.json >/dev/null
KEY_ID="$(curl -fsS -b "$COOKIE" -X POST "$BASE/api/oauth/upload" \
  -H "Content-Type: application/json" \
  --data-binary @/tmp/codex-oauth-upload.json | jq -r '.id')"
if [ -z "$KEY_ID" ] || [ "$KEY_ID" = "null" ]; then
  echo "OAuth upload did not return a key id" >&2
  exit 1
fi

echo "Creating isolated Duckway client and JWT phantom..."
CLIENT_JSON="$(curl -fsS -b "$COOKIE" -X POST "$BASE/api/clients" \
  -H "Content-Type: application/json" \
  -d "{\"name\":\"$RUN_ID\"}")"
CLIENT_ID="$(printf '%s' "$CLIENT_JSON" | jq -r '.id')"
CLIENT_TOKEN="$(printf '%s' "$CLIENT_JSON" | jq -r '.token')"
if [ -z "$CLIENT_ID" ] || [ "$CLIENT_ID" = "null" ] || [ -z "$CLIENT_TOKEN" ] || [ "$CLIENT_TOKEN" = "null" ]; then
  echo "Client creation failed" >&2
  exit 1
fi

PLACEHOLDER="$(curl -fsS -b "$COOKIE" -X POST "$BASE/api/placeholders" \
  -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$OPENAI_SERVICE_ID\",\"api_key_id\":\"$KEY_ID\",\"client_id\":\"$CLIENT_ID\",\"env_name\":\"OPENAI_API_KEY\",\"requires_approval\":false}" | jq -r '.placeholder')"
if [ "$(printf '%s' "$PLACEHOLDER" | awk -F. '{print NF}')" != "3" ]; then
  echo "Expected JWT-shaped phantom placeholder, got non-JWT shape" >&2
  exit 1
fi

cat >"$DUCKWAY_CONFIG_DIR/config.yaml" <<EOF
server_url: $BASE
client_name: $RUN_ID
token: $CLIENT_TOKEN
proxy_port: $PROXY_PORT
EOF
chmod 600 "$DUCKWAY_CONFIG_DIR/config.yaml"

echo "Downloading Duckway CA and syncing client state..."
curl -fsS -o "$DUCKWAY_CONFIG_DIR/ca.pem" "$BASE/skill/ca.pem"
curl -fsS -H "X-Duckway-Token: $CLIENT_TOKEN" -o "$DUCKWAY_CONFIG_DIR/ca-key.pem" "$BASE/client/ca-key"
chmod 600 "$DUCKWAY_CONFIG_DIR/ca-key.pem"
/tmp/duckway sync >/tmp/duckway-sync.log 2>&1

python3 - <<'PY'
import json, os, sys

with open("/run/secrets/codex-auth.json", "r", encoding="utf-8") as f:
    real = json.load(f)
real_tokens = real.get("tokens") if isinstance(real.get("tokens"), dict) else real
with open(os.path.expanduser("~/.codex/auth.json"), "r", encoding="utf-8") as f:
    fake = json.load(f)
fake_tokens = fake.get("tokens") or {}
for key in ("access_token", "refresh_token", "id_token"):
    if not fake_tokens.get(key):
        raise SystemExit(f"synced Codex auth.json missing {key}")
    if fake_tokens.get(key) == real_tokens.get(key):
        raise SystemExit(f"real {key} leaked into synced Codex auth.json")
with open(os.path.expanduser("~/.duckway/keys.env"), "r", encoding="utf-8") as f:
    keys_env = f.read()
if "OPENAI_API_KEY=" not in keys_env:
    raise SystemExit("OPENAI_API_KEY phantom missing from keys.env")
if real_tokens.get("access_token") and real_tokens["access_token"] in keys_env:
    raise SystemExit("real access_token leaked into keys.env")
PY

echo "Starting Duckway local proxy..."
/tmp/duckway proxy --debug >"$PROXY_LOG" 2>&1 &
PROXY_PID=$!
for i in $(seq 1 60); do
  if grep -q "Duckway proxy listening" "$PROXY_LOG"; then break; fi
  if ! kill -0 "$PROXY_PID" >/dev/null 2>&1; then
    echo "Duckway proxy exited early" >&2
    cat "$PROXY_LOG" >&2 || true
    exit 1
  fi
  sleep 0.5
done
grep -q "Duckway proxy listening" "$PROXY_LOG"

set -a
. "$DUCKWAY_CONFIG_DIR/keys.env"
set +a
export HTTP_PROXY="http://127.0.0.1:$PROXY_PORT"
export HTTPS_PROXY="http://127.0.0.1:$PROXY_PORT"
export NO_PROXY="localhost,127.0.0.1"
export NODE_EXTRA_CA_CERTS="$DUCKWAY_CONFIG_DIR/ca.pem"
export SSL_CERT_FILE="$DUCKWAY_CONFIG_DIR/ca.pem"

echo "Running Codex prompt through Duckway proxy..."
mkdir -p /tmp/codex-work
set +e
printf '%s\n' "$PROMPT" | timeout 180 codex exec --json --skip-git-repo-check --sandbox read-only -C /tmp/codex-work - >"$CODEX_OUT" 2>/tmp/codex-stderr
CODEX_STATUS=$?
set -e
if [ "$CODEX_STATUS" -ne 0 ]; then
  echo "codex exec failed with status $CODEX_STATUS" >&2
  tail -n 80 /tmp/codex-stderr >&2 || true
  tail -n 80 "$PROXY_LOG" >&2 || true
  tail -n 80 "$SERVER_LOG" >&2 || true
  exit 1
fi

python3 - "$CODEX_OUT" <<'PY'
import json, sys

path = sys.argv[1]
messages = []
with open(path, "r", encoding="utf-8") as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except json.JSONDecodeError:
            continue
        text = obj.get("text") or obj.get("message") or obj.get("output")
        item = obj.get("item") if isinstance(obj.get("item"), dict) else {}
        text = text or item.get("text")
        if text:
            messages.append(str(text))
if not messages:
    raise SystemExit("codex exec returned no assistant text")
print("Codex assistant output:")
print(messages[-1][:1000])
PY

if ! grep -Eq '/proxy/openai/v1/(responses|chat/completions)' "$PROXY_LOG"; then
  echo "Duckway proxy log did not show an OpenAI API request" >&2
  tail -n 120 "$PROXY_LOG" >&2 || true
  exit 1
fi

if grep -Fq "$PLACEHOLDER" "$SERVER_LOG" "$PROXY_LOG" 2>/dev/null; then
  echo "Warning: phantom token appeared in logs; inspect proxy/server logs" >&2
fi

echo "PASS: Codex OAuth phantom token ran prompt through Duckway proxy"
CONTAINER_SCRIPT
