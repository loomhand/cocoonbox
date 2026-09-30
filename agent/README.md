# cocoonbox-agent

The in-box agent of a cocoonbox. One binary, two subcommands:

- `serve` runs as `cbagent`. It serves `cocoonbox.agent.v1.Agents` and `Session` over h2c gRPC on `AGENT_LISTEN` and runs the agent loop. The same listener also answers MCP Streamable HTTP at `/mcp` (mode B). It keeps state in `/var/lib/cocoonbox/agent.db`.
- `toolhost` runs as the desktop user `user`. It executes every tool behind `/run/cocoonbox/toolhost.sock`.

## Layers

The agent follows the layering in `docs/boundaries.md`:

```
transports: server.rs (gRPC Agents, Session) · mcp.rs (/mcp)
        │ runner.rs (loop, mode A) and mcp.rs both call dispatch::Dispatcher
        ▼
dispatch.rs: records every call and result (journal.rs), publishes ComputerAction, injects the desktop session
        │ Tools interface (dispatch::Tools; production impl: toolhost RPC client)
        ▼
toolhost: shell · files · CDP browser · cua-driver
```

- The loop reaches models only through `llm::Model`. Its production implementation is `llm::Client`, which speaks the chat wire to the gw URL.
- `server::serve` is the composition root: it builds the Model and Tools implementations and hands them to `Runtime::open`.
- Nothing below the dispatcher knows whether mode A or mode B issued a call. The agent knows nothing about how its machine is scheduled.

## Modules

| File | Role |
|---|---|
| `server.rs` | gRPC services and the `x-cocoonbox-edge-token` check (`UNAUTHENTICATED` otherwise) |
| `dispatch.rs` | The one tool path: `Tools` interface, `Dispatcher` (record call and result, ComputerAction, driver-session injection) |
| `journal.rs` | Store plus event bus: every transcript and status write is persisted, then published |
| `runner.rs` | Per-agent turn queue, the main agent loop, take-over approvals, cancel |
| `computer.rs` | Computer subagent: a vision model driving the desktop through `computer_state` / `computer_act` |
| `llm/` | Streaming gw client. `openai.rs` is chat completions (SSE) and serves every model by default. `anthropic.rs` is Messages, used for `anthropic/` models only when `GW_ANTHROPIC_WIRE=messages`. `sse.rs` is the decoder |
| `store.rs` | SQLite tables: agents, turns, transcript entries (global monotonic `seq`), client-nonce dedupe |
| `bus.rs` | Event fan-out. Offsets start at boot time in µs. A bounded replay ring serves `Events(from_offset)` |
| `mcp.rs` | MCP Streamable HTTP at `/mcp` for external agents (mode B), next to gRPC on the same listener |
| `screen.rs` | Raw RFB relay to `127.0.0.1:5900` after the `open` frame |
| `toolhost/` | `rpc.rs` (u32-length-prefixed JSON, one call per connection), `shell.rs`, `files.rs`, `browser.rs` (Chrome over CDP), `cua.rs` (cua-driver socket client) |
| `prompts.rs`, `tools.rs` | System prompts and tool schemas |

## Behaviour

- **Turns.** `Send` persists the user entry, then queues the turn. Each agent runs its turns in order. A `client_nonce` seen before returns the first turn id, and `Create` is deduplicated the same way. `Cancel` aborts the running turn: the in-flight model request is dropped, and the toolhost connection closes, which kills the shell command. The turn then records a `TurnEnd` with `ok=false`.
- **Transcript and streaming.** Each text or reasoning block streams live as `TextDelta` / `ReasoningDelta` events carrying `(turn_id, block)`. `block` numbers the blocks of a turn. When a block completes, it is recorded once as a `TranscriptEntry` whose `TextBlock` / `ReasoningBlock` carries the same `block`. Deltas take event offsets but are not replayed; a reconnecting client gets the full blocks. Tool calls and tool results are recorded as they happen. Subagent text, reasoning and tool calls carry `subagent: "computer"`, and the subagent's report is the `delegate_computer` result, which carries the final screenshot.
- **Images.** A main model with vision (`anthropic/`, `google/gemini`, `openai/gpt-4o`, `openai/gpt-5` or a `-vl` slug) gets a read-only `screenshot` tool, and it receives the delegate's final screenshot. Any other main model gets a text note instead. The vision subagent receives screenshots as image parts that follow the tool results.
- **Shell.** Each agent has one persistent bash session with cwd `/workspace`. Commands are sourced from a script file with stdin set to `/dev/null`. Output is capped to about 30 KB (head and tail). The default timeout is 120 s, up to 600 s. A timeout or cancel kills the session's process group, and the next call starts a fresh session.
- **File paths.** A relative path resolves under `/workspace`. An absolute path is used as given, so the uid `user` is the access boundary.
- **Browser.** The toolhost attaches to Chrome on `127.0.0.1:9222`, or launches it with the `/home/user/.config/cocoonbox-chrome` profile. A snapshot is a ref-tagged list (`- link "Learn more" [ref=e1]`) of at most 300 lines. It includes open shadow roots but not iframes. Clicks are CDP mouse events at the element's center.
- **Computer.** Each `delegate_computer` task runs in its own cua-driver session (`cu-…`). Every call of the task carries that label. `end_session` runs when the task ends, or from a detached task if a cancel drops it. `computer_state` returns the desktop screenshot, the window list and the AT-SPI elements of one window. `computer_act` targets an element by index from the latest state, or screen pixels. Keyboard input with neither goes to the frontmost app window at the moment of the action, and `foreground: true` escalates delivery. A fresh screenshot follows every action, and a `ComputerAction` event is published before each input. For `anthropic/` models on the chat wire, the system prompt and the newest user message carry `cache_control`. Screenshots are pruned in batches (above 6, keep 3) so the cached prefix survives most steps.

## MCP (mode B)

External agents such as Claude Code, Codex or Cursor use the box through `POST|GET|DELETE /mcp`, with protocol 2025-03-26, 2025-06-18 or 2025-11-25. Every request needs `x-cocoonbox-edge-token`, which the edge adds.

- `initialize` returns an `Mcp-Session-Id`. The session appears as an agent `mcp:<clientInfo.name>` whose status is RUNNING while the session lives.
- Each tool call and its result go into that agent's transcript and onto `Session.Events`. `computer_act` also publishes a `ComputerAction` event.
- Tools: `shell`, `read_file`, `write_file`, `edit_file`, `list_dir`, `browser_navigate`, `browser_snapshot`, `browser_click`, `browser_type`, `browser_read`, `computer_state`, `computer_act`, `screenshot`. They run in the toolhost exactly as the loop's calls do.
- Screenshots come back as MCP `image` content (PNG). `max_edge` scales a screenshot, and pixel coordinates then refer to the scaled image: cua-driver maps them back to the screen.
- Each MCP session owns one cua-driver session label. It is ended by `DELETE`, by closing the GET stream (a later call starts a new label), or after 10 min without activity, which also closes the MCP session.
- The toolhost holds one input lock per display, so a `computer_act` from mode A and one over MCP never interleave.
- POST answers with `application/json`. GET is an SSE stream that carries only keepalive comments, because the server sends no requests of its own.

## Limits

- Queued turns and pending take-over approvals live in memory. After a restart, every unfinished turn gets a `TurnEnd` with the error "interrupted: the agent restarted".
- Screenshots are stored in `agent.db` with every subagent tool result, about 50–100 KB each, and there is no retention policy yet.
- The browser snapshot skips iframes. A click is a CDP mouse event at the element's center, so a covered element gets the click on whatever covers it.
- On XFCE, a background element click on the Applications toggle has no effect. The subagent is told to trust only screenshots and to retry once with `foreground: true`.
- Delta events are not coalesced: one gw SSE chunk produces one event. A slow client that falls behind the live channel gets `DATA_LOSS` and reconnects with `from_offset`.
- The main agent's history from earlier turns keeps no images.

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
./build-release.sh        # cargo build --release --locked with path remapping; prints the sha256
```

`build-release.sh` remaps the target dir, `CARGO_HOME` and the source dir, so the same commit builds to the same bytes in any directory.

`protoc` must be on `PATH`. The protos are read from `../proto`.
