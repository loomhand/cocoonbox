#!/bin/bash
# Provisions the Android box guest (Ubuntu 24.04 outer VM, docker, the baked redroid container) in place; idempotent.
# Runs as root inside the guest from an unpacked copy of this directory, after /var/lib/docker holds the baked redroid container.
set -euo pipefail

DOCKER_VERSION=5:29.8.1-1~ubuntu.24.04~noble
PACKAGES=(adb libjpeg-turbo8 sudo locales iptables dnscrypt-proxy)
BUILD_PACKAGES=(gcc-13 make libc6-dev)
# cocoonstack/scrcpy-rfb master release at 9bdf6ed2b50953589ef153c18f087bcaafc429d9
SCRCPY_URL=https://github.com/cocoonstack/scrcpy-rfb/releases/download/master
SCRCPY_FILES=(
	"scrcpy-rfb-linux-amd64 30e287c466a19a1092b085d14af15cd9d73aae01ca75c436d491f5254547a641 /usr/local/bin/scrcpy-rfb 0755"
	"scrcpy-server-secure.jar 90fe59bd9cfa24b1f6a1dcbe844b1ee306b0c5f33f6f306a0d3676847aa32608 /usr/local/share/scrcpy/scrcpy-server.jar 0644"
	"su1000-linux-amd64 cac34bdbdb04c300c9ebd098dbb6c3e72abf18ee21427e578ba9f6f373d98643 /usr/local/share/scrcpy/su1000 0755"
)

SRC="$(cd "$(dirname "$0")" && pwd)"
export DEBIAN_FRONTEND=noninteractive

log() { printf '[provision] %s\n' "$*"; }

setup_packages() {
	log "packages"
	printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d
	chmod 0755 /usr/sbin/policy-rc.d
	install -m 0755 -d /etc/apt/keyrings
	[ -f /etc/apt/keyrings/docker.asc ] || curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
	echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" > /etc/apt/sources.list.d/docker.list
	apt-get update -qq
	apt-get install -y -qq --no-install-recommends "linux-modules-extra-$(ls /lib/modules | sort -V | tail -1)" \
		"docker-ce=$DOCKER_VERSION" "docker-ce-cli=$DOCKER_VERSION" containerd.io "${PACKAGES[@]}" >/dev/null
	apt-get purge -y -qq openssh-server openssh-sftp-server >/dev/null 2>&1 || true
	rm -f /usr/sbin/policy-rc.d
}

# the 24.04 kernel has no ashmem, which Android 16 gralloc needs; the module is a port of the staging driver to this kernel
setup_ashmem() {
	local kver headers
	kver=$(ls /lib/modules | sort -V | tail -1)
	headers=linux-headers-$kver
	log "ashmem for $kver"
	apt-get install -y -qq --no-install-recommends "$headers" "${BUILD_PACKAGES[@]}" >/dev/null
	rm -rf /tmp/ashmem && cp -r "$SRC/ashmem" /tmp/ashmem
	make -s -C "/lib/modules/$kver/build" M=/tmp/ashmem CC=gcc-13 modules
	install -D -m 0644 /tmp/ashmem/ashmem_linux.ko "/lib/modules/$kver/updates/ashmem_linux.ko"
	depmod -a "$kver"
	rm -rf /tmp/ashmem
	apt-get purge -y -qq --auto-remove "$headers" "${BUILD_PACKAGES[@]}" >/dev/null
	modinfo -k "$kver" ashmem_linux >/dev/null
	apt-get clean
}

setup_users() {
	log "users"
	if id ubuntu >/dev/null 2>&1; then
		usermod -l user -d /home/user -m ubuntu
		groupmod -n user ubuntu
	fi
	[ "$(id -u user)" = 1000 ] || { echo "user must be uid 1000" >&2; exit 1; }
	for g in sudo adm; do gpasswd -d user "$g" >/dev/null 2>&1 || true; done
	passwd -l user >/dev/null
	id cbagent >/dev/null 2>&1 || useradd --system --no-create-home --home-dir /var/lib/cocoonbox --shell /usr/sbin/nologin cbagent
	printf 'Defaults:user env_keep += "http_proxy https_proxy no_proxy HTTP_PROXY HTTPS_PROXY NO_PROXY ANDROID_SERIAL"\nuser ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/90-cocoonbox-user
	chmod 0440 /etc/sudoers.d/90-cocoonbox-user
	visudo -cqf /etc/sudoers.d/90-cocoonbox-user
	passwd -l root >/dev/null
	usermod -c "cocoonbox user" user
	sed -i -E 's/^# *(en_US\.UTF-8 UTF-8)$/\1/' /etc/locale.gen
	locale-gen >/dev/null
	printf 'LANG=en_US.UTF-8\n' > /etc/default/locale
}

install_rootfs() {
	log "rootfs files"
	(cd "$SRC/rootfs" && find . -type f) | while read -r f; do
		case "$f" in
		./usr/local/lib/cocoonbox/*) install -D -m 0755 "$SRC/rootfs/$f" "/${f#./}" ;;
		*) install -D -m 0644 "$SRC/rootfs/$f" "/${f#./}" ;;
		esac
	done
}

setup_scrcpy() {
	log "scrcpy-rfb"
	local f name sum dest mode tmp
	tmp=$(mktemp -d)
	for f in "${SCRCPY_FILES[@]}"; do
		read -r name sum dest mode <<<"$f"
		curl -fsSL "$SCRCPY_URL/$name" -o "$tmp/$name"
		echo "$sum  $tmp/$name" | sha256sum -c --quiet -
		install -D -m "$mode" "$tmp/$name" "$dest"
	done
	rm -rf "$tmp"
	! ldd /usr/local/bin/scrcpy-rfb | grep 'not found'
}

# adb stays on loopback; the box has no NIC, but nothing outside the VM needs it
setup_redroid() {
	log "redroid"
	local cfgs=(/var/lib/docker/containers/*/config.v2.json)
	[ "${#cfgs[@]}" = 1 ] && [ -f "${cfgs[0]}" ] || { echo "want one baked container, have ${#cfgs[@]}" >&2; exit 1; }
	local dir
	dir=$(dirname "${cfgs[0]}")
	sed -i 's/"androidboot.use_memfd=1"/"androidboot.use_memfd=0"/' "${cfgs[0]}"
	grep -q '"androidboot.use_memfd=0"' "${cfgs[0]}"
	sed -i 's/"HostIp":""/"HostIp":"127.0.0.1"/g' "$dir/hostconfig.json"
	grep -q '"5555/tcp":\[{"HostIp":"127.0.0.1"' "$dir/hostconfig.json"
	install -d /var/lib/redroid-data
	find /var/lib/redroid-data -mindepth 1 -maxdepth 1 -exec rm -rf {} +
}

setup_cocoonbox_dirs() {
	log "cocoonbox dirs"
	install -d -m 0755 -o user -g user /workspace
	install -d -m 0755 -o root -g root /etc/cocoonbox
	systemd-tmpfiles --create /etc/tmpfiles.d/cocoonbox.conf
}

# docker stays off until the box has its identity: a docker start replays the container, and a stopped one would not replay
enable_units() {
	log "units"
	systemctl daemon-reload
	systemctl disable docker.service docker.socket containerd.service >/dev/null 2>&1
	systemctl is-active -q docker.service && { echo "docker is running in the template" >&2; exit 1; }
	systemctl mask dnscrypt-proxy.socket dnscrypt-proxy.service dnscrypt-proxy-resolvconf.service >/dev/null 2>&1
	systemctl enable cocoonbox-android.path cocoonbox-android-proxy.socket >/dev/null
}

setup_packages
setup_ashmem
setup_users
install_rootfs
setup_scrcpy
setup_redroid
setup_cocoonbox_dirs
enable_units
log "done"
