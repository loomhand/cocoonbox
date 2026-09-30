#!/bin/sh
# Builds the release binary with every build-specific path remapped, so the
# same commit yields the same bytes whatever the target dir or CARGO_HOME.
set -eu
cd "$(dirname "$0")"
src=$(pwd)
target=${CARGO_TARGET_DIR:-$src/target}
cargo_home=${CARGO_HOME:-$HOME/.cargo}
sep=$(printf '\037')
CARGO_ENCODED_RUSTFLAGS="--remap-path-prefix=$target=/target$sep--remap-path-prefix=$cargo_home=/cargo$sep--remap-path-prefix=$src=/src"
export CARGO_ENCODED_RUSTFLAGS
cargo build --release --locked "$@"
sha256sum "$target/release/cocoonbox-agent" 2>/dev/null || shasum -a 256 "$target/release/cocoonbox-agent"
