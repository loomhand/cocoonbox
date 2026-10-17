#!/bin/bash
# Cuts Android template <family>:<vN> on the cbsbx cocoon root: boot the base, agent, seal, export, import.
# usage: sudo image/android/cut.sh [-f <family>] -a <agent binary> [-d <docker data dir>] <base image> <vN>
#   the base is an android-base image (base/Dockerfile); with -d it is a plain Ubuntu 24.04 cocoon image that the cut
#   provisions here, with a /var/lib/docker holding one created, never stopped redroid container (base/bake.sh bakes one);
#   every step goes to ~/ops-audit.md
set -Eeuo pipefail
export LC_ALL=C

FAMILY=cbdroid
AGENT=""
DOCKER=""
while getopts f:a:d: o; do
	case $o in
	f) FAMILY=$OPTARG ;;
	a) AGENT=$OPTARG ;;
	d) DOCKER=$OPTARG ;;
	*) exit 2 ;;
	esac
done
shift $((OPTIND - 1))
BASE=${1:?usage: cut.sh [-f family] -a agent -d docker-data <base image> <vN>}
V=${2:?usage: cut.sh [-f family] -a agent -d docker-data <base image> <vN>}
S=/srv/cb/sbx
COCOON=$S/bin/cocoon-cbsbx
PROXY=http://127.0.0.1:3128
NO_PROXY=localhost,127.0.0.1,::1
EGRESS_PORT=2049
AUDIT=${AUDIT:-$(getent passwd "${SUDO_USER:-root}" | cut -d: -f6)/ops-audit.md}
case $FAMILY in
*[!a-z0-9]* | [!a-z]*) echo "family $FAMILY is not [a-z][a-z0-9]*" >&2; exit 2 ;;
esac
VM=cbcut-$FAMILY-$V
TEMPLATE=$FAMILY:$V
TARBALL=$S/build/cocoonbox-$FAMILY-$V.tar.gz
SRC="$(cd "$(dirname "$0")" && pwd)"
STEP=start
PROXY_PID=""

step() {
	STEP=$1
	printf '%s [cut] %s %s\n' "$(date -u +%FT%TZ)" "$TEMPLATE" "$1" | tee -a "$AUDIT"
}
fail() {
	printf '%s [cut] %s FAILED at %s: %s\n' "$(date -u +%FT%TZ)" "$TEMPLATE" "$STEP" "$BASH_COMMAND" | tee -a "$AUDIT" >&2
	stop_proxy
	echo "vm $VM is kept; remove it with: sudo $COCOON vm rm --force $VM" >&2
}
stop_proxy() {
	[ -z "$PROXY_PID" ] || kill "$PROXY_PID" 2>/dev/null || true
	PROXY_PID=""
}
guest() { "$COCOON" vm exec "$@"; }

trap fail ERR
trap 'stop_proxy' EXIT
[ -x "$AGENT" ] || { echo "agent binary $AGENT is not executable" >&2; exit 1; }
[ -z "$DOCKER" ] || [ "$(find "$DOCKER/containers" -name config.v2.json 2>/dev/null | wc -l)" = 1 ] || { echo "$DOCKER holds no single baked container" >&2; exit 1; }
[ ! -e "$TARBALL" ] || { echo "$TARBALL exists" >&2; exit 1; }

step "boot $BASE as $VM"
"$COCOON" vm run --name "$VM" --cpu 4 --memory 8G --storage 40G --nics 0 --no-direct-io "$BASE"
for _ in $(seq 100); do guest "$VM" -- true 2>/dev/null && break; sleep 0.3; done
if [ -n "$DOCKER" ]; then
	# the base image has no silkd: a transient one serves the build's egress door
	guest -i "$VM" -- sh -c 'cat > /usr/local/bin/silkd && chmod 0755 /usr/local/bin/silkd' < "$S/bin/silkd"
	guest "$VM" -- systemd-run --unit=cbcut-silkd -E SILKD_PORT=2048 /usr/local/bin/silkd
	VSOCK=$("$COCOON" vm inspect "$VM" | python3 -c 'import json,sys; d=json.load(sys.stdin); print((d[0] if isinstance(d,list) else d)["vsock_socket"])')
	python3 "$SRC/../cut-proxy.py" "${VSOCK}_$EGRESS_PORT" &
	PROXY_PID=$!

	step "docker data $DOCKER"
	tar -C "$DOCKER" --numeric-owner --xattrs --xattrs-include='*' -cf - . | guest -i "$VM" -- sh -c 'rm -rf /var/lib/docker && mkdir /var/lib/docker && tar -C /var/lib/docker --numeric-owner --xattrs --xattrs-include="*" -xpf -'
	step "provision"
	tar -C "$SRC/.." -cf - android desk/rootfs/opt/desk/lib desk/rootfs/usr/local/bin/desk | guest -i "$VM" -- sh -c 'rm -rf /tmp/cb-image && mkdir -p /tmp/cb-image && tar -xf - -C /tmp/cb-image'
	guest -e "http_proxy=$PROXY" -e "https_proxy=$PROXY" -e "no_proxy=$NO_PROXY" "$VM" -- bash /tmp/cb-image/android/provision.sh
	stop_proxy
else
	guest "$VM" -- test -f /etc/cocoonbox/android-base || { echo "$BASE is no android base: give -d" >&2; exit 1; }
fi
step "agent $(sha256sum "$AGENT" | cut -c1-16)"
guest -i "$VM" -- sh -c 'cat > /tmp/cocoonbox-agent' < "$AGENT"
guest "$VM" -- sh -c 'install -m 0755 /tmp/cocoonbox-agent /usr/local/bin/cocoonbox-agent && rm /tmp/cocoonbox-agent && systemctl enable cocoonbox-toolhost cocoonbox-agent'

step "seal"
guest "$VM" -- bash -c 'set -e; grep -q " localhost.localdomain" /etc/hosts || echo "127.0.0.1 localhost.localdomain" >> /etc/hosts; rm -rf /tmp/cb-image /var/lib/apt/lists/* /root/.bash_history; apt-get clean; journalctl --rotate >/dev/null; journalctl --vacuum-time=1s >/dev/null; ! systemctl -q is-active docker.service; [ -z "$(ls -A /var/lib/redroid-data)" ]; sync'
step "stop"
"$COCOON" vm stop "$VM"
step "export $TARBALL"
"$SRC/../export-rootfs.sh" "$VM" "$S/bin/silkd" "$TARBALL"
"$COCOON" vm rm "$VM"
step "import"
"$COCOON" image import "$TEMPLATE" "$TARBALL"
step "done: $(du -h "$TARBALL" | cut -f1) $(sha256sum "$TARBALL" | cut -c1-16)"
