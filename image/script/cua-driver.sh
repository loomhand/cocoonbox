#!/bin/bash
# Builds a cua-driver tarball from CMGS/cua the way upstream's cd-rust-cua-driver.yml builds its linux-x86_64 release:
# debian:11 with the same apt snapshot, rust 1.97.1, the same cargo command and GitHub container paths, here in a chroot.
# usage: sudo [COMMIT=<sha> VERSION=<name>] image/script/cua-driver.sh <out dir>; the defaults build the pin of both families, and
# COMMIT=<sha of tag cua-driver-rs-v0.34.0> rebuilds the upstream binary byte for byte.
set -euo pipefail
export LC_ALL=C

REPO=https://github.com/CMGS/cua.git
COMMIT=${COMMIT:-e08dbbe487f8ec4a7ac066953f0f7569427f8406}
VERSION=${VERSION:-0.34.0-cmgs.4}
UPSTREAM_SHA256=8851c028e5d88a3b779ad2a8d49ed6f4b92aa94de2edf5c2851cee63bcc274c5
UPSTREAM_URL=https://github.com/trycua/cua/releases/download/cua-driver-rs-v0.34.0/cua-driver-rs-0.34.0-linux-x86_64.tar.gz
DEBIAN=sha256:c0a2ad73611131275b0e9a2e7544cfe3725f4268d4085d8c6f61fdd55aef7917
SNAPSHOT=20260901T000000Z
RUST=1.97.1
W=/__w/cua/cua
PATHS=(libs/cua-driver libs/cua/Cargo.toml libs/cua/crates/cua-telemetry libs/cua/crates/cua-teleport/src/ux/targets.json libs/images/sandbox-images.json LICENSE.md)
OUT=$(realpath "${1:?usage: cua-driver.sh <out dir>}")
TMP=$(mktemp -d /var/tmp/cua-driver.XXXXXX)
R=$TMP/root

cleanup() {
	for m in "$R/dev" "$R/sys" "$R/proc"; do
		if mountpoint -q "$m"; then umount -R "$m" || true; fi
	done
	if grep -qF " $R/" /proc/self/mounts; then
		echo "mounts left under $R; $TMP kept" >&2
		return
	fi
	rm -rf "$TMP"
}
trap cleanup EXIT
in_root() { chroot "$R" /usr/bin/env -i HOME=/github/home PATH=/github/home/.cargo/bin:/usr/local/bin:/usr/bin:/bin LANG=C.UTF-8 /bin/bash -euo pipefail -c "$1"; }

token=$(curl -fsS "https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/debian:pull" | jq -r .token)
layer=$(curl -fsS -H "Authorization: Bearer $token" -H "Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json" \
	"https://registry-1.docker.io/v2/library/debian/manifests/$DEBIAN" | jq -r '.layers[0].digest')
curl -fsSL -H "Authorization: Bearer $token" -o "$TMP/debian.tgz" "https://registry-1.docker.io/v2/library/debian/blobs/$layer"
echo "${layer#sha256:}  $TMP/debian.tgz" | sha256sum -c --quiet -
mkdir -p "$R" && tar -xzf "$TMP/debian.tgz" -C "$R"
cp /etc/resolv.conf "$R/etc/resolv.conf"
cat > "$R/etc/apt/sources.list" <<EOF
deb [check-valid-until=no signed-by=/usr/share/keyrings/debian-archive-keyring.gpg] http://snapshot.debian.org/archive/debian/$SNAPSHOT/ bullseye main
deb [check-valid-until=no signed-by=/usr/share/keyrings/debian-archive-keyring.gpg] http://snapshot.debian.org/archive/debian/$SNAPSHOT/ bullseye-updates main
deb [check-valid-until=no signed-by=/usr/share/keyrings/debian-archive-keyring.gpg] http://snapshot.debian.org/archive/debian-security/$SNAPSHOT/ bullseye-security main
EOF
for m in proc sys dev; do mount --rbind "/$m" "$R/$m" && mount --make-rslave "$R/$m"; done
in_root 'apt-get -o Dir::Etc::sourceparts=- -o Acquire::Retries=5 update >/dev/null
DEBIAN_FRONTEND=noninteractive apt-get -o Dir::Etc::sourceparts=- -o Acquire::Retries=5 install -y --no-install-recommends \
	git ca-certificates curl python3 build-essential pkg-config libx11-dev libxi-dev libxtst-dev libxext-dev libwayland-dev libxkbcommon-dev >/dev/null'
in_root "curl -fsSL https://sh.rustup.rs -o /tmp/rustup.sh && sh /tmp/rustup.sh -y -q --profile minimal --default-toolchain $RUST --target x86_64-unknown-linux-gnu >/dev/null 2>&1"

git init -q "$TMP/src"
git -C "$TMP/src" remote add origin "$REPO"
git -C "$TMP/src" fetch -q --depth 1 --filter=blob:none origin "$COMMIT"
mkdir -p "$R$W"
git -C "$TMP/src" archive FETCH_HEAD "${PATHS[@]}" | tar -x -C "$R$W"
in_root "cd $W/libs/cua-driver/rust && cargo build -q --locked -p cua-driver -p cursor-theme-cli -p cua-driver-sdk --release --features cua-driver/portal-input --target x86_64-unknown-linux-gnu"

curl -fsSL -o "$TMP/upstream.tgz" "$UPSTREAM_URL"
echo "$UPSTREAM_SHA256  $TMP/upstream.tgz" | sha256sum -c --quiet -
tar -xzf "$TMP/upstream.tgz" -C "$TMP"
stage=$TMP/cua-driver-rs-$VERSION-linux-x86_64
mv "$TMP/cua-driver-rs-0.34.0-linux-x86_64" "$stage"
for f in cua-driver cua-cursor-theme libcua_driver_sdk.so; do
	install -m 0755 "$R$W/libs/cua-driver/rust/target/x86_64-unknown-linux-gnu/release/$f" "$stage/$f"
done
mkdir -p "$OUT"
tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -C "$TMP" -cf - "$(basename "$stage")" | gzip -n -9 > "$OUT/$(basename "$stage").tar.gz"
sha256sum "$stage/cua-driver" "$OUT/$(basename "$stage").tar.gz"
