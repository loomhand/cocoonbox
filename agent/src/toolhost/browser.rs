//! Chrome over CDP: page snapshots as ref-tagged element lists, actions by ref.

use std::collections::HashMap;
use std::fmt::Write as _;
use std::path::PathBuf;
use std::process::Stdio;
use std::sync::Arc;
use std::time::Duration;

use futures_util::{SinkExt, StreamExt};
use serde::Deserialize;
use serde_json::{Value, json};
use tokio::net::TcpStream;
use tokio::process::Command;
use tokio::sync::{Mutex, RwLock};
use tokio_tungstenite::tungstenite::Message as WsMessage;
use tokio_tungstenite::{MaybeTlsStream, WebSocketStream};
use zeroize::Zeroizing;

use super::timing;

const LAUNCH_WAIT: Duration = Duration::from_secs(20);
const LOAD_WAIT: Duration = Duration::from_secs(15);
const CALL_TIMEOUT: Duration = Duration::from_secs(30);
const POLL: Duration = Duration::from_millis(250);
const READ_CHARS: usize = 20_000;
const RUNNING_MARKER: &str = "cocoonbox-was-running";
const XPROP: &str = "xprop";
const XPROP_WAIT: Duration = Duration::from_secs(2);
const CHROME_APPS: [&str; 2] = ["Google Chrome", "Chromium"];

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

const FILL_JS: &str = r#"(async (origin, username, secret, kind, submit) => {
  if (location.origin !== origin) return { error: `the active tab is on ${location.origin}, not ${origin}` };
  const shown = (el) => {
    const r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0 && getComputedStyle(el).visibility !== 'hidden';
  };
  const inputs = [...document.querySelectorAll('input')].filter((el) => !el.disabled && !el.readOnly && shown(el));
  const textual = (el) => ['text', 'email', 'tel', 'number'].includes(el.type);
  const userish = (el) => /user|mail|login|account|phone|mobile|账号|帐号|邮箱|手机/i
    .test([el.autocomplete, el.type, el.name, el.id, el.placeholder, el.getAttribute('aria-label')].join(' '));
  const set = (el, v) => {
    el.focus();
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(el, v);
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.dispatchEvent(new Event('change', { bubbles: true }));
  };
  const focused = inputs.includes(document.activeElement) ? document.activeElement : null;
  const filled = [];
  let last = null;
  if (kind === 'code') {
    const field = (focused && textual(focused) ? focused : null)
      || inputs.find((el) => el.autocomplete === 'one-time-code') || inputs.find(textual);
    if (field) { set(field, secret); filled.push('code'); last = field; }
  } else {
    let pass = focused && focused.type === 'password' ? focused : null;
    let user = focused && textual(focused) ? focused : null;
    if (user && !pass) pass = inputs.slice(inputs.indexOf(user) + 1).find((el) => el.type === 'password') || null;
    if (!user && !pass) pass = inputs.find((el) => el.type === 'password') || null;
    if (pass && !user) user = inputs.slice(0, inputs.indexOf(pass)).filter(textual).pop() || null;
    if (!user && !pass) user = inputs.find((el) => textual(el) && userish(el)) || null;
    if (user && username) { set(user, username); filled.push('username'); last = user; }
    if (pass && secret) { set(pass, secret); filled.push('password'); last = pass; }
  }
  if (!last) return { error: 'no sign-in field on this page' };
  if (!submit) return { filled, submitted: '' };
  await new Promise((r) => setTimeout(r, 100));
  const named = (el) => el.textContent.length < 40
    && /^(登\s*录|登入|log\s*in|sign\s*in|continue|next|submit|verify|下一步|继续|提交|验证|确[定认])$/i
      .test((el.innerText || el.value || el.getAttribute('aria-label') || '').trim());
  const clickable = (el) => !el.disabled && shown(el)
    && (el.matches('button,input,[role=button],a') || getComputedStyle(el).cursor === 'pointer');
  let btn = last.form && [...last.form.elements].find((el) => ['submit', 'image'].includes(el.type) && clickable(el));
  for (let a = last.parentElement; a && !btn; a = a.parentElement) {
    btn = [...a.querySelectorAll('button,input,[role=button],a,div,span')].find((el) =>
      last.compareDocumentPosition(el) & Node.DOCUMENT_POSITION_FOLLOWING && named(el) && clickable(el));
  }
  if (btn) {
    btn.scrollIntoView({ block: 'center', inline: 'center' });
    const r = btn.getBoundingClientRect();
    const x = r.left + r.width / 2, y = r.top + r.height / 2;
    if (btn.contains(document.elementFromPoint(x, y))) return { filled, submitted: 'click', x, y };
  }
  last.focus();
  return { filled, submitted: 'enter' };
})"#;

type Ws = WebSocketStream<MaybeTlsStream<TcpStream>>;

/// Owns the Chrome process lifecycle and one window per agent; an agent's calls run in order.
pub struct Browser {
    bin: String,
    profile: PathBuf,
    port: u16,
    http: reqwest::Client,
    life: RwLock<()>,
    starting: Mutex<()>,
    windows: Mutex<HashMap<String, Arc<Mutex<Option<Target>>>>>,
    last: Mutex<String>,
}

impl Browser {
    /// Creates a browser handle; Chrome starts on first use.
    pub fn new(bin: String, profile: PathBuf, port: u16) -> Self {
        Self {
            bin,
            profile,
            port,
            http: reqwest::Client::new(),
            life: RwLock::new(()),
            starting: Mutex::new(()),
            windows: Mutex::new(HashMap::new()),
            last: Mutex::new(String::new()),
        }
    }

    /// Dispatches a `browser_*` tool in the window of `agent_id`.
    pub async fn run(&self, agent_id: &str, tool: &str, args: Value) -> Result<String, String> {
        if tool == "browser_close" {
            let _life = self.life.write().await;
            return self.close().await;
        }
        let _life = self.life.read().await;
        match tool {
            "browser_start" => {
                if args["if_was_running"].as_bool() == Some(true)
                    && tokio::fs::symlink_metadata(self.profile.join(RUNNING_MARKER))
                        .await
                        .is_err()
                {
                    return Ok("not running".to_owned());
                }
                self.running().await?;
                return Ok("running".to_owned());
            }
            "browser_end" => return Ok(self.end(agent_id).await),
            _ => {}
        }
        let slot = Arc::clone(
            self.windows
                .lock()
                .await
                .entry(agent_id.to_owned())
                .or_default(),
        );
        let mut window = slot.lock().await;
        let mut cdp = self.connect(&mut window).await?;
        timing::mark("connect");
        if let Some(t) = window.as_ref() {
            t.ws_url.clone_into(&mut *self.last.lock().await);
        }
        match tool {
            "browser_navigate" => {
                let a: NavigateArgs = parse(args)?;
                let url = if a.url.contains("://") || a.url.starts_with("about:") {
                    a.url
                } else {
                    format!("https://{}", a.url)
                };
                let nav = cdp.call("Page.navigate", json!({"url": url})).await?;
                timing::mark("navigate");
                if let Some(err) = nav["errorText"].as_str().filter(|e| !e.is_empty()) {
                    return Err(format!("navigate {url}: {err}"));
                }
                cdp.wait_loaded().await;
                timing::mark("load");
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
                cdp.click(x, y).await?;
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
                    cdp.press_enter().await?;
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
            _ => Err(format!("unknown browser tool {tool}")),
        }
    }

    /// Origin of the active tab; Chrome is not started for it.
    pub async fn origin(&self) -> Result<String, String> {
        let _life = self.life.read().await;
        let mut cdp = self.attach().await?;
        let origin = cdp.eval("location.origin").await?;
        Ok(origin.as_str().unwrap_or_default().to_owned())
    }

    /// Fills the sign-in form of the active tab when its origin equals `origin`; errors never carry a value.
    pub async fn fill(&self, args: &Value) -> Result<String, String> {
        let origin = args["origin"].as_str().unwrap_or_default();
        if origin.is_empty() || origin == "null" {
            return Err("fill needs the origin of the tab".to_owned());
        }
        let (kind, secret) = match (args["password"].as_str(), args["code"].as_str()) {
            (Some(p), None) => ("password", p),
            (None, Some(c)) => ("code", c),
            _ => return Err("fill needs either a password or a code".to_owned()),
        };
        let username = args["username"].as_str().unwrap_or_default();
        let submit = args["submit"].as_bool().unwrap_or(false);
        let _life = self.life.read().await;
        let mut cdp = self.attach().await?;
        let user = js_secret(username);
        let secret = js_secret(secret);
        let mut expr = Zeroizing::new(String::with_capacity(
            FILL_JS.len() + origin.len() * 6 + user.len() + secret.len() + 64,
        ));
        for part in [FILL_JS, "(", &js_str(origin), ",", &user, ",", &secret, ","] {
            expr.push_str(part);
        }
        let _ = write!(expr, "{},{submit})", js_str(kind));
        let r = cdp.eval(&expr).await?;
        if let Some(e) = r["error"].as_str() {
            return Err(e.to_owned());
        }
        let submitted = match r["submitted"].as_str() {
            Some("click") => {
                cdp.click(
                    r["x"].as_f64().unwrap_or(0.0),
                    r["y"].as_f64().unwrap_or(0.0),
                )
                .await?;
                true
            }
            Some("enter") => {
                cdp.press_enter().await?;
                true
            }
            _ => false,
        };
        Ok(json!({"filled": r["filled"], "submitted": submitted}).to_string())
    }

    /// The tab the user sees in the X active window, else the window the browser tools
    /// last acted on, else the first tab Chrome lists.
    async fn attach(&self) -> Result<Cdp, String> {
        let targets = self
            .targets()
            .await
            .map_err(|_| "Chrome is not running in the box".to_owned())?;
        if let Some(cdp) = self.shown(&targets).await {
            return Ok(cdp);
        }
        let last = self.last.lock().await.clone();
        if !last.is_empty()
            && let Ok(cdp) = Cdp::open(&last).await
        {
            return Ok(cdp);
        }
        let page = targets
            .into_iter()
            .find(Target::is_tab)
            .ok_or("no tab is open in the box's Chrome")?;
        Cdp::open(&page.ws_url).await
    }

    /// The visible tab titled as the X active window, when exactly one matches.
    async fn shown(&self, targets: &[Target]) -> Option<Cdp> {
        let title = active_title().await?;
        let infos = self
            .browser()
            .await
            .ok()?
            .call("Target.getTargets", json!({}))
            .await
            .ok()?;
        let ids: Vec<&str> = infos["targetInfos"]
            .as_array()?
            .iter()
            .filter(|i| i["type"] == "page" && i["title"] == title.as_str())
            .filter_map(|i| i["targetId"].as_str())
            .collect();
        let mut found = None;
        for t in targets.iter().filter(|t| ids.contains(&t.id.as_str())) {
            let Ok(mut cdp) = Cdp::open(&t.ws_url).await else {
                continue;
            };
            if cdp.eval("document.visibilityState").await == Ok(json!("visible"))
                && found.replace(cdp).is_some()
            {
                return None;
            }
        }
        found
    }

    /// Attaches to the window in `window`, opening a new one when it is missing or was closed.
    async fn connect(&self, window: &mut Option<Target>) -> Result<Cdp, String> {
        if let Some(t) = window.as_ref()
            && let Ok(cdp) = Cdp::open(&t.ws_url).await
        {
            return Ok(cdp);
        }
        self.running().await?;
        let created = self
            .browser()
            .await?
            .call(
                "Target.createTarget",
                json!({"url": "about:blank", "newWindow": true, "background": true}),
            )
            .await?;
        let id = created["targetId"].as_str().unwrap_or_default();
        let t = self
            .targets()
            .await?
            .into_iter()
            .find(|t| t.id == id)
            .ok_or_else(|| format!("new window {id} is not listed"))?;
        let cdp = Cdp::open(&t.ws_url).await?;
        *window = Some(t);
        Ok(cdp)
    }

    /// Closes the window of `agent_id`; Chrome is not started for it.
    async fn end(&self, agent_id: &str) -> String {
        let Some(slot) = self.windows.lock().await.remove(agent_id) else {
            return "no window".to_owned();
        };
        let Some(t) = slot.lock().await.take() else {
            return "no window".to_owned();
        };
        if let Ok(mut cdp) = self.browser().await {
            cdp.close_window(&t.id).await;
        }
        "closed".to_owned()
    }

    async fn running(&self) -> Result<(), String> {
        let _starting = self.starting.lock().await;
        if self.targets().await.is_err() {
            self.launch().await?;
        }
        Ok(())
    }

    async fn browser(&self) -> Result<Cdp, String> {
        let url = format!("http://127.0.0.1:{}/json/version", self.port);
        let version: Value = self
            .http
            .get(&url)
            .timeout(Duration::from_secs(2))
            .send()
            .await
            .map_err(|e| format!("get {url}: {e}"))?
            .json()
            .await
            .map_err(|e| format!("decode {url}: {e}"))?;
        Cdp::open(version["webSocketDebuggerUrl"].as_str().unwrap_or_default()).await
    }

    /// Closes Chrome through CDP so it flushes its profile; reports whether it was running
    /// and records it in the profile for `browser_start` with `if_was_running`.
    /// Agent windows close first, so the restored session holds only the user's.
    async fn close(&self) -> Result<String, String> {
        let marker = self.profile.join(RUNNING_MARKER);
        let windows: Vec<_> = self.windows.lock().await.drain().map(|(_, w)| w).collect();
        if self.targets().await.is_err() {
            let _ = tokio::fs::remove_file(&marker).await;
            return Ok("not running".to_owned());
        }
        let mut cdp = self.browser().await?;
        for w in windows {
            if let Some(t) = w.lock().await.take() {
                cdp.close_window(&t.id).await;
            }
        }
        let _ = cdp.call("Browser.close", json!({})).await;
        // the CDP port closes before the profile is flushed; SingletonLock goes last on a clean exit
        let lock = self.profile.join("SingletonLock");
        let deadline = tokio::time::Instant::now() + LAUNCH_WAIT;
        while tokio::time::Instant::now() < deadline {
            if self.targets().await.is_err() && tokio::fs::symlink_metadata(&lock).await.is_err() {
                tokio::fs::write(&marker, b"")
                    .await
                    .map_err(|e| format!("write {}: {e}", marker.display()))?;
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
                "--password-store=basic",
                "--start-maximized",
                "--restore-last-session",
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
    #[serde(default)]
    id: String,
    #[serde(rename = "type")]
    kind: String,
    url: String,
    #[serde(rename = "webSocketDebuggerUrl", default)]
    ws_url: String,
}

impl Target {
    fn is_tab(&self) -> bool {
        self.kind == "page" && !self.url.starts_with("devtools://")
    }
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
    async fn open(ws_url: &str) -> Result<Self, String> {
        let (ws, _) = tokio_tungstenite::connect_async(ws_url)
            .await
            .map_err(|e| format!("connect cdp {ws_url}: {e}"))?;
        Ok(Self { ws, next_id: 0 })
    }

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

    async fn click(&mut self, x: f64, y: f64) -> Result<(), String> {
        for kind in ["mouseMoved", "mousePressed", "mouseReleased"] {
            self.call(
                "Input.dispatchMouseEvent",
                json!({"type": kind, "x": x, "y": y, "button": "left", "clickCount": 1}),
            )
            .await?;
        }
        Ok(())
    }

    async fn press_enter(&mut self) -> Result<(), String> {
        for kind in ["keyDown", "keyUp"] {
            self.call(
                "Input.dispatchKeyEvent",
                json!({"type": kind, "key": "Enter", "code": "Enter",
                       "windowsVirtualKeyCode": 13, "text": "\r"}),
            )
            .await?;
        }
        tokio::time::sleep(Duration::from_millis(500)).await;
        self.wait_loaded().await;
        Ok(())
    }

    /// Closes the agent window of page `id` with every tab the agent opened from it.
    async fn close_window(&mut self, id: &str) {
        let Ok(infos) = self.call("Target.getTargets", json!({})).await else {
            return;
        };
        let mut pages = Vec::new();
        for info in infos["targetInfos"]
            .as_array()
            .into_iter()
            .flatten()
            .filter(|i| i["type"] == "page")
        {
            let window = self
                .call(
                    "Browser.getWindowForTarget",
                    json!({"targetId": info["targetId"]}),
                )
                .await
                .ok()
                .and_then(|w| w["windowId"].as_i64());
            pages.push((info, window));
        }
        for t in window_tabs(id, &pages) {
            let _ = self
                .call("Target.closeTarget", json!({"targetId": t}))
                .await;
        }
    }

    async fn snapshot(&mut self) -> Result<String, String> {
        let v = self.eval(SNAPSHOT_JS).await?;
        let snapshot = v.as_str().unwrap_or_default().to_owned();
        timing::mark("snapshot");
        timing::note("snapshot_bytes", snapshot.len());
        Ok(snapshot)
    }
}

/// Page `id`, the tabs in its window, and the pages opened from any of them, transitively.
fn window_tabs<'a>(id: &'a str, pages: &[(&'a Value, Option<i64>)]) -> Vec<&'a str> {
    let target = |info: &'a Value| info["targetId"].as_str().unwrap_or_default();
    let window = pages
        .iter()
        .find(|(info, _)| target(info) == id)
        .and_then(|(_, w)| *w);
    let mut owned = vec![id];
    owned.extend(
        pages
            .iter()
            .filter(|(info, w)| window.is_some() && *w == window && target(info) != id)
            .map(|(info, _)| target(info)),
    );
    loop {
        let opened: Vec<&str> = pages
            .iter()
            .map(|(info, _)| *info)
            .filter(|info| !owned.contains(&target(info)))
            .filter(|info| {
                ["openerId", "openerFrameId"]
                    .iter()
                    .any(|k| info[*k].as_str().is_some_and(|o| owned.contains(&o)))
            })
            .map(target)
            .collect();
        if opened.is_empty() {
            return owned;
        }
        owned.extend(opened);
    }
}

/// Page title of the X active window when it is a Chrome window.
async fn active_title() -> Option<String> {
    let root = xprop(&["-root", "_NET_ACTIVE_WINDOW"]).await?;
    let name = xprop(&["-id", active_window(&root)?, "_NET_WM_NAME"]).await?;
    chrome_page_title(&name)
}

async fn xprop(args: &[&str]) -> Option<String> {
    let run = Command::new(XPROP)
        .args(args)
        .stdin(Stdio::null())
        .kill_on_drop(true)
        .output();
    let out = tokio::time::timeout(XPROP_WAIT, run).await.ok()?.ok()?;
    out.status
        .success()
        .then(|| String::from_utf8_lossy(&out.stdout).into_owned())
}

/// The window id in `_NET_ACTIVE_WINDOW(WINDOW): window id # 0x600003, 0x0`; none for `0x0`.
fn active_window(xprop: &str) -> Option<&str> {
    let id = xprop.split_once('#')?.1.split(',').next()?.trim();
    (id.starts_with("0x") && id != "0x0").then_some(id)
}

/// The page title in `_NET_WM_NAME(UTF8_STRING) = "<title> - Google Chrome"`.
fn chrome_page_title(xprop: &str) -> Option<String> {
    let name = xprop_string(xprop)?;
    let (title, app) = name.rsplit_once(" - ")?;
    CHROME_APPS.contains(&app).then(|| title.to_owned())
}

/// The quoted value xprop prints, undoing its `\"`, `\\` and octal `\ooo` escapes.
fn xprop_string(xprop: &str) -> Option<String> {
    let quoted = xprop.split_once(" = \"")?.1.trim_end().strip_suffix('"')?;
    let mut out = Vec::with_capacity(quoted.len());
    let mut rest = quoted.as_bytes();
    while let Some((&b, tail)) = rest.split_first() {
        rest = tail;
        if b != b'\\' {
            out.push(b);
        } else if let Some(octal) = rest
            .get(..3)
            .filter(|d| d.iter().all(|c| (b'0'..=b'7').contains(c)))
        {
            out.push(u8::from_str_radix(std::str::from_utf8(octal).ok()?, 8).ok()?);
            rest = &rest[3..];
        } else if let Some((&c, tail)) = rest.split_first() {
            out.push(c);
            rest = tail;
        }
    }
    String::from_utf8(out).ok()
}

fn js_str(s: &str) -> String {
    Value::String(s.to_owned()).to_string()
}

/// `s` as a JS string literal in a buffer sized for the worst-case escape, so it never reallocates.
fn js_secret(s: &str) -> Zeroizing<String> {
    let mut out = Zeroizing::new(String::with_capacity(s.len() * 6 + 2));
    out.push('"');
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            c if u32::from(c) < 0x20 || c == '\u{2028}' || c == '\u{2029}' => {
                let _ = write!(out, "\\u{:04x}", u32::from(c));
            }
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

fn parse<T: serde::de::DeserializeOwned>(args: Value) -> Result<T, String> {
    serde_json::from_value(args).map_err(|e| format!("invalid arguments: {e}"))
}

#[cfg(test)]
mod tests {
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tokio::net::TcpListener;

    use super::*;

    /// Serves Chrome's CDP endpoints on the returned port until a `Browser.close` arrives.
    async fn fake_chrome() -> u16 {
        let http = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let ws = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = http.local_addr().unwrap().port();
        let version = json!({
            "webSocketDebuggerUrl": format!("ws://{}/devtools/browser/fake", ws.local_addr().unwrap()),
        })
        .to_string();
        let (closed, mut stopped) = tokio::sync::watch::channel(false);
        tokio::spawn(async move {
            let (stream, _) = ws.accept().await.unwrap();
            let mut ws = tokio_tungstenite::accept_async(stream).await.unwrap();
            while let Some(Ok(WsMessage::Text(text))) = ws.next().await {
                let v: Value = serde_json::from_str(&text).unwrap();
                if v["method"] == "Browser.close" {
                    let reply = json!({"id": v["id"], "result": {}}).to_string();
                    ws.send(WsMessage::text(reply)).await.unwrap();
                    closed.send(true).unwrap();
                    return;
                }
            }
        });
        tokio::spawn(async move {
            loop {
                tokio::select! {
                    _ = stopped.changed() => return,
                    Ok((mut s, _)) = http.accept() => {
                        let mut buf = [0u8; 1024];
                        let n = s.read(&mut buf).await.unwrap_or(0);
                        let body = if buf[..n].starts_with(b"GET /json/version") { version.as_str() } else { "[]" };
                        let head = format!("HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n", body.len());
                        let _ = s.write_all(format!("{head}{body}").as_bytes()).await;
                    }
                }
            }
        });
        port
    }

    /// Serves CDP on the returned port only after `delay`; every new window is page 1, whose script evaluations return "Page: slow".
    async fn slow_chrome(delay: Duration) -> u16 {
        let port = std::net::TcpListener::bind("127.0.0.1:0")
            .unwrap()
            .local_addr()
            .unwrap()
            .port();
        let ws = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = ws.local_addr().unwrap();
        let list = json!([{
            "id": "1",
            "type": "page",
            "url": "about:blank",
            "webSocketDebuggerUrl": format!("ws://{addr}/devtools/page/1"),
        }])
        .to_string();
        let version = json!({"webSocketDebuggerUrl": format!("ws://{addr}/devtools/browser/fake")})
            .to_string();
        tokio::spawn(async move {
            while let Ok((stream, _)) = ws.accept().await {
                tokio::spawn(async move {
                    let mut ws = tokio_tungstenite::accept_async(stream).await.unwrap();
                    while let Some(Ok(WsMessage::Text(text))) = ws.next().await {
                        let v: Value = serde_json::from_str(&text).unwrap();
                        let reply = json!({"id": v["id"], "result": {"targetId": "1", "result": {"value": "Page: slow"}}});
                        ws.send(WsMessage::text(reply.to_string())).await.unwrap();
                    }
                });
            }
        });
        tokio::spawn(async move {
            tokio::time::sleep(delay).await;
            let http = TcpListener::bind(("127.0.0.1", port)).await.unwrap();
            while let Ok((mut s, _)) = http.accept().await {
                let mut buf = [0u8; 1024];
                let n = s.read(&mut buf).await.unwrap_or(0);
                let body = if buf[..n].starts_with(b"GET /json/version") {
                    &version
                } else {
                    &list
                };
                let head = format!(
                    "HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n",
                    body.len()
                );
                let _ = s.write_all(format!("{head}{body}").as_bytes()).await;
            }
        });
        port
    }

    #[tokio::test]
    async fn a_browser_tool_waits_for_a_start_in_flight() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join(RUNNING_MARKER), b"").unwrap();
        let port = slow_chrome(Duration::from_millis(800)).await;
        let b = std::sync::Arc::new(Browser::new("true".into(), dir.path().into(), port));
        let start = {
            let b = std::sync::Arc::clone(&b);
            tokio::spawn(async move {
                b.run("state", "browser_start", json!({"if_was_running": true}))
                    .await
            })
        };
        tokio::time::sleep(Duration::from_millis(50)).await;
        let asked = tokio::time::Instant::now();
        assert_eq!(
            b.run("a", "browser_snapshot", json!({})).await.unwrap(),
            "Page: slow"
        );
        assert!(
            asked.elapsed() >= Duration::from_millis(500),
            "{:?}",
            asked.elapsed()
        );
        assert_eq!(start.await.unwrap().unwrap(), "running");
    }

    #[test]
    fn the_active_window_title_names_a_chrome_page_in_either_locale() {
        assert_eq!(
            active_window("_NET_ACTIVE_WINDOW(WINDOW): window id # 0x600003, 0x0\n"),
            Some("0x600003")
        );
        assert_eq!(
            active_window("_NET_ACTIVE_WINDOW(WINDOW): window id # 0x0\n"),
            None
        );
        let title = Some("登录 – \"q\" \\ back slash".to_owned());
        let utf8 =
            "_NET_WM_NAME(UTF8_STRING) = \"登录 – \\\"q\\\" \\\\ back slash - Google Chrome\"\n";
        let c = r#"_NET_WM_NAME(UTF8_STRING) = "\347\231\273\345\275\225 \342\200\223 \"q\" \\ back slash - Google Chrome""#;
        assert_eq!(chrome_page_title(utf8), title);
        assert_eq!(chrome_page_title(c), title);
        assert_eq!(
            chrome_page_title("_NET_WM_NAME(UTF8_STRING) = \"a - b - Chromium\""),
            Some("a - b".to_owned())
        );
        assert_eq!(
            chrome_page_title("_NET_WM_NAME(UTF8_STRING) = \"*notes.txt - Mousepad\""),
            None
        );
    }

    #[test]
    fn window_tabs_are_the_agent_window_and_every_page_opened_from_it() {
        let pages = [
            json!({"targetId": "agent"}),
            json!({"targetId": "user"}),
            json!({"targetId": "sametab"}),
            json!({"targetId": "blank", "openerFrameId": "agent"}),
            json!({"targetId": "popup", "openerId": "blank"}),
            json!({"targetId": "usertab", "openerId": "user"}),
        ];
        let windows = [Some(1), Some(2), Some(1), Some(3), Some(4), Some(2)];
        let pages: Vec<_> = pages.iter().zip(windows).collect();
        assert_eq!(
            window_tabs("agent", &pages),
            ["agent", "sametab", "blank", "popup"]
        );
        assert_eq!(window_tabs("gone", &pages), ["gone"]);
    }

    #[test]
    fn js_secret_is_a_string_literal_of_its_input_without_reallocating() {
        for s in ["plain", "quo\"te\\back", "line\nbreak\u{2028}", "密码", ""] {
            let lit = js_secret(s);
            assert_eq!(serde_json::from_str::<String>(&lit).unwrap(), s);
            assert_eq!(lit.capacity(), s.len() * 6 + 2);
        }
    }

    #[tokio::test]
    async fn fill_refuses_bad_arguments_and_never_starts_chrome() {
        let dir = tempfile::tempdir().unwrap();
        let b = Browser::new("/nonexistent/chrome".into(), dir.path().into(), 1);
        let both = json!({"origin": "https://a.test", "password": "p", "code": "1"});
        for (args, want) in [
            (json!({"password": "p"}), "fill needs the origin of the tab"),
            (
                json!({"origin": "null", "password": "p"}),
                "fill needs the origin of the tab",
            ),
            (both, "fill needs either a password or a code"),
            (
                json!({"origin": "https://a.test", "password": "p"}),
                "Chrome is not running in the box",
            ),
        ] {
            assert_eq!(b.fill(&args).await.unwrap_err(), want);
        }
        assert_eq!(
            b.origin().await.unwrap_err(),
            "Chrome is not running in the box"
        );
    }

    #[tokio::test]
    async fn close_records_whether_chrome_ran_and_start_follows_the_record() {
        let dir = tempfile::tempdir().unwrap();
        let marker = dir.path().join(RUNNING_MARKER);
        let running = Browser::new(
            "/nonexistent/chrome".into(),
            dir.path().into(),
            fake_chrome().await,
        );
        assert_eq!(
            running
                .run("state", "browser_close", json!({}))
                .await
                .unwrap(),
            "closed"
        );
        assert!(marker.exists());
        let start = running
            .run("state", "browser_start", json!({"if_was_running": true}))
            .await;
        assert!(start.unwrap_err().contains("launch /nonexistent/chrome"));

        let idle = Browser::new("/nonexistent/chrome".into(), dir.path().into(), 1);
        assert_eq!(
            idle.run("state", "browser_close", json!({})).await.unwrap(),
            "not running"
        );
        assert!(!marker.exists());
        let start = idle
            .run("state", "browser_start", json!({"if_was_running": true}))
            .await;
        assert_eq!(start.unwrap(), "not running");
    }
}
