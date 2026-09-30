#![cfg(test)]

use std::collections::{HashMap, VecDeque};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use cocoonbox_agent::config::{ServeConfig, ToolhostConfig};
use cocoonbox_agent::pb::agents_client::AgentsClient;
use cocoonbox_agent::pb::session_client::SessionClient;
use cocoonbox_agent::pb::{self, event, screen_client_frame, transcript_entry::Body};
use cocoonbox_agent::runner::Runtime;
use cocoonbox_agent::{server, toolhost};
use serde_json::{Value, json};
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
    workspace: PathBuf,
    endpoint: String,
    gw_log: Log,
    cua_log: Log,
}

impl Harness {
    async fn start(script: Vec<(&'static str, String)>, anthropic_messages: bool) -> Self {
        let dir = tempfile::tempdir().unwrap();
        let workspace = dir.path().join("workspace");
        std::fs::create_dir_all(&workspace).unwrap();
        let gw_log = Log::default();
        let gw = mock_gw(script, Arc::clone(&gw_log)).await;
        let cua_log = Log::default();
        let cua_socket = dir.path().join("cua.sock");
        fake_cua(&cua_socket, Arc::clone(&cua_log));
        let toolhost_socket = dir.path().join("toolhost.sock");
        let th_cfg = ToolhostConfig {
            socket: toolhost_socket.clone(),
            socket_group: "no-such-group".into(),
            workspace: workspace.clone(),
            chrome_bin: "false".into(),
            chrome_profile: dir.path().join("chrome"),
            cdp_port: 1,
            cua_socket,
        };
        tokio::spawn(async move { toolhost::run(th_cfg).await });
        let cfg = ServeConfig {
            gw_base_url: format!("http://{gw}/v1"),
            gw_api_key: "k".into(),
            model_main: "deepseek/deepseek-v4.1-flash".into(),
            model_computer: "anthropic/claude-sonnet-5".into(),
            edge_token: TOKEN.into(),
            listen: "127.0.0.1:0".parse().unwrap(),
            state_dir: dir.path().join("state"),
            toolhost_socket: toolhost_socket.clone(),
            vnc_addr: fake_vnc().await,
            anthropic_messages,
        };
        let rt = Runtime::open(cfg).await.unwrap();
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let endpoint = format!("http://{}", listener.local_addr().unwrap());
        tokio::spawn(server::serve_on(listener, rt, std::future::pending()));
        for _ in 0..100 {
            if toolhost_socket.exists() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        Self {
            _dir: dir,
            workspace,
            endpoint,
            gw_log,
            cua_log,
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
    let tool_msg = msgs.iter().find(|m| m["role"] == "tool").unwrap();
    assert_eq!(tool_msg["tool_call_id"], "call_1");
    assert_eq!(msgs.iter().filter(|m| m["role"] == "user").count(), 1);

    let mut offsets = Vec::new();
    let mut transcript_seqs = Vec::new();
    while transcript_seqs.len() < entries.len() {
        let ev = tokio::time::timeout(Duration::from_secs(5), events.message())
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        offsets.push(ev.offset);
        if let Some(event::Body::Transcript(t)) = ev.body {
            transcript_seqs.push(t.seq);
        }
    }
    assert_eq!(transcript_seqs, seqs);
    assert!(offsets.windows(2).all(|w| w[1] == w[0] + 1), "{offsets:?}");

    let mut replay = session
        .events(authed(pb::EventsRequest {
            from_offset: offsets[1],
        }))
        .await
        .unwrap()
        .into_inner();
    let mut replayed = Vec::new();
    for _ in 2..offsets.len() {
        replayed.push(replay.message().await.unwrap().unwrap().offset);
    }
    assert_eq!(replayed, offsets[2..]);
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
