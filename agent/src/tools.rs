//! Tool schemas offered to the main agent and the computer subagent.

use serde_json::json;

use crate::llm::ToolSpec;

/// Tools of the main agent; a vision model also gets a read-only screenshot.
pub fn main_tools(vision: bool) -> Vec<ToolSpec> {
    let mut tools = vec![
        ToolSpec {
            name: "shell",
            description: "Run a bash command in the agent's persistent shell session (cwd /workspace). Returns merged stdout/stderr and the exit status. Output over ~30 KB keeps only its head and tail.",
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
            name: "read_file",
            description: "Read a text file with line numbers. Relative paths resolve under /workspace.",
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
            name: "write_file",
            description: "Create or replace a file with the given content; parent directories are created.",
            parameters: json!({
                "type": "object",
                "properties": {"path": {"type": "string"}, "content": {"type": "string"}},
                "required": ["path", "content"]
            }),
        },
        ToolSpec {
            name: "edit_file",
            description: "Replace an exact string in a file. old_string must match exactly once unless replace_all is true.",
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
            name: "list_dir",
            description: "List a directory (default /workspace).",
            parameters: json!({"type": "object", "properties": {"path": {"type": "string"}}}),
        },
        ToolSpec {
            name: "browser_navigate",
            description: "Open a URL in the box's Chrome and return the page snapshot: headings, text and interactive elements tagged [ref=eN].",
            parameters: json!({
                "type": "object",
                "properties": {"url": {"type": "string"}},
                "required": ["url"]
            }),
        },
        ToolSpec {
            name: "browser_snapshot",
            description: "Return the current page as a ref-tagged element list. Refs from older snapshots become invalid.",
            parameters: json!({"type": "object", "properties": {}}),
        },
        ToolSpec {
            name: "browser_click",
            description: "Click the element with the given ref from the latest snapshot.",
            parameters: json!({
                "type": "object",
                "properties": {"ref": {"type": "string", "description": "e.g. e12"}},
                "required": ["ref"]
            }),
        },
        ToolSpec {
            name: "browser_type",
            description: "Focus the element with the given ref and type text into it.",
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
            name: "browser_read",
            description: "Return the visible text of the page, or of one element by ref.",
            parameters: json!({"type": "object", "properties": {"ref": {"type": "string"}}}),
        },
        ToolSpec {
            name: "delegate_computer",
            description: "Hand a GUI task to the computer subagent, which sees the screen and operates the mouse and keyboard. Returns its report. Use for desktop applications or pages the browser tools cannot handle.",
            parameters: json!({
                "type": "object",
                "properties": {"task": {"type": "string", "description": "Self-contained goal and what done looks like"}},
                "required": ["task"]
            }),
        },
        ToolSpec {
            name: "request_take_over",
            description: "Hand the screen to the user for a step only a human may do (sign-in, 2FA, captcha, payment). Blocks until the user finishes or declines.",
            parameters: json!({
                "type": "object",
                "properties": {"reason": {"type": "string", "description": "One-line instruction for the user"}},
                "required": ["reason"]
            }),
        },
    ];
    if vision {
        tools.push(ToolSpec {
            name: "screenshot",
            description: "Look at the desktop: a read-only screenshot of the whole 1280x800 screen.",
            parameters: json!({"type": "object", "properties": {}}),
        });
    }
    tools
}

/// Tools of the computer subagent.
pub fn computer_tools() -> Vec<ToolSpec> {
    vec![
        ToolSpec {
            name: "computer_state",
            description: "Screenshot of the whole 1280x800 screen, the visible windows (pid, window_id), and the accessibility elements [index] of the frontmost window or of the given pid.",
            parameters: json!({
                "type": "object",
                "properties": {
                    "pid": {"type": "integer"},
                    "window_id": {"type": "integer"},
                    "query": {"type": "string", "description": "Only elements whose text contains this"}
                }
            }),
        },
        ToolSpec {
            name: "computer_act",
            description: "Perform one input action, then return a fresh screenshot. Target an element by index from the latest computer_state, or screen coordinates x,y.",
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
                    "foreground": {"type": "boolean", "description": "Raise the target window and inject there; use when a background action had no visible effect"}
                },
                "required": ["action"]
            }),
        },
    ]
}
