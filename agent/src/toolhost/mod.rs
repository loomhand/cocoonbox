//! The tool host: runs every tool as the desktop user behind a unix socket.

pub mod browser;
pub mod cua;
pub mod files;
pub mod rpc;
pub mod shell;

use std::collections::HashMap;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use base64::Engine;
use base64::engine::general_purpose::STANDARD as B64;
use serde_json::Value;
use tokio::io::AsyncReadExt;
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::Mutex;

use crate::config::ToolhostConfig;
use browser::Browser;
use cua::Cua;
use rpc::{Call, Output};
use shell::Shell;

const SOCKET_MODE: u32 = 0o660;
const SCRATCH_DIR: &str = "/tmp/cocoonbox-shell";

/// Shared tool state for all connections.
pub struct Host {
    workspace: PathBuf,
    scratch: PathBuf,
    shells: Mutex<HashMap<String, Arc<Mutex<Option<Shell>>>>>,
    browser: Browser,
    cua: Cua,
}

impl Host {
    /// Creates a host from its configuration.
    pub fn new(cfg: &ToolhostConfig) -> Self {
        Self {
            workspace: cfg.workspace.clone(),
            scratch: PathBuf::from(SCRATCH_DIR),
            shells: Mutex::new(HashMap::new()),
            browser: Browser::new(
                cfg.chrome_bin.clone(),
                cfg.chrome_profile.clone(),
                cfg.cdp_port,
            ),
            cua: Cua::new(cfg.cua_socket.clone()),
        }
    }

    /// Executes one call; failures become error outputs for the model.
    pub async fn dispatch(&self, call: Call) -> Output {
        let result = match call.tool.as_str() {
            "shell" => self
                .shell(&call.agent_id, call.args)
                .await
                .map(Output::text),
            "read_file" => files::read(&self.workspace, call.args)
                .await
                .map(Output::text),
            "write_file" => files::write(&self.workspace, call.args)
                .await
                .map(Output::text),
            "edit_file" => files::edit(&self.workspace, call.args)
                .await
                .map(Output::text),
            "list_dir" => files::list(&self.workspace, call.args)
                .await
                .map(Output::text),
            "upload" => self.upload(call.args).await.map(Output::text),
            t if t.starts_with("browser_") => {
                self.browser.run(t, call.args).await.map(Output::text)
            }
            "computer_state" => self.cua.state(call.args).await,
            "computer_act" => self.cua.act(call.args).await,
            "screenshot" => self.cua.screenshot().await.map(|image_b64| Output {
                text: String::new(),
                is_error: false,
                image_b64,
            }),
            other => Err(format!("unknown tool {other}")),
        };
        result.unwrap_or_else(Output::error)
    }

    async fn shell(&self, agent_id: &str, args: Value) -> Result<String, String> {
        let command = args["command"]
            .as_str()
            .ok_or("shell needs a command string")?;
        let timeout = args["timeout_s"].as_u64().map(Duration::from_secs);
        let slot = {
            let mut shells = self.shells.lock().await;
            Arc::clone(shells.entry(agent_id.to_owned()).or_default())
        };
        let mut slot = slot.lock().await;
        if slot.as_ref().is_none_or(Shell::is_dead) {
            tokio::fs::create_dir_all(&self.scratch)
                .await
                .map_err(|e| format!("create {}: {e}", self.scratch.display()))?;
            let _ =
                tokio::fs::set_permissions(&self.scratch, std::fs::Permissions::from_mode(0o700))
                    .await;
            let script = self.scratch.join(format!("{}.sh", safe_name(agent_id)));
            *slot = Some(Shell::spawn(&self.workspace, script).await?);
        }
        let shell = slot.as_mut().ok_or("shell missing")?;
        shell.run(command, timeout).await
    }

    async fn upload(&self, args: Value) -> Result<String, String> {
        let path = files::resolve(
            &self.workspace,
            args["path"].as_str().ok_or("upload needs path")?,
        );
        let data = B64
            .decode(args["data_b64"].as_str().unwrap_or_default())
            .map_err(|e| format!("decode upload: {e}"))?;
        files::write_bytes(&path, &data).await?;
        Ok(path.display().to_string())
    }
}

/// Runs the toolhost until the process is stopped.
pub async fn run(cfg: ToolhostConfig) -> Result<(), String> {
    let listener = bind(&cfg.socket, &cfg.socket_group)?;
    eprintln!("toolhost listening on {}", cfg.socket.display());
    let host = Arc::new(Host::new(&cfg));
    loop {
        let (stream, _) = listener
            .accept()
            .await
            .map_err(|e| format!("accept: {e}"))?;
        tokio::spawn(serve_conn(Arc::clone(&host), stream));
    }
}

/// Serves one request; the client closing its end cancels the tool.
pub async fn serve_conn(host: Arc<Host>, stream: UnixStream) {
    let (mut rd, mut wr) = stream.into_split();
    let call: Call = match rpc::read_frame(&mut rd).await {
        Ok(c) => c,
        Err(e) => {
            eprintln!("toolhost: bad request: {e}");
            return;
        }
    };
    let mut probe = [0u8; 1];
    let out = tokio::select! {
        out = host.dispatch(call) => out,
        _ = rd.read(&mut probe) => return,
    };
    if let Err(e) = rpc::write_frame(&mut wr, &out).await {
        eprintln!("toolhost: reply: {e}");
    }
}

fn bind(path: &Path, group: &str) -> Result<UnixListener, String> {
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent).map_err(|e| format!("create {}: {e}", parent.display()))?;
    }
    let _ = std::fs::remove_file(path);
    let listener = UnixListener::bind(path).map_err(|e| format!("bind {}: {e}", path.display()))?;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(SOCKET_MODE))
        .map_err(|e| format!("chmod {}: {e}", path.display()))?;
    if let Some(gid) = group_id(group) {
        std::os::unix::fs::chown(path, None, Some(gid))
            .map_err(|e| format!("chgrp {} {group}: {e}", path.display()))?;
    }
    Ok(listener)
}

fn group_id(name: &str) -> Option<u32> {
    let groups = std::fs::read_to_string("/etc/group").ok()?;
    groups.lines().find_map(|line| {
        let mut f = line.split(':');
        (f.next()? == name).then_some(())?;
        f.nth(1)?.parse().ok()
    })
}

fn safe_name(s: &str) -> String {
    s.chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || c == '-' {
                c
            } else {
                '_'
            }
        })
        .collect()
}
