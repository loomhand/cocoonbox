# Android box

An Android box is a template of family `cbdroid`: an Ubuntu 24.04 VM that runs Android (redroid) in docker, with the same silkd, `cocoonbox-agent serve` and `toolhost` as the desktop box.

## Recipe

| Piece | Where |
|---|---|
| guest provisioning (docker, adb, scrcpy-rfb, users, units) | `image/android/provision.sh`, `image/android/rootfs/` |
| ashmem for the 24.04 kernel, which Android's gralloc needs | `image/android/ashmem/` (GPL-2.0, built at cut time) |
| the UI server: the accessibility tree from a resident `app_process`, built at cut time | `image/android/ui/` |
| Node and the desk runtime of cbcore, for GUI plans and scripts | `image/android/provision.sh` (`setup_desk`) |
| cut, from an Ubuntu 24.04 cocoon image and a docker data dir with one baked redroid container | `image/android/cut.sh` |

```bash
sudo image/android/cut.sh -a cocoonbox-agent-linux-amd64 -d <docker data dir> ghcr.io/cocoonstack/cocoon/ubuntu:24.04 vN
```

The redroid image in the docker data decides what the box ships; a public image carries AOSP only.

## Boot

The golden holds the Ubuntu VM only; docker stays off. When the api writes `/run/cocoonbox-identity/identity`, `cocoonbox-android.path` starts `android-up`: docker replays the container and Android cold-boots with a fresh `/data`, so no two boxes share an Android identity. `android-up` then sets Android's global proxy and starts the screen: scrcpy-rfb on 127.0.0.1:5900, 720x1280, no password, which the agent's Screen relay serves as is. adb listens on 127.0.0.1:5555.

## Network

The box has no NIC. Android on the docker bridge reaches silkd's proxy through `172.18.0.1:3128` (global HTTP proxy), and DNS through `172.18.0.1:53`, which `dnscrypt-proxy` answers with DNS over HTTPS through the same proxy, so Android validates its network. Apps that ignore the system proxy, UDP and QUIC have no route.

## Agent

`/opt/android/AGENTS.md` makes the agent's automation `Android`: the MCP instructions and prompts describe the phone, and there are no browser tools.

| Tool | On Android |
|---|---|
| `screenshot` | `adb exec-out screencap -p`, 720x1280 PNG |
| `computer_state` | the UI server's tree as elements `[index]`; `uiautomator dump` when the server fails |
| `computer_act` | `adb shell input` taps, swipes and keys; `type` pastes through a control-only scrcpy session as uid 1000, since key injection drops text outside Latin-1; `launch_app` by package |
| `gui_plan`, `script_run` | `desk.mts` and `gui_plan.mts` unchanged; the toolhost serves `desk_<op>` from Android (`agent/src/toolhost/android_desk.rs`) |

On the desk wire a window is an app's activity or dialog titled `<title> - <package>`, and an element id is `package:path:role:name`, since a list reuses its rows as it scrolls and an app recreates its window on a theme change. A press scrolls an off-screen row into view. Only one accessibility client runs at a time, so `uiautomator dump` fails while the UI server runs.

## Pool

`cbsbx-conf` gives a `cbdroid` pool a warmup that waits for the agent only (`ANDROID_FAMILIES`).
