#!/bin/bash
# Provisions a cocoon-macos guest as a desk box: the agent's serve as a LaunchDaemon, shuttle as a LaunchAgent in the Aqua
# session with the agent's toolhost as its service plugin, the desk runtime and guide, /workspace and /run, and autologin.
# Runs as root inside the guest from an unpacked copy of image/macos with vendor/ filled; idempotent. A reboot follows the
# first run, as /etc/synthetic.conf makes /workspace and /run only at boot. The shuttle app gets Accessibility, Screen
# Recording, the capture consent and Full Disk Access once per image through UI input; the toolhost runs as its child.
#   BOX_USER (default cocoon), BOX_PASSWORD: the desktop user that logs in automatically
#   GW_BASE_URL, GW_API_KEY, MODEL_MAIN, MODEL_COMPUTER: the agent's model contract
#   SHUTTLE_DECIDE (default 1): 0 leaves shuttle without the gw, for a test box with no model access
set -euo pipefail

SHUTTLE_VERSION=v0.1.23
SHUTTLE_TARBALL=shuttle-${SHUTTLE_VERSION}-macos-x64.tar.gz
SHUTTLE_SHA256=236de7f7a66080b0ebd7c4ac6b3b5d718631172bcc21114f821d48bce6b0691e
SHUTTLE_APP=/Applications/Loomhand/Shuttle.app
SHUTTLE_AGENT=io.github.loomhand.shuttle
AGENT_ID=401
AGENT_GROUP=cbagent
WORKSPACE_DIR=/Users/Shared/workspace
RUN_DIR=/usr/local/var/run
STATE_DIR=/var/lib/cocoonbox
ETC=/etc/cocoonbox
ROOT=/opt/desk
DAEMON=io.github.loomhand.cocoonbox-agent
AGENT_LOG=/var/log/cocoonbox-agent.log
SUBSTITUTIONS="NSAutomaticCapitalizationEnabled NSAutomaticSpellingCorrectionEnabled NSAutomaticPeriodSubstitutionEnabled NSAutomaticQuoteSubstitutionEnabled NSAutomaticDashSubstitutionEnabled NSAutomaticTextCompletionEnabled"

SRC="$(cd "$(dirname "$0")" && pwd)"
BOX_USER=${BOX_USER:-cocoon}

log() { printf '[macos] %s\n' "$*"; }

synthetic() {
	log "/workspace and /run at boot"
	install -d -o "$BOX_USER" -g staff -m 0755 "$WORKSPACE_DIR"
	install -d -m 0755 "$RUN_DIR"
	touch /etc/synthetic.conf
	for line in "workspace	${WORKSPACE_DIR#/}" "run	${RUN_DIR#/}"; do
		grep -qxF "$line" /etc/synthetic.conf || printf '%s\n' "$line" >>/etc/synthetic.conf
	done
}

accounts() {
	log "$AGENT_GROUP group and the hidden _cbagent user"
	dscl . -read "/Groups/$AGENT_GROUP" >/dev/null 2>&1 || {
		dscl . -create "/Groups/$AGENT_GROUP"
		dscl . -create "/Groups/$AGENT_GROUP" PrimaryGroupID "$AGENT_ID"
		dscl . -create "/Groups/$AGENT_GROUP" RealName "cocoonbox agent"
	}
	dscl . -read /Users/_cbagent >/dev/null 2>&1 || {
		dscl . -create /Users/_cbagent
		dscl . -create /Users/_cbagent UniqueID "$AGENT_ID"
		dscl . -create /Users/_cbagent PrimaryGroupID "$AGENT_ID"
		dscl . -create /Users/_cbagent UserShell /usr/bin/false
		dscl . -create /Users/_cbagent NFSHomeDirectory /var/empty
		dscl . -create /Users/_cbagent RealName "cocoonbox agent"
		dscl . -create /Users/_cbagent IsHidden 1
	}
	dseditgroup -o edit -a "$BOX_USER" -t user "$AGENT_GROUP"
}

agent() {
	log "agent binary, desk command and serve"
	install -m 0755 "$SRC/vendor/cocoonbox-agent" /usr/local/bin/cocoonbox-agent
	printf '#!/bin/sh\n# desk: the desk API from a shell, through the toolhost socket; desk VERB [key=value ...|JSON]\nexec /usr/local/bin/cocoonbox-agent desk "$@"\n' >/usr/local/bin/desk
	chmod 0755 /usr/local/bin/desk
	install -d -m 0755 /usr/local/lib/cocoonbox
	install -m 0755 "$SRC/rootfs/usr/local/lib/cocoonbox/serve" "$SRC/rootfs/usr/local/lib/cocoonbox/shuttle-session" \
		"$SRC/rootfs/usr/local/lib/cocoonbox/restart-shuttle" /usr/local/lib/cocoonbox/
	install -d -o _cbagent -g "$AGENT_GROUP" -m 0700 "$STATE_DIR"
	install -d -o "$BOX_USER" -g "$AGENT_GROUP" -m 2750 "$RUN_DIR/cocoonbox"
	install -d -m 0755 "$RUN_DIR/cocoonbox-identity"
	touch "$AGENT_LOG"
	chown _cbagent:"$AGENT_GROUP" "$AGENT_LOG"
	install -d -m 0755 "$ETC" "$ETC/tools.d" "$ETC/routing.d"
	install -m 0644 "$SRC/../desk/service/cocoonbox.json" "$ETC/tools.d/cocoonbox.json"
	umask 077
	cat >"$ETC/agent.env.tmp" <<EOF
AGENT_LISTEN=127.0.0.1:7443
GW_BASE_URL=${GW_BASE_URL:?}
GW_API_KEY=${GW_API_KEY:?}
MODEL_MAIN=${MODEL_MAIN:?}
MODEL_COMPUTER=${MODEL_COMPUTER:?}
SHUTTLE_SOCKET=$RUN_DIR/cocoonbox/desk.sock
TOOLHOST_SOCKET=$RUN_DIR/cocoonbox/desk.sock
CB_HOME=/Users/$BOX_USER
CB_WORKSPACE=/workspace
EOF
	umask 022
	chown _cbagent:wheel "$ETC/agent.env.tmp"
	chmod 0600 "$ETC/agent.env.tmp"
	mv -f "$ETC/agent.env.tmp" "$ETC/agent.env"
	install -m 0644 "$SRC/rootfs/Library/LaunchDaemons/$DAEMON.plist" "/Library/LaunchDaemons/$DAEMON.plist"
	launchctl bootout "system/$DAEMON" 2>/dev/null || true
	for _ in $(seq 20); do launchctl print "system/$DAEMON" >/dev/null 2>&1 || break; sleep 0.5; done
	launchctl bootstrap system "/Library/LaunchDaemons/$DAEMON.plist"
}

shuttle() {
	log "shuttle $SHUTTLE_VERSION, its desk runtime and the guide"
	local tarball="$SRC/vendor/$SHUTTLE_TARBALL" tmp
	echo "$SHUTTLE_SHA256  $tarball" | shasum -a 256 -c --quiet -
	tmp=$(mktemp -d)
	tar -xzf "$tarball" -C "$tmp"
	launchctl bootout "gui/$(id -u "$BOX_USER")/io.github.loomhand.cocoon-shuttle" 2>/dev/null || true
	pkill -TERM -f "MacOS/cocoon-shuttle serve" || true
	rm -rf "$SHUTTLE_APP" "/Applications/Cocoon Shuttle.app" /Library/LaunchAgents/io.github.loomhand.cocoon-shuttle.plist
	install -d -m 0755 "$(dirname "$SHUTTLE_APP")"
	mv "$tmp/Shuttle.app" "$SHUTTLE_APP"
	chown -R root:wheel "$SHUTTLE_APP"
	rm -rf "$tmp"
	install -d -m 0755 "$ROOT/lib"
	for f in desk gui_plan png; do install -m 0644 "$SHUTTLE_APP/Contents/Resources/runtime/$f.mts" "$ROOT/lib/$f.mts"; done
	install -m 0644 "$SRC/rootfs/opt/desk/AGENTS.md" "$ROOT/AGENTS.md"
	sed -i '' -e "/^<!-- routing -->\$/{r $SRC/../desk/routing.md" -e 'd' -e '}' "$ROOT/AGENTS.md"
	: >"$ETC/shuttle.toml.tmp"
	[ "${SHUTTLE_DECIDE:-1}" = 0 ] || cat >>"$ETC/shuttle.toml.tmp" <<EOF
base_url = "$GW_BASE_URL"
decisions_url = "$GW_BASE_URL/decisions"
api_key = "unprovisioned"
decide_build = "typesafe/jev-1.14-########"

EOF
	cat >>"$ETC/shuttle.toml.tmp" <<EOF
[safety]
indicator = false
confirm = "off"
apps = []
kill_hotkey = "off"

[tools]
script_run = true
plugins = "$ETC/tools.d"

[driver]
overlay = false

[daemon]
socket_group = "$AGENT_GROUP"
EOF
	chmod 0644 "$ETC/shuttle.toml.tmp"
	mv -f "$ETC/shuttle.toml.tmp" "$ETC/shuttle.toml"
	install -m 0644 "$SRC/rootfs/Library/LaunchAgents/$SHUTTLE_AGENT.plist" /Library/LaunchAgents/
}

session() {
	log "autologin of $BOX_USER to an unlocked screen, with no screen saver or screen lock, and text substitution off"
	perl -e 'my @k=(0x7D,0x89,0x52,0x23,0xD2,0xBC,0xDD,0xEA,0xA3,0xB9,0x1F); my $p=$ARGV[0]; my $n=length($p)+1; $n+=12-($n%12) if $n%12; my @c=unpack("C*",$p); my $o=""; for my $i (0..$n-1){ my $b=$i<@c?$c[$i]:0; $o.=chr($b ^ $k[$i % @k]); } open(F,">","/etc/kcpassword") or die; binmode F; print F $o; close F; chmod 0600,"/etc/kcpassword";' "${BOX_PASSWORD:?}"
	defaults write /Library/Preferences/com.apple.loginwindow autoLoginUser "$BOX_USER"
	defaults delete /Library/Preferences/com.apple.loginwindow autoLoginUserScreenLocked 2>/dev/null || true
	sudo -u "$BOX_USER" sysadminctl -screenLock off -password "$BOX_PASSWORD" 2>/dev/null
	sudo -u "$BOX_USER" defaults -currentHost write com.apple.screensaver idleTime -int 0
	for k in $SUBSTITUTIONS; do sudo -u "$BOX_USER" defaults write -g "$k" -bool false; done
}

chrome() {
	local dmg="$SRC/vendor/googlechrome.dmg" mnt
	[ -f "$dmg" ] || { log "no vendor/googlechrome.dmg: Chrome not installed"; return; }
	log "Google Chrome"
	mnt=$(mktemp -d)
	hdiutil attach -nobrowse -quiet -mountpoint "$mnt" "$dmg"
	rm -rf "/Applications/Google Chrome.app"
	cp -R "$mnt/Google Chrome.app" /Applications/
	hdiutil detach -quiet "$mnt"
}

check() {
	[ -d /workspace ]
	[ -d /run/cocoonbox ]
	[ "$(stat -f %Su:%Sg:%Mp%Lp /run/cocoonbox)" = "$BOX_USER:$AGENT_GROUP:2750" ]
	[ "$(stat -f %Su:%Lp "$STATE_DIR")" = _cbagent:700 ]
	[ "$(stat -f %Su:%Sg:%Lp "$ETC/agent.env")" = _cbagent:wheel:600 ]
	[ -z "$(sudo -u "$BOX_USER" sh -c "test -r $ETC/agent.env && echo readable")" ]
	dseditgroup -o checkmember -m "$BOX_USER" "$AGENT_GROUP" >/dev/null
	launchctl print "system/$DAEMON" | grep -q 'state = running'
	/usr/local/bin/cocoonbox-agent version
	for f in desk gui_plan png; do [ -s "$ROOT/lib/$f.mts" ]; done
	grep -qxF '## Pick the tool tier first' "$ROOT/AGENTS.md"
	[ "$(grep -cF '<!-- routing -->' "$ROOT/AGENTS.md")" = 0 ]
	[ "$(defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser)" = "$BOX_USER" ]
	[ -z "$(defaults read /Library/Preferences/com.apple.loginwindow autoLoginUserScreenLocked 2>/dev/null)" ]
	[ "$(sudo -u "$BOX_USER" defaults -currentHost read com.apple.screensaver idleTime)" = 0 ]
	plutil -lint -s "/Library/LaunchAgents/$SHUTTLE_AGENT.plist" "/Library/LaunchDaemons/$DAEMON.plist"
	[ ! -e "/Applications/Cocoon Shuttle.app" ]
	[ ! -e /Library/LaunchAgents/io.github.loomhand.cocoon-shuttle.plist ]
	echo check: ok
}

case "${1:-all}" in
all)
	synthetic
	accounts
	agent
	shuttle
	session
	chrome
	;;
check) check ;;
*) echo "usage: $0 [all|check]" >&2; exit 2 ;;
esac
