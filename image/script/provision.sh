#!/bin/bash
# Adds the cbplan family's Script runtime and its own cua-driver to a provisioned desktop guest; idempotent.
# Runs as root inside the guest from an unpacked copy of image/script (see cut.sh -f cbplan).
set -euo pipefail

NODE_VERSION=24.21.0
NODE_SHA256=fd8e59d5a511510f6a298afb548f18c7d2b1be404d8b4a27d94fbe49f56cb2d6
NODE_URL="https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz"
CLI_VERSION=6.0.0
CLI_SHA256=2d1cb26121149772f77dfb8d1a1542c869bda7314aaf87fe23f994d420847db4
CLI_URL="https://registry.npmjs.org/@vendor/script/-/script-${CLI_VERSION}.tgz"
JS_VERSION=14.0.0
JS_SHA256=a15b58671bcec8dc19941cac1b3df27f2be8061bd1af6e64a643856b3e6e45de
JS_URL="https://registry.npmjs.org/@vendor/script-js/-/script-js-${JS_VERSION}.tgz"
CUA_VERSION=0.34.0
CUA_SHA256=8851c028e5d88a3b779ad2a8d49ed6f4b92aa94de2edf5c2851cee63bcc274c5
CUA_TARBALL="cua-driver-rs-${CUA_VERSION}-linux-x86_64.tar.gz"
CUA_URL="https://github.com/trycua/cua/releases/download/cua-driver-rs-v${CUA_VERSION}/${CUA_TARBALL}"
# private release asset; cut.sh brings it in as vendor/, since the cut host holds no token
NATIVE_RELEASE=v14.0.0-cmgs.1
NATIVE_SHA256=d63405e94c61eaa8438eff385fa8cc29d9ae23c429d79d353817862a2d2ec109
NATIVE_URL="https://github.com/vendor/script-js-cmgs/releases/download/${NATIVE_RELEASE}/script-js.linux-x64-gnu.node"
ROOT=/opt/script

SRC="$(cd "$(dirname "$0")" && pwd)"

log() { printf '[script] %s\n' "$*"; }

fetch() {
	curl -fsSL "$1" -o "$2"
	echo "$3  $2" | sha256sum -c --quiet -
}

install_runtime() {
	log "cli $CLI_VERSION, script-js $JS_VERSION ($NATIVE_RELEASE), node $NODE_VERSION"
	local node=$ROOT/node-v$NODE_VERSION cli=$ROOT/cli-$CLI_VERSION js=$ROOT/js-$NATIVE_RELEASE native="$SRC/vendor/script-js.linux-x64-gnu.node" tmp
	tmp=$(mktemp -d)
	if [ ! -x "$node/bin/node" ]; then
		fetch "$NODE_URL" "$tmp/node.tar.xz" "$NODE_SHA256"
		install -d "$node"
		tar -xJf "$tmp/node.tar.xz" -C "$node" --strip-components=1
	fi
	if [ ! -f "$cli/package.json" ]; then
		fetch "$CLI_URL" "$tmp/cli.tgz" "$CLI_SHA256"
		install -d "$cli"
		tar -xzf "$tmp/cli.tgz" -C "$cli" --strip-components=1 --no-same-owner
	fi
	if [ ! -f "$js/script-js.linux-x64-gnu.node" ]; then
		[ -f "$native" ] || { echo "missing $native: put $NATIVE_URL there" >&2; exit 1; }
		echo "$NATIVE_SHA256  $native" | sha256sum -c --quiet -
		fetch "$JS_URL" "$tmp/js.tgz" "$JS_SHA256"
		install -d "$js"
		tar -xzf "$tmp/js.tgz" -C "$js" --strip-components=1 --no-same-owner
		install -m 0755 "$native" "$js/script-js.linux-x64-gnu.node"
	fi
	rm -rf "$tmp"
	ln -sfn "$node" "$ROOT/node"
	ln -sfn "$cli" "$ROOT/cli"
	ln -sfn "$js" "$ROOT/js"
	find "$ROOT" -mindepth 1 -maxdepth 1 -name 'js-*' ! -name "js-$NATIVE_RELEASE" -exec rm -rf {} +
}

install_cua_driver() {
	log "cua-driver $CUA_VERSION"
	local dir=/opt/cua-driver/$CUA_VERSION tmp
	if [ ! -x "$dir/cua-driver" ]; then
		tmp=$(mktemp -d)
		fetch "$CUA_URL" "$tmp/$CUA_TARBALL" "$CUA_SHA256"
		install -d "$dir"
		tar -xzf "$tmp/$CUA_TARBALL" -C "$dir" --strip-components=1 --no-same-owner
		rm -rf "$tmp"
	fi
	ln -sfn "$dir/cua-driver" /usr/local/bin/cua-driver
	find /opt/cua-driver -mindepth 1 -maxdepth 1 ! -name "$CUA_VERSION" -exec rm -rf {} +
	runuser -u user -- env HOME=/home/user /usr/local/bin/cua-driver telemetry disable >/dev/null
	runuser -u user -- env HOME=/home/user /usr/local/bin/cua-driver telemetry reset-id >/dev/null
	systemctl --user -M user@ restart cua-driver.service
}

install_rootfs() {
	log "rootfs files"
	(cd "$SRC/rootfs" && find . -type f) | while read -r f; do
		case "$f" in
		./usr/local/bin/*) install -D -m 0755 "$SRC/rootfs/$f" "/${f#./}" ;;
		*) install -D -m 0644 "$SRC/rootfs/$f" "/${f#./}" ;;
		esac
	done
	sed -i -e "/^<!-- routing -->\$/{r $SRC/routing.md" -e 'd}' "$ROOT/AGENTS.md"
}

check() {
	"$ROOT/node/bin/node" -e "require('$ROOT/js')"
	runuser -u user -- env HOME=/home/user /usr/local/bin/script --version
	[ ! -e /home/user/.claude/skills/script ] && [ ! -e /root/.claude/skills/script ]
	grep -qxF '## Pick the tool tier first' "$ROOT/AGENTS.md"
	[ "$(grep -cF '<!-- routing -->' "$ROOT/AGENTS.md")" = 0 ]
	[ "$(readlink -f /usr/local/bin/cua-driver)" = "/opt/cua-driver/$CUA_VERSION/cua-driver" ]
}

install_runtime
install_cua_driver
install_rootfs
check
log "done"
