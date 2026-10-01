//! Environment configuration for both subcommands.

use std::env;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::time::Duration;

use crate::toolhost::state::Root;

const DEFAULT_STATE_DIR: &str = "/var/lib/cocoonbox";
const DEFAULT_TOOLHOST_SOCKET: &str = "/run/cocoonbox/toolhost.sock";
const DEFAULT_VNC_ADDR: &str = "127.0.0.1:5900";
const DEFAULT_WORKSPACE: &str = "/workspace";
const IDENTITY_PATH: &str = "/run/cocoonbox-identity/identity";
const ROOT_UID: u32 = 0;
const REDACTED: &str = "<redacted>";
const DESKTOP_UNIT: &str = "cocoonbox-session.service";
const SESSION_SAVE: &str = "/usr/local/lib/cocoonbox/session-save";
const MCP_IDLE: Duration = Duration::from_secs(600);
const MCP_PRUNE_AFTER: Duration = Duration::from_secs(24 * 3600);

/// Settings of `serve`, read from `/etc/cocoonbox/agent.env` via systemd.
#[derive(Clone)]
pub struct ServeConfig {
    pub gw_base_url: String,
    pub gw_api_key: String,
    pub model_main: String,
    pub model_computer: String,
    pub edge_token: Option<String>,
    pub identity_path: PathBuf,
    pub identity_owner: u32,
    pub listen: SocketAddr,
    pub state_dir: PathBuf,
    pub toolhost_socket: PathBuf,
    pub vnc_addr: String,
    pub anthropic_messages: bool,
    pub mcp_idle: Duration,
    pub mcp_prune_after: Duration,
    pub state_roots: Vec<Root>,
    pub desktop_unit: Option<String>,
}

impl std::fmt::Debug for ServeConfig {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ServeConfig")
            .field("gw_base_url", &self.gw_base_url)
            .field("gw_api_key", &REDACTED)
            .field("model_main", &self.model_main)
            .field("model_computer", &self.model_computer)
            .field("edge_token", &self.edge_token.as_ref().map(|_| REDACTED))
            .field("identity_path", &self.identity_path)
            .field("identity_owner", &self.identity_owner)
            .field("listen", &self.listen)
            .field("state_dir", &self.state_dir)
            .field("toolhost_socket", &self.toolhost_socket)
            .field("vnc_addr", &self.vnc_addr)
            .field("anthropic_messages", &self.anthropic_messages)
            .field("mcp_idle", &self.mcp_idle)
            .field("mcp_prune_after", &self.mcp_prune_after)
            .field("state_roots", &self.state_roots)
            .field("desktop_unit", &self.desktop_unit)
            .finish()
    }
}

impl ServeConfig {
    /// Reads the contract keys; optional overrides exist for tests and debugging.
    pub fn from_env() -> Result<Self, String> {
        let listen = required("AGENT_LISTEN")?;
        Ok(Self {
            gw_base_url: required("GW_BASE_URL")?.trim_end_matches('/').to_owned(),
            gw_api_key: required("GW_API_KEY")?,
            model_main: required("MODEL_MAIN")?,
            model_computer: required("MODEL_COMPUTER")?,
            edge_token: env::var("EDGE_TOKEN").ok().filter(|v| !v.is_empty()),
            identity_path: IDENTITY_PATH.into(),
            identity_owner: ROOT_UID,
            listen: listen
                .parse()
                .map_err(|e| format!("parse AGENT_LISTEN {listen}: {e}"))?,
            state_dir: optional("CB_STATE_DIR", DEFAULT_STATE_DIR).into(),
            toolhost_socket: optional("TOOLHOST_SOCKET", DEFAULT_TOOLHOST_SOCKET).into(),
            vnc_addr: optional("VNC_ADDR", DEFAULT_VNC_ADDR),
            anthropic_messages: optional("GW_ANTHROPIC_WIRE", "chat") == "messages",
            mcp_idle: MCP_IDLE,
            mcp_prune_after: MCP_PRUNE_AFTER,
            state_roots: crate::state::default_roots(),
            desktop_unit: Some(DESKTOP_UNIT.to_owned()),
        })
    }
}

/// Settings of `toolhost`.
#[derive(Clone, Debug)]
pub struct ToolhostConfig {
    pub socket: PathBuf,
    pub socket_group: String,
    pub workspace: PathBuf,
    pub chrome_bin: String,
    pub chrome_profile: PathBuf,
    pub cdp_port: u16,
    pub cua_socket: PathBuf,
    pub session_save: PathBuf,
}

impl ToolhostConfig {
    /// Reads toolhost settings; every key has a contract default.
    pub fn from_env() -> Result<Self, String> {
        let home = env::var("HOME").map_err(|e| format!("read HOME: {e}"))?;
        Ok(Self {
            socket: optional("TOOLHOST_SOCKET", DEFAULT_TOOLHOST_SOCKET).into(),
            socket_group: optional("TOOLHOST_SOCKET_GROUP", "cbagent"),
            workspace: optional("CB_WORKSPACE", DEFAULT_WORKSPACE).into(),
            chrome_bin: optional("CHROME_BIN", "google-chrome"),
            chrome_profile: env::var("CHROME_PROFILE").map_or_else(
                |_| format!("{home}/.config/cocoonbox-chrome").into(),
                Into::into,
            ),
            cdp_port: 9222,
            cua_socket: env::var("CUA_DRIVER_SOCKET").map_or_else(
                |_| format!("{home}/.cache/cua-driver/cua-driver.sock").into(),
                Into::into,
            ),
            session_save: SESSION_SAVE.into(),
        })
    }
}

fn required(key: &str) -> Result<String, String> {
    match env::var(key) {
        Ok(v) if !v.is_empty() => Ok(v),
        _ => Err(format!("missing required env {key}")),
    }
}

fn optional(key: &str, default: &str) -> String {
    env::var(key)
        .ok()
        .filter(|v| !v.is_empty())
        .unwrap_or_else(|| default.to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn debug_redacts_secrets() {
        let cfg = ServeConfig {
            gw_base_url: "http://gw/v1".into(),
            gw_api_key: "sk-secret-key".into(),
            model_main: "m".into(),
            model_computer: "c".into(),
            edge_token: Some("edge-secret".into()),
            identity_path: "/i".into(),
            identity_owner: 0,
            listen: "127.0.0.1:1".parse().unwrap(),
            state_dir: "/s".into(),
            toolhost_socket: "/t".into(),
            vnc_addr: "v".into(),
            anthropic_messages: false,
            mcp_idle: MCP_IDLE,
            mcp_prune_after: MCP_PRUNE_AFTER,
            state_roots: Vec::new(),
            desktop_unit: None,
        };
        let shown = format!("{cfg:?}");
        assert!(
            !shown.contains("sk-secret-key") && !shown.contains("edge-secret"),
            "{shown}"
        );
        assert!(
            shown.contains("<redacted>") && shown.contains("http://gw/v1"),
            "{shown}"
        );
    }
}
