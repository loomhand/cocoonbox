#!/bin/bash
# Builds and runs the swift-testing suite (see Package.swift for why it is an executable), with
# MockBox and the CLI, which the offload tests run.
set -euo pipefail

cd "$(dirname "$0")/.."
source scripts/toolchain.sh
for product in CocoonBoxKitTests MockBox cocoonbox-cli; do swift build --product "$product"; done
home="$(mktemp -d)"
trap 'rm -rf "$home"' EXIT
# newer Command Line Tools link Testing against lib_TestingInterop, which an older macOS does not carry
interop=/Library/Developer/CommandLineTools/Library/Developer/usr/lib
CFFIXED_USER_HOME="$home" DYLD_FRAMEWORK_PATH=/Library/Developer/CommandLineTools/Library/Developer/Frameworks DYLD_LIBRARY_PATH="$interop" .build/debug/CocoonBoxKitTests "$@"
