#!/bin/bash
# Pins the shuttle archive (desk daemon and runtime) and installs it; sourced by the desk and Android provisioning, or run as
# `shuttle.sh fetch DIR` to download it into DIR (image/desk/vendor for a cut). SHUTTLE_ARCH (x64 or arm64) overrides the machine's architecture.
SHUTTLE_VERSION=v0.1.40
case ${SHUTTLE_ARCH:-$(uname -m)} in
aarch64 | arm64) SHUTTLE_ARCH=arm64 SHUTTLE_SHA256=6e3419e085ec617bb59b19d93de810841fbb3a88e7155a1d4d9a3f441a069bf0 ;;
*) SHUTTLE_ARCH=x64 SHUTTLE_SHA256=d3c5309470c1ea2571abd315bd2d061c37fb65ac4f09d8a81d64c361127afa04 ;;
esac
SHUTTLE_TARBALL="shuttle-${SHUTTLE_VERSION}-linux-${SHUTTLE_ARCH}.tar.gz"
SHUTTLE_URL="https://github.com/loomhand/shuttle/releases/download/${SHUTTLE_VERSION}/${SHUTTLE_TARBALL}"

# install_shuttle TARBALL: the daemon with its bundled Node, cua-driver and runtime in /opt/loomhand/shuttle/VERSION, on PATH
install_shuttle() {
	local tarball=$1 dir=/opt/loomhand/shuttle/$SHUTTLE_VERSION
	echo "$SHUTTLE_SHA256  $tarball" | sha256sum -c --quiet -
	install -d "$dir"
	tar -xzf "$tarball" -C "$dir" --strip-components=1 --no-same-owner
	ln -sfn "$dir/shuttle" /usr/local/bin/shuttle
	find /opt/loomhand/shuttle -mindepth 1 -maxdepth 1 ! -name "$SHUTTLE_VERSION" -exec rm -rf {} +
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
