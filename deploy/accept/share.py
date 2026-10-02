#!/usr/bin/env python3
"""Acceptance of box sharing (design §23.3) against a deployed api and a v18 box, run on bench.

Users qa1, qa2 and qa3 exist; qa1 has a box. Their console passwords come from QA1_PASSWORD, QA2_PASSWORD and
QA3_PASSWORD; the script signs in, issues fresh API tokens for the three scratch users and never prints a secret.
"""

import base64
import json
import os
import socket
import ssl
import struct
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path
from urllib.parse import urlsplit

ENDPOINT = os.environ.get("CB_ENDPOINT", "https://box.example.com").rstrip("/")
PROTO = Path(__file__).resolve().parents[2] / "proto"
READ_ONLY = sorted(["screenshot", "computer_state", "read_file", "list_dir", "browser_snapshot", "browser_read"])
CUT_LIMIT = 1.0
POINTER = r"""
import ctypes, ctypes.util
x = ctypes.CDLL(ctypes.util.find_library("X11"))
x.XOpenDisplay.restype = ctypes.c_void_p
x.XDefaultRootWindow.restype = ctypes.c_ulong
x.XDefaultRootWindow.argtypes = [ctypes.c_void_p]
x.XQueryPointer.argtypes = [ctypes.c_void_p, ctypes.c_ulong] + [ctypes.c_void_p] * 7
d = x.XOpenDisplay(b":0")
w, c = ctypes.c_ulong(), ctypes.c_ulong()
rx, ry, wx, wy, m = ctypes.c_int(), ctypes.c_int(), ctypes.c_int(), ctypes.c_int(), ctypes.c_uint()
x.XQueryPointer(d, x.XDefaultRootWindow(d), *[ctypes.byref(v) for v in (w, c, rx, ry, wx, wy, m)])
print(rx.value, ry.value)
"""

failures = []


def check(ok, what):
    print(("PASS " if ok else "FAIL ") + what, flush=True)
    if not ok:
        failures.append(what)


def note(text):
    print("     " + text, flush=True)


def http(method, path, body=None, headers=None, cookie=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(ENDPOINT + path, data=data, method=method)
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    if data is not None:
        req.add_header("Content-Type", "application/json")
        req.add_header("Accept", "application/json, text/event-stream")
    if cookie:
        req.add_header("Cookie", "cb_session=" + cookie)
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            return resp.status, resp.headers, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read()


class Console:
    def __init__(self, name):
        status, headers, body = http("POST", "/cb/api/login", {"name": name, "password": os.environ[name.upper() + "_PASSWORD"]})
        if status != 200:
            sys.exit(f"sign-in of {name} failed: {status} {body[:200]!r}")
        self.name = name
        self.cookie = headers["Set-Cookie"].split(";", 1)[0].split("=", 1)[1]

    def call(self, method, path, body=None):
        status, _, raw = http(method, "/cb/api/" + path, None if method == "GET" else (body or {}), cookie=self.cookie)
        return status, (json.loads(raw) if raw.strip() else None)

    def token(self):
        status, out = self.call("POST", "me/token")
        if status != 200:
            sys.exit(f"token of {self.name}: {status}")
        return out["token"]


class MCP:
    """One MCP Streamable HTTP session at a path, as a bearer and an optional guest token."""

    def __init__(self, path, bearer, guest=None):
        self.path, self.bearer, self.guest, self.session, self.next = path, bearer, guest, None, 0

    def headers(self):
        h = {"Authorization": "Bearer " + self.bearer}
        if self.guest:
            h["X-Cocoonbox-Guest-Token"] = self.guest
        if self.session:
            h["Mcp-Session-Id"] = self.session
        return h

    def post(self, method, params=None):
        self.next += 1
        msg = {"jsonrpc": "2.0", "id": self.next, "method": method, "params": params or {}}
        status, headers, raw = http("POST", self.path, msg, self.headers())
        if method == "initialize" and status == 200:
            self.session = headers["Mcp-Session-Id"]
            http("POST", self.path, {"jsonrpc": "2.0", "method": "notifications/initialized"}, self.headers())
        text = raw.decode(errors="replace")
        if text.startswith("event:") or text.startswith("data:"):
            text = [line[5:] for line in text.splitlines() if line.startswith("data:")][-1]
        return status, (json.loads(text) if text.strip() else None)

    def open(self, client="share-accept"):
        return self.post("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": client, "version": "1"}})[0]

    def tools(self):
        status, reply = self.post("tools/list")
        return status, sorted(t["name"] for t in (reply or {}).get("result", {}).get("tools", []))

    def call(self, name, arguments):
        status, reply = self.post("tools/call", {"name": name, "arguments": arguments})
        return status, (reply or {})

    def stream(self):
        """Opens the GET server stream; returns a Watch that records when it ends."""
        return Watch(lambda: raw_stream(self.path, self.headers()))


def raw_stream(path, headers):
    u = urlsplit(ENDPOINT)
    sock = ssl.create_default_context().wrap_socket(socket.create_connection((u.hostname, u.port or 443)), server_hostname=u.hostname)
    lines = [f"GET {path} HTTP/1.1", f"Host: {u.hostname}", "Accept: text/event-stream", "Connection: close"]
    lines += [f"{k}: {v}" for k, v in headers.items()]
    sock.sendall(("\r\n".join(lines) + "\r\n\r\n").encode())
    return sock


class Watch:
    """Reads a socket in the background until EOF and keeps the time it ended."""

    def __init__(self, connect):
        self.sock = connect()
        self.ended = None
        self.data = b""
        threading.Thread(target=self.run, daemon=True).start()

    def run(self):
        try:
            while True:
                chunk = self.sock.recv(65536)
                if not chunk:
                    break
                self.data += chunk
        except OSError:
            pass
        self.ended = time.time()

    def wait(self, limit):
        deadline = time.time() + limit
        while self.ended is None and time.time() < deadline:
            time.sleep(0.02)
        return self.ended


class Viewer:
    """The share page's side of the WebSocket: the secret first, then RFB as binary messages."""

    def __init__(self, secret, cookie=None):
        u = urlsplit(ENDPOINT)
        self.sock = ssl.create_default_context().wrap_socket(socket.create_connection((u.hostname, u.port or 443)), server_hostname=u.hostname)
        key = base64.b64encode(os.urandom(16)).decode()
        lines = [
            "GET /cb/s/ws HTTP/1.1", f"Host: {u.hostname}", "Upgrade: websocket", "Connection: Upgrade",
            f"Sec-WebSocket-Key: {key}", "Sec-WebSocket-Version: 13", f"Origin: {ENDPOINT}",
        ]
        if cookie:
            lines.append("Cookie: cb_session=" + cookie)
        self.sock.sendall(("\r\n".join(lines) + "\r\n\r\n").encode())
        head = b""
        while b"\r\n\r\n" not in head:
            head += self.sock.recv(1)
        self.status = int(head.split()[1])
        self.buf = b""
        self.ended = None
        if self.status == 101:
            self.send(secret.encode(), opcode=1)

    def send(self, payload, opcode=2):
        mask = os.urandom(4)
        header = bytes([0x80 | opcode])
        n = len(payload)
        header += bytes([0x80 | n]) if n < 126 else bytes([0x80 | 126]) + struct.pack(">H", n)
        self.sock.sendall(header + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(payload)))

    def message(self):
        hdr = self.exact(2)
        opcode, n = hdr[0] & 0x0F, hdr[1] & 0x7F
        if n == 126:
            n = struct.unpack(">H", self.exact(2))[0]
        elif n == 127:
            n = struct.unpack(">Q", self.exact(8))[0]
        payload = self.exact(n)
        if opcode == 8:
            raise EOFError("closed")
        if opcode == 9:
            self.sock.sendall(bytes([0x8A, 0x80]) + os.urandom(4))
            return self.message()
        return opcode, payload

    def exact(self, n):
        out = b""
        while len(out) < n:
            chunk = self.sock.recv(n - len(out))
            if not chunk:
                raise EOFError("eof")
            out += chunk
        return out

    def reply(self):
        _, payload = self.message()
        return json.loads(payload)

    def rfb(self, n):
        while len(self.buf) < n:
            _, payload = self.message()
            self.buf += payload
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def handshake(self):
        version = self.rfb(12)
        self.send(b"RFB 003.008\n")
        types = self.rfb(self.rfb(1)[0])
        self.send(b"\x01")
        result = struct.unpack(">I", self.rfb(4))[0]
        self.send(b"\x01")
        width, height = struct.unpack(">HH", self.rfb(4))
        self.rfb(16)
        self.rfb(struct.unpack(">I", self.rfb(4))[0])
        return version, types, result, width, height

    def update(self):
        """Requests a 16x16 corner in Raw, the only encoding without SetEncodings, and reads the whole update."""
        self.send(struct.pack(">BBHHHH", 3, 0, 0, 0, 16, 16))
        while True:
            kind = self.rfb(1)[0]
            if kind == 0:
                self.rfb(1)
                for _ in range(struct.unpack(">H", self.rfb(2))[0]):
                    _, _, w, h, encoding = struct.unpack(">HHHHi", self.rfb(12))
                    if encoding != 0:
                        return False
                    self.rfb(w * h * 4)
                return True
            if kind == 2:
                continue
            if kind == 3:
                self.rfb(3)
                self.rfb(struct.unpack(">I", self.rfb(4))[0])
                continue
            return False

    def watch(self):
        def run():
            try:
                while True:
                    self.message()
            except (EOFError, OSError):
                pass
            self.ended = time.time()
        threading.Thread(target=run, daemon=True).start()


def grpc(token_var, method, body, box):
    args = [
        "grpcurl", "-expand-headers", "-import-path", str(PROTO),
        "-proto", "cocoonbox/control/v1/boxes.proto", "-proto", "cocoonbox/agent/v1/agents.proto", "-proto", "cocoonbox/agent/v1/session.proto",
        "-H", "authorization: Bearer ${" + token_var + "}", "-H", "x-cocoonbox-box: " + box,
        "-d", json.dumps(body), urlsplit(ENDPOINT).hostname + ":443", method,
    ]
    out = subprocess.run(args, capture_output=True, text=True, timeout=600)
    return out.returncode, out.stdout, out.stderr


def agents(box):
    code, out, err = grpc("QA1_TOKEN", "cocoonbox.agent.v1.Agents/List", {}, box)
    if code != 0:
        sys.exit("Agents/List: " + err)
    return json.loads(out or "{}").get("agents", [])


def pointer(owner):
    status, out = owner.call("shell", {"command": "python3 - <<'EOF'\n" + POINTER + "EOF"})
    text = out.get("result", {}).get("content", [{}])[0].get("text", "")
    try:
        return tuple(int(v) for v in text.split()[:2])
    except ValueError:
        return None


def cut_check(what, qa1, share, action):
    """Opens the share's page and a guest MCP stream, runs action, and checks both end within CUT_LIMIT."""
    secret = share["secret"]
    viewer = Viewer(secret)
    reply = viewer.reply()
    viewer.handshake()
    viewer.watch()
    guest = MCP(f"/cocoonbox.mcp/{share['share']['box_id']}", secret)
    guest.open()
    stream = guest.stream()
    time.sleep(1)
    check(reply.get("ok") and stream.ended is None and viewer.ended is None, f"{what}: the page and a guest MCP stream are open")
    started = action()
    mcp_end = stream.wait(5)
    deadline = time.time() + 5
    while viewer.ended is None and time.time() < deadline:
        time.sleep(0.02)
    for end, side in ((viewer.ended, "share page"), (mcp_end, "guest MCP stream")):
        took = "did not end within 5 s" if end is None else f"ended {end - started:.3f}s after"
        check(end is not None and end - started <= CUT_LIMIT, f"{what}: the {side} {took}")
    status, _ = MCP(guest.path, secret).post("ping")
    check(status == 403, f"{what}: the old link answers {status} on MCP, want 403 (PermissionDenied)")
    again = Viewer(secret)
    check(again.status == 101 and again.reply().get("code") == "permissiondenied", f"{what}: the old link's page is refused with PermissionDenied")


def main():
    for name in ("QA1_PASSWORD", "QA2_PASSWORD", "QA3_PASSWORD"):
        if not os.environ.get(name):
            sys.exit(f"{name} is not set")
    qa1, qa2, qa3 = Console("qa1"), Console("qa2"), Console("qa3")
    os.environ["QA1_TOKEN"], os.environ["QA2_TOKEN"], os.environ["QA3_TOKEN"] = qa1.token(), qa2.token(), qa3.token()
    control = MCP("/cocoonbox.mcp/control", os.environ["QA1_TOKEN"])
    control.open()
    _, ensured = control.call("box_ensure", {"wait": True})
    box = ensured["result"]["structuredContent"]["box_id"]
    note(f"qa1's box {box}: {ensured['result']['structuredContent']['state']}")
    for sh in qa1.call("GET", "shares")[1]:
        qa1.call("DELETE", "shares/" + sh["id"])
    path = f"/cocoonbox.mcp/{box}"

    print("== 1. an open share: view-only page, read-only tools")
    status, share = qa1.call("POST", "shares", {"box_id": box})
    check(status == 200 and share["secret"].startswith("cbs_"), f"qa1 creates an open share ({status})")
    secret = share["secret"]
    owner = MCP(path, os.environ["QA1_TOKEN"])
    owner.open("owner-accept")
    before = pointer(owner)
    viewer = Viewer(secret)
    check(viewer.status == 101 and viewer.reply().get("ok"), "the page's WebSocket admits the link's secret")
    version, types, result, width, height = viewer.handshake()
    check(version.startswith(b"RFB 003.00") and types == b"\x01" and result == 0 and (width, height) == (1280, 800),
          f"RFB handshake through the bridge: {version!r}, security {types!r}, {width}x{height}")
    check(viewer.update(), "a framebuffer update reaches the viewer")
    target = ((before or (0, 0))[0] + 333) % width, ((before or (0, 0))[1] + 222) % height
    viewer.send(struct.pack(">BBHH", 5, 1, *target) + struct.pack(">BBHH", 5, 0, *target))
    viewer.send(struct.pack(">BBHI", 4, 1, 0, 0x61) + struct.pack(">BBHI", 4, 0, 0, 0x61))
    check(viewer.update(), "the stream stays up after the viewer sends pointer and key events")
    after = pointer(owner)
    if before is None or after is None:
        note("SKIP pointer check: the box has no python3 or libX11 for XQueryPointer")
    else:
        check(after == before, f"the box pointer stayed at {before} (viewer asked for {target}); now {after}")
    guest = MCP(path, secret)
    check(guest.open() == 200, "a guest opens an MCP session with only the link's secret")
    status, tools = guest.tools()
    check(tools == READ_ONLY, f"the guest lists only the read-only tools: {tools}")
    status, out = guest.call("shell", {"command": "echo guest"})
    check(out.get("error", {}).get("code") == -32602, f"a guest shell call is refused: {out.get('error')}")
    status, out = guest.call("screenshot", {"max_edge": 320})
    check(status == 200 and not out.get("result", {}).get("isError"), "a guest screenshot works")
    note("Manual: open the link in a browser; the screen shows and ignores the mouse and keys.")
    note("Manual: claude mcp add ... with the share snippet; `claude mcp list` shows it and /mcp lists only the read-only tools.")

    print("== 2. allow write, then take it back")
    status, _ = qa1.call("PATCH", "shares/" + share["share"]["id"], {"write": True})
    status, out = guest.call("shell", {"command": "echo guest-write"})
    check("guest-write" in json.dumps(out.get("result", {})), "with write on, the next guest shell call runs")
    status, tools = guest.tools()
    check(len(tools) == 13 and not {"fill_credential", "box_ensure", "box_save", "box_upgrade", "box_hibernate"} & set(tools),
          "with write on the guest lists 13 tools, none of them lifecycle or FillCredential")
    status, _, _ = http("POST", "/cocoonbox.mcp/control", {"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, {"Authorization": "Bearer " + secret})
    check(status == 401, f"the control MCP refuses the share secret ({status})")
    os.environ["SHARE_SECRET"] = secret
    code, _, err = grpc("SHARE_SECRET", "cocoonbox.agent.v1.Session/FillCredential", {"origin": "https://example.com", "username": "u", "password": "p"}, box)
    check(code != 0 and "Unauthenticated" in err, "Session.FillCredential with the share secret is refused (Unauthenticated)")
    qa1.call("PATCH", "shares/" + share["share"]["id"], {"write": False})
    status, out = guest.call("shell", {"command": "echo again"})
    check(out.get("error", {}).get("code") == -32602, "with write off again, the next guest shell call is refused")

    print("== 3. a share for qa2 only")
    qa1.call("DELETE", "shares/" + share["share"]["id"])
    status, named = qa1.call("POST", "shares", {"box_id": box, "user_names": ["qa2"]})
    check(status == 200 and named["share"]["users"] == ["qa2"], "qa1 creates a share for qa2")
    check(MCP(path, named["secret"]).open() == 401, "the link alone is refused (401)")
    check(MCP(path, named["secret"], os.environ["QA2_TOKEN"]).open() == 200, "the link with qa2's token is admitted")
    check(MCP(path, named["secret"], os.environ["QA3_TOKEN"]).open() == 403, "the link with qa3's token is refused (403)")
    page = Viewer(named["secret"])
    check(page.reply().get("code") == "unauthenticated", "the page without a console session asks to sign in")
    check(Viewer(named["secret"], qa2.cookie).reply().get("ok"), "the page with qa2's console session is admitted")
    check(Viewer(named["secret"], qa3.cookie).reply().get("code") == "permissiondenied", "the page with qa3's console session is refused")

    print("== 4. who shows as an agent")
    names = [a["name"] for a in agents(box)]
    owner_mcp = [n for n in names if n.startswith("mcp:owner-accept")]
    check(not owner_mcp, f"qa1's own MCP session made no agent: {owner_mcp}")
    shared = [n for n in names if n.startswith("mcp:share-accept@" + named["share"]["id"] + ":")]
    check(len(shared) >= 1, f"qa2's session shows as {shared[:1]}")
    note("Manual: qa1's app lists the qa2 session under External with its calls, and End closes it.")

    print("== 5. a Rebase waits for a guest's running shell call")
    qa1.call("PATCH", "shares/" + named["share"]["id"], {"write": True})
    worker = MCP(path, named["secret"], os.environ["QA2_TOKEN"])
    worker.open()
    done = {}

    def slow():
        status, out = worker.call("shell", {"command": "sleep 15; echo slept", "timeout_s": 120})
        done["at"], done["out"] = time.time(), json.dumps(out.get("result", {}))

    thread = threading.Thread(target=slow)
    thread.start()
    time.sleep(2)
    code, out, err = grpc("QA1_TOKEN", "cocoonbox.control.v1.Boxes/Rebase", {"box_id": box}, box)
    op = json.loads(out or "{}").get("id", "")
    code, events, err = grpc("QA1_TOKEN", "cocoonbox.control.v1.Boxes/WatchOperation", {"operation_id": op}, box)
    rebased = time.time()
    thread.join()
    check("slept" in done.get("out", ""), "the guest's shell call ran to its end")
    check(rebased > done.get("at", rebased + 1), f"the Rebase finished {rebased - done.get('at', 0):.1f}s after the call")
    check('"PHASE_DONE"' in events, "the Rebase is done")
    control.call("box_ensure", {"wait": True})

    print("== 6. revoke, regenerate and expiry cut the page and MCP within 1 s")
    sid = named["share"]["id"]
    qa1.call("DELETE", "shares/" + sid)
    _, opened = qa1.call("POST", "shares", {"box_id": box})
    cut_check("revoke", qa1, opened, lambda: (qa1.call("DELETE", "shares/" + opened["share"]["id"]), time.time())[1])
    _, opened = qa1.call("POST", "shares", {"box_id": box})
    fresh = {}

    def regenerate():
        _, fresh["share"] = qa1.call("POST", "shares/" + opened["share"]["id"] + "/regenerate")
        return time.time()

    cut_check("regenerate", qa1, opened, regenerate)
    check(MCP(path, fresh["share"]["secret"]).open() == 200, "the regenerated link works")
    qa1.call("DELETE", "shares/" + opened["share"]["id"])
    _, short = qa1.call("POST", "shares", {"box_id": box, "ttl_seconds": 60})

    def expire():
        until = short["share"]["expires_at_ms"] / 1000
        time.sleep(max(0, until - time.time()))
        return until

    cut_check("expiry", qa1, short, expire)

    for sh in qa1.call("GET", "shares")[1]:
        qa1.call("DELETE", "shares/" + sh["id"])
    print(f"== {'FAILED: ' + str(len(failures)) if failures else 'ALL PASSED'}")
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
