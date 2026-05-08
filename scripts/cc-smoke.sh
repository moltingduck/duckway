#!/usr/bin/env bash
# scripts/cc-smoke.sh — real-Discord smoke test for the Control Channels
# feature. Drives a live duckway-server through the full lifecycle:
# create CC, run commands, exercise the daemon, verify channels appear
# in Discord, then clean up.
#
# REQUIREMENTS:
#   1. duckway-server running and reachable.
#   2. A Discord bot you own + a guild it's invited to + a category it
#      can manage. Bot needs MANAGE_CHANNELS, SEND_MESSAGES,
#      ADD_REACTIONS, READ_MESSAGE_HISTORY, MESSAGE_CONTENT INTENT.
#   3. The `claude` binary in PATH if you want to drive the daemon.
#
# USAGE:
#   export CC_SMOKE_BOT_TOKEN=...           (required)
#   export CC_SMOKE_GUILD_ID=...            (required)
#   export CC_SMOKE_CATEGORY_ID=...         (required)
#   export CC_SMOKE_BASE=http://localhost:9090   (default)
#   export CC_SMOKE_ADMIN_USER=duckway            (default)
#   export CC_SMOKE_ADMIN_PASS=duckway             (default)
#   ./scripts/cc-smoke.sh
#
# What it does (in order):
#   1. Login to admin
#   2. Upload bot token under service=discord
#   3. Create a fresh client + a fresh CC bound to that client
#   4. POST /api/cc/{id}/test (round-trip channel create+delete)
#   5. Print the management channel name + handle
#   6. (optional, if --watch passed) start `duckway cc watch` against
#      the new client, send a test message via the inject endpoint
#   7. Cleanup: delete CC + client + bot key

set -e

BASE="${CC_SMOKE_BASE:-http://localhost:9090}"
USER="${CC_SMOKE_ADMIN_USER:-duckway}"
PASS="${CC_SMOKE_ADMIN_PASS:-duckway}"
COOKIES=/tmp/dw-cc-smoke-cookies

if [ -z "$CC_SMOKE_BOT_TOKEN" ] || [ -z "$CC_SMOKE_GUILD_ID" ] || [ -z "$CC_SMOKE_CATEGORY_ID" ]; then
  echo "Missing env vars. See header for required vars." >&2
  exit 1
fi

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m'

log()  { printf "${YELLOW}[smoke]${NC} %s\n" "$*"; }
ok()   { printf "  ${GREEN}OK${NC}  %s\n" "$*"; }
die()  { printf "  ${RED}FAIL${NC} %s\n" "$*" >&2; exit 1; }

cleanup() {
  echo
  log "Cleanup..."
  if [ -n "$CC_ID" ]; then
    curl -s -b "$COOKIES" -X DELETE "$BASE/api/cc/$CC_ID" >/dev/null && ok "deleted CC $CC_ID" || true
  fi
  if [ -n "$CLIENT_ID" ]; then
    curl -s -b "$COOKIES" -X DELETE "$BASE/api/clients/$CLIENT_ID" >/dev/null && ok "deleted client $CLIENT_ID" || true
  fi
  if [ -n "$KEY_ID" ]; then
    curl -s -b "$COOKIES" -X DELETE "$BASE/api/keys/$KEY_ID" >/dev/null && ok "deleted bot key $KEY_ID" || true
  fi
  rm -f "$COOKIES"
}
trap cleanup EXIT

# ---- 1. login ----
log "Login to $BASE as $USER..."
LOGIN_CODE=$(curl -s -c "$COOKIES" -o /dev/null -w "%{http_code}" \
  -X POST "$BASE/api/auth/login" -H 'Content-Type: application/json' \
  -d "{\"username\":\"$USER\",\"password\":\"$PASS\"}")
[ "$LOGIN_CODE" = "200" ] || die "login returned $LOGIN_CODE"
ok "logged in"

DISCORD_SVC=$(curl -s -b "$COOKIES" "$BASE/api/services" \
  | python3 -c "import sys,json;print([s['id'] for s in json.load(sys.stdin) if s['name']=='discord'][0])")
[ -n "$DISCORD_SVC" ] || die "could not find 'discord' service id"

# ---- 2. upload bot token ----
log "Upload bot token..."
KEY_RESP=$(curl -s -b "$COOKIES" -X POST "$BASE/api/keys" \
  -H 'Content-Type: application/json' \
  -d "{\"service_id\":\"$DISCORD_SVC\",\"name\":\"cc-smoke-$(date +%s)\",\"key\":\"$CC_SMOKE_BOT_TOKEN\"}")
KEY_ID=$(echo "$KEY_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))")
[ -n "$KEY_ID" ] || die "key upload failed: $KEY_RESP"
ok "bot token stored as key $KEY_ID"

# ---- 3. create client + CC ----
log "Create test client..."
CLIENT_RESP=$(curl -s -b "$COOKIES" -X POST "$BASE/api/clients" \
  -H 'Content-Type: application/json' -d "{\"name\":\"cc-smoke-$(date +%s)\"}")
CLIENT_ID=$(echo "$CLIENT_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))")
CLIENT_TOKEN=$(echo "$CLIENT_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('token',''))")
[ -n "$CLIENT_ID" ] || die "client create failed: $CLIENT_RESP"
ok "client $CLIENT_ID"

log "Create CC (calls Discord to provision management channel)..."
CC_RESP=$(curl -s -b "$COOKIES" -X POST "$BASE/api/cc" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"cc-smoke\",\"client_id\":\"$CLIENT_ID\",\"agent_type\":\"claude_code\",\"service_id\":\"$DISCORD_SVC\",\"api_key_id\":\"$KEY_ID\",\"config\":{\"guild_id\":\"$CC_SMOKE_GUILD_ID\",\"category_id\":\"$CC_SMOKE_CATEGORY_ID\"}}")
CC_ID=$(echo "$CC_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('id',''))")
if [ -z "$CC_ID" ]; then
  die "CC create failed: $CC_RESP"
fi
MGMT_NAME=$(echo "$CC_RESP" | python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('channels',[{}])[0].get('name',''))")
MGMT_HANDLE=$(echo "$CC_RESP" | python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('channels',[{}])[0].get('handle',''))")
ok "CC $CC_ID — management channel '#$MGMT_NAME' (handle $MGMT_HANDLE)"
echo "    👀 Go check Discord — '#$MGMT_NAME' should now exist under your category."

# ---- 4. test endpoint ----
log "Round-trip test (create+delete a temp channel)..."
TEST_RESP=$(curl -s -b "$COOKIES" -X POST "$BASE/api/cc/$CC_ID/test")
TEST_OK=$(echo "$TEST_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('ok'))")
if [ "$TEST_OK" != "True" ]; then
  echo "$TEST_RESP" | python3 -m json.tool
  die "test endpoint reports failure"
fi
ok "test passed (4 steps: decrypt + parse + create + delete)"

# ---- 5. exercise client API ----
log "Exercise /client/cc/* endpoints..."
LIST=$(curl -s -H "X-Duckway-Token: $CLIENT_TOKEN" "$BASE/client/cc/channels" \
  | python3 -c "import sys,json;d=json.load(sys.stdin);print(len(d) if isinstance(d,list) else -1)")
[ "$LIST" = "1" ] || die "expected 1 channel (management), got $LIST"
ok "/client/cc/channels lists 1 channel"

POST_RESP=$(curl -s -X POST -H "X-Duckway-Token: $CLIENT_TOKEN" -H 'Content-Type: application/json' \
  -d '{"content":"smoke test ✅"}' "$BASE/client/cc/channels/$MGMT_HANDLE/messages")
MID=$(echo "$POST_RESP" | python3 -c "import sys,json;print(json.load(sys.stdin).get('message_id',''))")
[ -n "$MID" ] || die "post failed: $POST_RESP"
ok "posted message $MID to management channel"
echo "    👀 Check '#$MGMT_NAME' — you should see 'smoke test ✅'."

# ---- 6. (optional) daemon round-trip ----
if [ "$1" = "--watch" ]; then
  echo
  log "Daemon round-trip (--watch mode)"
  log "Starting 'duckway cc watch' for client $CLIENT_ID..."
  echo "  Set DUCKWAY_CONFIG_DIR to point at a temp dir, then run:"
  TMPDIR=$(mktemp -d -t cc-smoke-XXXXXX)
  cat > "$TMPDIR/config.yaml" <<EOF
server_url: $BASE
client_name: cc-smoke
token: $CLIENT_TOKEN
proxy_port: 18080
EOF
  echo "    DUCKWAY_CONFIG_DIR=$TMPDIR duckway cc watch &"
  echo "  Then in Discord, post a message in '#$MGMT_NAME' (or in a task"
  echo "  channel created via '!new test-1') and watch claude respond."
  echo
  echo "  When done: kill the daemon, then re-run this script without --watch"
  echo "  to exercise just the cleanup."
  echo
  echo "  Skipping cleanup so the channel + state persist for your test."
  trap - EXIT
  exit 0
fi

# ---- 7. cleanup happens on exit (trap) ----
echo
log "All smoke checks passed."
echo "    👀 The Discord channels will be archived (renamed to 'archived-…')"
echo "       on the next line as cleanup runs."
