# Android box

An Android box is a template of family `cbdroid`: an Ubuntu 24.04 VM that runs Android (redroid) in docker, with the same silkd, `cocoonbox-agent serve` and `toolhost` as the desktop box.

## Recipe

| Piece | Where |
|---|---|
| guest provisioning (docker, adb, scrcpy-rfb, users, units) | `image/android/provision.sh`, `image/android/rootfs/` |
| ashmem for the 24.04 kernel, which Android's gralloc needs | `image/android/ashmem/` (GPL-2.0, built at cut time) |
| cut, from an Ubuntu 24.04 cocoon image and a docker data dir with one baked redroid container | `image/android/cut.sh` |

```bash
sudo image/android/cut.sh -a cocoonbox-agent-linux-amd64 -d <docker data dir> ghcr.io/cocoonstack/cocoon/ubuntu:24.04 vN
```

The redroid image in the docker data decides what the box ships; a public image carries AOSP only.

## Boot

The golden holds the Ubuntu VM only; docker stays off. When the api writes `/run/cocoonbox-identity/identity`, `cocoonbox-android.path` starts `android-up`: docker replays the container and Android cold-boots with a fresh `/data`, so no two boxes share an Android identity. `android-up` then sets Android's global proxy and starts the screen: scrcpy-rfb on 127.0.0.1:5900, 720x1280, no password, which the agent's Screen relay serves as is. adb listens on 127.0.0.1:5555.

## Network

The box has no NIC. Android on the docker bridge reaches silkd's proxy through `172.18.0.1:3128` (global HTTP proxy), and DNS through `172.18.0.1:53`, which `dnscrypt-proxy` answers with DNS over HTTPS through the same proxy, so Android validates its network. Apps that ignore the system proxy, UDP and QUIC have no route.

## Pool

`cbsbx-conf` gives a `cbdroid` pool a warmup that waits for the agent only (`ANDROID_FAMILIES`).
