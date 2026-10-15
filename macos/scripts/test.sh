#!/bin/bash
# Builds and runs the swift-testing suite (see Package.swift for why it is an executable), with
# MockBox and the CLI, which the offload tests run.
set -euo pipefail

cd "$(dirname "$0")/.."
source scripts/toolchain.sh
for product in CocoonBoxKitTests MockBox cocoonbox-cli; do swift build --product "$product"; done
home="$(mktemp -d)"
keychain="$home/test.keychain-db"
trap 'HOME="$home" security delete-keychain "$keychain" 2>/dev/null; rm -rf "$home"' EXIT
HOME="$home" security create-keychain -p test "$keychain"
HOME="$home" security set-keychain-settings "$keychain"
HOME="$home" security unlock-keychain -p test "$keychain"
# newer Command Line Tools link Testing against lib_TestingInterop, which an older macOS does not carry
frameworks=${CB_TESTING_FRAMEWORKS:-/Library/Developer/CommandLineTools/Library/Developer/Frameworks}
interop=${CB_TESTING_INTEROP:-/Library/Developer/CommandLineTools/Library/Developer/usr/lib}
CFFIXED_USER_HOME="$home" CB_KEYCHAIN_FILE="$keychain" CB_KEYCHAIN_SERVICE=io.github.loomhand.cocoonbox.test DYLD_FRAMEWORK_PATH="$frameworks" DYLD_LIBRARY_PATH="$interop" .build/debug/CocoonBoxKitTests "$@"
