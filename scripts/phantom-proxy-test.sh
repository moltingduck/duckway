#!/bin/bash
# Phantom-token end-to-end test against real upstream APIs.
#
# For each service whose real key is in env, this script:
#   1. Uploads the real key to Duckway (encrypted server-side)
#   2. Registers a fresh test client
#   3. Generates a phantom token for client+service (auto-approved)
#   4. Calls /proxy/<service>/<endpoint> with ONLY the client token
#      — the real key is never sent by the script
#   5. Verifies upstream returned success → phantom→real swap works
#   6. Cleans up the test client and uploaded key
#
# Required env (set the ones you want to test, others are skipped):
#   OPENAI_API_KEY     sk-...
#   ANTHROPIC_API_KEY  sk-ant-api03-...
#   GITHUB_TOKEN       github_pat_...   (only read:user scope needed)
#   DISCORD_BOT_TOKEN  bot token
#
# Optional env:
#   DUCKWAY_URL        Duckway server URL (default: http://127.0.0.1:9090)
#   DUCKWAY_ADMIN_PW   Admin password (default: duckway)
#
# Usage:
#   export OPENAI_API_KEY=sk-... ANTHROPIC_API_KEY=sk-ant-...
#   ./scripts/phantom-proxy-test.sh
#
# OR drop the same exports into scripts/phantom-test.env (gitignored)
# and the script will auto-source it on startup. Example:
#   cat > scripts/phantom-test.env <<'EOF'
#   export OPENAI_API_KEY=sk-...
#   export ANTHROPIC_API_KEY=sk-ant-...
#   export GITHUB_TOKEN=github_pat_...
#   export DISCORD_BOT_TOKEN=...
#   EOF
#   chmod 600 scripts/phantom-test.env

set -e

# Auto-load credentials from scripts/phantom-test.env if present.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ENV_FILE="$SCRIPT_DIR/phantom-test.env"
if [ -f "$ENV_FILE" ]; then
  echo "Loading credentials from $ENV_FILE"
  set -a
  . "$ENV_FILE"
  set +a
fi

BASE="${DUCKWAY_URL:-http://127.0.0.1:9090}"
PW="${DUCKWAY_ADMIN_PW:-duckway}"
RUN_ID="phantom-test-$(date +%s)"
COOKIES=/tmp/dw-pt-cookies
RESP=/tmp/dw-pt-resp

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[0;33m'
NC='\033[0m'

PASS=0
FAIL=0
SKIP=0
ERRORS=""

cleanup() {
  rm -f "$COOKIES" "$RESP"
}
trap cleanup EXIT

# --- Login ---
echo "Logging into ${BASE} as duckway..."
HTTP=$(curl -s -o /dev/null -w "%{http_code}" -c "$COOKIES" -X POST "$BASE/api/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"duckway\",\"password\":\"$PW\"}")
if [ "$HTTP" != "200" ]; then
  echo "Login failed (HTTP $HTTP). Set DUCKWAY_ADMIN_PW or check that ${BASE} is reachable."
  exit 1
fi

get_svc_id() {
  curl -s -b "$COOKIES" "$BASE/api/services" | \
    python3 -c "import sys,json;print([s['id'] for s in json.load(sys.stdin) if s['name']=='$1'][0])" 2>/dev/null
}

# Run a single service test.
# Args: svc_name, svc_id, real_key, method, upstream_path, [extra_header], [body]
run_test() {
  local svc_name="$1"
  local svc_id="$2"
  local real_key="$3"
  local method="$4"
  local path="$5"
  local extra_header="${6:-}"
  local body="${7:-}"

  if [ -z "$real_key" ]; then
    SKIP=$((SKIP+1))
    printf "${YELLOW}SKIP${NC} %-10s no key in env\n" "$svc_name"
    return
  fi
  if [ -z "$svc_id" ]; then
    FAIL=$((FAIL+1))
    ERRORS="$ERRORS\n  - $svc_name: service not found in Duckway"
    printf "${RED}FAIL${NC} %-10s service id lookup failed\n" "$svc_name"
    return
  fi

  printf "      %-10s uploading key...                  \r" "$svc_name"

  # 1. Upload key
  KEY_ID=$(curl -s -b "$COOKIES" -X POST "$BASE/api/keys" \
    -H "Content-Type: application/json" \
    -d "{\"service_id\":\"$svc_id\",\"name\":\"${RUN_ID}-${svc_name}\",\"key\":\"$real_key\"}" \
    | python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('id',''))" 2>/dev/null)
  if [ -z "$KEY_ID" ]; then
    FAIL=$((FAIL+1))
    ERRORS="$ERRORS\n  - $svc_name: key upload failed"
    printf "${RED}FAIL${NC} %-10s key upload failed                  \n" "$svc_name"
    return
  fi

  # 2. Register a fresh client
  CLIENT_JSON=$(curl -s -b "$COOKIES" -X POST "$BASE/api/clients" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"${RUN_ID}-${svc_name}\"}")
  CLIENT_ID=$(echo "$CLIENT_JSON" | python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('id',''))" 2>/dev/null)
  CLIENT_TOKEN=$(echo "$CLIENT_JSON" | python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('token',''))" 2>/dev/null)
  if [ -z "$CLIENT_ID" ] || [ -z "$CLIENT_TOKEN" ]; then
    curl -s -b "$COOKIES" -X DELETE "$BASE/api/keys/$KEY_ID" > /dev/null
    FAIL=$((FAIL+1))
    ERRORS="$ERRORS\n  - $svc_name: client registration failed"
    printf "${RED}FAIL${NC} %-10s client registration failed         \n" "$svc_name"
    return
  fi

  # 3. Assign phantom (no approval needed)
  curl -s -b "$COOKIES" -X POST "$BASE/api/placeholders" \
    -H "Content-Type: application/json" \
    -d "{\"service_id\":\"$svc_id\",\"api_key_id\":\"$KEY_ID\",\"client_id\":\"$CLIENT_ID\",\"requires_approval\":false}" > /dev/null

  # 4. Make the proxied request — note: NO real key sent, only client token
  printf "      %-10s calling /proxy/%s%s...                  \r" "$svc_name" "$svc_name" "$path"
  if [ "$method" = "POST" ]; then
    STATUS=$(curl -s -o "$RESP" -w "%{http_code}" -X POST "$BASE/proxy/${svc_name}${path}" \
      -H "X-Duckway-Token: $CLIENT_TOKEN" \
      -H "Content-Type: application/json" \
      ${extra_header:+-H "$extra_header"} \
      -d "$body")
  else
    STATUS=$(curl -s -o "$RESP" -w "%{http_code}" "$BASE/proxy/${svc_name}${path}" \
      -H "X-Duckway-Token: $CLIENT_TOKEN" \
      ${extra_header:+-H "$extra_header"})
  fi

  # 5. Verify
  if [ "$STATUS" = "200" ]; then
    PASS=$((PASS+1))
    # Extract a useful field from the upstream response to prove it really hit upstream
    PROOF=$(python3 -c "
import sys,json
try:
    d = json.load(open('$RESP'))
    if 'data' in d and isinstance(d['data'], list): print('models='+str(len(d['data'])))
    elif 'login' in d: print('user='+d['login'])
    elif 'username' in d: print('bot='+d['username'])
    elif 'id' in d: print('id='+str(d['id'])[:12])
    else: print('ok')
except: print('ok')
" 2>/dev/null)
    printf "${GREEN}PASS${NC} %-10s upstream 200  (%s)                        \n" "$svc_name" "$PROOF"
  else
    FAIL=$((FAIL+1))
    BODY=$(head -c 200 "$RESP" 2>/dev/null)
    ERRORS="$ERRORS\n  - $svc_name: upstream returned $STATUS — $BODY"
    printf "${RED}FAIL${NC} %-10s upstream %s                        \n" "$svc_name" "$STATUS"
    echo "      response: $BODY"
  fi

  # 6. Cleanup
  curl -s -b "$COOKIES" -X DELETE "$BASE/api/clients/$CLIENT_ID" > /dev/null
  curl -s -b "$COOKIES" -X DELETE "$BASE/api/keys/$KEY_ID" > /dev/null
}

# --- Resolve service IDs ---
OPENAI_ID=$(get_svc_id openai)
ANTHROPIC_ID=$(get_svc_id anthropic)
GITHUB_ID=$(get_svc_id github)
DISCORD_ID=$(get_svc_id discord)

# --- Tests ---
echo ""
echo "============================================"
echo "  Phantom token proxy tests"
echo "  Server: $BASE"
echo "============================================"
echo ""

# OpenAI: /v1/models is free, returns model list
run_test "openai" "$OPENAI_ID" "$OPENAI_API_KEY" \
  "GET" "/v1/models"

# Anthropic: /v1/models needs anthropic-version header
run_test "anthropic" "$ANTHROPIC_ID" "$ANTHROPIC_API_KEY" \
  "GET" "/v1/models" "anthropic-version: 2023-06-01"

# GitHub: /user returns authenticated user info
run_test "github" "$GITHUB_ID" "$GITHUB_TOKEN" \
  "GET" "/user"

# Discord: /api/v10/users/@me returns bot info. Service URL already
# includes /api so we hit /v10/users/@me as the path.
run_test "discord" "$DISCORD_ID" "$DISCORD_BOT_TOKEN" \
  "GET" "/v10/users/@me"

# Telegram skipped — its API embeds the token in the URL path, which
# doesn't fit the phantom-in-header model. Would need a path-substitution
# feature to support cleanly.

# ====================================================================
# loan_proxy end-to-end test: real GitHub PAT through duckway-client
# MITM proxy (HTTPS_PROXY → sidecar → cached real token → upstream).
# Proves the full sidecar flow, not just the gateway endpoints.
# Skipped if GITHUB_TOKEN isn't set or duckway-client container isn't running.
# ====================================================================

run_loan_proxy_github_test() {
  if [ -z "$GITHUB_TOKEN" ]; then
    SKIP=$((SKIP+1))
    printf "${YELLOW}SKIP${NC} %-10s no GITHUB_TOKEN for loan_proxy MITM test\n" "github-loan"
    return
  fi
  if ! docker ps --format '{{.Names}}' | grep -qx 'duckway-client'; then
    SKIP=$((SKIP+1))
    printf "${YELLOW}SKIP${NC} %-10s duckway-client container not running\n" "github-loan"
    printf "      %-10s start it with: docker compose --profile client up -d client\n" "github-loan"
    return
  fi
  if [ -z "$GITHUB_ID" ]; then
    SKIP=$((SKIP+1))
    printf "${YELLOW}SKIP${NC} %-10s github service not found\n" "github-loan"
    return
  fi

  printf "      %-10s setting up loan_proxy test...                     \r" "github-loan"

  # Make sure the github service is in loan_proxy mode. GitHub defaults to
  # simple phantom proxy mode; this test explicitly exercises the optional
  # direct-upstream loan path.
  curl -s -b "$COOKIES" -X PUT "$BASE/api/services/$GITHUB_ID" \
    -H "Content-Type: application/json" \
    -d '{"delivery_mode":"loan_proxy","host_pattern":"api.github.com,github.com"}' > /dev/null

  # 1. Upload the real key
  KEY_ID=$(curl -s -b "$COOKIES" -X POST "$BASE/api/keys" \
    -H "Content-Type: application/json" \
    -d "{\"service_id\":\"$GITHUB_ID\",\"name\":\"${RUN_ID}-loan\",\"key\":\"$GITHUB_TOKEN\"}" \
    | python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('id',''))" 2>/dev/null)
  if [ -z "$KEY_ID" ]; then
    FAIL=$((FAIL+1))
    printf "${RED}FAIL${NC} %-10s could not upload github key                    \n" "github-loan"
    return
  fi

  # 2. Register a fresh client
  CLIENT_JSON=$(curl -s -b "$COOKIES" -X POST "$BASE/api/clients" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"${RUN_ID}-loan\"}")
  CLIENT_ID=$(echo "$CLIENT_JSON" | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
  CLIENT_TOKEN=$(echo "$CLIENT_JSON" | python3 -c "import sys,json;print(json.load(sys.stdin).get('token',''))" 2>/dev/null)

  # 3. Bind a phantom for github
  curl -s -b "$COOKIES" -X POST "$BASE/api/placeholders" \
    -H "Content-Type: application/json" \
    -d "{\"service_id\":\"$GITHUB_ID\",\"api_key_id\":\"$KEY_ID\",\"client_id\":\"$CLIENT_ID\",\"requires_approval\":false}" > /dev/null

  # 4. Configure duckway-client and start the proxy daemon inside it
  printf "      %-10s starting duckway proxy in container...           \r" "github-loan"
  docker exec duckway-client sh -c '
    rm -f /root/.duckway/config.yaml /root/.duckway/proxy.pid
    mkdir -p /root/.duckway
  ' > /dev/null

  docker exec duckway-client sh -c "
    echo 'server_url: http://duckway-server:8080' > /root/.duckway/config.yaml
    echo 'token: $CLIENT_TOKEN' >> /root/.duckway/config.yaml
    echo 'client_name: ${RUN_ID}-loan' >> /root/.duckway/config.yaml
    echo 'proxy_port: 18080' >> /root/.duckway/config.yaml
  " > /dev/null

  # CA cert is required for the MITM. duckway sync doesn't fetch it; pull
  # it from the server's public /skill/ca.pem and the auth'd /client/ca-key.
  docker exec duckway-client pkill -9 -f 'duckway proxy' >/dev/null 2>&1 || true
  docker exec duckway-client rm -f /root/.duckway/proxy.pid /root/.duckway/proxy.log >/dev/null 2>&1
  docker exec duckway-client sh -c "curl -sf -o /root/.duckway/ca.pem http://duckway-server:8080/skill/ca.pem" >/dev/null 2>&1
  docker exec duckway-client sh -c "curl -sf -H 'X-Duckway-Token: $CLIENT_TOKEN' -o /root/.duckway/ca-key.pem http://duckway-server:8080/client/ca-key" >/dev/null 2>&1
  docker exec duckway-client duckway sync >/dev/null 2>&1
  docker exec duckway-client duckway proxy -d >/dev/null 2>&1
  sleep 2

  # 5. Make a request through HTTPS_PROXY to api.github.com (loan_proxy MITM)
  #    Phantom token is sent as Authorization Bearer; duckway-client should
  #    swap it for the real one via /client/loan.
  printf "      %-10s curl https://api.github.com/user via MITM...     \r" "github-loan"

  PHANTOM=$(docker exec duckway-client sh -c '. /root/.duckway/keys.env 2>/dev/null && echo "$GITHUB_TOKEN"' | tr -d "\r\n")

  # Run curl in the container, status into one file, body into another.
  # `set +e` locally so curl timeouts don't trip the surrounding `set -e`.
  set +e
  docker exec duckway-client sh -c "
    curl -s --max-time 20 -o /tmp/loan-resp -w '%{http_code}' \
      --cacert /root/.duckway/ca.pem \
      -x http://127.0.0.1:18080 \
      -H 'Authorization: Bearer $PHANTOM' \
      'https://api.github.com/user' > /tmp/loan-status 2>&1
    echo \"curl_exit=\$?\" > /tmp/loan-exit
  " >/dev/null 2>&1
  set -e

  STATUS=$(docker exec duckway-client cat /tmp/loan-status 2>/dev/null | tr -d '\r\n ')
  CURL_EXIT=$(docker exec duckway-client cat /tmp/loan-exit 2>/dev/null | sed 's/.*=//')

  if [ "$STATUS" = "200" ]; then
    PASS=$((PASS+1))
    LOGIN=$(docker exec duckway-client cat /tmp/loan-resp 2>/dev/null | python3 -c "
import sys,json
try: print(json.load(sys.stdin).get('login','?'))
except: print('?')
" 2>/dev/null)
    printf "${GREEN}PASS${NC} %-12s loan_proxy MITM works  (login=%s)                    \n" "github-loan" "$LOGIN"
  else
    FAIL=$((FAIL+1))
    BODY=$(docker exec duckway-client cat /tmp/loan-resp 2>/dev/null | head -c 200 | tr '\n' ' ')
    ERRORS="$ERRORS\n  - github-loan: status=$STATUS curl_exit=$CURL_EXIT body=$BODY"
    printf "${RED}FAIL${NC} %-12s loan_proxy status=%s curl_exit=%s                  \n" "github-loan" "$STATUS" "$CURL_EXIT"
  fi

  # 6. Cleanup
  docker exec duckway-client duckway proxy stop 2>/dev/null > /dev/null || true
  curl -s -b "$COOKIES" -X DELETE "$BASE/api/clients/$CLIENT_ID" > /dev/null
  curl -s -b "$COOKIES" -X DELETE "$BASE/api/keys/$KEY_ID" > /dev/null
}

run_loan_proxy_github_test

echo ""
echo "============================================"
if [ "$FAIL" -eq 0 ]; then
  echo -e "  ${GREEN}${PASS} passed${NC}, ${SKIP} skipped"
else
  echo -e "  ${PASS} passed, ${RED}${FAIL} failed${NC}, ${SKIP} skipped"
  echo -e "${ERRORS}"
fi
echo "============================================"

[ "$FAIL" -eq 0 ]
