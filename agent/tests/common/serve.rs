use std::path::Path;
use std::process::Stdio;
use std::time::Duration;

use tokio::net::TcpListener;
use tokio::process::{Child, Command};
use tonic::transport::Channel;

pub const UP: Duration = Duration::from_secs(30);

pub async fn spawn_serve(dir: &Path, gw_url: &str) -> (Child, Channel, String) {
    let port = free_port().await;
    let identity = dir.join("identity").join("identity");
    crate::common::provision(&identity);
    let child = Command::new(env!("CARGO_BIN_EXE_cocoonbox-agent"))
        .arg("serve")
        .env("GW_BASE_URL", gw_url)
        .env("GW_API_KEY", "k")
        .env("MODEL_MAIN", "deepseek/deepseek-v4.1-flash")
        .env("MODEL_COMPUTER", "anthropic/claude-sonnet-5")
        .env("AGENT_LISTEN", format!("127.0.0.1:{port}"))
        .env("CB_STATE_DIR", dir.join("state"))
        .env("CB_IDENTITY_PATH", &identity)
        .env(
            "CB_IDENTITY_OWNER",
            std::os::unix::fs::MetadataExt::uid(&dir.metadata().unwrap()).to_string(),
        )
        .env("TOOLHOST_SOCKET", dir.join("toolhost.sock"))
        .env("SHUTTLE_SOCKET", dir.join("desk.sock"))
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .spawn()
        .unwrap();
    let endpoint = format!("http://127.0.0.1:{port}");
    let channel = tokio::time::timeout(UP, async {
        loop {
            if let Ok(ch) = Channel::from_shared(endpoint.clone())
                .unwrap()
                .connect()
                .await
            {
                break ch;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
    })
    .await
    .expect("serve did not come up");
    (child, channel, endpoint)
}

async fn free_port() -> u16 {
    TcpListener::bind("127.0.0.1:0")
        .await
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}
