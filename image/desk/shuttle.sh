#!/bin/bash
# The shuttle archive cbcore runs as its desk daemon, and whose desk runtime (desk.mts, gui_plan.mts, png.mts) cbcore and the
# Android base install in /opt/desk/lib. Sourced by image/desk/provision.sh and image/android/provision.sh; run as
# `shuttle.sh fetch DIR` it downloads the pinned archive into DIR (image/desk/vendor for a cut; cb-image-cut copies the
# runner's vendor tarballs there). The archive follows the machine's architecture; SHUTTLE_ARCH (x64 or arm64) names
# another one, as a fetch for a different machine needs.
SHUTTLE_VERSION=v0.1.15
case ${SHUTTLE_ARCH:-$(uname -m)} in
aarch64 | arm64) SHUTTLE_ARCH=arm64 SHUTTLE_SHA256=c42d4558acbcf59f2c780248ee525ee5cfda31f93846505f8304010372baa52a ;;
*) SHUTTLE_ARCH=x64 SHUTTLE_SHA256=9b8703638008403e82efc3cfec84afec66055e057732fd6f5dc93d2dbdee930f ;;
esac
SHUTTLE_TARBALL="shuttle-${SHUTTLE_VERSION}-linux-${SHUTTLE_ARCH}.tar.gz"
SHUTTLE_URL="https://github.com/loomhand/shuttle/releases/download/${SHUTTLE_VERSION}/${SHUTTLE_TARBALL}"

# install_shuttle TARBALL: the daemon with its bundled Node, cua-driver and runtime in /opt/loomhand/shuttle/VERSION, on PATH;
# the install under the pre-0.1.13 name goes
install_shuttle() {
	local tarball=$1 dir=/opt/loomhand/shuttle/$SHUTTLE_VERSION
	echo "$SHUTTLE_SHA256  $tarball" | sha256sum -c --quiet -
	install -d "$dir"
	tar -xzf "$tarball" -C "$dir" --strip-components=1 --no-same-owner
	ln -sfn "$dir/shuttle" /usr/local/bin/shuttle
	find /opt/loomhand/shuttle -mindepth 1 -maxdepth 1 ! -name "$SHUTTLE_VERSION" -exec rm -rf {} +
	rm -rf /opt/cocoon-shuttle /usr/local/bin/cocoon-shuttle
}

install_shuttle_runtime() {
	local tarball=$1 tmp
	echo "$SHUTTLE_SHA256  $tarball" | sha256sum -c --quiet -
	tmp=$(mktemp -d)
	tar -xzf "$tarball" -C "$tmp" --no-same-owner shuttle/runtime
	for f in desk gui_plan png; do install -D -m 0644 "$tmp/shuttle/runtime/$f.mts" "/opt/desk/lib/$f.mts"; done
	rm -rf "$tmp"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	set -euo pipefail
	[ "${1:-}" = fetch ] && [ -n "${2:-}" ] || { echo "usage: $0 fetch DIR" >&2; exit 2; }
	mkdir -p "$2"
	curl -fsSL "$SHUTTLE_URL" -o "$2/$SHUTTLE_TARBALL.tmp"
	echo "$SHUTTLE_SHA256  $2/$SHUTTLE_TARBALL.tmp" | sha256sum -c --quiet -
	mv -f "$2/$SHUTTLE_TARBALL.tmp" "$2/$SHUTTLE_TARBALL"
	echo "$2/$SHUTTLE_TARBALL"
fi
