#!/bin/sh
# Builds the release binary with every build-specific path remapped, so the
# same commit yields the same bytes whatever the checkout dir, target dir or CARGO_HOME.
set -eu
cd "$(dirname "$0")"
src=$(pwd)
target=${CARGO_TARGET_DIR:-$src/target}
cargo_home=${CARGO_HOME:-$HOME/.cargo}
sep=$(printf '\037')
root=$(cd .. && pwd)
fixed=/tmp/cocoonbox-release-src
ln -sfn "$root" "$fixed"
# rustc applies the last matching remap, so the most specific prefix goes last
CARGO_ENCODED_RUSTFLAGS="--remap-path-prefix=$root=/src$sep--remap-path-prefix=$fixed=/src$sep--remap-path-prefix=$cargo_home=/cargo$sep--remap-path-prefix=$target=/target"
export CARGO_ENCODED_RUSTFLAGS
# a git archive has no .git, so its builder sets CB_AGENT_REV
CB_AGENT_REV=${CB_AGENT_REV:-$(git -C "$src" rev-parse --short HEAD)}
export CB_AGENT_REV
CARGO_TARGET_DIR=$target cargo build --manifest-path "$fixed/agent/Cargo.toml" --release --locked "$@"
sha256sum "$target/release/cocoonbox-agent" 2>/dev/null || shasum -a 256 "$target/release/cocoonbox-agent"
