#!/bin/bash
# Checks record-packages and replay-packages in a throwaway ubuntu:24.04 container: a package the replay fails stays listed until it installs or the user removes it.
set -euo pipefail

IMAGE="$(cd "$(dirname "$0")/.." && pwd)"
docker run --rm -i -v "$IMAGE/rootfs:/rootfs:ro" ubuntu:24.04 bash -euo pipefail <<'IN'
export DEBIAN_FRONTEND=noninteractive LC_ALL=C
apt-get update -qq >/dev/null
useradd -m user
install -d /etc/cocoonbox
install -m 0755 /rootfs/usr/local/lib/cocoonbox/record-packages /rootfs/usr/local/lib/cocoonbox/replay-packages -D -t /usr/local/lib/cocoonbox
install -m 0644 /rootfs/etc/apt/apt.conf.d/90cocoonbox-packages /etc/apt/apt.conf.d/
apt-mark showmanual | sort -u > /etc/cocoonbox/packages.baseline
list=/home/user/.config/cocoonbox/packages
fails=0
expect() {
	local got
	got=$(tr '\n' ' ' < "$list")
	if [ "$got" = "$2" ]; then echo "ok   $1"; else echo "FAIL $1: want [$2] got [$got]"; fails=$((fails + 1)); fi
}
apt-get install -y -qq cowsay >/dev/null
expect "an install is listed" "cowsay "
printf 'cowsay\nno-such-package-cb\nsl\n' > "$list"
/usr/local/lib/cocoonbox/replay-packages >/dev/null 2>&1 || true
expect "the replay installs what it can and keeps what failed" "cowsay no-such-package-cb sl "
apt-get install -y -qq figlet >/dev/null
expect "a later install keeps the failed package" "cowsay figlet no-such-package-cb sl "
apt-get remove -y -qq cowsay >/dev/null
expect "a removal drops the package and keeps the failed one" "figlet no-such-package-cb sl "
echo fortune-mod >> /etc/cocoonbox/packages.pending
apt-get install -y -qq fortune-mod >/dev/null
apt-get remove -y -qq fortune-mod >/dev/null
expect "a pending package that installed and was removed stays removed" "figlet no-such-package-cb sl "
[ "$fails" = 0 ] || exit 1
IN
