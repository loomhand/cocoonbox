# Android box

An Android box is a template of family `cbdroid`: an Ubuntu 24.04 VM that runs Android (redroid) in docker, with the same silkd, `cocoonbox-agent serve` and `toolhost` as the desktop box.

## Recipe

| Piece | Where |
|---|---|
| guest provisioning (docker, adb, scrcpy-rfb, users, units) | `image/android/provision.sh`, `image/android/rootfs/` |
| ashmem for the 24.04 kernel, which Android's gralloc needs | `image/android/ashmem/` (GPL-2.0, built in the base) |
| the UI server: the accessibility tree from a resident `app_process`, built in the base | `image/android/ui/` |
| Node and the desk runtime of cbcore, for GUI plans and scripts | `image/android/provision.sh` (`setup_desk`) |
| cut, from the base and a release's agent | `image/android/cut.sh` |

The base is `ghcr.io/loomhand/cocoonbox/android-base`, built by `.github/workflows/android-base.yml` from `image/android/base/`: `bake.sh` bakes a created container of AOSP redroid 16 (`redroid.Dockerfile`, no Google apps) into docker data, and `Dockerfile` provisions cocoon's Ubuntu 24.04 image with it. A template is the base plus a release's agent:

```bash
cocoon image pull ghcr.io/loomhand/cocoonbox/android-base:<sha>
sudo image/android/cut.sh -a cocoonbox-agent-linux-amd64 ghcr.io/loomhand/cocoonbox/android-base:<sha> vN
```

The redroid image in the docker data decides what the box ships; a public base carries AOSP only. A pull request that touches `image/android/` builds the base without publishing it.

## Boot

The golden holds the Ubuntu VM only; docker stays off. When the api writes `/run/cocoonbox-identity/identity`, `cocoonbox-android.path` starts `android-up`: it starts docker and the container, and Android cold-boots with a fresh `/data`, so no two boxes share an Android identity. `android-up` then sets Android's global proxy and starts the screen: scrcpy-rfb on 127.0.0.1:5900, 720x1280, no password, which the agent's Screen relay serves as is. adb listens on 127.0.0.1:5555.

## Network

The box has no NIC. Android on the docker bridge (pinned to `172.18.0.1/16`) reaches silkd's proxy through `172.18.0.1:3128` (global HTTP proxy), and DNS through `172.18.0.1:53`, which `dnscrypt-proxy` answers with DNS over HTTPS through the same proxy, so Android validates its network. TCP from apps that ignore the system proxy (Google Play services' FCM on 5228, `nc`) is redirected by iptables to `redsocks` on `172.18.0.1:3129`, which opens it through the same proxy as `CONNECT <ip>:<port>`, so the proxy's port rules and audit cover it; such connections reach the proxy by address only, so hostname-based egress rules and credential injection apply to proxy-aware traffic only. UDP and QUIC have no route.

## Agent

`/opt/android/AGENTS.md` makes the agent's automation `Android`: the MCP instructions and prompts describe the phone, and there are no browser tools.

| Tool | On Android |
|---|---|
| `screenshot` | `adb exec-out screencap -p`, 720x1280 PNG |
| `computer_state` | the UI server's tree as the desk's elements `[index]`: role, name, centre, states |
| `computer_act` | `adb shell input` taps, swipes and keys; `type` pastes through a control-only scrcpy session as uid 1000, since key injection drops text outside Latin-1; `launch_app` by package |
| `gui_plan`, `script_run` | `desk.mts` and `gui_plan.mts` unchanged; the toolhost serves `desk_<op>` from Android (`agent/src/toolhost/android_desk.rs`) |

On the desk wire a window is an app's activity or dialog titled `<title> - <package>`, and an element id is `package:path:role:name`, since a list reuses its rows as it scrolls and an app recreates its window on a theme change. A press scrolls an off-screen row into view. Only one accessibility client runs at a time, so `uiautomator dump` fails while the UI server runs.

## State

Android's `/data` is a btrfs subvolume in a sparse loop image (`android-mount`). A quiesced save (Rebase, Delete, SaveState, park) runs `session-save`, which takes a read-only snapshot while Android runs and writes it as a plain tar, sorted by name with numeric owners and without `dalvik-cache` and `cache`, to `/home/user/.android/data.tar` with its sha256, both by rename; the `home` root then carries it. A failed save fails the quiesced save, so a snapshot never carries a stale tar. Live saves every 15 minutes and idle hibernation do not refresh it, so after a restore `/data` is as of the last quiesced save.

Restore stops and starts `cocoonbox-session.service`, which runs `android-apply`: a tar whose sha256 does not match, or that was applied before, is left alone; otherwise Android shuts down, the tar extracts into a new subvolume, and only a whole extract replaces `/data`. On any failure Android boots on its old `/data` and the journal says why. A Rebase from an Android image without this unit fails at the restore and the api keeps the box on its old image.

## Pool

`cbsbx-conf` gives a `cbdroid` pool a warmup that waits for the agent only (`ANDROID_FAMILIES`).
