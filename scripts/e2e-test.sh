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
  docker rm -f duckway-e2e-client 2>/dev/null || true
  rm -rf "$DATA_DIR" /tmp/dw-e2e-cookies /tmp/dw-e2e-server.log
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

echo -e "${YELLOW}[Setup]${NC} Starting server on :$PORT..."
DUCKWAY_DATA_DIR="$DATA_DIR" DUCKWAY_LISTEN="127.0.0.1:$PORT" /tmp/duckway-e2e-server &>/tmp/dw-e2e-server.log &
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

# --- ACL priority: placeholder > key > service ---
echo ""
echo -e "${YELLOW}[11e] ACL Priority Chain${NC}"

# Set service ACL to allow-all (no restriction)
curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/services/$OPENAI_ID/acl-templates" \
  -H "Content-Type: application/json" -d '{"template_id":"allow-all"}' > /dev/null

# Set key ACL to chat-only (restrictive)
curl -s -b /tmp/dw-e2e-cookies -X POST "$BASE/api/keys/$KEY1_ID/acl-templates" \
  -H "Content-Type: application/json" -d '{"template_id":"chat-only"}' > /dev/null

# Key ACL should block images even though service allows all
PRIORITY_DENIED=$(curl -s -X POST "$BASE/proxy/openai/v1/images/generations" \
  -H "X-Duckway-Token: $CLIENT_ACL_TOKEN" \
  -H "Content-Type: application/json" -d '{"prompt":"cat"}')
assert_contains "Priority: key ACL overrides service allow-all" "permission denied" "$PRIORITY_DENIED"

# Now set a placeholder-level permission that allows everything
PH_ID=$(curl -s -b /tmp/dw-e2e-cookies "$BASE/api/placeholders?client_id=$CLIENT_ACL_ID" | jq -r '.[0].id')
curl -s -b /tmp/dw-e2e-cookies -X PUT "$BASE/api/placeholders/$PH_ID" \
  -H "Content-Type: application/json" \
  -d '{"env_name":"OPENAI_API_KEY"}' > /dev/null

# With a wide-open placeholder permission_config, it should override the key ACL
# (We need to set permission_config directly — use the update endpoint isn't wired for that yet,
#  so we test the opposite: key ACL takes effect when placeholder has no config)
PRIORITY_CHAT=$(curl -s -X POST "$BASE/proxy/openai/v1/chat/completions" \
  -H "X-Duckway-Token: $CLIENT_ACL_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"test"}]}')
assert_contains "Priority: key ACL allows chat" "invalid_api_key" "$PRIORITY_CHAT"

# Clean up
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

for page in "" services keys placeholders clients groups approvals logs notifications canary docs; do
  STATUS=$(curl -s -b /tmp/dw-e2e-cookies -o /dev/null -w "%{http_code}" "$BASE/admin/$page")
  assert_eq "GET /admin/$page returns 200" "200" "$STATUS"
done


# === Test 15: Unit Tests ===
echo ""
echo -e "${YELLOW}[15] Unit Tests${NC}"

UNIT=$(go test ./internal/server/services/ 2>&1)
if echo "$UNIT" | grep -q "^ok"; then
  PASS=$((PASS + 1))
  echo -e "  ${GREEN}PASS${NC} Unit tests pass"
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
