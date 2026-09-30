#!/bin/bash
# End-to-end test of the cocoonbox CLI and its stdio MCP bridge against MockBox:
# lifecycle commands over gRPC, the bridge's tool list while the box is
# hibernated, the wake on the first box-tool call with progress, the control
# tools and their refusals, --no-wake, and the headers helper. `login` stores
# the mock token in a throwaway Keychain item, deleted on exit; the item is the
# CLI's own, so reading it back raises no prompt.
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
service="$CB_KEYCHAIN_SERVICE.cli"

"$bin/MockBox" --port "$port" --boot-ms 1500 >"$work/mockbox.log" 2>&1 &
mock=$!
cleanup() {
  kill "$mock" 2>/dev/null || true
  security delete-generic-password -s "$service" -a "$COCOONBOX_ENDPOINT" >/dev/null 2>&1 || true
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
    def __init__(self, *flags):
        self.p = subprocess.Popen([cli, "mcp", *flags], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True, bufsize=1)
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
check("tools/list while hibernated: 5 control + 13 box tools", len(names) == 18 and "box_ensure" in names and "computer_act" in names, names)

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

log = open(mocklog).read()
check("the box sees the client's own name", "rpc=initialize client=test-client" in log)
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

# Writes stay inside the HOME the CLI is given: every file command runs under a
# sandbox that denies writes outside the test directory and the process temp.
import os, hashlib, pathlib
work = os.path.dirname(mocklog)
home = os.path.join(work, "home")
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
