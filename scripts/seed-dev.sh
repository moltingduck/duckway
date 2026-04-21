#!/bin/sh
set -e

# Dev seed script — populates the Duckway server with test data
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

# Check if already seeded
EXISTING_KEYS=$(curl -s -b /tmp/dw-seed-cookies "$BASE/api/keys" | python3 -c "import sys,json;print(len(json.load(sys.stdin)))" 2>/dev/null)
if [ "$EXISTING_KEYS" -gt 1 ] 2>/dev/null; then
  echo "Already seeded ($EXISTING_KEYS keys). Skipping."
  rm -f /tmp/dw-seed-cookies
  exit 0
fi

echo "Adding API keys..."
OA_KEY=$(curl -s -b /tmp/dw-seed-cookies -X POST "$BASE/api/keys" \
  -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$OPENAI_ID\",\"name\":\"OpenAI Dev\",\"key\":\"sk-proj-fake-dev-openai-key-1234567890abcdef1234567890\"}" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")

AN_KEY=$(curl -s -b /tmp/dw-seed-cookies -X POST "$BASE/api/keys" \
  -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$ANTHROPIC_ID\",\"name\":\"Anthropic Dev\",\"key\":\"sk-ant-fake-dev-anthropic-key-1234567890abcdef\"}" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")

GH_KEY=$(curl -s -b /tmp/dw-seed-cookies -X POST "$BASE/api/keys" \
  -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$GITHUB_ID\",\"name\":\"GitHub Dev\",\"key\":\"ghp_fakeDevGitHubToken1234567890abcd\"}" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")

echo "  OpenAI: $OA_KEY"
echo "  Anthropic: $AN_KEY"
echo "  GitHub: $GH_KEY"

echo "Creating clients..."
CLIENT1=$(curl -s -b /tmp/dw-seed-cookies -X POST "$BASE/api/clients" \
  -H "Content-Type: application/json" \
  -d '{"name":"dev-laptop"}')
C1_ID=$(echo "$CLIENT1" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")
C1_TOKEN=$(echo "$CLIENT1" | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])")

CLIENT2=$(curl -s -b /tmp/dw-seed-cookies -X POST "$BASE/api/clients" \
  -H "Content-Type: application/json" \
  -d '{"name":"ci-runner"}')
C2_ID=$(echo "$CLIENT2" | python3 -c "import sys,json;print(json.load(sys.stdin)['id'])")
C2_TOKEN=$(echo "$CLIENT2" | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])")

echo "  dev-laptop: $C1_ID"
echo "  ci-runner: $C2_ID"

echo "Assigning placeholder keys..."
# dev-laptop: all 3 services, no approval
for pair in "$OPENAI_ID:$OA_KEY" "$ANTHROPIC_ID:$AN_KEY" "$GITHUB_ID:$GH_KEY"; do
  SID=$(echo "$pair" | cut -d: -f1)
  KID=$(echo "$pair" | cut -d: -f2)
  curl -s -b /tmp/dw-seed-cookies -X POST "$BASE/api/placeholders" \
    -H "Content-Type: application/json" \
    -d "{\"service_id\":\"$SID\",\"api_key_id\":\"$KID\",\"client_id\":\"$C1_ID\",\"requires_approval\":false}" > /dev/null
done
echo "  dev-laptop: 3 keys (auto-approve)"

# ci-runner: openai only, requires approval
curl -s -b /tmp/dw-seed-cookies -X POST "$BASE/api/placeholders" \
  -H "Content-Type: application/json" \
  -d "{\"service_id\":\"$OPENAI_ID\",\"api_key_id\":\"$OA_KEY\",\"client_id\":\"$C2_ID\",\"requires_approval\":true,\"approval_ttl_minutes\":60}" > /dev/null
echo "  ci-runner: 1 key (approval required, 1h TTL)"

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
echo "Clients:"
echo "  dev-laptop token: $C1_TOKEN"
echo "  ci-runner  token: $C2_TOKEN"

rm -f /tmp/dw-seed-cookies
