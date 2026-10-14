//! Tool schemas offered to the main agent and the computer subagent.

use std::borrow::Cow;

use serde_json::json;

use crate::config::Automation;
use crate::llm::ToolSpec;

const LOOP_ONLY: [&str; 2] = ["delegate_computer", "request_take_over"];
const PLAN_HINT: &str = "On this box, desktop-app GUI work (menus, dialogs, settings, Save As) goes to one gui_plan call with the whole plan, and loops over many items to one script_run script, instead of a chain of computer_act calls. ";
const DESK_OPS: [&str; 15] = [
    "windows", "snapshot", "find", "read", "press", "act", "at", "key", "type", "click", "focus",
    "close", "launch", "shot", "screen",
];
const TAB_TOOLS: [&str; 5] = [
    "browser_navigate",
    "browser_snapshot",
    "browser_click",
    "browser_type",
    "browser_read",
];
const TAB_HINT: &str = "A tab the user already has open, instead of your own window: \"active\" for the tab in front, or an id from browser_tabs. Leave it out to work in your own window.";
const NAVIGATE_TAB: &str =
    " It works in your own Chrome window; to act on a page the user already has open, pass tab.";
const FRONT_HINT: &str = "true brings the page to the front of the desktop with the keyboard focus, for the desktop tools or the user to see it, and takes the focus from whatever the user types in. Leave it out to keep the page in the background.";
const SHOT_HINT: &str = "Look at the desktop: a read-only screenshot of the whole 1280x800 screen, scaled to 1024x640 unless max_edge says otherwise; the result gives its size and input tokens. To look again, prefer a desk snapshot (text, no image) or a region at full resolution to another full screenshot.";
const EDGE_HINT: &str = "Scale the screenshot so its long edge is at most this many pixels, default 1024, 0 for full size; x,y then refer to the scaled image";
const SCALED_SHOT: &str = "Look at the desktop: a read-only screenshot of the whole screen, scaled to a long edge of 1024 pixels unless max_edge says otherwise.";
const SCREEN_POINT: &str = "or screen coordinates x,y.";
const SHOT_POINT: &str = "or x,y in the pixels of your latest screenshot.";
const SCALED_STATE: &str = "Screenshot of the whole screen, scaled to a long edge of 1024 pixels unless max_edge says otherwise, the visible windows (pid, window_id), and the accessibility elements [index] of the frontmost window or of the given pid.";
const REGION_HINT: &str = "Only this part of the screen, at full resolution: x, y, w, h in the pixels of your last screenshot";
const SHELL_HINT: &str = " Keep each command's wait well under the time left; save the required output early and improve it after.";
const LIST_HINT: &str = " For a few steps you can already place, such as opening a menu and picking its item, pass actions in place of action: a list of such actions, run in order with one screenshot at the end; it stops at the first failure. Each action already waits for a still screen, so do not add wait.";

/// Remote tools that only read the box: all a read-only share offers.
pub const READ_ONLY: [&str; 6] = [
    "screenshot",
    "computer_state",
    "read_file",
    "list_dir",
    "browser_snapshot",
    "browser_read",
];

/// Tools of the main agent; a vision model also gets a read-only screenshot, a box that runs GUI plans
/// `gui_plan`, its script tool and `browser_fetch`.
pub fn main_tools(vision: bool, automation: Automation) -> Vec<ToolSpec> {
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
    if let Some(script) = script_tool(automation) {
        with_tab(&mut tools);
        tools.extend([gui_plan_tool(), script, fetch_tool(), tabs_tool()]);
        with_scale(&mut tools);
        with_budget(&mut tools);
    }
    tools
}

/// Tools served over MCP to external agents; the names are the contract, and `gui_plan`, the script tool,
/// `browser_fetch`, `desk` and the `computer_act` hint exist only on a box that runs GUI plans.
pub fn remote_tools(automation: Automation) -> Vec<ToolSpec> {
    let mut tools: Vec<ToolSpec> = main_tools(true, Automation::Plain)
        .into_iter()
        .filter(|t| !LOOP_ONLY.contains(&t.name.as_ref()))
        .collect();
    tools.extend(computer_tools(Automation::Plain));
    if let Some(script) = script_tool(automation) {
        if let Some(act) = tools.iter_mut().find(|t| t.name == "computer_act") {
            act.description = Cow::Owned(format!("{PLAN_HINT}{}{LIST_HINT}", act.description));
            if let Some(actions) = act.parameters["properties"]["action"]["enum"].as_array_mut() {
                actions.insert(3, "triple_click".into());
            }
            let mut step = act.parameters.clone();
            if let Some(props) = step["properties"].as_object_mut() {
                props.remove("max_edge");
            }
            act.parameters["properties"]["actions"] = json!({"type": "array", "items": step});
            if let Some(schema) = act.parameters.as_object_mut() {
                schema.remove("required");
            }
        }
        with_tab(&mut tools);
        tools.extend([
            script,
            gui_plan_tool(),
            fetch_tool(),
            tabs_tool(),
            desk_tool(),
        ]);
        with_scale(&mut tools);
        with_region(&mut tools);
        with_budget(&mut tools);
    }
    tools
}

/// Gives the screenshots of a box that runs GUI plans their default scale in place of the screen size.
fn with_scale(tools: &mut [ToolSpec]) {
    for t in tools.iter_mut() {
        if t.parameters["properties"].get("max_edge").is_some() {
            t.parameters["properties"]["max_edge"] =
                json!({"type": "integer", "description": EDGE_HINT});
        }
        match t.name.as_ref() {
            "screenshot" => t.description = Cow::Borrowed(SCALED_SHOT),
            "computer_state" => t.description = Cow::Borrowed(SCALED_STATE),
            "computer_act" => {
                t.description = Cow::Owned(t.description.replace(SCREEN_POINT, SHOT_POINT));
            }
            _ => {}
        }
    }
}

/// Gives the `screenshot` of a box that runs GUI plans its region.
fn with_region(tools: &mut [ToolSpec]) {
    for t in tools.iter_mut() {
        if t.name == "screenshot" {
            t.description = Cow::Borrowed(SHOT_HINT);
            t.parameters["properties"]["region"] = json!({
                "type": "object",
                "description": REGION_HINT,
                "properties": {"x": {"type": "number"}, "y": {"type": "number"}, "w": {"type": "number"}, "h": {"type": "number"}},
                "required": ["x", "y", "w", "h"]
            });
        }
    }
}

/// Gives the `shell` of a box that runs GUI plans its time budget.
fn with_budget(tools: &mut [ToolSpec]) {
    for t in tools.iter_mut().filter(|t| t.name == "shell") {
        t.description = Cow::Owned(format!("{}{SHELL_HINT}", t.description));
    }
}

/// Gives the page tools of a box that runs GUI plans their `tab` argument, and browser_navigate its `front`.
fn with_tab(tools: &mut [ToolSpec]) {
    for t in tools
        .iter_mut()
        .filter(|t| TAB_TOOLS.contains(&t.name.as_ref()))
    {
        t.parameters["properties"]["tab"] = json!({"type": "string", "description": TAB_HINT});
        if t.name == "browser_navigate" {
            t.description = Cow::Owned(format!("{}{NAVIGATE_TAB}", t.description));
            t.parameters["properties"]["front"] =
                json!({"type": "boolean", "description": FRONT_HINT});
        }
    }
}

/// The automation's script tool; none on a box without one.
fn script_tool(automation: Automation) -> Option<ToolSpec> {
    match automation {
        Automation::Plain => None,
        Automation::Desk => Some(desk_script_tool()),
    }
}

/// Tools of the computer subagent; on a box that runs GUI plans their screenshots name the default scale.
pub fn computer_tools(automation: Automation) -> Vec<ToolSpec> {
    let mut tools = vec![
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
                    "action": {"type": "string", "enum": ["click", "double_click", "right_click", "type", "type_from_file", "key", "hotkey", "scroll", "drag", "launch_app", "wait"]},
                    "element": {"type": "integer", "description": "Element index from the latest computer_state"},
                    "x": {"type": "number"},
                    "y": {"type": "number"},
                    "to_x": {"type": "number", "description": "drag end"},
                    "to_y": {"type": "number", "description": "drag end"},
                    "text": {"type": "string", "description": "type: text to enter"},
                    "path": {"type": "string", "description": "type_from_file: a link-cli --output-file card JSON under /tmp or $HOME"},
                    "field": {"type": "string", "description": "type_from_file: number, exp_month, exp_year, exp (MM/YY), cvc or billing_address.<key>; types that card field and never returns it"},
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
    ];
    if automation.plans() {
        with_scale(&mut tools);
    }
    tools
}

fn desk_script_tool() -> ToolSpec {
    ToolSpec {
        name: Cow::Borrowed("script_run"),
        description: Cow::Borrowed(
            "Run one script for desktop work a single gui_plan cannot hold: loops over many items, data between apps, logic. A script is a JavaScript or TypeScript ES module run by Node 24 that drives the desktop through the desk API (import * as desk from '/opt/desk/lib/desk.mts': windows, accessibility snapshots with checked states, find and press elements, keys, typing, clicks, screen crops, gw model calls); the desk command does the same from shell. Read /opt/desk/AGENTS.md first (also the resource cocoonbox://desk/AGENTS.md). Pass code, or script: a file in the box. A module that exports main(params) is called with params and its return value comes back as result. Nothing persists between runs. Returns JSON {result, logs, error?: {kind, message}, duration_ms, exit_code, stderr, snapshot?, screenshot_path} and a screenshot taken after the run.",
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

fn desk_tool() -> ToolSpec {
    ToolSpec {
        name: Cow::Borrowed("desk"),
        description: Cow::Borrowed(
            "One desktop call in plain JSON on the accessibility tree: op windows; snapshot {window?, text?}; find {name, role?, window?}; read {id, text?}; press {id} (selects, opens menus, clicks or runs the element's action); act {id, action: click|action|select|focus}; at {x, y, window?}; key {keys: \"ctrl+shift+s\"}; type {text}; click {x, y, button?, count?}; focus {window}; close {window}; launch {app}; shot {x?, y?, w?, h?}; screen. Element ids come from snapshot or find; window is title text, default the focused window. x, y, w, h and bounds are screen pixels, unlike computer_act's screenshot pixels. For GUI work of several steps use gui_plan.",
        ),
        parameters: json!({
            "type": "object",
            "properties": {
                "op": {"type": "string", "enum": DESK_OPS},
                "window": {"type": "string"},
                "id": {"type": "string"},
                "name": {"type": "string"},
                "role": {"type": "string"},
                "action": {"type": "string"},
                "keys": {"type": "string"},
                "text": {"description": "type: the text; snapshot and read: true for text"},
                "x": {"type": "number"},
                "y": {"type": "number"},
                "w": {"type": "number"},
                "h": {"type": "number"},
                "button": {"type": "string"},
                "count": {"type": "integer"},
                "app": {"type": "string"}
            },
            "required": ["op"]
        }),
    }
}

fn gui_plan_tool() -> ToolSpec {
    ToolSpec {
        name: Cow::Borrowed("gui_plan"),
        description: Cow::Borrowed(
            "Run a desktop-app GUI plan in ONE call: the box runs each step on the accessibility tree (0.1-0.5 s), checks it, and stops at the first surprise. Use it for GUI work in desktop apps (menus, toggles, dialogs, Preferences, Save As, settings). Write the whole plan at once, without looking first.\n\
One op per step; optional window: title text of the window the step acts in (waited for):\n\
{app: \"mousepad\"} focus or launch; {menu: \"Edit > Preferences\"} (an approximate path is fine); {menu: \"Document > Word Wrap\", on: true} set a menu toggle (read first, never toggled twice); {click: \"Save\"}; {check: \"Show line numbers\", on: false} a check box or switch; {set: \"font size\", value: \"14\"} a text, spin or combo field; {type: \"text\"}; {key: \"ctrl+s\"}; {wait: \"Save As\"} a window title; {verify: \"Name\", value: \"x\"} (or on, enabled); {close: true} close the focused dialog.\n\
Returns {ok: true, trace} or {ok: false, escalate: {step, reason, window, outline}}. Trust ok: true: each step was verified, so add no checks or screenshots. On escalate, handle the reason, then call again with only the remaining steps. Close the dialogs you open. A path for a file dialog may end in \\n to submit it. Name targets by their visible label.\n\
A passing plan that starts with an app step is saved as a flow: name it with save: \"app-action\" and write values a later request changes as {{name}} with params: {name: value}. To save a plan that already ran, or the steps that passed before an escalation, call {save: \"app-action\"} alone: it does not run them again. Replay a flow with {flow: \"name\", params: {...}}: one call, no model. Check saved flows with {flows: true} before planning a task you may have done before. screenshot: true adds a screenshot (default: only on escalate).",
        ),
        parameters: json!({
            "type": "object",
            "properties": {
                "steps": {"type": "array", "items": {"type": "object"}, "description": "The plan, one op per step"},
                "flow": {"type": "string", "description": "Replay this saved flow instead of steps"},
                "params": {"type": "object", "description": "Values for the {{name}} slots"},
                "save": {"type": "string", "description": "Flow name for this plan, like mousepad-font-size; without steps, saves the last plan without running it again"},
                "start": {"type": "integer", "description": "First step to run, default 0"},
                "find": {"type": "string", "description": "A request: returns the saved flow that does it"},
                "flows": {"type": "boolean", "description": "Return the saved flows, one line each, instead of running"},
                "screenshot": {"type": "boolean"},
                "timeout_s": {"type": "integer", "description": "Default 90, max 110"}
            }
        }),
    }
}

fn fetch_tool() -> ToolSpec {
    ToolSpec {
        name: Cow::Borrowed("browser_fetch"),
        description: Cow::Borrowed(
            "Read a web page without showing it: the box's Chrome loads the URL in a hidden tab with the user's cookies and sign-ins, waits for the page to load and its network to settle (at most 15 s, then returns what has loaded), and returns the page title, final URL and visible text, up to 20000 characters, then closes the tab. The user's windows and tabs are untouched. Prefer it to browser_navigate plus browser_read for looking something up. For reading only: to sign in, click, fill a form, or show the user a page, use browser_navigate.",
        ),
        parameters: json!({
            "type": "object",
            "properties": {"url": {"type": "string"}},
            "required": ["url"]
        }),
    }
}

fn tabs_tool() -> ToolSpec {
    ToolSpec {
        name: Cow::Borrowed("browser_tabs"),
        description: Cow::Borrowed(
            "List the tabs the user has open in the box's Chrome, outside your own window: id, title, URL and which ones are in front. To act on one, such as a page a task refers to that is already open, pass its id, or \"active\" for the tab in front, as tab to browser_navigate, browser_snapshot, browser_click, browser_type or browser_read. Without tab those tools work in your own window and leave the user's tabs alone.",
        ),
        parameters: json!({"type": "object", "properties": {}}),
    }
}

fn max_edge() -> serde_json::Value {
    json!({"type": "integer", "description": "Scale the screenshot so its long edge is at most this many pixels; x,y then refer to the scaled image"})
}
