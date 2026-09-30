# CocoonBox for macOS

Native client for cocoonbox: sign in, start or wake the box, talk to its agents, and watch or take over the box's desktop.

| Target | What |
|---|---|
| `CocoonBoxKit` | the app's only interface below the UI: `BoxConnection` (one HTTP/2 connection; Boxes, Agents and Session calls and the resumable event follower, with the gRPC clients kept internal) and `LiveScreen` (the swift-rfb client and view over `Session.Screen`) |
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

MockBox also serves MCP at `/cocoonbox.mcp/<box id>` on the same port: tools/list with the contract's tool names, and tools/call against the mock desktop, as an `mcp:<clientInfo.name>` agent. `--external-ms 3000` makes the mock play one scripted MCP session 3 s after the box runs: an agent named `mcp:claude-code` that calls `shell`, `screenshot`, `computer_act` and `read_file`.

## Local agents over MCP

**+ → Local agent → Claude Code / Codex / Cursor** runs that CLI on this Mac in a terminal pane (`CocoonBoxTerminal`: Ghostty's libghostty through [libghostty-spm](https://github.com/Lakr233/libghostty-spm)), already pointed at the box's MCP endpoint. The CLI uses your own login on this Mac. The pane shows the terminal above the transcript of the `mcp:*` agent the CLI opens on the box, and the live screen shows its actions.

| CLI | How it reaches the box |
|---|---|
| Claude Code | `--mcp-config` with header `Authorization: Bearer ${COCOONBOX_TOKEN}` (Claude Code expands it), `--append-system-prompt`, `--allowedTools mcp__cocoonbox` |
| Codex | `-c mcp_servers.cocoonbox.url=…`, `-c mcp_servers.cocoonbox.bearer_token_env_var="COCOONBOX_TOKEN"`, `-c developer_instructions=…` |
| Cursor | `.cursor/mcp.json` with `${env:COCOONBOX_TOKEN}` and a rule file in its workspace, `--approve-mcps --trust` |

The token reaches the CLI only through `COCOONBOX_TOKEN` in its environment. It is never in argv or on disk. Each session runs in its own private directory, `~/Library/Application Support/CocoonBox/sessions/<id>` (mode 0700), which is removed when the session ends. The CLI gets the environment your login shell sets up (`$SHELL -l -i`, seeded with only HOME, USER, SHELL and the like), not the app's own: an app started by `open` carries the caller's shell state, and an inherited `PWD` under `~/Documents` made Claude Code read there, which raised the Documents prompt. `PWD` names the session directory. The app runs the CLI on a pseudo-terminal it owns, with an exact argv, feeding Ghostty through the in-memory backend. Ghostty's own exec backend is not used: it wraps every command in `/usr/bin/login -flp <user> /bin/bash --noprofile --norc -c exec -l <command>`, and that `login` spins at 100 % CPU and never starts the command when the app was started by launchd (Finder, `open`). It works only when the app is started from a shell. **End** hangs the CLI up. Quitting the app ends every local agent.

macOS charges a CLI's file access to CocoonBox, so a CLI that reads `~/Documents` makes macOS ask whether CocoonBox may access Documents. With the environment above, none of the three CLIs does so at startup. Because each session directory is new, Claude Code asks whether to trust it on every launch; its default answer is No.

`scripts/bundle.sh` copies GhosttyTerminal's resources to `Contents/Resources`. SwiftPM's resource accessor looks only in the app's root, where a signed app cannot hold files, so `GhosttyResources` redirects that one lookup.

### Copy-paste setup


**Connect a Local Agent** (toolbar and File menu) shows the box's MCP endpoint, `<endpoint>/cocoonbox.mcp/<box id>`, and ready-to-paste setup for Claude Code (`claude mcp add --scope user --transport http …`), Codex (`~/.codex/config.toml` with `bearer_token_env_var`) and Cursor (`~/.cursor/mcp.json`). The sheet shows the token masked. Copy reads it from the Keychain and puts the snippet with the real token on the pasteboard. The token is never logged. There is no one-click install: `claude mcp add` takes a header only as an argument, so the token would show up in the process list.

Each MCP session appears under Agents as `mcp:<client>`, with an External badge. Its tool calls and ComputerAction markers render like the box agent's. The composer is replaced by a note, because the agent is prompted from the local client.

## Live view

The app opens `Session.Screen` and runs a [swift-rfb](https://github.com/CMGS/swift-rfb) client directly on it: server bytes feed an `RFBPipe`, and the client's bytes go back as `ScreenClientFrame`s. There is no loopback socket. The client decodes into page-aligned memory that `RFBView` shows as a Metal texture without a copy. It draws on the display link only after a change, and asks for the next update once the GPU has read the frame, so an idle screen costs no CPU and no wakeups. It uses ContinuousUpdates when the server offers them. Otherwise it paces incremental requests every 33 ms while updates arrive, so a high-latency path is not held to one update per round trip.

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
