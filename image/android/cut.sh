#!/bin/bash
# Cuts Android template <family>:<vN> on the cbsbx cocoon root from an android-base image (base/Dockerfile): boot the
# base, install the agent, seal, export, import; every step goes to ~/ops-audit.md.
# usage: sudo image/android/cut.sh [-f <family>] -a <agent binary> <base image> <vN>
set -Eeuo pipefail
export LC_ALL=C

FAMILY=cbdroid
AGENT=""
while getopts f:a: o; do
	case $o in
	f) FAMILY=$OPTARG ;;
	a) AGENT=$OPTARG ;;
	*) exit 2 ;;
	esac
done
shift $((OPTIND - 1))
BASE=${1:?usage: cut.sh [-f family] -a agent <base image> <vN>}
V=${2:?usage: cut.sh [-f family] -a agent <base image> <vN>}
S=/srv/cb/sbx
COCOON=$S/bin/cocoon-cbsbx
AUDIT=${AUDIT:-$(getent passwd "${SUDO_USER:-root}" | cut -d: -f6)/ops-audit.md}
case $FAMILY in
*[!a-z0-9]* | [!a-z]*) echo "family $FAMILY is not [a-z][a-z0-9]*" >&2; exit 2 ;;
esac
VM=cbcut-$FAMILY-$V
TEMPLATE=$FAMILY:$V
TARBALL=$S/build/cocoonbox-$FAMILY-$V.tar.gz
SRC="$(cd "$(dirname "$0")" && pwd)"
STEP=start

step() {
	STEP=$1
	printf '%s [cut] %s %s\n' "$(date -u +%FT%TZ)" "$TEMPLATE" "$1" | tee -a "$AUDIT"
}
fail() {
	printf '%s [cut] %s FAILED at %s: %s\n' "$(date -u +%FT%TZ)" "$TEMPLATE" "$STEP" "$BASH_COMMAND" | tee -a "$AUDIT" >&2
	echo "vm $VM is kept; remove it with: sudo $COCOON vm rm --force $VM" >&2
}
guest() { "$COCOON" vm exec "$@"; }

trap fail ERR
[ -x "$AGENT" ] || { echo "agent binary $AGENT is not executable" >&2; exit 1; }
[ ! -e "$TARBALL" ] || { echo "$TARBALL exists" >&2; exit 1; }

step "boot $BASE as $VM"
"$COCOON" vm run --name "$VM" --cpu 4 --memory 8G --storage 40G --nics 0 --no-direct-io "$BASE"
for _ in $(seq 100); do guest "$VM" -- true 2>/dev/null && break; sleep 0.3; done
guest "$VM" -- test -f /etc/cocoonbox/android-base || { echo "$BASE is no android base" >&2; exit 1; }
step "agent $(sha256sum "$AGENT" | cut -c1-16)"
guest -i "$VM" -- sh -c 'cat > /tmp/cocoonbox-agent' < "$AGENT"
guest "$VM" -- sh -c 'install -m 0755 /tmp/cocoonbox-agent /usr/local/bin/cocoonbox-agent && rm /tmp/cocoonbox-agent && systemctl enable cocoonbox-agent'
guest "$VM" -- sh -c '/usr/local/bin/cocoonbox-agent toolhost-service 2>&1 | grep -q SHUTTLE_SERVICE_SOCKET' || { echo "$AGENT has no toolhost-service" >&2; exit 1; }

step "seal"
guest "$VM" -- bash -c 'set -e; grep -q " localhost.localdomain" /etc/hosts || echo "127.0.0.1 localhost.localdomain" >> /etc/hosts; rm -rf /var/lib/apt/lists/* /root/.bash_history; apt-get clean; journalctl --rotate >/dev/null; journalctl --vacuum-time=1s >/dev/null; ! systemctl -q is-active docker.service; [ -z "$(ls -A /var/lib/redroid-data)" ]; sync'
step "stop"
"$COCOON" vm stop "$VM"
step "export $TARBALL"
"$SRC/../export-rootfs.sh" "$VM" "$S/bin/silkd" "$TARBALL"
"$COCOON" vm rm "$VM"
step "import"
"$COCOON" image import "$TEMPLATE" "$TARBALL"
step "done: $(du -h "$TARBALL" | cut -f1) $(sha256sum "$TARBALL" | cut -c1-16)"
