//! Environment configuration for both subcommands.

use std::env;
use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::time::Duration;

use crate::plugins::{self, Plugin};
use crate::toolhost::state::Root;

#[cfg(unix)]
const DEFAULT_STATE_DIR: &str = "/var/lib/cocoonbox";
#[cfg(windows)]
const DEFAULT_STATE_DIR: &str = r"C:\ProgramData\cocoonbox\state";
const DEFAULT_OFFLOAD_SOCKET: &str = "/run/cocoonbox/offload.sock";
#[cfg(unix)]
const DEFAULT_WORKSPACE: &str = "/workspace";
#[cfg(windows)]
const DEFAULT_WORKSPACE: &str = r"C:\workspace";
#[cfg(unix)]
const DEFAULT_HOME: &str = "/home/user";
#[cfg(windows)]
const DEFAULT_HOME: &str = r"C:\Users\user";
#[cfg(unix)]
const DEFAULT_CHROME: &str = "google-chrome";
#[cfg(windows)]
const DEFAULT_CHROME: &str = r"C:\Program Files\Google\Chrome\Application\chrome.exe";
#[cfg(unix)]
const IDENTITY_PATH: &str = "/run/cocoonbox-identity/identity";
#[cfg(windows)]
const IDENTITY_PATH: &str = r"C:\ProgramData\cocoonbox\identity\identity";
const ROOT_UID: &str = "0";
#[cfg(target_os = "linux")]
const DESKTOP_UNIT: Option<&str> = Some("cocoonbox-session.service");
#[cfg(not(target_os = "linux"))]
const DESKTOP_UNIT: Option<&str> = None;
const SESSION_SAVE: &str = "/usr/local/lib/cocoonbox/session-save";
#[cfg(unix)]
const DESK_GUIDE: &str = "/opt/desk/AGENTS.md";
#[cfg(windows)]
const DESK_GUIDE: &str = r"C:\ProgramData\cocoonbox\desk\AGENTS.md";
const ANDROID_GUIDE: &str = "/opt/android/AGENTS.md";
#[cfg(unix)]
const TOOLS_DIR: &str = "/etc/cocoonbox/tools.d";
#[cfg(windows)]
const TOOLS_DIR: &str = r"C:\ProgramData\cocoonbox\tools.d";
#[cfg(unix)]
const ROUTING_DIR: &str = "/etc/cocoonbox/routing.d";
#[cfg(windows)]
const ROUTING_DIR: &str = r"C:\ProgramData\cocoonbox\routing.d";
const MCP_IDLE: Duration = Duration::from_secs(600);
const MCP_PRUNE_AFTER: Duration = Duration::from_secs(24 * 3600);
const QUIESCE_WAIT: Duration = Duration::from_secs(30);
const EFFORTS: [&str; 7] = ["none", "minimal", "low", "medium", "high", "xhigh", "max"];

/// The desktop automation of the box's image family, found by its guide at start.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum Automation {
    /// No desktop automation: shell and files only, on a box whose image has no guide.
    #[default]
    Minimal,
    /// The desktop on shuttle, with GUI plans and scripts on Node with the desk API: the cbcore family.
    Desk,
    /// Android in the box, driven by shuttle's adb backend: the cbdroid family.
    Android,
}

impl Automation {
    /// The automation whose guide the image holds.
    pub fn detect() -> Self {
        if Path::new(ANDROID_GUIDE).exists() {
            Self::Android
        } else if Path::new(DESK_GUIDE).exists() {
            Self::Desk
        } else {
            Self::Minimal
        }
    }

    /// Whether the box runs GUI plans on its desktop.
    pub fn plans(self) -> bool {
        self == Self::Desk
    }

    /// Whether the box runs GUI plans and scripts on the desk API: a desk box, and an Android box on shuttle.
    pub fn scripts(self) -> bool {
        self != Self::Minimal
    }
}

/// Settings of `serve`, read from `/etc/cocoonbox/agent.env` via systemd.
pub struct ServeConfig {
    pub gw_base_url: String,
    pub gw_api_key: String,
    pub model_main: String,
    pub model_computer: String,
    pub identity_path: PathBuf,
    pub identity_owner: u32,
    pub listen: SocketAddr,
    pub state_dir: PathBuf,
    pub toolhost_socket: PathBuf,
    /// The offload inbox, whose changes become `offload` events.
    pub offload_socket: PathBuf,
    pub anthropic_messages: bool,
    pub mcp_idle: Duration,
    pub mcp_prune_after: Duration,
    /// How long a quiesce waits for running turns and MCP calls before it interrupts the share calls.
    pub quiesce_wait: Duration,
    pub state_roots: Vec<Root>,
    pub desktop_unit: Option<String>,
    /// The Connect box door; without it the loop offers no connector tools.
    pub connect_url: Option<String>,
    /// The box's desktop automation, which adds GUI plans, a script tool and its guide to MCP.
    pub automation: Automation,
    /// Script tools the image adds; a box that runs GUI plans offers them.
    pub plugins: Vec<Plugin>,
    /// Paragraphs the image appends to the box agent's routing and, within their cap, to the MCP instructions.
    pub routing_extra: Vec<String>,
    /// shuttle's desk endpoint, which takes the desk calls and runs straight from serve.
    pub shuttle_socket: PathBuf,
    /// The workspace that relative script paths of runs on shuttle start from.
    pub workspace: PathBuf,
    /// The turn time after which the main loop asks once for a saved first version; none never asks.
    pub checkpoint_nudge: Option<Duration>,
    /// The reasoning effort the main model and the computer subagent ask gw for; none leaves the model default.
    pub reasoning_effort: Option<&'static str>,
}

impl ServeConfig {
    /// Reads the contract keys; optional overrides exist for tests and debugging.
    pub fn from_env() -> Result<Self, String> {
        let listen = required("AGENT_LISTEN")?;
        let shuttle_socket = required("SHUTTLE_SOCKET")?;
        let workspace = optional("CB_WORKSPACE", DEFAULT_WORKSPACE);
        Ok(Self {
            gw_base_url: required("GW_BASE_URL")?.trim_end_matches('/').to_owned(),
            gw_api_key: required("GW_API_KEY")?,
            model_main: required("MODEL_MAIN")?,
            model_computer: required("MODEL_COMPUTER")?,
            identity_path: optional("CB_IDENTITY_PATH", IDENTITY_PATH).into(),
            identity_owner: optional("CB_IDENTITY_OWNER", ROOT_UID)
                .parse()
                .map_err(|e| format!("parse CB_IDENTITY_OWNER: {e}"))?,
            listen: listen
                .parse()
                .map_err(|e| format!("parse AGENT_LISTEN {listen}: {e}"))?,
            state_dir: optional("CB_STATE_DIR", DEFAULT_STATE_DIR).into(),
            toolhost_socket: optional("TOOLHOST_SOCKET", &shuttle_socket).into(),
            offload_socket: optional("OFFLOAD_SOCKET", DEFAULT_OFFLOAD_SOCKET).into(),
            anthropic_messages: optional("GW_ANTHROPIC_WIRE", "chat") == "messages",
            mcp_idle: MCP_IDLE,
            mcp_prune_after: MCP_PRUNE_AFTER,
            quiesce_wait: QUIESCE_WAIT,
            state_roots: crate::state::default_roots(
                Path::new(&optional("CB_HOME", DEFAULT_HOME)),
                Path::new(&workspace),
            ),
            desktop_unit: DESKTOP_UNIT.map(str::to_owned),
            connect_url: env::var("CONNECT_URL").ok().filter(|v| !v.is_empty()),
            automation: Automation::detect(),
            plugins: plugins::load(Path::new(&optional("TOOLS_DIR", TOOLS_DIR))),
            routing_extra: routing_extra(Path::new(&optional("ROUTING_DIR", ROUTING_DIR))),
            shuttle_socket: shuttle_socket.into(),
            workspace: workspace.into(),
            checkpoint_nudge: minutes(
                "CB_CHECKPOINT_NUDGE_MIN",
                &optional("CB_CHECKPOINT_NUDGE_MIN", "0"),
            )?,
            reasoning_effort: effort("CB_REASONING_EFFORT", &optional("CB_REASONING_EFFORT", ""))?,
        })
    }
}

/// Settings of `toolhost-service`.
pub struct ToolhostConfig {
    pub socket: PathBuf,
    pub workspace: PathBuf,
    pub chrome_bin: String,
    pub chrome_profile: PathBuf,
    pub cdp_port: u16,
    pub session_save: PathBuf,
    pub home: PathBuf,
    /// The box's desktop automation.
    pub automation: Automation,
    /// Script tools the image adds, each run by its own program.
    pub plugins: Vec<Plugin>,
    /// shuttle's desk endpoint, which answers the desk verbs, model calls and runs.
    pub shuttle_socket: PathBuf,
}

impl ToolhostConfig {
    /// Reads toolhost settings: the two endpoints shuttle sets, and keys with a contract default.
    pub fn from_env() -> Result<Self, String> {
        let home = env::var("HOME")
            .or_else(|_| env::var("USERPROFILE"))
            .map_err(|e| format!("read HOME: {e}"))?;
        Ok(Self {
            socket: required("SHUTTLE_SERVICE_SOCKET")?.into(),
            shuttle_socket: required("SHUTTLE_SOCKET")?.into(),
            workspace: optional("CB_WORKSPACE", DEFAULT_WORKSPACE).into(),
            chrome_bin: optional("CHROME_BIN", DEFAULT_CHROME),
            chrome_profile: env::var("CHROME_PROFILE").map_or_else(
                |_| format!("{home}/.config/cocoonbox-chrome").into(),
                Into::into,
            ),
            cdp_port: optional("CDP_PORT", "9222")
                .parse()
                .map_err(|e| format!("parse CDP_PORT: {e}"))?,
            session_save: SESSION_SAVE.into(),
            home: home.into(),
            automation: Automation::detect(),
            plugins: plugins::load(Path::new(&optional("TOOLS_DIR", TOOLS_DIR))),
        })
    }
}

/// The `*.md` files in `dir` in file-name order, each trimmed at its end; none without any.
pub fn routing_extra(dir: &Path) -> Vec<String> {
    let mut parts = Vec::new();
    for file in plugins::sorted_files(dir, "md") {
        match std::fs::read_to_string(&file) {
            Ok(part) => parts.push(part.trim_end().to_owned()),
            Err(e) => eprintln!("routing: skip {}: {e}", file.display()),
        }
    }
    parts
}

fn required(key: &str) -> Result<String, String> {
    match env::var(key) {
        Ok(v) if !v.is_empty() => Ok(v),
        _ => Err(format!("missing required env {key}")),
    }
}

/// Parses `raw`, the value of `key`, as whole minutes; 0 is none.
fn minutes(key: &str, raw: &str) -> Result<Option<Duration>, String> {
    let n: u64 = raw.parse().map_err(|e| format!("parse {key}: {e}"))?;
    Ok((n > 0).then(|| Duration::from_secs(n * 60)))
}

/// Parses `raw`, the value of `key`, as one of gw's reasoning efforts; empty is none.
fn effort(key: &str, raw: &str) -> Result<Option<&'static str>, String> {
    if raw.is_empty() {
        return Ok(None);
    }
    match EFFORTS.iter().find(|e| **e == raw) {
        Some(e) => Ok(Some(e)),
        None => Err(format!(
            "parse {key}: {raw} is not one of {}",
            EFFORTS.join(", ")
        )),
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
    fn checkpoint_minutes_are_off_at_zero_and_refuse_junk() {
        assert_eq!(minutes("K", "0"), Ok(None));
        assert_eq!(minutes("K", "40"), Ok(Some(Duration::from_secs(2400))));
        assert!(minutes("K", "forty").is_err());
        assert!(minutes("K", "-1").is_err());
    }

    #[test]
    fn reasoning_effort_is_off_when_empty_and_refuses_words_gw_does_not_take() {
        assert_eq!(effort("K", ""), Ok(None));
        assert_eq!(effort("K", "medium"), Ok(Some("medium")));
        assert_eq!(effort("K", "max"), Ok(Some("max")));
        assert!(effort("K", "Medium").is_err());
        assert!(effort("K", "4096").is_err());
    }

    #[test]
    fn routing_extra_joins_the_md_files_in_file_name_order() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("20-b.md"), "second\n").unwrap();
        std::fs::write(dir.path().join("10-a.md"), "first").unwrap();
        std::fs::write(dir.path().join("30-c.txt"), "ignored").unwrap();
        assert_eq!(routing_extra(dir.path()), ["first", "second"]);
        assert!(routing_extra(Path::new("/nonexistent/routing.d")).is_empty());
    }
}
