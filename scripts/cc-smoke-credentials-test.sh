#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d -t dw-cc-smoke-credentials-test-XXXXXX)"
trap 'rm -rf "$TMP"' EXIT
CREDENTIALS="$TMP/discord-bot.json"

printf '%s\n' '{"bot_token":"fixture-secret-token","guild_id":"guild-from-file","category_id":"category-from-file"}' >"$CREDENTIALS"
chmod 600 "$CREDENTIALS"

output="$(env -u CC_SMOKE_BOT_TOKEN -u CC_SMOKE_GUILD_ID -u CC_SMOKE_CATEGORY_ID \
  CC_SMOKE_CREDENTIALS="$CREDENTIALS" "$ROOT/scripts/cc-smoke.sh" --check-credentials)"
grep -Fq "source: $CREDENTIALS" <<<"$output"
grep -Fq "guild: guild-from-file" <<<"$output"
grep -Fq "category: category-from-file" <<<"$output"
if grep -Fq "fixture-secret-token" <<<"$output"; then
  echo "credential check leaked the Discord bot token" >&2
  exit 1
fi

output="$(CC_SMOKE_BOT_TOKEN=environment-token CC_SMOKE_GUILD_ID=guild-from-env \
  CC_SMOKE_CATEGORY_ID=category-from-env CC_SMOKE_CREDENTIALS="$CREDENTIALS" \
  "$ROOT/scripts/cc-smoke.sh" --check-credentials)"
grep -Fq "source: environment" <<<"$output"
grep -Fq "guild: guild-from-env" <<<"$output"
grep -Fq "category: category-from-env" <<<"$output"

chmod 644 "$CREDENTIALS"
if env -u CC_SMOKE_BOT_TOKEN -u CC_SMOKE_GUILD_ID -u CC_SMOKE_CATEGORY_ID \
  CC_SMOKE_CREDENTIALS="$CREDENTIALS" "$ROOT/scripts/cc-smoke.sh" --check-credentials >"$TMP/insecure.out" 2>&1; then
  echo "credential check accepted an insecure file" >&2
  exit 1
fi
grep -Fq "permissions 644" "$TMP/insecure.out"

echo "[cc-smoke-credentials] PASS"
