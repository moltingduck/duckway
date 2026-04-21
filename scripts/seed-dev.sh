#!/bin/sh
set -e

# Dev seed script — populates the Duckway server with test data (20+ items)
# Run after server is up: ./scripts/seed-dev.sh

BASE="${DUCKWAY_URL:-http://127.0.0.1:9090}"
PW="${DUCKWAY_ADMIN_PW:-duckway}"

# Discord dev bot (from env)
DISCORD_BOT_TOKEN="${DISCORD_BOT_TOKEN:-}"
DISCORD_CHANNEL_ID="${DISCORD_CHANNEL_ID:-}"

echo "Seeding dev data on $BASE..."

# Login
curl -s -c /tmp/dw-seed-cookies -X POST "$BASE/api/auth/login" \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"duckway\",\"password\":\"$PW\"}" > /dev/null

# Get service IDs
OPENAI_ID=$(curl -s -b /tmp/dw-seed-cookies "$BASE/api/services" | python3 -c "import sys,json;print([s['id'] for s in json.load(sys.stdin) if s['name']=='openai'][0])" 2>/dev/null)
ANTHROPIC_ID=$(curl -s -b /tmp/dw-seed-cookies "$BASE/api/services" | python3 -c "import sys,json;print([s['id'] for s in json.load(sys.stdin) if s['name']=='anthropic'][0])" 2>/dev/null)
GITHUB_ID=$(curl -s -b /tmp/dw-seed-cookies "$BASE/api/services" | python3 -c "import sys,json;print([s['id'] for s in json.load(sys.stdin) if s['name']=='github'][0])" 2>/dev/null)
DISCORD_ID=$(curl -s -b /tmp/dw-seed-cookies "$BASE/api/services" | python3 -c "import sys,json;print([s['id'] for s in json.load(sys.stdin) if s['name']=='discord'][0])" 2>/dev/null)
TELEGRAM_ID=$(curl -s -b /tmp/dw-seed-cookies "$BASE/api/services" | python3 -c "import sys,json;print([s['id'] for s in json.load(sys.stdin) if s['name']=='telegram'][0])" 2>/dev/null)

# Check if already seeded
EXISTING_KEYS=$(curl -s -b /tmp/dw-seed-cookies "$BASE/api/keys" | python3 -c "import sys,json;print(len(json.load(sys.stdin)))" 2>/dev/null)
if [ "$EXISTING_KEYS" -gt 2 ] 2>/dev/null; then
  echo "Already seeded ($EXISTING_KEYS keys). Skipping."
  rm -f /tmp/dw-seed-cookies
  exit 0
fi

# Helper
add_key() {
  curl -s -b /tmp/dw-seed-cookies -X POST "$BASE/api/keys" \
    -H "Content-Type: application/json" \
    -d "{\"service_id\":\"$1\",\"name\":\"$2\",\"key\":\"$3\"}" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])" 2>/dev/null
}

add_client() {
  curl -s -b /tmp/dw-seed-cookies -X POST "$BASE/api/clients" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"$1\"}"
}

add_placeholder() {
  curl -s -b /tmp/dw-seed-cookies -X POST "$BASE/api/placeholders" \
    -H "Content-Type: application/json" \
    -d "{\"service_id\":\"$1\",\"api_key_id\":\"$2\",\"client_id\":\"$3\",\"requires_approval\":$4}" > /dev/null
}

echo "Adding API keys (8)..."
OA_KEY1=$(add_key "$OPENAI_ID" "OpenAI Production" "sk-proj-fake-prod-openai-key-1234567890abcdef1234567890")
OA_KEY2=$(add_key "$OPENAI_ID" "OpenAI Staging" "sk-proj-fake-staging-openai-key-abcdef1234567890abcdef")
OA_KEY3=$(add_key "$OPENAI_ID" "OpenAI Batch" "sk-proj-fake-batch-openai-key-9876543210fedcba9876543210")
AN_KEY1=$(add_key "$ANTHROPIC_ID" "Anthropic Production" "sk-ant-fake-prod-anthropic-key-1234567890abcdef")
AN_KEY2=$(add_key "$ANTHROPIC_ID" "Anthropic Dev" "sk-ant-fake-dev-anthropic-key-abcdef1234567890")
GH_KEY1=$(add_key "$GITHUB_ID" "GitHub Deploy Bot" "ghp_fakeDeployBotToken1234567890abcd")
GH_KEY2=$(add_key "$GITHUB_ID" "GitHub CI Runner" "ghp_fakeCIRunnerToken9876543210wxyz")
GH_KEY3=$(add_key "$GITHUB_ID" "GitHub Read-Only" "ghp_fakeReadOnlyToken5555666677778888")
echo "  8 API keys created"

echo "Creating clients (6)..."
C1=$(add_client "dev-laptop")
C1_ID=$(echo "$C1" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")
C1_TOKEN=$(echo "$C1" | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])")

C2=$(add_client "ci-runner-01")
C2_ID=$(echo "$C2" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")
C2_TOKEN=$(echo "$C2" | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])")

C3=$(add_client "ci-runner-02")
C3_ID=$(echo "$C3" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")

C4=$(add_client "staging-server")
C4_ID=$(echo "$C4" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")

C5=$(add_client "prod-worker-01")
C5_ID=$(echo "$C5" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")

C6=$(add_client "claude-agent")
C6_ID=$(echo "$C6" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")

echo "  6 clients created"

echo "Assigning placeholder keys (18)..."
# dev-laptop: all services, auto-approve
add_placeholder "$OPENAI_ID" "$OA_KEY1" "$C1_ID" "false"
add_placeholder "$ANTHROPIC_ID" "$AN_KEY1" "$C1_ID" "false"
add_placeholder "$GITHUB_ID" "$GH_KEY1" "$C1_ID" "false"

# ci-runner-01: openai + github, approval required 1h
add_placeholder "$OPENAI_ID" "$OA_KEY2" "$C2_ID" "true"
add_placeholder "$GITHUB_ID" "$GH_KEY2" "$C2_ID" "true"

# ci-runner-02: openai batch, approval 30min
curl -s -b /tmp/dw-seed-cookies -X POST "$BASE/api/placeholders" \
  -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$OPENAI_ID\",\"api_key_id\":\"$OA_KEY3\",\"client_id\":\"$C3_ID\",\"requires_approval\":true,\"approval_ttl_minutes\":30}" > /dev/null

# staging-server: openai + anthropic + github, auto
add_placeholder "$OPENAI_ID" "$OA_KEY1" "$C4_ID" "false"
add_placeholder "$ANTHROPIC_ID" "$AN_KEY2" "$C4_ID" "false"
add_placeholder "$GITHUB_ID" "$GH_KEY1" "$C4_ID" "false"

# prod-worker-01: openai + anthropic, approval 24h
add_placeholder "$OPENAI_ID" "$OA_KEY1" "$C5_ID" "true"
add_placeholder "$ANTHROPIC_ID" "$AN_KEY1" "$C5_ID" "true"

# claude-agent: all services, auto
add_placeholder "$OPENAI_ID" "$OA_KEY1" "$C6_ID" "false"
add_placeholder "$ANTHROPIC_ID" "$AN_KEY1" "$C6_ID" "false"
add_placeholder "$GITHUB_ID" "$GH_KEY3" "$C6_ID" "false"

echo "  18 placeholder keys assigned"

# Generate some proxy requests for the log
echo "Generating request log entries..."
for i in 1 2 3 4 5; do
  curl -s -X POST "$BASE/proxy/openai/v1/chat/completions" \
    -H "X-Duckway-Token: $C1_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4o","messages":[{"role":"user","content":"test"}]}' > /dev/null
done
curl -s -X GET "$BASE/proxy/heartbeat/ping" -H "X-Duckway-Token: $C1_TOKEN" > /dev/null
echo "  6 log entries"

# Discord bot notification
if [ -n "$DISCORD_BOT_TOKEN" ] && [ -n "$DISCORD_CHANNEL_ID" ]; then
  echo "Adding Discord bot notification..."
  CONFIG=$(python3 -c "import json;print(json.dumps({'bot_token':'$DISCORD_BOT_TOKEN','channel_id':'$DISCORD_CHANNEL_ID'}))")
  curl -s -b /tmp/dw-seed-cookies -X POST "$BASE/api/notifications" \
    -H "Content-Type: application/json" \
    -d "{\"channel_type\":\"discord_bot\",\"name\":\"duckway-dev\",\"config\":$(python3 -c "import json;print(json.dumps('$CONFIG'))" 2>/dev/null || echo "\"$CONFIG\"")}" > /dev/null
  echo "  Discord bot: channel $DISCORD_CHANNEL_ID"
else
  echo "Skipping Discord bot (set DISCORD_BOT_TOKEN + DISCORD_CHANNEL_ID env vars)"
fi

echo ""
echo "=== Dev seed complete ==="
echo "  8 API keys | 6 clients | 18 placeholders | 6 log entries"
echo ""
echo "Clients:"
echo "  dev-laptop    token: $C1_TOKEN"
echo "  ci-runner-01  token: $C2_TOKEN"

rm -f /tmp/dw-seed-cookies
