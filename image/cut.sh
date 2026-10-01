#!/bin/bash
# Cuts desktop template cbdesk:<vN> on the cbsbx cocoon root: boot <base template>, provision, seal, stop, export, import.
# usage: sudo image/cut.sh [-p] [-a <agent binary>] <base template> <vN>   (-p reruns provision.sh; logs every step to ~/ops-audit.md)
set -Eeuo pipefail
export LC_ALL=C

PROVISION=0
AGENT=""
while getopts pa: o; do
	case $o in
	p) PROVISION=1 ;;
	a) AGENT=$OPTARG ;;
	*) exit 2 ;;
	esac
done
shift $((OPTIND - 1))
BASE=${1:?usage: cut.sh [-p] [-a agent] <base template> <vN>}
V=${2:?usage: cut.sh [-p] [-a agent] <base template> <vN>}
S=/srv/cb/sbx
COCOON=$S/bin/cocoon-cbsbx
HOST_PUBKEY_FILE=${HOST_PUBKEY_FILE:-/home/builder/.ssh/cocoonbox_deploy.pub}
PROXY=http://127.0.0.1:3128
NO_PROXY=localhost,127.0.0.1,::1
EGRESS_PORT=2049
AUDIT=${AUDIT:-$(getent passwd "${SUDO_USER:-root}" | cut -d: -f6)/ops-audit.md}
TEMPLATE=cbdesk:$V
VM=cbcut-$V
TARBALL=$S/build/cocoonbox-desktop-$V.tar.gz
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
active() { guest "$VM" -- systemctl -q is-active cocoonbox-xorg cocoonbox-session 2>/dev/null; }

trap fail ERR
trap 'stop_proxy' EXIT
if [ -n "$AGENT" ] && [ ! -x "$AGENT" ]; then
	echo "agent binary $AGENT is not executable" >&2
	exit 1
fi
if [ -e "$TARBALL" ]; then
	echo "$TARBALL exists" >&2
	exit 1
fi

step "boot $BASE as $VM"
"$COCOON" vm run --name "$VM" --cpu 4 --memory 8G --storage 40G --nics 0 --no-direct-io "$BASE"
for _ in $(seq 180); do active && break; sleep 1; done
active

if [ "$PROVISION" = 1 ]; then
	step "provision"
	VSOCK=$("$COCOON" vm inspect "$VM" | python3 -c 'import json,sys; d=json.load(sys.stdin); print((d[0] if isinstance(d,list) else d)["vsock_socket"])')
	python3 "$SRC/cut-proxy.py" "${VSOCK}_$EGRESS_PORT" &
	PROXY_PID=$!
	tar -C "$SRC" -cf - provision.sh rootfs | guest -i "$VM" -- sh -c 'rm -rf /tmp/cb-image && mkdir -p /tmp/cb-image && tar -xf - -C /tmp/cb-image'
	guest -e "CB_HOST_PUBKEY=$(cat "$HOST_PUBKEY_FILE")" -e "http_proxy=$PROXY" -e "https_proxy=$PROXY" -e "no_proxy=$NO_PROXY" \
		"$VM" -- bash /tmp/cb-image/provision.sh
	guest "$VM" -- rm -rf /tmp/cb-image
	stop_proxy
fi
if [ -n "$AGENT" ]; then
	step "agent $(sha256sum "$AGENT" | cut -c1-16)"
	guest -i "$VM" -- sh -c 'cat > /tmp/cocoonbox-agent' < "$AGENT"
	guest "$VM" -- sh -c 'install -m 0755 /tmp/cocoonbox-agent /usr/local/bin/cocoonbox-agent && rm /tmp/cocoonbox-agent && systemctl enable --now cocoonbox-toolhost cocoonbox-agent'
fi

step "seal"
guest -i "$VM" -- bash -s < "$SRC/seal.sh"
step "stop"
"$COCOON" vm stop "$VM"
step "export $TARBALL"
"$SRC/export-rootfs.sh" "$VM" "$S/bin/silkd" "$TARBALL"
"$COCOON" vm rm "$VM"
step "import"
"$COCOON" image import "$TEMPLATE" "$TARBALL"
step "done: $(du -h "$TARBALL" | cut -f1) $(sha256sum "$TARBALL" | cut -c1-16)"
