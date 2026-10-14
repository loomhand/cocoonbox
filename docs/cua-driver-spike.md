# cua-driver 0.30.4 spike (M1 gate)

Date: 2026-09-30. Guest: `cocoonbox-desktop` provisioned by `image/provision.sh` on the bench host (cocoon-cb, Cloud Hypervisor v54, Ubuntu 24.04, kernel 6.8.0-142-generic), real Xorg 1.21.1.11 with `xserver-xorg-video-dummy` at 1280x800, XFCE 4.18, `uinput` built into the kernel, `/dev/uinput` root:input 0660, `user` in `input`. cua-driver runs as the systemd user unit `cua-driver.service` (`cua-driver serve`).

**Verdict: GO.** AT-SPI trees, element-token clicks, focus-free pixel input (MPX virtual pointer and keyboard over uinput) and x11vnc capture all work on Xorg + dummy + uinput. Foreground delivery works as the fallback. The findings below are rules for the agent's `computer` tool.

The test scripts ran in the guest as `user`; `page.html` is the Chrome fixture. Screenshots (`spike-run1` .. `spike-run6`) are x11vnc frames grabbed over RFB, independent of cua-driver's own capture.

## doctor

```
$ cua-driver doctor
[ok  ] binary: cua-driver 0.30.4 (x86_64-linux)
[ok  ] install dir: /opt/cua-driver/0.30.4/cua-driver
[ok  ] home dir: /home/user/.cua-driver (0 release dirs cached)
[ok  ] telemetry: disabled via persisted (installation ID retained)
[ok  ] display server: X11 (DISPLAY=:0)
[ok  ] X11 connection: connected, 4 visible top-level windows
[ok  ] AT-SPI: org.a11y.Bus reachable via session bus
```

The same output comes back on a clone of `cocoonbox-desktop-v1`. Telemetry is on by default: the first daemon start sent a registration event before provisioning disabled it. `provision.sh` now runs `telemetry disable` and `reset-id`, and the unit sets `CUA_DRIVER_RS_TELEMETRY_ENABLED=false`.

## get_window_state

| App | Elements | Walk | Sample |
|---|---|---|---|
| Mousepad (`/workspace/note.txt`) | 20 (complete) | 55 ms | `0 menu 'File' [click] {x5 y56 w39 h25}`, `5 menu 'Help'`, `17 text 'line one\nline two\n'`, `7 push button 'Open...'` |
| Chrome 154 (`page.html`, `--force-renderer-accessibility`) | 38 (complete) | 356 ms | `12 entry 'Address and search bar' [activate]`, `21 document web 'cb-spike'`, `23 push button 'Element A' [press] {x708 y187 w360 h120}`, `24 push button 'Pixel B'`, `25 heading 'none clicked'` |

Web content is in the tree, with screen frames and live text: after each click, the heading's label is the page state.

## Input results

Two windows sit side by side: Mousepad on the left and Chrome on the right. "Active/focus" means `_NET_ACTIVE_WINDOW` / X input focus, read before and after each call.

| # | Action | Route reported | Landed? | Focus | Evidence |
|---|---|---|---|---|---|
| 1 | `click {element_token}` on Chrome "Element A", Mousepad active | `accessibility`, 0.32 s | yes: page red, "A clicked" | Mousepad before and after | run1 `10-before` → `11-after-element-click` |
| 2 | background `click {window_id, x, y}` on "Pixel B" | `x11_atspi`, hit `push button action=press`, 1.73 s | yes: page green, "B clicked" | unchanged | run1 `12-after-pixel-click` |
| 3 | background `click {window_id, x, y}` on a plain `<div>` | `x11_atspi`, hit `section action=doDefault` | **no**: onmousedown never fired, but the result says "Clicked" | unchanged | run2 `21-after-raw-pixel-click` |
| 4 | background `click` in Mousepad's text area, Chrome active, then background `type_text "PX1"` | `mpx_pointer` ("focus restored"), 2.43 s; then `mpx_uinput` | yes: caret moved to the end of line 2, text is `line twoPX1` (AT-SPI readback and screenshot) | Chrome before and after | run3, run4 `41` |
| 5 | background `type_text "BG1"` into Mousepad, Chrome active | `mpx_uinput` ("focus untouched"), 0.69 s | yes (AT-SPI readback) | Chrome before and after | run2 `22-after-bg-type` |
| 6 | `click {target: desktop}` at screen (884,467) | `global_input` (XTEST), 0.05 s | yes: `C trusted=true` | moves to Chrome | run3 `31` |
| 7 | `click {..., delivery_mode: foreground}` on the plain div | `x11_xtest_fg`, 0.92 s | yes: `C trusted=true at 180,50` | moves to Chrome | run4 `41-after-fg-pixel` |
| 8 | `type_text {delivery_mode: foreground}` into a settled Mousepad | `global_input`, 0.91 s | yes: `BG2 FG3 clone line` | moves to Mousepad | run6 `60-after-fg-type` |

Background input on Xorg + uinput therefore works for both routes the design needs: element tokens go through AT-SPI actions, and pixels plus keys go through a per-session XI2 master pair backed by uinput. No run froze the desktop or X (upstream #3871) or silently dropped background `type_text` (upstream #3774).

## Rules for the computer tool

1. **Share one `session` label across a task.** Element tokens and the pixel-click capture context are scoped per session. A `click` from a new CLI/MCP session answers `stale_element_token` or `screenshot_context_missing`, even straight after a `get_window_state`. A fresh `get_window_state` in the same session also replaces the capture context, so take the screenshot last before a pixel click.
2. **Call `end_session` when a task ends.** Each session adds one XI2 master pointer/keyboard pair (`xinput list` went from 1 to 4 master pointers over three sessions) and keeps it until `end_session`. After `end_session` the count dropped back to 1.
3. **A background pixel click on an accessible runs that accessible's action, not a mouse event.** That is right for buttons (row 2) and a silent no-op for custom-drawn web regions (row 3). For web content use CDP (tier 3) or `delivery_mode: foreground`. The raw virtual-pointer path (`mpx_pointer`) only runs where AT-SPI finds nothing actionable (row 4).
4. **Treat `effect` as advisory.** Row 3 says "Clicked". In one foreground `type_text` sent while Mousepad was still swapping its startup window, the result claimed `effect: confirmed` although the text went nowhere. Verify with a screenshot or an AT-SPI value readback.
5. **Foreground needs a window manager that has settled.** On the golden, after an earlier test toggled XFCE "Show Desktop", `_NET_ACTIVE_WINDOW` stayed `0x0` and foreground `type_text` refused with `foreground_unavailable` (no input sent). On a clean clone, foreground works (rows 7 and 8). `bring_to_front` sometimes leaves `_NET_ACTIVE_WINDOW` and X focus on different windows, so check both.
6. `get_window_state` screenshots are window-local. `click {window_id, x, y}` takes window-local pixels, and `get_desktop_state` coordinates need `coordinate_frame: "desktop"`.

## x11vnc

```
$ python3 rfbshot.py 127.0.0.1 5900 out.png      # run in the box, or through an ssh -L tunnel from the host
RFB 003.008 name='vk-cocoonbox-golden-0-6eeb38:0' 1280x800 -> out.png
```

The handshake uses RFB 3.8 with security type None, and the framebuffer is 1280x800 at 32 bpp. x11vnc listens only on `127.0.0.1:5900` and `[::1]:5900`. Clones keep the golden's name in the desktop string, because clones restore memory.

## Open items

- Chrome's UI language follows the caller's locale. SSH `AcceptEnv LANG LC_*` passed `zh_CN` through until provisioning disabled it. The toolhost should launch Chrome with an explicit `LANG`.
- Chrome launched by the base image's autostart entry is removed. The toolhost owns Chrome (CDP on `127.0.0.1:9222`, profile `/home/user/.config/cocoonbox-chrome`).
- Not tested: `scroll`, `drag`, `hotkey`, menus through `invoke_menu`, and LibreOffice or GTK2 apps.
