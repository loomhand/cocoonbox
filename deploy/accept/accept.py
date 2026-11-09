"""What the acceptance scripts share: PASS and FAIL lines, HTTP to the api, and one MCP Streamable HTTP session."""

import json
import os
import urllib.error
import urllib.request
from email.message import Message

ENDPOINT = os.environ["CB_ENDPOINT"].rstrip("/")

failures: list[str] = []


class MCP:
    """One MCP Streamable HTTP session at a path, as a bearer and an optional guest token."""

    def __init__(self, path: str, bearer: str, guest: str | None = None):
        self.path, self.bearer, self.guest, self.session, self.next = path, bearer, guest, None, 0

    def headers(self) -> dict[str, str]:
        h = {"Authorization": "Bearer " + self.bearer}
        if self.guest:
            h["X-Cocoonbox-Guest-Token"] = self.guest
        if self.session:
            h["Mcp-Session-Id"] = self.session
        return h

    def post(self, method: str, params: dict | None = None) -> tuple[int, list[dict]]:
        """Sends one request; returns the status and the messages of the JSON or SSE answer."""
        self.next += 1
        msg = {"jsonrpc": "2.0", "id": self.next, "method": method, "params": params or {}}
        status, headers, raw = http("POST", self.path, msg, self.headers())
        if method == "initialize" and status == 200:
            self.session = headers["Mcp-Session-Id"]
            http("POST", self.path, {"jsonrpc": "2.0", "method": "notifications/initialized"}, self.headers())
        text = raw.decode(errors="replace")
        if text.startswith(("event:", "data:")):
            return status, [json.loads(line[5:]) for line in text.splitlines() if line.startswith("data:")]
        return status, [json.loads(text)] if text.strip() else []

    def open(self, client: str) -> int:
        return self.post(
            "initialize",
            {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": client, "version": "1"}},
        )[0]


def check(ok: object, what: str) -> None:
    print(("PASS " if ok else "FAIL ") + what, flush=True)
    if not ok:
        failures.append(what)


def note(text: str) -> None:
    print("     " + text, flush=True)


def http(
    method: str, url: str, body: object = None, headers: dict[str, str] | None = None
) -> tuple[int, Message, bytes]:
    """Sends a request to url, or to the api when url is a path."""
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url if url.startswith("http") else ENDPOINT + url, data=data, method=method)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    if data is not None:
        req.add_header("Content-Type", "application/json")
        req.add_header("Accept", "application/json, text/event-stream")
    try:
        with urllib.request.urlopen(req, timeout=900) as resp:
            return resp.status, resp.headers, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read()
