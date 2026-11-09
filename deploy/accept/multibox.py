#!/usr/bin/env python3
"""Acceptance of several boxes per user (design §19.5) against a deployed api, run on the deployment host.

A scratch user (CB_USER, default mb1) exists with max boxes 2 and exactly one box, main; its console password comes
from <CB_USER>_PASSWORD. The script issues the user a fresh API token, never prints a secret, and refuses the real
users in CB_REAL_USERS (comma-separated, default test,cmgs,dev). It needs sudo for the sandboxd root token and the
state store's credentials, which it only reads.
A failed run can leave the box test; delete it in the console before the next run.
"""

import hashlib
import json
import os
import subprocess
import sys
import time

import accept
from accept import check, failures, http, note

USER = os.environ.get("CB_USER", "mb1")
REAL = set(os.environ.get("CB_REAL_USERS", "test,cmgs,dev").split(","))
SANDBOXD = "http://127.0.0.1:17780"


class MCP(accept.MCP):
    """One MCP session as the scratch user."""

    def tool(self, name, arguments=None, progress=False):
        params = {"name": name, "arguments": arguments or {}}
        if progress:
            params["_meta"] = {"progressToken": "p"}
        status, messages = self.post("tools/call", params)
        result = (messages[-1] if messages else {}).get("result", {})
        notes = [m["params"]["message"] for m in messages if m.get("method") == "notifications/progress"]
        return status, result, notes


def box_id(user):
    return "cb-" + hashlib.sha256(user.encode()).hexdigest()[:12]


def text(result):
    return " ".join(c.get("text", "") for c in result.get("content", []))


def sudo(*argv):
    return subprocess.run(["sudo", *argv], capture_output=True, text=True, timeout=60)


def max_claims():
    return json.loads(sudo("cat", "/srv/cb/sbx/sandboxd.json").stdout)["max_claims"]


def claims():
    token = sudo("cat", "/etc/cocoonbox/sandboxd-token").stdout.strip()
    _, _, raw = http("GET", SANDBOXD + "/v1/sandboxes", headers={"Authorization": "Bearer " + token})
    return [r for r in json.loads(raw).get("sandboxes", []) if not r.get("archived")]


def stored(key):
    script = (
        ". /etc/cocoonbox/store.env; set -- $(python3 -c 'import yaml; "
        's = yaml.safe_load(open("/etc/cocoonbox/api.yaml"))["state"]; '
        'print("https" if s.get("secure") else "http", s["endpoint"], s["bucket"], s.get("region") or "us-east-1")\'); '
        'printf \'user = "%s:%s"\\n\' "$STORE_ACCESS_KEY" "$STORE_SECRET_KEY" | '
        'curl -sf -o /dev/null -I -K - --aws-sigv4 "aws:amz:$4:s3" "$1://$2/$3/' + key + '"'
    )
    return sudo("sh", "-c", script).returncode == 0


def main():
    if USER in REAL:
        sys.exit(f"{USER} is a real user; run this with a scratch user only")
    status, headers, body = http(
        "POST", "/cb/api/login", {"name": USER, "password": os.environ[USER.upper() + "_PASSWORD"]}
    )
    if status != 200:
        sys.exit(f"sign-in of {USER} failed: {status}")
    cookie = {"Cookie": headers["Set-Cookie"].split(";", 1)[0]}
    _, _, raw = http("GET", "/cb/api/me", headers=cookie)
    me = json.loads(raw)
    uid = me["user"]["id"]
    if uid in REAL or me["user"]["name"] in REAL:
        sys.exit(f"{USER} has id {uid}, a real user")
    if me["user"]["max_boxes"] != 2 or [b["name"] for b in me["boxes"]] != ["main"]:
        sys.exit(
            f"{USER} needs max boxes 2 and only its box main; "
            f"it has max boxes {me['user']['max_boxes']} and boxes {[b['name'] for b in me['boxes']]}"
        )
    _, _, raw = http("POST", "/cb/api/me/token", {}, cookie)
    token = json.loads(raw)["token"]

    live, cap = claims(), max_claims()
    own = [r for r in live if r.get("tenant") == uid or (r.get("metadata") or {}).get("cb_owner") == uid]
    free = cap - len(live)
    note(f"sandboxd holds {len(live)} of {cap} claims, {len(own)} of them {USER}'s")
    if free < 2 - len(own):
        sys.exit(f"needs {2 - len(own)} free claims, sandboxd has {free}")

    control = MCP("/cocoonbox.mcp/control", token)
    check(control.open("multibox-accept") == 200, "the control MCP opens for the scratch user")
    main_id = me["boxes"][0]["id"]
    _, result, _ = control.tool("box_ensure", {"box": "main"})
    check(result.get("structuredContent", {}).get("state") == "running", f"main ({main_id}) runs")

    _, result, _ = control.tool("box_create", {"name": "test"})
    check(
        result.get("isError") and f"box main ({main_id}) is running" in text(result),
        "1. creating test while main runs is refused, naming main: " + text(result)[:160],
    )
    _, result, _ = control.tool("box_hibernate", {"box": "main"})
    check(result.get("structuredContent", {}).get("state") == "hibernated", "1. main hibernates")
    started = time.time()
    _, result, _ = control.tool("box_create", {"name": "test"})
    test_id = result.get("structuredContent", {}).get("box_id", "")
    check(
        result.get("structuredContent", {}).get("state") == "running",
        f"1. test ({test_id}) is created and runs in {time.time() - started:.0f}s",
    )
    _, result, _ = control.tool("box_ensure", {"box": "main"})
    check(
        result.get("isError") and f"box test ({test_id}) is running" in text(result),
        "1. waking main while test runs is refused, naming test",
    )

    # clones share a hostname, so each box answers from its own identity file
    hosts = {}
    for name, box in (("test", test_id), ("main", main_id)):
        if name == "main":
            control.tool("box_hibernate", {"box": "test"})
            control.tool("box_ensure", {"box": "main"})
        session = MCP("/cocoonbox.mcp/" + box, token)
        check(session.open("multibox-accept") == 200, f"2. an MCP session opens on {name}")
        _, result, _ = session.tool("shell", {"command": "uname -n; cat /run/cocoonbox-identity/identity"})
        hosts[name] = text(result)
        note(f"{name}: uname -n = {hosts[name].split()[0] if hosts[name].split() else ''!r}")
        check(f'"box_id":"{box}"' in hosts[name], f"2. the shell call to {name} ran in {box}")
    _, result, _ = control.tool("box_list")
    listed = [(b["name"], b["state"]) for b in result.get("structuredContent", {}).get("boxes", [])]
    check(listed == [("main", "running"), ("test", "hibernated")], f"2. box_list = {listed}")

    # no box runs, so the running limit cannot answer first
    control.tool("box_hibernate", {"box": "main"})
    _, result, _ = control.tool("box_create", {"name": "third"})
    check(
        result.get("isError") and "quota" in text(result),
        "3. a third box is refused by max boxes: " + text(result)[:120],
    )

    other = box_id("dev")
    status, _ = MCP("/cocoonbox.mcp/" + other, token).post("ping")
    check(status == 404, f"5. the edge answers {status} for another user's box id, want 404")
    _, result, _ = control.tool("box_status", {"box": other})
    check(result.get("isError") and "no box" in text(result), "5. the control MCP refuses another user's box id")

    control.tool("box_ensure", {"box": "test"})
    machines = [
        r["metadata"]["cb_machine"]
        for r in claims()
        if (r.get("metadata") or {}).get("cb_machine", "").startswith(test_id)
    ]
    _, result, notes = control.tool("box_delete", {"box": "test", "confirm": True}, progress=True)
    check(result.get("structuredContent", {}).get("box_id") == test_id, "4. box_delete test")
    check(
        any(n.startswith("saving:") for n in notes) and any(n.startswith("deleting:") for n in notes),
        "4. the delete saved before it removed the machine: " + " | ".join(notes[-3:])[:200],
    )
    left = [r for r in claims() if (r.get("metadata") or {}).get("cb_machine", "").startswith(test_id)]
    check(machines and not left, f"4. test's claims {machines} are released")
    check(stored(f"{uid}/boxes/{test_id}/head.json"), "4. test's last snapshot is still in the store")
    _, result, _ = control.tool("box_list")
    check([b["name"] for b in result.get("structuredContent", {}).get("boxes", [])] == ["main"], "4. only main is left")
    _, result, _ = control.tool("box_ensure", {"box": "main"})
    check(
        result.get("structuredContent", {}).get("state") == "running",
        "4. main still wakes with the user's gw key in place",
    )
    control.tool("box_hibernate", {"box": "main"})

    print(
        "manual: one Claude Code session through the bridge (cocoonbox mcp, no restart and no config change): box_list,"
    )
    print(
        "        box_create test, box_ensure test, shell uname -n with box=test and with box=main, "
        "then box_delete test."
    )
    print(f"{len(failures)} failed" if failures else "all passed")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
