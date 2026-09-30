#!/bin/bash
# Exports a golden snapshot as a flat rootfs tarball for `cocoon image import` on the sandboxd backend; read-only on the cb store.
# usage: sudo image/export-rootfs.sh <golden snapshot name> <silkd binary> <out dir>
set -euo pipefail
export LC_ALL=C

SNAP_NAME=${1:?usage: export-rootfs.sh <snapshot> <silkd> <out dir>}
SILKD=${2:?silkd binary}
OUT=${3:?out dir}
COCOON=${COCOON:-/usr/local/bin/cocoon-cb}
STORE=/srv/cb/cocoon/snapshot/localfile
BLOBS=/srv/cb/cocoon/oci/blobs
PROXY=http://127.0.0.1:3128
NO_PROXY=localhost,127.0.0.1,::1

log() { printf '[export %s] %s\n' "$(date -u +%T)" "$*"; }

id=$("$COCOON" snapshot inspect "$SNAP_NAME" | python3 -c 'import json,sys; d=json.load(sys.stdin); print((d[0] if isinstance(d,list) else d)["id"])')
snap=$STORE/$id
mapfile -t layers < <(python3 -c 'import json,sys; [print(d["path"]) for d in json.load(open(sys.argv[1])).get("disks", []) if d.get("path","").endswith(".erofs")]' "$snap/config.json")
[ "${#layers[@]}" -ge 1 ] || { echo "no erofs layers in $snap/config.json" >&2; exit 1; }
for l in "${layers[@]}"; do
	case "$l" in "$BLOBS"/*) ;; *) echo "layer outside $BLOBS: $l" >&2; exit 1 ;; esac
	[ -f "$l" ] || { echo "missing layer $l" >&2; exit 1; }
done

B=$OUT/$SNAP_NAME.build
rm -rf "$B"
install -d "$B" "$OUT"
lower=""
for i in "${!layers[@]}"; do
	log "extract layer $i ($(basename "${layers[$i]}"))"
	fsck.erofs --extract="$B/l$i" --xattrs "${layers[$i]}"
	lower="$B/l$i${lower:+:$lower}"
done
log "reflink cow"
cp --reflink=always "$snap/cow.raw" "$B/cow.raw"

add=$B/add
install -d "$add/usr/local/bin" "$add/etc/systemd/system/sysinit.target.wants" "$add/etc/systemd/system/multi-user.target.wants" "$add/etc/cocoonbox" "$add/etc/ssh"
install -m 0755 "$SILKD" "$add/usr/local/bin/silkd"
cat > "$add/etc/systemd/system/silkd.service" <<EOF
[Unit]
Description=silkd (sandbox product daemon: exec, files, sessions over vsock)
DefaultDependencies=no
Before=basic.target

[Service]
Type=simple
ExecStart=/usr/local/bin/silkd
Environment=SILKD_PORT=2048
Environment=http_proxy=$PROXY https_proxy=$PROXY no_proxy=$NO_PROXY,169.254.169.254
Restart=always
RestartSec=2s
LimitNOFILE=65536

[Install]
WantedBy=sysinit.target
EOF
ln -s ../silkd.service "$add/etc/systemd/system/sysinit.target.wants/silkd.service"
ln -s /dev/null "$add/etc/systemd/system/systemd-networkd-wait-online.service"
for u in cocoonbox-agent cocoonbox-toolhost cocoonbox-session; do
	install -d "$add/etc/systemd/system/$u.service.d"
	cat > "$add/etc/systemd/system/$u.service.d/10-sbx-proxy.conf" <<EOF
[Service]
Environment=http_proxy=$PROXY https_proxy=$PROXY HTTP_PROXY=$PROXY HTTPS_PROXY=$PROXY
Environment=no_proxy=$NO_PROXY NO_PROXY=$NO_PROXY
EOF
done
for k in $(ls "$B"/l*/etc/ssh 2>/dev/null | grep '^ssh_host_' | sort -u); do mknod "$add/etc/ssh/$k" c 0 0; done
cat > "$add/etc/systemd/system/cocoonbox-hostkeys.service" <<'EOF'
[Unit]
Description=generate ssh host keys on first boot
Before=ssh.service ssh.socket
ConditionPathExists=!/etc/ssh/ssh_host_ed25519_key

[Service]
Type=oneshot
ExecStart=/usr/bin/ssh-keygen -A

[Install]
WantedBy=multi-user.target
EOF
ln -s ../cocoonbox-hostkeys.service "$add/etc/systemd/system/multi-user.target.wants/cocoonbox-hostkeys.service"

unshare -m bash -euo pipefail -c '
B=$1; lower=$2; out=$3
mount --make-rprivate /
mkdir -p "$B/m/cow" "$B/m/base" "$B/m/root"
mount -o loop "$B/cow.raw" "$B/m/cow"
mount -t overlay overlay -o ro,lowerdir="$B/m/cow/upper:$lower" "$B/m/base"
gid=$(awk -F: "\$1==\"cbagent\"{print \$3}" "$B/m/base/etc/group")
[ -n "$gid" ] || { echo "no cbagent group in the image" >&2; exit 1; }
printf "GW_BASE_URL=http://10.203.0.1:18090/v1\nGW_API_KEY=unprovisioned\nMODEL_MAIN=deepseek/deepseek-v4.1-flash\nMODEL_COMPUTER=anthropic/claude-sonnet-5\nAGENT_LISTEN=127.0.0.1:7443\n" > "$B/add/etc/cocoonbox/agent.env"
chown 0:"$gid" "$B/add/etc/cocoonbox/agent.env"
chmod 0640 "$B/add/etc/cocoonbox/agent.env"
mount -t overlay overlay -o ro,lowerdir="$B/add:$B/m/cow/upper:$lower" "$B/m/root"
if grep -q "^EDGE_TOKEN=" "$B/m/root/etc/cocoonbox/agent.env"; then echo "edge token in the exported agent.env" >&2; exit 1; fi
tar -C "$B/m/root" --numeric-owner --xattrs --xattrs-include="*" -cf - . | gzip -1 > "$out.tmp"
mv "$out.tmp" "$out"
' _ "$B" "$lower" "$OUT/$SNAP_NAME.tar.gz"
rm -rf "$B"
log "$(sha256sum "$OUT/$SNAP_NAME.tar.gz")"
