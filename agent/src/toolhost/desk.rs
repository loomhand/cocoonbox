//! Desk tools: accessibility snapshots of desktop windows, actions on their elements, and desktop
//! input, window and launch calls through cua-driver.

use std::fmt::Write as _;
use std::time::{Duration, Instant};

use base64::Engine;
use base64::engine::general_purpose::STANDARD as B64;
use futures_util::future::join_all;
use serde::Deserialize;
use serde_json::{Map, Value, json};
use x11rb::connection::Connection as _;
use x11rb::protocol::xproto::{AtomEnum, ClientMessageEvent, ConnectionExt as _, EventMask};

use super::atspi::{Atspi, Extras, Frame, Node, Obj, SHOWING, key_combos};
use super::cua::Cua;
use super::files;
use super::rpc::{Call, Client, Output};

const FRAME_ROLES: [u32; 5] = [2, 16, 19, 23, WINDOW_ROLE];
const WINDOW_ROLE: u32 = 69;
const DEFAULT_SOCKET: &str = "/run/cocoonbox/toolhost.sock";
const CLI_USAGE: &str = "usage: desk VERB [key=value ...|JSON]; verbs: windows, snapshot, find, read, press, act, at, key, type, click, focus, close, launch, shot, screen";
const NAME_CHARS: usize = 80;
const READ_CHARS: i32 = 64 << 10;
const MAX_FOUND: usize = 20;
const MENU_POLL: Duration = Duration::from_millis(30);
const MENU_POLLS: u32 = 10;
const SELECTED_ROLES: [&str; 5] = ["tab", "option", "radio", "listitem", "treeitem"];

/// Snapshots and element actions over AT-SPI; the bus connection opens on first use.
#[derive(Default)]
pub struct Desk {
    atspi: Atspi,
}

impl Desk {
    /// Runs desk tool `tool`; input, window and launch calls go through `cua`.
    pub async fn run(&self, cua: &Cua, tool: &str, args: Value) -> Result<Output, String> {
        let text = match tool {
            "desk_windows" => self.windows().await?,
            "desk_snapshot" => self.snapshot(parse(args)?).await?,
            "desk_find" => self.find(parse(args)?).await?,
            "desk_read" => self.read(parse(args)?).await?,
            "desk_press" => self.press(cua, parse::<IdArgs>(args)?.id).await?,
            "desk_act" => self.act(cua, parse(args)?).await?,
            "desk_at" => self.at(parse(args)?).await?,
            "desk_key" => key(cua, &parse::<KeyArgs>(args)?.keys).await?,
            "desk_type" => type_text(cua, &parse::<TypeArgs>(args)?.text).await?,
            "desk_click" => click(cua, parse(args)?).await?,
            "desk_focus" => focus(cua, parse(args)?).await?,
            "desk_close" => close(parse(args)?)?,
            "desk_launch" => {
                let app = parse::<LaunchArgs>(args)?.app;
                let v = cua.desk_call("launch_app", json!({"name": app})).await?;
                json!({"ok": true, "launched": text_of(&v)}).to_string()
            }
            "desk_shot" => return shot(cua, parse(args)?).await,
            "desk_screen" => {
                let v = cua.desk_call("get_screen_size", json!({})).await?;
                v["structuredContent"].to_string()
            }
            other => return Err(format!("unknown desk tool {other}")),
        };
        Ok(Output::text(text))
    }

    /// The accessible top-level windows, focused first.
    async fn windows(&self) -> Result<String, String> {
        let mut frames = self.frames().await?;
        frames.sort_by_key(|f| !f.active());
        let list: Vec<Value> = frames.iter().map(frame_json).collect();
        Ok(Value::Array(list).to_string())
    }

    /// The element tree of one window, the focused one unless `window` names another.
    async fn snapshot(&self, a: SnapArgs) -> Result<String, String> {
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

    /// The elements of one window whose name is `name`, else those that contain it, ignoring case and punctuation.
    async fn find(&self, a: FindArgs) -> Result<String, String> {
        let frame = pick(self.frames().await?, a.window.as_deref(), a.pid)?;
        let extras = Extras {
            values: true,
            bounds: true,
            keys: false,
        };
        let tree = self.atspi.tree(frame, extras).await?;
        let want = norm(&a.name);
        let pool: Vec<(usize, &Node)> = tree
            .nodes
            .iter()
            .enumerate()
            .filter(|(_, n)| {
                a.role
                    .as_deref()
                    .is_none_or(|r| r.split('|').any(|r| r == n.role()))
            })
            .collect();
        let exact: Vec<&(usize, &Node)> =
            pool.iter().filter(|(_, n)| norm(&n.name) == want).collect();
        let hits = if exact.is_empty() {
            pool.iter()
                .filter(|(_, n)| !want.is_empty() && norm(&n.name).contains(&want))
                .collect()
        } else {
            exact
        };
        let found: Vec<Value> = hits
            .iter()
            .take(MAX_FOUND)
            .map(|(i, n)| node_json((*i, n)))
            .collect();
        Ok(json!({"window": frame_json(&tree.frame), "found": found}).to_string())
    }

    /// One element read live, with its full text when `text` is set.
    async fn read(&self, a: ReadArgs) -> Result<String, String> {
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

    /// Presses an element the way a user would: selects a tab or list row, opens the menus above a hidden
    /// menu entry, clicks the element's centre when the element is what lies there, else runs its action.
    async fn press(&self, cua: &Cua, id: String) -> Result<String, String> {
        let obj = Obj::parse(&id)?;
        let shown = Extras {
            values: false,
            bounds: true,
            keys: false,
        };
        let mut node = self.atspi.node(&obj, shown, 0).await?;
        let role = node.role();
        if SELECTED_ROLES.contains(&role) && self.atspi.select(&obj).await.unwrap_or(false) {
            return Ok(json!({"ok": true, "how": "select"}).to_string());
        }
        let chain = self.atspi.ancestors(&obj).await?;
        if role.starts_with("menu") && !node.has(SHOWING) {
            self.open_menus(&chain).await;
            for _ in 0..MENU_POLLS {
                node = self.atspi.node(&obj, shown, 0).await?;
                if node.has(SHOWING) {
                    break;
                }
                tokio::time::sleep(MENU_POLL).await;
            }
        }
        if let Some((x, y)) = self.reachable(&obj, &node, &chain).await {
            cua.desk_input("click", json!({"x": x, "y": y})).await?;
            return Ok(json!({"ok": true, "how": "click", "x": x, "y": y}).to_string());
        }
        let done = self.atspi.do_action(&obj).await?;
        Ok(json!({"ok": done, "how": "action"}).to_string())
    }

    /// Opens the menus right above a menu entry, outermost first, by their own actions.
    async fn open_menus(&self, chain: &[Obj]) {
        let nodes = join_all(
            chain
                .iter()
                .map(|o| self.atspi.node(o, Extras::default(), 0)),
        )
        .await;
        let start = nodes
            .iter()
            .rposition(|n| !n.as_ref().is_ok_and(|n| n.role() == "menu"))
            .map_or(0, |i| i + 1);
        for menu in &chain[start..] {
            let _ = self.atspi.do_action(menu).await;
        }
    }

    /// The centre of `node` when a click there reaches it: a menu entry that shows, else an element whose
    /// centre lies inside every container above it, where a hit test of its window finds it, a part of it
    /// or a container of it.
    async fn reachable(&self, obj: &Obj, node: &Node, chain: &[Obj]) -> Option<(i32, i32)> {
        let [x, y, w, h] = node.bounds?;
        if !node.has(SHOWING) || w <= 0 || h <= 0 || x < 0 || y < 0 {
            return None;
        }
        let (cx, cy) = (x + w / 2, y + h / 2);
        if node.role().starts_with("menu") {
            return Some((cx, cy));
        }
        let shown = Extras {
            values: false,
            bounds: true,
            keys: false,
        };
        let above = join_all(chain.iter().map(|o| self.atspi.node(o, shown, 0))).await;
        let inside = |n: &Node| {
            n.role() == "tab"
                || n.bounds.is_none_or(|[bx, by, bw, bh]| {
                    (bx..bx + bw).contains(&cx) && (by..by + bh).contains(&cy)
                })
        };
        if !above.iter().flatten().all(inside) {
            return None;
        }
        let hit = self.atspi.at_point(chain.first()?, cx, cy).await.ok()??;
        if &hit == obj
            || chain.contains(&hit)
            || self.atspi.ancestors(&hit).await.ok()?.contains(obj)
        {
            return Some((cx, cy));
        }
        None
    }

    /// One element action: `click` at its centre, `action` (its own default action), `select` or `focus`.
    async fn act(&self, cua: &Cua, a: ActArgs) -> Result<String, String> {
        let obj = Obj::parse(&a.id)?;
        let ok = match a.action.as_str() {
            "action" => self.atspi.do_action(&obj).await?,
            "select" => self.atspi.select(&obj).await?,
            "focus" => self.atspi.grab_focus(&obj).await?,
            "click" => {
                let shown = Extras {
                    values: false,
                    bounds: true,
                    keys: false,
                };
                let n = self.atspi.node(&obj, shown, 0).await?;
                let [x, y, w, h] = n.bounds.ok_or("the element has no screen position")?;
                cua.desk_input("click", json!({"x": x + w / 2, "y": y + h / 2}))
                    .await?;
                true
            }
            other => {
                return Err(format!(
                    "unknown action {other}; use click, action, select or focus"
                ));
            }
        };
        Ok(json!({"ok": ok}).to_string())
    }

    /// The element at the screen point `(x, y)` of a window, with the names and roles above it.
    async fn at(&self, a: AtArgs) -> Result<String, String> {
        let frame = pick(self.frames().await?, a.window.as_deref(), a.pid)?;
        let Some(hit) = self.atspi.at_point(&frame.obj, a.x, a.y).await? else {
            return Ok(json!({"found": null, "window": frame_json(&frame)}).to_string());
        };
        let (node, above) = tokio::join!(
            self.atspi.node(&hit, Extras::default(), 0),
            self.atspi.ancestors(&hit)
        );
        let mut path = Vec::new();
        for o in above? {
            if let Ok(n) = self.atspi.node(&o, Extras::default(), 0).await {
                path.push(json!({"id": o.id(), "role": n.role(), "name": n.name}));
            }
        }
        Ok(json!({"found": node_json((0, &node?)), "path": path}).to_string())
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

/// Presses a key or a combination such as `ctrl+shift+s` on the desktop.
async fn key(cua: &Cua, combo: &str) -> Result<String, String> {
    let keys = key_names(combo)?;
    match keys.as_slice() {
        [one] => cua.desk_input("press_key", json!({"key": one})).await?,
        _ => cua.desk_input("hotkey", json!({"keys": keys})).await?,
    };
    Ok(json!({"ok": true}).to_string())
}

/// Types `text` on the desktop; each newline is a Return key.
async fn type_text(cua: &Cua, text: &str) -> Result<String, String> {
    for (i, line) in text.split('\n').enumerate() {
        if i > 0 {
            cua.desk_input("press_key", json!({"key": "return"}))
                .await?;
        }
        let line = line.trim_end_matches('\r');
        if !line.is_empty() {
            cua.desk_type(line).await?;
        }
    }
    Ok(json!({"ok": true, "chars": text.chars().count()}).to_string())
}

async fn click(cua: &Cua, a: ClickArgs) -> Result<String, String> {
    let mut req = json!({"x": a.x, "y": a.y});
    if let Some(button) = a.button {
        req["button"] = button.into();
    }
    if let Some(count) = a.count {
        req["count"] = count.into();
    }
    cua.desk_input("click", req).await?;
    Ok(json!({"ok": true}).to_string())
}

/// Brings the window titled `window` to the front.
async fn focus(cua: &Cua, a: WindowArgs) -> Result<String, String> {
    let listed = cua.desk_call("list_windows", json!({})).await?;
    let windows = listed["structuredContent"]["windows"]
        .as_array()
        .cloned()
        .unwrap_or_default();
    let titled = |w: &Value| w["title"].as_str().unwrap_or_default().to_owned();
    let pid_ok = |w: &Value| {
        a.pid
            .is_none_or(|p| w["pid"].as_u64() == Some(u64::from(p)))
    };
    let lower = a.window.trim().to_lowercase();
    let w = windows
        .iter()
        .rev()
        .find(|w| pid_ok(w) && titled(w) == a.window)
        .or_else(|| {
            windows
                .iter()
                .rev()
                .find(|w| pid_ok(w) && titled(w).to_lowercase().contains(&lower))
        })
        .ok_or_else(|| format!("no window {:?}", a.window))?;
    cua.desk_call(
        "bring_to_front",
        json!({"pid": w["pid"], "window_id": w["window_id"]}),
    )
    .await?;
    Ok(json!({"ok": true, "title": titled(w), "pid": w["pid"]}).to_string())
}

/// Asks the window manager to close the window titled `window`, as its close button does.
fn close(a: WindowArgs) -> Result<String, String> {
    let (conn, n) = x11rb::connect(None).map_err(|e| format!("connect X: {e}"))?;
    let root = conn.setup().roots[n].root;
    let atom = |name: &[u8]| -> Result<u32, String> {
        conn.intern_atom(false, name)
            .map_err(|e| format!("intern atom: {e}"))?
            .reply()
            .map(|r| r.atom)
            .map_err(|e| format!("intern atom: {e}"))
    };
    let [list, name, utf8, wm_pid, close] = [
        &b"_NET_CLIENT_LIST"[..],
        b"_NET_WM_NAME",
        b"UTF8_STRING",
        b"_NET_WM_PID",
        b"_NET_CLOSE_WINDOW",
    ]
    .map(atom);
    let (list, name, utf8, wm_pid, close) = (list?, name?, utf8?, wm_pid?, close?);
    let prop = |w: u32, p: u32, kind: u32| {
        conn.get_property(false, w, p, kind, 0, 1024)
            .ok()
            .and_then(|c| c.reply().ok())
    };
    let clients: Vec<u32> = prop(root, list, AtomEnum::WINDOW.into())
        .and_then(|r| r.value32().map(Iterator::collect))
        .unwrap_or_default();
    let windows: Vec<(u32, String, Option<u32>)> = clients
        .into_iter()
        .map(|w| {
            let title = prop(w, name, utf8)
                .map(|r| String::from_utf8_lossy(&r.value).into_owned())
                .unwrap_or_default();
            let pid = prop(w, wm_pid, AtomEnum::CARDINAL.into())
                .and_then(|r| r.value32().and_then(|mut v| v.next()));
            (w, title, pid)
        })
        .filter(|(_, _, pid)| a.pid.is_none_or(|p| *pid == Some(p)))
        .collect();
    let lower = a.window.trim().to_lowercase();
    let (w, title, _) = windows
        .iter()
        .rev()
        .find(|(_, t, _)| *t == a.window)
        .or_else(|| {
            windows
                .iter()
                .rev()
                .find(|(_, t, _)| t.to_lowercase().contains(&lower))
        })
        .ok_or_else(|| format!("no window {:?}", a.window))?;
    let event = ClientMessageEvent::new(32, *w, close, [0, 2, 0, 0, 0]);
    conn.send_event(
        false,
        root,
        EventMask::SUBSTRUCTURE_REDIRECT | EventMask::SUBSTRUCTURE_NOTIFY,
        event,
    )
    .map_err(|e| format!("close {title:?}: {e}"))?;
    conn.flush().map_err(|e| format!("close {title:?}: {e}"))?;
    Ok(json!({"ok": true, "title": title}).to_string())
}

/// A PNG of a screen rectangle, the whole screen by default, written to `path` when given.
async fn shot(cua: &Cua, a: ShotArgs) -> Result<Output, String> {
    let rect = [
        a.x.unwrap_or(0.0),
        a.y.unwrap_or(0.0),
        a.w.unwrap_or(f64::MAX / 4.0),
        a.h.unwrap_or(f64::MAX / 4.0),
    ];
    let (w, h, png) = cua.region_png(rect)?;
    let image = B64.encode(&png);
    let mut out = json!({"ok": true, "width": w, "height": h});
    match a.path {
        Some(path) => {
            files::write_bytes(std::path::Path::new(&path), &png).await?;
            out["path"] = path.into();
        }
        None => out["png_b64"] = image.clone().into(),
    }
    Ok(Output {
        text: out.to_string(),
        image_b64: Some(image),
        ..Output::default()
    })
}

/// The driver's key names of `combo`: `ctrl+shift+s` is `["ctrl", "shift", "s"]`.
fn key_names(combo: &str) -> Result<Vec<String>, String> {
    let keys: Vec<String> = combo
        .split('+')
        .map(|k| {
            let k = k.trim().to_lowercase();
            match k.as_str() {
                "control" | "primary" => "ctrl".to_owned(),
                "enter" => "return".to_owned(),
                "esc" => "escape".to_owned(),
                "del" => "delete".to_owned(),
                "cmd" | "meta" | "win" => "super".to_owned(),
                _ => k,
            }
        })
        .collect();
    if keys.iter().any(String::is_empty) {
        return Err(format!("bad key combination {combo:?}"));
    }
    Ok(keys)
}

fn text_of(v: &Value) -> String {
    v["content"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|c| c["text"].as_str())
        .collect::<Vec<_>>()
        .join("\n")
}

/// Lower case without the punctuation that labels add: underscores, ellipses, a trailing colon.
fn norm(s: &str) -> String {
    let s = s.to_lowercase().replace(['_', '…'], "").replace("...", "");
    let s = s.trim().trim_end_matches(':');
    s.split_whitespace().collect::<Vec<_>>().join(" ")
}

fn parse<T: serde::de::DeserializeOwned>(args: Value) -> Result<T, String> {
    serde_json::from_value(args).map_err(|e| format!("invalid arguments: {e}"))
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
struct FindArgs {
    name: String,
    #[serde(default)]
    role: Option<String>,
    #[serde(default)]
    window: Option<String>,
    #[serde(default)]
    pid: Option<u32>,
}

#[derive(Deserialize)]
struct ReadArgs {
    id: String,
    #[serde(default)]
    text: bool,
}

#[derive(Deserialize)]
struct IdArgs {
    id: String,
}

#[derive(Deserialize)]
struct ActArgs {
    id: String,
    action: String,
}

#[derive(Deserialize)]
struct AtArgs {
    x: i32,
    y: i32,
    #[serde(default)]
    window: Option<String>,
    #[serde(default)]
    pid: Option<u32>,
}

#[derive(Deserialize)]
struct KeyArgs {
    keys: String,
}

#[derive(Deserialize)]
struct TypeArgs {
    text: String,
}

#[derive(Deserialize)]
struct ClickArgs {
    x: f64,
    y: f64,
    #[serde(default)]
    button: Option<String>,
    #[serde(default)]
    count: Option<u32>,
}

#[derive(Deserialize)]
struct WindowArgs {
    window: String,
    #[serde(default)]
    pid: Option<u32>,
}

#[derive(Deserialize)]
struct LaunchArgs {
    app: String,
}

#[derive(Deserialize)]
struct ShotArgs {
    #[serde(default)]
    x: Option<f64>,
    #[serde(default)]
    y: Option<f64>,
    #[serde(default)]
    w: Option<f64>,
    #[serde(default)]
    h: Option<f64>,
    #[serde(default)]
    path: Option<String>,
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
        let active: Vec<&Frame> = frames
            .iter()
            .filter(|f| f.active() && (f.role != WINDOW_ROLE || !f.name.is_empty()))
            .collect();
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
    if let Some(native) = n.native_role() {
        v["native_role"] = native.into();
    }
    if let Some(keys) = &n.keys {
        let (mnemonic, shortcut) = key_combos(keys);
        if let Some(m) = mnemonic {
            v["mnemonic"] = m.into();
        }
        if let Some(s) = shortcut {
            v["shortcut"] = s.into();
        }
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
    fn key_combinations_use_the_driver_names() {
        assert_eq!(key_names("Ctrl+Shift+S").unwrap(), ["ctrl", "shift", "s"]);
        assert_eq!(key_names("enter").unwrap(), ["return"]);
        assert_eq!(key_names("esc").unwrap(), ["escape"]);
        assert!(key_names("ctrl+").is_err());
    }

    #[test]
    fn names_compare_without_label_punctuation() {
        assert_eq!(norm("  _Save As...  "), "save as");
        assert_eq!(norm("Tab width:"), "tab width");
        assert_eq!(norm("Long  line\tmargin…"), "long line margin");
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
