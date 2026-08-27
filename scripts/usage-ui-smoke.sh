#!/usr/bin/env bash
set -euo pipefail

PORT="${1:-19191}"
BASE="http://127.0.0.1:${PORT}"
TMP="$(mktemp -d)"
SERVER="$TMP/duckway-server"
COOKIE="$TMP/cookies"
LOG="$TMP/server.log"

cleanup() {
  if [[ -n "${SERVER_PID:-}" ]]; then kill "$SERVER_PID" 2>/dev/null || true; fi
  rm -rf "$TMP"
}
trap cleanup EXIT

need() { command -v "$1" >/dev/null || { echo "missing dependency: $1" >&2; exit 1; }; }
need curl
need jq

go build -o "$SERVER" ./cmd/server
DUCKWAY_DATA_DIR="$TMP/data" DUCKWAY_LISTEN="127.0.0.1:$PORT" "$SERVER" >"$LOG" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 50); do
  curl -fsS "$BASE/admin/login" >/dev/null 2>&1 && break
  sleep .1
done
curl -fsS "$BASE/admin/login" >/dev/null

PASSWORD="$(sed -n 's/.*Password: //p' "$LOG" | head -1)"
[[ -n "$PASSWORD" ]] || { echo "admin password not found" >&2; cat "$LOG" >&2; exit 1; }
curl -fsS -c "$COOKIE" -H 'Content-Type: application/json' -d "{\"username\":\"duckway\",\"password\":\"$PASSWORD\"}" "$BASE/api/auth/login" | jq -e '.status == "ok"' >/dev/null

SERVICES="$(curl -fsS -b "$COOKIE" "$BASE/api/services")"
OPENAI_ID="$(jq -r '.[] | select(.name=="openai") | .id' <<<"$SERVICES")"
[[ -n "$OPENAI_ID" ]]

KEY="$(curl -fsS -b "$COOKIE" -H 'Content-Type: application/json' -d "{\"service_id\":\"$OPENAI_ID\",\"name\":\"usage-smoke\",\"key\":\"sk-smoke-test-not-real\"}" "$BASE/api/keys")"
KEY_ID="$(jq -r '.id' <<<"$KEY")"
[[ -n "$KEY_ID" && "$KEY_ID" != null ]]

DETAIL="$(curl -fsS -b "$COOKIE" "$BASE/api/usage/detail?key_id=$KEY_ID&days=90")"
jq -e '.window_days == 90 and .summary.total_tokens == 0 and (.clients | type == "array")' <<<"$DETAIL" >/dev/null

PRICE="$(curl -fsS -b "$COOKIE" -H 'Content-Type: application/json' -d '{"model":"gpt-smoke","version":"smoke-v1","input_usd_micros_per_mtok":1000000,"output_usd_micros_per_mtok":2000000}' "$BASE/api/services/$OPENAI_ID/pricing")"
jq -e '.model == "gpt-smoke" and .version == "smoke-v1"' <<<"$PRICE" >/dev/null
curl -fsS -b "$COOKIE" "$BASE/api/services/$OPENAI_ID/pricing" | jq -e 'any(.[]; .model == "gpt-smoke" and .input_usd_micros_per_mtok == 1000000)' >/dev/null

API_KEYS_PAGE="$(curl -fsS -b "$COOKIE" "$BASE/admin/keys")"
OAUTH_PAGE="$(curl -fsS -b "$COOKIE" "$BASE/admin/oauth")"
SERVICES_PAGE="$(curl -fsS -b "$COOKIE" "$BASE/admin/services")"
USAGE_PAGE="$(curl -fsS -b "$COOKIE" "$BASE/admin/usage?key_id=$KEY_ID")"
grep -q 'LLM Tokens (90d)' <<<"$API_KEYS_PAGE"
grep -q 'View usage details' <<<"$OAUTH_PAGE"
grep -q 'Model Pricing' <<<"$SERVICES_PAGE"
grep -q 'key_group_id' <<<"$USAGE_PAGE"
grep -q 'usage-heatmap' <<<"$USAGE_PAGE"

echo "usage UI smoke test passed"
