#!/bin/bash
# The shuttle archive cbcore runs as its desk daemon, and whose desk runtime (desk.mts, gui_plan.mts, png.mts) cbcore and the
# Android base install in /opt/desk/lib. Sourced by image/desk/provision.sh and image/android/provision.sh; run as
# `shuttle.sh fetch DIR` it downloads the pinned archive into DIR (image/desk/vendor for a cut; cb-image-cut copies the
# runner's vendor tarballs there). The archive follows the machine's architecture; SHUTTLE_ARCH (x64 or arm64) names
# another one, as a fetch for a different machine needs.
SHUTTLE_VERSION=v0.1.10
case ${SHUTTLE_ARCH:-$(uname -m)} in
aarch64 | arm64) SHUTTLE_ARCH=arm64 SHUTTLE_SHA256=eb6804e32d6ba5bfb28871108026c5365c3809de718109407689d85f9a3517f6 ;;
*) SHUTTLE_ARCH=x64 SHUTTLE_SHA256=7db84e97a99a74fdc32c49cbf6317671cad09be6df6fe65e5262bb6a116b6119 ;;
esac
SHUTTLE_TARBALL="cocoon-shuttle-${SHUTTLE_VERSION}-linux-${SHUTTLE_ARCH}.tar.gz"
SHUTTLE_URL="https://github.com/loomhand/shuttle/releases/download/${SHUTTLE_VERSION}/${SHUTTLE_TARBALL}"

# install_shuttle TARBALL: the daemon with its bundled Node, cua-driver and runtime in /opt/cocoon-shuttle/VERSION, on PATH
install_shuttle() {
	local tarball=$1 dir=/opt/cocoon-shuttle/$SHUTTLE_VERSION
	echo "$SHUTTLE_SHA256  $tarball" | sha256sum -c --quiet -
	install -d "$dir"
	tar -xzf "$tarball" -C "$dir" --strip-components=1 --no-same-owner
	ln -sfn "$dir/cocoon-shuttle" /usr/local/bin/cocoon-shuttle
	find /opt/cocoon-shuttle -mindepth 1 -maxdepth 1 ! -name "$SHUTTLE_VERSION" -exec rm -rf {} +
}

install_shuttle_runtime() {
	local tarball=$1 tmp
	echo "$SHUTTLE_SHA256  $tarball" | sha256sum -c --quiet -
	tmp=$(mktemp -d)
	tar -xzf "$tarball" -C "$tmp" --no-same-owner cocoon-shuttle/runtime
	for f in desk gui_plan png; do install -D -m 0644 "$tmp/cocoon-shuttle/runtime/$f.mts" "/opt/desk/lib/$f.mts"; done
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
