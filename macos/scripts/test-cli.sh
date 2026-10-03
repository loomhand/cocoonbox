#!/bin/bash
# End-to-end test of the cocoonbox CLI and its stdio MCP bridge against MockBox:
# lifecycle commands over gRPC, the bridge's tool list while the box is
# hibernated, the wake on the first box-tool call with progress, the control
# tools and their refusals, --no-wake, the headers helper, several boxes:
# routing by `box`, the default box, the running limit, --box and box_delete,
# and the app's socket: reported box-tool calls and a default box the app sets. Nothing touches
# the login keychain or the real HOME: `login` stores the mock token in a
# throwaway keychain file (CB_KEYCHAIN_FILE), and HOME is a test directory.
set -euo pipefail

cd "$(dirname "$0")/.."
source scripts/toolchain.sh
swift build --product MockBox >/dev/null
swift build --product cocoonbox-cli >/dev/null
bin="$(swift build --show-bin-path)"

port=50071
work="$(mktemp -d)"
export COCOONBOX_ENDPOINT="http://127.0.0.1:$port"
export CB_KEYCHAIN_SERVICE=me.cmgs.cocoonbox.test
export COCOONBOX_CACHE_DIR="$work/cache"
export COCOONBOX_CONFIG_DIR="$work/config"
export HOME="$work/home"
export CB_KEYCHAIN_FILE="$work/test.keychain-db"
mkdir -p "$HOME"
security create-keychain -p test "$CB_KEYCHAIN_FILE"
security set-keychain-settings "$CB_KEYCHAIN_FILE"
security unlock-keychain -p test "$CB_KEYCHAIN_FILE"

"$bin/MockBox" --port "$port" --boot-ms 1500 >"$work/mockbox.log" 2>&1 &
mock=$!
cleanup() {
  kill "$mock" 2>/dev/null || true
  security delete-keychain "$CB_KEYCHAIN_FILE" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT
sleep 1
echo mock-token | "$bin/cocoonbox-cli" login

python3 - "$bin/cocoonbox-cli" "$work/mockbox.log" <<'PY'
import json, subprocess, sys, time

cli, mocklog = sys.argv[1], sys.argv[2]
failures = []

def check(name, ok, detail=""):
    print(("ok   " if ok else "FAIL ") + name + (f"  ({detail})" if detail and not ok else ""))
    if not ok:
        failures.append(name)

def run(*args):
    return subprocess.run([cli, *args], capture_output=True, text=True, timeout=60)

status = run("status", "--json")
check("status over gRPC", json.loads(status.stdout)["state"] == "hibernated", status.stdout + status.stderr)

class Bridge:
    def __init__(self, *flags, env=None):
        self.p = subprocess.Popen([cli, "mcp", *flags], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1, env=env)
        self.next = 1

    def send(self, method, params=None, notify=False):
        message = {"jsonrpc": "2.0", "method": method, "params": params or {}}
        if not notify:
            message["id"] = self.next
            self.next += 1
        self.p.stdin.write(json.dumps(message) + "\n")
        self.p.stdin.flush()
        return message.get("id")

    def call(self, method, params=None):
        id = self.send(method, params)
        seen = []
        while True:
            line = self.p.stdout.readline()
            if not line:
                raise RuntimeError("bridge closed stdout")
            message = json.loads(line)
            if message.get("id") == id:
                return message, seen
            seen.append(message)

    def tool(self, name, arguments=None, token=None):
        params = {"name": name, "arguments": arguments or {}}
        if token:
            params["_meta"] = {"progressToken": token}
        answer, seen = self.call("tools/call", params)
        return answer["result"], seen

    def close(self):
        self.p.stdin.close()
        self.p.wait(timeout=10)

def text(result):
    return " ".join(c.get("text", "") for c in result.get("content", []))

b = Bridge()
init, _ = b.call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "test-client", "version": "9"}})
check("initialize answered by the bridge", init["result"]["serverInfo"]["name"] == "cocoonbox")
b.send("notifications/initialized", notify=True)
tools, _ = b.call("tools/list")
names = [t["name"] for t in tools["result"]["tools"]]
check("tools/list while hibernated: 8 control + 13 box tools", len(names) == 21 and "box_ensure" in names and "box_create" in names and "computer_act" in names, names)
shell_tool = next(t for t in tools["result"]["tools"] if t["name"] == "shell")
check("box tools take a box argument", "box" in shell_tool["inputSchema"].get("properties", {}), shell_tool)

result, _ = b.tool("box_status")
check("box_status via the control MCP", result["structuredContent"]["state"] == "hibernated", result)

started = time.time()
result, seen = b.tool("shell", {"command": "uname -a"}, token="t1")
progress = [m["params"]["message"] for m in seen if m.get("method") == "notifications/progress"]
changed = any(m.get("method") == "notifications/tools/list_changed" for m in seen)
check("first box-tool call wakes the box and runs", "Linux 6.12.0 (mockbox)" in text(result), result)
check("wake progress reaches the client with its token", any(p.startswith("waking:") for p in progress) and all(m["params"]["progressToken"] == "t1" for m in seen if m.get("method") == "notifications/progress"), progress)
check("tools/list_changed after the box's own list arrives", changed)
print(f"     wake + call took {time.time() - started:.1f} s; progress: {progress}")

result, _ = b.tool("box_upgrade")
check("box_upgrade without confirm is refused", result.get("isError") and "confirm: true" in text(result), result)
result, seen = b.tool("box_save", token="t2")
check("box_save returns bytes", result["structuredContent"]["bytes"] > 0 and any(m.get("method") == "notifications/progress" for m in seen), result)
result, _ = b.tool("box_hibernate")
check("box_hibernate", result["structuredContent"]["state"] == "hibernated", result)
result, _ = b.tool("read_file", {"path": "/workspace/notes.md"})
check("a box tool after hibernate wakes the box again", "Notes" in text(result), result)
b.close()

time.sleep(1)
log = open(mocklog).read()
check("the box sees the client's own name", "rpc=initialize client=test-client" in log)
check("closing stdin ends the box session", "mcp DELETE /cocoonbox.mcp/cb-mock00000000" in log)
t = Bridge()
t.call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "term-client", "version": "1"}})
t.send("notifications/initialized", notify=True)
time.sleep(2)
t.p.terminate()
t.p.wait(timeout=10)
time.sleep(1)
log = open(mocklog).read()
check("SIGTERM ends the box session too", log.count("mcp DELETE /cocoonbox.mcp/cb-mock00000000") >= 2, log[-400:])
check("no request reached MockBox unauthorised", "auth=ok" in log and "auth=bad" not in log and "auth=missing" not in log)

subprocess.run([cli, "hibernate"], capture_output=True, timeout=60)
n = Bridge("--no-wake")
n.call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "test-client", "version": "9"}})
result, _ = n.tool("list_dir", {"path": "/"})
check("--no-wake refuses with a hint", result.get("isError") and "box_ensure" in text(result), result)
n.close()

headers = run("mcp-headers")
check("mcp-headers prints the Authorization header", json.loads(headers.stdout) == {"Authorization": "Bearer mock-token"}, headers.stdout)

ensure = run("ensure")
check("ensure over gRPC with progress on stderr", "waking" in ensure.stderr and "running" in ensure.stdout, ensure.stdout + ensure.stderr)
upgrade = run("upgrade")
check("upgrade without --yes refuses", upgrade.returncode != 0 and "--yes" in upgrade.stderr, upgrade.stderr)

# Several boxes: main runs here after `ensure`.
m = Bridge()
m.call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "multi-client", "version": "1"}})
m.send("notifications/initialized", notify=True)
result, _ = m.tool("box_create", {"name": "test"})
check("box_create beside a running box is refused, naming it", result.get("isError") and "box main (cb-mock00000000) is running" in text(result), result)
result, _ = m.tool("box_hibernate")
check("box_hibernate without box acts on the default box, main", result["structuredContent"]["box_id"] == "cb-mock00000000" and result["structuredContent"]["state"] == "hibernated", result)
result, _ = m.tool("box_create", {"name": "test"})
check("box_create test", result.get("structuredContent", {}).get("name") == "test" and result["structuredContent"]["state"] == "running", result)
test_id = result.get("structuredContent", {}).get("box_id", "")
result, _ = m.tool("box_list")
listed = [(b["name"], b["state"]) for b in result["structuredContent"]["boxes"]]
check("box_list lists both, oldest first", listed == [("main", "hibernated"), ("test", "running")], listed)
result, _ = m.tool("shell", {"box": "test", "command": "uname -n"})
check("shell with box=test runs in test", "on test" in text(result), result)
result, _ = m.tool("shell", {"box": "main", "command": "uname -n"})
check("waking main while test runs is refused by the running limit", result.get("isError") and "box test (" + test_id + ") is running" in text(result), result)
result, _ = m.tool("shell", {"box": "cb-mock99999999", "command": "id"})
check("another user's box id is refused", result.get("isError") and "no box" in text(result), result)
m.close()

d = Bridge()
d.call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "default-client", "version": "1"}})
result, _ = d.tool("shell", {"command": "uname -n"})
check("without box and --box, the default is the newest running box", "on test" in text(result), result)
d.close()

# The app's socket: the bridge names its agent, reports each box-tool call and
# takes the default box the app sends, first in answer to its hello, then on a change.
import os, socket, threading
class FakeApp:
    def __init__(self, path, default):
        self.default, self.hellos, self.calls, self.conns = default, [], [], []
        self.s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.s.bind(path)
        self.s.listen(4)
        threading.Thread(target=self.accept, daemon=True).start()

    def accept(self):
        while True:
            try:
                c, _ = self.s.accept()
            except OSError:
                return
            self.conns.append(c)
            threading.Thread(target=self.read, args=(c,), daemon=True).start()

    def read(self, c):
        for line in c.makefile("r"):
            m = json.loads(line)
            if "box" in m:
                self.calls.append(m)
                continue
            self.hellos.append(m)
            c.sendall((json.dumps({"box": self.default}) + "\n").encode())

    def push(self, box):
        for c in self.conns:
            c.sendall((json.dumps({"box": box}) + "\n").encode())
        time.sleep(0.3)

    def close(self):
        for c in self.conns:
            c.close()
        self.s.close()

def wait_for(condition, seconds=5):
    deadline = time.time() + seconds
    while not condition() and time.time() < deadline:
        time.sleep(0.05)
    return condition()

app_sock = os.path.join(os.path.dirname(mocklog), "app.sock")
app = FakeApp(app_sock, test_id)
linked_env = dict(os.environ, COCOONBOX_APP_SOCKET=app_sock, COCOONBOX_AGENT="agent-1")
l = Bridge("--box", "main", env=linked_env)
l.call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "linked-client", "version": "1"}})
check("the bridge names its agent on the app's socket", wait_for(lambda: app.hellos == [{"agent": "agent-1"}]), app.hellos)
time.sleep(0.3)
started = time.time() * 1000
result, _ = l.tool("shell", {"command": "uname -n"})
check("the default the app answers the hello with beats --box", "on test" in text(result), result)
result, _ = l.tool("shell", {"box": "test", "command": "uname -n"})
check("shell with box=test still runs in test", "on test" in text(result), result)
check("each routed box call is reported with agent, box id and time",
      wait_for(lambda: len(app.calls) == 2) and [(c["agent"], c["box"]) for c in app.calls] == [("agent-1", test_id)] * 2
      and all(started - 1000 <= c["at"] <= time.time() * 1000 for c in app.calls), app.calls)
app.push("cb-mock00000000")
result, _ = l.tool("box_status")
check("a default box pushed by the app reaches control tools with no restart", result["structuredContent"]["box_id"] == "cb-mock00000000", result)
l.tool("box_list")
l.tool("box_status", {"box": "test"})
time.sleep(0.3)
check("control tools are not reported, with or without box", len(app.calls) == 2, app.calls)
result, _ = l.tool("shell", {"command": "uname -n"})
check("and box tools: shell goes to hibernated main, refused by the running limit", result.get("isError") and "box test (" + test_id + ") is running" in text(result), result)
app.push(test_id)
result, _ = l.tool("shell", {"command": "uname -n"})
check("a second change also applies at the next call", "on test" in text(result), result)
check("box-tool reports follow the default", wait_for(lambda: len(app.calls) == 4) and [c["box"] for c in app.calls[2:]] == ["cb-mock00000000", test_id], app.calls)
app.close()
os.unlink(app_sock)
time.sleep(0.3)
result, _ = l.tool("shell", {"command": "uname -n"})
check("after the app quits, the bridge keeps working with the last default", "on test" in text(result), result)
l.close()

for label, env in (("with no socket", dict(os.environ, COCOONBOX_APP_SOCKET=app_sock, COCOONBOX_AGENT="agent-1")),
                   ("with the socket path unset", dict(os.environ, COCOONBOX_AGENT="agent-1"))):
    n = Bridge("--box", "test", env=env)
    n.call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "unlinked-client", "version": "1"}})
    result, _ = n.tool("shell", {"command": "uname -n"})
    check("the bridge works " + label + ", with its --box", "on test" in text(result), result)
    n.close()

p = Bridge("--box", "main")
p.call("initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "pinned-client", "version": "1"}})
result, _ = p.tool("box_status")
check("--box main is the default of control tools", result["structuredContent"]["box_id"] == "cb-mock00000000", result)
result, _ = p.tool("box_hibernate", {"box": "test"})
check("box_hibernate with box=test", result["structuredContent"]["box_id"] == test_id and result["structuredContent"]["state"] == "hibernated", result)
result, _ = p.tool("shell", {"command": "uname -n"})
check("--box main wakes main for a box tool", "on main" in text(result), result)
result, _ = p.tool("box_delete", {"box": "test"})
check("box_delete without confirm is refused", result.get("isError") and "confirm: true" in text(result), result)
result, _ = p.tool("box_delete", {"confirm": True})
check("box_delete without box is refused", result.get("isError") and "Name the box" in text(result), result)
result, _ = p.tool("box_delete", {"box": "test", "confirm": True})
check("box_delete test", result.get("structuredContent", {}).get("box_id") == test_id, result)
result, _ = p.tool("box_list")
check("box_list after delete", [b["name"] for b in result["structuredContent"]["boxes"]] == ["main"], result)
p.close()

import os, urllib.request, urllib.error
def mcp_status(box):
    req = urllib.request.Request(os.environ["COCOONBOX_ENDPOINT"] + "/cocoonbox.mcp/" + box, data=b'{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}',
                                 headers={"Authorization": "Bearer mock-token", "Content-Type": "application/json"}, method="POST")
    try:
        return urllib.request.urlopen(req, timeout=10).status
    except urllib.error.HTTPError as e:
        return e.code
check("the edge refuses another user's box id with 403", mcp_status("cb-mock99999999") == 403)
time.sleep(1)
log = open(mocklog).read()
check("box tool calls reached each box's own MCP path", "/cocoonbox.mcp/" + test_id + " auth=ok" in log and "rpc=tools/call tool=shell" in log, log[-600:])

# Writes stay inside the HOME the CLI is given: every file command runs under a
# sandbox that denies writes outside the test directory and the process temp.
import os, hashlib, pathlib
work = os.path.dirname(mocklog)
home = os.path.join(work, "confined-home")
os.makedirs(os.path.join(home, ".cursor"))
profile = os.path.join(work, "confine.sb")
open(profile, "w").write(f"""(version 1)
(allow default)
(deny file-write* (require-all (require-not (subpath "{os.path.realpath(work)}")) (require-not (subpath "/private/var/folders")) (require-not (subpath "/dev"))))
""")
def confined(*args):
    env = dict(os.environ, HOME=home)
    env.pop("COCOONBOX_CONFIG_DIR", None)
    env.pop("COCOONBOX_CACHE_DIR", None)
    return subprocess.run(["sandbox-exec", "-f", profile, cli, *args], capture_output=True, text=True, timeout=60, env=env)

cursor = os.path.join(home, ".cursor", "mcp.json")
open(cursor, "w").write(json.dumps({"mcpServers": {"other": {"command": "other-server", "args": ["x"]}}, "keep": 1}))
done = confined("setup", "cursor")
merged = json.load(open(cursor))
check("setup cursor merges into an existing mcp.json", done.returncode == 0 and merged["mcpServers"]["other"] == {"command": "other-server", "args": ["x"]} and merged["keep"] == 1 and merged["mcpServers"]["cocoonbox"]["args"] == ["mcp"], done.stderr + json.dumps(merged))
jsonc = '{\n  // a comment\n  "mcpServers": {"other": {"command": "other-server"}}\n}\n'
open(cursor, "w").write(jsonc)
refused = confined("setup", "cursor")
check("setup cursor leaves a file it cannot parse unchanged", refused.returncode != 0 and open(cursor).read() == jsonc, refused.stderr)
for args in (["plugin", "export", os.path.join(home, "plugins")], ["install"]):
    ran = confined(*args)
    check("confined: " + " ".join(args[:2]), ran.returncode == 0, ran.stderr)
check("install links inside the given HOME", os.path.islink(os.path.join(home, ".local", "bin", "cocoonbox")))

print(f"{len(failures)} failed" if failures else "all passed")
sys.exit(1 if failures else 0)
PY
