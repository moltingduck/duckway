#!/usr/bin/env python3
"""Delete one exact smoke-owned Discord category and all of its children."""

import json
import os
import sys
import time
import urllib.error
import urllib.request


TOKEN = os.environ["CC_SMOKE_BOT_TOKEN"]
GUILD_ID = os.environ["CC_SMOKE_GUILD_ID"]
CATEGORY_ID = os.environ["CC_SMOKE_TEMP_CATEGORY_ID"]
CATEGORY_NAME = os.environ["CC_SMOKE_TEMP_CATEGORY_NAME"]
API = os.environ.get("CC_SMOKE_DISCORD_API", "https://discord.com/api/v10").rstrip("/")


def request(method: str, path: str):
    req = urllib.request.Request(
        API + path,
        method=method,
        headers={"Authorization": "Bot " + TOKEN, "User-Agent": "Duckway-CC-Smoke/1"},
    )
    for attempt in range(5):
        try:
            with urllib.request.urlopen(req, timeout=20) as response:
                body = response.read()
                return response.status, json.loads(body) if body else None
        except urllib.error.HTTPError as error:
            body = error.read()
            if error.code == 429 and attempt < 4:
                try:
                    retry_after = float(json.loads(body).get("retry_after", 1))
                except (ValueError, TypeError, json.JSONDecodeError):
                    retry_after = 1
                time.sleep(min(max(retry_after, 0.05), 5))
                continue
            if error.code == 404:
                return 404, None
            raise RuntimeError(f"Discord {method} {path} returned HTTP {error.code}") from error
    raise RuntimeError(f"Discord {method} {path} exhausted rate-limit retries")


_, channels = request("GET", f"/guilds/{GUILD_ID}/channels")
category = next((channel for channel in channels if channel.get("id") == CATEGORY_ID), None)
if category is None:
    print(f"temporary category {CATEGORY_ID} is already absent")
    raise SystemExit(0)
if category.get("type") != 4 or category.get("name") != CATEGORY_NAME:
    raise SystemExit(
        f"refusing cleanup: {CATEGORY_ID} is not the expected category {CATEGORY_NAME!r}"
    )

children = [channel for channel in channels if channel.get("parent_id") == CATEGORY_ID]
for channel in children:
    request("DELETE", f"/channels/{channel['id']}")
    print(f"deleted temporary channel {channel['id']}")
request("DELETE", f"/channels/{CATEGORY_ID}")

status, _ = request("GET", f"/channels/{CATEGORY_ID}")
if status != 404:
    raise SystemExit(f"temporary category {CATEGORY_ID} still exists after cleanup")
_, remaining = request("GET", f"/guilds/{GUILD_ID}/channels")
if any(channel.get("id") == CATEGORY_ID or channel.get("parent_id") == CATEGORY_ID for channel in remaining):
    raise SystemExit("temporary Discord resources remain after cleanup")
print(f"deleted temporary category {CATEGORY_ID}")
