#!/bin/bash
# Pins silkd as sandbox's guest images carry it (the silkd carrier image); sourced by the cut scripts, or run as
# `silkd.sh fetch DIR` to pack the carrier's /silkd and /silkd-static into DIR (image/desk/vendor for a cut).
SILKD_VERSION=0.1.37
SILKD_IMAGE=ghcr.io/cocoonstack/sandbox/silkd:0.1.37-cc55400@sha256:e22e8b6e44cb97d8f0a25136bad2d34e4694417133b5fc6471e9b418f21ab0bb
SILKD_SHA256=3a44a0117bbf4560c18a4c00c8613eff4215eb4f10310c55348d43d2843bb193
SILKD_STATIC_SHA256=3fe7b9cb58297761ab0628cb419928774cd55c60789be30d4d81ad2b1ead240d
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
