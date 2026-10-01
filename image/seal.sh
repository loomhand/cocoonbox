#!/bin/bash
# Resets per-use state in a provisioned guest before it is snapshotted as a golden; runs as root.
set -euo pipefail

systemctl stop cocoonbox-agent.service cocoonbox-toolhost.service
rm -rf /var/lib/cocoonbox/* /run/cocoonbox/*
sed -i -e 's/^GW_API_KEY=.*/GW_API_KEY=unprovisioned/' -e '/^EDGE_TOKEN=/d' /etc/cocoonbox/agent.env

pkill -u user -x chrome || true
pkill -u user -x mousepad || true
sleep 2
rm -rf /workspace/* /workspace/.[!.]* \
	/home/user/.config/cocoonbox-chrome /home/user/.config/google-chrome /home/user/.cache/google-chrome /home/user/.cache/fontconfig \
	/home/user/.config/Mousepad /home/user/.local/share/Mousepad /home/user/.cache/Mousepad /home/user/.local/share/recently-used.xbel \
	/home/user/.bash_history /home/deploy/.bash_history /root/.bash_history
find /tmp -mindepth 1 -maxdepth 1 ! -name '.X11-unix' ! -name '.ICE-unix' ! -name '.X*-lock' ! -name 'systemd-private-*' -exec rm -rf {} +

apt-get clean
journalctl --rotate >/dev/null
journalctl --vacuum-time=1s >/dev/null

systemctl restart cocoonbox-xorg.service cocoonbox-session.service
systemctl --user -M user@ restart cua-driver.service
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
	systemctl start cocoonbox-toolhost.service cocoonbox-agent.service
fi
sync
