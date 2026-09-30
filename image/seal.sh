#!/bin/bash
# Resets per-use state in a provisioned guest before it is snapshotted as a golden; runs as root.
set -euo pipefail

systemctl disable --now cocoonbox-agent.service cocoonbox-toolhost.service >/dev/null 2>&1 || true
rm -f /usr/local/bin/cocoonbox-agent
rm -rf /var/lib/cocoonbox /run/cocoonbox/*

pkill -u user -x chrome || true
pkill -u user -x mousepad || true
sleep 2
rm -rf /workspace/* /workspace/.[!.]* \
	/home/user/.config/cocoonbox-chrome /home/user/.config/google-chrome /home/user/.cache/google-chrome \
	/home/user/.config/Mousepad /home/user/.local/share/recently-used.xbel \
	/home/user/.bash_history /home/deploy/.bash_history /root/.bash_history
find /tmp -mindepth 1 -maxdepth 1 ! -name '.X11-unix' ! -name '.ICE-unix' ! -name '.X*-lock' ! -name 'systemd-private-*' -exec rm -rf {} +

apt-get clean
journalctl --rotate >/dev/null
journalctl --vacuum-time=1s >/dev/null

systemctl restart cocoonbox-xorg.service cocoonbox-session.service cocoonbox-vnc.service
systemctl --user -M user@ restart cua-driver.service
sync
