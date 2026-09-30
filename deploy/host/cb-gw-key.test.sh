#!/bin/sh
# usage: sh cb-gw-key.test.sh DIR  (DIR holds cb-gw-key, install-gw-config, gateway.yaml); runs offline in a temp dir
set -u
src=$(cd "${1:?usage: cb-gw-key.test.sh DIR}" && pwd)
t=$(mktemp -d)
trap 'rm -rf "$t"' EXIT
pass=0 failn=0
ok() { pass=$((pass + 1)); echo "PASS $*"; }
no() { failn=$((failn + 1)); echo "FAIL $*"; }

printf 'cb-sharedsharedsharedsharedsharedsharedsharedshare\n' > "$t/shared.key"
export CB_GW_KEY_DIR="$t/keys" CB_GW_RELOAD=0 CB_GW_LOCK="$t/lock" CB_GW_TEMPLATE="$src/gateway.yaml" \
	CB_GW_SHARED_KEY="$t/shared.key" CB_GW_OUT="$t/gateway.yaml" CB_GW_OWNER="$(id -un)" CB_GW_GROUP="$(id -gn)" CB_GW_RENDER="$src/install-gw-config"
k() { sh "$src/cb-gw-key" "$@" 2>/dev/null; }

for bad in "cb-xyz" "../../etc/passwd" "cb-0123456789ab/../x" "cb-0123456789AB" "" "cb-0123456789ab-R2" "cb-0123456789abc"; do
	out=$(k add "$bad"); case "$out" in error*) ok "reject add '$bad'" ;; *) no "accepted add '$bad': $out" ;; esac
done
out=$(k frob cb-0123456789ab); case "$out" in error*) ok "reject unknown verb" ;; *) no "unknown verb: $out" ;; esac
out=$(k list cb-0123456789ab); case "$out" in error*) ok "reject list with id" ;; *) no "list with id: $out" ;; esac

out=$(k add cb-0123456789ab); key=${out#ok }
case "$out" in ok\ cb-*) [ ${#key} -eq 48 ] && ok "add returns a 48-char key" || no "key length ${#key}" ;; *) no "add: $out" ;; esac
[ "$(stat -c %a "$t/keys/cb-0123456789ab")" = 600 ] && ok "key file is 0600" || no "key file mode"
[ "$(k add cb-0123456789ab)" = "ok $key" ] && ok "add is idempotent" || no "second add changed the key"
out=$(k add cb-0123456789ab-r2); key2=${out#ok }
[ -n "$key2" ] && [ "$key2" != "$key" ] && ok "Rebase machine id gets its own key" || no "rebase key: $out"

python3 - "$t/gateway.yaml" "$key" "$key2" <<'PY' && ok "render: shared once, each box key once, owner = base box id" || no "render content"
import sys, yaml
cfg = yaml.safe_load(open(sys.argv[1])); keys = {k["ak"]: k for k in cfg["access_keys"]}
aks = [k["ak"] for k in cfg["access_keys"]]
shared = open(sys.argv[1].rsplit("/", 1)[0] + "/shared.key").read().strip()
assert aks.count(shared) == 1 and aks.count(sys.argv[2]) == 1 and aks.count(sys.argv[3]) == 1
for a in (sys.argv[2], sys.argv[3]):
    e = keys[a]; assert e["owner"] == "cb-0123456789ab" and e["qps"] == 4 and e["tenant"] == "cocoonbox-boxes"
t = {x["name"]: x for x in cfg["tenants"]}
assert "key_daily_cost_quota_micros" not in t["cocoonbox"] and t["cocoonbox"]["qps"] == 4
assert t["cocoonbox-boxes"]["key_daily_cost_quota_micros"] == 10000000 and t["cocoonbox-boxes"]["qps"] == 16
assert keys[shared]["tenant"] == "cocoonbox"
PY
[ "$(grep -cF "$key" "$t/gateway.yaml")" = 1 ] && ok "box key appears exactly once in the file" || no "box key count"

out=$(k list); echo "$out" | grep -qx cb-0123456789ab && echo "$out" | grep -qx cb-0123456789ab-r2 && ok "list shows both ids" || no "list: $out"
[ -n "$key" ] && echo "$out" | grep -qF "$key" && no "list leaked a key" || { [ -n "$key" ] && ok "list never prints keys" || no "list leak check had no key"; }

[ "$(k revoke cb-0123456789ab)" = ok ] && [ ! -e "$t/keys/cb-0123456789ab" ] && ok "revoke removes the key" || no "revoke"
grep -qF "$key" "$t/gateway.yaml" && no "revoked key still rendered" || ok "revoked key gone from the render"
[ "$(k revoke cb-0123456789ab)" = ok ] && ok "revoke of an absent key is ok" || no "revoke absent"

out=$(printf 'add cb-00000000000a\n' | sh "$src/cb-gw-key" 2>/dev/null); case "$out" in ok\ cb-*) ok "stdin request (socket mode)" ;; *) no "stdin: $out" ;; esac
out=$(printf 'add cb-00000000000a extra\n' | sh "$src/cb-gw-key" 2>/dev/null); case "$out" in error*) ok "stdin extra args rejected" ;; *) no "stdin extra: $out" ;; esac

sed '/__CB_BOX_KEYS__/d' "$src/gateway.yaml" > "$t/nomarker.yaml"
CB_GW_TEMPLATE="$t/nomarker.yaml" sh "$src/install-gw-config" 2>/dev/null && no "render without marker succeeded" || ok "render refuses a template without the marker"

echo "passed $pass failed $failn"
[ "$failn" = 0 ]
