#!/usr/bin/env python3
"""Acceptance of several boxes per user (design §19.5) against a deployed api, run on bench.

A scratch user (CB_USER, default mb1) exists with max boxes 2 and exactly one box, main; its console password comes
from <CB_USER>_PASSWORD. The script issues the user a fresh API token, never prints a secret, and refuses CMGS's and
the other real users. It needs sudo for the sandboxd root token and the MinIO root credentials, which it only reads.
"""

import hashlib
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

ENDPOINT = os.environ["CB_ENDPOINT"].rstrip("/")
USER = os.environ.get("CB_USER", "mb1")
REAL = {"test", "cmgs", "dev", "a tester"}
SANDBOXD = "http://127.0.0.1:17780"
MAX_CLAIMS = int(os.environ.get("CB_MAX_CLAIMS", "6"))
BUCKET = "cocoonbox-state"

failures = []


def check(ok, what):
    print(("PASS " if ok else "FAIL ") + what, flush=True)
    if not ok:
        failures.append(what)


def note(text):
    print("     " + text, flush=True)


def http(method, url, body=None, headers=None):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method=method)
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


def box_id(user):
    return "cb-" + hashlib.sha256(user.encode()).hexdigest()[:12]


class MCP:
    """One MCP Streamable HTTP session as the scratch user."""

    def __init__(self, path, token):
        self.path, self.token, self.session, self.next = path, token, None, 0

    def post(self, method, params=None):
        self.next += 1
        headers = {"Authorization": "Bearer " + self.token}
        if self.session:
            headers["Mcp-Session-Id"] = self.session
        status, out, raw = http("POST", ENDPOINT + self.path, {"jsonrpc": "2.0", "id": self.next, "method": method, "params": params or {}}, headers)
        if method == "initialize" and status == 200:
            self.session = out["Mcp-Session-Id"]
            headers["Mcp-Session-Id"] = self.session
            http("POST", ENDPOINT + self.path, {"jsonrpc": "2.0", "method": "notifications/initialized"}, headers)
        text = raw.decode(errors="replace")
        messages = [json.loads(line[5:]) for line in text.splitlines() if line.startswith("data:")] if text.startswith(("event:", "data:")) else ([json.loads(text)] if text.strip() else [])
        return status, messages

    def open(self):
        return self.post("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "multibox-accept", "version": "1"}})[0]

    def tool(self, name, arguments=None, progress=False):
        params = {"name": name, "arguments": arguments or {}}
        if progress:
            params["_meta"] = {"progressToken": "p"}
        status, messages = self.post("tools/call", params)
        result = (messages[-1] if messages else {}).get("result", {})
        notes = [m["params"]["message"] for m in messages if m.get("method") == "notifications/progress"]
        return status, result, notes


def text(result):
    return " ".join(c.get("text", "") for c in result.get("content", []))


def sudo(*argv):
    return subprocess.run(["sudo", *argv], capture_output=True, text=True, timeout=60)


def claims():
    token = sudo("cat", "/etc/cocoonbox/sandboxd-token").stdout.strip()
    _, _, raw = http("GET", SANDBOXD + "/v1/sandboxes", headers={"Authorization": "Bearer " + token})
    return [r for r in json.loads(raw).get("sandboxes", []) if not r.get("archived")]


def stored(key):
    script = ". /etc/cocoonbox/minio.env; MC_HOST_cbroot=http://$MINIO_ROOT_USER:$MINIO_ROOT_PASSWORD@10.203.0.1:9000 mcli --config-dir /root/.mcli stat --json cbroot/" + BUCKET + "/" + key
    return sudo("sh", "-c", script).returncode == 0


def main():
    if USER in REAL:
        sys.exit(f"{USER} is a real user; run this with a scratch user only")
    status, headers, body = http("POST", ENDPOINT + "/cb/api/login", {"name": USER, "password": os.environ[USER.upper() + "_PASSWORD"]})
    if status != 200:
        sys.exit(f"sign-in of {USER} failed: {status}")
    cookie = {"Cookie": headers["Set-Cookie"].split(";", 1)[0]}
    _, _, raw = http("GET", ENDPOINT + "/cb/api/me", headers=cookie)
    me = json.loads(raw)
    uid = me["user"]["id"]
    if uid in REAL or me["user"]["name"] in REAL:
        sys.exit(f"{USER} has id {uid}, a real user")
    if me["user"]["max_boxes"] != 2 or [b["name"] for b in me["boxes"]] != ["main"]:
        sys.exit(f"{USER} needs max boxes 2 and only its box main; it has max boxes {me['user']['max_boxes']} and boxes {[b['name'] for b in me['boxes']]}")
    _, _, raw = http("POST", ENDPOINT + "/cb/api/me/token", {}, cookie)
    token = json.loads(raw)["token"]

    live = claims()
    own = [r for r in live if r.get("tenant") == uid or (r.get("metadata") or {}).get("cb_owner") == uid]
    free = MAX_CLAIMS - len(live)
    note(f"sandboxd holds {len(live)} of {MAX_CLAIMS} claims, {len(own)} of them {USER}'s")
    if free < 2 - len(own):
        sys.exit(f"needs {2 - len(own)} free claims, sandboxd has {free}")

    control = MCP("/cocoonbox.mcp/control", token)
    check(control.open() == 200, "the control MCP opens for the scratch user")
    main_id = me["boxes"][0]["id"]
    _, result, _ = control.tool("box_ensure", {"box": "main"})
    check(result.get("structuredContent", {}).get("state") == "running", f"main ({main_id}) runs")

    # 1. a second box beside a running one is refused; after hibernating main it starts.
    _, result, _ = control.tool("box_create", {"name": "test"})
    check(result.get("isError") and f"box main ({main_id}) is running" in text(result), "1. creating test while main runs is refused, naming main: " + text(result)[:160])
    _, result, _ = control.tool("box_hibernate", {"box": "main"})
    check(result.get("structuredContent", {}).get("state") == "hibernated", "1. main hibernates")
    started = time.time()
    _, result, _ = control.tool("box_create", {"name": "test"})
    test_id = result.get("structuredContent", {}).get("box_id", "")
    check(result.get("structuredContent", {}).get("state") == "running", f"1. test ({test_id}) is created and runs in {time.time() - started:.0f}s")
    _, result, _ = control.tool("box_ensure", {"box": "main"})
    check(result.get("isError") and f"box test ({test_id}) is running" in text(result), "1. waking main while test runs is refused, naming test")

    # 2. the same token drives both boxes by id over the edge; each answers from its own identity file, since clones share a hostname.
    hosts = {}
    for name, box in (("test", test_id), ("main", main_id)):
        if name == "main":
            control.tool("box_hibernate", {"box": "test"})
            control.tool("box_ensure", {"box": "main"})
        session = MCP("/cocoonbox.mcp/" + box, token)
        check(session.open() == 200, f"2. an MCP session opens on {name}")
        _, result, _ = session.tool("shell", {"command": "uname -n; cat /run/cocoonbox-identity/identity"})
        hosts[name] = text(result)
        note(f"{name}: uname -n = {hosts[name].split()[0] if hosts[name].split() else ''!r}")
        check(f'"box_id":"{box}"' in hosts[name], f"2. the shell call to {name} ran in {box}")
    _, result, _ = control.tool("box_list")
    listed = [(b["name"], b["state"]) for b in result.get("structuredContent", {}).get("boxes", [])]
    check(listed == [("main", "running"), ("test", "hibernated")], f"2. box_list = {listed}")

    # 3. the total quota, with no box running, so the running limit cannot answer first.
    control.tool("box_hibernate", {"box": "main"})
    _, result, _ = control.tool("box_create", {"name": "third"})
    check(result.get("isError") and "quota" in text(result), "3. a third box is refused by max boxes: " + text(result)[:120])

    # 5. another user's box, by id at the edge and by name or id at the control MCP.
    other = box_id("dev")
    status, _ = MCP("/cocoonbox.mcp/" + other, token).post("ping")
    check(status == 403, f"5. the edge answers {status} for another user's box id, want 403")
    _, result, _ = control.tool("box_status", {"box": other})
    check(result.get("isError") and "no box" in text(result), "5. the control MCP refuses another user's box id")

    # 4. delete test, running, so it is saved first.
    control.tool("box_hibernate", {"box": "main"})
    control.tool("box_ensure", {"box": "test"})
    machines = [r["metadata"]["cb_machine"] for r in claims() if (r.get("metadata") or {}).get("cb_machine", "").startswith(test_id)]
    _, result, notes = control.tool("box_delete", {"box": "test", "confirm": True}, progress=True)
    check(result.get("structuredContent", {}).get("box_id") == test_id, "4. box_delete test")
    check(any(n.startswith("saving:") for n in notes) and any(n.startswith("deleting:") for n in notes), "4. the delete saved before it removed the machine: " + " | ".join(notes[-3:])[:200])
    left = [r for r in claims() if (r.get("metadata") or {}).get("cb_machine", "").startswith(test_id)]
    check(machines and not left, f"4. test's claims {machines} are released")
    check(stored(f"{uid}/boxes/{test_id}/head.json"), "4. test's last snapshot is still in the store")
    _, result, _ = control.tool("box_list")
    check([b["name"] for b in result.get("structuredContent", {}).get("boxes", [])] == ["main"], "4. only main is left")
    _, result, _ = control.tool("box_ensure", {"box": "main"})
    check(result.get("structuredContent", {}).get("state") == "running", "4. main still wakes with the user's gw key in place")
    control.tool("box_hibernate", {"box": "main"})

    print("manual: one Claude Code session through the bridge (cocoonbox mcp, no restart and no config change): box_list,")
    print("        box_create test, box_ensure test, shell uname -n with box=test and with box=main, then box_delete test.")
    print(f"{len(failures)} failed" if failures else "all passed")
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
