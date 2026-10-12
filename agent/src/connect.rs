//! MCP client of the Connect box door: the box owner's connector tools, offered to the main loop only.

use std::borrow::Cow;
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, Instant};

use base64::Engine;
use base64::engine::general_purpose::STANDARD as B64;
use bytes::Bytes;
use futures_util::StreamExt;
use reqwest::{StatusCode, header};
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

use crate::dispatch::ToolOutput;
use crate::llm::ToolSpec;
use crate::tools;

const PROTOCOL: &str = "2025-11-25";
const SESSION: &str = "mcp-session-id";
const APPROVAL: &str = "cocoonbox/approval";
const APPROVAL_ID: &str = "cocoonbox/approval_id";
const LIST_CHANGED: &str = "notifications/tools/list_changed";
const ANSWERED: &str = "notifications/cocoonbox/approval_answered";
const EVENT_STREAM: &str = "text/event-stream";
const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const CALL_TIMEOUT: Duration = Duration::from_secs(150);
const RETRY: Duration = Duration::from_secs(30);
const RETRY_MIN: Duration = Duration::from_secs(1);
const MAX_PAGES: usize = 20;

/// What a connector call came back with.
pub enum Outcome {
    Done(ToolOutput),
    /// The broker runs the call only after the user approves `id`.
    Approval {
        id: String,
        summary: String,
    },
}

/// One MCP session on the box door; the egress proxy adds the machine's door key.
pub struct Client {
    url: String,
    http: reqwest::Client,
    state: Mutex<State>,
}

impl Client {
    /// A client of the door at `url`; its tools are listed on first use.
    pub fn new(url: &str) -> Result<Self, String> {
        let http = reqwest::Client::builder()
            .connect_timeout(CONNECT_TIMEOUT)
            .build()
            .map_err(|e| format!("build connect client: {e}"))?;
        Ok(Self {
            url: url.to_owned(),
            http,
            state: Mutex::new(State {
                session: None,
                tools: Arc::default(),
                stale: true,
                retry_at: None,
            }),
        })
    }

    /// Follows the door's stream until `stop`: after a `list_changed` the next turn step lists the tools again, and
    /// `answered` gets each approval the user answered elsewhere, with whether it was approved.
    pub fn watch<F>(self: &Arc<Self>, stop: CancellationToken, answered: F)
    where
        F: Fn(&str, bool) + Send + Sync + 'static,
    {
        let client = Arc::clone(self);
        // detached: ends with stop
        tokio::spawn(async move {
            let mut delay = RETRY_MIN;
            loop {
                let start = Instant::now();
                match client.open().await {
                    Ok(resp) => {
                        delay = RETRY_MIN;
                        let e = client.follow(resp, &answered).await;
                        eprintln!("connect: list_changed stream: {e}");
                    }
                    Err(e) => eprintln!("connect: list_changed stream: {e}"),
                }
                client.state().stale = true;
                if start.elapsed() >= RETRY {
                    delay = RETRY_MIN;
                }
                tokio::select! {
                    () = stop.cancelled() => return,
                    () = tokio::time::sleep(delay) => {}
                }
                delay = (delay * 2).min(RETRY);
            }
        });
    }

    /// The connector tools, listed again after a change; a failed list keeps the last one and waits before the next.
    pub async fn tools(&self) -> Arc<Vec<ToolSpec>> {
        {
            let st = self.state();
            if !st.stale || st.retry_at.is_some_and(|at| Instant::now() < at) {
                return Arc::clone(&st.tools);
            }
        }
        match self.list().await {
            Ok(tools) => {
                let mut st = self.state();
                st.tools = Arc::new(tools);
                st.stale = false;
                st.retry_at = None;
                Arc::clone(&st.tools)
            }
            Err(e) => {
                eprintln!("connect: list tools: {e}");
                let mut st = self.state();
                st.retry_at = Some(Instant::now() + RETRY);
                Arc::clone(&st.tools)
            }
        }
    }

    /// Whether `name` is one of the connector tools last listed.
    pub fn serves(&self, name: &str) -> bool {
        self.state().tools.iter().any(|t| t.name == name)
    }

    /// Calls connector tool `name`, carrying `approval` when the user approved this call.
    pub async fn call(&self, name: &str, args: &Value, approval: Option<&str>) -> Outcome {
        let mut params = json!({"name": name, "arguments": args});
        if let Some(id) = approval {
            params["_meta"] = json!({APPROVAL_ID: id});
        }
        let result = match self.rpc("tools/call", params).await {
            Ok(r) => r,
            Err(e) => return Outcome::Done(ToolOutput::error(format!("Connect: {e}"))),
        };
        let pending = &result["_meta"][APPROVAL];
        if let Some(id) = pending["approval_id"].as_str() {
            return Outcome::Approval {
                id: id.to_owned(),
                summary: pending["summary"].as_str().unwrap_or_default().to_owned(),
            };
        }
        Outcome::Done(output(&result))
    }

    async fn list(&self) -> Result<Vec<ToolSpec>, String> {
        let reserved = tools::main_tools(true, true);
        let mut out = Vec::new();
        let mut cursor: Option<String> = None;
        for _ in 0..MAX_PAGES {
            let params = cursor.map_or_else(|| json!({}), |c| json!({"cursor": c}));
            let mut page = self.rpc("tools/list", params).await?;
            let Value::Array(listed) = page["tools"].take() else {
                return Err("tools/list carries no tools".to_owned());
            };
            out.extend(
                listed
                    .into_iter()
                    .filter_map(spec)
                    .filter(|t| !reserved.iter().any(|r| r.name == t.name)),
            );
            cursor = page["nextCursor"]
                .as_str()
                .filter(|c| !c.is_empty())
                .map(str::to_owned);
            if cursor.is_none() {
                break;
            }
        }
        Ok(out)
    }

    /// Sends one request, opening a session first and once more when the door ended it.
    async fn rpc(&self, method: &str, params: Value) -> Result<Value, String> {
        let session = self.session().await?;
        match self.post(&session, method, &params).await {
            Err(Gone) => {
                self.forget(&session);
                let session = self.session().await?;
                self.post(&session, method, &params)
                    .await
                    .map_err(|_| "the door ended a new session".to_owned())?
            }
            Ok(r) => r,
        }
    }

    async fn session(&self) -> Result<String, String> {
        if let Some(s) = self.state().session.clone() {
            return Ok(s);
        }
        let init = json!({
            "jsonrpc": "2.0", "id": 0, "method": "initialize",
            "params": {"protocolVersion": PROTOCOL, "capabilities": {}, "clientInfo": {"name": "cocoonbox-agent", "version": env!("CB_AGENT_VERSION")}},
        });
        let resp = self.send(None, &init).await?;
        let session = resp
            .headers()
            .get(SESSION)
            .and_then(|v| v.to_str().ok())
            .unwrap_or_default()
            .to_owned();
        answer(resp).await?;
        let note = json!({"jsonrpc": "2.0", "method": "notifications/initialized"});
        self.send(Some(&session), &note).await?;
        self.state().session = Some(session.clone());
        Ok(session)
    }

    async fn post(
        &self,
        session: &str,
        method: &str,
        params: &Value,
    ) -> Result<Result<Value, String>, Gone> {
        let msg = json!({"jsonrpc": "2.0", "id": 1, "method": method, "params": params});
        match self.send(Some(session), &msg).await {
            Ok(resp) if resp.status() == StatusCode::NOT_FOUND => Err(Gone),
            Ok(resp) => Ok(answer(resp).await),
            Err(e) => Ok(Err(e)),
        }
    }

    async fn send(&self, session: Option<&str>, msg: &Value) -> Result<reqwest::Response, String> {
        let mut req = self
            .http
            .post(&self.url)
            .timeout(CALL_TIMEOUT)
            .header(header::ACCEPT, "application/json, text/event-stream")
            .header("mcp-protocol-version", PROTOCOL)
            .json(msg);
        if let Some(s) = session.filter(|s| !s.is_empty()) {
            req = req.header(SESSION, s);
        }
        req.send()
            .await
            .map_err(|e| format!("reach {}: {e}", self.url))
    }

    async fn open(&self) -> Result<reqwest::Response, String> {
        let session = self.session().await?;
        let resp = self
            .http
            .get(&self.url)
            .header(header::ACCEPT, EVENT_STREAM)
            .header(SESSION, &session)
            .send()
            .await
            .map_err(|e| format!("reach {}: {e}", self.url))?;
        if !resp.status().is_success() {
            if resp.status() == StatusCode::NOT_FOUND {
                self.forget(&session);
            }
            return Err(format!("the door answered {}", resp.status()));
        }
        self.state().stale = true;
        Ok(resp)
    }

    /// Reads the door's event stream, marking the tools stale on each `list_changed` and passing on each answer.
    async fn follow(&self, resp: reqwest::Response, answered: &impl Fn(&str, bool)) -> String {
        let mut body = resp.bytes_stream();
        let mut line = Vec::new();
        while let Some(chunk) = body.next().await {
            let chunk: Bytes = match chunk {
                Ok(c) => c,
                Err(e) => return format!("read: {e}"),
            };
            for b in chunk {
                if b != b'\n' {
                    line.push(b);
                    continue;
                }
                if let Some(data) = line.strip_prefix(b"data:") {
                    let data = String::from_utf8_lossy(data);
                    if data.contains(LIST_CHANGED) {
                        self.state().stale = true;
                    } else if let Some((id, approve)) = approval_answer(&data) {
                        answered(&id, approve);
                    }
                }
                line.clear();
            }
        }
        "the stream ended".to_owned()
    }

    fn forget(&self, session: &str) {
        let mut st = self.state();
        if st.session.as_deref() == Some(session) {
            st.session = None;
        }
    }

    fn state(&self) -> MutexGuard<'_, State> {
        self.state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }
}

struct State {
    session: Option<String>,
    tools: Arc<Vec<ToolSpec>>,
    stale: bool,
    retry_at: Option<Instant>,
}

/// The door no longer knows the session.
struct Gone;

/// The result of a JSON-RPC answer, or its error as text.
async fn answer(resp: reqwest::Response) -> Result<Value, String> {
    let status = resp.status();
    if status == StatusCode::UNAUTHORIZED {
        return Err("the box door refused this box (no current door key)".to_owned());
    }
    let sse = resp
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .is_some_and(|v| v.starts_with(EVENT_STREAM));
    let body = resp.bytes().await.map_err(|e| format!("read: {e}"))?;
    if !status.is_success() {
        return Err(format!(
            "the door answered {status}: {}",
            String::from_utf8_lossy(&body)
        ));
    }
    let mut msg: Value = if sse {
        String::from_utf8_lossy(&body)
            .lines()
            .filter_map(|l| l.strip_prefix("data:"))
            .filter_map(|d| serde_json::from_str::<Value>(d.trim()).ok())
            .find(|m| m.get("id").is_some())
            .ok_or("the event stream carried no answer")?
    } else {
        serde_json::from_slice(&body).map_err(|e| format!("decode the answer: {e}"))?
    };
    if let Some(err) = msg.get("error") {
        return Err(err["message"].as_str().unwrap_or("error").to_owned());
    }
    Ok(msg["result"].take())
}

fn approval_answer(data: &str) -> Option<(String, bool)> {
    let msg: Value = serde_json::from_str(data.trim()).ok()?;
    if msg["method"] != ANSWERED {
        return None;
    }
    let params = &msg["params"];
    let id = params["approval_id"].as_str()?.to_owned();
    Some((id, params["decision"].as_str()? != "deny"))
}

fn spec(mut tool: Value) -> Option<ToolSpec> {
    let name = tool["name"].as_str()?.to_owned();
    let description = tool["description"]
        .as_str()
        .or_else(|| tool["title"].as_str())
        .unwrap_or_default()
        .to_owned();
    let parameters = match tool["inputSchema"].take() {
        Value::Null => json!({"type": "object"}),
        schema => schema,
    };
    Some(ToolSpec {
        name: Cow::Owned(name),
        description: Cow::Owned(description),
        parameters,
    })
}

fn output(result: &Value) -> ToolOutput {
    let mut text = String::new();
    let mut image = None;
    for part in result["content"].as_array().into_iter().flatten() {
        match part["type"].as_str() {
            Some("text") => {
                if !text.is_empty() {
                    text.push('\n');
                }
                text.push_str(part["text"].as_str().unwrap_or_default());
            }
            Some("image") if image.is_none() => {
                image = part["data"]
                    .as_str()
                    .and_then(|d| B64.decode(d).ok())
                    .map(Bytes::from);
            }
            _ => {
                if !text.is_empty() {
                    text.push('\n');
                }
                text.push_str(&part.to_string());
            }
        }
    }
    ToolOutput {
        text,
        image,
        is_error: result["isError"].as_bool().unwrap_or(false),
    }
}
