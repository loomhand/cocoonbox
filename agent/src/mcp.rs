//! MCP Streamable HTTP server at `/mcp`: external agents drive the box through the toolhost (mode B).

use std::collections::HashMap;
use std::convert::Infallible;
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{Duration, Instant};

use axum::Router;
use axum::body::{Body, Bytes};
use axum::extract::{DefaultBodyLimit, State};
use axum::http::{HeaderMap, StatusCode, header};
use axum::response::{IntoResponse, Response};
use axum::routing::post;
use serde_json::{Value, json};

use crate::dispatch::{Caller, uses_driver};
use crate::llm::{b64, image_mime};
use crate::pb::{self, AgentStatus, Role, transcript_entry::Body as Entry};
use crate::runner::Runtime;
use crate::server::{EDGE_TOKEN_HEADER, constant_time_eq};
use crate::{new_id, tools};

/// Path of the MCP endpoint on the agent listener.
pub const PATH: &str = "/mcp";

const SESSION_HEADER: &str = "mcp-session-id";
const AGENT_PREFIX: &str = "mcp:";
const PROTOCOLS: [&str; 3] = ["2025-11-25", "2025-06-18", "2025-03-26"];
const FALLBACK_PROTOCOL: &str = "2025-06-18";
const KEEPALIVE: Duration = Duration::from_secs(25);
const MAX_REAP_PERIOD: Duration = Duration::from_secs(60);
const MAX_BODY: usize = 64 << 20;
const INSTRUCTIONS: &str = "These tools act on a cloud computer: Ubuntu 24.04 with an XFCE desktop at 1280x800. \
Relative file paths resolve under /workspace, and shell is one persistent bash session. \
Prefer shell and file tools, then browser_* (pages as ref-tagged element lists), and use computer_state / computer_act only for GUI work. \
Every computer_act returns a fresh screenshot; trust the screenshot, not the action's report. \
The user watches the screen live and sees every call.";

/// MCP sessions of external agents, each shown as an agent `mcp:<client>`.
pub struct Mcp {
    rt: Arc<Runtime>,
    token: Arc<str>,
    tools: Value,
    names: Vec<&'static str>,
    idle: Duration,
    sessions: Mutex<HashMap<String, Arc<Session>>>,
}

impl Mcp {
    /// Creates the server; sessions idle for `idle` are closed.
    pub fn new(rt: Arc<Runtime>, idle: Duration) -> Arc<Self> {
        let specs = tools::remote_tools();
        let names = specs.iter().map(|t| t.name).collect();
        let tools = specs
            .into_iter()
            .map(|t| {
                let mut tool = json!({"name": t.name, "description": t.description});
                tool["inputSchema"] = t.parameters;
                tool
            })
            .collect();
        Arc::new(Self {
            token: Arc::from(rt.cfg.edge_token.as_str()),
            rt,
            tools,
            names,
            idle,
            sessions: Mutex::new(HashMap::new()),
        })
    }

    /// Routes `POST|GET|DELETE /mcp`.
    pub fn router(self: Arc<Self>) -> Router {
        Router::new()
            .route(PATH, post(post_mcp).get(get_mcp).delete(delete_mcp))
            .layer(DefaultBodyLimit::max(MAX_BODY))
            .with_state(self)
    }

    /// Closes idle sessions until the process exits.
    pub fn spawn_reaper(self: &Arc<Self>) {
        let mcp = Arc::clone(self);
        let period = (self.idle / 2).min(MAX_REAP_PERIOD);
        // detached: runs for the life of the process
        tokio::spawn(async move {
            let mut tick = tokio::time::interval(period);
            loop {
                tick.tick().await;
                let idle: Vec<Arc<Session>> = {
                    let mut sessions = lock(&mcp.sessions);
                    let ids: Vec<String> = sessions
                        .iter()
                        .filter(|(_, s)| lock(&s.driver).last_seen.elapsed() >= mcp.idle)
                        .map(|(id, _)| id.clone())
                        .collect();
                    ids.iter().filter_map(|id| sessions.remove(id)).collect()
                };
                for s in idle {
                    mcp.close(&s, "idle").await;
                }
                mcp.prune().await;
            }
        });
    }

    /// Deletes closed `mcp:*` agents untouched for longer than the prune age.
    async fn prune(&self) {
        let age = i64::try_from(self.rt.cfg.mcp_prune_after.as_millis()).unwrap_or(i64::MAX);
        let before = crate::now_ms().saturating_sub(age);
        match self
            .rt
            .journal
            .store
            .prune_agents(AGENT_PREFIX.to_owned(), before)
            .await
        {
            Ok(ids) if !ids.is_empty() => eprintln!("mcp: pruned {} idle agent(s)", ids.len()),
            Ok(_) => {}
            Err(e) => eprintln!("mcp: prune: {e}"),
        }
    }

    /// Closes every session; used when the agent shuts down.
    pub async fn close_all(&self) {
        let sessions: Vec<Arc<Session>> = lock(&self.sessions).drain().map(|(_, s)| s).collect();
        for s in sessions {
            self.close(&s, "agent shutting down").await;
        }
    }

    fn authorized(&self, headers: &HeaderMap) -> bool {
        let got = headers
            .get(EDGE_TOKEN_HEADER)
            .map(|v| v.as_bytes())
            .unwrap_or_default();
        constant_time_eq(got, self.token.as_bytes())
    }

    fn session(
        &self,
        headers: &HeaderMap,
    ) -> Result<Arc<Session>, (StatusCode, i64, &'static str)> {
        let Some(id) = headers.get(SESSION_HEADER).and_then(|v| v.to_str().ok()) else {
            return Err((StatusCode::BAD_REQUEST, -32000, "missing Mcp-Session-Id"));
        };
        let session = lock(&self.sessions).get(id).cloned();
        let session =
            session.ok_or((StatusCode::NOT_FOUND, -32001, "unknown or expired session"))?;
        lock(&session.driver).last_seen = Instant::now();
        Ok(session)
    }

    async fn open(&self, params: &Value) -> Result<(Arc<Session>, Value), String> {
        let client = params["clientInfo"]["name"].as_str().unwrap_or("client");
        let version = params["clientInfo"]["version"].as_str().unwrap_or_default();
        let agent = self
            .rt
            .create_agent(pb::CreateAgentRequest {
                name: format!("{AGENT_PREFIX}{client}"),
                main_model: "external".to_owned(),
                client_nonce: String::new(),
            })
            .await
            .map_err(|e| e.message().to_owned())?;
        let turn_id = self.rt.journal.store.open_turn(agent.id.clone()).await?;
        let session = Arc::new(Session {
            id: new_id("mcp"),
            agent_id: agent.id,
            turn_id,
            driver: Mutex::new(Driver::default()),
        });
        let note = format!("MCP session opened by {client} {version}");
        self.note(&session, note.trim_end()).await;
        self.rt
            .journal
            .set_status(&session.agent_id, AgentStatus::Running)
            .await;
        lock(&self.sessions).insert(session.id.clone(), Arc::clone(&session));
        let requested = params["protocolVersion"].as_str().unwrap_or_default();
        let protocol = PROTOCOLS
            .iter()
            .find(|p| **p == requested)
            .copied()
            .unwrap_or(FALLBACK_PROTOCOL);
        let result = json!({
            "protocolVersion": protocol,
            "capabilities": {"tools": {"listChanged": false}},
            "serverInfo": {"name": "cocoonbox-agent", "version": env!("CARGO_PKG_VERSION")},
            "instructions": INSTRUCTIONS,
        });
        Ok((session, result))
    }

    async fn close(&self, s: &Session, reason: &str) {
        self.end_driver(s).await;
        self.note(s, &format!("MCP session closed: {reason}")).await;
        let end = Entry::TurnEnd(pb::TurnEnd {
            ok: true,
            error: String::new(),
            usage: None,
        });
        self.record(s, Role::System, end).await;
        if let Err(e) = self.rt.journal.store.finish_turn(s.turn_id.clone()).await {
            eprintln!("mcp: finish turn {}: {e}", s.turn_id);
        }
        self.rt
            .journal
            .set_status(&s.agent_id, AgentStatus::Idle)
            .await;
    }

    async fn end_driver(&self, s: &Session) {
        let label = {
            let mut d = lock(&s.driver);
            if !d.used {
                return;
            }
            d.used = false;
            std::mem::replace(&mut d.label, new_id("cu"))
        };
        let out = self.rt.dispatcher.end_driver(&s.agent_id, &label).await;
        if out.is_error {
            eprintln!("mcp: end driver session {label}: {}", out.text);
        }
    }

    async fn dispatch(&self, s: Option<&Session>, msg: &Value) -> Option<Value> {
        let id = msg.get("id")?.clone();
        let method = msg["method"].as_str()?;
        let result = match (method, s) {
            ("ping", _) => Ok(json!({})),
            ("tools/list", Some(_)) => Ok(json!({"tools": self.tools})),
            ("tools/call", Some(s)) => self.call_tool(s, &msg["params"]).await,
            ("initialize", _) => Err((-32600, "initialize must be the only message".to_owned())),
            _ => Err((-32601, format!("method not found: {method}"))),
        };
        Some(match result {
            Ok(result) => json!({"jsonrpc": "2.0", "id": id, "result": result}),
            Err((code, message)) => {
                json!({"jsonrpc": "2.0", "id": id, "error": {"code": code, "message": message}})
            }
        })
    }

    async fn call_tool(&self, s: &Session, params: &Value) -> Result<Value, (i64, String)> {
        let name = params["name"].as_str().unwrap_or_default();
        let Some(&name) = self.names.iter().find(|n| **n == name) else {
            return Err((-32602, format!("unknown tool: {name}")));
        };
        let requested = match &params["arguments"] {
            Value::Null => json!({}),
            v @ Value::Object(_) => v.clone(),
            _ => return Err((-32602, "arguments must be an object".to_owned())),
        };
        let label = {
            let mut d = lock(&s.driver);
            d.used |= uses_driver(name);
            d.label.clone()
        };
        let caller = Caller {
            agent_id: &s.agent_id,
            turn_id: &s.turn_id,
            subagent: "",
            driver: Some(&label),
        };
        let out = self
            .rt
            .dispatcher
            .call(&caller, &new_id("call"), name, requested)
            .await
            .map_err(|e| (-32603, e))?;
        lock(&s.driver).last_seen = Instant::now();
        let mut content = vec![json!({"type": "text", "text": out.text})];
        if let Some(img) = &out.image {
            let mut part = json!({"type": "image", "mimeType": image_mime(img)});
            part["data"] = b64(img).into();
            content.push(part);
        }
        let mut result = json!({"isError": out.is_error});
        result["content"] = Value::Array(content);
        Ok(result)
    }

    async fn note(&self, s: &Session, text: &str) {
        let body = Entry::Text(pb::TextBlock {
            text: text.to_owned(),
            ..Default::default()
        });
        self.record(s, Role::System, body).await;
    }

    async fn record(&self, s: &Session, role: Role, body: Entry) {
        if let Err(e) = self
            .rt
            .journal
            .record(&s.agent_id, &s.turn_id, role, body, false)
            .await
        {
            eprintln!("mcp: record for {}: {e}", s.agent_id);
        }
    }
}

struct Session {
    id: String,
    agent_id: String,
    turn_id: String,
    driver: Mutex<Driver>,
}

/// The cua-driver session behind an MCP session; the label rotates each time it is ended.
struct Driver {
    label: String,
    used: bool,
    last_seen: Instant,
}

impl Default for Driver {
    fn default() -> Self {
        Self {
            label: new_id("cu"),
            used: false,
            last_seen: Instant::now(),
        }
    }
}

/// Ends the driver session when the client's GET stream closes.
struct StreamGuard {
    mcp: Arc<Mcp>,
    session: Arc<Session>,
}

impl Drop for StreamGuard {
    fn drop(&mut self) {
        let mcp = Arc::clone(&self.mcp);
        let session = Arc::clone(&self.session);
        tokio::spawn(async move { mcp.end_driver(&session).await });
    }
}

async fn post_mcp(State(mcp): State<Arc<Mcp>>, headers: HeaderMap, body: Bytes) -> Response {
    if !mcp.authorized(&headers) {
        return StatusCode::UNAUTHORIZED.into_response();
    }
    let Ok(msg) = serde_json::from_slice::<Value>(&body) else {
        return rpc_failure(StatusCode::BAD_REQUEST, -32700, "parse error");
    };
    if msg["method"] == "initialize" {
        let id = msg.get("id").cloned().unwrap_or(Value::Null);
        return match mcp.open(&msg["params"]).await {
            Ok((session, result)) => {
                let reply = json!({"jsonrpc": "2.0", "id": id, "result": result});
                let mut resp = json_response(StatusCode::OK, &reply);
                if let Ok(v) = session.id.parse() {
                    resp.headers_mut().insert(SESSION_HEADER, v);
                }
                resp
            }
            Err(e) => rpc_failure(StatusCode::INTERNAL_SERVER_ERROR, -32603, &e),
        };
    }
    let (batch, messages) = match msg {
        Value::Array(items) => (true, items),
        single => (false, vec![single]),
    };
    let sessionless = !headers.contains_key(SESSION_HEADER)
        && !messages.iter().any(|m| {
            m["method"]
                .as_str()
                .is_some_and(|m| m.starts_with("tools/"))
        });
    let session = if sessionless {
        None
    } else {
        match mcp.session(&headers) {
            Ok(s) => Some(s),
            Err((status, code, message)) => return rpc_failure(status, code, message),
        }
    };
    let mut replies = Vec::new();
    for m in &messages {
        if let Some(reply) = mcp.dispatch(session.as_deref(), m).await {
            replies.push(reply);
        }
    }
    match (batch, replies.len()) {
        (_, 0) => StatusCode::ACCEPTED.into_response(),
        (false, _) => json_response(StatusCode::OK, &replies[0]),
        (true, _) => json_response(StatusCode::OK, &Value::Array(replies)),
    }
}

async fn get_mcp(State(mcp): State<Arc<Mcp>>, headers: HeaderMap) -> Response {
    if !mcp.authorized(&headers) {
        return StatusCode::UNAUTHORIZED.into_response();
    }
    let session = match mcp.session(&headers) {
        Ok(s) => s,
        Err((status, code, message)) => return rpc_failure(status, code, message),
    };
    let guard = StreamGuard { mcp, session };
    let stream = futures_util::stream::unfold((guard, true), |(guard, first)| async move {
        if !first {
            tokio::select! {
                () = tokio::time::sleep(KEEPALIVE) => {}
                () = guard.mcp.rt.stop.cancelled() => return None,
            }
            lock(&guard.session.driver).last_seen = Instant::now();
        }
        let chunk: Result<Bytes, Infallible> = Ok(Bytes::from_static(b": keepalive\n\n"));
        Some((chunk, (guard, false)))
    });
    let mut resp = Response::new(Body::from_stream(stream));
    let h = resp.headers_mut();
    h.insert(
        header::CONTENT_TYPE,
        header::HeaderValue::from_static("text/event-stream"),
    );
    h.insert(
        header::CACHE_CONTROL,
        header::HeaderValue::from_static("no-cache"),
    );
    resp
}

async fn delete_mcp(State(mcp): State<Arc<Mcp>>, headers: HeaderMap) -> Response {
    if !mcp.authorized(&headers) {
        return StatusCode::UNAUTHORIZED.into_response();
    }
    let removed = headers
        .get(SESSION_HEADER)
        .and_then(|v| v.to_str().ok())
        .and_then(|id| lock(&mcp.sessions).remove(id));
    match removed {
        Some(s) => {
            mcp.close(&s, "deleted by the client").await;
            StatusCode::NO_CONTENT.into_response()
        }
        None => StatusCode::NOT_FOUND.into_response(),
    }
}

fn json_response(status: StatusCode, body: &Value) -> Response {
    let mut resp = (status, body.to_string()).into_response();
    resp.headers_mut().insert(
        header::CONTENT_TYPE,
        header::HeaderValue::from_static("application/json"),
    );
    resp
}

fn rpc_failure(status: StatusCode, code: i64, message: &str) -> Response {
    let body = json!({"jsonrpc": "2.0", "id": null, "error": {"code": code, "message": message}});
    json_response(status, &body)
}

fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
}
