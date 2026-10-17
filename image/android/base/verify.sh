#!/bin/bash
# Static checks of a built Android base, run inside its root filesystem.
set -euo pipefail
kver=$(ls /lib/modules | sort -V | tail -1)
modinfo -k "$kver" ashmem_linux >/dev/null
modinfo -k "$kver" binder_linux >/dev/null
modinfo -k "$kver" btrfs >/dev/null
test -s /usr/local/share/cocoonbox/cb-ui.jar
ldd /usr/local/bin/scrcpy-rfb | grep -q 'not found' && exit 1
command -v adb dnscrypt-proxy mkfs.btrfs node git >/dev/null
test -f /opt/desk/lib/gui_plan.mts
test -f /opt/android/AGENTS.md
test -d /var/lib/redroid-data && test -z "$(ls -A /var/lib/redroid-data)"
cfgs=(/var/lib/docker/containers/*/config.v2.json)
[ "${#cfgs[@]}" = 1 ]
grep -Fq '"Name":"/redroid"' "${cfgs[0]}"
grep -Fq '"androidboot.use_memfd=0"' "${cfgs[0]}"
grep -Fq '"Name":"unless-stopped"' "$(dirname "${cfgs[0]}")/hostconfig.json"
grep -Fq '"5555/tcp":[{"HostIp":"127.0.0.1"' "$(dirname "${cfgs[0]}")/hostconfig.json"
[ -z "$(find /var/lib/docker/vfs/dir -maxdepth 5 -path '*/priv-app/PrebuiltGmsCore' -print -quit)" ]
for u in cocoonbox-android.path cocoonbox-android-proxy.socket cocoonbox-session.service; do systemctl is-enabled -q "$u"; done
systemctl is-enabled -q docker.service && exit 1
echo android-base-verify=ok
