# CocoonBox for macOS

Native client for cocoonbox: sign in, start or wake the box, talk to its agents, and watch or take over the box's desktop.

| Target | What |
|---|---|
| `CocoonBoxKit` | generated protobuf and gRPC clients, the shared HTTP/2 connection, resumable event follower, the RFB session over `Session.Screen` |
| `CocoonBox` | the SwiftUI + AppKit app |
| `MockBox` | a local server for Boxes, Agents and Session with canned turns and a synthetic RFB desktop |
| `RFBTestServer` | the synthetic RFB 3.8 server used by MockBox and the tests |
| `CocoonBoxKitTests` | swift-testing suite, built as an executable |

## Build and run

Requires macOS 15 and the Command Line Tools (Swift 6.2). Xcode is not needed.

```bash
scripts/bundle.sh            # dist/CocoonBox.app, ad hoc signed; --clean rebuilds from scratch
scripts/test.sh              # unit tests
scripts/gen-proto.sh         # regenerate Sources/CocoonBoxKit/Generated from ../proto
open dist/CocoonBox.app
```

`scripts/toolchain.sh` is sourced by every script. It works around Command Line Tools installs that carry stale files from older releases, and it does nothing on a clean install.

Against the mock:

```bash
swift build --product MockBox && .build/debug/MockBox --port 50051 [--animate 30]
open --env CB_ENDPOINT=http://127.0.0.1:50051 dist/CocoonBox.app   # sign in with token mock-token
```

## Live view

The app opens `Session.Screen` and runs a [swift-rfb](https://github.com/CMGS/swift-rfb) client directly on it: server bytes feed an `RFBPipe`, and the client's bytes go back as `ScreenClientFrame`s. There is no loopback socket. The client decodes into page-aligned memory that `RFBView` shows as a Metal texture without a copy. It draws on the display link only after a change, and asks for the next update once the GPU has read the frame, so an idle screen costs no CPU and no wakeups. It uses ContinuousUpdates when the server offers them (TigerVNC). Otherwise it paces incremental requests every 33 ms while updates arrive (x11vnc), so a high-latency path is not held to one update per round trip.

swift-rfb is a local path package (`../../swift-rfb`), so it must be checked out next to this repository.

## Automation and measurements

These environment variables serve test runs:

- `CB_ENDPOINT`: the endpoint URL. It overrides the saved endpoint.
- `CB_KEYCHAIN_SERVICE`: the Keychain service for the token (default `me.cmgs.cocoonbox`). Ad hoc signed dev builds use `me.cmgs.cocoonbox.dev`, so they never read the installed app's item and never raise its Keychain prompt. To remove that item when you are done: `security delete-generic-password -s me.cmgs.cocoonbox.dev -a <endpoint url>`.
- `CB_TOKEN_STDIN=1`: read one token line from stdin and store it in the Keychain before starting. Launch with `open --stdin <fifo> --env CB_TOKEN_STDIN=1 dist/CocoonBox.app`: exec'ing the binary directly may not get the app a window.
- `CB_AUTOPILOT`: a `;`-separated list of steps, listed in `Sources/CocoonBox/Autopilot.swift`.
- `CB_RFB_RAW=1`: offer only Raw, for throughput baselines.
- `CB_RFB_PACE=0`: turn off request pacing, for A/B runs.
- `CB_RFB_EAGER=1`: send the whole client side of the RFB handshake before the server's banner. This saves a round trip, but works only with a server that speaks RFB 3.8 without a password. It overrides the `rfbEagerHandshake` default (`defaults write me.cmgs.cocoonbox rfbEagerHandshake -bool true`), which is off.
- `CB_SCREEN_TRACE=<n>`: log the first n Screen chunks each way (`screen_up`/`screen_down`, default 8). Autopilot's `reopen-screen` starts a new RFB session, which begins with a full frame.
- `CB_RFB_PROBE=1`: count the RFB rectangle encodings the server uses. It logs `rfb_encodings` when the session ends, and adds a clock read per rectangle.

Timings go to `~/Library/Logs/CocoonBox/debug.log` and to os_signpost (subsystem `me.cmgs.cocoonbox`), in milliseconds since process start. The key lines are `first_frame_presented`, `rfb_first_update`, `input_rtt` (from input to the decoded update) and `input_to_present` (from input to the frame on glass).

Ad hoc signing gives every build a new code hash. After a rebuild, the Keychain asks before it lets the new binary read a token that an older build stored. A Developer ID signature removes the prompt.

Tokens live in the login Keychain under service `me.cmgs.cocoonbox`, one item per endpoint URL. Builds before the rename used `me.cmgs.cocoonbox`. To remove the old items, run this once per endpoint until it reports that the item could not be found:

```bash
security delete-generic-password -s me.cmgs.cocoonbox -a https://box.example.com
```
