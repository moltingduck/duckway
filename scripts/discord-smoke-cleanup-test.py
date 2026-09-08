#!/usr/bin/env python3
import json
import os
import pathlib
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


ROOT = pathlib.Path(__file__).resolve().parent.parent


class DiscordFixture(BaseHTTPRequestHandler):
    channels = []
    deletes = []

    def log_message(self, *_args):
        pass

    def do_GET(self):
        if self.path == "/guilds/GUILD/channels":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(self.channels).encode())
            return
        if self.path.startswith("/channels/"):
            channel_id = self.path.rsplit("/", 1)[-1]
            channel = next((item for item in self.channels if item["id"] == channel_id), None)
            self.send_response(200 if channel else 404)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(channel or {"message": "Unknown Channel"}).encode())
            return
        self.send_error(404)

    def do_DELETE(self):
        channel_id = self.path.rsplit("/", 1)[-1]
        self.deletes.append(channel_id)
        self.channels = [item for item in self.channels if item["id"] != channel_id]
        type(self).channels = self.channels
        self.send_response(204)
        self.end_headers()


server = ThreadingHTTPServer(("127.0.0.1", 0), DiscordFixture)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
try:
    DiscordFixture.channels = [
        {"id": "CAT", "name": "duckway-cc-smoke-test", "type": 4, "parent_id": None},
        {"id": "ONE", "name": "one", "type": 0, "parent_id": "CAT"},
        {"id": "TWO", "name": "two", "type": 0, "parent_id": "CAT"},
        {"id": "KEEP", "name": "keep", "type": 0, "parent_id": None},
    ]
    DiscordFixture.deletes = []
    env = os.environ.copy()
    env.update(
        CC_SMOKE_BOT_TOKEN="fixture-token-must-not-leak",
        CC_SMOKE_GUILD_ID="GUILD",
        CC_SMOKE_TEMP_CATEGORY_ID="CAT",
        CC_SMOKE_TEMP_CATEGORY_NAME="duckway-cc-smoke-test",
        CC_SMOKE_DISCORD_API=f"http://127.0.0.1:{server.server_port}",
    )
    result = subprocess.run(
        [str(ROOT / "scripts/discord-smoke-cleanup.py")],
        env=env,
        text=True,
        capture_output=True,
        check=True,
    )
    assert DiscordFixture.deletes == ["ONE", "TWO", "CAT"], DiscordFixture.deletes
    assert [item["id"] for item in DiscordFixture.channels] == ["KEEP"]
    assert "fixture-token-must-not-leak" not in result.stdout + result.stderr

    DiscordFixture.channels = [
        {"id": "CAT", "name": "not-the-smoke-category", "type": 4, "parent_id": None}
    ]
    DiscordFixture.deletes = []
    refused = subprocess.run(
        [str(ROOT / "scripts/discord-smoke-cleanup.py")],
        env=env,
        text=True,
        capture_output=True,
    )
    assert refused.returncode != 0
    assert DiscordFixture.deletes == []
finally:
    server.shutdown()
    server.server_close()
    thread.join()

print("[discord-smoke-cleanup] PASS")
