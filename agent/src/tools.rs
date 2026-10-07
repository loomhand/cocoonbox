//! Tool schemas offered to the main agent and the computer subagent.

use std::borrow::Cow;

use serde_json::json;

use crate::llm::ToolSpec;

const LOOP_ONLY: [&str; 2] = ["delegate_computer", "request_take_over"];
const SCRIPT_HINT: &str = " On this box, complex or repeated GUI work (typing and saving in an app, dialogs, forms, several items) goes to script_run as one script, not a chain of computer_act calls.";

/// Remote tools that only read the box: all a read-only share offers.
pub const READ_ONLY: [&str; 6] = [
    "screenshot",
    "computer_state",
    "read_file",
    "list_dir",
    "browser_snapshot",
    "browser_read",
];

/// Tools of the main agent; a vision model also gets a read-only screenshot, a Script box `script_run`.
pub fn main_tools(vision: bool, script: bool) -> Vec<ToolSpec> {
    let mut tools = vec![
        ToolSpec {
            name: Cow::Borrowed("shell"),
            description: Cow::Borrowed(
                "Run a bash command in the agent's persistent shell session (cwd /workspace). Returns merged stdout/stderr and the exit status. Output over ~30 KB keeps only its head and tail.",
            ),
            parameters: json!({
                "type": "object",
                "properties": {
                    "command": {"type": "string", "description": "Bash command or script"},
                    "timeout_s": {"type": "integer", "description": "Timeout in seconds, default 120, max 600. A timeout resets the session."}
                },
                "required": ["command"]
            }),
        },
        ToolSpec {
            name: Cow::Borrowed("read_file"),
            description: Cow::Borrowed(
                "Read a text file with line numbers. Relative paths resolve under /workspace.",
            ),
            parameters: json!({
                "type": "object",
                "properties": {
                    "path": {"type": "string"},
                    "offset": {"type": "integer", "description": "First line, 1-based"},
                    "limit": {"type": "integer", "description": "Max lines, default 2000"}
                },
                "required": ["path"]
            }),
        },
        ToolSpec {
            name: Cow::Borrowed("write_file"),
            description: Cow::Borrowed(
                "Create or replace a file with the given content; parent directories are created.",
            ),
            parameters: json!({
                "type": "object",
                "properties": {"path": {"type": "string"}, "content": {"type": "string"}},
                "required": ["path", "content"]
            }),
        },
        ToolSpec {
            name: Cow::Borrowed("edit_file"),
            description: Cow::Borrowed(
                "Replace an exact string in a file. old_string must match exactly once unless replace_all is true.",
            ),
            parameters: json!({
                "type": "object",
                "properties": {
                    "path": {"type": "string"},
                    "old_string": {"type": "string"},
                    "new_string": {"type": "string"},
                    "replace_all": {"type": "boolean"}
                },
                "required": ["path", "old_string", "new_string"]
            }),
        },
        ToolSpec {
            name: Cow::Borrowed("list_dir"),
            description: Cow::Borrowed("List a directory (default /workspace)."),
            parameters: json!({"type": "object", "properties": {"path": {"type": "string"}}}),
        },
        ToolSpec {
            name: Cow::Borrowed("browser_navigate"),
            description: Cow::Borrowed(
                "Open a URL in the box's Chrome and return the page snapshot: headings, text and interactive elements tagged [ref=eN].",
            ),
            parameters: json!({
                "type": "object",
                "properties": {"url": {"type": "string"}},
                "required": ["url"]
            }),
        },
        ToolSpec {
            name: Cow::Borrowed("browser_snapshot"),
            description: Cow::Borrowed(
                "Return the current page as a ref-tagged element list. Refs from older snapshots become invalid.",
            ),
            parameters: json!({"type": "object", "properties": {}}),
        },
        ToolSpec {
            name: Cow::Borrowed("browser_click"),
            description: Cow::Borrowed(
                "Click the element with the given ref from the latest snapshot.",
            ),
            parameters: json!({
                "type": "object",
                "properties": {"ref": {"type": "string", "description": "e.g. e12"}},
                "required": ["ref"]
            }),
        },
        ToolSpec {
            name: Cow::Borrowed("browser_type"),
            description: Cow::Borrowed(
                "Focus the element with the given ref and type text into it.",
            ),
            parameters: json!({
                "type": "object",
                "properties": {
                    "ref": {"type": "string"},
                    "text": {"type": "string"},
                    "clear": {"type": "boolean", "description": "Replace the current value"},
                    "submit": {"type": "boolean", "description": "Press Enter afterwards"}
                },
                "required": ["ref", "text"]
            }),
        },
        ToolSpec {
            name: Cow::Borrowed("browser_read"),
            description: Cow::Borrowed(
                "Return the visible text of the page, or of one element by ref.",
            ),
            parameters: json!({"type": "object", "properties": {"ref": {"type": "string"}}}),
        },
        ToolSpec {
            name: Cow::Borrowed("delegate_computer"),
            description: Cow::Borrowed(
                "Hand a GUI task to the computer subagent, which sees the screen and operates the mouse and keyboard. Returns its report. Use for desktop applications or pages the browser tools cannot handle.",
            ),
            parameters: json!({
                "type": "object",
                "properties": {"task": {"type": "string", "description": "Self-contained goal and what done looks like"}},
                "required": ["task"]
            }),
        },
        ToolSpec {
            name: Cow::Borrowed("request_take_over"),
            description: Cow::Borrowed(
                "Hand the screen to the user for a step only a human may do (sign-in, 2FA, captcha, payment). Blocks until the user finishes or declines.",
            ),
            parameters: json!({
                "type": "object",
                "properties": {"reason": {"type": "string", "description": "One-line instruction for the user"}},
                "required": ["reason"]
            }),
        },
    ];
    if vision {
        tools.push(ToolSpec {
            name: Cow::Borrowed("screenshot"),
            description: Cow::Borrowed(
                "Look at the desktop: a read-only screenshot of the whole 1280x800 screen.",
            ),
            parameters: json!({"type": "object", "properties": {"max_edge": max_edge()}}),
        });
    }
    if script {
        tools.push(script_tool());
        tools.push(route_tool());
    }
    tools
}

/// Tools served over MCP to external agents; the names are the contract, and `script_run`, `cua_route` and the `computer_act` hint exist only with `script`.
pub fn remote_tools(script: bool) -> Vec<ToolSpec> {
    let mut tools: Vec<ToolSpec> = main_tools(true, false)
        .into_iter()
        .filter(|t| !LOOP_ONLY.contains(&t.name.as_ref()))
        .collect();
    tools.extend(computer_tools());
    if script {
        if let Some(act) = tools.iter_mut().find(|t| t.name == "computer_act") {
            act.description = Cow::Owned(format!("{}{SCRIPT_HINT}", act.description));
        }
        tools.push(script_tool());
        tools.push(route_tool());
    }
    tools
}

/// Tools of the computer subagent.
pub fn computer_tools() -> Vec<ToolSpec> {
    vec![
        ToolSpec {
            name: Cow::Borrowed("computer_state"),
            description: Cow::Borrowed(
                "Screenshot of the whole 1280x800 screen, the visible windows (pid, window_id), and the accessibility elements [index] of the frontmost window or of the given pid.",
            ),
            parameters: json!({
                "type": "object",
                "properties": {
                    "pid": {"type": "integer"},
                    "window_id": {"type": "integer"},
                    "query": {"type": "string", "description": "Only elements whose text contains this"},
                    "max_edge": max_edge()
                }
            }),
        },
        ToolSpec {
            name: Cow::Borrowed("computer_act"),
            description: Cow::Borrowed(
                "Perform one input action, then return a fresh screenshot. Target an element by index from the latest computer_state, or screen coordinates x,y.",
            ),
            parameters: json!({
                "type": "object",
                "properties": {
                    "action": {"type": "string", "enum": ["click", "double_click", "right_click", "type", "key", "hotkey", "scroll", "drag", "launch_app", "wait"]},
                    "element": {"type": "integer", "description": "Element index from the latest computer_state"},
                    "x": {"type": "number"},
                    "y": {"type": "number"},
                    "to_x": {"type": "number", "description": "drag end"},
                    "to_y": {"type": "number", "description": "drag end"},
                    "text": {"type": "string", "description": "type: text to enter"},
                    "key": {"type": "string", "description": "key: e.g. return, tab, escape, a"},
                    "keys": {"type": "array", "items": {"type": "string"}, "description": "hotkey: e.g. [\"ctrl\", \"s\"]"},
                    "direction": {"type": "string", "enum": ["up", "down", "left", "right"]},
                    "amount": {"type": "integer"},
                    "button": {"type": "string", "enum": ["left", "right", "middle"]},
                    "app": {"type": "string", "description": "launch_app: application name, e.g. mousepad"},
                    "seconds": {"type": "number", "description": "wait: up to 10"},
                    "foreground": {"type": "boolean", "description": "Raise the target window and inject there; use when a background action had no visible effect"},
                    "max_edge": max_edge()
                },
                "required": ["action"]
            }),
        },
    ]
}

fn script_tool() -> ToolSpec {
    ToolSpec {
        name: Cow::Borrowed("script_run"),
        description: Cow::Borrowed(
            "The preferred tool for any multi-step desktop task on this box (open an app, navigate, type, fill, save, repeat): write the whole flow as one script instead of many computer_act calls. One round trip, replayable with new params, and ground/verify are built in. Read /opt/script/AGENTS.md first (also the resource cocoonbox://script/AGENTS.md); it has a working save-a-file flow to adapt. A script is JavaScript or TypeScript importing @vendor/script-js (synchronous calls: apps, windows, accessibility tree, keyboard, mouse, screenshots, grounding). Pass code, or script: a file in the box. A module that exports main(params) is called with params and its return value comes back as result. Nothing persists between runs. Returns JSON {result, logs, error?: {kind, message}, duration_ms, exit_code, stderr, snapshot?, screenshot_path} and a screenshot taken after the run.",
        ),
        parameters: json!({
            "type": "object",
            "properties": {
                "code": {"type": "string", "description": "The script as an ES module"},
                "script": {"type": "string", "description": "Path of a script file in the box; relative paths resolve under /workspace"},
                "params": {"type": "object", "description": "Passed to the script's exported main(params)"},
                "timeout_s": {"type": "integer", "description": "Default 90, max 110; the run is stopped after it"},
                "screenshot": {"type": "boolean", "description": "Return a screenshot taken after the run, default true"},
                "max_edge": max_edge()
            }
        }),
    }
}

fn route_tool() -> ToolSpec {
    ToolSpec {
        name: Cow::Borrowed("cua_route"),
        description: Cow::Borrowed(
            "Ask the box's fast router (about 0.2 s) which tool tier fits a task: command, cua, browser, script or jev. Call it first when unsure which tier to use. Returns JSON {tier, probabilities, confidence, uncertain, meaning, tiers}; when uncertain is true, decide yourself.",
        ),
        parameters: json!({
            "type": "object",
            "properties": {"task": {"type": "string", "description": "The user's request, as written"}},
            "required": ["task"]
        }),
    }
}

fn max_edge() -> serde_json::Value {
    json!({"type": "integer", "description": "Scale the screenshot so its long edge is at most this many pixels; x,y then refer to the scaled image"})
}
