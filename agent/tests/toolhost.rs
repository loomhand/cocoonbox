#![cfg(test)]
#![cfg(unix)]

#[path = "common/toolhost.rs"]
mod toolhost;

use std::sync::Arc;
use std::time::Duration;

use cocoonbox_agent::toolhost::rpc::{Call, Client, Output, read_frame, write_frame};
use serde_json::{Value, json};
use tokio::io::AsyncReadExt;
use tokio::net::UnixListener;
use tokio::sync::{Notify, mpsc};

use toolhost::spawn_toolhost;

const SCRIPT: &str = r"
const { createConnection } = require('node:net')
const body = Buffer.from(JSON.stringify({ agent_id: 'shell', tool: 'desk_windows', args: {} }))
const head = Buffer.alloc(4)
head.writeUInt32BE(body.length)
const s = createConnection(process.env.SHUTTLE_SOCKET)
let got = Buffer.alloc(0)
s.on('connect', () => s.write(Buffer.concat([head, body])))
s.on('data', (d) => {
  got = Buffer.concat([got, d])
  if (got.length >= 4 && got.length >= 4 + got.readUInt32BE(0)) { s.destroy(); console.log('reply ' + got.subarray(4).toString()) }
})
s.on('error', (e) => console.log('failed ' + e))
";

#[tokio::test]
async fn a_node_script_in_the_agent_shell_reaches_shuttle_at_shuttle_socket() {
    let Some(node) = std::env::var_os("NODE_BIN") else {
        eprintln!("SKIP: no Node; set NODE_BIN to run");
        return;
    };
    let dir = tempfile::tempdir().unwrap();
    let desk = dir.path().join("desk.sock");
    let listener = UnixListener::bind(&desk).unwrap();
    let (seen, mut calls) = mpsc::unbounded_channel::<String>();
    tokio::spawn(async move {
        loop {
            let (mut conn, _) = listener.accept().await.unwrap();
            let call: Call = read_frame(&mut conn).await.unwrap();
            seen.send(call.tool).unwrap();
            write_frame(&mut conn, &Output::text("[]")).await.unwrap();
        }
    });
    let (_toolhost, socket) = spawn_toolhost(dir.path(), |_| {}).await;
    std::fs::write(dir.path().join("probe.js"), SCRIPT).unwrap();
    let command = format!(
        "\"{}\" {}",
        node.to_string_lossy(),
        dir.path().join("probe.js").display()
    );
    let out = Client::new(socket)
        .call(&Call {
            agent_id: "t".into(),
            tool: "shell".into(),
            args: json!({"command": command}),
        })
        .await
        .unwrap();
    assert!(!out.is_error, "{}", out.text);
    assert!(
        out.text.contains("reply {") && out.text.contains("\"is_error\":false"),
        "{}",
        out.text
    );
    assert_eq!(calls.recv().await.unwrap(), "desk_windows");
}

#[tokio::test]
async fn the_toolhost_relays_desk_and_model_calls_and_drops_a_call_its_caller_dropped() {
    let dir = tempfile::tempdir().unwrap();
    let desk = dir.path().join("desk.sock");
    let (seen, mut calls) = mpsc::unbounded_channel::<(Call, bool)>();
    let arrived = Arc::new(Notify::new());
    let held = Arc::clone(&arrived);
    let listener = UnixListener::bind(&desk).unwrap();
    tokio::spawn(async move {
        loop {
            let (mut conn, _) = listener.accept().await.unwrap();
            let (seen, held) = (seen.clone(), Arc::clone(&held));
            tokio::spawn(async move {
                let call: Call = read_frame(&mut conn).await.unwrap();
                let out = match call.tool.as_str() {
                    "desk_windows" => Output {
                        text: "[]".into(),
                        is_error: false,
                        image_b64: Some("aW1n".into()),
                        secrets: vec![("number".into(), "4242".into())],
                    },
                    _ if call.args["hold"] == true => {
                        held.notify_one();
                        let mut probe = [0u8; 1];
                        let closed = conn.read(&mut probe).await.unwrap() == 0;
                        seen.send((call, closed)).unwrap();
                        return;
                    }
                    _ => Output::text("{}"),
                };
                seen.send((call, false)).unwrap();
                write_frame(&mut conn, &out).await.unwrap();
            });
        }
    });
    let (_toolhost, socket) = spawn_toolhost(dir.path(), |_| {}).await;
    let th = Client::new(socket.clone());
    let call = |tool: &str, args: Value| Call {
        agent_id: "ag".into(),
        tool: tool.into(),
        args,
    };

    let out = th.call(&call("desk_windows", json!({}))).await.unwrap();
    assert_eq!(
        (out.text.as_str(), out.image_b64.as_deref(), out.secrets),
        ("[]", Some("aW1n"), vec![("number".into(), "4242".into())])
    );
    let (got, _) = calls.recv().await.unwrap();
    assert_eq!(
        (got.agent_id.as_str(), got.tool.as_str()),
        ("ag", "desk_windows")
    );

    th.call(&call("desk", json!({"op": "click", "x": 1, "y": 2})))
        .await
        .unwrap();
    let (got, _) = calls.recv().await.unwrap();
    assert_eq!(
        (got.tool.as_str(), &got.args["x"]),
        ("desk_click", &json!(1))
    );

    th.call(&call("model_ask", json!({"prompt": "p"})))
        .await
        .unwrap();
    assert_eq!(calls.recv().await.unwrap().0.tool, "model_ask");

    let held = call("model_ask", json!({"hold": true}));
    let pending = tokio::spawn(async move { Client::new(socket).call(&held).await });
    tokio::time::timeout(Duration::from_secs(5), arrived.notified())
        .await
        .unwrap();
    pending.abort();
    let (got, closed) = tokio::time::timeout(Duration::from_secs(5), calls.recv())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(got.tool, "model_ask");
    assert!(closed, "a dropped call closes its shuttle connection");
}
