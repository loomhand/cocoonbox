#!/bin/bash
# Regenerates Sources/CocoonBoxKit/Generated from ../proto with the protoc
# plugins built from the pinned swift-protobuf and grpc-swift-protobuf.
# agent/v1/state.proto is left out: only the api calls State.
set -euo pipefail

cd "$(dirname "$0")/.."
source scripts/toolchain.sh

swift build -c release --product protoc-gen-swift >/dev/null
swift build -c release --product protoc-gen-grpc-swift-2 >/dev/null

out=Sources/CocoonBoxKit/Generated
rm -rf "$out"
mkdir -p "$out"
protoc \
  --proto_path=../proto \
  --plugin=protoc-gen-swift=.build/release/protoc-gen-swift \
  --plugin=protoc-gen-grpc-swift=.build/release/protoc-gen-grpc-swift-2 \
  --swift_opt=Visibility=Public,FileNaming=PathToUnderscores \
  --swift_out="$out" \
  --grpc-swift_opt=Visibility=Public,Server=true,Client=true,FileNaming=PathToUnderscores \
  --grpc-swift_out="$out" \
  $(cd ../proto && find cocoonbox -name '*.proto' ! -name state.proto | sort)
ls "$out"
