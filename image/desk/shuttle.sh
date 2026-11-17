#!/bin/bash
# Pins the shuttle archive (desk daemon and runtime) and installs it; sourced by the desk and Android provisioning, or run as
# `shuttle.sh fetch DIR` to download it into DIR (image/desk/vendor for a cut). SHUTTLE_ARCH (x64 or arm64) overrides the machine's architecture.
SHUTTLE_VERSION=v0.1.58
case ${SHUTTLE_ARCH:-$(uname -m)} in
aarch64 | arm64) SHUTTLE_ARCH=arm64 SHUTTLE_SHA256=ed16df24cc88b867245b4cfa26d65053681f707471ef0dfac0c91bc9a42d8b58 ;;
*) SHUTTLE_ARCH=x64 SHUTTLE_SHA256=70bd84b3d6238f30ceefbf69e56fb5a4e1521c4d9d0498efe793a79d30a4e92c ;;
esac
SHUTTLE_TARBALL="shuttle-${SHUTTLE_VERSION}-linux-${SHUTTLE_ARCH}.tar.gz"
SHUTTLE_URL="https://github.com/loomhand/shuttle/releases/download/${SHUTTLE_VERSION}/${SHUTTLE_TARBALL}"

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
	install -d /opt/desk/lib
	install -m 0644 "$tmp"/shuttle/runtime/*.mts /opt/desk/lib/
	rm -rf "$tmp"
}

desk_lib_resolves() {
	local f dep
	for f in "$1"/*.mts; do
		while read -r dep; do
			[ -s "$1/$dep" ] || { echo "$f imports $dep, which $1 lacks" >&2; return 1; }
		done < <(sed -n "s/.* from '\.\/\([a-z_]*\.mts\)'.*/\1/p" "$f")
	done
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
