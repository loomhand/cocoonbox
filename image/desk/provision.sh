#!/bin/bash
# Adds the cbcore family's desk daemon (shuttle, with its own cua-driver child) and desk runtime to a provisioned desktop guest;
# idempotent.
# Runs as root inside the guest from an unpacked copy of image/desk (see cut.sh -f cbcore).
set -euo pipefail

NODE_VERSION=24.21.0
ROOT=/opt/desk
ENV_FILE=/etc/cocoonbox/desk.env
AGENT_ENV=/etc/cocoonbox/agent.env
SHUTTLE_CONFIG=/etc/cocoonbox/shuttle.toml
WANTS=/home/user/.config/systemd/user/default.target.wants

SRC="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=image/desk/shuttle.sh
. "$SRC/shuttle.sh"

log() { printf '[desk] %s\n' "$*"; }

install_runtime() {
	log "shuttle $SHUTTLE_VERSION and its desk runtime"
	local tarball="$SRC/vendor/$SHUTTLE_TARBALL"
	[ -f "$tarball" ] || { echo "missing $tarball: fetch it with image/desk/shuttle.sh fetch image/desk/vendor" >&2; exit 1; }
	install_shuttle "$tarball"
	install_shuttle_runtime "$tarball"
}

install_rootfs() {
	log "rootfs files"
	systemctl disable cocoonbox-toolhost.service >/dev/null 2>&1 || true
	rm -rf /etc/systemd/system/cocoonbox-toolhost.service /etc/systemd/system/cocoonbox-toolhost.service.d
	(cd "$SRC/rootfs" && find . -type f) | while read -r f; do
		case "$f" in
		./usr/local/bin/* | ./usr/local/lib/cocoonbox/*) install -D -m 0755 "$SRC/rootfs/$f" "/${f#./}" ;;
		*) install -D -m 0644 "$SRC/rootfs/$f" "/${f#./}" ;;
		esac
	done
	sed -i -e "/^<!-- routing -->\$/{r $SRC/routing.md" -e 'd}' "$ROOT/AGENTS.md"
	systemctl daemon-reload
	systemctl --user -M user@ daemon-reload
}

# the gw endpoint of the agent's contract, for the plan executor and scripts that run as user
write_env() {
	log "gw endpoint"
	grep '^GW_BASE_URL=' "$AGENT_ENV" >"$ENV_FILE.tmp"
	chmod 0644 "$ENV_FILE.tmp"
	mv -f "$ENV_FILE.tmp" "$ENV_FILE"
	local gw
	gw=$(sed -n 's/^GW_BASE_URL=//p' "$ENV_FILE")
	cat >"$SHUTTLE_CONFIG.tmp" <<EOF
base_url = "$gw"
decisions_url = "$gw/decisions"
api_key = "unprovisioned"
decide_build = "typesafe/jev-1.14-########"

[safety]
indicator = false
confirm = "off"
apps = []
kill_hotkey = "off"

[tools]
script_run = true
plugins = "/etc/cocoonbox/tools.d"

[driver]
overlay = false

[daemon]
socket_group = "cbagent"
EOF
	chmod 0644 "$SHUTTLE_CONFIG.tmp"
	mv -f "$SHUTTLE_CONFIG.tmp" "$SHUTTLE_CONFIG"
}

# shuttle's own cua-driver child replaces the box's cua-driver; the mask keeps a restored home's wants link inert
switch_driver() {
	log "shuttle in the session"
	if [ -f /etc/systemd/user/cua-driver.service ] && [ ! -L /etc/systemd/user/cua-driver.service ]; then
		systemctl --user -M user@ disable --now cua-driver.service
	fi
	rm -rf /etc/xdg/autostart/cocoonbox-cua-driver.desktop /etc/systemd/user/cua-driver.service \
		/etc/systemd/user/cua-driver.service.d /usr/local/lib/cocoonbox/cua-driver-session \
		/opt/cua-driver /usr/local/bin/cua-driver "$WANTS/cua-driver.service"
	systemctl --global mask cua-driver.service
	install -d -o user -g user "$WANTS"
	sed -i 's#^d /run/cocoonbox 2770 user cbagent -$#d /run/cocoonbox 2750 user cbagent -#' /etc/tmpfiles.d/cocoonbox.conf
	chmod 2750 /run/cocoonbox
	if [ -f /etc/systemd/user/cocoon-shuttle.service ]; then
		systemctl --user -M user@ stop cocoon-shuttle.service
	fi
	rm -rf /etc/systemd/user/cocoon-shuttle.service /etc/systemd/user/cocoon-shuttle.service.d "$WANTS/cocoon-shuttle.service" \
		/home/user/.config/cocoon-shuttle
	systemctl --global enable shuttle.service
	systemctl --user -M user@ daemon-reload
	systemctl --user -M user@ restart shuttle.service
}

apt_sources() {
	log "apt sources"
	apt-get update -q >/dev/null
}

route_chrome() {
	log "chrome launchers"
	install -d /usr/local/share/applications
	for f in google-chrome com.google.Chrome; do
		sed 's|/usr/bin/google-chrome-stable|/usr/local/bin/cocoonbox-chrome|' "/usr/share/applications/$f.desktop" >"/usr/local/share/applications/$f.desktop"
	done
	update-alternatives --install /usr/bin/x-www-browser x-www-browser /usr/local/bin/cocoonbox-chrome 250 >/dev/null
}

check() {
	[ "$(readlink -f /usr/local/bin/node)" = "/opt/node/$NODE_VERSION/bin/node" ]
	runuser -u user -- /usr/local/bin/node -e "import('$ROOT/lib/desk.mts').then((d) => { if (typeof d.snapshot !== 'function') process.exit(1) })"
	runuser -u user -- /usr/local/bin/node --check "$ROOT/lib/gui_plan.mts"
	for f in desk gui_plan png; do [ -s "$ROOT/lib/$f.mts" ]; done
	[ "$(readlink -f /usr/local/bin/shuttle)" = "/opt/loomhand/shuttle/$SHUTTLE_VERSION/shuttle" ]
	[ ! -e /opt/cocoon-shuttle ] && [ ! -e /usr/local/bin/cocoon-shuttle ] && [ ! -e /etc/systemd/user/cocoon-shuttle.service ]
	systemctl --user -M user@ -q is-enabled shuttle.service
	[ "$(systemctl --user -M user@ is-enabled cua-driver.service 2>/dev/null)" = masked ]
	[ ! -e /etc/xdg/autostart/cocoonbox-cua-driver.desktop ]
	for _ in $(seq 60); do [ -S /run/cocoonbox/desk.sock ] && [ -S /run/cocoonbox/driver.sock ] && break; sleep 0.5; done
	[ "$(stat -c %a:%U:%G /run/cocoonbox)" = 2750:user:cbagent ]
	[ "$(stat -c %a:%G /run/cocoonbox/desk.sock)" = 660:cbagent ]
	[ "$(pgrep -u user -x cua-driver | wc -l)" = 1 ]
	tr '\0' '\n' <"/proc/$(pgrep -u user -x cua-driver)/cmdline" | grep -qx -- --no-overlay
	[ "$(readlink -f /home/user/.config/loomhand/shuttle/flows)" = /workspace/flows ]
	[ "$(stat -c %a /home/user/.config/loomhand/shuttle/config.toml)" = 600 ]
	grep -qx 'plugins = "/etc/cocoonbox/tools.d"' /home/user/.config/loomhand/shuttle/config.toml
	systemctl show -p Environment cocoonbox-agent | grep 'SHUTTLE_SOCKET=/run/cocoonbox/desk.sock' | grep -q 'TOOLHOST_SOCKET=/run/cocoonbox/desk.sock'
	systemctl show -p Environment cocoonbox-session | grep -q 'SHUTTLE_SOCKET=/run/cocoonbox/desk.sock'
	systemctl --user -M user@ show-environment | grep -qx 'SHUTTLE_SOCKET=/run/cocoonbox/desk.sock'
	! systemctl cat cocoonbox-toolhost >/dev/null 2>&1
	grep -qxF '## Pick the tool tier first' "$ROOT/AGENTS.md"
	[ "$(grep -cF '<!-- routing -->' "$ROOT/AGENTS.md")" = 0 ]
	grep -q '^GW_BASE_URL=http' "$ENV_FILE"
	[ "$(stat -c %a "$ENV_FILE")" = 644 ]
	[ -x /usr/local/bin/desk ]
	[ ! -e /opt/cua-driver ] && [ ! -e /usr/local/bin/cua-driver ] && [ ! -e /usr/local/lib/cocoonbox/cua-driver-session ]
	[ "$(readlink -f /usr/bin/x-www-browser)" = /usr/local/bin/cocoonbox-chrome ]
	for f in google-chrome com.google.Chrome; do grep -qx 'Exec=/usr/local/bin/cocoonbox-chrome %U' "/usr/local/share/applications/$f.desktop"; done
	grep -qx 'URIs: mirror+file:/etc/apt/cocoonbox-ubuntu.mirrors' /etc/apt/sources.list.d/ubuntu.sources
	[ -s /var/lib/apt/lists/_etc_apt_cocoonbox-ubuntu.mirrors_dists_noble_InRelease ]
	[ -s /var/lib/apt/lists/_etc_apt_cocoonbox-ubuntu-security.mirrors_dists_noble-security_InRelease ]
}

install_runtime
install_rootfs
write_env
switch_driver
apt_sources
route_chrome
check
log "done"
