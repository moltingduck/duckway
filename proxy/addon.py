"""
Duckway mitmproxy addon.

Intercepts HTTPS traffic, detects placeholder keys (containing 'dw_'),
resolves them to real API keys via the Duckway server, and replaces them
in the outgoing request.

Usage:
    mitmdump -s proxy/addon.py

Environment:
    DUCKWAY_SERVER_URL    - Go server URL (default: http://localhost:8080)
    DUCKWAY_INTERNAL_SECRET - Shared secret for /internal/resolve
"""

import json
import os
import re
import logging

from mitmproxy import http, ctx

logger = logging.getLogger("duckway")

# Pattern to detect placeholder keys: prefix + dw_ + hex chars
PLACEHOLDER_RE = re.compile(
    r"(sk-proj-|sk-ant-|sk-|ghp_|ghs_|gho_|xoxb-|xoxp-|AKIA|glpat-)"
    r"dw_[a-f0-9]+"
)


class DuckwayAddon:
    def __init__(self):
        self.server_url = os.environ.get("DUCKWAY_SERVER_URL", "http://localhost:8080")
        self.internal_secret = os.environ.get("DUCKWAY_INTERNAL_SECRET", "duckway-internal-default")
        self._session = None

    @property
    def session(self):
        if self._session is None:
            import requests
            self._session = requests.Session()
        return self._session

    def request(self, flow: http.HTTPFlow):
        """Called for every intercepted request."""
        # Extract client ID from proxy auth header (stripped before forwarding)
        client_id = flow.request.headers.pop("X-Duckway-Client-ID", None)
        if not client_id:
            return  # Not a Duckway-managed request

        # Scan headers for placeholder keys
        replacements = []
        for name, value in flow.request.headers.items():
            for match in PLACEHOLDER_RE.finditer(value):
                placeholder = match.group(0)
                resolved = self._resolve(placeholder, client_id, flow.request.host)
                if resolved:
                    replacements.append((name, placeholder, resolved))

        # Apply header replacements
        for header_name, placeholder, real_key in replacements:
            old_value = flow.request.headers[header_name]
            flow.request.headers[header_name] = old_value.replace(placeholder, real_key)
            logger.info(f"Replaced key in header '{header_name}' for host {flow.request.host}")

        # Scan request body for placeholder keys (JSON APIs)
        if flow.request.content:
            body = flow.request.content.decode("utf-8", errors="ignore")
            body_modified = False
            for match in PLACEHOLDER_RE.finditer(body):
                placeholder = match.group(0)
                resolved = self._resolve(placeholder, client_id, flow.request.host)
                if resolved:
                    body = body.replace(placeholder, resolved)
                    body_modified = True
                    logger.info(f"Replaced key in body for host {flow.request.host}")

            if body_modified:
                flow.request.content = body.encode("utf-8")

    def _resolve(self, placeholder: str, client_id: str, target_host: str) -> str | None:
        """Call the Go server's /internal/resolve endpoint."""
        try:
            resp = self.session.post(
                f"{self.server_url}/internal/resolve",
                json={
                    "placeholder": placeholder,
                    "client_id": client_id,
                    "target_host": target_host,
                },
                headers={"X-Internal-Secret": self.internal_secret},
                timeout=5,
            )
            if resp.status_code != 200:
                logger.warning(f"Resolve failed ({resp.status_code}): {resp.text}")
                return None

            data = resp.json()
            if data.get("permitted"):
                return data["real_key"]
            elif data.get("need_approval"):
                logger.info(f"Approval needed for placeholder {placeholder[:20]}...")
                return None
            else:
                logger.warning(f"Resolve denied: {data.get('error', 'unknown')}")
                return None

        except Exception as e:
            logger.error(f"Resolve error: {e}")
            return None


addons = [DuckwayAddon()]
