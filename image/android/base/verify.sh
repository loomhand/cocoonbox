#!/bin/bash
# Static checks of a built Android base, run inside its root filesystem.
set -euo pipefail
kver=$(ls /lib/modules | sort -V | tail -1)
modinfo -k "$kver" ashmem_linux >/dev/null
modinfo -k "$kver" binder_linux >/dev/null
modinfo -k "$kver" btrfs >/dev/null
ldd /usr/local/bin/scrcpy-rfb | grep -q 'not found' && exit 1
test -s /usr/local/share/scrcpy/scrcpy-server-shell.jar
grep -q -- '--clipboard-port "$CLIP_PORT"' /usr/local/lib/cocoonbox/remoteview
grep -q '^done &$' /usr/local/lib/cocoonbox/remoteview
grep -qx 'wait -n "$server_pid" "$rfb_pid"' /usr/local/lib/cocoonbox/remoteview
command -v adb dnscrypt-proxy redsocks mkfs.btrfs node git >/dev/null
for f in desk gui_plan png; do test -s /opt/desk/lib/$f.mts; done
test ! -e /etc/systemd/system/cocoonbox-toolhost.service
grep -qx 'Environment=TOOLHOST_SOCKET=/run/cocoonbox/desk.sock' /etc/systemd/system/cocoonbox-agent.service
grep -qx 'Environment=SHUTTLE_SOCKET=/run/cocoonbox/desk.sock' /etc/systemd/system/cocoonbox-agent.service
grep -q '"args": \["toolhost-service"\]' /etc/cocoonbox/tools.d/cocoonbox.json
grep -qx 'plugins = "/etc/cocoonbox/tools.d"' /etc/cocoonbox/shuttle.toml
grep -qx 'socket_group = "cbagent"' /etc/cocoonbox/shuttle.toml
grep -qx 'd /run/cocoonbox 2750 user cbagent -' /etc/tmpfiles.d/cocoonbox.conf
shuttle=$(dirname "$(readlink -f /usr/local/bin/cocoon-shuttle)")
test -x "$shuttle/node" && test -s "$shuttle/runtime/gui_plan.mts"
case $(dpkg --print-architecture) in arm64) machine=00b7 ;; *) machine=003e ;; esac
[ "$(od -An -tx2 -j18 -N2 "$shuttle/cocoon-shuttle" | tr -d ' ')" = "$machine" ]
grep -Fqx "ui_jar = \"$shuttle/android/shuttle-ui.jar\"" /etc/cocoonbox/shuttle.toml
test -s "$shuttle/android/shuttle-ui.jar"
test -f /opt/android/AGENTS.md
test -d /var/lib/redroid-data && test -z "$(ls -A /var/lib/redroid-data)"
cfgs=(/var/lib/docker/containers/*/config.v2.json)
[ "${#cfgs[@]}" = 1 ]
grep -Fq '"Name":"/redroid"' "${cfgs[0]}"
grep -Fq '"androidboot.use_memfd=0"' "${cfgs[0]}"
grep -Fq '"5555/tcp":[{"HostIp":"127.0.0.1"' "$(dirname "${cfgs[0]}")/hostconfig.json"
[ -z "$(find /var/lib/docker/vfs/dir -maxdepth 5 -path '*/priv-app/PrebuiltGmsCore' -print -quit)" ]
for u in cocoonbox-android.path cocoonbox-android-proxy.socket cocoonbox-session.service cocoon-shuttle.service; do systemctl is-enabled -q "$u"; done
systemctl is-enabled -q docker.service && exit 1
echo android-base-verify=ok
