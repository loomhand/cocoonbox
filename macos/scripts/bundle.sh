#!/bin/bash
# Builds dist/CocoonBox.app (release, arm64) and signs it ad hoc.
#   scripts/bundle.sh [--clean]
set -euo pipefail

cd "$(dirname "$0")/.."
if [ "${1:-}" = "--clean" ]; then
  rm -rf .build dist
fi
source scripts/toolchain.sh

swift build -c release --product CocoonBox
swift build -c release --product cocoonbox-cli
bin="$(swift build -c release --show-bin-path)"

app=dist/CocoonBox.app
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources" "$app/Contents/Helpers"
cp "$bin/CocoonBox" "$app/Contents/MacOS/CocoonBox"
# the CLI sits in Helpers: next to CocoonBox in MacOS it would collide on a case-insensitive volume
cp "$bin/cocoonbox-cli" "$app/Contents/Helpers/cocoonbox"
cp -R "$bin/GhosttyKit_GhosttyTerminal.bundle" "$app/Contents/Resources/"
version="$(git describe --tags --always 2>/dev/null || echo dev)"

cat >"$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key><string>me.cmgs.cocoonbox</string>
  <key>CFBundleName</key><string>CocoonBox</string>
  <key>CFBundleDisplayName</key><string>CocoonBox</string>
  <key>CFBundleExecutable</key><string>CocoonBox</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>0.1.0</string>
  <key>CFBundleVersion</key><string>${version}</string>
  <key>LSMinimumSystemVersion</key><string>15.0</string>
  <key>LSApplicationCategoryType</key><string>public.app-category.developer-tools</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSPrincipalClass</key><string>NSApplication</string>
  <key>NSSupportsAutomaticGraphicsSwitching</key><true/>
</dict>
</plist>
PLIST

strip -x "$app/Contents/MacOS/CocoonBox" "$app/Contents/Helpers/cocoonbox"
codesign --force --sign - --identifier me.cmgs.cocoonbox.cli "$app/Contents/Helpers/cocoonbox"
codesign --force --sign - --identifier me.cmgs.cocoonbox "$app"
codesign --verify --strict "$app"
du -sh "$app"
echo "built $app"
