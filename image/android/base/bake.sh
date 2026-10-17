#!/bin/bash
# Builds the redroid image of redroid.Dockerfile and bakes a created redroid container of it into docker-data-<arch>.tar
# with a privileged DinD (vfs); the VM's android-up starts the container.
# usage: ARCH=amd64|arm64 image/android/base/bake.sh [redroid image]   (default: build redroid.Dockerfile for ARCH)
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ARCH=${ARCH:-amd64}
SRC=${1:-}
DIND=docker:29.8.1-dind@sha256:3f3c01aaaebf7cce837356b688b7c059a4749f10bd7660dec7c58fc454a283f0
UBUNTU=ubuntu:24.04@sha256:68ee22004a9109b44973313b02ff7b1de250f1de0f5ecb628d73f579aaff33ac
DNS1=8.8.8.8
DNS2=1.1.1.1

cleanup() {
	docker rm -f cb-bake >/dev/null 2>&1 || true
	docker volume rm cb-bake >/dev/null 2>&1 || true
	rm -f "$HERE/redroid-image.tar"
}
trap cleanup EXIT

if [ -z "$SRC" ]; then
	SRC=local/redroid-aosp:16-$ARCH
	docker buildx build --load --platform "linux/$ARCH" -f "$HERE/redroid.Dockerfile" -t "$SRC" "$HERE"
fi
[ "$(docker image inspect --format '{{len .RootFS.Layers}}' "$SRC")" = 1 ] || { echo "$SRC is not one layer" >&2; exit 1; }
docker save "$SRC" -o "$HERE/redroid-image.tar"
docker rm -f cb-bake >/dev/null 2>&1 || true
docker volume rm cb-bake >/dev/null 2>&1 || true
docker run -d --privileged --name cb-bake \
	-v "$HERE/../rootfs/etc/docker/daemon.json":/etc/docker/daemon.json:ro \
	-v "$HERE/redroid-image.tar":/redroid.tar:ro \
	-v cb-bake:/var/lib/docker \
	"$DIND" >/dev/null
docker exec -e SRC="$SRC" -e DNS1="$DNS1" -e DNS2="$DNS2" -e ARCH="$ARCH" cb-bake sh -c '
	set -e
	for i in $(seq 60); do docker info >/dev/null 2>&1 && break; sleep 1; done
	test "$(docker info --format "{{.Driver}}")" = vfs
	docker load -i /redroid.tar
	docker tag "$SRC" local/redroid:vm
	[ "$SRC" = local/redroid:vm ] || docker image rm "$SRC" >/dev/null
	docker create --platform "linux/$ARCH" --privileged --name redroid \
		-p 127.0.0.1:5555:5555 -v /var/lib/redroid-data:/data \
		local/redroid:vm \
		androidboot.use_memfd=0 \
		androidboot.redroid_width=720 androidboot.redroid_height=1280 \
		androidboot.redroid_dpi=320 androidboot.redroid_fps=60 \
		androidboot.redroid_gpu_mode=guest \
		androidboot.redroid_net_ndns=2 \
		"androidboot.redroid_net_dns1=$DNS1" "androidboot.redroid_net_dns2=$DNS2" \
		ro.setupwizard.mode=DISABLED >/dev/null
'
docker stop -t 40 cb-bake >/dev/null
docker run --rm -v cb-bake:/vld -v "$HERE":/out "$UBUNTU" bash -c "
	# vfs keeps the image, the container and its init layer as three full copies; hardlinks ship them once
	hardlink /vld/vfs/dir >/dev/null
	tar --numeric-owner -C /vld -cf /out/docker-data-$ARCH.tar ."
echo "docker-data-$ARCH.tar $(du -h "$HERE/docker-data-$ARCH.tar" | cut -f1)"
