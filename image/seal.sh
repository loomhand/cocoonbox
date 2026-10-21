#!/bin/bash
# Resets per-use state in a provisioned guest before it is snapshotted as a golden; runs as root.
set -euo pipefail

systemctl stop cocoonbox-agent.service
systemctl --user -M user@ stop cocoonbox-offload-inbox.service
rm -rf /var/lib/cocoonbox/* /run/cocoonbox/*

pkill -u user -x chrome || true
pkill -u user -x mousepad || true
pkill -u user -x soffice.bin || true
sleep 2
rm -rf /workspace/* /workspace/.[!.]* \
	/home/user/.config/cocoonbox-chrome /home/user/.config/google-chrome /home/user/.cache/google-chrome /home/user/.cache/fontconfig \
	/home/user/.config/Mousepad /home/user/.local/share/Mousepad /home/user/.cache/Mousepad /home/user/.local/share/recently-used.xbel \
	/home/user/.config/libreoffice \
	/home/user/.local/state/xfce4-session /home/user/.config/cocoonbox /home/user/.local/share/cocoonbox \
	/home/user/.bash_history /root/.bash_history
runuser -u user -- env DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus dconf reset -f /org/xfce/mousepad/
find /tmp -mindepth 1 -maxdepth 1 ! -name '.X11-unix' ! -name '.ICE-unix' ! -name '.X*-lock' ! -name 'systemd-private-*' -exec rm -rf {} +

apt-get clean
journalctl --rotate >/dev/null
journalctl --vacuum-time=1s >/dev/null
find /var/log/journal -mindepth 1 -maxdepth 1 -type d ! -name "$(cat /etc/machine-id)" -exec rm -rf {} +

systemctl restart cocoonbox-xorg.service cocoonbox-session.service
systemctl --user -M user@ reset-failed
! systemctl --user -M user@ -q is-enabled shuttle.service 2>/dev/null || systemctl --user -M user@ restart shuttle.service
systemctl --user -M user@ restart cocoonbox-offload-inbox.service
systemctl reset-failed
abandoned=$(systemctl list-units 'session-*.scope' --state=abandoned --no-legend --plain | awk '{print $1}')
[ -z "$abandoned" ] || { echo "seal: abandoned session scopes: $abandoned" >&2; exit 1; }
desktop_session() {
	local s
	for s in $(loginctl list-sessions --no-legend | awk '$3 == "user" {print $1}'); do
		[ "$(loginctl show-session "$s" -p Active -p Remote -p Class -p Type | sort | tr '\n' ' ')" = "Active=yes Class=user Remote=no Type=x11 " ] && return 0
	done
	return 1
}
for _ in $(seq 20); do desktop_session && break; sleep 0.5; done
desktop_session || { echo "seal: no active local logind session for user" >&2; exit 1; }
if systemctl -q is-enabled cocoonbox-agent.service; then
	systemctl start cocoonbox-agent.service
fi
sync
