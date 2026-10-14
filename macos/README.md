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
CB_ENDPOINT=https://<api host> scripts/bundle.sh   # dist/CocoonBox.app with that endpoint, ad hoc signed; --clean rebuilds from scratch
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

MockBox also serves MCP at `/cocoonbox.mcp/<box id>` on the same port: tools/list with the contract's tool names, and tools/call against the mock desktop; like the box, it records no agent for the owner's sessions. It also serves `Shares` in memory (users `qa2` and `qa3`) and `Agents.End`. `--external-ms 3000` makes the mock play one scripted guest session through the share 3 s after the box runs: an agent named `mcp:claude-code@<share id>[:<user id>]` that calls `shell`, `screenshot`, `computer_act` and `read_file`, then stays open for a minute or until ended.

## Local agents over MCP

**+ → Local agent**, then an agent CLI (`claude`, `codex` or `cursor-agent`), runs that CLI on this Mac in a terminal pane (`CocoonBoxTerminal`: Ghostty's libghostty through [libghostty-spm](https://github.com/Lakr233/libghostty-spm)), with the bridge (`cocoonbox mcp`, design.md §19.6) as its stdio MCP server and the selected box as its default box. The CLI uses your own login on this Mac. The pane shows the terminal, and the live screen shows its actions. The box keeps no agent or transcript for your own MCP sessions; the CLI keeps its own history.

| CLI | How it reaches the box |
|---|---|
| `claude` | `--mcp-config` with a stdio server `cocoonbox mcp --box <id> --endpoint <url>` and `env` `${COCOONBOX_TOKEN}`, `${COCOONBOX_APP_SOCKET}`, `${COCOONBOX_AGENT}` (the CLI expands them), `--append-system-prompt`, `--allowedTools mcp__cocoonbox` |
| `codex` | `-c mcp_servers.cocoonbox.command=…`, `args=[…]`, `env_vars=["COCOONBOX_AGENT", "COCOONBOX_APP_SOCKET", "COCOONBOX_TOKEN"]`, `-c developer_instructions=…` |
| `cursor-agent` | `.cursor/mcp.json` with the same stdio server and `${env:…}` for the three variables, `.cursor/cli.json` allowing `Mcp(*cocoonbox:*)` and a rule file in its workspace, `--approve-mcps --trust` |

The token, the app's socket and the agent's id reach the CLI only through its environment. They are never in argv or on disk. Over the socket the bridge reports each call it routes to a box (not the box_* control tools) and takes the agent's default box (design.md §19.6). **On this Mac** lists every local agent whatever box is selected; a row shows the agent's kind and, after a call in the last minute, the boxes it acted on ("↗ main · test2"). The **Default box** picker in the agent's pane header sets the box of the next call that names none, with no CLI restart; when the switcher shows another box, the header says so. Selecting one leaves the screen on the selected box. Each CLI runs in its own private directory, `~/Library/Application Support/CocoonBox/local-agents/<cli>` (mode 0700), so its trust prompt is answered once. The CLI gets the environment your login shell sets up (`$SHELL -l -i`, seeded with only HOME, USER, SHELL and the like), not the app's own: an app started by `open` carries the caller's shell state, and an inherited `PWD` under `~/Documents` made `claude` read there, which raised the Documents prompt. `PWD` names the session directory. The app runs the CLI on a pseudo-terminal it owns, with an exact argv, feeding Ghostty through the in-memory backend. Ghostty's own exec backend is not used: it wraps every command in `/usr/bin/login -flp <user> /bin/bash --noprofile --norc -c exec -l <command>`, and that `login` spins at 100 % CPU and never starts the command when the app was started by launchd (Finder, `open`). It works only when the app is started from a shell. **End** hangs the CLI up and forgets the agent. Quitting the app (or a crash) leaves the open agents in `~/Library/Application Support/CocoonBox/local-agents.json`, and the next launch reopens each at sign-in, resuming its conversation (`claude --resume`, `codex resume`, `cursor-agent --resume`). Each row shows the CLI's terminal title as its name; double-click to rename it, and an empty name returns to the terminal title.

macOS charges a CLI's file access to CocoonBox, so a CLI that reads `~/Documents` makes macOS ask whether CocoonBox may access Documents. With the environment above, none of the three CLIs does so at startup.

`scripts/bundle.sh` copies GhosttyTerminal's resources to `Contents/Resources`. SwiftPM's resource accessor looks only in the app's root, where a signed app cannot hold files, so `GhosttyResources` redirects that one lookup.

### Copy-paste setup


**Connect a Local Agent** (toolbar and File menu) shows the box's MCP endpoint, `<endpoint>/cocoonbox.mcp/<box id>`, and ready-to-paste setup for the three agent CLIs: `claude mcp add --scope user --transport http … --header 'Authorization: Bearer ${COCOONBOX_TOKEN}'`, `~/.codex/config.toml` with `bearer_token_env_var = "COCOONBOX_TOKEN"`, and `~/.cursor/mcp.json` with `${env:COCOONBOX_TOKEN}`. No snippet contains the token, so none of it lands in a config file or shell history: every client expands `COCOONBOX_TOKEN` at runtime, and one more snippet sets it from the Keychain (`export COCOONBOX_TOKEN="$(security find-generic-password -s me.cmgs.cocoonbox -a <endpoint> -w)"`; macOS asks once whether `security` may read the CocoonBox item).

## Sharing

**Share** (toolbar and File menu) creates the box's share link (design.md §23): an expiry of 24 hours, 7 days or 30 days, optionally only for named cocoonbox users, and read-only unless **Allow write** is on. Right after Create or Regenerate the sheet shows the link (`<endpoint>/cb/s/#<secret>`, the secret in the fragment) and the guest's MCP setup for the three agent CLIs (`Authorization: Bearer <secret>`, plus `X-Cocoonbox-Guest-Token: ${COCOONBOX_TOKEN}` for a named share). The secret lives only in the app's memory; after a restart the sheet offers Regenerate instead. Write can be switched at any time and applies to each guest's next call; Regenerate and Revoke end every session of the old link.

Each guest's MCP session through the share (`mcp:<client>@<share id>[:<user id>]` in the box) appears in the sidebar's collapsible **External** section while it is open, with who (the user's name, or "link"), the client and since when, and an **End** button (`Agents.End`). A session that closes while selected stays listed until you select something else. Selecting it shows its full tool-call log; its ComputerAction markers render on the live view. Your own MCP sessions are not listed: the box does not track them.

## The `cocoonbox` CLI and the stdio MCP bridge

`cocoonbox` (product `cocoonbox-cli`; `Contents/Helpers/cocoonbox` inside the app) lets a local coding agent drive the box with the app closed (design.md §13).

```bash
cocoonbox install                 # link into ~/.local/bin
cocoonbox login                   # token from stdin or a prompt, into the Keychain (service me.cmgs.cocoonbox.cli)
cocoonbox setup claude            # local marketplace + plugin: claude plugin marketplace add / install
cocoonbox setup codex             # codex plugin marketplace add / codex plugin add
cocoonbox setup cursor            # merges {"command": "<cli>", "args": ["mcp"]} into ~/.cursor/mcp.json
cocoonbox status | ensure | save | hibernate | upgrade --yes
```

Every client runs `cocoonbox mcp`, a stdio MCP server. It reads the token from the Keychain and keeps it only in memory. Lifecycle tools go to the control MCP (`/cocoonbox.mcp/control`) and box tools to `/cocoonbox.mcp/<box id>`. The client's own `initialize` is passed through, so the app lists the session under the client's name. With the box hibernated, `tools/list` answers from the box's last list (`~/Library/Caches/cocoonbox`), and the first box-tool call wakes the box, reporting progress on the caller's token; `--no-wake` refuses instead. No client config holds a token or a URL.

The endpoint comes from `login` (`~/Library/Application Support/CocoonBox/cli.json`), not from the environment, because `codex` passes MCP servers only a fixed set of variables. The CLI keeps its own Keychain item while builds are ad hoc signed. Each rebuild has a new code hash, so the first read after a rebuild raises a Keychain prompt, and the bridge waits on it, which a client may count as a failed start; run `cocoonbox login` again after rebuilding. `cocoonbox login --from-app` copies the app's token after one "Always Allow".

`scripts/test-cli.sh` runs the CLI and the bridge against MockBox, which also serves the control MCP.



The box header shows when the box's user data (home, `/workspace`, agent history) was last saved to storage, with **Save now** (`Boxes.SaveState`; the browser in the box closes during the save). **Upgrade desktop…** is enabled only when `Box.latest_image` differs from `Box.image`; it runs `Boxes.Rebase` after a confirmation that running programs restart while files, logins and agent history are kept. Both show their operation's steps, with a byte progress bar while saving or restoring. MockBox starts on `cocoonbox-desktop-v1` with `v2` available and plays both operations.

## Offload

Offload moves a coding task from the user's own `claude` or `codex` session into a box, where a fresh box CLI continues it, and back (design.md §27, contracts.md "Offload"). The app's own local agents drive boxes over MCP and do not offload.

```bash
cocoonbox setup claude                  # once: installs or updates the plugin; then /cocoonbox:offload and /cocoonbox:back in any repo
cocoonbox offload --handover-prompt     # codex and others: paste the printed request; it writes the hand-over, then runs cocoonbox offload
cocoonbox offload --kind codex          # by hand, after writing the hand-over to `cocoonbox offload handover-path`
cocoonbox back                          # lands the box's work and prints the box agent's hand-over
```

The File menu's plugin install item runs `cocoonbox setup claude` and shows its output. The sidebar's **Offloaded** section lists the offloads in flight on every box: repo, box, box CLI, status and time since its last activity. Selecting one shows the box CLI's terminal (tmux over Exec), with Reattach. The app only watches: offload and back start from your own agent CLI's TUI (`/cocoonbox:back` or `cocoonbox back`), and the pane says so with a Copy button for the command. The section's menu installs a box CLI or signs it in, in any box. Sign-in runs the CLI's own login in the box with `BROWSER=true NO_OPEN_BROWSER=1`, so the box's browser stays shut; the app opens the first link it prints on the vendor's sign-in hosts in the Mac's browser, once, with Open in Browser and Copy Link, and the code goes in with ⌘V in the terminal or the field under it. Autopilot's `offloads`, `select-offload`, `plugin-setup`, `box-signin` and `signin-paste` steps log the same.

## Live view

The app opens `Session.Screen` and runs a [swift-rfb](https://github.com/loomhand/swift-rfb) client directly on it: server bytes feed an `RFBPipe`, and the client's bytes go back as `ScreenClientFrame`s. There is no loopback socket. The client decodes into page-aligned memory that `RFBView` shows as a Metal texture without a copy. It draws on the display link only after a change, and asks for the next update once the GPU has read the frame, so an idle screen costs no CPU and no wakeups. It uses ContinuousUpdates when the server offers them. Otherwise it paces incremental requests every 33 ms while updates arrive, so a high-latency path is not held to one update per round trip.

While you have taken over, the Edit menu's Paste, Copy and Cut (⌘V, ⌘C, ⌘X) act on the box when the screen has focus. ⌘V hands the Mac's plain text to the box's clipboard (ExtendedClipboard, UTF-8) and presses Shift+Insert there. ⌘C and ⌘X press Ctrl+Insert or Shift+Delete, and the box's next clipboard within 2 s goes to the Mac's. These are the keys that terminals honour as well as GTK apps and Chrome: in xfce4-terminal Ctrl+V types a literal next character and Ctrl+C interrupts. Nothing crosses otherwise: the box's own clipboard changes are ignored, so an agent in the box cannot rewrite the Mac's clipboard. Text over 1 MiB is dropped either way. The log records lengths only, never the text.

swift-rfb is a local path package (`../../swift-rfb`), so it must be checked out next to this repository.

## Automation and measurements

These environment variables serve test runs:

- `CB_ENDPOINT`: the endpoint URL. It overrides the one `scripts/bundle.sh` wrote into the app. The app speaks plaintext `http://` only to loopback (`127.*`, `::1`, `localhost`), so a bearer token never crosses the network unencrypted.
- `CB_KEYCHAIN_SERVICE`: the Keychain service for the token (default `me.cmgs.cocoonbox`). Ad hoc signed dev builds use `me.cmgs.cocoonbox.dev`, so they never read the installed app's item and never raise its Keychain prompt. To remove that item when you are done: `security delete-generic-password -s me.cmgs.cocoonbox.dev -a <endpoint url>`.
- `CB_KEYCHAIN_FILE`: a keychain file to use instead of the login keychain, so tests never touch the user's own. Create it with `security create-keychain` and unlock it with `security unlock-keychain`; `scripts/test-cli.sh` does this, and also runs with `HOME` set to a test directory.
- `CB_TOKEN_STDIN=1`: read one token line from stdin and store it in the Keychain before starting. Launch with `open --stdin <fifo> --env CB_TOKEN_STDIN=1 dist/CocoonBox.app`: exec'ing the binary directly may not get the app a window.
- `CB_AUTOPILOT`: a `;`-separated list of steps, listed in `Sources/CocoonBox/Autopilot.swift`.
- `CB_PASTEBOARD=<name>`: use that private pasteboard instead of the general one, so a test run leaves the user's clipboard alone. Autopilot's `clip-paste` and `signin-paste` need it.
- `CB_NO_BROWSER=1`: log a box CLI's sign-in link (`signin_link`) instead of opening it in the Mac's browser.
- `CB_RFB_RAW=1`: offer only Raw, for throughput baselines.
- `CB_RFB_PACE=0`: turn off request pacing, for A/B runs.
- `CB_RFB_EAGER=1`: send the whole client side of the RFB handshake before the server's banner. This saves a round trip, but works only with a server that speaks RFB 3.8 without a password. It overrides the `rfbEagerHandshake` default (`defaults write me.cmgs.cocoonbox rfbEagerHandshake -bool true`), which is off.
- `CB_SCREEN_TRACE=<n>`: log the first n Screen chunks each way (`screen_up`/`screen_down`, default 8). Autopilot's `reopen-screen` starts a new RFB session, which begins with a full frame.
- `CB_RFB_PROBE=1`: count the RFB rectangle encodings the server uses. It logs `rfb_encodings` when the session ends, and adds a clock read per rectangle.

Timings go to `~/Library/Logs/CocoonBox/debug.log` and to os_signpost (subsystem `me.cmgs.cocoonbox`), in milliseconds since process start. The key lines are `first_frame_presented`, `rfb_first_update`, `input_rtt` (from input to the decoded update) and `input_to_present` (from input to the frame on glass).

Ad hoc signing gives every build a new code hash. After a rebuild, the Keychain asks before it lets the new binary read a token that an older build stored. A Developer ID signature removes the prompt.

Tokens live in the login Keychain under service `me.cmgs.cocoonbox`, one item per endpoint URL.
