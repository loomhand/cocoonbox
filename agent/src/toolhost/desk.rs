//! Desk tools: accessibility snapshots of desktop windows and actions on their elements.

use std::fmt::Write as _;
use std::time::Instant;

use serde::Deserialize;
use serde_json::{Map, Value, json};

use super::atspi::{Atspi, Extras, Frame, Node, Obj};
use super::rpc::{Call, Client};

const FRAME_ROLES: [u32; 5] = [2, 16, 19, 23, 69];
const DEFAULT_SOCKET: &str = "/run/cocoonbox/toolhost.sock";
const CLI_USAGE: &str = "usage: desk VERB [key=value ...|JSON]; verbs: windows, snapshot, read";
const NAME_CHARS: usize = 80;
const READ_CHARS: i32 = 64 << 10;

/// Snapshots and element actions over AT-SPI; the bus connection opens on first use.
#[derive(Default)]
pub struct Desk {
    atspi: Atspi,
}

impl Desk {
    /// `desk_windows`: the accessible top-level windows, focused first.
    pub async fn windows(&self) -> Result<String, String> {
        let mut frames = self.frames().await?;
        frames.sort_by_key(|f| !f.active());
        let list: Vec<Value> = frames.iter().map(frame_json).collect();
        Ok(Value::Array(list).to_string())
    }

    /// `desk_snapshot`: the element tree of one window, the focused one unless `window` names another.
    pub async fn snapshot(&self, args: Value) -> Result<String, String> {
        let a: SnapArgs =
            serde_json::from_value(args).map_err(|e| format!("invalid arguments: {e}"))?;
        let started = Instant::now();
        let frames = self.frames().await?;
        let found = started.elapsed().as_micros();
        let frame = pick(frames, a.window.as_deref(), a.pid)?;
        let extras = Extras {
            values: true,
            bounds: a.bounds.unwrap_or(true),
            keys: a.keys,
        };
        let tree = self.atspi.tree(frame, extras).await?;
        let mut phases = Map::new();
        phases.insert("frames".into(), ms(found));
        for (name, us) in &tree.phases {
            phases.insert((*name).into(), ms(*us));
        }
        let mut out = json!({
            "window": frame_json(&tree.frame),
            "ms": ms(started.elapsed().as_micros()),
            "phases": phases,
            "truncated": tree.truncated,
        });
        if a.text {
            out["text"] = outline(&tree.nodes, a.max_chars.unwrap_or(NAME_CHARS)).into();
        } else {
            out["nodes"] = tree.nodes.iter().enumerate().map(node_json).collect();
        }
        Ok(out.to_string())
    }

    /// `desk_read`: one element read live, with its full text when `text` is set.
    pub async fn read(&self, args: Value) -> Result<String, String> {
        let a: ReadArgs =
            serde_json::from_value(args).map_err(|e| format!("invalid arguments: {e}"))?;
        let obj = Obj::parse(&a.id)?;
        let extras = Extras {
            values: true,
            bounds: true,
            keys: true,
        };
        let chars = if a.text {
            READ_CHARS
        } else {
            NAME_CHARS as i32
        };
        let node = self.atspi.node(&obj, extras, chars).await?;
        Ok(node_json((0, &node)).to_string())
    }

    async fn frames(&self) -> Result<Vec<Frame>, String> {
        match self.atspi.frames().await {
            Ok(frames) => Ok(frames),
            Err(e) => {
                self.atspi.reset().await;
                Err(e)
            }
        }
    }
}

/// `cocoonbox-agent desk VERB [key=value ...|JSON]`: calls the toolhost's `desk_VERB` and prints its result.
pub async fn cli(args: Vec<String>) -> u8 {
    let Some((verb, rest)) = args.split_first() else {
        eprintln!("{CLI_USAGE}");
        return 2;
    };
    let args = match cli_args(rest) {
        Ok(a) => a,
        Err(e) => {
            eprintln!("desk: {e}\n{CLI_USAGE}");
            return 2;
        }
    };
    let socket = std::env::var("TOOLHOST_SOCKET")
        .ok()
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| DEFAULT_SOCKET.to_owned());
    let call = Call {
        agent_id: "desk".to_owned(),
        tool: format!("desk_{verb}"),
        args,
    };
    match Client::new(socket.into()).call(&call).await {
        Ok(out) if !out.is_error => {
            println!("{}", out.text);
            0
        }
        Ok(out) => {
            eprintln!("desk {verb}: {}", out.text);
            1
        }
        Err(e) => {
            eprintln!("desk {verb}: {e}");
            1
        }
    }
}

/// One JSON object, or `key=value` pairs whose values parse as JSON when they can.
fn cli_args(rest: &[String]) -> Result<Value, String> {
    if let [one] = rest
        && one.trim_start().starts_with('{')
    {
        return serde_json::from_str(one).map_err(|e| format!("arguments: {e}"));
    }
    let mut args = Map::new();
    for pair in rest {
        let (k, v) = pair
            .split_once('=')
            .ok_or_else(|| format!("{pair:?} is not key=value"))?;
        let v = serde_json::from_str(v).unwrap_or_else(|_| Value::String(v.to_owned()));
        args.insert(k.to_owned(), v);
    }
    Ok(Value::Object(args))
}

#[derive(Deserialize)]
struct SnapArgs {
    #[serde(default)]
    window: Option<String>,
    #[serde(default)]
    pid: Option<u32>,
    #[serde(default)]
    text: bool,
    #[serde(default)]
    keys: bool,
    #[serde(default)]
    bounds: Option<bool>,
    #[serde(default)]
    max_chars: Option<usize>,
}

#[derive(Deserialize)]
struct ReadArgs {
    id: String,
    #[serde(default)]
    text: bool,
}

/// The window named `want` (exact, then trimmed, then contained, ignoring case), else the focused one; among equals the active one, else the newest.
fn pick(frames: Vec<Frame>, want: Option<&str>, pid: Option<u32>) -> Result<Frame, String> {
    let frames: Vec<Frame> = frames
        .into_iter()
        .filter(|f| FRAME_ROLES.contains(&f.role) && pid.is_none_or(|p| f.pid == Some(p)))
        .collect();
    let best = |c: Vec<&Frame>| {
        c.iter()
            .rev()
            .find(|f| f.active())
            .or_else(|| c.last())
            .map(|f| (*f).clone())
    };
    let Some(want) = want else {
        let active: Vec<&Frame> = frames.iter().filter(|f| f.active()).collect();
        return best(active).ok_or_else(|| "no focused window".to_owned());
    };
    let lower = want.trim().to_lowercase();
    let tests: [&dyn Fn(&Frame) -> bool; 3] = [
        &|f| f.name == want,
        &|f| f.name.trim().to_lowercase() == lower,
        &|f| f.name.to_lowercase().contains(&lower),
    ];
    for test in tests {
        let hits: Vec<&Frame> = frames.iter().filter(|f| test(f)).collect();
        if let Some(f) = best(hits) {
            return Ok(f);
        }
    }
    let names: Vec<&str> = frames.iter().map(|f| f.name.as_str()).collect();
    Err(format!("no window {want:?}; windows: {names:?}"))
}

fn frame_json(f: &Frame) -> Value {
    json!({
        "id": f.obj.id(),
        "title": f.name,
        "role": super::atspi::role_name(f.role, true),
        "pid": f.pid,
        "active": f.active(),
    })
}

fn node_json((i, n): (usize, &Node)) -> Value {
    let mut v = json!({
        "ref": i,
        "id": n.obj.as_ref().map(Obj::id),
        "parent": n.parent.map_or(-1, |p| p as i64),
        "depth": n.depth,
        "role": n.role(),
        "name": n.name,
        "states": n.state_names(),
    });
    if !n.description.is_empty() {
        v["description"] = n.description.clone().into();
    }
    if let Some(value) = &n.value {
        v["value"] = value.clone().into();
    }
    if let Some(keys) = &n.keys {
        v["keys"] = keys.clone().into();
    }
    if let Some(b) = n.bounds {
        v["bounds"] = json!(b);
    }
    v
}

/// One line per node: `- role "name" [disabled] [checked] [keys] [ref=N]: "value"`, indented by depth.
pub fn outline(nodes: &[Node], max_chars: usize) -> String {
    let mut out = String::new();
    for (i, n) in nodes.iter().enumerate() {
        let _ = write!(out, "{}- {}", "  ".repeat(n.depth), n.role());
        if !n.name.is_empty() {
            let _ = write!(out, " \"{}\"", quote(&n.name, max_chars));
        }
        if n.disabled() {
            out.push_str(" [disabled]");
        }
        if n.checked() {
            out.push_str(" [checked]");
        }
        if let Some(keys) = &n.keys {
            let _ = write!(out, " [{keys}]");
        }
        let _ = write!(out, " [ref={i}]");
        if let Some(value) = &n.value {
            let _ = write!(out, ": \"{}\"", quote(value, max_chars));
        }
        out.push('\n');
    }
    out
}

fn quote(s: &str, max: usize) -> String {
    let cut: String = s.chars().take(max).collect();
    cut.replace('\\', "\\\\")
        .replace('"', "\\\"")
        .replace('\n', "\\n")
}

fn ms(us: u128) -> Value {
    json!((us as f64 / 100.0).round() / 10.0)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn frame(name: &str, role: u32, active: bool, pid: u32) -> Frame {
        Frame {
            obj: Obj {
                bus: ":1.1".into(),
                path: format!("/org/a11y/atspi/accessible/{name}"),
            },
            role,
            name: name.into(),
            states: if active { 1 << 1 } else { 0 },
            pid: Some(pid),
        }
    }

    #[test]
    fn a_window_is_picked_by_exact_then_trimmed_then_contained_title() {
        let frames = vec![
            frame("Untitled 1 - Mousepad", 23, false, 7),
            frame("Mousepad Preferences", 16, true, 7),
            frame("Settings", 16, false, 9),
            frame("xfce4-panel", 35, true, 3),
        ];
        assert_eq!(
            pick(frames.clone(), None, None).unwrap().name,
            "Mousepad Preferences"
        );
        assert_eq!(
            pick(frames.clone(), Some("settings"), None).unwrap().name,
            "Settings"
        );
        assert_eq!(
            pick(frames.clone(), Some("Mousepad"), None).unwrap().name,
            "Mousepad Preferences"
        );
        assert_eq!(
            pick(frames.clone(), Some("Mousepad"), Some(7))
                .unwrap()
                .name,
            "Mousepad Preferences"
        );
        assert!(pick(frames.clone(), Some("Mousepad"), Some(9)).is_err());
        assert!(pick(frames, Some("xfce4-panel"), None).is_err());
    }

    #[test]
    fn cli_arguments_are_json_or_pairs_with_json_values() {
        let pairs = cli_args(&[
            "window=Mousepad Preferences".into(),
            "text=true".into(),
            "pid=7".into(),
        ])
        .unwrap();
        assert_eq!(
            pairs,
            json!({"window": "Mousepad Preferences", "text": true, "pid": 7})
        );
        assert_eq!(
            cli_args(&[r#"{"id": "1.2/3"}"#.into()]).unwrap(),
            json!({"id": "1.2/3"})
        );
        assert!(cli_args(&["nope".into()]).is_err());
    }

    #[test]
    fn the_outline_shows_states_keys_and_values() {
        let nodes = vec![
            Node {
                role: 16,
                name: "Prefs".into(),
                states: 1 << 8,
                ..Node::default()
            },
            Node {
                role: 7,
                name: "Wrap \"long\" lines".into(),
                states: (1 << 8) | (1 << 4),
                depth: 1,
                parent: Some(0),
                ..Node::default()
            },
            Node {
                role: 52,
                states: 0,
                depth: 1,
                parent: Some(0),
                value: Some("80".into()),
                keys: Some("<Alt>n".into()),
                ..Node::default()
            },
        ];
        assert_eq!(
            outline(&nodes, 80),
            "- dialog \"Prefs\" [ref=0]\n  - checkbox \"Wrap \\\"long\\\" lines\" [checked] [ref=1]\n  - spinbutton [disabled] [<Alt>n] [ref=2]: \"80\"\n"
        );
    }
}
