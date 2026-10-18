#![cfg(test)]

use std::time::Duration;

use cocoonbox_agent::toolhost::rpc::{Call, Client};
use serde_json::json;

const SCRIPT: &str = r"
const { createConnection } = require('node:net')
const body = Buffer.from(JSON.stringify({ agent_id: 'shell', tool: 'desktop_info', args: {} }))
const head = Buffer.alloc(4)
head.writeUInt32BE(body.length)
const s = createConnection(process.env.TOOLHOST_SOCKET)
let got = Buffer.alloc(0)
s.on('connect', () => s.write(Buffer.concat([head, body])))
s.on('data', (d) => {
  got = Buffer.concat([got, d])
  if (got.length >= 4 && got.length >= 4 + got.readUInt32BE(0)) { s.destroy(); console.log('reply ' + got.subarray(4).toString()) }
})
s.on('error', (e) => console.log('failed ' + e))
";

#[tokio::test]
async fn a_node_script_in_the_agent_shell_reaches_the_toolhost_it_runs_under() {
    let Some(node) = std::env::var_os("NODE_BIN") else {
        eprintln!("SKIP: no Node; set NODE_BIN to run");
        return;
    };
    let dir = tempfile::tempdir().unwrap();
    let socket = dir.path().join("toolhost.sock");
    let _toolhost = tokio::process::Command::new(env!("CARGO_BIN_EXE_cocoonbox-agent"))
        .arg("toolhost")
        .env("TOOLHOST_SOCKET", &socket)
        .env("TOOLHOST_SOCKET_GROUP", "no-such-group")
        .env("CB_WORKSPACE", dir.path())
        .env("HOME", dir.path())
        .kill_on_drop(true)
        .spawn()
        .unwrap();
    for _ in 0..500 {
        if socket.exists() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
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
}
