# cocoonbox-agent

The in-box agent of a cocoonbox. One binary, two subcommands:

- `serve` runs as `cbagent`. It serves `cocoonbox.agent.v1.Agents` and `Session` over h2c gRPC on `AGENT_LISTEN` and runs the agent loop. It keeps state in `/var/lib/cocoonbox/agent.db`.
- `toolhost` runs as the desktop user `user`. It executes every tool behind `/run/cocoonbox/toolhost.sock`.

## Modules

| File | Role |
|---|---|
| `server.rs` | gRPC services and the `x-cocoonbox-edge-token` check (`UNAUTHENTICATED` otherwise) |
| `runner.rs` | Per-agent turn queue, the main agent loop, take-over approvals, cancel |
| `computer.rs` | Computer subagent: a vision model driving the desktop through `computer_state` / `computer_act` |
| `llm/` | Streaming gw client. `openai.rs` is chat completions (SSE) and serves every model by default. `anthropic.rs` is Messages, used for `anthropic/` models only when `GW_ANTHROPIC_WIRE=messages`. `sse.rs` is the decoder |
| `store.rs` | SQLite tables: agents, turns, transcript entries (global monotonic `seq`), client-nonce dedupe |
| `bus.rs` | Event fan-out. Offsets start at boot time in µs. A bounded replay ring serves `Events(from_offset)` |
| `screen.rs` | Raw RFB relay to `127.0.0.1:5900` after the `open` frame |
| `toolhost/` | `rpc.rs` (u32-length-prefixed JSON, one call per connection), `shell.rs`, `files.rs`, `browser.rs` (Chrome over CDP), `cua.rs` (cua-driver socket client) |
| `prompts.rs`, `tools.rs` | System prompts and tool schemas |

## Behaviour

- **Turns.** `Send` persists the user entry, then queues the turn. Each agent runs its turns in order. A `client_nonce` seen before returns the first turn id, and `Create` is deduplicated the same way. `Cancel` aborts the running turn: the in-flight model request is dropped, and the toolhost connection closes, which kills the shell command. The turn then records a `TurnEnd` with `ok=false`.
- **Transcript.** Text and reasoning blocks are recorded when each block completes: text before a tool call is flushed when the call starts. Tool calls and tool results are recorded as they happen. Subagent tool calls carry `subagent: "computer"`. The subagent's own prose is not recorded; its report is the `delegate_computer` result, and that result carries the final screenshot.
- **Images.** The main model is treated as text-only, so screenshots are replaced with a note. The vision subagent receives screenshots as image parts that follow the tool results.
- **Shell.** Each agent has one persistent bash session with cwd `/workspace`. Commands are sourced from a script file with stdin set to `/dev/null`. Output is capped to about 30 KB (head and tail). The default timeout is 120 s, up to 600 s. A timeout or cancel kills the session's process group, and the next call starts a fresh session.
- **File paths.** A relative path resolves under `/workspace`. An absolute path is used as given, so the uid `user` is the access boundary.
- **Browser.** The toolhost attaches to Chrome on `127.0.0.1:9222`, or launches it with the `/home/user/.config/cocoonbox-chrome` profile. A snapshot is a ref-tagged list (`- link "Learn more" [ref=e1]`) of at most 300 lines. It includes open shadow roots but not iframes. Clicks are CDP mouse events at the element's center.
- **Computer.** Each `delegate_computer` task runs in its own cua-driver session (`cu-…`). Every call of the task carries that label. `end_session` runs when the task ends, or from a detached task if a cancel drops it. `computer_state` returns the desktop screenshot, the window list and the AT-SPI elements of one window. `computer_act` targets an element by index from the latest state, or screen pixels. Keyboard input with neither goes to the frontmost app window at the moment of the action, and `foreground: true` escalates delivery. A fresh screenshot follows every action, and a `ComputerAction` event is published before each input. For `anthropic/` models on the chat wire, the system prompt and the newest user message carry `cache_control`. Screenshots are pruned in batches (above 6, keep 3) so the cached prefix survives most steps.

## Environment

These keys come from `/etc/cocoonbox/agent.env` (see `docs/contracts.md`): `GW_BASE_URL`, `GW_API_KEY`, `MODEL_MAIN`, `MODEL_COMPUTER`, `EDGE_TOKEN`, `AGENT_LISTEN`.

Optional overrides with contract defaults, used by tests:

| Key | Default |
|---|---|
| `CB_STATE_DIR` | `/var/lib/cocoonbox` |
| `TOOLHOST_SOCKET` | `/run/cocoonbox/toolhost.sock` |
| `VNC_ADDR` | `127.0.0.1:5900` |
| `CB_WORKSPACE` | `/workspace` |
| `CHROME_BIN` | `google-chrome` |
| `CHROME_PROFILE` | `~/.config/cocoonbox-chrome` |
| `CUA_DRIVER_SOCKET` | `~/.cache/cua-driver/cua-driver.sock` |
| `GW_ANTHROPIC_WIRE` | `chat` (`messages` switches `anthropic/` models to `/v1/messages`) |

## Build

```
cargo fmt --check && cargo clippy --all-targets -- -D warnings && cargo test
cargo build --release --locked
```

`protoc` must be on `PATH`. The protos are read from `../proto`.
