//! The tool host: runs every tool as the desktop user behind a unix socket.

pub mod atspi;
pub mod browser;
pub mod cua;
pub mod desk;
pub mod exec;
pub mod files;
pub mod flows;
pub mod rpc;
pub mod scope;
pub mod script;
pub mod shell;
pub mod state;
pub mod timing;
pub mod volatile;
pub mod xshm;
pub mod xtest;

use std::collections::HashMap;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;
use std::time::Duration;

use base64::Engine;
use base64::engine::general_purpose::STANDARD as B64;
use serde_json::Value;
use tokio::io::AsyncReadExt;
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::{Mutex, watch};

use crate::config::{Automation, ToolhostConfig};
use browser::Browser;
use cua::Cua;
use desk::Desk;
use rpc::{Call, Output};
use script::Scripts;
use shell::Shell;

const SOCKET_MODE: u32 = 0o660;
const SCRATCH_DIR: &str = "/tmp/cocoonbox-shell";
const SCRIPT_DIR: &str = "/tmp/cocoonbox-scripts";
#[cfg(feature = "script")]
const SCRIPT_DIR: &str = "/tmp/cocoonbox-script";

/// Shared tool state for all connections.
pub struct Host {
    workspace: PathBuf,
    scratch: PathBuf,
    shells: Mutex<HashMap<String, Arc<Mutex<Option<Shell>>>>>,
    browser: Browser,
    cua: Cua,
    desk: Desk,
    scripts: Scripts,
    session_save: PathBuf,
    home: PathBuf,
    /// The saved flows of a box that runs GUI plans, which a read-only share does not read.
    hidden: Option<PathBuf>,
    flow_writes: watch::Sender<u64>,
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
            cua: Cua::new(cfg.cua_socket.clone(), cfg.automation.plans()),
            desk: Desk::default(),
            scripts: Scripts::new(
                cfg.automation,
                cfg.script_bin.clone(),
                script_dir(cfg.automation),
            ),
            session_save: cfg.session_save.clone(),
            home: cfg.home.clone(),
            hidden: cfg.automation.plans().then(|| cfg.workspace.join("flows")),
            flow_writes: watch::channel(0).0,
        }
    }

    /// Executes one call; failures become error outputs for the model.
    pub async fn dispatch(&self, call: Call) -> Output {
        let result = match call.tool.as_str() {
            "shell" => self
                .shell(&call.agent_id, call.args)
                .await
                .map(Output::text),
            "shell_close" => self.close_shell(&call.agent_id).await.map(Output::text),
            "read_file" => files::read(&self.workspace, self.hidden.as_deref(), call.args)
                .await
                .map(Output::text),
            "write_file" => files::write(&self.workspace, call.args)
                .await
                .map(Output::text),
            "edit_file" => files::edit(&self.workspace, call.args)
                .await
                .map(Output::text),
            "list_dir" => files::list(&self.workspace, self.hidden.as_deref(), call.args)
                .await
                .map(Output::text),
            "upload" => self.upload(call.args).await.map(Output::text),
            "state_walk" => state::walk(call.args).await.map(Output::text),
            "state_apply" => state::apply(call.args).await.map(Output::text),
            "state_swap" => state::swap(call.args).await.map(Output::text),
            "state_place" => state::place(call.args).await.map(Output::text),
            "desktop_save" => self.desktop_save().await.map(Output::text),
            "active_origin" => self.browser.origin().await.map(Output::text),
            "credential_fill" => {
                let mut args = call.args;
                let out = self.browser.fill(&args).await.map(Output::text);
                rpc::wipe(&mut args);
                out
            }
            t if t.starts_with("browser_") => self
                .browser
                .run(&call.agent_id, t, call.args)
                .await
                .map(Output::text),
            "computer_state" => self.cua.state(call.args).await,
            "computer_act" => self.cua.act(&self.browser, &call.agent_id, call.args).await,
            "computer_end" => self.cua.end(call.args).await.map(Output::text),
            t if t.starts_with("desk_") => self.desk.run(&self.cua, t, call.args).await,
            #[cfg(feature = "script")]
            "script_run" => {
                self.scripts
                    .run(&self.cua, &self.workspace, call.args)
                    .await
            }
            "script_run" => {
                self.scripts
                    .run(&self.cua, &self.workspace, call.args)
                    .await
            }
            "gui_plan" => {
                self.scripts
                    .plan(&self.cua, &self.workspace, call.args)
                    .await
            }
            "gui_flows" => Ok(Output::text(Scripts::flows(&self.workspace).await)),
            "desk" => {
                let op = call.args["op"].as_str().unwrap_or_default().to_owned();
                self.desk
                    .run(&self.cua, &format!("desk_{op}"), call.args)
                    .await
            }
            "flows_scan" => flows::scan(&self.workspace).await.map(Output::text),
            "flows_read" => flows::read(&self.workspace, call.args)
                .await
                .map(Output::text),
            "flows_write" => flows::write(&self.workspace, call.args)
                .await
                .map(Output::text),
            "flows_remove" => flows::remove(&self.workspace, call.args)
                .await
                .map(Output::text),
            "flows_mark" => flows::mark(&self.workspace, call.args)
                .await
                .map(Output::text),
            "flows_index" => self.scripts.reindex(&self.workspace).await,
            "flows_wait" => flows::wait(&self.flow_writes, call.args)
                .await
                .map(Output::text),
            "screenshot" => {
                let max_edge = call.args["max_edge"]
                    .as_u64()
                    .and_then(|e| u32::try_from(e).ok());
                let shot = match call.args["session"].as_str() {
                    Some(session) => self.cua.screenshot(session, max_edge).await,
                    None => self.cua.screenshot_once(max_edge).await,
                };
                shot.map(|image_b64| Output {
                    text: if image_b64.is_some() {
                        "screenshot of the whole screen".to_owned()
                    } else {
                        "no screenshot returned".to_owned()
                    },
                    is_error: false,
                    image_b64,
                    ..Output::default()
                })
            }
            other => Err(format!("unknown tool {other}")),
        };
        let out = result.unwrap_or_else(Output::error);
        timing::note("error", out.is_error);
        out
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
            *slot = Some(Shell::spawn(&self.workspace, self.script(agent_id)).await?);
        }
        let shell = slot.as_mut().ok_or("shell missing")?;
        shell.run(command, timeout).await
    }

    async fn close_shell(&self, agent_id: &str) -> Result<String, String> {
        let slot = self.shells.lock().await.remove(agent_id);
        if let Some(slot) = slot
            && let Some(mut shell) = slot.lock().await.take()
        {
            shell.kill();
        }
        let _ = tokio::fs::remove_file(self.script(agent_id)).await;
        Ok("closed".to_owned())
    }

    fn script(&self, agent_id: &str) -> PathBuf {
        self.scratch.join(format!("{}.sh", safe_name(agent_id)))
    }

    async fn desktop_save(&self) -> Result<String, String> {
        let out = tokio::process::Command::new(&self.session_save)
            .stdin(Stdio::null())
            .output()
            .await
            .map_err(|e| format!("run {}: {e}", self.session_save.display()))?;
        if out.status.success() {
            Ok("saved".to_owned())
        } else {
            Err(format!(
                "{}: {}: {}",
                self.session_save.display(),
                out.status,
                String::from_utf8_lossy(&out.stderr).trim()
            ))
        }
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

/// Runs the toolhost until SIGTERM or SIGINT, which closes Chrome with its agent windows first.
pub async fn run(cfg: ToolhostConfig) -> Result<(), String> {
    let listener = bind(&cfg.socket, &cfg.socket_group)?;
    eprintln!("toolhost listening on {}", cfg.socket.display());
    let host = Arc::new(Host::new(&cfg));
    if cfg.automation.plans() {
        let (workspace, writes) = (cfg.workspace.clone(), host.flow_writes.clone());
        // detached: lives as long as the toolhost
        tokio::spawn(async move {
            if let Err(e) = flows::watch(workspace, writes).await {
                eprintln!("toolhost: watch the flows: {e}");
            }
        });
    }
    let stop = crate::server::shutdown_signal();
    tokio::pin!(stop);
    loop {
        tokio::select! {
            () = &mut stop => {
                let closed = host.browser.run("toolhost", "browser_close", Value::Null).await;
                eprintln!("toolhost stopping: browser_close: {closed:?}");
                return Ok(());
            }
            accepted = listener.accept() => {
                let (stream, _) = accepted.map_err(|e| format!("accept: {e}"))?;
                tokio::spawn(serve_conn(Arc::clone(&host), stream));
            }
        }
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
    match call.tool.as_str() {
        "state_read" => {
            if let Err(e) = state::read(call.args, &mut wr).await {
                eprintln!("toolhost: state_read: {e}");
            }
            return;
        }
        "exec" => {
            exec::run(&host.workspace, &host.home, &call.args, rd, wr).await;
            return;
        }
        "state_write" => {
            let out = state::write(call.args, &mut rd).await;
            if let Err(e) = rpc::write_frame(&mut wr, &out).await {
                eprintln!("toolhost: reply: {e}");
            }
            return;
        }
        _ => {}
    }
    let mut probe = [0u8; 1];
    let tool = call.tool.clone();
    let out = tokio::select! {
        out = timing::scope(&tool, host.dispatch(call)) => out,
        _ = rd.read(&mut probe) => return,
    };
    if let Err(e) = rpc::write_frame(&mut wr, &out).await {
        eprintln!("toolhost: reply: {e}");
    }
}

/// Binds a unix socket at `path`, mode 0660, owned by `group` when it exists.
pub(crate) fn bind(path: &Path, group: &str) -> Result<UnixListener, String> {
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

fn script_dir(automation: Automation) -> PathBuf {
    match automation {
        #[cfg(feature = "script")]
        Automation::Script => PathBuf::from(SCRIPT_DIR),
        _ => PathBuf::from(SCRIPT_DIR),
    }
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
