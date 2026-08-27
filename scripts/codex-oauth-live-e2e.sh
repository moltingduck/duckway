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
#   6. run `codex exec` in native OAuth mode with the prompt `hello?`
#      through Duckway's proxy
#
# Required:
#   PODMAN        podman executable (default: podman)
#   CODEX_AUTH    path to the live Codex auth JSON
#
# Usage:
#   CODEX_AUTH=live-credentials/codex-auth.json ./scripts/codex-oauth-live-e2e.sh --check-token
#   CODEX_AUTH=live-credentials/codex-auth.json ./scripts/codex-oauth-live-e2e.sh
#   CODEX_AUTH=live-credentials/codex-auth.json ./scripts/codex-oauth-live-e2e.sh --refresh-only
#   CODEX_AUTH=live-credentials/codex-auth.json ./scripts/codex-oauth-live-e2e.sh --llm-only
#   CODEX_AUTH=live-credentials/codex-auth.json ./scripts/codex-oauth-live-e2e.sh --wss-only
#   CODEX_AUTH=live-credentials/codex-auth.json ./scripts/codex-oauth-live-e2e.sh --cc-watch

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
PODMAN="${PODMAN:-podman}"
CODEX_AUTH="${CODEX_AUTH:-$REPO_ROOT/live-credentials/codex-auth.json}"
IMAGE="${DUCKWAY_LIVE_IMAGE:-docker.io/library/golang:1.25-alpine}"
PROMPT="${DUCKWAY_CODEX_PROMPT:-hello?}"
CHECK_TOKEN=0
CC_WATCH=0
RUN_REFRESH=1
RUN_LLM=1
RUN_WSS=1

while [ "$#" -gt 0 ]; do
  case "$1" in
    --check-token) CHECK_TOKEN=1 ;;
    --cc-watch) CC_WATCH=1 ;;
    --refresh-only) RUN_REFRESH=1; RUN_LLM=0; RUN_WSS=0 ;;
    --llm-only) RUN_REFRESH=0; RUN_LLM=1; RUN_WSS=0 ;;
    --wss-only) RUN_REFRESH=0; RUN_LLM=1; RUN_WSS=1 ;;
    *)
      echo "Unknown argument: $1" >&2
      exit 1
      ;;
  esac
  shift
done

if [ ! -r "$CODEX_AUTH" ]; then
  echo "Missing readable CODEX_AUTH file: $CODEX_AUTH" >&2
  echo "Create one with: install -m 600 ~/.codex/auth.json live-credentials/codex-auth.json" >&2
  exit 1
fi
CODEX_AUTH="$(cd "$(dirname "$CODEX_AUTH")" && pwd)/$(basename "$CODEX_AUTH")"

mode="$(stat -c '%a' "$CODEX_AUTH" 2>/dev/null || stat -f '%Lp' "$CODEX_AUTH" 2>/dev/null || true)"
if [ "$mode" != "600" ]; then
  echo "Refusing to use $CODEX_AUTH because permissions are $mode; run: chmod 600 '$CODEX_AUTH'" >&2
  exit 1
fi

if [ "$CHECK_TOKEN" = "1" ]; then
  "$PODMAN" run --rm -i \
    -v "$CODEX_AUTH":/run/secrets/codex-auth.json:ro \
    docker.io/library/alpine:3.21 \
    sh -s <<'CHECK_TOKEN'
set -eu
apk add --no-cache python3 >/dev/null
python3 - <<'PY'
import base64, json, sys, time

required = set()
with open("/run/secrets/codex-auth.json", "r", encoding="utf-8") as f:
    auth = json.load(f)
tokens = auth.get("tokens") if isinstance(auth.get("tokens"), dict) else auth
for key in ("access_token", "refresh_token", "id_token"):
    print(f"{key}: {'present' if tokens.get(key) or auth.get(key) else 'missing'}")
access = tokens.get("access_token") or auth.get("access_token")
if not access or len(access.split(".")) != 3:
    raise SystemExit("access_token is missing or not JWT-shaped")
payload = access.split(".")[1]
payload += "=" * (-len(payload) % 4)
claims = json.loads(base64.urlsafe_b64decode(payload.encode()))
raw_scopes = claims.get("scp", claims.get("scope", []))
if isinstance(raw_scopes, str):
    scopes = set(raw_scopes.split())
elif isinstance(raw_scopes, list):
    scopes = {str(s) for s in raw_scopes}
else:
    scopes = set()
missing = sorted(required - scopes)
print("issuer:", claims.get("iss", ""))
print("audience:", claims.get("aud", ""))
print("scopes:", ", ".join(sorted(scopes)) if scopes else "(none)")
print("missing:", ", ".join(missing) if missing else "(none)")
if claims.get("exp"):
    print("expired:", "true" if int(claims["exp"]) <= int(time.time()) else "false")
PY
CHECK_TOKEN
  exit $?
fi

cat <<'EOF'
============================================
  Codex OAuth phantom live E2E
  - podman isolated HOME
  - real token file mounted read-only
  - prompt: hello?
  - cc watch: optional with --cc-watch
============================================
EOF

"$PODMAN" run --rm -i \
  -v "$REPO_ROOT":/workspace:ro \
  -v "$CODEX_AUTH":/run/secrets/codex-auth.json:ro \
  -w /workspace \
  "$IMAGE" \
  sh -s -- "$PROMPT" "$CC_WATCH" "$RUN_REFRESH" "$RUN_LLM" "$RUN_WSS" <<'CONTAINER_SCRIPT'
set -eu

PROMPT="$1"
CC_WATCH="$2"
RUN_REFRESH="$3"
RUN_LLM="$4"
RUN_WSS="$5"
export HOME=/tmp/duckway-home
export DUCKWAY_CONFIG_DIR="$HOME/.duckway"
export DUCKWAY_DEV=1
export DUCKWAY_DATA_DIR=/tmp/duckway-server-data
export DUCKWAY_LISTEN=127.0.0.1:19090
export DUCKWAY_CC_DISABLE_GATEWAY=1
BASE=http://127.0.0.1:19090
PROXY_PORT=18080
DISCORD_MOCK_PORT=19091
RUN_ID="codex-oauth-live-$(date +%s)"
COOKIE=/tmp/duckway-cookies
SERVER_LOG=/tmp/duckway-server.log
PROXY_LOG=/tmp/duckway-proxy.log
CC_WATCH_LOG=/tmp/duckway-cc-watch.log
DISCORD_MOCK_LOG=/tmp/duckway-discord-mock.log
CODEX_OUT=/tmp/codex-output.jsonl

cleanup() {
  if [ -n "${CC_WATCH_PID:-}" ]; then kill "$CC_WATCH_PID" >/dev/null 2>&1 || true; fi
  if [ -n "${PROXY_PID:-}" ]; then kill "$PROXY_PID" >/dev/null 2>&1 || true; fi
  if [ -n "${SERVER_PID:-}" ]; then kill "$SERVER_PID" >/dev/null 2>&1 || true; fi
  if [ -n "${DISCORD_MOCK_PID:-}" ]; then kill "$DISCORD_MOCK_PID" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

echo "Installing container test tools..."
apk add --no-cache bash curl jq nodejs npm ca-certificates python3 >/dev/null
npm install -g @openai/codex >/dev/null

echo "Building Duckway binaries..."
/usr/local/go/bin/go build -buildvcs=false -o /tmp/duckway-server ./cmd/server
/usr/local/go/bin/go build -buildvcs=false -o /tmp/duckway ./cmd/client

echo "Starting local Discord mock..."
python3 - "$DISCORD_MOCK_PORT" >"$DISCORD_MOCK_LOG" 2>&1 <<'PY' &
import http.server, json, sys, threading

port = int(sys.argv[1])
counter = [7000]
lock = threading.Lock()

class H(http.server.BaseHTTPRequestHandler):
    def _send(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        ln = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(ln) if ln else b"{}"
        try:
            body = json.loads(raw or b"{}")
        except Exception:
            body = {}
        if "/guilds/" in self.path and self.path.endswith("/channels"):
            with lock:
                counter[0] += 1
                cid = str(counter[0])
            return self._send(200, {
                "id": cid,
                "name": body.get("name", "duckway-control"),
                "type": 0,
                "parent_id": body.get("parent_id"),
                "guild_id": self.path.split("/")[3] if len(self.path.split("/")) > 3 else "",
            })
        if "/messages" in self.path:
            with lock:
                counter[0] += 1
                mid = str(counter[0])
            return self._send(200, {"id": mid, "channel_id": self.path.split("/")[-2]})
        return self._send(404, {"message": "mock: unknown POST " + self.path})

    def do_PATCH(self):
        ln = int(self.headers.get("Content-Length", "0"))
        if ln:
            self.rfile.read(ln)
        return self._send(200, {"id": self.path.split("/")[-1]})

    def do_DELETE(self):
        return self._send(200, {"id": self.path.split("/")[-1]})

    def do_GET(self):
        if self.path.endswith("/channels") or "/messages" in self.path:
            return self._send(200, [])
        return self._send(404, {"message": "mock: unknown GET " + self.path})

    def log_message(self, *args):
        pass

http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
PY
DISCORD_MOCK_PID=$!
export DUCKWAY_DISCORD_BASE_URL="http://127.0.0.1:$DISCORD_MOCK_PORT"

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
DISCORD_SERVICE_ID="$(curl -fsS -b "$COOKIE" "$BASE/api/services" | jq -r '.[] | select(.name=="discord") | .id')"
if [ "$CC_WATCH" = "1" ] && { [ -z "$DISCORD_SERVICE_ID" ] || [ "$DISCORD_SERVICE_ID" = "null" ]; }; then
  echo "Discord service not found" >&2
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
    print(
        "Codex OAuth token lacks OpenAI API-key provider scopes, which is OK for native OAuth mode: "
        + ", ".join(missing_scopes)
        + ". Present scopes: "
        + (", ".join(sorted(scopes)) if scopes else "(none)"),
        file=sys.stderr,
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

if [ "$CC_WATCH" = "1" ]; then
  echo "Creating Codex control channel for live cc watch..."
  BOT_KEY_ID="$(curl -fsS -b "$COOKIE" -X POST "$BASE/api/keys" \
    -H "Content-Type: application/json" \
    -d "{\"service_id\":\"$DISCORD_SERVICE_ID\",\"name\":\"$RUN_ID discord bot\",\"key\":\"NzLive.codex.testbot\"}" | jq -r '.id')"
  if [ -z "$BOT_KEY_ID" ] || [ "$BOT_KEY_ID" = "null" ]; then
    echo "Discord bot key creation failed" >&2
    exit 1
  fi
  CC_JSON="$(curl -fsS -b "$COOKIE" -X POST "$BASE/api/cc" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"$RUN_ID codex cc\",\"service_id\":\"$DISCORD_SERVICE_ID\",\"api_key_id\":\"$BOT_KEY_ID\",\"client_id\":\"$CLIENT_ID\",\"agent_type\":\"codex\",\"config\":{\"guild_id\":\"guild-live\",\"category_id\":\"cat-live\"},\"agent_options\":{\"sandbox\":\"read-only\"}}")"
  CC_ID="$(printf '%s' "$CC_JSON" | jq -r '.id')"
  CC_HANDLE="$(printf '%s' "$CC_JSON" | jq -r '.channels[0].handle')"
  if [ -z "$CC_ID" ] || [ "$CC_ID" = "null" ] || [ -z "$CC_HANDLE" ] || [ "$CC_HANDLE" = "null" ]; then
    echo "Codex CC creation failed: $CC_JSON" >&2
    exit 1
  fi
fi

echo "Downloading Duckway CA and syncing client state..."
curl -fsS -o "$DUCKWAY_CONFIG_DIR/ca.pem" "$BASE/skill/ca.pem"
curl -fsS -H "X-Duckway-Token: $CLIENT_TOKEN" -o "$DUCKWAY_CONFIG_DIR/ca-key.pem" "$BASE/client/ca-key"
chmod 600 "$DUCKWAY_CONFIG_DIR/ca-key.pem"
cp "$DUCKWAY_CONFIG_DIR/ca.pem" /usr/local/share/ca-certificates/duckway-ca.crt
update-ca-certificates >/dev/null
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
config_path = os.path.expanduser("~/.codex/config.toml")
if os.path.exists(config_path):
    with open(config_path, "r", encoding="utf-8") as f:
        config = f.read()
    if "duckway-openai" in config:
        raise SystemExit("Codex config still contains duckway-openai after OAuth sync")
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

REFRESH_FAILED=0
LLM_FAILED=0
WSS_FAILED=0

if [ "$RUN_REFRESH" = "1" ]; then
echo "[refresh] Refreshing Codex OAuth token once through Duckway proxy..."
REFRESH_FORM="$(python3 - <<'PY'
import json, os
from urllib.parse import urlencode

with open(os.path.expanduser("~/.codex/auth.json"), "r", encoding="utf-8") as f:
    auth = json.load(f)
tokens = auth.get("tokens") or {}
refresh = tokens.get("refresh_token", "")
client_id = tokens.get("client_id") or auth.get("client_id") or "app_EMoamEEZ73f0CkXaXp7hrann"
print(urlencode({
    "grant_type": "refresh_token",
    "refresh_token": refresh,
    "client_id": client_id,
}))
PY
)"
if ! printf '%s' "$REFRESH_FORM" | grep -q 'refresh_token='; then
  echo "Synced Codex auth.json has no fake refresh token" >&2
  exit 1
fi
set +e
REFRESH_STATUS="$(curl -sS --max-time 30 -o /tmp/codex-refresh-response.json -w '%{http_code}' \
  --cacert "$DUCKWAY_CONFIG_DIR/ca.pem" \
  --proxytunnel \
  -x "http://127.0.0.1:$PROXY_PORT" \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data "$REFRESH_FORM" \
  'https://auth.openai.com/oauth/token')"
CURL_REFRESH_EXIT=$?
set -e
if [ "$CURL_REFRESH_EXIT" -ne 0 ] || [ "$REFRESH_STATUS" != "200" ]; then
  echo "[refresh] FAIL: HTTP $REFRESH_STATUS curl_exit=$CURL_REFRESH_EXIT" >&2
  head -c 1000 /tmp/codex-refresh-response.json >&2 || true
  echo >&2
  REFRESH_FAILED=1
elif ! python3 - <<'PY'
import json
with open("/tmp/codex-refresh-response.json", "r", encoding="utf-8") as f:
    refreshed = json.load(f)
for key in ("access_token", "refresh_token"):
    value = refreshed.get(key, "")
    if not value:
        raise SystemExit(f"refresh response missing {key}")
    if "duckway" not in value and key == "refresh_token":
        raise SystemExit("refresh response leaked a non-phantom refresh token")
print("Refresh through Duckway proxy returned phantom tokens")
PY
then
  echo "[refresh] FAIL: invalid refresh response" >&2
  REFRESH_FAILED=1
else
  echo "[refresh] PASS"
fi
if ! grep -q 'openai-auth' "$PROXY_LOG"; then
  echo "[refresh] FAIL: proxy log has no openai-auth request" >&2
  REFRESH_FAILED=1
fi
fi

if [ "$RUN_LLM" = "1" ]; then
echo "Using Codex CLI native OAuth mode from duckway sync..."
unset OPENAI_API_KEY

echo "[llm] Running Codex prompt through Duckway proxy..."
mkdir -p /tmp/codex-work
set +e
printf '%s\n' "$PROMPT" | timeout 180 codex exec --json --skip-git-repo-check --sandbox read-only -C /tmp/codex-work - >"$CODEX_OUT" 2>/tmp/codex-stderr
CODEX_STATUS=$?
set -e
if [ "$CODEX_STATUS" -ne 0 ]; then
  echo "[llm] FAIL: codex exec exited with status $CODEX_STATUS" >&2
  tail -n 80 /tmp/codex-stderr >&2 || true
  tail -n 80 "$PROXY_LOG" >&2 || true
  tail -n 80 "$SERVER_LOG" >&2 || true
  LLM_FAILED=1
elif ! python3 - "$CODEX_OUT" <<'PY'
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
then
  echo "[llm] FAIL: codex exec returned no assistant text" >&2
  LLM_FAILED=1
fi
if ! grep -q 'openai-chatgpt' "$PROXY_LOG"; then
  echo "[llm] FAIL: proxy log has no native Codex ChatGPT backend traffic" >&2
  tail -n 120 "$PROXY_LOG" >&2 || true
  LLM_FAILED=1
fi
if [ "$LLM_FAILED" = "0" ]; then
  echo "[llm] PASS"
fi
if [ "$RUN_WSS" = "1" ]; then
  if ! grep -q '\[proxy openai-chatgpt\] WSS .* → 101' "$PROXY_LOG"; then
    echo "[wss] FAIL: proxy log has no successful Codex WebSocket bridge" >&2
    WSS_FAILED=1
  elif grep -q 'Falling back from WebSockets to HTTPS transport' /tmp/codex-stderr; then
    echo "[wss] FAIL: Codex fell back to HTTPS after the WebSocket attempt" >&2
    WSS_FAILED=1
  elif [ "$LLM_FAILED" = "1" ]; then
    echo "[wss] FAIL: Codex did not complete the request over WebSocket" >&2
    WSS_FAILED=1
  else
    echo "[wss] PASS"
  fi
fi
fi

if grep -Fq "$PLACEHOLDER" "$SERVER_LOG" "$PROXY_LOG" 2>/dev/null; then
  echo "Warning: phantom token appeared in logs; inspect proxy/server logs" >&2
fi

if [ "$CC_WATCH" = "1" ] && [ "$RUN_LLM" = "1" ] && [ "$LLM_FAILED" = "0" ]; then
  echo "Starting duckway cc watch and running Codex agent test..."
  /tmp/duckway cc watch --no-tmux --debug >"$CC_WATCH_LOG" 2>&1 &
  CC_WATCH_PID=$!
  for i in $(seq 1 60); do
    if grep -q "SSE connected" "$CC_WATCH_LOG"; then break; fi
    if ! kill -0 "$CC_WATCH_PID" >/dev/null 2>&1; then
      echo "duckway cc watch exited early" >&2
      tail -n 120 "$CC_WATCH_LOG" >&2 || true
      tail -n 80 "$SERVER_LOG" >&2 || true
      exit 1
    fi
    sleep 0.5
  done
  if ! grep -q "SSE connected" "$CC_WATCH_LOG"; then
    echo "duckway cc watch did not connect to SSE" >&2
    tail -n 120 "$CC_WATCH_LOG" >&2 || true
    exit 1
  fi

  TEST_JSON="$(curl -fsS -b "$COOKIE" -X POST "$BASE/api/cc/$CC_ID/test-agent")"
  TEST_ID="$(printf '%s' "$TEST_JSON" | jq -r '.test_id')"
  if [ -z "$TEST_ID" ] || [ "$TEST_ID" = "null" ]; then
    echo "test-agent did not return a test_id: $TEST_JSON" >&2
    exit 1
  fi

  for i in $(seq 1 180); do
    STATUS_JSON="$(curl -fsS -b "$COOKIE" "$BASE/api/cc/$CC_ID/test-agent/$TEST_ID")"
    STATUS="$(printf '%s' "$STATUS_JSON" | jq -r '.status')"
    ERR_TEXT="$(printf '%s' "$STATUS_JSON" | jq -r '.error // ""')"
    case "$STATUS" in
      replied)
        echo "duckway cc watch Codex agent replied for $TEST_ID"
        break
        ;;
      failed)
        echo "duckway cc watch Codex agent test failed: $ERR_TEXT" >&2
        tail -n 160 "$CC_WATCH_LOG" >&2 || true
        tail -n 120 "$PROXY_LOG" >&2 || true
        tail -n 100 "$SERVER_LOG" >&2 || true
        exit 1
        ;;
    esac
    sleep 1
  done
  if [ "$STATUS" != "replied" ]; then
    echo "duckway cc watch Codex agent test timed out; last status=$STATUS" >&2
    tail -n 160 "$CC_WATCH_LOG" >&2 || true
    tail -n 120 "$PROXY_LOG" >&2 || true
    tail -n 100 "$SERVER_LOG" >&2 || true
    exit 1
  fi
fi

if [ "$RUN_REFRESH" = "1" ] && [ "$REFRESH_FAILED" = "1" ]; then
  echo "FAIL: Codex OAuth refresh case" >&2
fi
if [ "$RUN_LLM" = "1" ] && [ "$LLM_FAILED" = "1" ]; then
  echo "FAIL: Codex LLM request case" >&2
fi
if [ "$RUN_WSS" = "1" ] && [ "$WSS_FAILED" = "1" ]; then
  echo "FAIL: Codex WebSocket case" >&2
fi
if [ "$RUN_LLM" = "1" ] && [ "$LLM_FAILED" = "0" ]; then
  echo "PASS: Codex OAuth phantom token ran prompt through Duckway proxy"
fi
if [ "$CC_WATCH" = "1" ] && [ "$LLM_FAILED" = "0" ]; then
  echo "PASS: duckway cc watch ran Codex agent through Duckway proxy"
fi
if [ "$REFRESH_FAILED" = "1" ] || [ "$LLM_FAILED" = "1" ] || [ "$WSS_FAILED" = "1" ]; then
  exit 1
fi
CONTAINER_SCRIPT
