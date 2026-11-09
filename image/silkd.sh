#!/bin/bash
# Pins silkd as sandbox's guest images carry it (the silkd carrier image); sourced by the cut scripts, or run as
# `silkd.sh fetch DIR` to pack the carrier's /silkd and /silkd-static into DIR (image/desk/vendor for a cut).
SILKD_VERSION=0.1.28
SILKD_IMAGE=ghcr.io/cocoonstack/sandbox/silkd:0.1.28-84b8a42@sha256:6098964d78540e0edd4f515df523c2300fcbc89646fa173dd361f5523a58851d
SILKD_SHA256=4db624533749d9544292c27be91be2a7d701b2a9bb9f1503db9d531e7850d06d
SILKD_STATIC_SHA256=02c4c2a09e354ce708c8f327e238bb747302bde5a5db5a428e211a2156ba1ea2
SILKD_TARBALL="silkd-${SILKD_VERSION}-linux-x64.tar.gz"

# unpack_silkd DIR NAME OUT: writes NAME (silkd or silkd-static) of DIR's silkd tarball to OUT and checks it against its pin.
unpack_silkd() {
	local want=$SILKD_SHA256
	[ "$2" = silkd ] || want=$SILKD_STATIC_SHA256
	tar -xzf "$1/$SILKD_TARBALL" -O "$2" > "$3" || return
	echo "$want  $3" | sha256sum -c --quiet - || return
	chmod 0755 "$3"
}

if [ "${1:-}" = fetch ]; then
	set -euo pipefail
	work=$(mktemp -d)
	cid=$(docker create --platform linux/amd64 "$SILKD_IMAGE" /silkd)
	trap 'docker rm "$cid" >/dev/null; rm -rf "$work"' EXIT
	docker cp "$cid:/silkd" "$work/silkd"
	docker cp "$cid:/silkd-static" "$work/silkd-static"
	echo "$SILKD_SHA256  $work/silkd" | sha256sum -c --quiet -
	echo "$SILKD_STATIC_SHA256  $work/silkd-static" | sha256sum -c --quiet -
	tar -czf "$2/$SILKD_TARBALL" -C "$work" silkd silkd-static
fi
