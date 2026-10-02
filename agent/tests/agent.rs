#![cfg(test)]

use std::collections::{HashMap, VecDeque};
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use base64::Engine;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use cocoonbox_agent::config::{ServeConfig, ToolhostConfig};
use cocoonbox_agent::pb::agents_client::AgentsClient;
use cocoonbox_agent::pb::session_client::SessionClient;
use cocoonbox_agent::pb::{self, event, screen_client_frame, transcript_entry::Body};
use cocoonbox_agent::runner::Runtime;
use cocoonbox_agent::toolhost::state::Root;
use cocoonbox_agent::{server, toolhost};
use ed25519_dalek::{Signer, SigningKey};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::{TcpListener, UnixListener};
use tonic::metadata::MetadataValue;
use tonic::transport::Channel;
use tonic::{Code, Request};

const TOKEN: &str = "edge-token-for-tests";
const PNG: &str = "iVBORw0KGgoAAAANSUhEUg==";

type Log = Arc<Mutex<Vec<(String, Value)>>>;

struct Harness {
    _dir: tempfile::TempDir,
    identity: PathBuf,
    workspace: PathBuf,
    home: PathBuf,
    endpoint: String,
    gw_log: Log,
    cua_log: Log,
    session_save: PathBuf,
}

impl Harness {
    async fn start(script: Vec<(&'static str, String)>, anthropic_messages: bool) -> Self {
        let day = Duration::from_secs(24 * 3600);
        Self::start_with(script, anthropic_messages, Duration::from_secs(600), day).await
    }

    async fn start_with(
        script: Vec<(&'static str, String)>,
        anthropic_messages: bool,
        mcp_idle: Duration,
        mcp_prune_after: Duration,
    ) -> Self {
        let dir = tempfile::tempdir().unwrap();
        let workspace = dir.path().join("workspace");
        std::fs::create_dir_all(&workspace).unwrap();
        let home = dir.path().join("home");
        std::fs::create_dir_all(&home).unwrap();
        let identity = dir.path().join("identity").join("identity");
        let gw_log = Log::default();
        let gw = mock_gw(script, Arc::clone(&gw_log)).await;
        let cua_log = Log::default();
        let cua_socket = dir.path().join("cua.sock");
        fake_cua(&cua_socket, Arc::clone(&cua_log));
        let session_save = dir.path().join("session-save");
        write_script(&session_save, "echo saved >> \"$0.log\"");
        let toolhost_socket = dir.path().join("toolhost.sock");
        let th_cfg = ToolhostConfig {
            socket: toolhost_socket.clone(),
            socket_group: "no-such-group".into(),
            workspace: workspace.clone(),
            chrome_bin: "false".into(),
            chrome_profile: dir.path().join("chrome"),
            cdp_port: 1,
            cua_socket,
            session_save: session_save.clone(),
        };
        tokio::spawn(async move { toolhost::run(th_cfg).await });
        let cfg = ServeConfig {
            gw_base_url: format!("http://{gw}/v1"),
            gw_api_key: "k".into(),
            model_main: "deepseek/deepseek-v4.1-flash".into(),
            model_computer: "anthropic/claude-sonnet-5".into(),
            edge_token: Some(TOKEN.into()),
            identity_path: identity.clone(),
            identity_owner: std::os::unix::fs::MetadataExt::uid(&dir.path().metadata().unwrap()),
            listen: "127.0.0.1:0".parse().unwrap(),
            state_dir: dir.path().join("state"),
            toolhost_socket: toolhost_socket.clone(),
            vnc_addr: fake_vnc().await,
            anthropic_messages,
            mcp_idle,
            mcp_prune_after,
            desktop_unit: None,
            state_roots: vec![
                Root {
                    name: "home".into(),
                    path: home.clone(),
                },
                Root {
                    name: "workspace".into(),
                    path: workspace.clone(),
                },
            ],
        };
        let model = Arc::new(
            cocoonbox_agent::llm::Client::new(
                &cfg.gw_base_url,
                &cfg.gw_api_key,
                anthropic_messages,
            )
            .unwrap(),
        );
        let tools = Arc::new(cocoonbox_agent::toolhost::rpc::Client::new(
            cfg.toolhost_socket.clone(),
        ));
        let rt = Runtime::open(cfg, model, tools).await.unwrap();
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let endpoint = format!("http://{}", listener.local_addr().unwrap());
        tokio::spawn(server::serve_on(listener, rt));
        for _ in 0..100 {
            if toolhost_socket.exists() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        Self {
            _dir: dir,
            identity,
            workspace,
            home,
            endpoint,
            gw_log,
            cua_log,
            session_save,
        }
    }

    async fn channel(&self) -> Channel {
        Channel::from_shared(self.endpoint.clone())
            .unwrap()
            .connect()
            .await
            .unwrap()
    }

    async fn agents(&self) -> AgentsClient<Channel> {
        AgentsClient::new(self.channel().await)
    }

    async fn session(&self) -> SessionClient<Channel> {
        SessionClient::new(self.channel().await)
    }
}

fn authed<T>(msg: T) -> Request<T> {
    let mut req = Request::new(msg);
    req.metadata_mut()
        .insert("x-cocoonbox-edge-token", MetadataValue::from_static(TOKEN));
    req
}

async fn create_agent(agents: &mut AgentsClient<Channel>) -> String {
    agents
        .create(authed(pb::CreateAgentRequest {
            name: "t".into(),
            ..Default::default()
        }))
        .await
        .unwrap()
        .into_inner()
        .id
}

async fn send(
    agents: &mut AgentsClient<Channel>,
    agent_id: &str,
    text: &str,
    nonce: &str,
) -> String {
    agents
        .send(authed(pb::SendRequest {
            agent_id: agent_id.into(),
            text: text.into(),
            client_nonce: nonce.into(),
            ..Default::default()
        }))
        .await
        .unwrap()
        .into_inner()
        .turn_id
}

async fn wait_turn_end(
    agents: &mut AgentsClient<Channel>,
    agent_id: &str,
) -> Vec<pb::TranscriptEntry> {
    for _ in 0..500 {
        let page = agents
            .transcript(authed(pb::TranscriptRequest {
                agent_id: agent_id.into(),
                after_seq: 0,
                limit: 1000,
            }))
            .await
            .unwrap()
            .into_inner();
        if page
            .entries
            .iter()
            .any(|e| matches!(e.body, Some(Body::TurnEnd(_))))
        {
            return page.entries;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    panic!("turn did not end");
}

fn chat_tool_call(id: &str, name: &str, args: &Value) -> String {
    let args = args.to_string();
    let (a, b) = args.split_at(args.len() / 2);
    sse(&[
        json!({"choices":[{"delta":{"content":"Let me check."}}]}),
        json!({"choices":[{"delta":{"tool_calls":[{"index":0,"id":id,"type":"function","function":{"name":name,"arguments":a}}]}}]}),
        json!({"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":b}}]},"finish_reason":"tool_calls"}]}),
        json!({"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10}}),
    ]) + "data: [DONE]\n\n"
}

fn chat_text(text: &str) -> String {
    sse(&[
        json!({"choices":[{"delta":{"content":text},"finish_reason":"stop"}]}),
        json!({"choices":[],"usage":{"prompt_tokens":150,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":100}}}),
    ]) + "data: [DONE]\n\n"
}

fn messages_tool_use(id: &str, name: &str, input: &Value) -> String {
    sse(&[
        json!({"type":"message_start","message":{"usage":{"input_tokens":50,"output_tokens":1}}}),
        json!({"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":id,"name":name,"input":{}}}),
        json!({"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":input.to_string()}}),
        json!({"type":"content_block_stop","index":0}),
        json!({"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}),
        json!({"type":"message_stop"}),
    ])
}

fn messages_text(text: &str) -> String {
    sse(&[
        json!({"type":"message_start","message":{"usage":{"input_tokens":60,"output_tokens":1}}}),
        json!({"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}),
        json!({"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":text}}),
        json!({"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}),
        json!({"type":"message_stop"}),
    ])
}

fn sse(events: &[Value]) -> String {
    events.iter().map(|e| format!("data: {e}\n\n")).collect()
}

async fn mock_gw(script: Vec<(&'static str, String)>, log: Log) -> String {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap().to_string();
    let mut queues: HashMap<String, VecDeque<String>> = HashMap::new();
    for (path, body) in script {
        queues
            .entry(format!("/v1/{path}"))
            .or_default()
            .push_back(body);
    }
    let queues = Arc::new(Mutex::new(queues));
    tokio::spawn(async move {
        loop {
            let (stream, _) = listener.accept().await.unwrap();
            let queues = Arc::clone(&queues);
            let log = Arc::clone(&log);
            tokio::spawn(async move {
                let mut reader = BufReader::new(stream);
                let mut line = String::new();
                reader.read_line(&mut line).await.unwrap();
                let path = line
                    .split_whitespace()
                    .nth(1)
                    .unwrap_or_default()
                    .to_owned();
                let mut len = 0;
                loop {
                    let mut h = String::new();
                    reader.read_line(&mut h).await.unwrap();
                    if h == "\r\n" {
                        break;
                    }
                    if let Some(v) = h.to_ascii_lowercase().strip_prefix("content-length:") {
                        len = v.trim().parse().unwrap();
                    }
                }
                let mut body = vec![0u8; len];
                reader.read_exact(&mut body).await.unwrap();
                log.lock()
                    .unwrap()
                    .push((path.clone(), serde_json::from_slice(&body).unwrap()));
                let next = queues
                    .lock()
                    .unwrap()
                    .get_mut(&path)
                    .and_then(VecDeque::pop_front);
                let mut stream = reader.into_inner();
                let resp = match next {
                    Some(sse) => format!(
                        "HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\nconnection: close\r\n\r\n{sse}"
                    ),
                    None => "HTTP/1.1 500 Internal Server Error\r\ncontent-length: 11\r\nconnection: close\r\n\r\nno script\n".to_owned(),
                };
                stream.write_all(resp.as_bytes()).await.unwrap();
                stream.shutdown().await.unwrap();
            });
        }
    });
    addr
}

fn fake_cua(path: &Path, log: Log) {
    let listener = UnixListener::bind(path).unwrap();
    tokio::spawn(async move {
        loop {
            let (stream, _) = listener.accept().await.unwrap();
            let log = Arc::clone(&log);
            tokio::spawn(async move {
                let mut reader = BufReader::new(stream);
                let mut line = String::new();
                reader.read_line(&mut line).await.unwrap();
                let req: Value = serde_json::from_str(&line).unwrap();
                let name = req["name"].as_str().unwrap().to_owned();
                log.lock()
                    .unwrap()
                    .push((name.clone(), req["args"].clone()));
                let result = match name.as_str() {
                    "get_window_state" => json!({
                        "content": [{"type": "text", "text": "tree"}],
                        "structuredContent": {"window_id": 7, "window_bounds": {"x": 100, "y": 50, "width": 800, "height": 600}, "elements": [{"element_index": 0, "element_token": "tok-0", "role": "text", "label": "editor", "depth": 0, "frame": {"x": 10, "y": 20, "w": 100, "h": 40}}]}
                    }),
                    "get_desktop_state" => json!({
                        "content": [{"type": "image", "mimeType": "image/png", "data": PNG}],
                        "structuredContent": {"windows": fake_windows()}
                    }),
                    "list_windows" => json!({"structuredContent": {"windows": fake_windows()}}),
                    _ => json!({"content": [{"type": "text", "text": format!("✅ {name} done")}]}),
                };
                let reply = json!({"ok": true, "result": result}).to_string() + "\n";
                let mut stream = reader.into_inner();
                stream.write_all(reply.as_bytes()).await.unwrap();
            });
        }
    });
}

fn fake_windows() -> Value {
    json!([
        {"pid": 1, "window_id": 2, "app_name": "Xfdesktop", "title": "Desktop", "bounds": {"x": 0, "y": 0, "width": 1280, "height": 800}, "z_index": 0},
        {"pid": 42, "window_id": 7, "app_name": "Mousepad", "title": "Untitled", "bounds": {"x": 100, "y": 50, "width": 800, "height": 600}, "z_index": 3},
        {"pid": 5, "window_id": 9, "app_name": "Xfce4-panel", "title": "hidden", "bounds": {"x": -9999, "y": -9999, "width": 3, "height": 3}, "z_index": 4},
        {"pid": 5, "window_id": 10, "app_name": "Xfce4-panel", "title": "panel", "bounds": {"x": 0, "y": 0, "width": 1280, "height": 27}, "z_index": 5}
    ])
}

async fn fake_vnc() -> String {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap().to_string();
    tokio::spawn(async move {
        loop {
            let (mut stream, _) = listener.accept().await.unwrap();
            tokio::spawn(async move {
                stream.write_all(b"RFB 003.008\n").await.unwrap();
                let mut buf = [0u8; 64];
                while let Ok(n) = stream.read(&mut buf).await {
                    if n == 0 || stream.write_all(&buf[..n]).await.is_err() {
                        break;
                    }
                }
            });
        }
    });
    addr
}

fn gw_bodies(log: &Log, path: &str) -> Vec<Value> {
    log.lock()
        .unwrap()
        .iter()
        .filter(|(p, _)| p.ends_with(path))
        .map(|(_, b)| b.clone())
        .collect()
}

fn alive(pid_file: &Path) -> bool {
    let pid = std::fs::read_to_string(pid_file).unwrap();
    let out = std::process::Command::new("ps")
        .args(["-o", "stat=", "-p", pid.trim()])
        .output()
        .unwrap();
    let stat = String::from_utf8_lossy(&out.stdout);
    !stat.trim().is_empty() && !stat.trim_start().starts_with('Z')
}

async fn wait_gone(pid_file: &Path) -> bool {
    for _ in 0..100 {
        if !alive(pid_file) {
            return true;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    false
}

fn write_identity(path: &Path, key: &SigningKey, wake_id: &str) -> String {
    let public_key = key.verifying_key().to_bytes();
    let key_id = hex::encode(Sha256::digest(public_key))[..16].to_owned();
    let dir = path.parent().unwrap();
    std::fs::create_dir_all(dir).unwrap();
    std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o755)).unwrap();
    let identity = json!({
        "box_id": "cb-box",
        "wake_id": wake_id,
        "key_id": key_id,
        "public_key": URL_SAFE_NO_PAD.encode(public_key),
    });
    std::fs::write(dir.join("tmp"), identity.to_string()).unwrap();
    std::fs::set_permissions(dir.join("tmp"), std::fs::Permissions::from_mode(0o644)).unwrap();
    std::fs::rename(dir.join("tmp"), path).unwrap();
    key_id
}

fn credential(key: &SigningKey, key_id: &str, box_id: &str, wake_id: &str) -> String {
    let exp = cocoonbox_agent::now_ms() / 1000 + 600;
    let signed = format!(
        "v1.{}",
        URL_SAFE_NO_PAD.encode(format!("{key_id}|{box_id}|{wake_id}|{exp}"))
    );
    let sig = URL_SAFE_NO_PAD.encode(key.sign(signed.as_bytes()).to_bytes());
    format!("{signed}.{sig}")
}

async fn surface_codes(h: &Harness, credential: Option<&str>, token: Option<&str>) -> Vec<String> {
    fn with<T>(msg: T, credential: Option<&str>, token: Option<&str>) -> Request<T> {
        let mut req = Request::new(msg);
        if let Some(c) = credential {
            req.metadata_mut()
                .insert("x-cocoonbox-credential", c.parse().unwrap());
        }
        if let Some(t) = token {
            req.metadata_mut()
                .insert("x-cocoonbox-edge-token", t.parse().unwrap());
        }
        req
    }
    fn code<T>(r: Result<T, tonic::Status>) -> String {
        r.map_or_else(|e| format!("{:?}", e.code()), |_| "Ok".into())
    }
    let agents = h
        .agents()
        .await
        .list(with(pb::ListAgentsRequest {}, credential, token))
        .await;
    let hello = h
        .session()
        .await
        .hello(with(pb::HelloRequest::default(), credential, token))
        .await;
    let plan = state_client(h)
        .await
        .plan(with(pb::PlanRequest::default(), credential, token))
        .await;
    let mut mcp = reqwest::Client::new()
        .post(format!("{}/mcp", h.endpoint))
        .json(&json!({"jsonrpc": "2.0", "id": 1, "method": "ping"}));
    if let Some(c) = credential {
        mcp = mcp.header("x-cocoonbox-credential", c);
    }
    if let Some(t) = token {
        mcp = mcp.header("x-cocoonbox-edge-token", t);
    }
    let mcp = mcp.send().await.unwrap().status().as_u16();
    vec![code(agents), code(hello), code(plan), mcp.to_string()]
}

#[tokio::test]
async fn credential_guards_every_surface() {
    let h = Harness::start(Vec::new(), false).await;
    let admitted = ["Ok", "Ok", "InvalidArgument", "200"];
    let refused = [
        "Unauthenticated",
        "Unauthenticated",
        "Unauthenticated",
        "401",
    ];
    let unavailable = ["Unavailable", "Unavailable", "Unavailable", "503"];
    assert_eq!(surface_codes(&h, None, Some(TOKEN)).await, admitted);

    let key = SigningKey::from_bytes(&[5; 32]);
    let key_id = write_identity(&h.identity, &key, "w1");
    let good = credential(&key, &key_id, "cb-box", "w1");
    assert_eq!(surface_codes(&h, Some(&good), None).await, admitted);
    assert_eq!(surface_codes(&h, None, Some(TOKEN)).await, refused);
    let other_box = credential(&key, &key_id, "cb-other", "w1");
    assert_eq!(surface_codes(&h, Some(&other_box), None).await, refused);
    let forged = credential(&SigningKey::from_bytes(&[6; 32]), &key_id, "cb-box", "w1");
    assert_eq!(surface_codes(&h, Some(&forged), Some(TOKEN)).await, refused);

    write_identity(&h.identity, &key, "w2");
    let woken = credential(&key, &key_id, "cb-box", "w2");
    assert_eq!(surface_codes(&h, Some(&woken), None).await, admitted);
    assert_eq!(surface_codes(&h, Some(&good), None).await, refused);

    let attacker = SigningKey::from_bytes(&[8; 32]);
    let swapped = write_identity(&h.identity, &attacker, "w2");
    std::fs::set_permissions(&h.identity, std::fs::Permissions::from_mode(0o666)).unwrap();
    let self_signed = credential(&attacker, &swapped, "cb-box", "w2");
    assert_eq!(
        surface_codes(&h, Some(&self_signed), Some(TOKEN)).await,
        unavailable
    );
}

#[tokio::test]
async fn edge_token_is_required() {
    let h = Harness::start(Vec::new(), false).await;
    let mut agents = h.agents().await;
    let err = agents
        .list(Request::new(pb::ListAgentsRequest {}))
        .await
        .unwrap_err();
    assert_eq!(err.code(), Code::Unauthenticated);
    let mut wrong = Request::new(pb::ListAgentsRequest {});
    wrong.metadata_mut().insert(
        "x-cocoonbox-edge-token",
        MetadataValue::from_static("edge-token-for-testX"),
    );
    assert_eq!(
        agents.list(wrong).await.unwrap_err().code(),
        Code::Unauthenticated
    );
    let mut session = h.session().await;
    let err = session
        .hello(Request::new(pb::HelloRequest::default()))
        .await
        .unwrap_err();
    assert_eq!(err.code(), Code::Unauthenticated);
    let hello = session
        .hello(authed(pb::HelloRequest::default()))
        .await
        .unwrap()
        .into_inner();
    assert_eq!((hello.screen_width, hello.screen_height), (1280, 800));
    assert_eq!(hello.default_main_model, "deepseek/deepseek-v4.1-flash");
    assert_eq!(hello.models.len(), 2);
    assert!(agents.list(authed(pb::ListAgentsRequest {})).await.is_ok());
}

#[tokio::test]
async fn shell_tool_loop_streams_into_transcript_and_events() {
    let h = Harness::start(
        vec![
            (
                "chat/completions",
                chat_tool_call(
                    "call_1",
                    "shell",
                    &json!({"command": "uname > ok.txt && cat ok.txt"}),
                ),
            ),
            ("chat/completions", chat_text("Wrote ok.txt.")),
        ],
        false,
    )
    .await;
    let mut agents = h.agents().await;
    let mut session = h.session().await;
    let agent_id = create_agent(&mut agents).await;
    let mut events = session
        .events(authed(pb::EventsRequest { from_offset: 0 }))
        .await
        .unwrap()
        .into_inner();
    let turn = send(&mut agents, &agent_id, "run uname", "nonce-1").await;
    let again = send(&mut agents, &agent_id, "run uname", "nonce-1").await;
    assert_eq!(turn, again);
    let entries = wait_turn_end(&mut agents, &agent_id).await;

    let bodies: Vec<&Body> = entries.iter().filter_map(|e| e.body.as_ref()).collect();
    assert!(matches!(bodies[0], Body::Text(t) if t.text == "run uname"));
    assert!(matches!(bodies[1], Body::Text(t) if t.text == "Let me check."));
    let Body::ToolCall(call) = bodies[2] else {
        panic!("want tool call, got {:?}", bodies[2]);
    };
    assert_eq!(
        (call.call_id.as_str(), call.name.as_str()),
        ("call_1", "shell")
    );
    let Body::ToolResult(result) = bodies[3] else {
        panic!("want tool result, got {:?}", bodies[3]);
    };
    assert!(!result.is_error, "{}", result.text);
    assert!(result.text.ends_with("[exit status 0]"), "{}", result.text);
    assert!(matches!(bodies[4], Body::Text(t) if t.text == "Wrote ok.txt."));
    let Body::TurnEnd(end) = bodies[5] else {
        panic!("want turn end, got {:?}", bodies[5]);
    };
    assert!(end.ok, "{}", end.error);
    let usage = end.usage.unwrap();
    assert_eq!(
        (
            usage.input_tokens,
            usage.output_tokens,
            usage.cached_input_tokens
        ),
        (250, 15, 100)
    );
    assert_eq!(bodies.len(), 6);
    let seqs: Vec<u64> = entries.iter().map(|e| e.seq).collect();
    assert!(seqs.windows(2).all(|w| w[0] < w[1]), "{seqs:?}");
    assert!(h.workspace.join("ok.txt").exists());

    let chats = gw_bodies(&h.gw_log, "chat/completions");
    assert_eq!(chats.len(), 2);
    let msgs = chats[1]["messages"].as_array().unwrap();
    assert!(msgs[0]["content"].as_str().unwrap().starts_with(
        "You are deepseek/deepseek-v4.1-flash, served through the cocoonbox gateway."
    ));
    let tool_msg = msgs.iter().find(|m| m["role"] == "tool").unwrap();
    assert_eq!(tool_msg["tool_call_id"], "call_1");
    assert_eq!(msgs.iter().filter(|m| m["role"] == "user").count(), 1);

    let mut offsets = Vec::new();
    let mut stored = Vec::new();
    let mut transcript_seqs = Vec::new();
    let mut deltas = Vec::new();
    while transcript_seqs.len() < entries.len() {
        let ev = tokio::time::timeout(Duration::from_secs(5), events.message())
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        offsets.push(ev.offset);
        match ev.body {
            Some(event::Body::TextDelta(d)) => {
                assert_eq!(
                    (d.agent_id.as_str(), d.turn_id.as_str()),
                    (agent_id.as_str(), turn.as_str())
                );
                deltas.push((d.block, d.text));
            }
            Some(event::Body::Transcript(t)) => {
                transcript_seqs.push(t.seq);
                stored.push(ev.offset);
            }
            _ => stored.push(ev.offset),
        }
    }
    assert_eq!(transcript_seqs, seqs);
    assert!(offsets.windows(2).all(|w| w[1] == w[0] + 1), "{offsets:?}");
    assert_eq!(
        deltas,
        [
            (1, "Let me check.".to_owned()),
            (2, "Wrote ok.txt.".to_owned())
        ]
    );
    let blocks: Vec<(u32, &str)> = bodies
        .iter()
        .filter_map(|b| match b {
            Body::Text(t) if !t.text.starts_with("run") => Some((t.block, t.text.as_str())),
            _ => None,
        })
        .collect();
    assert_eq!(blocks, [(1, "Let me check."), (2, "Wrote ok.txt.")]);
    assert!(
        !chats[0]["tools"]
            .as_array()
            .unwrap()
            .iter()
            .any(|t| t["function"]["name"] == "screenshot")
    );

    let mut replay = session
        .events(authed(pb::EventsRequest {
            from_offset: stored[1],
        }))
        .await
        .unwrap()
        .into_inner();
    let mut replayed = Vec::new();
    for _ in 2..stored.len() {
        replayed.push(replay.message().await.unwrap().unwrap().offset);
    }
    assert_eq!(replayed, stored[2..]);
    let stale = session
        .events(authed(pb::EventsRequest { from_offset: 3 }))
        .await
        .unwrap_err();
    assert_eq!(stale.code(), Code::OutOfRange);
}

#[tokio::test]
async fn delegate_over_chat_wire() {
    run_delegate(false).await;
}

#[tokio::test]
async fn delegate_over_messages_wire() {
    run_delegate(true).await;
}

async fn run_delegate(messages_wire: bool) {
    let state_args = json!({});
    let act_args = json!({"action": "type", "element": 0, "text": "hello"});
    let key_args = json!({"action": "key", "key": "return"});
    let sub = if messages_wire {
        vec![
            (
                "messages",
                messages_tool_use("toolu_1", "computer_state", &state_args),
            ),
            (
                "messages",
                messages_tool_use("toolu_2", "computer_act", &act_args),
            ),
            (
                "messages",
                messages_tool_use("toolu_3", "computer_act", &key_args),
            ),
            ("messages", messages_text("Typed hello into Mousepad.")),
        ]
    } else {
        vec![
            (
                "chat/completions",
                chat_tool_call("toolu_1", "computer_state", &state_args),
            ),
            (
                "chat/completions",
                chat_tool_call("toolu_2", "computer_act", &act_args),
            ),
            (
                "chat/completions",
                chat_tool_call("toolu_3", "computer_act", &key_args),
            ),
            ("chat/completions", chat_text("Typed hello into Mousepad.")),
        ]
    };
    let mut script = vec![(
        "chat/completions",
        chat_tool_call(
            "call_d",
            "delegate_computer",
            &json!({"task": "type hello into the editor"}),
        ),
    )];
    script.extend(sub);
    script.push(("chat/completions", chat_text("Done.")));
    let h = Harness::start(script, messages_wire).await;
    let mut agents = h.agents().await;
    let mut session = h.session().await;
    let agent_id = create_agent(&mut agents).await;
    let mut events = session
        .events(authed(pb::EventsRequest { from_offset: 0 }))
        .await
        .unwrap()
        .into_inner();
    send(&mut agents, &agent_id, "type hello", "").await;
    let entries = wait_turn_end(&mut agents, &agent_id).await;

    let Some(Body::TurnEnd(end)) = entries.last().and_then(|e| e.body.as_ref()) else {
        panic!("no turn end");
    };
    assert!(end.ok, "{}", end.error);
    let sub_calls: Vec<&pb::ToolCall> = entries
        .iter()
        .filter_map(|e| match &e.body {
            Some(Body::ToolCall(c)) if c.subagent == "computer" => Some(c),
            _ => None,
        })
        .collect();
    assert_eq!(sub_calls.len(), 3);
    let sub_texts: Vec<&str> = entries
        .iter()
        .filter_map(|e| match &e.body {
            Some(Body::Text(t)) if t.subagent == "computer" => Some(t.text.as_str()),
            _ => None,
        })
        .collect();
    assert!(
        sub_texts.contains(&"Typed hello into Mousepad."),
        "{sub_texts:?}"
    );
    let state = entries
        .iter()
        .find_map(|e| match &e.body {
            Some(Body::ToolResult(r)) if r.call_id == "toolu_1" => Some(r),
            _ => None,
        })
        .unwrap();
    assert!(
        state.text.contains("[0] text \"editor\" center=(60,40)"),
        "{}",
        state.text
    );
    assert!(
        state.text.find("Mousepad") < state.text.find("Xfdesktop"),
        "{}",
        state.text
    );
    assert!(!state.text.contains("hidden"), "{}", state.text);
    assert!(state.image.starts_with(b"\x89PNG"));
    let delegate = entries
        .iter()
        .find_map(|e| match &e.body {
            Some(Body::ToolResult(r)) if r.call_id == "call_d" => Some(r),
            _ => None,
        })
        .unwrap();
    assert!(
        delegate.text.starts_with("Typed hello into Mousepad."),
        "{}",
        delegate.text
    );
    assert!(delegate.image.starts_with(b"\x89PNG"));

    let typed = h
        .cua_log
        .lock()
        .unwrap()
        .iter()
        .find(|(n, _)| n == "type_text")
        .map(|(_, a)| a.clone())
        .unwrap();
    assert_eq!(typed["element_token"], "tok-0");
    assert_eq!(typed["pid"], 42);
    assert_eq!(typed["text"], "hello");
    assert_eq!(typed["window_id"], 7);
    let pressed = h
        .cua_log
        .lock()
        .unwrap()
        .iter()
        .find(|(n, _)| n == "press_key")
        .map(|(_, a)| a.clone())
        .unwrap();
    assert_eq!(
        (pressed["pid"].clone(), pressed["window_id"].clone()),
        (json!(42), json!(7))
    );
    let session = typed["session"].as_str().unwrap().to_owned();
    assert!(session.starts_with("cu-"), "{session}");
    let cua_calls = h.cua_log.lock().unwrap().clone();
    let names: Vec<&str> = cua_calls.iter().map(|(n, _)| n.as_str()).collect();
    assert_eq!(
        names[..3],
        ["list_windows", "get_window_state", "get_desktop_state"]
    );
    assert!(
        cua_calls
            .iter()
            .all(|(_, a)| a["session"] == session.as_str()),
        "{cua_calls:?}"
    );
    assert_eq!(
        cua_calls.last().map(|(n, _)| n.as_str()),
        Some("end_session")
    );

    let chats = gw_bodies(&h.gw_log, "chat/completions");
    let main_after = if messages_wire {
        let vision = gw_bodies(&h.gw_log, "/v1/messages");
        assert_eq!(vision.len(), 4);
        assert_eq!(vision[0]["model"], "anthropic/claude-sonnet-5");
        let last_user = vision[3]["messages"]
            .as_array()
            .unwrap()
            .last()
            .unwrap()
            .clone();
        assert_eq!(last_user["content"][0]["type"], "tool_result");
        assert_eq!(last_user["content"][0]["content"][1]["type"], "image");
        assert_eq!(chats.len(), 2);
        &chats[1]
    } else {
        assert_eq!(chats.len(), 6);
        assert!(gw_bodies(&h.gw_log, "/v1/messages").is_empty());
        let models: Vec<&str> = chats.iter().map(|c| c["model"].as_str().unwrap()).collect();
        assert_eq!(models[1..5], ["anthropic/claude-sonnet-5"; 4]);
        let msgs = chats[4]["messages"].as_array().unwrap();
        let roles: Vec<&str> = msgs.iter().map(|m| m["role"].as_str().unwrap()).collect();
        assert_eq!(roles.last(), Some(&"user"), "{roles:?}");
        let shot = &msgs.last().unwrap()["content"][1]["image_url"]["url"];
        assert!(shot.as_str().unwrap().starts_with("data:image/png;base64,"));
        &chats[5]
    };
    let main_tool = main_after["messages"]
        .as_array()
        .unwrap()
        .iter()
        .find(|m| m["role"] == "tool")
        .unwrap()
        .clone();
    assert!(main_tool["content"].as_str().unwrap().contains("no vision"));
    assert!(!main_after.to_string().contains(PNG));

    let mut actions = Vec::new();
    while let Ok(Some(Ok(ev))) = tokio::time::timeout(Duration::from_millis(500), events.message())
        .await
        .map(Result::transpose)
    {
        if let Some(event::Body::ComputerAction(a)) = ev.body {
            assert_eq!(a.agent_id, agent_id);
            actions.push((a.action, a.text));
        }
    }
    assert_eq!(
        actions,
        [
            ("type".to_owned(), "hello".to_owned()),
            ("key".to_owned(), "return".to_owned())
        ]
    );
}

#[tokio::test]
async fn take_over_waits_for_respond_and_cancel_ends_the_turn() {
    let h = Harness::start(
        vec![
            (
                "chat/completions",
                chat_tool_call(
                    "call_t",
                    "request_take_over",
                    &json!({"reason": "sign in to GitHub"}),
                ),
            ),
            ("chat/completions", chat_text("Signed in.")),
            (
                "chat/completions",
                chat_tool_call("call_s", "shell", &json!({"command": "sleep 30"})),
            ),
        ],
        false,
    )
    .await;
    let mut agents = h.agents().await;
    let agent_id = create_agent(&mut agents).await;
    send(&mut agents, &agent_id, "log in", "").await;
    let approval_id = loop {
        let page = agents
            .transcript(authed(pb::TranscriptRequest {
                agent_id: agent_id.clone(),
                after_seq: 0,
                limit: 0,
            }))
            .await
            .unwrap()
            .into_inner();
        if let Some(id) = page.entries.iter().find_map(|e| match &e.body {
            Some(Body::Approval(a)) if a.kind == "take_over" => Some(a.approval_id.clone()),
            _ => None,
        }) {
            break id;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    };
    let listed = agents
        .list(authed(pb::ListAgentsRequest {}))
        .await
        .unwrap()
        .into_inner();
    assert_eq!(
        listed.agents[0].status,
        i32::from(pb::AgentStatus::WaitingForUser)
    );
    let missing = agents
        .respond(authed(pb::RespondRequest {
            agent_id: agent_id.clone(),
            approval_id: "nope".into(),
            approve: true,
            text: String::new(),
        }))
        .await
        .unwrap_err();
    assert_eq!(missing.code(), Code::NotFound);
    agents
        .respond(authed(pb::RespondRequest {
            agent_id: agent_id.clone(),
            approval_id,
            approve: true,
            text: "done".into(),
        }))
        .await
        .unwrap();
    let entries = wait_turn_end(&mut agents, &agent_id).await;
    assert!(entries.iter().any(
        |e| matches!(&e.body, Some(Body::ToolResult(r)) if r.text.contains("User note: done"))
    ));

    send(&mut agents, &agent_id, "sleep", "").await;
    loop {
        let page = agents
            .transcript(authed(pb::TranscriptRequest {
                agent_id: agent_id.clone(),
                after_seq: entries.last().unwrap().seq,
                limit: 0,
            }))
            .await
            .unwrap()
            .into_inner();
        if page
            .entries
            .iter()
            .any(|e| matches!(&e.body, Some(Body::ToolCall(c)) if c.call_id == "call_s"))
        {
            break;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    tokio::time::sleep(Duration::from_millis(100)).await;
    agents
        .cancel(authed(pb::CancelRequest {
            agent_id: agent_id.clone(),
        }))
        .await
        .unwrap();
    for _ in 0..200 {
        let page = agents
            .transcript(authed(pb::TranscriptRequest {
                agent_id: agent_id.clone(),
                after_seq: entries.last().unwrap().seq,
                limit: 0,
            }))
            .await
            .unwrap()
            .into_inner();
        if let Some(Body::TurnEnd(end)) = page.entries.last().and_then(|e| e.body.as_ref()) {
            assert!(!end.ok);
            assert_eq!(end.error, "cancelled");
            return;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    panic!("cancel did not end the turn");
}

#[tokio::test]
async fn delete_cancels_a_running_turn_and_removes_the_agent() {
    let h = Harness::start(
        vec![
            (
                "chat/completions",
                chat_tool_call(
                    "call_s",
                    "shell",
                    &json!({"command": "sleep 30 & echo $! > sleep.pid; wait"}),
                ),
            ),
            ("chat/completions", chat_text("kept")),
        ],
        false,
    )
    .await;
    let mut agents = h.agents().await;
    let mut session = h.session().await;
    let doomed = create_agent(&mut agents).await;
    let kept = create_agent(&mut agents).await;
    let mut events = session
        .events(authed(pb::EventsRequest { from_offset: 0 }))
        .await
        .unwrap()
        .into_inner();
    send(&mut agents, &doomed, "sleep", "n1").await;
    send(&mut agents, &doomed, "queued behind the sleep", "n2").await;
    let sleep_pid = h.workspace.join("sleep.pid");
    loop {
        let page = agents
            .transcript(authed(pb::TranscriptRequest {
                agent_id: doomed.clone(),
                after_seq: 0,
                limit: 0,
            }))
            .await
            .unwrap()
            .into_inner();
        if page
            .entries
            .iter()
            .any(|e| matches!(&e.body, Some(Body::ToolCall(c)) if c.call_id == "call_s"))
            && std::fs::read_to_string(&sleep_pid).is_ok_and(|p| p.ends_with('\n'))
        {
            break;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    let started = std::time::Instant::now();
    agents
        .delete(authed(pb::DeleteAgentRequest {
            agent_id: doomed.clone(),
        }))
        .await
        .unwrap();
    assert!(
        started.elapsed() < Duration::from_secs(5),
        "{:?}",
        started.elapsed()
    );
    assert!(
        wait_gone(&sleep_pid).await,
        "the cancelled command survived the delete"
    );

    let listed: Vec<String> = agents
        .list(authed(pb::ListAgentsRequest {}))
        .await
        .unwrap()
        .into_inner()
        .agents
        .into_iter()
        .map(|a| a.id)
        .collect();
    assert_eq!(listed, [kept.as_str()]);
    let page = agents
        .transcript(authed(pb::TranscriptRequest {
            agent_id: doomed.clone(),
            after_seq: 0,
            limit: 0,
        }))
        .await
        .unwrap()
        .into_inner();
    assert!(page.entries.is_empty(), "{:?}", page.entries);
    let again = agents
        .delete(authed(pb::DeleteAgentRequest {
            agent_id: doomed.clone(),
        }))
        .await
        .unwrap_err();
    assert_eq!(again.code(), Code::NotFound);
    let send_after = agents
        .send(authed(pb::SendRequest {
            agent_id: doomed.clone(),
            text: "hello?".into(),
            ..Default::default()
        }))
        .await
        .unwrap_err();
    assert_eq!(send_after.code(), Code::NotFound);
    let recreated = agents
        .create(authed(pb::CreateAgentRequest {
            name: "fresh".into(),
            ..Default::default()
        }))
        .await
        .unwrap()
        .into_inner();
    assert_ne!(recreated.id, doomed);

    let deleted = loop {
        let ev = tokio::time::timeout(Duration::from_secs(5), events.message())
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        if let Some(event::Body::AgentDeleted(d)) = ev.body {
            break d.agent_id;
        }
    };
    assert_eq!(deleted, doomed);

    send(&mut agents, &kept, "still here", "").await;
    let entries = wait_turn_end(&mut agents, &kept).await;
    assert!(
        entries
            .iter()
            .any(|e| matches!(&e.body, Some(Body::Text(t)) if t.text == "kept"))
    );
    let chats = gw_bodies(&h.gw_log, "chat/completions");
    assert_eq!(
        chats.len(),
        2,
        "the queued turn of the deleted agent never ran"
    );
}

#[tokio::test]
async fn delete_ends_the_agents_shell_and_its_background_jobs() {
    let h = Harness::start(
        vec![
            (
                "chat/completions",
                chat_tool_call(
                    "call_bg",
                    "shell",
                    &json!({"command": "echo $$ > shell.pid; sleep 300 & echo $! > job.pid"}),
                ),
            ),
            ("chat/completions", chat_text("started")),
        ],
        false,
    )
    .await;
    let mut agents = h.agents().await;
    let doomed = create_agent(&mut agents).await;
    send(&mut agents, &doomed, "start a job", "").await;
    wait_turn_end(&mut agents, &doomed).await;
    let pids = [h.workspace.join("shell.pid"), h.workspace.join("job.pid")];
    for pid in &pids {
        assert!(
            alive(pid),
            "{} not running before the delete",
            pid.display()
        );
    }
    agents
        .delete(authed(pb::DeleteAgentRequest {
            agent_id: doomed.clone(),
        }))
        .await
        .unwrap();
    for pid in &pids {
        assert!(
            wait_gone(pid).await,
            "{} survived the delete",
            pid.display()
        );
    }
}

#[tokio::test]
async fn untitled_agents_take_their_first_line_and_a_rename_wins() {
    let h = Harness::start(
        vec![
            ("chat/completions", chat_text("好的")),
            ("chat/completions", chat_text("ok")),
            ("chat/completions", chat_text("ok")),
        ],
        false,
    )
    .await;
    let mut agents = h.agents().await;
    let mut session = h.session().await;
    let mut events = session
        .events(authed(pb::EventsRequest { from_offset: 0 }))
        .await
        .unwrap()
        .into_inner();
    let untitled = agents
        .create(authed(pb::CreateAgentRequest::default()))
        .await
        .unwrap()
        .into_inner();
    assert_eq!(untitled.name, "agent");
    send(
        &mut agents,
        &untitled.id,
        "\n  帮我整理一下桌面上的下载文件夹，并按照文件类型分类放好  \n谢谢",
        "",
    )
    .await;
    wait_turn_end(&mut agents, &untitled.id).await;
    let name = |list: &pb::ListAgentsResponse, id: &str| {
        list.agents
            .iter()
            .find(|a| a.id == id)
            .map(|a| a.name.clone())
            .unwrap()
    };
    let list = agents
        .list(authed(pb::ListAgentsRequest {}))
        .await
        .unwrap()
        .into_inner();
    assert_eq!(
        name(&list, &untitled.id),
        "帮我整理一下桌面上的下载文件夹，并按照文件类型分"
    );
    let titled = loop {
        let ev = tokio::time::timeout(Duration::from_secs(5), events.message())
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        if let Some(event::Body::Agent(a)) = ev.body
            && a.id == untitled.id
            && a.name != "agent"
        {
            break a.name;
        }
    };
    assert_eq!(titled, "帮我整理一下桌面上的下载文件夹，并按照文件类型分");

    let renamed = agents
        .rename(authed(pb::RenameAgentRequest {
            agent_id: untitled.id.clone(),
            name: "  下载整理  ".into(),
        }))
        .await
        .unwrap()
        .into_inner();
    assert_eq!(renamed.name, "下载整理");
    let named_first = agents
        .create(authed(pb::CreateAgentRequest::default()))
        .await
        .unwrap()
        .into_inner();
    agents
        .rename(authed(pb::RenameAgentRequest {
            agent_id: named_first.id.clone(),
            name: "mine".into(),
        }))
        .await
        .unwrap();
    send(
        &mut agents,
        &named_first.id,
        "a message that would title it",
        "",
    )
    .await;
    wait_turn_end(&mut agents, &named_first.id).await;
    let list = agents
        .list(authed(pb::ListAgentsRequest {}))
        .await
        .unwrap()
        .into_inner();
    assert_eq!(name(&list, &untitled.id), "下载整理");
    assert_eq!(name(&list, &named_first.id), "mine");

    let blank = agents
        .rename(authed(pb::RenameAgentRequest {
            agent_id: untitled.id.clone(),
            name: " ".into(),
        }))
        .await
        .unwrap_err();
    assert_eq!(blank.code(), Code::InvalidArgument);
    let unknown = agents
        .rename(authed(pb::RenameAgentRequest {
            agent_id: "ag-nope".into(),
            name: "x".into(),
        }))
        .await
        .unwrap_err();
    assert_eq!(unknown.code(), Code::NotFound);
}

#[tokio::test]
async fn screen_relays_raw_rfb_bytes() {
    let h = Harness::start(Vec::new(), false).await;
    let mut session = h.session().await;
    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(pb::ScreenClientFrame {
        body: Some(screen_client_frame::Body::Open(pb::ScreenOpen {})),
    })
    .await
    .unwrap();
    let mut frames = session
        .screen(authed(tokio_stream_from(rx)))
        .await
        .unwrap()
        .into_inner();
    let banner = frames.message().await.unwrap().unwrap();
    assert_eq!(&banner.data[..], b"RFB 003.008\n");
    tx.send(pb::ScreenClientFrame {
        body: Some(screen_client_frame::Body::Data(bytes::Bytes::from_static(
            b"RFB 003.008\n",
        ))),
    })
    .await
    .unwrap();
    let echo = frames.message().await.unwrap().unwrap();
    assert_eq!(&echo.data[..], b"RFB 003.008\n");
}

fn tokio_stream_from(
    rx: tokio::sync::mpsc::Receiver<pb::ScreenClientFrame>,
) -> tokio_stream::wrappers::ReceiverStream<pb::ScreenClientFrame> {
    tokio_stream::wrappers::ReceiverStream::new(rx)
}

#[tokio::test]
async fn cancel_during_computer_task_ends_the_driver_session() {
    let h = Harness::start(
        vec![
            (
                "chat/completions",
                chat_tool_call("call_d", "delegate_computer", &json!({"task": "wait"})),
            ),
            (
                "chat/completions",
                chat_tool_call(
                    "toolu_w",
                    "computer_act",
                    &json!({"action": "wait", "seconds": 10}),
                ),
            ),
        ],
        false,
    )
    .await;
    let mut agents = h.agents().await;
    let agent_id = create_agent(&mut agents).await;
    send(&mut agents, &agent_id, "wait", "").await;
    for _ in 0..200 {
        let page = agents
            .transcript(authed(pb::TranscriptRequest {
                agent_id: agent_id.clone(),
                after_seq: 0,
                limit: 0,
            }))
            .await
            .unwrap()
            .into_inner();
        if page
            .entries
            .iter()
            .any(|e| matches!(&e.body, Some(Body::ToolCall(c)) if c.call_id == "toolu_w"))
        {
            break;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    agents
        .cancel(authed(pb::CancelRequest {
            agent_id: agent_id.clone(),
        }))
        .await
        .unwrap();
    let entries = wait_turn_end(&mut agents, &agent_id).await;
    assert!(
        matches!(entries.last().and_then(|e| e.body.as_ref()), Some(Body::TurnEnd(end)) if end.error == "cancelled")
    );
    for _ in 0..200 {
        if h.cua_log.lock().unwrap().iter().any(|(n, a)| {
            n == "end_session" && a["session"].as_str().is_some_and(|s| s.starts_with("cu-"))
        }) {
            return;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    panic!(
        "cancel did not end the driver session: {:?}",
        h.cua_log.lock().unwrap()
    );
}

#[tokio::test]
async fn vision_main_model_gets_a_screenshot_tool() {
    let h = Harness::start(
        vec![
            (
                "chat/completions",
                chat_tool_call("call_s", "screenshot", &json!({})),
            ),
            ("chat/completions", chat_text("I see the desktop.")),
        ],
        false,
    )
    .await;
    let mut agents = h.agents().await;
    let agent_id = agents
        .create(authed(pb::CreateAgentRequest {
            name: "v".into(),
            main_model: "anthropic/claude-sonnet-5".into(),
            ..Default::default()
        }))
        .await
        .unwrap()
        .into_inner()
        .id;
    send(&mut agents, &agent_id, "what is on screen", "").await;
    let entries = wait_turn_end(&mut agents, &agent_id).await;
    assert!(
        matches!(entries.last().and_then(|e| e.body.as_ref()), Some(Body::TurnEnd(end)) if end.ok)
    );
    let chats = gw_bodies(&h.gw_log, "chat/completions");
    assert!(
        chats[0]["tools"]
            .as_array()
            .unwrap()
            .iter()
            .any(|t| t["function"]["name"] == "screenshot")
    );
    let last = chats[1]["messages"]
        .as_array()
        .unwrap()
        .last()
        .unwrap()
        .clone();
    assert_eq!(last["role"], "user");
    assert!(
        last["content"][1]["image_url"]["url"]
            .as_str()
            .unwrap()
            .starts_with("data:image/png;base64,")
    );
    let cua = h.cua_log.lock().unwrap().clone();
    let names: Vec<&str> = cua.iter().map(|(n, _)| n.as_str()).collect();
    assert_eq!(names, ["get_desktop_state", "end_session"]);
    assert!(
        cua.iter()
            .all(|(_, a)| a["session"].as_str().unwrap().starts_with("shot-"))
    );
}

struct McpClient {
    http: reqwest::Client,
    url: String,
    session: String,
    next_id: u64,
}

impl McpClient {
    async fn connect(h: &Harness) -> Self {
        let mut c = Self {
            http: reqwest::Client::new(),
            url: format!("{}/mcp", h.endpoint),
            session: String::new(),
            next_id: 0,
        };
        let resp = c
            .post(
                json!({"jsonrpc": "2.0", "id": 0, "method": "initialize", "params": {
                    "protocolVersion": "2025-06-18",
                    "capabilities": {},
                    "clientInfo": {"name": "test-client", "version": "1.0"}
                }}),
            )
            .await;
        assert_eq!(resp.status(), 200);
        c.session = resp.headers()["mcp-session-id"]
            .to_str()
            .unwrap()
            .to_owned();
        let init: Value = resp.json().await.unwrap();
        assert_eq!(init["result"]["protocolVersion"], "2025-06-18");
        assert_eq!(init["result"]["serverInfo"]["name"], "cocoonbox-agent");
        assert!(init["result"]["capabilities"]["tools"].is_object());
        let note = c
            .post(json!({"jsonrpc": "2.0", "method": "notifications/initialized"}))
            .await;
        assert_eq!(note.status(), 202);
        c
    }

    async fn post(&self, body: Value) -> reqwest::Response {
        let mut req = self
            .http
            .post(&self.url)
            .header("x-cocoonbox-edge-token", TOKEN)
            .header("accept", "application/json, text/event-stream")
            .json(&body);
        if !self.session.is_empty() {
            req = req.header("mcp-session-id", &self.session);
        }
        req.send().await.unwrap()
    }

    async fn rpc(&mut self, method: &str, params: Value) -> Value {
        self.next_id += 1;
        let resp = self
            .post(json!({"jsonrpc": "2.0", "id": self.next_id, "method": method, "params": params}))
            .await;
        assert_eq!(resp.status(), 200);
        let reply: Value = resp.json().await.unwrap();
        assert_eq!(reply["id"], self.next_id);
        reply
    }

    async fn call(&mut self, name: &str, arguments: Value) -> Value {
        let reply = self
            .rpc("tools/call", json!({"name": name, "arguments": arguments}))
            .await;
        reply["result"].clone()
    }
}

fn driver_calls(h: &Harness, name: &str) -> Vec<Value> {
    h.cua_log
        .lock()
        .unwrap()
        .iter()
        .filter(|(n, _)| n == name)
        .map(|(_, a)| a.clone())
        .collect()
}

#[tokio::test]
async fn mcp_round_trip_drives_tools_and_shows_in_the_transcript() {
    let h = Harness::start(Vec::new(), false).await;
    let http = reqwest::Client::new();
    let denied = http
        .post(format!("{}/mcp", h.endpoint))
        .json(&json!({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}}))
        .send()
        .await
        .unwrap();
    assert_eq!(denied.status(), 401);

    let mut session = h.session().await;
    let mut events = session
        .events(authed(pb::EventsRequest { from_offset: 0 }))
        .await
        .unwrap()
        .into_inner();
    let mut mcp = McpClient::connect(&h).await;

    let listed = mcp.rpc("tools/list", json!({})).await;
    let mut names: Vec<&str> = listed["result"]["tools"]
        .as_array()
        .unwrap()
        .iter()
        .map(|t| t["name"].as_str().unwrap())
        .collect();
    names.sort_unstable();
    assert_eq!(
        names,
        [
            "browser_click",
            "browser_navigate",
            "browser_read",
            "browser_snapshot",
            "browser_type",
            "computer_act",
            "computer_state",
            "edit_file",
            "list_dir",
            "read_file",
            "screenshot",
            "shell",
            "write_file"
        ]
    );
    assert!(listed["result"]["tools"][0]["inputSchema"]["type"] == "object");

    let shell = mcp
        .call(
            "shell",
            json!({"command": "echo from-mcp > m.txt; cat m.txt"}),
        )
        .await;
    assert_eq!(shell["isError"], false);
    assert!(
        shell["content"][0]["text"]
            .as_str()
            .unwrap()
            .starts_with("from-mcp"),
        "{shell}"
    );
    assert!(h.workspace.join("m.txt").exists());

    let shot = mcp.call("screenshot", json!({"max_edge": 640})).await;
    assert_eq!(shot["content"][1]["type"], "image");
    assert_eq!(shot["content"][1]["mimeType"], "image/png");
    assert_eq!(shot["content"][1]["data"], PNG);
    let captures = driver_calls(&h, "get_desktop_state");
    assert_eq!(captures[0]["max_image_dimension"], 640);
    let label = captures[0]["session"].as_str().unwrap().to_owned();
    assert!(label.starts_with("cu-"), "{label}");

    let act = mcp
        .call("computer_act", json!({"action": "click", "x": 10, "y": 20}))
        .await;
    assert_eq!(act["isError"], false, "{act}");
    assert_eq!(driver_calls(&h, "click")[0]["session"], label.as_str());

    let unknown = mcp
        .rpc(
            "tools/call",
            json!({"name": "delegate_computer", "arguments": {}}),
        )
        .await;
    assert_eq!(unknown["error"]["code"], -32602);

    let mut agents = h.agents().await;
    let listed = agents
        .list(authed(pb::ListAgentsRequest {}))
        .await
        .unwrap()
        .into_inner();
    let agent = listed
        .agents
        .iter()
        .find(|a| a.name == "mcp:test-client")
        .unwrap();
    assert_eq!(agent.status, i32::from(pb::AgentStatus::Running));
    let page = agents
        .transcript(authed(pb::TranscriptRequest {
            agent_id: agent.id.clone(),
            after_seq: 0,
            limit: 0,
        }))
        .await
        .unwrap()
        .into_inner();
    let calls: Vec<&str> = page
        .entries
        .iter()
        .filter_map(|e| match &e.body {
            Some(Body::ToolCall(c)) => Some(c.name.as_str()),
            _ => None,
        })
        .collect();
    assert_eq!(calls, ["shell", "screenshot", "computer_act"]);
    let results = page
        .entries
        .iter()
        .filter(|e| matches!(e.body, Some(Body::ToolResult(_))))
        .count();
    assert_eq!(results, 3);

    let mut saw_action = false;
    while let Ok(Some(Ok(ev))) = tokio::time::timeout(Duration::from_millis(300), events.message())
        .await
        .map(Result::transpose)
    {
        if let Some(event::Body::ComputerAction(a)) = ev.body {
            assert_eq!(
                (a.agent_id.as_str(), a.action.as_str(), a.x, a.y),
                (agent.id.as_str(), "click", 10, 20)
            );
            saw_action = true;
        }
    }
    assert!(saw_action);

    let discover = reqwest::Client::new()
        .post(format!("{}/mcp", h.endpoint))
        .header("x-cocoonbox-edge-token", TOKEN)
        .json(&json!({"jsonrpc": "2.0", "id": 8, "method": "server/discover", "params": {}}))
        .send()
        .await
        .unwrap();
    assert_eq!(discover.status(), 200);
    let discover: Value = discover.json().await.unwrap();
    assert_eq!(discover["error"]["code"], -32601);
    assert_eq!(discover["id"], 8);
    let unknown_in_session = mcp.rpc("resources/list", json!({})).await;
    assert_eq!(unknown_in_session["error"]["code"], -32601);

    let no_session = reqwest::Client::new()
        .post(format!("{}/mcp", h.endpoint))
        .header("x-cocoonbox-edge-token", TOKEN)
        .json(&json!({"jsonrpc": "2.0", "id": 9, "method": "tools/list"}))
        .send()
        .await
        .unwrap();
    assert_eq!(no_session.status(), 400);

    let deleted = http
        .delete(format!("{}/mcp", h.endpoint))
        .header("x-cocoonbox-edge-token", TOKEN)
        .header("mcp-session-id", &mcp.session)
        .send()
        .await
        .unwrap();
    assert_eq!(deleted.status(), 204);
    let ended = driver_calls(&h, "end_session");
    assert_eq!(ended.len(), 1);
    assert_eq!(ended[0]["session"], label.as_str());
    let gone = mcp
        .post(json!({"jsonrpc": "2.0", "id": 10, "method": "tools/list"}))
        .await;
    assert_eq!(gone.status(), 404);
    let listed = agents
        .list(authed(pb::ListAgentsRequest {}))
        .await
        .unwrap()
        .into_inner();
    let agent = listed
        .agents
        .iter()
        .find(|a| a.name == "mcp:test-client")
        .unwrap();
    assert_eq!(agent.status, i32::from(pb::AgentStatus::Idle));
}

#[tokio::test]
async fn mcp_ends_the_driver_session_on_stream_close_and_idle() {
    let day = Duration::from_secs(24 * 3600);
    let h = Harness::start_with(Vec::new(), false, Duration::from_millis(400), day).await;
    let mut mcp = McpClient::connect(&h).await;
    mcp.call("screenshot", json!({})).await;
    let first = driver_calls(&h, "get_desktop_state")[0]["session"].clone();

    let mut stream = mcp
        .http
        .get(&mcp.url)
        .header("x-cocoonbox-edge-token", TOKEN)
        .header("mcp-session-id", &mcp.session)
        .header("accept", "text/event-stream")
        .send()
        .await
        .unwrap();
    assert_eq!(stream.headers()["content-type"], "text/event-stream");
    assert!(stream.chunk().await.unwrap().is_some());
    drop(stream);
    for _ in 0..100 {
        if !driver_calls(&h, "end_session").is_empty() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    assert_eq!(driver_calls(&h, "end_session")[0]["session"], first);

    mcp.call("screenshot", json!({})).await;
    let second = driver_calls(&h, "get_desktop_state")[1]["session"].clone();
    assert_ne!(first, second);
    tokio::time::sleep(Duration::from_millis(1200)).await;
    let ended = driver_calls(&h, "end_session");
    assert_eq!(ended.len(), 2, "{ended:?}");
    assert_eq!(ended[1]["session"], second);
    let gone = mcp
        .post(json!({"jsonrpc": "2.0", "id": 5, "method": "tools/list"}))
        .await;
    assert_eq!(gone.status(), 404);
}

#[tokio::test]
async fn concurrent_computer_acts_do_not_interleave() {
    let h = Harness::start(Vec::new(), false).await;
    let mut a = McpClient::connect(&h).await;
    let mut b = McpClient::connect(&h).await;
    let (ra, rb) = tokio::join!(
        a.call("computer_act", json!({"action": "click", "x": 1, "y": 1})),
        b.call("computer_act", json!({"action": "click", "x": 2, "y": 2}))
    );
    assert_eq!(
        (ra["isError"].clone(), rb["isError"].clone()),
        (json!(false), json!(false))
    );
    let log: Vec<(String, String)> = h
        .cua_log
        .lock()
        .unwrap()
        .iter()
        .map(|(n, a)| {
            (
                n.clone(),
                a["session"].as_str().unwrap_or_default().to_owned(),
            )
        })
        .collect();
    assert_eq!(log.len(), 4, "{log:?}");
    assert_eq!(
        (log[0].0.as_str(), log[1].0.as_str()),
        ("click", "get_desktop_state")
    );
    assert_eq!(
        (log[2].0.as_str(), log[3].0.as_str()),
        ("click", "get_desktop_state")
    );
    assert_eq!(log[0].1, log[1].1);
    assert_eq!(log[2].1, log[3].1);
    assert_ne!(log[0].1, log[2].1);

    let raw = a
        .post(
            json!({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
                "protocolVersion": "2099-01-01",
                "clientInfo": {"name": "future"}
            }}),
        )
        .await;
    let init: Value = raw.json().await.unwrap();
    assert_eq!(init["result"]["protocolVersion"], "2025-06-18");
}

#[tokio::test]
async fn non_ascii_text_is_typed_one_character_per_call() {
    let h = Harness::start(Vec::new(), false).await;
    let mut mcp = McpClient::connect(&h).await;
    let out = mcp
        .call(
            "computer_act",
            json!({"action": "type", "text": "hi — 你好 🙂"}),
        )
        .await;
    assert_eq!(out["isError"], false, "{out}");
    let typed: Vec<(String, Value, Value)> = driver_calls(&h, "type_text")
        .into_iter()
        .map(|a| {
            (
                a["text"].as_str().unwrap().to_owned(),
                a["pid"].clone(),
                a["window_id"].clone(),
            )
        })
        .collect();
    let texts: Vec<&str> = typed.iter().map(|t| t.0.as_str()).collect();
    assert_eq!(texts, ["hi ", "—", " ", "你", "好", " ", "🙂"]);
    assert!(typed.iter().all(|t| t.1 == 42 && t.2 == 7), "{typed:?}");
    let ascii = mcp
        .call(
            "computer_act",
            json!({"action": "type", "text": "plain ascii"}),
        )
        .await;
    assert_eq!(ascii["isError"], false);
    assert_eq!(driver_calls(&h, "type_text").len(), 8);
}

#[tokio::test]
async fn closed_mcp_agents_are_pruned_after_the_prune_age() {
    let h = Harness::start_with(
        Vec::new(),
        false,
        Duration::from_millis(400),
        Duration::from_millis(300),
    )
    .await;
    let mut closed = McpClient::connect(&h).await;
    let job = closed
        .call(
            "shell",
            json!({"command": "sleep 300 & echo $! > mcp-job.pid"}),
        )
        .await;
    assert_eq!(job["isError"], false, "{job}");
    let job = h.workspace.join("mcp-job.pid");
    assert!(alive(&job));
    let deleted = reqwest::Client::new()
        .delete(format!("{}/mcp", h.endpoint))
        .header("x-cocoonbox-edge-token", TOKEN)
        .header("mcp-session-id", &closed.session)
        .send()
        .await
        .unwrap();
    assert_eq!(deleted.status(), 204);
    let mut open = McpClient::connect(&h).await;
    let mut agents = h.agents().await;
    let loop_agent = create_agent(&mut agents).await;
    for _ in 0..8 {
        tokio::time::sleep(Duration::from_millis(100)).await;
        open.rpc("ping", json!({})).await;
    }
    let names: Vec<(String, String)> = agents
        .list(authed(pb::ListAgentsRequest {}))
        .await
        .unwrap()
        .into_inner()
        .agents
        .into_iter()
        .map(|a| (a.id, a.name))
        .collect();
    let mcp_count = names.iter().filter(|(_, n)| n == "mcp:test-client").count();
    assert_eq!(mcp_count, 1, "{names:?}");
    assert!(names.iter().any(|(id, _)| *id == loop_agent), "{names:?}");
    assert!(wait_gone(&job).await, "the pruned agent's job survived");
}

#[tokio::test]
async fn mcp_agents_are_deleted_only_after_their_session_closes() {
    let h = Harness::start(Vec::new(), false).await;
    let mcp = McpClient::connect(&h).await;
    let mut agents = h.agents().await;
    let agent_id = agents
        .list(authed(pb::ListAgentsRequest {}))
        .await
        .unwrap()
        .into_inner()
        .agents
        .into_iter()
        .find(|a| a.name == "mcp:test-client")
        .unwrap()
        .id;
    let refused = agents
        .delete(authed(pb::DeleteAgentRequest {
            agent_id: agent_id.clone(),
        }))
        .await
        .unwrap_err();
    assert_eq!(refused.code(), Code::FailedPrecondition);
    assert_eq!(
        mcp.post(json!({"jsonrpc": "2.0", "id": 9, "method": "ping"}))
            .await
            .status(),
        200
    );
    let closed = reqwest::Client::new()
        .delete(format!("{}/mcp", h.endpoint))
        .header("x-cocoonbox-edge-token", TOKEN)
        .header("mcp-session-id", &mcp.session)
        .send()
        .await
        .unwrap();
    assert_eq!(closed.status(), 204);
    agents
        .delete(authed(pb::DeleteAgentRequest {
            agent_id: agent_id.clone(),
        }))
        .await
        .unwrap();
    let listed = agents
        .list(authed(pb::ListAgentsRequest {}))
        .await
        .unwrap()
        .into_inner();
    assert!(listed.agents.is_empty(), "{:?}", listed.agents);
}

#[tokio::test]
async fn launch_app_reuses_a_running_apps_window() {
    let h = Harness::start(Vec::new(), false).await;
    let mut mcp = McpClient::connect(&h).await;
    let running = mcp
        .call(
            "computer_act",
            json!({"action": "launch_app", "app": "mousepad"}),
        )
        .await;
    assert_eq!(running["isError"], false, "{running}");
    assert!(
        running["content"][0]["text"]
            .as_str()
            .unwrap()
            .contains("already running"),
        "{running}"
    );
    let raised = driver_calls(&h, "bring_to_front");
    assert_eq!(
        (raised[0]["pid"].clone(), raised[0]["window_id"].clone()),
        (json!(42), json!(7))
    );
    assert!(driver_calls(&h, "launch_app").is_empty());

    let fresh = mcp
        .call(
            "computer_act",
            json!({"action": "launch_app", "app": "gedit"}),
        )
        .await;
    assert_eq!(fresh["isError"], false, "{fresh}");
    assert_eq!(driver_calls(&h, "launch_app")[0]["name"], "gedit");
}

type Objects = Arc<Mutex<HashMap<String, Vec<u8>>>>;

async fn fake_store() -> (String, Objects) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let base = format!("http://{}/bucket", listener.local_addr().unwrap());
    let objects = Objects::default();
    let shared = Arc::clone(&objects);
    tokio::spawn(async move {
        loop {
            let (stream, _) = listener.accept().await.unwrap();
            let objects = Arc::clone(&shared);
            tokio::spawn(async move {
                let mut reader = BufReader::new(stream);
                loop {
                    let mut line = String::new();
                    if reader.read_line(&mut line).await.unwrap_or(0) == 0 {
                        return;
                    }
                    let mut parts = line.split_whitespace();
                    let method = parts.next().unwrap_or_default().to_owned();
                    let name = parts
                        .next()
                        .unwrap_or_default()
                        .rsplit('/')
                        .next()
                        .unwrap()
                        .to_owned();
                    let mut len = 0;
                    loop {
                        let mut h = String::new();
                        reader.read_line(&mut h).await.unwrap();
                        if h == "\r\n" {
                            break;
                        }
                        if let Some(v) = h.to_ascii_lowercase().strip_prefix("content-length:") {
                            len = v.trim().parse().unwrap();
                        }
                    }
                    let mut body = vec![0u8; len];
                    reader.read_exact(&mut body).await.unwrap();
                    let (status, reply) = if method == "PUT" {
                        objects.lock().unwrap().insert(name, body);
                        ("200 OK", Vec::new())
                    } else {
                        match objects.lock().unwrap().get(&name) {
                            Some(b) => ("200 OK", b.clone()),
                            None => ("404 Not Found", Vec::new()),
                        }
                    };
                    let head = format!(
                        "HTTP/1.1 {status}\r\ncontent-length: {}\r\n\r\n",
                        reply.len()
                    );
                    let stream = reader.get_mut();
                    stream.write_all(head.as_bytes()).await.unwrap();
                    stream.write_all(&reply).await.unwrap();
                }
            });
        }
    });
    (base, objects)
}

fn state_key() -> pb::StateKey {
    pb::StateKey {
        key: vec![42u8; 32].into(),
        key_id: "k1".into(),
    }
}

async fn state_client(h: &Harness) -> cocoonbox_agent::pb::state_client::StateClient<Channel> {
    cocoonbox_agent::pb::state_client::StateClient::new(h.channel().await)
}

async fn plan(h: &Harness, consistency: pb::Consistency, parent: &str) -> pb::PlanResponse {
    state_client(h)
        .await
        .plan(authed(pb::PlanRequest {
            key: Some(state_key()),
            consistency: consistency.into(),
            parent: parent.into(),
        }))
        .await
        .unwrap()
        .into_inner()
}

async fn upload_missing(
    h: &Harness,
    base: &str,
    objects: &Objects,
    planned: &pb::PlanResponse,
) -> usize {
    let missing: Vec<String> = {
        let have = objects.lock().unwrap();
        planned
            .chunks
            .iter()
            .filter(|c| !have.contains_key(&c.id))
            .map(|c| c.id.clone())
            .collect()
    };
    let put = |id: &str| pb::PresignedPut {
        chunk_id: id.into(),
        url: format!("{base}/{id}"),
        headers: HashMap::from([("x-amz-meta-test".to_owned(), "1".to_owned())]),
    };
    let mut stream = state_client(h)
        .await
        .upload(authed(pb::UploadRequest {
            snapshot_id: planned.snapshot_id.clone(),
            key: Some(state_key()),
            puts: missing.iter().map(|id| put(id)).collect(),
            manifest: Some(put(&planned.snapshot_id)),
        }))
        .await
        .unwrap()
        .into_inner();
    let mut last = None;
    while let Some(p) = stream.message().await.unwrap() {
        last = Some(p);
    }
    let last = last.unwrap();
    assert!(last.finished);
    assert_eq!(last.manifest_id, planned.snapshot_id);
    assert_eq!(last.done_chunks as usize, missing.len() + 1);
    missing.len()
}

async fn restore(
    h: &Harness,
    base: &str,
    objects: &Objects,
    manifest_id: &str,
) -> Result<pb::TransferProgress, tonic::Status> {
    let gets = objects
        .lock()
        .unwrap()
        .keys()
        .filter(|k| *k != manifest_id)
        .map(|k| pb::PresignedGet {
            chunk_id: k.clone(),
            url: format!("{base}/{k}"),
        })
        .collect();
    restore_pages(h, base, manifest_id, gets).await
}

async fn restore_pages(
    h: &Harness,
    base: &str,
    manifest_id: &str,
    gets: Vec<pb::PresignedGet>,
) -> Result<pb::TransferProgress, tonic::Status> {
    let mut reqs = vec![pb::RestoreRequest {
        key: Some(state_key()),
        manifest: Some(pb::PresignedGet {
            chunk_id: manifest_id.into(),
            url: format!("{base}/{manifest_id}"),
        }),
        gets: Vec::new(),
    }];
    reqs.extend(gets.chunks(2).map(|page| pb::RestoreRequest {
        gets: page.to_vec(),
        ..Default::default()
    }));
    let mut stream = state_client(h)
        .await
        .restore(authed(tokio_stream::iter(reqs)))
        .await?
        .into_inner();
    let mut last = None;
    while let Some(p) = stream.message().await? {
        last = Some(p);
    }
    Ok(last.unwrap())
}

fn manifest_of(objects: &Objects, id: &str) -> Value {
    let keys = cocoonbox_agent::state::crypto::Keys::derive(&state_key().key).unwrap();
    let sealed = objects.lock().unwrap()[id].clone();
    serde_json::from_slice(&keys.open(id, &sealed, 1 << 30).unwrap()).unwrap()
}

fn known_files(h: &Harness) -> Value {
    let path = h
        .home
        .parent()
        .unwrap()
        .join("state/state/known-chunks.json");
    serde_json::from_slice::<Value>(&std::fs::read(path).unwrap()).unwrap()["files"].take()
}

fn chunks_of(m: &Value, path: &str) -> Value {
    m["files"]
        .as_array()
        .unwrap()
        .iter()
        .find(|f| f["path"] == path)
        .unwrap()["chunks"]
        .clone()
}

fn write_script(path: &Path, body: &str) {
    let tmp = path.with_extension("tmp");
    std::fs::write(&tmp, format!("#!/bin/sh\n{body}\n")).unwrap();
    std::fs::set_permissions(&tmp, std::os::unix::fs::PermissionsExt::from_mode(0o755)).unwrap();
    std::fs::rename(&tmp, path).unwrap();
}

fn noise(n: usize, seed: u64) -> Vec<u8> {
    let mut x = seed;
    (0..n)
        .map(|_| {
            x ^= x << 13;
            x ^= x >> 7;
            x ^= x << 17;
            x as u8
        })
        .collect()
}

#[tokio::test]
async fn state_round_trip_is_incremental_and_restores_files_and_agent_db() {
    let h = Harness::start(Vec::new(), false).await;
    let (base, objects) = fake_store().await;
    let big = noise(3 << 20, 7);
    std::fs::create_dir_all(h.home.join(".config/app")).unwrap();
    std::fs::create_dir_all(h.home.join(".config/chrome/Default/Code Cache")).unwrap();
    std::fs::create_dir_all(h.home.join(".cache/fontconfig")).unwrap();
    std::fs::create_dir_all(h.home.join("Documents")).unwrap();
    std::fs::write(h.home.join(".config/app/settings.json"), b"{\"a\":1}").unwrap();
    std::fs::write(
        h.home.join(".config/chrome/Default/Code Cache/js"),
        b"cache",
    )
    .unwrap();
    std::fs::write(h.home.join(".config/chrome/SingletonLock"), b"lock").unwrap();
    std::fs::write(h.home.join(".cache/fontconfig/c"), b"junk").unwrap();
    std::fs::write(h.home.join("Documents/big.bin"), &big).unwrap();
    std::os::unix::fs::symlink("Documents/big.bin", h.home.join("big-link")).unwrap();
    std::fs::write(h.workspace.join("notes.txt"), b"first").unwrap();
    let db = h.home.join(".config/app/data.db");
    let conn = rusqlite::Connection::open(&db).unwrap();
    conn.execute_batch(
        "PRAGMA journal_mode=WAL; CREATE TABLE t(v); INSERT INTO t VALUES ('kept');",
    )
    .unwrap();
    assert!(h.home.join(".config/app/data.db-wal").exists());
    let mut agents = h.agents().await;
    let keeper = create_agent(&mut agents).await;

    let first = plan(&h, pb::Consistency::Live, "").await;
    assert!(first.files >= 4, "{first:?}");
    assert!(first.chunks.len() >= 4, "{}", first.chunks.len());
    let sent_first = upload_missing(&h, &base, &objects, &first).await;
    assert_eq!(sent_first, first.chunks.len());
    let m = manifest_of(&objects, &first.snapshot_id);
    let paths: Vec<String> = m["files"]
        .as_array()
        .unwrap()
        .iter()
        .map(|f| {
            format!(
                "{}/{}",
                f["root"].as_str().unwrap(),
                f["path"].as_str().unwrap()
            )
        })
        .collect();
    assert!(
        paths.contains(&"home/.config/app/data.db".to_owned()),
        "{paths:?}"
    );
    for gone in [
        "data.db-wal",
        "data.db-shm",
        "Code Cache",
        "SingletonLock",
        ".cache",
    ] {
        assert!(
            !paths.iter().any(|p| p.contains(gone)),
            "{gone} in {paths:?}"
        );
    }
    assert_eq!(m["consistency"], "live");
    assert!(!m["agent_db"]["chunks"].as_array().unwrap().is_empty());

    std::fs::write(h.workspace.join("notes.txt"), b"second").unwrap();
    let second = plan(&h, pb::Consistency::Quiesced, &first.snapshot_id).await;
    let sent_second = upload_missing(&h, &base, &objects, &second).await;
    assert!(sent_second <= 2, "incremental sent {sent_second} chunks");
    assert_eq!(
        manifest_of(&objects, &second.snapshot_id)["consistency"],
        "quiesced"
    );
    assert_eq!(
        manifest_of(&objects, &second.snapshot_id)["parent"],
        first.snapshot_id.as_str()
    );

    drop(conn);
    std::fs::remove_dir_all(h.home.join("Documents")).unwrap();
    std::fs::write(h.home.join("stray.txt"), b"not in the snapshot").unwrap();
    std::fs::write(h.workspace.join("notes.txt"), b"third").unwrap();
    let intruder = agents
        .create(authed(pb::CreateAgentRequest {
            name: "intruder".into(),
            ..Default::default()
        }))
        .await
        .unwrap()
        .into_inner()
        .id;
    let done = restore(&h, &base, &objects, &second.snapshot_id)
        .await
        .unwrap();
    assert!(done.finished);
    assert_eq!(
        std::fs::read(h.home.join("Documents/big.bin")).unwrap(),
        big
    );
    assert_eq!(
        std::fs::read_link(h.home.join("big-link")).unwrap(),
        PathBuf::from("Documents/big.bin")
    );
    assert_eq!(
        std::fs::read(h.workspace.join("notes.txt")).unwrap(),
        b"second"
    );
    assert!(!h.home.join("stray.txt").exists());
    assert!(
        h.home.join(".cache/fontconfig/c").exists(),
        "excluded top-level dirs are kept"
    );
    let restored = rusqlite::Connection::open(&db).unwrap();
    let v: String = restored
        .query_row("SELECT v FROM t", [], |r| r.get(0))
        .unwrap();
    assert_eq!(v, "kept");
    let ids: Vec<String> = agents
        .list(authed(pb::ListAgentsRequest {}))
        .await
        .unwrap()
        .into_inner()
        .agents
        .into_iter()
        .map(|a| a.id)
        .collect();
    assert!(ids.contains(&keeper));
    assert!(
        !ids.contains(&intruder),
        "agent.db was not restored: {ids:?}"
    );
}

#[tokio::test]
async fn quiesced_plan_saves_the_desktop_session_and_survives_a_failed_save() {
    let h = Harness::start(Vec::new(), false).await;
    let log = h.session_save.with_extension("log");
    std::fs::write(h.workspace.join("notes.txt"), b"x").unwrap();

    plan(&h, pb::Consistency::Live, "").await;
    assert!(!log.exists(), "a live plan saved the desktop session");
    plan(&h, pb::Consistency::Quiesced, "").await;
    assert_eq!(std::fs::read_to_string(&log).unwrap(), "saved\n");

    write_script(&h.session_save, "echo no session manager >&2; exit 1");
    let p = plan(&h, pb::Consistency::Quiesced, "").await;
    assert!(p.files >= 1, "{p:?}");
    assert_eq!(std::fs::read_to_string(&log).unwrap(), "saved\n");
}

#[tokio::test]
async fn restore_rejects_tampered_chunks_before_touching_files() {
    let h = Harness::start(Vec::new(), false).await;
    let (base, objects) = fake_store().await;
    std::fs::write(h.workspace.join("a.txt"), noise(10_000, 3)).unwrap();
    let planned = plan(&h, pb::Consistency::Live, "").await;
    upload_missing(&h, &base, &objects, &planned).await;
    std::fs::write(h.workspace.join("a.txt"), b"current").unwrap();
    {
        let mut store = objects.lock().unwrap();
        let victim = store
            .keys()
            .find(|k| **k != planned.snapshot_id)
            .unwrap()
            .clone();
        let body = store.get_mut(&victim).unwrap();
        let last = body.len() - 1;
        body[last] ^= 0x01;
    }
    let err = restore(&h, &base, &objects, &planned.snapshot_id)
        .await
        .unwrap_err();
    assert!(err.message().contains("failed authentication"), "{err:?}");
    assert_eq!(
        std::fs::read(h.workspace.join("a.txt")).unwrap(),
        b"current"
    );

    let wrong_key = state_client(&h)
        .await
        .plan(authed(pb::PlanRequest {
            key: Some(pb::StateKey {
                key: vec![1u8; 8].into(),
                key_id: "short".into(),
            }),
            ..Default::default()
        }))
        .await
        .unwrap_err();
    assert_eq!(wrong_key.code(), Code::InvalidArgument);
}

#[tokio::test]
async fn restore_rejects_a_manifest_whose_chunks_never_all_got_urls() {
    let h = Harness::start(Vec::new(), false).await;
    let (base, objects) = fake_store().await;
    std::fs::write(h.workspace.join("a.txt"), noise(10_000, 3)).unwrap();
    std::fs::write(h.workspace.join("b.txt"), noise(10_000, 4)).unwrap();
    let planned = plan(&h, pb::Consistency::Live, "").await;
    upload_missing(&h, &base, &objects, &planned).await;
    std::fs::write(h.workspace.join("a.txt"), b"current").unwrap();
    let withheld = &planned.chunks[0].id;
    let gets = planned
        .chunks
        .iter()
        .filter(|c| c.id != *withheld)
        .map(|c| pb::PresignedGet {
            chunk_id: c.id.clone(),
            url: format!("{base}/{}", c.id),
        })
        .collect();
    let err = restore_pages(&h, &base, &planned.snapshot_id, gets)
        .await
        .unwrap_err();
    assert_eq!(err.code(), Code::InvalidArgument, "{err:?}");
    assert!(
        err.message()
            .contains(&format!("no url for chunk {withheld}")),
        "{err:?}"
    );
    assert_eq!(
        std::fs::read(h.workspace.join("a.txt")).unwrap(),
        b"current"
    );
}

#[tokio::test]
async fn restore_fetches_a_page_before_the_stream_ends() {
    let h = Harness::start(Vec::new(), false).await;
    let (base, objects) = fake_store().await;
    std::fs::write(h.workspace.join("a.txt"), noise(10_000, 3)).unwrap();
    let planned = plan(&h, pb::Consistency::Live, "").await;
    upload_missing(&h, &base, &objects, &planned).await;
    let get = |id: &str| pb::PresignedGet {
        chunk_id: id.into(),
        url: format!("{base}/{id}"),
    };
    let (tx, rx) = tokio::sync::mpsc::channel(4);
    tx.send(pb::RestoreRequest {
        key: Some(state_key()),
        manifest: Some(get(&planned.snapshot_id)),
        gets: vec![get(&planned.chunks[0].id)],
    })
    .await
    .unwrap();
    let mut stream = state_client(&h)
        .await
        .restore(authed(tokio_stream::wrappers::ReceiverStream::new(rx)))
        .await
        .unwrap()
        .into_inner();
    let early = tokio::time::timeout(Duration::from_secs(10), stream.message())
        .await
        .unwrap()
        .unwrap()
        .unwrap();
    assert_eq!(early.done_chunks, 1, "{early:?}");
    assert_eq!(early.total_chunks as usize, planned.chunks.len());
    for c in &planned.chunks[1..] {
        tx.send(pb::RestoreRequest {
            gets: vec![get(&c.id)],
            ..Default::default()
        })
        .await
        .unwrap();
    }
    drop(tx);
    let mut last = early;
    while let Some(p) = stream.message().await.unwrap() {
        last = p;
    }
    assert!(last.finished, "{last:?}");
    assert_eq!(last.manifest_id, planned.snapshot_id);
}

#[tokio::test]
async fn live_snapshot_skips_a_file_that_keeps_changing() {
    let h = Harness::start(Vec::new(), false).await;
    let (base, objects) = fake_store().await;
    let growing = h.workspace.join("growing.bin");
    std::fs::write(&growing, noise(16 << 20, 5)).unwrap();
    std::fs::write(h.workspace.join("steady.txt"), b"steady").unwrap();
    let stop = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let writer = {
        let stop = Arc::clone(&stop);
        let growing = growing.clone();
        std::thread::spawn(move || {
            use std::io::Write;
            let mut f = std::fs::OpenOptions::new()
                .append(true)
                .open(growing)
                .unwrap();
            while !stop.load(std::sync::atomic::Ordering::Relaxed) {
                f.write_all(b"x").unwrap();
                std::thread::sleep(Duration::from_millis(1));
            }
        })
    };
    let planned = plan(&h, pb::Consistency::Live, "").await;
    stop.store(true, std::sync::atomic::Ordering::Relaxed);
    writer.join().unwrap();
    upload_missing(&h, &base, &objects, &planned).await;
    let m = manifest_of(&objects, &planned.snapshot_id);
    assert_eq!(m["skipped"], json!(["workspace/growing.bin"]));
    let paths: Vec<&str> = m["files"]
        .as_array()
        .unwrap()
        .iter()
        .map(|f| f["path"].as_str().unwrap())
        .collect();
    assert!(
        paths.contains(&"steady.txt") && !paths.contains(&"growing.bin"),
        "{paths:?}"
    );
}

#[tokio::test]
async fn upload_can_span_several_calls_with_the_manifest_last() {
    let h = Harness::start(Vec::new(), false).await;
    let (base, objects) = fake_store().await;
    std::fs::write(h.workspace.join("big.bin"), noise(6 << 20, 11)).unwrap();
    let planned = plan(&h, pb::Consistency::Live, "").await;
    let ids: Vec<String> = planned.chunks.iter().map(|c| c.id.clone()).collect();
    assert!(ids.len() >= 3, "{}", ids.len());
    let put = |id: &str| pb::PresignedPut {
        chunk_id: id.into(),
        url: format!("{base}/{id}"),
        headers: HashMap::new(),
    };
    let (first, rest) = ids.split_at(ids.len() / 2);
    let call = |puts: Vec<pb::PresignedPut>, manifest: Option<pb::PresignedPut>| {
        let snapshot_id = planned.snapshot_id.clone();
        let h = &h;
        async move {
            let mut stream = state_client(h)
                .await
                .upload(authed(pb::UploadRequest {
                    snapshot_id,
                    key: Some(state_key()),
                    puts,
                    manifest,
                }))
                .await
                .unwrap()
                .into_inner();
            let mut last = None;
            while let Some(p) = stream.message().await.unwrap() {
                last = Some(p);
            }
            last.unwrap()
        }
    };
    let part = call(first.iter().map(|id| put(id)).collect(), None).await;
    assert!(!part.finished);
    assert_eq!(part.done_chunks as usize, first.len());
    let done = call(
        rest.iter().map(|id| put(id)).collect(),
        Some(put(&planned.snapshot_id)),
    )
    .await;
    assert!(done.finished);
    assert_eq!(done.manifest_id, planned.snapshot_id);
    assert_eq!(objects.lock().unwrap().len(), ids.len() + 1);
    let again = state_client(&h)
        .await
        .upload(authed(pb::UploadRequest {
            snapshot_id: planned.snapshot_id.clone(),
            key: Some(state_key()),
            puts: Vec::new(),
            manifest: Some(put(&planned.snapshot_id)),
        }))
        .await
        .unwrap()
        .into_inner()
        .message()
        .await
        .unwrap_err();
    assert_eq!(
        again.code(),
        Code::InvalidArgument,
        "the snapshot is unpinned after the manifest"
    );
}

#[tokio::test]
async fn plan_reuses_unchanged_files_and_rereads_a_rewrite_that_keeps_size_and_mtime() {
    let h = Harness::start(Vec::new(), false).await;
    let (base, objects) = fake_store().await;
    let a = h.workspace.join("a.txt");
    std::fs::write(&a, noise(10_000, 1)).unwrap();
    std::fs::write(h.workspace.join("b.txt"), noise(10_000, 2)).unwrap();
    std::fs::write(h.workspace.join("c.txt"), noise(10_000, 3)).unwrap();
    tokio::time::sleep(Duration::from_millis(1200)).await;
    let first = plan(&h, pb::Consistency::Live, "").await;
    upload_missing(&h, &base, &objects, &first).await;
    let index = known_files(&h);
    for f in ["workspace/a.txt", "workspace/b.txt", "workspace/c.txt"] {
        assert!(index.get(f).is_some(), "{f} not indexed: {index}");
    }
    let m1 = manifest_of(&objects, &first.snapshot_id);
    assert!(
        m1["files"]
            .as_array()
            .unwrap()
            .iter()
            .all(|f| f.get("ctime_ns").is_none())
    );

    let mtime = std::fs::metadata(&a).unwrap().modified().unwrap();
    std::fs::write(&a, noise(10_000, 4)).unwrap();
    std::fs::File::options()
        .write(true)
        .open(&a)
        .unwrap()
        .set_modified(mtime)
        .unwrap();
    assert_eq!(std::fs::metadata(&a).unwrap().modified().unwrap(), mtime);
    let known = h
        .home
        .parent()
        .unwrap()
        .join("state/state/known-chunks.json");
    let mut forged: Value = serde_json::from_slice(&std::fs::read(&known).unwrap()).unwrap();
    forged["files"]["workspace/c.txt"]["chunks"] =
        forged["files"]["workspace/b.txt"]["chunks"].clone();
    std::fs::write(&known, serde_json::to_vec(&forged).unwrap()).unwrap();
    tokio::time::sleep(Duration::from_millis(1200)).await;
    std::fs::write(h.workspace.join("fresh.txt"), b"fresh").unwrap();

    let second = plan(&h, pb::Consistency::Quiesced, &first.snapshot_id).await;
    let sent = upload_missing(&h, &base, &objects, &second).await;
    let m2 = manifest_of(&objects, &second.snapshot_id);
    assert_ne!(
        chunks_of(&m2, "a.txt"),
        chunks_of(&m1, "a.txt"),
        "a rewrite with the same size and mtime changes ctime and is reread"
    );
    assert_eq!(chunks_of(&m2, "b.txt"), chunks_of(&m1, "b.txt"));
    assert_eq!(
        chunks_of(&m2, "c.txt"),
        chunks_of(&m1, "b.txt"),
        "missed by design: an unchanged size, mtime and ctime reuses the recorded chunks unread"
    );
    assert!(sent <= 3, "sent {sent} chunks");
    let index = known_files(&h);
    assert!(index.get("workspace/a.txt").is_some(), "{index}");
    assert!(
        index.get("workspace/fresh.txt").is_none(),
        "a file changed within a second of the walk is never reused: {index}"
    );

    let fresh_root = plan(&h, pb::Consistency::Live, "").await;
    upload_missing(&h, &base, &objects, &fresh_root).await;
    let m3 = manifest_of(&objects, &fresh_root.snapshot_id);
    assert_eq!(
        chunks_of(&m3, "c.txt"),
        chunks_of(&m1, "c.txt"),
        "a plan without a parent reads every file"
    );
    restore(&h, &base, &objects, &second.snapshot_id)
        .await
        .unwrap();
    assert_eq!(std::fs::read(&a).unwrap(), noise(10_000, 4));
}

#[tokio::test]
async fn restore_fetches_a_chunk_shared_by_several_files_once() {
    let h = Harness::start(Vec::new(), false).await;
    let (base, objects) = fake_store().await;
    let big = noise(6 << 20, 21);
    let small = noise(5_000, 22);
    std::fs::create_dir_all(h.workspace.join("many")).unwrap();
    for name in ["big1.bin", "big2.bin"] {
        std::fs::write(h.workspace.join(name), &big).unwrap();
    }
    for i in 0..30 {
        std::fs::write(h.workspace.join(format!("many/s{i:02}")), &small).unwrap();
    }
    std::fs::write(h.workspace.join("own.txt"), noise(7_000, 23)).unwrap();
    let planned = plan(&h, pb::Consistency::Live, "").await;
    upload_missing(&h, &base, &objects, &planned).await;
    let m = manifest_of(&objects, &planned.snapshot_id);
    let mut order: Vec<String> = Vec::new();
    for id in m["files"]
        .as_array()
        .unwrap()
        .iter()
        .chain([&m["agent_db"]])
        .flat_map(|f| f["chunks"].as_array().cloned().unwrap_or_default())
    {
        let id = id.as_str().unwrap().to_owned();
        if !order.contains(&id) {
            order.push(id);
        }
    }
    let planned_order: Vec<String> = planned.chunks.iter().map(|c| c.id.clone()).collect();
    assert_eq!(planned_order, order, "plan lists chunks in manifest order");
    assert!(chunks_of(&m, "big1.bin").as_array().unwrap().len() >= 3);

    std::fs::remove_dir_all(h.workspace.join("many")).unwrap();
    std::fs::write(h.workspace.join("big2.bin"), b"changed").unwrap();
    let done = restore(&h, &base, &objects, &planned.snapshot_id)
        .await
        .unwrap();
    assert!(done.finished);
    assert_eq!(done.total_chunks as usize, planned.chunks.len());
    assert_eq!(
        done.done_chunks, done.total_chunks,
        "every chunk is fetched exactly once"
    );
    for name in ["big1.bin", "big2.bin"] {
        assert_eq!(std::fs::read(h.workspace.join(name)).unwrap(), big);
    }
    for i in 0..30 {
        assert_eq!(
            std::fs::read(h.workspace.join(format!("many/s{i:02}"))).unwrap(),
            small
        );
    }
    assert_eq!(
        std::fs::read(h.workspace.join("own.txt")).unwrap(),
        noise(7_000, 23)
    );
    let work = h.home.parent().unwrap().join("state/state");
    let leftovers: Vec<_> = std::fs::read_dir(&work)
        .unwrap()
        .map(|e| e.unwrap().file_name())
        .filter(|n| n.to_string_lossy().starts_with("restore-"))
        .collect();
    assert!(leftovers.is_empty(), "{leftovers:?}");
}
