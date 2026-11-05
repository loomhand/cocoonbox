#![cfg(test)]
#![cfg(unix)]

mod common;
#[path = "common/serve.rs"]
mod serve;

use std::time::{Duration, Instant};

use cocoonbox_agent::pb::agents_client::AgentsClient;
use cocoonbox_agent::pb::session_client::SessionClient;
use cocoonbox_agent::pb::{self, event, transcript_entry::Body};
use cocoonbox_agent::store::Store;
use serde_json::json;
use tokio::net::TcpListener;

use common::authed;

async fn hanging_gw() -> String {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap().to_string();
    tokio::spawn(async move {
        let mut held = Vec::new();
        while let Ok((stream, _)) = listener.accept().await {
            held.push(stream);
        }
    });
    addr
}

#[tokio::test]
async fn sigterm_with_open_streams_and_a_running_turn_exits_quickly() {
    let dir = tempfile::tempdir().unwrap();
    let gw = format!("http://{}/v1", hanging_gw().await);
    let (mut child, channel, endpoint) = serve::spawn_serve(dir.path(), &gw).await;
    let mut agents = AgentsClient::new(channel.clone());
    let mut session = SessionClient::new(channel);

    let mut events = session
        .events(authed(pb::EventsRequest { from_offset: 0 }))
        .await
        .unwrap()
        .into_inner();

    let http = reqwest::Client::new();
    let init = http
        .post(format!("{endpoint}/mcp"))
        .header("x-cocoonbox-credential", common::owner())
        .json(&json!({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"clientInfo": {"name": "t"}}}))
        .send()
        .await
        .unwrap();
    let sid = init.headers()["mcp-session-id"]
        .to_str()
        .unwrap()
        .to_owned();
    let mut mcp_stream = http
        .get(format!("{endpoint}/mcp"))
        .header("x-cocoonbox-credential", common::owner())
        .header("mcp-session-id", &sid)
        .send()
        .await
        .unwrap();
    assert!(mcp_stream.chunk().await.unwrap().is_some());

    let agent_id = agents
        .create(authed(pb::CreateAgentRequest::default()))
        .await
        .unwrap()
        .into_inner()
        .id;
    agents
        .send(authed(pb::SendRequest {
            agent_id: agent_id.clone(),
            text: "hang".into(),
            ..Default::default()
        }))
        .await
        .unwrap();
    tokio::time::timeout(serve::UP, async {
        loop {
            let ev = events.message().await.unwrap().unwrap();
            if let Some(event::Body::Agent(a)) = ev.body
                && a.status == i32::from(pb::AgentStatus::Running)
            {
                break;
            }
        }
    })
    .await
    .expect("the turn did not start");

    let pid = child.id().unwrap().to_string();
    let started = Instant::now();
    let kill = std::process::Command::new("kill")
        .args(["-TERM", &pid])
        .status()
        .unwrap();
    assert!(kill.success());
    let status = tokio::time::timeout(Duration::from_secs(3), child.wait())
        .await
        .expect("agent did not exit within 3s of SIGTERM")
        .unwrap();
    let took = started.elapsed();
    assert!(status.success(), "exit status {status}");

    let ended = tokio::time::timeout(Duration::from_secs(1), async {
        while let Ok(Some(_)) = events.message().await {}
    })
    .await;
    assert!(ended.is_ok(), "events stream stayed open");

    let store = Store::open(&dir.path().join("state").join("agent.db")).unwrap();
    let page = store.transcript(agent_id, 0, 100).await.unwrap();
    let end = page
        .entries
        .iter()
        .find_map(|e| match &e.body {
            Some(Body::TurnEnd(end)) => Some(end),
            _ => None,
        })
        .expect("the running turn got no TurnEnd");
    assert!(!end.ok);
    assert_eq!(end.error, "interrupted: the agent is shutting down");
    let listed = store.list_agents().await.unwrap();
    assert!(
        listed.iter().all(|a| !a.name.starts_with("mcp:")),
        "the owner's MCP session made an agent: {listed:?}"
    );
    eprintln!("agent exited {took:?} after SIGTERM");
}
