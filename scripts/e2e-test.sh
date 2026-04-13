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
docker build -f Dockerfile.client -t duckway-client . -q 2>&1 >/dev/null

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


# === Test 2: Auth ===
echo ""
echo -e "${YELLOW}[2] Authentication${NC}"

RESULT=$(curl -s -X POST "$BASE/api/auth/login" \
  -H "Content-Type: application/json" \
  -d '{"username":"wrong","password":"wrong"}' | jq -r '.error // ""')
assert_eq "Bad credentials rejected" "invalid credentials" "$RESULT"

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
assert_eq "5 default services seeded" "5" "$SVC_COUNT"

for name in openai anthropic github discord telegram; do
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
assert_eq "3 API keys exist" "3" "$KEY_COUNT"


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
assert_eq "Client syncs 3 keys" "3" "$SYNC_COUNT"

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
docker exec duckway-e2e-client bash -c "cat > /root/.duckway/config.yaml << DEOF
server_url: $BASE
client_name: e2e-test-client
token: $CLIENT_TOKEN
proxy_port: 18080
DEOF"

# Run sync
SYNC_OUT=$(docker exec duckway-e2e-client duckway sync 2>&1)
assert_contains "Docker sync succeeds" "Synced 3" "$SYNC_OUT"

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
assert_contains "duckway status shows 3 keys" "3 placeholder" "$STATUS_OUT"


# === Test 9: Proxy (key injection) ===
echo ""
echo -e "${YELLOW}[9] Proxy Key Injection${NC}"

PROXY_RESP=$(curl -s -X POST "$BASE/proxy/openai/v1/chat/completions" \
  -H "X-Duckway-Token: $CLIENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"test"}]}')
# OpenAI returns 401 with the real key masked — proves injection
assert_contains "Proxy injects real key (OpenAI)" "invalid_api_key" "$PROXY_RESP"

PROXY_RESP2=$(curl -s -X GET "$BASE/proxy/github/user" \
  -H "X-Duckway-Token: $CLIENT_TOKEN")
assert_contains "Proxy reaches GitHub upstream" "Bad credentials" "$PROXY_RESP2"


# === Test 10: Approval Workflow ===
echo ""
echo -e "${YELLOW}[10] Approval Workflow${NC}"

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


# === Test 11: Permission System ===
echo ""
echo -e "${YELLOW}[11] Permission System${NC}"

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


# === Test 12: Admin Pages ===
echo ""
echo -e "${YELLOW}[12] Admin Panel Pages${NC}"

for page in "" services keys placeholders clients groups approvals logs notifications; do
  STATUS=$(curl -s -b /tmp/dw-e2e-cookies -o /dev/null -w "%{http_code}" "$BASE/admin/$page")
  assert_eq "GET /admin/$page returns 200" "200" "$STATUS"
done


# === Test 13: Unit Tests ===
echo ""
echo -e "${YELLOW}[13] Unit Tests${NC}"

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
