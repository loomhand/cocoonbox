//! Chrome over CDP: page snapshots as ref-tagged element lists, actions by ref.

use std::fmt::Write as _;
use std::path::PathBuf;
use std::process::Stdio;
use std::time::Duration;

use futures_util::{SinkExt, StreamExt};
use serde::Deserialize;
use serde_json::{Value, json};
use tokio::net::TcpStream;
use tokio::process::Command;
use tokio::sync::Mutex;
use tokio_tungstenite::tungstenite::Message as WsMessage;
use tokio_tungstenite::{MaybeTlsStream, WebSocketStream};

const LAUNCH_WAIT: Duration = Duration::from_secs(20);
const LOAD_WAIT: Duration = Duration::from_secs(15);
const CALL_TIMEOUT: Duration = Duration::from_secs(30);
const POLL: Duration = Duration::from_millis(250);
const READ_CHARS: usize = 20_000;

const SNAPSHOT_JS: &str = r#"(() => {
  const MAX = 300;
  const refs = new Map();
  window.__cbRefs = refs;
  const out = [`Page: ${document.title} (${location.href})`];
  let truncated = false;
  let headings = 0;
  const ACTIVE_TAGS = new Set(['A', 'BUTTON', 'INPUT', 'SELECT', 'TEXTAREA', 'SUMMARY']);
  const ACTIVE_ROLES = new Set(['button', 'link', 'checkbox', 'radio', 'tab', 'menuitem',
    'menuitemcheckbox', 'menuitemradio', 'combobox', 'option', 'switch', 'searchbox',
    'textbox', 'slider', 'spinbutton', 'treeitem']);
  const TEXT_TAGS = new Set(['P', 'LI', 'TD', 'TH', 'LABEL', 'DT', 'DD', 'CAPTION',
    'FIGCAPTION', 'BLOCKQUOTE', 'PRE']);
  const SKIP = new Set(['SCRIPT', 'STYLE', 'NOSCRIPT', 'TEMPLATE', 'svg', 'IFRAME']);
  const clip = (s, n) => {
    s = (s || '').replace(/\s+/g, ' ').trim();
    return s.length > n ? s.slice(0, n - 1) + '…' : s;
  };
  const shown = (el) => {
    if (el.getAttribute('aria-hidden') === 'true') return false;
    const st = getComputedStyle(el);
    if (st.display === 'none' || st.visibility === 'hidden') return false;
    if (st.display === 'contents') return true;
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  const active = (el) =>
    (ACTIVE_TAGS.has(el.tagName) && !(el.tagName === 'A' && !el.hasAttribute('href'))
      && !(el.tagName === 'INPUT' && el.type === 'hidden'))
    || ACTIVE_ROLES.has(el.getAttribute('role'))
    || el.isContentEditable && el.hasAttribute('contenteditable')
    || el.hasAttribute('onclick');
  const roleOf = (el) => {
    const r = el.getAttribute('role');
    if (r) return r;
    switch (el.tagName) {
      case 'A': return 'link';
      case 'BUTTON': case 'SUMMARY': return 'button';
      case 'SELECT': return 'combobox';
      case 'TEXTAREA': return 'textbox';
      case 'INPUT':
        return ({ checkbox: 'checkbox', radio: 'radio', submit: 'button', button: 'button',
          reset: 'button', image: 'button', range: 'slider' })[el.type] || 'textbox';
    }
    return el.isContentEditable ? 'textbox' : el.tagName.toLowerCase();
  };
  const nameOf = (el) => clip(el.getAttribute('aria-label')
    || (el.labels && el.labels[0] && el.labels[0].innerText)
    || el.getAttribute('placeholder') || el.getAttribute('alt') || el.getAttribute('title')
    || (el.tagName === 'INPUT' ? '' : el.innerText) || '', 80);
  const pad = (d) => '  '.repeat(Math.min(d, 8));
  const walk = (root, depth) => {
    for (const el of root.children) {
      if (out.length > MAX) { truncated = true; return; }
      if (SKIP.has(el.tagName) || !shown(el)) continue;
      if (active(el)) {
        const ref = 'e' + (refs.size + 1);
        refs.set(ref, el);
        let line = `${pad(depth)}- ${roleOf(el)} "${nameOf(el)}" [ref=${ref}]`;
        if (el.disabled) line += ' disabled';
        if (el.checked) line += ' checked';
        if (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT') {
          if (el.type === 'password') line += ' value=<redacted>';
          else if (el.value) line += ` value="${clip(el.value, 60)}"`;
        }
        const href = el.tagName === 'A' ? el.getAttribute('href') : null;
        if (href && !href.startsWith('javascript:')) line += ` href="${clip(href, 100)}"`;
        out.push(line);
        continue;
      }
      const tag = /^H([1-6])$/.exec(el.tagName);
      const level = tag ? tag[1] : el.getAttribute('role') === 'heading' && (el.getAttribute('aria-level') || '2');
      const nested = el.querySelector('a[href],button,input,select,textarea,[role],[contenteditable],[onclick]');
      if (level) {
        headings++;
        out.push(`${pad(depth)}- heading "${clip(el.innerText, 150)}" [level=${level}]`);
        if (nested) walk(el, depth + 1);
        continue;
      }
      if (!nested && (TEXT_TAGS.has(el.tagName) || el.children.length === 0)) {
        const t = clip(el.innerText, 200);
        if (t) out.push(`${pad(depth)}- text "${t}"`);
        continue;
      }
      walk(el, depth + 1);
      if (el.shadowRoot) walk(el.shadowRoot, depth + 1);
    }
  };
  if (document.body) walk(document.body, 0);
  if (!headings && !truncated) out.splice(1, 0, '(this page has no headings)');
  if (truncated) out.push(`[snapshot truncated at ${MAX} lines; use browser_read for page text]`);
  return out.join('\n');
})()"#;

const POINT_JS: &str = r#"((ref) => {
  const el = window.__cbRefs && window.__cbRefs.get(ref);
  if (!el || !el.isConnected) throw new Error(`unknown or stale ref ${ref}; take a new browser_snapshot`);
  el.scrollIntoView({ block: 'center', inline: 'center' });
  const r = el.getBoundingClientRect();
  return { x: r.left + r.width / 2, y: r.top + r.height / 2,
    label: (el.getAttribute('aria-label') || el.innerText || el.value || el.tagName).replace(/\s+/g, ' ').trim().slice(0, 60) };
})"#;

const FOCUS_JS: &str = r#"((ref, clear) => {
  const el = window.__cbRefs && window.__cbRefs.get(ref);
  if (!el || !el.isConnected) throw new Error(`unknown or stale ref ${ref}; take a new browser_snapshot`);
  el.scrollIntoView({ block: 'center' });
  el.focus();
  if (clear) {
    if (typeof el.select === 'function') el.select();
    else document.execCommand('selectAll');
  }
  return true;
})"#;

const READ_JS: &str = r#"((ref) => {
  const el = ref ? window.__cbRefs && window.__cbRefs.get(ref) : document.body;
  if (!el) throw new Error(`unknown or stale ref ${ref}; take a new browser_snapshot`);
  return el.innerText || '';
})"#;

type Ws = WebSocketStream<MaybeTlsStream<TcpStream>>;

/// Owns the Chrome process lifecycle and serializes CDP sessions.
pub struct Browser {
    bin: String,
    profile: PathBuf,
    port: u16,
    http: reqwest::Client,
    lock: Mutex<()>,
}

impl Browser {
    /// Creates a browser handle; Chrome starts on first use.
    pub fn new(bin: String, profile: PathBuf, port: u16) -> Self {
        Self {
            bin,
            profile,
            port,
            http: reqwest::Client::new(),
            lock: Mutex::new(()),
        }
    }

    /// Dispatches a `browser_*` tool.
    pub async fn run(&self, tool: &str, args: Value) -> Result<String, String> {
        let _serial = self.lock.lock().await;
        if tool == "browser_close" {
            return self.close().await;
        }
        let mut cdp = self.connect().await?;
        match tool {
            "browser_navigate" => {
                let a: NavigateArgs = parse(args)?;
                let url = if a.url.contains("://") || a.url.starts_with("about:") {
                    a.url
                } else {
                    format!("https://{}", a.url)
                };
                let nav = cdp.call("Page.navigate", json!({"url": url})).await?;
                if let Some(err) = nav["errorText"].as_str().filter(|e| !e.is_empty()) {
                    return Err(format!("navigate {url}: {err}"));
                }
                cdp.wait_loaded().await;
                cdp.snapshot().await
            }
            "browser_snapshot" => cdp.snapshot().await,
            "browser_click" => {
                let a: RefArgs = parse(args)?;
                let p = cdp
                    .eval(&format!("{POINT_JS}({})", js_str(&a.element)))
                    .await?;
                let (x, y) = (
                    p["x"].as_f64().unwrap_or(0.0),
                    p["y"].as_f64().unwrap_or(0.0),
                );
                for kind in ["mouseMoved", "mousePressed", "mouseReleased"] {
                    cdp.call(
                        "Input.dispatchMouseEvent",
                        json!({"type": kind, "x": x, "y": y, "button": "left", "clickCount": 1}),
                    )
                    .await?;
                }
                tokio::time::sleep(Duration::from_millis(500)).await;
                cdp.wait_loaded().await;
                let here = cdp
                    .eval("document.title + ' (' + location.href + ')'")
                    .await?;
                Ok(format!(
                    "clicked {} \"{}\"; page is now {}",
                    a.element,
                    p["label"].as_str().unwrap_or_default(),
                    here.as_str().unwrap_or_default()
                ))
            }
            "browser_type" => {
                let a: TypeArgs = parse(args)?;
                cdp.eval(&format!("{FOCUS_JS}({}, {})", js_str(&a.element), a.clear))
                    .await?;
                cdp.call("Input.insertText", json!({"text": a.text}))
                    .await?;
                if a.submit {
                    for kind in ["keyDown", "keyUp"] {
                        cdp.call(
                            "Input.dispatchKeyEvent",
                            json!({"type": kind, "key": "Enter", "code": "Enter",
                                   "windowsVirtualKeyCode": 13, "text": "\r"}),
                        )
                        .await?;
                    }
                    tokio::time::sleep(Duration::from_millis(500)).await;
                    cdp.wait_loaded().await;
                }
                Ok(format!(
                    "typed {} characters into {}{}",
                    a.text.chars().count(),
                    a.element,
                    if a.submit { " and pressed Enter" } else { "" }
                ))
            }
            "browser_read" => {
                let a: ReadArgs = parse(args)?;
                let arg = a
                    .element
                    .as_deref()
                    .map_or_else(|| "null".to_owned(), js_str);
                let text = cdp.eval(&format!("{READ_JS}({arg})")).await?;
                let text = text.as_str().unwrap_or_default();
                let total = text.chars().count();
                let mut out: String = text.chars().take(READ_CHARS).collect();
                if total > READ_CHARS {
                    let _ = write!(out, "\n[truncated: {total} characters total]");
                }
                Ok(out)
            }
            "browser_start" => Ok("running".to_owned()),
            _ => Err(format!("unknown browser tool {tool}")),
        }
    }

    async fn connect(&self) -> Result<Cdp, String> {
        let targets = match self.targets().await {
            Ok(t) => t,
            Err(_) => {
                self.launch().await?;
                self.targets().await?
            }
        };
        let page = targets
            .into_iter()
            .find(|t| t.kind == "page" && !t.url.starts_with("devtools://"));
        let ws_url = match page {
            Some(t) => t.ws_url,
            None => {
                let url = format!("http://127.0.0.1:{}/json/new?about:blank", self.port);
                let t: Target = self
                    .http
                    .put(&url)
                    .send()
                    .await
                    .map_err(|e| format!("open tab: {e}"))?
                    .json()
                    .await
                    .map_err(|e| format!("decode new tab: {e}"))?;
                t.ws_url
            }
        };
        let (ws, _) = tokio_tungstenite::connect_async(ws_url.as_str())
            .await
            .map_err(|e| format!("connect cdp {ws_url}: {e}"))?;
        Ok(Cdp { ws, next_id: 0 })
    }

    /// Closes Chrome through CDP so it flushes its profile; reports whether it was running.
    async fn close(&self) -> Result<String, String> {
        let url = format!("http://127.0.0.1:{}/json/version", self.port);
        let Ok(resp) = self
            .http
            .get(&url)
            .timeout(Duration::from_secs(2))
            .send()
            .await
        else {
            return Ok("not running".to_owned());
        };
        let version: Value = resp
            .json()
            .await
            .map_err(|e| format!("decode {url}: {e}"))?;
        let ws_url = version["webSocketDebuggerUrl"].as_str().unwrap_or_default();
        let (ws, _) = tokio_tungstenite::connect_async(ws_url)
            .await
            .map_err(|e| format!("connect cdp {ws_url}: {e}"))?;
        let mut cdp = Cdp { ws, next_id: 0 };
        let _ = cdp.call("Browser.close", json!({})).await;
        // the CDP port closes before the profile is flushed; SingletonLock goes last on a clean exit
        let lock = self.profile.join("SingletonLock");
        let deadline = tokio::time::Instant::now() + LAUNCH_WAIT;
        while tokio::time::Instant::now() < deadline {
            if self.targets().await.is_err() && tokio::fs::symlink_metadata(&lock).await.is_err() {
                return Ok("closed".to_owned());
            }
            tokio::time::sleep(POLL).await;
        }
        Err(format!(
            "chrome did not exit within {}s of Browser.close",
            LAUNCH_WAIT.as_secs()
        ))
    }

    async fn targets(&self) -> Result<Vec<Target>, String> {
        let url = format!("http://127.0.0.1:{}/json/list", self.port);
        self.http
            .get(&url)
            .timeout(Duration::from_secs(2))
            .send()
            .await
            .map_err(|e| format!("get {url}: {e}"))?
            .json()
            .await
            .map_err(|e| format!("decode {url}: {e}"))
    }

    async fn launch(&self) -> Result<(), String> {
        Command::new(&self.bin)
            .arg(format!("--remote-debugging-port={}", self.port))
            .arg("--remote-debugging-address=127.0.0.1")
            .arg(format!("--user-data-dir={}", self.profile.display()))
            .args([
                "--force-renderer-accessibility",
                "--no-first-run",
                "--no-default-browser-check",
                "--disable-session-crashed-bubble",
                "--hide-crash-restore-bubble",
                "--start-maximized",
                "about:blank",
            ])
            .env("LANG", "en_US.UTF-8")
            .env("LANGUAGE", "en_US")
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .process_group(0)
            .spawn()
            .map_err(|e| format!("launch {}: {e}", self.bin))?;
        let deadline = tokio::time::Instant::now() + LAUNCH_WAIT;
        while tokio::time::Instant::now() < deadline {
            if self.targets().await.is_ok() {
                return Ok(());
            }
            tokio::time::sleep(POLL).await;
        }
        Err(format!(
            "chrome did not expose CDP on port {} within {}s",
            self.port,
            LAUNCH_WAIT.as_secs()
        ))
    }
}

#[derive(Deserialize)]
struct Target {
    #[serde(rename = "type")]
    kind: String,
    url: String,
    #[serde(rename = "webSocketDebuggerUrl", default)]
    ws_url: String,
}

#[derive(Deserialize)]
struct NavigateArgs {
    url: String,
}

#[derive(Deserialize)]
struct RefArgs {
    #[serde(rename = "ref")]
    element: String,
}

#[derive(Deserialize)]
struct TypeArgs {
    #[serde(rename = "ref")]
    element: String,
    text: String,
    #[serde(default)]
    clear: bool,
    #[serde(default)]
    submit: bool,
}

#[derive(Deserialize)]
struct ReadArgs {
    #[serde(rename = "ref", default)]
    element: Option<String>,
}

struct Cdp {
    ws: Ws,
    next_id: u64,
}

impl Cdp {
    async fn call(&mut self, method: &str, params: Value) -> Result<Value, String> {
        self.next_id += 1;
        let id = self.next_id;
        let mut msg = json!({"id": id, "method": method});
        msg["params"] = params;
        self.ws
            .send(WsMessage::text(msg.to_string()))
            .await
            .map_err(|e| format!("cdp send {method}: {e}"))?;
        let wait = async {
            while let Some(frame) = self.ws.next().await {
                let frame = frame.map_err(|e| format!("cdp read: {e}"))?;
                let WsMessage::Text(text) = frame else {
                    continue;
                };
                let mut v: Value =
                    serde_json::from_str(&text).map_err(|e| format!("cdp decode: {e}"))?;
                if v["id"].as_u64() != Some(id) {
                    continue;
                }
                if let Some(err) = v.get("error") {
                    return Err(format!("cdp {method}: {}", err["message"]));
                }
                return Ok(v["result"].take());
            }
            Err(format!("cdp connection closed during {method}"))
        };
        tokio::time::timeout(CALL_TIMEOUT, wait)
            .await
            .map_err(|_| format!("cdp {method} timed out"))?
    }

    async fn eval(&mut self, expression: &str) -> Result<Value, String> {
        let mut r = self
            .call(
                "Runtime.evaluate",
                json!({"expression": expression, "returnByValue": true, "awaitPromise": true}),
            )
            .await?;
        if let Some(ex) = r.get("exceptionDetails") {
            let msg = ex["exception"]["description"]
                .as_str()
                .or_else(|| ex["text"].as_str())
                .unwrap_or("script error");
            return Err(msg.lines().next().unwrap_or(msg).to_owned());
        }
        Ok(r["result"]["value"].take())
    }

    async fn wait_loaded(&mut self) {
        let deadline = tokio::time::Instant::now() + LOAD_WAIT;
        while tokio::time::Instant::now() < deadline {
            if let Ok(v) = self.eval("document.readyState").await
                && v == "complete"
            {
                return;
            }
            tokio::time::sleep(POLL).await;
        }
    }

    async fn snapshot(&mut self) -> Result<String, String> {
        let v = self.eval(SNAPSHOT_JS).await?;
        Ok(v.as_str().unwrap_or_default().to_owned())
    }
}

fn js_str(s: &str) -> String {
    Value::String(s.to_owned()).to_string()
}

fn parse<T: serde::de::DeserializeOwned>(args: Value) -> Result<T, String> {
    serde_json::from_value(args).map_err(|e| format!("invalid arguments: {e}"))
}
