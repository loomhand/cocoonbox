#!/bin/bash
# Pins silkd as sandbox's guest images carry it (the silkd carrier image); sourced by the cut scripts, or run as
# `silkd.sh fetch DIR` to pack the carrier's /silkd and /silkd-static into DIR (image/desk/vendor for a cut).
SILKD_VERSION=0.1.31
SILKD_IMAGE=ghcr.io/cocoonstack/sandbox/silkd:0.1.31-c4c1eb2@sha256:97862f7b61e7e719992014dd5aff4985504b0a664bb5b66185b950f8990dfaba
SILKD_SHA256=2acdcb84d785b5e5c1f4ae0d8193a974870d3c90d98c88e21440df1b5ef1fb50
SILKD_STATIC_SHA256=01c3e31a29651338d98b3239cb0cedcd0dba839783aa897f299448d8903d0e3c
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
