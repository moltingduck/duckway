#!/bin/bash
set -euo pipefail

# Duckway End-to-End Test Suite
# Tests the full flow: server → admin panel → client → proxy

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$PROJECT_DIR"

PORT="${1:-19090}"
BASE="http://127.0.0.1:$PORT"
DATA_DIR="/tmp/duckway-e2e-$$"
PASS=0
FAIL=0
ERRORS=""

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

cleanup() {
  fuser -k "$PORT/tcp" 2>/dev/null || true
  fuser -k "$((PORT + 100))/tcp" 2>/dev/null || true
  docker rm -f duckway-e2e-client 2>/dev/null || true
  rm -rf "$DATA_DIR" /tmp/dw-e2e-cookies /tmp/dw-e2e-server.log /tmp/dw-e2e-discord.log
}

assert_eq() {
  local desc="$1" expected="$2" actual="$3"
  if [ "$expected" = "$actual" ]; then
    echo -e "  ${GREEN}PASS${NC} $desc"
    PASS=$((PASS + 1))
  else
    echo -e "  ${RED}FAIL${NC} $desc (expected=$expected actual=$actual)"
    FAIL=$((FAIL + 1))
    ERRORS="$ERRORS\n  - $desc: expected=$expected got=$actual"
  fi
}

assert_contains() {
  local desc="$1" needle="$2" haystack="$3"
  if echo "$haystack" | grep -q "$needle"; then
    echo -e "  ${GREEN}PASS${NC} $desc"
    PASS=$((PASS + 1))
  else
    echo -e "  ${RED}FAIL${NC} $desc (missing: $needle)"
    FAIL=$((FAIL + 1))
    ERRORS="$ERRORS\n  - $desc: '$needle' not found"
  fi
}

assert_not_empty() {
  local desc="$1" val="$2"
  if [ -n "$val" ] && [ "$val" != "null" ]; then
    echo -e "  ${GREEN}PASS${NC} $desc"
    PASS=$((PASS + 1))
  else
    echo -e "  ${RED}FAIL${NC} $desc (empty or null)"
    FAIL=$((FAIL + 1))
    ERRORS="$ERRORS\n  - $desc: value was empty/null"
  fi
}

echo "============================================"
echo " Duckway E2E Test Suite"
echo " Port: $PORT | Data: $DATA_DIR"
echo "============================================"
echo ""

# --- Setup ---
echo -e "${YELLOW}[Setup]${NC} Building binaries..."
go build -o /tmp/duckway-e2e-server ./cmd/server/ 2>&1
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o client ./cmd/client/ 2>&1
echo -e "${YELLOW}[Setup]${NC} Building Docker client..."
docker build --target client -t duckway-client . -q 2>&1 >/dev/null

# Cleanup old runs
cleanup 2>/dev/null || true

# Mock Discord upstream so [17] CC tests can drive the real assign path
# without going to the network. Channel IDs are deterministic (incrementing
# from 1000) so assertions can pin them.
DISCORD_MOCK_PORT=$((PORT + 100))
python3 - <<PYEOF >/tmp/dw-e2e-discord.log 2>&1 &
import http.server, json, sys, threading
counter = [1000]
lock = threading.Lock()
class H(http.server.BaseHTTPRequestHandler):
    def _send(self, code, body):
        self.send_response(code)
        self.send_header('Content-Type','application/json')
        b = json.dumps(body).encode()
        self.send_header('Content-Length', str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def do_POST(self):
        ln = int(self.headers.get('Content-Length','0'))
        raw = self.rfile.read(ln) if ln else b''
        body = json.loads(raw or b'{}')
        if '/guilds/' in self.path and self.path.endswith('/channels'):
            with lock:
                counter[0] += 1
                cid = str(counter[0])
            return self._send(200, {
              'id': cid, 'name': body.get('name','x'), 'type': 0,
              'parent_id': body.get('parent_id'),
              'guild_id': self.path.split('/')[2],
            })
        if '/messages' in self.path:
            return self._send(200, {'id':'9999','channel_id':self.path.split('/')[2]})
        return self._send(404, {'message':'mock: unknown POST '+self.path})
    def do_PATCH(self):
        ln = int(self.headers.get('Content-Length','0'))
        self.rfile.read(ln) if ln else None
        return self._send(200, {'id': self.path.split('/')[-1]})
    def do_GET(self):
        if self.path.endswith('/channels'):
            return self._send(200, [])
        if '/messages' in self.path:
            return self._send(200, [])
        return self._send(404, {'message':'mock: unknown GET '+self.path})
    def log_message(self, *a, **k): pass
http.server.HTTPServer(('127.0.0.1', $DISCORD_MOCK_PORT), H).serve_forever()
PYEOF
DISCORD_MOCK_PID=$!
sleep 0.5

echo -e "${YELLOW}[Setup]${NC} Starting server on :$PORT (Discord mock on :$DISCORD_MOCK_PORT)..."
DUCKWAY_DATA_DIR="$DATA_DIR" DUCKWAY_LISTEN="127.0.0.1:$PORT" \
  DUCKWAY_DISCORD_BASE_URL="http://127.0.0.1:$DISCORD_MOCK_PORT" \
  DUCKWAY_CC_DISABLE_GATEWAY=1 \
  /tmp/duckway-e2e-server &>/tmp/dw-e2e-server.log &
SERVER_PID=$!
sleep 3

if ! kill -0 $SERVER_PID 2>/dev/null; then
  echo -e "${RED}Server failed to start:${NC}"
  cat /tmp/dw-e2e-server.log
  exit 1
fi

PW=$(grep "Password:" /tmp/dw-e2e-server.log | sed 's/.*Password: //')


# === Test 1: Server Health ===
echo ""
echo -e "${YELLOW}[1] Server Health${NC}"
STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/admin/login")
assert_eq "Login page returns 200" "200" "$STATUS"

STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/static/style.css")
assert_eq "Static CSS serves" "200" "$STATUS"

STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/static/htmx.min.js")
assert_eq "Static HTMX serves" "200" "$STATUS"

SKILL=$(curl -s "$BASE/skill/duckway-agent.md" | head -1)
assert_contains "Skill file serves" "Duckway" "$SKILL"


# === Test 2: Auth + Session Redirect ===
echo ""
echo -e "${YELLOW}[2] Authentication${NC}"

RESULT=$(curl -s -X POST "$BASE/api/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"username":"wrong","password":"wrong"}' | jq -r '.error // ""')
assert_eq "Bad credentials rejected" "invalid credentials" "$RESULT"

# No cookie → admin pages redirect to login
REDIR=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/admin/")
assert_eq "No session → admin page redirects (303)" "303" "$REDIR"

REDIR_LOC=$(curl -s -o /dev/null -w "%{redirect_url}" "$BASE/admin/services")
assert_contains "Redirect goes to /admin/login" "/admin/login" "$REDIR_LOC"

# No cookie → API returns JSON 401
API_NO_AUTH=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/api/services")
assert_eq "No session → API returns 401" "401" "$API_NO_AUTH"

# Expired/fake cookie → redirect
REDIR_BAD=$(curl -s -o /dev/null -w "%{http_code}" -b "duckway_session=invalid_garbage" "$BASE/admin/clients")
assert_eq "Bad session → admin page redirects (303)" "303" "$REDIR_BAD"

RESULT=$(curl -s -c /tmp/dw-e2e-cookies -X POST "$BASE/api/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"duckway\",\"password\":\"$PW\"}" | jq -r '.status')
assert_eq "JSON login succeeds" "ok" "$RESULT"

STATUS=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/admin/login" \
  -d "username=duckway&password=$PW" -o /dev/null -w "%{http_code}")
assert_eq "Form login redirects (303)" "303" "$STATUS"


# === Test 3: Default Services ===
echo ""
echo -e "${YELLOW}[3] Default Services${NC}"

SERVICES=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/services")
SVC_COUNT=$(echo "$SERVICES" | jq 'length')
assert_eq "6 default services seeded" "6" "$SVC_COUNT"

for name in heartbeat openai anthropic github discord telegram; do
  FOUND=$(echo "$SERVICES" | jq -r ".[] | select(.name==\"$name\") | .name")
  assert_eq "Service '$name' exists" "$name" "$FOUND"
done

OPENAI_ID=$(echo "$SERVICES" | jq -r '.[] | select(.name=="openai") | .id')
ANTHROPIC_ID=$(echo "$SERVICES" | jq -r '.[] | select(.name=="anthropic") | .id')
GITHUB_ID=$(echo "$SERVICES" | jq -r '.[] | select(.name=="github") | .id')


# === Test 4: API Key CRUD ===
echo ""
echo -e "${YELLOW}[4] API Key CRUD${NC}"

# JSON create
KEY1=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/keys" \
  -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$OPENAI_ID\",\"name\":\"OpenAI Prod\",\"key\":\"sk-proj-fake-openai-key-1234567890abcdef1234567890abcdef\"}")
KEY1_ID=$(echo "$KEY1" | jq -r '.id')
assert_not_empty "Create API key (JSON)" "$KEY1_ID"

# Form create
KEY2=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/keys" \
  -d "service_id=$ANTHROPIC_ID&name=Anthropic+Prod&key=sk-ant-fake-anthropic-key-1234567890abcdef")
KEY2_ID=$(echo "$KEY2" | jq -r '.id')
assert_not_empty "Create API key (form)" "$KEY2_ID"

KEY3=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/keys" \
  -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$GITHUB_ID\",\"name\":\"GitHub Token\",\"key\":\"ghp_fakeGitHubToken1234567890abcdef12\"}")
KEY3_ID=$(echo "$KEY3" | jq -r '.id')
assert_not_empty "Create GitHub key" "$KEY3_ID"

KEY_COUNT=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/keys" | jq 'length')
assert_eq "4 API keys exist (3 + heartbeat)" "4" "$KEY_COUNT"


# === Test 5: Client Registration ===
echo ""
echo -e "${YELLOW}[5] Client Registration${NC}"

CLIENT=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients" \
  -H "Content-Type: application/json" \
  -d '{"name":"e2e-test-client"}')
CLIENT_ID=$(echo "$CLIENT" | jq -r '.id')
CLIENT_TOKEN=$(echo "$CLIENT" | jq -r '.token')
assert_not_empty "Client registered" "$CLIENT_ID"
assert_not_empty "Client token returned" "$CLIENT_TOKEN"

# Form create
CLIENT2=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients" \
  -d "name=form-client")
CLIENT2_ID=$(echo "$CLIENT2" | jq -r '.id')
assert_not_empty "Client registered (form)" "$CLIENT2_ID"


# === Test 6: Placeholder Keys ===
echo ""
echo -e "${YELLOW}[6] Placeholder Keys${NC}"

PH1=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/placeholders" \
  -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$OPENAI_ID\",\"api_key_id\":\"$KEY1_ID\",\"client_id\":\"$CLIENT_ID\",\"requires_approval\":false}")
PH1_ID=$(echo "$PH1" | jq -r '.id')
PH1_KEY=$(echo "$PH1" | jq -r '.placeholder')
PH1_ENV=$(echo "$PH1" | jq -r '.env_name')
assert_not_empty "Placeholder created" "$PH1_ID"
assert_contains "Placeholder has dw_ marker" "dw_" "$PH1_KEY"
assert_eq "Env name is OPENAI_API_KEY" "OPENAI_API_KEY" "$PH1_ENV"

PH2=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/placeholders" \
  -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$ANTHROPIC_ID\",\"api_key_id\":\"$KEY2_ID\",\"client_id\":\"$CLIENT_ID\",\"requires_approval\":false}")
PH2_KEY=$(echo "$PH2" | jq -r '.placeholder')
assert_contains "Anthropic placeholder has sk-ant-dw_" "sk-ant-dw_" "$PH2_KEY"

PH3=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/placeholders" \
  -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$GITHUB_ID\",\"api_key_id\":\"$KEY3_ID\",\"client_id\":\"$CLIENT_ID\",\"requires_approval\":false}")
PH3_KEY=$(echo "$PH3" | jq -r '.placeholder')
assert_contains "GitHub placeholder has ghp_dw_" "ghp_dw_" "$PH3_KEY"


# === Test 7: Client Key Sync ===
echo ""
echo -e "${YELLOW}[7] Client Key Sync (API)${NC}"

KEYS=$(curl -s -H "X-Duckway-Token: $CLIENT_TOKEN" "$BASE/client/keys")
SYNC_COUNT=$(echo "$KEYS" | jq 'length')
assert_eq "Client syncs 4 keys (3 + heartbeat)" "4" "$SYNC_COUNT"

SYNCED_ENVS=$(echo "$KEYS" | jq -r '.[].env_name' | sort | tr '\n' ',')
assert_contains "Has OPENAI_API_KEY" "OPENAI_API_KEY" "$SYNCED_ENVS"
assert_contains "Has ANTHROPIC_API_KEY" "ANTHROPIC_API_KEY" "$SYNCED_ENVS"
assert_contains "Has GITHUB_TOKEN" "GITHUB_TOKEN" "$SYNCED_ENVS"


# === Test 8: Docker Client Sync ===
echo ""
echo -e "${YELLOW}[8] Docker Client Sync${NC}"

docker rm -f duckway-e2e-client 2>/dev/null || true
docker run -d --name duckway-e2e-client --network host duckway-client >/dev/null

# Write config into the container
docker exec duckway-e2e-client sh -c "cat > /root/.duckway/config.yaml << DEOF
server_url: $BASE
client_name: e2e-test-client
token: $CLIENT_TOKEN
proxy_port: 18080
DEOF"

# Run sync
SYNC_OUT=$(docker exec duckway-e2e-client duckway sync 2>&1)
assert_contains "Docker sync succeeds" "Synced 4" "$SYNC_OUT"

# Check keys.env
KEYS_ENV=$(docker exec duckway-e2e-client cat /root/.duckway/keys.env)
assert_contains "keys.env has OPENAI_API_KEY" "OPENAI_API_KEY" "$KEYS_ENV"
assert_contains "keys.env has ANTHROPIC_API_KEY" "ANTHROPIC_API_KEY" "$KEYS_ENV"
assert_contains "keys.env has GITHUB_TOKEN" "GITHUB_TOKEN" "$KEYS_ENV"
assert_contains "keys.env has dw_ marker" "dw_" "$KEYS_ENV"

# Run env
ENV_OUT=$(docker exec duckway-e2e-client duckway env 2>&1)
assert_contains "duckway env exports OPENAI_API_KEY" "export OPENAI_API_KEY=" "$ENV_OUT"

# Run status
STATUS_OUT=$(docker exec duckway-e2e-client duckway status 2>&1)
assert_contains "duckway status shows OK" "Connection:  OK" "$STATUS_OUT"
assert_contains "duckway status shows 4 keys" "4 placeholder" "$STATUS_OUT"
assert_contains "duckway status heartbeat OK" "Heartbeat:   OK" "$STATUS_OUT"
assert_contains "duckway status shows CA cert" "CA cert:" "$STATUS_OUT"


# === Test 9: Docker Client Proxy (full chain) ===
echo ""
echo -e "${YELLOW}[9] Docker Client Proxy Chain${NC}"

# Start duckway proxy inside docker container (background)
docker exec -d duckway-e2e-client duckway proxy --port 18099
sleep 2

# Test 1: Heartbeat through client proxy → server → internal response
HB_VIA_PROXY=$(docker exec duckway-e2e-client curl -s \
  --proxy http://127.0.0.1:18099 \
  http://doesnt-matter/proxy/heartbeat/ping 2>&1)
assert_contains "Heartbeat via client proxy" "duckway-heartbeat" "$HB_VIA_PROXY"
assert_contains "Heartbeat shows client name" "e2e-test-client" "$HB_VIA_PROXY"

# Test 2: OpenAI via client proxy → server → upstream (proves key injection)
OPENAI_VIA_PROXY=$(docker exec duckway-e2e-client curl -s \
  --proxy http://127.0.0.1:18099 \
  -X POST http://doesnt-matter/proxy/openai/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"test"}]}' 2>&1)
assert_contains "OpenAI via client proxy reaches upstream" "invalid_api_key" "$OPENAI_VIA_PROXY"

# Test 3: Verify server captured the placeholder key by checking request log
LOG_COUNT=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/services" | jq 'length')
assert_not_empty "Server API still responsive after proxy test" "$LOG_COUNT"

# Test 4: GitHub via client proxy
GH_VIA_PROXY=$(docker exec duckway-e2e-client curl -s \
  --proxy http://127.0.0.1:18099 \
  http://doesnt-matter/proxy/github/user 2>&1)
assert_contains "GitHub via client proxy reaches upstream" "Bad credentials" "$GH_VIA_PROXY"

# Test 5: Direct heartbeat without proxy (client → server API)
HB_DIRECT=$(docker exec duckway-e2e-client curl -s \
  -H "X-Duckway-Token: $CLIENT_TOKEN" \
  "$BASE/proxy/heartbeat/ping" 2>&1)
assert_contains "Direct heartbeat (no proxy)" "duckway-heartbeat" "$HB_DIRECT"

# Test 6: Verify the proxy resolved placeholder and logged the request
# Check via logs API
LOGS_API=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/logs" 2>&1)
assert_contains "Request log captured heartbeat" "heartbeat" "$LOGS_API"
assert_contains "Request log captured openai" "openai" "$LOGS_API"


# === Test 10: Proxy Key Injection (direct, no client proxy) ===
echo ""
echo -e "${YELLOW}[10] Proxy Key Injection (direct)${NC}"

PROXY_RESP=$(curl -s -X POST "$BASE/proxy/openai/v1/chat/completions" \
  -H "X-Duckway-Token: $CLIENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"test"}]}')
# OpenAI returns 401 with the real key masked — proves injection
assert_contains "Proxy injects real key (OpenAI)" "invalid_api_key" "$PROXY_RESP"

PROXY_RESP2=$(curl -s -X GET "$BASE/proxy/github/user" \
  -H "X-Duckway-Token: $CLIENT_TOKEN")
assert_contains "Proxy reaches GitHub upstream" "Bad credentials" "$PROXY_RESP2"

HEARTBEAT=$(curl -s "$BASE/proxy/heartbeat/ping" \
  -H "X-Duckway-Token: $CLIENT_TOKEN")
assert_contains "Heartbeat responds OK" "duckway-heartbeat" "$HEARTBEAT"


# === Test 11: Approval Workflow ===
echo ""
echo -e "${YELLOW}[11] Approval Workflow${NC}"

# Create placeholder that requires approval
PH_APPROVAL=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/placeholders" \
  -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$OPENAI_ID\",\"api_key_id\":\"$KEY1_ID\",\"client_id\":\"$CLIENT2_ID\",\"requires_approval\":true,\"env_name\":\"OPENAI_GATED\"}")

CLIENT2_TOKEN=$(echo "$CLIENT2" | jq -r '.token')
BLOCKED=$(curl -s -X POST "$BASE/proxy/openai/v1/models" \
  -H "X-Duckway-Token: $CLIENT2_TOKEN")
assert_contains "Approval blocks request" "duckway_approval_pending" "$BLOCKED"

APPROVAL_ID=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/approvals" | jq -r '.[0].id')
assert_not_empty "Pending approval created" "$APPROVAL_ID"

curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/approvals/$APPROVAL_ID/approve" \
  -H "Content-Type: application/json" \
  -d '{"duration_minutes":60}' >/dev/null

AFTER=$(curl -s -X GET "$BASE/proxy/openai/v1/models" \
  -H "X-Duckway-Token: $CLIENT2_TOKEN")
assert_contains "After approval, proxy works" "invalid_api_key" "$AFTER"


# === Test 11b: Service-level ACL templates ===
echo ""
echo -e "${YELLOW}[11b] Service ACL Templates${NC}"

# List templates for openai
TEMPLATES=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/services/$OPENAI_ID/acl-templates")
TEMPL_COUNT=$(echo "$TEMPLATES" | jq '.templates | length')
# Should have at least: allow-all, chat-only, chat-embeddings, inference-all, no-admin
if [ "$TEMPL_COUNT" -ge 4 ]; then
  echo -e "  ${GREEN}PASS${NC} OpenAI has $TEMPL_COUNT ACL templates (>=4)"
  PASS=$((PASS + 1))
else
  echo -e "  ${RED}FAIL${NC} Expected >=4 OpenAI templates, got $TEMPL_COUNT"
  FAIL=$((FAIL + 1))
fi

assert_contains "Has chat-only template" "chat-only" "$TEMPLATES"
assert_contains "Has allow-all template" "allow-all" "$TEMPLATES"

# Apply chat-only template
APPLY=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/services/$OPENAI_ID/acl-templates" \
  -H "Content-Type: application/json" \
  -d '{"template_id":"chat-only"}')
assert_eq "Apply chat-only template" "ok" "$(echo "$APPLY" | jq -r '.status')"

# Verify service now has ACL set
SVC_WITH_ACL=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/services" | jq -r ".[] | select(.id==\"$OPENAI_ID\") | .default_acl")
assert_contains "Service default_acl contains chat-only rule" "chat-only" "$SVC_WITH_ACL"

# Test ACL blocks unlisted endpoint (using a placeholder without its own config)
# Create a new client + placeholder without permission_config
CLIENT_ACL=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients" \
  -H "Content-Type: application/json" -d '{"name":"acl-test-client"}')
CLIENT_ACL_ID=$(echo "$CLIENT_ACL" | jq -r '.id')
CLIENT_ACL_TOKEN=$(echo "$CLIENT_ACL" | jq -r '.token')

curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/placeholders" \
  -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$OPENAI_ID\",\"api_key_id\":\"$KEY1_ID\",\"client_id\":\"$CLIENT_ACL_ID\",\"requires_approval\":false}" >/dev/null

# Allowed: POST /v1/chat/completions
ACL_OK=$(curl -s -X POST "$BASE/proxy/openai/v1/chat/completions" \
  -H "X-Duckway-Token: $CLIENT_ACL_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"test"}]}')
assert_contains "Service ACL allows chat completions" "invalid_api_key" "$ACL_OK"

# Denied: POST /v1/images/generations
ACL_DENIED=$(curl -s -X POST "$BASE/proxy/openai/v1/images/generations" \
  -H "X-Duckway-Token: $CLIENT_ACL_TOKEN" \
  -H "Content-Type: application/json" -d '{"prompt":"cat"}')
assert_contains "Service ACL blocks images endpoint" "permission denied" "$ACL_DENIED"

# Reset to allow-all for remaining tests
curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/services/$OPENAI_ID/acl-templates" \
  -H "Content-Type: application/json" \
  -d '{"template_id":"allow-all"}' >/dev/null


# === Test 11c: Per-API-key ACL ===
echo ""
echo -e "${YELLOW}[11c] API Key ACL${NC}"

# List ACL templates for an API key
KEY_TMPL=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/keys/$KEY1_ID/acl-templates")
KEY_TMPL_COUNT=$(echo "$KEY_TMPL" | jq '.templates | length')
if [ "$KEY_TMPL_COUNT" -ge 4 ]; then
  echo -e "  ${GREEN}PASS${NC} API key has $KEY_TMPL_COUNT ACL templates"
  PASS=$((PASS + 1))
else
  echo -e "  ${RED}FAIL${NC} Expected >=4 templates for key, got $KEY_TMPL_COUNT"
  FAIL=$((FAIL + 1))
fi

# Apply chat-only to the API key
KEY_APPLY=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/keys/$KEY1_ID/acl-templates" \
  -H "Content-Type: application/json" \
  -d '{"template_id":"chat-only"}')
assert_eq "Apply chat-only to API key" "ok" "$(echo "$KEY_APPLY" | jq -r '.status')"

# Verify key has ACL set
KEY_ACL=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/keys" | jq -r ".[] | select(.id==\"$KEY1_ID\") | .acl")
assert_contains "API key ACL contains chat-only" "chat-only" "$KEY_ACL"

# Test ACL blocks unlisted endpoint via the placeholder that uses this key
ACL_KEY_DENIED=$(curl -s -X POST "$BASE/proxy/openai/v1/images/generations" \
  -H "X-Duckway-Token: $CLIENT_ACL_TOKEN" \
  -H "Content-Type: application/json" -d '{"prompt":"cat"}')
assert_contains "API key ACL blocks images endpoint" "permission denied" "$ACL_KEY_DENIED"

# Set custom ACL JSON
CUSTOM_SET=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/keys/$KEY1_ID/acl" \
  -H "Content-Type: application/json" \
  -d '{"acl":""}')
assert_eq "Clear API key ACL" "ok" "$(echo "$CUSTOM_SET" | jq -r '.status')"

# Verify it's cleared
KEY_ACL_CLEARED=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/keys" | jq -r ".[] | select(.id==\"$KEY1_ID\") | .acl")
assert_eq "API key ACL is empty after clear" "" "$KEY_ACL_CLEARED"


# === Test 11d: ACL across different services ===
echo ""
echo -e "${YELLOW}[11d] ACL Across Services${NC}"

# --- GitHub read-only ACL ---
GH_SVC_ID=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/services" | jq -r '.[] | select(.name=="github") | .id')

# Apply read-only to GitHub service
curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/services/$GH_SVC_ID/acl-templates" \
  -H "Content-Type: application/json" \
  -d '{"template_id":"read-only"}' > /dev/null

# Create client + placeholder for GitHub
CLIENT_GH=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients" \
  -H "Content-Type: application/json" -d '{"name":"gh-acl-test"}')
CLIENT_GH_ID=$(echo "$CLIENT_GH" | jq -r '.id')
CLIENT_GH_TOKEN=$(echo "$CLIENT_GH" | jq -r '.token')

curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/placeholders" \
  -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$GH_SVC_ID\",\"api_key_id\":\"$KEY3_ID\",\"client_id\":\"$CLIENT_GH_ID\",\"requires_approval\":false}" > /dev/null

# GET should work (read-only allows GET /*)
GH_GET=$(curl -s "$BASE/proxy/github/user" -H "X-Duckway-Token: $CLIENT_GH_TOKEN")
assert_contains "GitHub read-only: GET allowed" "Bad credentials" "$GH_GET"

# POST should be denied
GH_POST=$(curl -s -X POST "$BASE/proxy/github/repos/owner/repo/issues" \
  -H "X-Duckway-Token: $CLIENT_GH_TOKEN" \
  -H "Content-Type: application/json" -d '{"title":"test"}')
assert_contains "GitHub read-only: POST denied" "permission denied" "$GH_POST"

# Reset GitHub ACL
curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/services/$GH_SVC_ID/acl-templates" \
  -H "Content-Type: application/json" -d '{"template_id":"allow-all"}' > /dev/null

# --- Anthropic messages-only ACL on API key ---
AN_SVC_ID=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/services" | jq -r '.[] | select(.name=="anthropic") | .id')

# Apply messages-only to the Anthropic key
curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/keys/$KEY2_ID/acl-templates" \
  -H "Content-Type: application/json" -d '{"template_id":"messages-only"}' > /dev/null

# Create client + placeholder for Anthropic
CLIENT_AN=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients" \
  -H "Content-Type: application/json" -d '{"name":"an-acl-test"}')
CLIENT_AN_ID=$(echo "$CLIENT_AN" | jq -r '.id')
CLIENT_AN_TOKEN=$(echo "$CLIENT_AN" | jq -r '.token')

curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/placeholders" \
  -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$AN_SVC_ID\",\"api_key_id\":\"$KEY2_ID\",\"client_id\":\"$CLIENT_AN_ID\",\"requires_approval\":false}" > /dev/null

# POST /v1/messages should work
AN_MSG=$(curl -s -X POST "$BASE/proxy/anthropic/v1/messages" \
  -H "X-Duckway-Token: $CLIENT_AN_TOKEN" \
  -H "Content-Type: application/json" -d '{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}')
# Should reach upstream (not permission denied)
AN_MSG_DENIED=$(echo "$AN_MSG" | grep -c "permission denied" || true)
assert_eq "Anthropic messages-only: POST /v1/messages allowed" "0" "$AN_MSG_DENIED"

# POST /v1/messages/batches should be denied (not in messages-only)
AN_BATCH=$(curl -s -X POST "$BASE/proxy/anthropic/v1/messages/batches" \
  -H "X-Duckway-Token: $CLIENT_AN_TOKEN" \
  -H "Content-Type: application/json" -d '{}')
assert_contains "Anthropic messages-only: batches denied" "permission denied" "$AN_BATCH"

# Clear key ACL
curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/keys/$KEY2_ID/acl" \
  -H "Content-Type: application/json" -d '{"acl":""}' > /dev/null

# --- ACL layering: all layers checked, can only narrow ---
echo ""
echo -e "${YELLOW}[11e] ACL Layering (narrow-only)${NC}"

# Service = chat-only (allows chat + models)
curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/services/$OPENAI_ID/acl-templates" \
  -H "Content-Type: application/json" -d '{"template_id":"chat-only"}' > /dev/null

# Key = allow-all (does NOT widen — service still blocks images)
curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/keys/$KEY1_ID/acl" \
  -H "Content-Type: application/json" -d '{"acl":""}' > /dev/null

# Service blocks images even though key has no restriction
LAYER_SVC=$(curl -s -X POST "$BASE/proxy/openai/v1/images/generations" \
  -H "X-Duckway-Token: $CLIENT_ACL_TOKEN" \
  -H "Content-Type: application/json" -d '{"prompt":"cat"}')
assert_contains "Layering: service blocks images (key can't widen)" "permission denied (service)" "$LAYER_SVC"

# Chat still allowed (service permits it)
LAYER_CHAT=$(curl -s -X POST "$BASE/proxy/openai/v1/chat/completions" \
  -H "X-Duckway-Token: $CLIENT_ACL_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"test"}]}')
assert_contains "Layering: chat still allowed through service ACL" "invalid_api_key" "$LAYER_CHAT"

# Now add key ACL that also restricts (both service + key checked)
# Service = chat-only, Key = no-admin (blocks org + fine-tuning)
curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/keys/$KEY1_ID/acl-templates" \
  -H "Content-Type: application/json" -d '{"template_id":"no-admin"}' > /dev/null

# Images blocked by service layer
LAYER_BOTH1=$(curl -s -X POST "$BASE/proxy/openai/v1/images/generations" \
  -H "X-Duckway-Token: $CLIENT_ACL_TOKEN" \
  -H "Content-Type: application/json" -d '{"prompt":"cat"}')
assert_contains "Layering: service still blocks images with key ACL set" "permission denied (service)" "$LAYER_BOTH1"

# Chat allowed by both layers
LAYER_BOTH2=$(curl -s -X POST "$BASE/proxy/openai/v1/chat/completions" \
  -H "X-Duckway-Token: $CLIENT_ACL_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"test"}]}')
assert_contains "Layering: chat passes both service + key ACL" "invalid_api_key" "$LAYER_BOTH2"

# Clean up
curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/services/$OPENAI_ID/acl-templates" \
  -H "Content-Type: application/json" -d '{"template_id":"allow-all"}' > /dev/null
curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/keys/$KEY1_ID/acl" \
  -H "Content-Type: application/json" -d '{"acl":""}' > /dev/null


# === Test 12: Permission System ===
echo ""
echo -e "${YELLOW}[12] Permission System${NC}"

PERM='{"version":"1","provider":"openai","rules":[{"name":"limited","endpoints":[{"method":"POST","path":"/v1/chat/completions","allow":true,"constraints":{"body":{"model":{"oneOf":["gpt-4o-mini"]}}}}],"deny_all_other":true}]}'

# Create a client with permission config
CLIENT3=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients" \
  -H "Content-Type: application/json" \
  -d '{"name":"perm-client"}')
CLIENT3_ID=$(echo "$CLIENT3" | jq -r '.id')
CLIENT3_TOKEN=$(echo "$CLIENT3" | jq -r '.token')

curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/placeholders" \
  -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$OPENAI_ID\",\"api_key_id\":\"$KEY1_ID\",\"client_id\":\"$CLIENT3_ID\",\"requires_approval\":false,\"permission_config\":$(echo "$PERM" | jq -Rs .)}" >/dev/null

# Allowed: gpt-4o-mini
ALLOWED=$(curl -s -X POST "$BASE/proxy/openai/v1/chat/completions" \
  -H "X-Duckway-Token: $CLIENT3_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}')
assert_contains "Allowed model passes" "invalid_api_key" "$ALLOWED"

# Denied: gpt-4o
DENIED=$(curl -s -X POST "$BASE/proxy/openai/v1/chat/completions" \
  -H "X-Duckway-Token: $CLIENT3_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}')
assert_contains "Denied model blocked" "permission denied" "$DENIED"

# Denied: wrong endpoint
DENIED2=$(curl -s -X POST "$BASE/proxy/openai/v1/images/generations" \
  -H "X-Duckway-Token: $CLIENT3_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"prompt":"cat"}')
assert_contains "Unlisted endpoint blocked" "permission denied" "$DENIED2"


# === Test 13: Canary Tokens ===
echo ""
echo -e "${YELLOW}[13] Canary Tokens${NC}"

# Save canary settings
CANARY_SAVE=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/canary/settings" \
  -H "Content-Type: application/json" \
  -d '{"email":"test@example.com","enabled_types":["aws_keys","github"]}')
assert_eq "Save canary settings" "ok" "$(echo "$CANARY_SAVE" | jq -r '.status')"

# Get canary settings
CANARY_GET=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/canary/settings")
assert_eq "Canary email saved" "test@example.com" "$(echo "$CANARY_GET" | jq -r '.email')"
assert_eq "2 types enabled" "2" "$(echo "$CANARY_GET" | jq '.enabled_types | length')"

# Generate canary tokens for e2e-test-client (skips canarytokens.org API in test)
# Just verify the endpoint exists and responds
GEN_STATUS=$(curl -s -o /dev/null -w "%{http_code}" -b /tmp/dw-e2e-cookies \
  -X POST "$BASE/api/canary/clients/$CLIENT_ID/generate?name=e2e-test-client")
assert_eq "Generate canary endpoint responds" "200" "$GEN_STATUS"

# Client canary sync endpoint
CANARY_SYNC=$(curl -s -H "X-Duckway-Token: $CLIENT_TOKEN" "$BASE/client/canaries")
CANARY_SYNC_STATUS=$(echo "$CANARY_SYNC" | jq 'type')
assert_eq "Client canary endpoint returns array" '"array"' "$CANARY_SYNC_STATUS"

# Verify available types are returned with all fields
AVAIL_COUNT=$(echo "$CANARY_GET" | jq '.available_types | length')
assert_eq "16 canary types available" "16" "$AVAIL_COUNT"

# Check types have required fields
FIRST_TYPE=$(echo "$CANARY_GET" | jq -r '.available_types[0].type')
assert_not_empty "Type has 'type' field" "$FIRST_TYPE"

HAS_PATH=$(echo "$CANARY_GET" | jq -r '.available_types[0].deploy_path')
assert_not_empty "Type has 'deploy_path' field" "$HAS_PATH"

HAS_DEFAULT=$(echo "$CANARY_GET" | jq -r '.available_types[0].default_enabled')
assert_not_empty "Type has 'default_enabled' field" "$HAS_DEFAULT"

# Verify no canary API works without auth
CANARY_NO_AUTH=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/api/canary/settings")
assert_eq "Canary settings requires auth" "401" "$CANARY_NO_AUTH"


# === Test 14: Admin Pages ===
echo ""
echo -e "${YELLOW}[14] Admin Panel Pages${NC}"

for page in "" services keys placeholders clients groups approvals logs notifications canary docs oauth cc; do
  STATUS=$(curl -s -b /tmp/dw-e2e-cookies -o /dev/null -w "%{http_code}" "$BASE/admin/$page")
  assert_eq "GET /admin/$page returns 200" "200" "$STATUS"
done


# === Test 16: Loan Proxy ===
echo ""
echo -e "${YELLOW}[16] Loan Proxy (delivery_mode=loan_proxy)${NC}"

# 16a: github service should default to loan_proxy + multi-host pattern
GH_SVC_JSON=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/services" | python3 -c "import sys,json;[print(json.dumps(s)) for s in json.load(sys.stdin) if s['name']=='github']")
GH_DELIVERY=$(echo "$GH_SVC_JSON" | python3 -c "import sys,json;print(json.load(sys.stdin).get('delivery_mode',''))")
GH_HOSTS=$(echo "$GH_SVC_JSON" | python3 -c "import sys,json;print(json.load(sys.stdin).get('host_pattern',''))")
assert_eq "github seeded with delivery_mode=loan_proxy" "loan_proxy" "$GH_DELIVERY"
assert_not_empty "github host_pattern not empty" "$GH_HOSTS"
assert_contains "github host_pattern includes api.github.com" "api.github.com" "$GH_HOSTS"
assert_contains "github host_pattern includes github.com" "github.com" "$GH_HOSTS"

# 16b: /client/services exposes delivery_mode for sidecar
CLIENT_SVCS=$(curl -s -H "X-Duckway-Token: $CLIENT_TOKEN" "$BASE/client/services")
assert_contains "/client/services includes delivery_mode" "delivery_mode" "$CLIENT_SVCS"
assert_contains "/client/services includes upstream_url" "upstream_url" "$CLIENT_SVCS"

# 16c: Upload a fake github key, register a fresh client, assign phantom
GH_SVC_ID=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/services" | python3 -c "import sys,json;print([s['id'] for s in json.load(sys.stdin) if s['name']=='github'][0])")
LOAN_KEY_ID=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/keys" -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$GH_SVC_ID\",\"name\":\"loan-test\",\"key\":\"ghp_realisticfaketokenforloantest1234\"}" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
assert_not_empty "Fake github key uploaded" "$LOAN_KEY_ID"

LOAN_CLIENT=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients" -H "Content-Type: application/json" -d '{"name":"loan-test"}')
LOAN_CLIENT_ID=$(echo "$LOAN_CLIENT" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")
LOAN_CLIENT_TOKEN=$(echo "$LOAN_CLIENT" | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])")

curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/placeholders" -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$GH_SVC_ID\",\"api_key_id\":\"$LOAN_KEY_ID\",\"client_id\":\"$LOAN_CLIENT_ID\",\"requires_approval\":false}" > /dev/null

# 16d: GET /client/loan returns the real token + auth scheme
LOAN_RESP=$(curl -s -H "X-Duckway-Token: $LOAN_CLIENT_TOKEN" "$BASE/client/loan?service=github")
LOAN_REAL=$(echo "$LOAN_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('real_token',''))")
LOAN_TTL=$(echo "$LOAN_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('ttl_seconds',''))")
LOAN_AUTH=$(echo "$LOAN_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('auth_type',''))")
assert_eq "/client/loan returns real_token" "ghp_realisticfaketokenforloantest1234" "$LOAN_REAL"
assert_eq "/client/loan returns 60s TTL" "60" "$LOAN_TTL"
assert_eq "/client/loan returns auth_type=bearer" "bearer" "$LOAN_AUTH"

# 16e: /client/loan refuses for proxy-mode services (openai)
OPENAI_REFUSE=$(curl -s -o /dev/null -w "%{http_code}" -H "X-Duckway-Token: $LOAN_CLIENT_TOKEN" "$BASE/client/loan?service=openai")
assert_eq "/client/loan?service=openai refused (proxy mode)" "403" "$OPENAI_REFUSE"

# 16f: /client/loan returns 404 for unknown service
UNKNOWN_STATUS=$(curl -s -o /dev/null -w "%{http_code}" -H "X-Duckway-Token: $LOAN_CLIENT_TOKEN" "$BASE/client/loan?service=does-not-exist")
assert_eq "/client/loan unknown service → 404" "404" "$UNKNOWN_STATUS"

# 16g: /client/loan rejects without client auth
NOAUTH_STATUS=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/client/loan?service=github")
assert_eq "/client/loan without token → 401" "401" "$NOAUTH_STATUS"

# 16h: POST /client/audit accepts batched entries
AUDIT_RESP=$(curl -s -X POST -H "X-Duckway-Token: $LOAN_CLIENT_TOKEN" -H "Content-Type: application/json" \
  -d "[{\"placeholder_id\":\"abc\",\"service\":\"github\",\"method\":\"POST\",\"path\":\"/git-upload-pack\",\"status\":200},{\"placeholder_id\":\"abc\",\"service\":\"github\",\"method\":\"GET\",\"path\":\"/info/refs\",\"status\":200}]" \
  "$BASE/client/audit")
AUDIT_LOGGED=$(echo "$AUDIT_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('logged',''))")
assert_eq "/client/audit logged 2 entries" "2" "$AUDIT_LOGGED"

# 16i: Audited entries reach the request log
sleep 0.3
LOGS=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/logs")
GIT_UPLOAD_PACK_LOGS=$(echo "$LOGS" | python3 -c "
import sys,json
logs = json.load(sys.stdin)
n = sum(1 for l in logs if l.get('path') == '/git-upload-pack' and l.get('service_name') == 'github')
print(n)
")
if [ "$GIT_UPLOAD_PACK_LOGS" != "0" ] && [ -n "$GIT_UPLOAD_PACK_LOGS" ]; then
  PASS=$((PASS + 1))
  echo -e "  ${GREEN}PASS${NC} audit entries appear in /api/logs (count=$GIT_UPLOAD_PACK_LOGS)"
else
  FAIL=$((FAIL + 1))
  echo -e "  ${RED}FAIL${NC} audit entries appear in /api/logs (count=$GIT_UPLOAD_PACK_LOGS)"
  ERRORS="$ERRORS\n  - audit entries not found in logs"
fi

# 16j: Service Update endpoint accepts delivery_mode
NEW_MODE_RESP=$(curl -s -b /tmp/dw-e2e-cookies -X PUT "$BASE/api/services/$GH_SVC_ID" -H "Content-Type: application/json" -d '{"delivery_mode":"proxy"}')
NEW_MODE=$(echo "$NEW_MODE_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('delivery_mode',''))")
assert_eq "PUT delivery_mode=proxy persists" "proxy" "$NEW_MODE"
# Restore for later tests
curl -s -b /tmp/dw-e2e-cookies -X PUT "$BASE/api/services/$GH_SVC_ID" -H "Content-Type: application/json" -d '{"delivery_mode":"loan_proxy"}' > /dev/null

# 16k: Update rejects invalid delivery_mode
BAD_MODE=$(curl -s -o /dev/null -w "%{http_code}" -b /tmp/dw-e2e-cookies -X PUT "$BASE/api/services/$GH_SVC_ID" -H "Content-Type: application/json" -d '{"delivery_mode":"bogus"}')
assert_eq "PUT delivery_mode=bogus → 400" "400" "$BAD_MODE"

# Cleanup
curl -s -b /tmp/dw-e2e-cookies -X DELETE "$BASE/api/clients/$LOAN_CLIENT_ID" > /dev/null
curl -s -b /tmp/dw-e2e-cookies -X DELETE "$BASE/api/keys/$LOAN_KEY_ID" > /dev/null


# === Test 17: Control Channels (Phase A — schema + admin CRUD only) ===
echo ""
echo -e "${YELLOW}[17] Control Channels (Phase A)${NC}"

# 17a: discord service has been widened by migration to include gateway host
DISCORD_HOSTS=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/services" | python3 -c "import sys,json;print([s for s in json.load(sys.stdin) if s['name']=='discord'][0]['host_pattern'])")
assert_contains "discord host_pattern includes gateway.discord.gg" "gateway.discord.gg" "$DISCORD_HOSTS"
assert_contains "discord host_pattern includes discordapp.net" "discordapp.net" "$DISCORD_HOSTS"

DISCORD_SVC_ID=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/services" | python3 -c "import sys,json;print([s['id'] for s in json.load(sys.stdin) if s['name']=='discord'][0])")

# 17b: empty CC list initially
CC_LIST=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/cc")
CC_LEN=$(echo "$CC_LIST" | python3 -c "import sys,json;d=json.load(sys.stdin);print(len(d) if isinstance(d,list) else -1)")
assert_eq "CC list returns array" "0" "$CC_LEN"

# 17c: Reject CC creation without bot token
BAD_KEY=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/cc" -H "Content-Type: application/json" \
  -d "{\"name\":\"x\",\"service_id\":\"$DISCORD_SVC_ID\"}" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('error',''))")
assert_contains "CC create rejects missing api_key_id" "required" "$BAD_KEY"

# Add a fake bot key under discord
CC_BOT_KEY=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/keys" -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$DISCORD_SVC_ID\",\"name\":\"e2e-bot\",\"key\":\"NzAtest.test.testbot1234567890\"}" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
assert_not_empty "Bot token uploaded" "$CC_BOT_KEY"

# 17d: Reject CC creation without discord guild_id/category_id
BAD_CFG=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/cc" -H "Content-Type: application/json" \
  -d "{\"name\":\"x\",\"service_id\":\"$DISCORD_SVC_ID\",\"api_key_id\":\"$CC_BOT_KEY\",\"config\":{\"guild_id\":\"\"}}" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('error',''))")
assert_contains "CC discord requires guild_id+category_id" "guild_id" "$BAD_CFG"

# 17e: Reject api_key from a different service
OPENAI_KEY_ID=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/keys" | python3 -c "import sys,json;[print(k['id']) for k in json.load(sys.stdin) if k.get('service_name')=='openai'][:1]" | head -1)
if [ -n "$OPENAI_KEY_ID" ]; then
  WRONG_SVC=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/cc" -H "Content-Type: application/json" \
    -d "{\"name\":\"x\",\"service_id\":\"$DISCORD_SVC_ID\",\"api_key_id\":\"$OPENAI_KEY_ID\",\"config\":{\"guild_id\":\"1\",\"category_id\":\"2\"}}" \
    | python3 -c "import sys,json;print(json.load(sys.stdin).get('error',''))")
  assert_contains "CC rejects api_key from other service" "does not belong" "$WRONG_SVC"
fi

# 17f: Successful CC creation populates joined fields
CC_CREATED=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/cc" -H "Content-Type: application/json" \
  -d "{\"name\":\"e2e-cc\",\"service_id\":\"$DISCORD_SVC_ID\",\"api_key_id\":\"$CC_BOT_KEY\",\"config\":{\"guild_id\":\"111\",\"category_id\":\"222\"}}")
CC_ID=$(echo "$CC_CREATED" | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))")
CC_SERVICE_NAME=$(echo "$CC_CREATED" | python3 -c "import sys,json;print(json.load(sys.stdin).get('service_name',''))")
CC_KEY_NAME=$(echo "$CC_CREATED" | python3 -c "import sys,json;print(json.load(sys.stdin).get('api_key_name',''))")
assert_not_empty "CC created" "$CC_ID"
assert_eq "CC create response includes service_name" "discord" "$CC_SERVICE_NAME"
assert_eq "CC create response includes api_key_name" "e2e-bot" "$CC_KEY_NAME"

# 17g: CC detail returns assignments as []
CC_DETAIL=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/cc/$CC_ID")
CC_ASN=$(echo "$CC_DETAIL" | python3 -c "import sys,json;d=json.load(sys.stdin);a=d.get('assignments',[]);print(len(a) if isinstance(a,list) else -1)")
assert_eq "CC detail assignments empty" "0" "$CC_ASN"

# 17h: Create test client + assign CC (real Discord call → mock returns numeric id)
CC_CLIENT_RESP=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients" -H "Content-Type: application/json" -d '{"name":"cc-e2e-client"}')
CC_CLIENT_ID=$(echo "$CC_CLIENT_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")
CC_CLIENT_TOKEN=$(echo "$CC_CLIENT_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])")

ASSIGN_RESP=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients/$CC_CLIENT_ID/cc" -H "Content-Type: application/json" \
  -d "{\"cc_id\":\"$CC_ID\",\"agent_type\":\"claude_code\"}")
ASSIGN_STATUS=$(echo "$ASSIGN_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('status',''))")
ASSIGN_HANDLE=$(echo "$ASSIGN_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('home_handle',''))")
ASSIGN_REAL_CHID=$(echo "$ASSIGN_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('home_channel_id',''))")
ASSIGN_REAL_NAME=$(echo "$ASSIGN_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('home_channel_name',''))")
ASSIGN_PHID=$(echo "$ASSIGN_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('placeholder_id',''))")
assert_eq "Assign CC succeeds" "assigned" "$ASSIGN_STATUS"
assert_contains "Assign returns handle (dwch_ prefix)" "dwch_" "$ASSIGN_HANDLE"
assert_not_empty "Assign returns real Discord channel id" "$ASSIGN_REAL_CHID"
assert_eq "Assign returns sanitized channel name" "cc-e2e-client" "$ASSIGN_REAL_NAME"
assert_not_empty "Assign issues a placeholder_id" "$ASSIGN_PHID"

# 17h-2: Phantom token row was created and bound to the bot api_key
PH_BIND=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/placeholders" | python3 -c "
import sys,json
phs = json.load(sys.stdin)
match = [p for p in phs if p.get('id') == '$ASSIGN_PHID']
if not match: print('missing'); sys.exit()
p = match[0]
print('|'.join([p.get('client_id',''), p.get('api_key_id',''), p.get('env_name','')]))
")
assert_contains "Placeholder bound to client" "$CC_CLIENT_ID" "$PH_BIND"
assert_contains "Placeholder bound to bot key" "$CC_BOT_KEY" "$PH_BIND"
assert_contains "Placeholder env name carries CC id prefix" "DUCKWAY_CC_" "$PH_BIND"

# 17i: Reject duplicate assignment
DUP_ASSIGN=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients/$CC_CLIENT_ID/cc" -H "Content-Type: application/json" \
  -d "{\"cc_id\":\"$CC_ID\",\"agent_type\":\"claude_code\"}" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('error',''))")
assert_contains "Reject duplicate assignment" "already assigned" "$DUP_ASSIGN"

# 17j: Reject unknown agent_type
CC_CLIENT2=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients" -H "Content-Type: application/json" -d '{"name":"cc-e2e-client-2"}' | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")
BAD_AGENT=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients/$CC_CLIENT2/cc" -H "Content-Type: application/json" \
  -d "{\"cc_id\":\"$CC_ID\",\"agent_type\":\"vim\"}" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('error',''))")
assert_contains "Reject unknown agent_type" "unknown agent_type" "$BAD_AGENT"

# 17k: Allow alternative agent_types reserved in v1
ALT=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients/$CC_CLIENT2/cc" -H "Content-Type: application/json" \
  -d "{\"cc_id\":\"$CC_ID\",\"agent_type\":\"cursor\"}" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('status',''))")
assert_eq "Reserved agent_type 'cursor' accepted" "assigned" "$ALT"

# 17l: Per-client CC list shows the assignment with cc_name + home_channel_name
CLIENT_CC_LIST=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/clients/$CC_CLIENT_ID/cc")
CC_NAME_IN_LIST=$(echo "$CLIENT_CC_LIST" | python3 -c "import sys,json;print(json.load(sys.stdin)[0].get('cc_name',''))")
HOME_NAME=$(echo "$CLIENT_CC_LIST" | python3 -c "import sys,json;print(json.load(sys.stdin)[0].get('home_channel_name',''))")
assert_eq "Client CC list — cc_name joined" "e2e-cc" "$CC_NAME_IN_LIST"
assert_eq "Client CC list — channel name = client name" "cc-e2e-client" "$HOME_NAME"

# 17m: CC detail shows 2 assignments after both clients assigned
CC_DETAIL2=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/cc/$CC_ID")
ASN_COUNT=$(echo "$CC_DETAIL2" | python3 -c "import sys,json;print(len(json.load(sys.stdin).get('assignments',[])))")
assert_eq "CC detail shows 2 assignments" "2" "$ASN_COUNT"

# 17m-2: Client API — list assigned CCs
CLIENT_CC_LIST_VIA_TOKEN=$(curl -s -H "X-Duckway-Token: $CC_CLIENT_TOKEN" "$BASE/client/cc")
CLIENT_CC_COUNT=$(echo "$CLIENT_CC_LIST_VIA_TOKEN" | python3 -c "import sys,json;d=json.load(sys.stdin);print(len(d) if isinstance(d,list) else -1)")
assert_eq "Client GET /client/cc lists 1 assignment" "1" "$CLIENT_CC_COUNT"

CLIENT_CC_NAME=$(echo "$CLIENT_CC_LIST_VIA_TOKEN" | python3 -c "import sys,json;print(json.load(sys.stdin)[0]['cc_name'])")
assert_eq "Client GET /client/cc has cc_name" "e2e-cc" "$CLIENT_CC_NAME"

# 17m-3: Client lists channels under the CC (both home channels — 2 clients
# are assigned to this CC by this point in the script).
LIST_CH=$(curl -s -H "X-Duckway-Token: $CC_CLIENT_TOKEN" "$BASE/client/cc/$CC_ID/channels")
LIST_CH_COUNT=$(echo "$LIST_CH" | python3 -c "import sys,json;print(len(json.load(sys.stdin)))")
assert_eq "Client lists CC channels (both homes)" "2" "$LIST_CH_COUNT"
HOME_HANDLE=$(echo "$LIST_CH" | python3 -c "
import sys,json
chans = json.load(sys.stdin)
mine = [c for c in chans if c['name'] == 'cc-e2e-client']
print(mine[0]['handle'] if mine else '')
")
assert_contains "Channel listing exposes handle, not channel_id" "dwch_" "$HOME_HANDLE"
NO_LEAK=$(echo "$LIST_CH" | grep -c "channel_id" || true)
assert_eq "Channel listing does NOT leak real channel_id" "0" "$NO_LEAK"

# 17m-4: Create a new task channel via the client API
NEW_CH=$(curl -s -X POST -H "X-Duckway-Token: $CC_CLIENT_TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"task-001","topic":"e2e test task"}' "$BASE/client/cc/$CC_ID/channels")
NEW_HANDLE=$(echo "$NEW_CH" | python3 -c "import sys,json;print(json.load(sys.stdin).get('handle',''))")
NEW_NAME=$(echo "$NEW_CH" | python3 -c "import sys,json;print(json.load(sys.stdin).get('name',''))")
assert_contains "Client create channel returns handle" "dwch_" "$NEW_HANDLE"
assert_eq "Client create channel returns name" "task-001" "$NEW_NAME"

# 17m-5: Post a message
POST_RESP=$(curl -s -X POST -H "X-Duckway-Token: $CC_CLIENT_TOKEN" -H "Content-Type: application/json" \
  -d '{"content":"hello from agent"}' "$BASE/client/cc/$CC_ID/channels/$NEW_HANDLE/messages")
MSG_ID=$(echo "$POST_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('message_id',''))")
assert_not_empty "Client post returns message_id" "$MSG_ID"

# 17m-6: Edit the message
EDIT_STATUS=$(curl -s -X PATCH -H "X-Duckway-Token: $CC_CLIENT_TOKEN" -H "Content-Type: application/json" \
  -d '{"content":"edited"}' "$BASE/client/cc/$CC_ID/channels/$NEW_HANDLE/messages/$MSG_ID" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('status',''))")
assert_eq "Client edit returns status" "edited" "$EDIT_STATUS"

# 17m-7: Read recent messages (mock returns [])
READ_OK=$(curl -s -H "X-Duckway-Token: $CC_CLIENT_TOKEN" "$BASE/client/cc/$CC_ID/channels/$NEW_HANDLE/messages?limit=10")
READ_TYPE=$(echo "$READ_OK" | python3 -c "import sys,json;d=json.load(sys.stdin);print('list' if isinstance(d,list) else type(d).__name__)")
assert_eq "Client read returns array" "list" "$READ_TYPE"

# 17m-8: Cannot archive a home channel (must unassign instead)
HOME_ARCHIVE=$(curl -s -X POST -H "X-Duckway-Token: $CC_CLIENT_TOKEN" "$BASE/client/cc/$CC_ID/channels/$HOME_HANDLE/archive" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('error',''))")
assert_contains "Reject archiving a home channel" "home channel" "$HOME_ARCHIVE"

# 17m-9: Archive the task channel we just created
ARCHIVE_OK=$(curl -s -X POST -H "X-Duckway-Token: $CC_CLIENT_TOKEN" "$BASE/client/cc/$CC_ID/channels/$NEW_HANDLE/archive" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('status',''))")
assert_eq "Archive task channel succeeds" "archived" "$ARCHIVE_OK"

# 17m-10: Inbox is empty (gateway disabled in test mode)
INBOX_RESP=$(curl -s -H "X-Duckway-Token: $CC_CLIENT_TOKEN" "$BASE/client/cc/$CC_ID/inbox?timeout=0")
INBOX_EVENTS=$(echo "$INBOX_RESP" | python3 -c "import sys,json;print(len(json.load(sys.stdin).get('events',[])))")
assert_eq "Inbox empty (gateway disabled)" "0" "$INBOX_EVENTS"

# 17m-11: ACL — request without auth
NOAUTH=$(curl -s -o /dev/null -w "%{http_code}" "$BASE/client/cc")
assert_eq "/client/cc rejects unauthenticated" "401" "$NOAUTH"

# 17m-12: ACL — second client tries to access first client's CC
CC_CLIENT3_RESP=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients" -H "Content-Type: application/json" -d '{"name":"cc-e2e-client-3-not-assigned"}')
CC_CLIENT3_TOKEN=$(echo "$CC_CLIENT3_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])")
CC_CLIENT3_ID=$(echo "$CC_CLIENT3_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")
SCOPE_REJECT=$(curl -s -o /dev/null -w "%{http_code}" -H "X-Duckway-Token: $CC_CLIENT3_TOKEN" "$BASE/client/cc/$CC_ID/channels")
assert_eq "Unassigned client → 403 on CC channels" "403" "$SCOPE_REJECT"

# 17m-13: Path-level ACL — handle from outside the CC
# Create a 2nd CC with a different client and a channel under it; then call
# the first client's CC with that foreign handle in the URL.
CC2=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/cc" -H "Content-Type: application/json" \
  -d "{\"name\":\"e2e-cc-2\",\"service_id\":\"$DISCORD_SVC_ID\",\"api_key_id\":\"$CC_BOT_KEY\",\"config\":{\"guild_id\":\"333\",\"category_id\":\"444\"}}")
CC2_ID=$(echo "$CC2" | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))")
ASSIGN2=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients/$CC_CLIENT3_ID/cc" -H "Content-Type: application/json" \
  -d "{\"cc_id\":\"$CC2_ID\",\"agent_type\":\"claude_code\"}")
CC2_HANDLE=$(echo "$ASSIGN2" | python3 -c "import sys,json;print(json.load(sys.stdin).get('home_handle',''))")
# Client1 (assigned to CC1) tries to use CC2's handle — should 403
FOREIGN=$(curl -s -o /dev/null -w "%{http_code}" -X POST -H "X-Duckway-Token: $CC_CLIENT_TOKEN" -H "Content-Type: application/json" \
  -d '{"content":"x"}' "$BASE/client/cc/$CC_ID/channels/$CC2_HANDLE/messages")
assert_eq "Cross-CC handle → 403" "403" "$FOREIGN"

# Cleanup CC2 + client3
curl -s -b /tmp/dw-e2e-cookies -X DELETE "$BASE/api/cc/$CC2_ID" > /dev/null
curl -s -b /tmp/dw-e2e-cookies -X DELETE "$BASE/api/clients/$CC_CLIENT3_ID" > /dev/null


# 17n: Unassign one client + verify CC detail drops to 1, placeholder gone
curl -s -b /tmp/dw-e2e-cookies -X DELETE "$BASE/api/clients/$CC_CLIENT_ID/cc/$CC_ID" > /dev/null
ASN_AFTER=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/cc/$CC_ID" | python3 -c "import sys,json;print(len(json.load(sys.stdin).get('assignments',[])))")
assert_eq "Unassign drops CC assignments to 1" "1" "$ASN_AFTER"

PH_AFTER_UNASSIGN=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/placeholders" | python3 -c "
import sys,json
phs = json.load(sys.stdin)
print(len([p for p in phs if p.get('id') == '$ASSIGN_PHID']))
")
assert_eq "Unassign deletes the placeholder" "0" "$PH_AFTER_UNASSIGN"

# 17o: PUT update CC name + is_active
curl -s -b /tmp/dw-e2e-cookies -X PUT "$BASE/api/cc/$CC_ID" -H "Content-Type: application/json" \
  -d '{"name":"renamed-cc","is_active":false}' > /dev/null
RENAMED=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/cc/$CC_ID" | python3 -c "import sys,json;d=json.load(sys.stdin);print(d['name']+'|'+str(d['is_active']))")
assert_eq "PUT updates name+is_active" "renamed-cc|False" "$RENAMED"

# 17p: Cleanup CC + clients + bot key
curl -s -b /tmp/dw-e2e-cookies -X DELETE "$BASE/api/cc/$CC_ID" > /dev/null
DEL_CHECK=$(curl -s -b /tmp/dw-e2e-cookies -o /dev/null -w "%{http_code}" "$BASE/api/cc/$CC_ID")
assert_eq "Deleted CC returns 404" "404" "$DEL_CHECK"
curl -s -b /tmp/dw-e2e-cookies -X DELETE "$BASE/api/clients/$CC_CLIENT_ID" > /dev/null
curl -s -b /tmp/dw-e2e-cookies -X DELETE "$BASE/api/clients/$CC_CLIENT2" > /dev/null
curl -s -b /tmp/dw-e2e-cookies -X DELETE "$BASE/api/keys/$CC_BOT_KEY" > /dev/null


# === Test 18: CC sync writes state file + Claude Code MCP config (Phase D) ===
echo ""
echo -e "${YELLOW}[18] CC Sync (Phase D)${NC}"

# Re-create a CC + bot key for this section, then assign the *docker* client
# (CLIENT_ID + CLIENT_TOKEN are the e2e-test-client from section [5/8]).
PD_BOT=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/keys" -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$DISCORD_SVC_ID\",\"name\":\"phaseD-bot\",\"key\":\"NzPhase.D.testbot\"}" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))")
PD_CC=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/cc" -H "Content-Type: application/json" \
  -d "{\"name\":\"phaseD-cc\",\"service_id\":\"$DISCORD_SVC_ID\",\"api_key_id\":\"$PD_BOT\",\"config\":{\"guild_id\":\"7\",\"category_id\":\"8\"}}" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))")
curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients/$CLIENT_ID/cc" -H "Content-Type: application/json" \
  -d "{\"cc_id\":\"$PD_CC\",\"agent_type\":\"claude_code\"}" > /dev/null

# Run sync inside the docker client container.
SYNC_PD=$(docker exec duckway-e2e-client duckway sync 2>&1)
assert_contains "Sync logs CC count" "Synced 1 Control Channel" "$SYNC_PD"

# State file: ~/.duckway/cc.json (container's HOME=/root)
CC_JSON=$(docker exec duckway-e2e-client cat /root/.duckway/cc.json 2>/dev/null)
assert_contains "cc.json contains the CC name" "phaseD-cc" "$CC_JSON"
assert_contains "cc.json contains the agent type" "claude_code" "$CC_JSON"
assert_contains "cc.json contains a home handle" "dwch_" "$CC_JSON"
NO_TOKEN_LEAK=$(echo "$CC_JSON" | grep -c "$CLIENT_TOKEN" || true)
assert_eq "cc.json does NOT leak the client token" "0" "$NO_TOKEN_LEAK"

# Claude Code MCP entry: ~/.claude/mcp.json
MCP_JSON=$(docker exec duckway-e2e-client cat /root/.claude/mcp.json 2>/dev/null)
assert_contains "Claude mcp.json has duckway-cc server" "duckway-cc" "$MCP_JSON"
assert_contains "Claude mcp.json command is duckway" "\"command\": \"duckway\"" "$MCP_JSON"
assert_contains "Claude mcp.json args includes mcp serve" "\"mcp\"" "$MCP_JSON"

# Unassign and re-sync — state file should clear.
curl -s -b /tmp/dw-e2e-cookies -X DELETE "$BASE/api/clients/$CLIENT_ID/cc/$PD_CC" > /dev/null
docker exec duckway-e2e-client duckway sync >/dev/null 2>&1
CC_JSON_EMPTY=$(docker exec duckway-e2e-client cat /root/.duckway/cc.json 2>/dev/null)
EMPTY_COUNT=$(echo "$CC_JSON_EMPTY" | python3 -c "import sys,json;print(len(json.load(sys.stdin).get('ccs',[])))")
assert_eq "Re-sync after unassign clears cc.json" "0" "$EMPTY_COUNT"

# Cleanup
curl -s -b /tmp/dw-e2e-cookies -X DELETE "$BASE/api/cc/$PD_CC" > /dev/null
curl -s -b /tmp/dw-e2e-cookies -X DELETE "$BASE/api/keys/$PD_BOT" > /dev/null


# === Test 19: MCP server (Phase E) ===
echo ""
echo -e "${YELLOW}[19] MCP server (Phase E)${NC}"

# Re-create CC + assign so the docker client has something to expose.
PE_BOT=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/keys" -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$DISCORD_SVC_ID\",\"name\":\"phaseE-bot\",\"key\":\"NzPhase.E.testbot\"}" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))")
PE_CC=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/cc" -H "Content-Type: application/json" \
  -d "{\"name\":\"phaseE-cc\",\"service_id\":\"$DISCORD_SVC_ID\",\"api_key_id\":\"$PE_BOT\",\"config\":{\"guild_id\":\"77\",\"category_id\":\"88\"}}" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))")
PE_ASSIGN=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients/$CLIENT_ID/cc" -H "Content-Type: application/json" \
  -d "{\"cc_id\":\"$PE_CC\",\"agent_type\":\"claude_code\"}")
PE_HOME_HANDLE=$(echo "$PE_ASSIGN" | python3 -c "import sys,json;print(json.load(sys.stdin).get('home_handle',''))")
docker exec duckway-e2e-client duckway sync >/dev/null 2>&1

# 19a: tools/list — should expose at least 9 discord_* tools
TOOLS_REQ='{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
TOOLS_OUT=$(echo "$TOOLS_REQ" | docker exec -i duckway-e2e-client duckway mcp serve 2>/dev/null)
TOOL_COUNT=$(echo "$TOOLS_OUT" | python3 -c "import sys,json;print(len(json.load(sys.stdin)['result']['tools']))")
assert_eq "MCP tools/list returns >= 9 tools" "9" "$TOOL_COUNT"

assert_contains "tools/list includes discord_post"          "discord_post"          "$TOOLS_OUT"
assert_contains "tools/list includes discord_create_task"   "discord_create_task_channel" "$TOOLS_OUT"
assert_contains "tools/list includes discord_wait_for_msg"  "discord_wait_for_message"    "$TOOLS_OUT"

# 19b: initialize handshake
INIT_OUT=$(echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' | docker exec -i duckway-e2e-client duckway mcp serve 2>/dev/null)
PROTO_VER=$(echo "$INIT_OUT" | python3 -c "import sys,json;print(json.load(sys.stdin)['result']['protocolVersion'])")
SERVER_NAME=$(echo "$INIT_OUT" | python3 -c "import sys,json;print(json.load(sys.stdin)['result']['serverInfo']['name'])")
assert_eq "MCP initialize protocolVersion" "2024-11-05" "$PROTO_VER"
assert_eq "MCP initialize serverInfo.name" "duckway-cc" "$SERVER_NAME"

# 19c: discord_list_assigned_ccs — should show the phaseE-cc
LIST_CC_OUT=$(echo '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"discord_list_assigned_ccs","arguments":{}}}' \
  | docker exec -i duckway-e2e-client duckway mcp serve 2>/dev/null)
assert_contains "discord_list_assigned_ccs includes phaseE-cc" "phaseE-cc" "$LIST_CC_OUT"

# 19d: discord_list_channels — should round-trip through duckway server and
# return the home channel only.
LIST_CH_OUT=$(echo "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"discord_list_channels\",\"arguments\":{\"cc_id\":\"$PE_CC\"}}}" \
  | docker exec -i duckway-e2e-client duckway mcp serve 2>/dev/null)
assert_contains "discord_list_channels routes through server" "$PE_HOME_HANDLE" "$LIST_CH_OUT"
NO_RAW_ID=$(echo "$LIST_CH_OUT" | grep -c "channel_id" || true)
assert_eq "discord_list_channels does NOT leak channel_id" "0" "$NO_RAW_ID"

# 19e: discord_post — exercise the full tool→server→Discord-mock chain
POST_OUT=$(echo "{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"tools/call\",\"params\":{\"name\":\"discord_post\",\"arguments\":{\"cc_id\":\"$PE_CC\",\"channel_handle\":\"$PE_HOME_HANDLE\",\"content\":\"hello via mcp\"}}}" \
  | docker exec -i duckway-e2e-client duckway mcp serve 2>/dev/null)
assert_contains "discord_post returns message_id" "message_id" "$POST_OUT"

# 19f: ambiguous cc (assign a 2nd CC, drop cc_id) → isError
PE_BOT2=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/keys" -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$DISCORD_SVC_ID\",\"name\":\"phaseE-bot2\",\"key\":\"NzPhase.E.testbot2\"}" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))")
PE_CC2=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/cc" -H "Content-Type: application/json" \
  -d "{\"name\":\"phaseE-cc-2\",\"service_id\":\"$DISCORD_SVC_ID\",\"api_key_id\":\"$PE_BOT2\",\"config\":{\"guild_id\":\"99\",\"category_id\":\"100\"}}" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))")
ASSIGN2_OUT=$(curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/clients/$CLIENT_ID/cc" -H "Content-Type: application/json" \
  -d "{\"cc_id\":\"$PE_CC2\",\"agent_type\":\"claude_code\"}")
ASSIGN2_STATUS=$(echo "$ASSIGN2_OUT" | python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('status') or d.get('error',''))")
assert_eq "Assign CC2 to client succeeds" "assigned" "$ASSIGN2_STATUS"
docker exec duckway-e2e-client duckway sync >/dev/null 2>&1

CC_JSON_DEBUG=$(docker exec duckway-e2e-client cat /root/.duckway/cc.json 2>/dev/null | python3 -c "import sys,json;print(len(json.load(sys.stdin).get('ccs',[])))")
assert_eq "cc.json has 2 CCs after re-sync" "2" "$CC_JSON_DEBUG"

AMBIG_OUT=$(echo '{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"discord_list_channels","arguments":{}}}' \
  | docker exec -i duckway-e2e-client duckway mcp serve 2>/dev/null)
IS_ERR=$(echo "$AMBIG_OUT" | python3 -c "import sys,json;print(json.load(sys.stdin)['result'].get('isError', False))")
if [ "$IS_ERR" != "True" ]; then
  echo "DEBUG ambig response: $AMBIG_OUT"
fi
assert_eq "Ambiguous cc → isError" "True" "$IS_ERR"

# Cleanup
curl -s -b /tmp/dw-e2e-cookies -X DELETE "$BASE/api/cc/$PE_CC" > /dev/null
curl -s -b /tmp/dw-e2e-cookies -X DELETE "$BASE/api/cc/$PE_CC2" > /dev/null
curl -s -b /tmp/dw-e2e-cookies -X DELETE "$BASE/api/keys/$PE_BOT" > /dev/null
curl -s -b /tmp/dw-e2e-cookies -X DELETE "$BASE/api/keys/$PE_BOT2" > /dev/null


# === Test 15: Unit Tests ===
echo ""
echo -e "${YELLOW}[15] Unit Tests${NC}"

UNIT=$(go test ./internal/server/services/ ./internal/database/queries/ ./internal/server/handlers/ ./internal/client/ 2>&1)
UNIT_OK=$(echo "$UNIT" | grep -c "^ok" || true)
UNIT_FAIL=$(echo "$UNIT" | grep -c "^FAIL" || true)
if [ "$UNIT_FAIL" = "0" ] && [ "$UNIT_OK" -ge "1" ]; then
  PASS=$((PASS + 1))
  echo -e "  ${GREEN}PASS${NC} Unit tests pass ($UNIT_OK packages)"
else
  FAIL=$((FAIL + 1))
  echo -e "  ${RED}FAIL${NC} Unit tests failed"
  ERRORS="$ERRORS\n  - Unit tests: $UNIT"
fi


# === Cleanup ===
echo ""
echo -e "${YELLOW}[Cleanup]${NC}"
cleanup
echo "  Done"

# === Summary ===
echo ""
echo "============================================"
TOTAL=$((PASS + FAIL))
if [ "$FAIL" -eq 0 ]; then
  echo -e " ${GREEN}ALL $TOTAL TESTS PASSED${NC}"
else
  echo -e " ${RED}$FAIL/$TOTAL TESTS FAILED${NC}"
  echo -e " Failures:$ERRORS"
fi
echo "============================================"

exit $FAIL
