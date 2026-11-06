use std::path::{Path, PathBuf};
use std::time::Duration;

use crate::config::{Automation, ServeConfig};

const SERVED_POLL: Duration = Duration::from_millis(10);

#[cfg(unix)]
pub fn endpoint(dir: &Path, name: &str) -> PathBuf {
    dir.join(name)
}

#[cfg(windows)]
pub fn endpoint(dir: &Path, name: &str) -> PathBuf {
    let dir = dir.file_name().unwrap_or_default().to_string_lossy();
    format!(r"\\.\pipe\cocoonbox-test-{dir}-{name}").into()
}

pub async fn served(path: &Path) {
    while crate::toolhost::rpc::connect(path).await.is_err() {
        tokio::time::sleep(SERVED_POLL).await;
    }
}

pub fn config(dir: &Path, url: &str) -> ServeConfig {
    ServeConfig {
        gw_base_url: url.to_owned(),
        gw_api_key: "test".into(),
        model_main: "test".into(),
        model_computer: "test".into(),
        identity_path: dir.join("identity"),
        identity_owner: 0,
        listen: "127.0.0.1:0".parse().unwrap(),
        state_dir: dir.to_owned(),
        toolhost_socket: endpoint(dir, "toolhost"),
        offload_socket: dir.join("offload"),
        anthropic_messages: false,
        mcp_idle: Duration::from_secs(600),
        mcp_prune_after: Duration::from_secs(86400),
        quiesce_wait: Duration::from_secs(1),
        state_roots: Vec::new(),
        desktop_unit: None,
        connect_url: None,
        automation: Automation::Minimal,
        plugins: Vec::new(),
        routing_extra: Vec::new(),
        shuttle_socket: endpoint(dir, "shuttle"),
        workspace: dir.to_owned(),
        checkpoint_nudge: None,
        reasoning_effort: None,
    }
}
