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
# private release asset built by build-native.sh from script-js-cmgs; cut.sh brings it in as vendor/, since the cut host holds no token
JS_RELEASE=v14.0.0-cmgs.5
JS_SHA256=7188c9dcc62ea69e4316f065f91341079d70d09bd2e80ad5f83efa4d6b55d8f7
JS_ARTIFACT=script-js-90db155-linux-x64.tgz
JS_URL="https://github.com/vendor/script-js-cmgs/releases/download/${JS_RELEASE}/${JS_ARTIFACT}"
# CMGS/cua cmgs/x11-typing, built by cua-driver.sh; cut.sh brings it in as vendor/
CUA_VERSION=0.34.0-cmgs.6
CUA_SHA256=553a7cac7906a293f9ca6ebcf614bd7bc4d153de115f2fead68c7ee5fbf8907b
CUA_TARBALL="cua-driver-rs-${CUA_VERSION}-linux-x86_64.tar.gz"
ROOT=/opt/script
PRESS="require('$ROOT/lib/press.cjs')(module.exports)"

SRC="$(cd "$(dirname "$0")" && pwd)"

log() { printf '[script] %s\n' "$*"; }

fetch() {
	curl -fsSL "$1" -o "$2"
	echo "$3  $2" | sha256sum -c --quiet -
}

install_runtime() {
	log "cli $CLI_VERSION, script-js $JS_RELEASE, node $NODE_VERSION"
	local node=/opt/node/$NODE_VERSION cli=$ROOT/cli-$CLI_VERSION js=$ROOT/js-$JS_RELEASE artifact="$SRC/vendor/$JS_ARTIFACT" tmp
	tmp=$(mktemp -d)
	if [ ! -x "$node/bin/node" ]; then
		fetch "$NODE_URL" "$tmp/node.tar.xz" "$NODE_SHA256"
		install -d "$node"
		tar -xJf "$tmp/node.tar.xz" -C "$node" --strip-components=1 --no-same-owner
	fi
	if [ ! -f "$cli/package.json" ]; then
		fetch "$CLI_URL" "$tmp/cli.tgz" "$CLI_SHA256"
		install -d "$cli"
		tar -xzf "$tmp/cli.tgz" -C "$cli" --strip-components=1 --no-same-owner
	fi
	if [ ! -f "$js/script-js.linux-x64-gnu.node" ]; then
		[ -f "$artifact" ] || { echo "missing $artifact: put $JS_URL there" >&2; exit 1; }
		echo "$JS_SHA256  $artifact" | sha256sum -c --quiet -
		install -d "$js"
		tar -xzf "$artifact" -C "$js" --strip-components=1 --no-same-owner
	fi
	rm -rf "$tmp"
	ln -sfn "$node" "$ROOT/node"
	ln -sfn "$cli" "$ROOT/cli"
	ln -sfn "$js" "$ROOT/js"
	for b in node npm npx corepack; do ln -sfn "$node/bin/$b" "/usr/local/bin/$b"; done
	find "$ROOT" -mindepth 1 -maxdepth 1 \( -name 'node-v*' -o -name 'js-*' ! -name "js-$JS_RELEASE" \) -exec rm -rf {} +
}

install_cua_driver() {
	log "cua-driver $CUA_VERSION"
	local dir=/opt/cua-driver/$CUA_VERSION tarball="$SRC/vendor/$CUA_TARBALL"
	if [ ! -x "$dir/cua-driver" ]; then
		[ -f "$tarball" ] || { echo "missing $tarball: build it with image/cua-driver.sh image/script/vendor" >&2; exit 1; }
		echo "$CUA_SHA256  $tarball" | sha256sum -c --quiet -
		install -d "$dir"
		tar -xzf "$tarball" -C "$dir" --strip-components=1 --no-same-owner
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
	grep -qxF "$PRESS" "$ROOT/js/wrapped.js" || echo "$PRESS" >>"$ROOT/js/wrapped.js"
}

apt_sources() {
	log "apt sources"
	apt-get update -q >/dev/null
}

route_chrome() {
	log "chrome launchers"
	install -d /usr/local/share/applications
	for f in google-chrome com.google.Chrome; do
		sed 's|/usr/bin/google-chrome-stable|/usr/local/bin/cocoonbox-chrome|' "/usr/share/applications/$f.desktop" >"/usr/local/share/applications/$f.desktop"
	done
	update-alternatives --install /usr/bin/x-www-browser x-www-browser /usr/local/bin/cocoonbox-chrome 250 >/dev/null
}

check() {
	"$ROOT/node/bin/node" -e "require('$ROOT/js')"
	[ "$(readlink -f /usr/local/bin/node)" = "/opt/node/$NODE_VERSION/bin/node" ]
	[ "$(grep -cxF "$PRESS" "$ROOT/js/wrapped.js")" = 1 ]
	runuser -u user -- env HOME=/home/user /usr/local/bin/script --version
	[ ! -e /home/user/.claude/skills/script ] && [ ! -e /root/.claude/skills/script ]
	grep -qxF '## Pick the tool tier first' "$ROOT/AGENTS.md"
	[ "$(grep -cF '<!-- routing -->' "$ROOT/AGENTS.md")" = 0 ]
	[ "$(readlink -f /usr/local/bin/cua-driver)" = "/opt/cua-driver/$CUA_VERSION/cua-driver" ]
	[ "$(readlink -f /usr/bin/x-www-browser)" = /usr/local/bin/cocoonbox-chrome ]
	for f in google-chrome com.google.Chrome; do grep -qx 'Exec=/usr/local/bin/cocoonbox-chrome %U' "/usr/local/share/applications/$f.desktop"; done
	grep -qx 'URIs: mirror+file:/etc/apt/cocoonbox-ubuntu.mirrors' /etc/apt/sources.list.d/ubuntu.sources
	[ -s /var/lib/apt/lists/_etc_apt_cocoonbox-ubuntu.mirrors_dists_noble_InRelease ]
	[ -s /var/lib/apt/lists/_etc_apt_cocoonbox-ubuntu-security.mirrors_dists_noble-security_InRelease ]
}

install_runtime
install_cua_driver
install_rootfs
apt_sources
route_chrome
check
log "done"
