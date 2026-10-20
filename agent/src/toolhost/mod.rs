//! The tool host: runs every tool as the desktop user behind a unix socket.

mod act;
mod browser;
pub mod desk;
mod exec;
pub(crate) mod files;
pub(crate) mod flows;
mod ground;
pub mod rpc;
mod script;
mod shell;
pub mod state;
mod timing;

use std::collections::{HashMap, HashSet};
use std::io;
#[cfg(unix)]
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;
use std::time::Duration;

use base64::Engine;
use base64::engine::general_purpose::STANDARD as B64;
use serde_json::{Value, json};
use tokio::io::AsyncReadExt;
#[cfg(windows)]
use tokio::net::windows::named_pipe::NamedPipeServer;
#[cfg(unix)]
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::{Mutex, watch};

use crate::config::{Automation, ToolhostConfig};
use crate::runs::Shots;
use browser::Browser;
use desk::Desk;
use rpc::{Call, Client, Output};
use script::{Screen, Scripts, ShuttleScreen};
use shell::Shell;

#[cfg(unix)]
const SOCKET_MODE: u32 = 0o660;

/// What creating the first instance of a pipe that exists fails with.
#[cfg(windows)]
const ERROR_ACCESS_DENIED: i32 = 5;

/// One accepted toolhost connection.
#[cfg(unix)]
type Conn = UnixStream;
/// One accepted toolhost connection.
#[cfg(windows)]
type Conn = NamedPipeServer;
const SCRATCH_DIR: &str = "cocoonbox-shell";
const SCRIPT_DIR: &str = "/tmp/cocoonbox-scripts";
const OS_RELEASE: &str = "/etc/os-release";
const SYSTEM_VERSION: &str = "/System/Library/CoreServices/SystemVersion.plist";
const DESKTOP_VAR: &str = "XDG_CURRENT_DESKTOP";
const SCREEN_WAIT: Duration = Duration::from_secs(2);

/// Shared tool state for all connections.
struct Host {
    workspace: PathBuf,
    scratch: PathBuf,
    shells: Mutex<HashMap<String, Arc<Mutex<Option<Shell>>>>>,
    browser: Browser,
    /// An Android box, whose screen, desk and script tools run on shuttle.
    android: bool,
    desk: Desk,
    scripts: Scripts,
    /// The plugin script tools, on a box that runs GUI plans.
    plugins: HashSet<String>,
    session_save: PathBuf,
    home: PathBuf,
    /// A box that runs GUI plans.
    plans: bool,
    /// The saved flows of a box that runs GUI plans, which a read-only share does not read.
    hidden: Option<PathBuf>,
    flow_writes: watch::Sender<u64>,
    /// shuttle's daemon, which answers the desk verbs, model calls and runs of a box that runs it.
    shuttle: Option<Client>,
}

impl Host {
    /// Creates a host from its configuration.
    fn new(cfg: &ToolhostConfig) -> Self {
        let shuttle = cfg.shuttle_socket.clone().map(Client::new);
        Self {
            workspace: cfg.workspace.clone(),
            scratch: scratch_dir(),
            shells: Mutex::new(HashMap::new()),
            browser: Browser::new(
                cfg.chrome_bin.clone(),
                cfg.chrome_profile.clone(),
                cfg.cdp_port,
                cfg.automation.plans(),
            ),
            android: cfg.automation == Automation::Android,
            desk: Desk::new(shuttle.clone()),
            scripts: Scripts::new(PathBuf::from(SCRIPT_DIR), shuttle.clone()),
            plugins: cfg
                .plugins
                .iter()
                .filter(|_| cfg.automation.plans())
                .map(|p| p.name.clone())
                .collect(),
            session_save: cfg.session_save.clone(),
            home: cfg.home.clone(),
            plans: cfg.automation.plans(),
            hidden: cfg.automation.scripts().then(|| flows::dir(&cfg.workspace)),
            flow_writes: watch::channel(0).0,
            shuttle,
        }
    }

    /// Executes one call; failures become error outputs for the model.
    async fn dispatch(&self, call: Call) -> Output {
        let call = match self.shuttle_call(call).await {
            Ok(result) => {
                let out = result.unwrap_or_else(Output::error);
                timing::note("error", out.is_error);
                return out;
            }
            Err(call) => call,
        };
        let call = match self.relay(call).await {
            Ok(out) => {
                timing::note("error", out.is_error);
                return out;
            }
            Err(call) => call,
        };
        let result = match call.tool.as_str() {
            "shell" => {
                let command = call.args["command"].as_str().unwrap_or_default();
                let note = self.held(|name| files::writes(command, name)).await;
                self.shell(&call.agent_id, call.args)
                    .await
                    .map(|out| Output::text(out + &note))
            }
            "shell_close" => self.close_shell(&call.agent_id).await.map(Output::text),
            "read_file" => files::read(&self.workspace, self.hidden.as_deref(), call.args)
                .await
                .map(Output::text),
            "write_file" => {
                let path = call.args["path"].as_str().unwrap_or_default();
                let note = self.held(|name| files::names(path, name)).await;
                files::write(&self.workspace, call.args)
                    .await
                    .map(|out| Output::text(out + &note))
            }
            "edit_file" => {
                let path = call.args["path"].as_str().unwrap_or_default();
                let note = self.held(|name| files::names(path, name)).await;
                files::edit(&self.workspace, call.args)
                    .await
                    .map(|out| Output::text(out + &note))
            }
            "list_dir" => files::list(&self.workspace, self.hidden.as_deref(), call.args)
                .await
                .map(Output::text),
            "upload" => self.upload(call.args).await.map(Output::text),
            "state_walk" => state::walk(call.args).await.map(Output::text),
            "state_apply" => state::apply(call.args).await.map(Output::text),
            "state_swap" => state::swap(call.args).await.map(Output::text),
            "state_place" => state::place(call.args).await.map(Output::text),
            "desktop_save" => self.desktop_save().await.map(Output::text),
            "desktop_info" => Ok(Output::text(desktop_info(self.shuttle.as_ref()).await)),
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
            "gui_flows" => Ok(Output::text(Scripts::flows(&self.workspace).await)),
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
            "flows_index" => self.scripts.reindex().await,
            "flows_wait" => flows::wait(&self.flow_writes, call.args)
                .await
                .map(Output::text),
            other => Err(format!("unknown tool {other}")),
        };
        let out = result.unwrap_or_else(Output::error);
        timing::note("error", out.is_error);
        out
    }

    /// Runs the screen, computer and script tools of a box that runs shuttle: runs with shuttle's screen, and a desk
    /// box's `computer_act` through [`act`] or [`ground`]; every other call comes back, its screen tools under shuttle's names.
    async fn shuttle_call(&self, mut call: Call) -> Result<Result<Output, String>, Call> {
        let Some(shuttle) = &self.shuttle else {
            return Err(call);
        };
        let screen = ShuttleScreen(shuttle);
        let target = call.args.get("target").is_some_and(|t| !t.is_null());
        Ok(match call.tool.as_str() {
            "computer_act" if !self.android && target => {
                ground::act(
                    shuttle,
                    &call.agent_id,
                    &self.desk,
                    &self.scripts,
                    call.args,
                )
                .await
            }
            "computer_act" if !self.android => {
                act::run(shuttle, &self.browser, &call.agent_id, call.args).await
            }
            "run_screenshot" => self.run_screenshot(&screen, &call.args).await,
            "script_run" => self.scripts.run(&screen, &self.workspace, call.args).await,
            "gui_plan" => self.scripts.plan(&screen, &self.workspace, call.args).await,
            t if self.plugins.contains(t) => {
                self.scripts
                    .run_with(t, &screen, &self.workspace, call.args)
                    .await
            }
            _ => {
                if let Some(tool) = shuttle_tool(&call.tool) {
                    call.tool = tool;
                }
                return Err(call);
            }
        })
    }

    /// A screenshot of `screen` for a run on shuttle that serve shapes: the image, and with `keep` the path of its kept file as the text.
    async fn run_screenshot(&self, screen: &impl Screen, args: &Value) -> Result<Output, String> {
        let max_edge = args["max_edge"]
            .as_u64()
            .and_then(|e| u32::try_from(e).ok());
        let keep = args["keep"].as_bool().unwrap_or(false);
        let shot = self.scripts.kept(screen).shot(max_edge, keep).await?;
        Ok(shot.map_or_else(Output::default, |s| Output {
            text: s.path.map(|p| p.display().to_string()).unwrap_or_default(),
            image_b64: Some(s.image_b64),
            ..Output::default()
        }))
    }

    /// Sends a desk verb or model call as it came to shuttle, on a box that runs it; every other call comes back.
    async fn relay(&self, mut call: Call) -> Result<Output, Call> {
        let Some(shuttle) = &self.shuttle else {
            return Err(call);
        };
        match crate::shuttle::desk_verb(&call.tool, &call.args) {
            Some(verb) => call.tool = verb,
            None if call.tool.starts_with("model_") => {}
            None => return Err(call),
        }
        Ok(shuttle
            .desk(&call, rpc::DESK_DEADLINE)
            .await
            .unwrap_or_else(Output::error))
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
            #[cfg(unix)]
            let _ =
                tokio::fs::set_permissions(&self.scratch, std::fs::Permissions::from_mode(0o700))
                    .await;
            *slot = Some(Shell::spawn(&self.workspace, self.script(agent_id)).await?);
        }
        let shell = slot.as_mut().ok_or("shell missing")?;
        let out = shell.run(command, timeout).await?;
        Ok(if self.plans {
            out + &shell::capped_note(timeout)
        } else {
            out
        })
    }

    /// With plans, a note on the files that a call `changes` and an app window has open (see [`files::held_note`]).
    async fn held(&self, changes: impl Fn(&str) -> bool) -> String {
        let titles = match &self.shuttle {
            Some(shuttle) if self.plans => window_titles(shuttle).await,
            _ => Vec::new(),
        };
        files::held_note(&titles, changes)
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

/// The named pipe the toolhost serves: a client takes the waiting instance, and a new instance waits for the next one.
#[cfg(windows)]
pub(crate) struct Listener {
    path: PathBuf,
    security: crate::sys::PipeSecurity,
    next: NamedPipeServer,
}

/// Runs the toolhost until SIGTERM or SIGINT, which closes Chrome with its agent windows first.
pub async fn run(cfg: ToolhostConfig) -> Result<(), String> {
    let mut listener = bind(&cfg.socket, &cfg.socket_group)?;
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
            accepted = accept(&mut listener) => {
                let stream = accepted.map_err(|e| format!("accept: {e}"))?;
                tokio::spawn(serve_conn(Arc::clone(&host), stream));
            }
        }
    }
}

/// Serves one request; the client closing its end cancels the tool.
async fn serve_conn(host: Arc<Host>, stream: Conn) {
    #[cfg(unix)]
    let (mut rd, mut wr) = stream.into_split();
    #[cfg(windows)]
    let (mut rd, mut wr) = {
        let pipe = crate::sys::PipeEnd::of(&stream);
        let (half, wr) = tokio::io::split(stream);
        (exec::AgentRead { half, pipe }, wr)
    };
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

/// Waits for the next connection to `listener`.
#[cfg(unix)]
pub(crate) async fn accept(listener: &mut UnixListener) -> io::Result<Conn> {
    listener.accept().await.map(|(stream, _)| stream)
}

/// Waits for the next connection to `listener`.
#[cfg(windows)]
pub(crate) async fn accept(listener: &mut Listener) -> io::Result<Conn> {
    listener.next.connect().await?;
    let next = listener.security.create(&listener.path, false)?;
    Ok(std::mem::replace(&mut listener.next, next))
}

/// Binds a unix socket at `path`, mode 0660, owned by `group` when it exists; a socket another process still serves there stays.
#[cfg(unix)]
pub(crate) fn bind(path: &Path, group: &str) -> Result<UnixListener, String> {
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent).map_err(|e| format!("create {}: {e}", parent.display()))?;
    }
    if std::os::unix::net::UnixStream::connect(path).is_ok() {
        return Err(format!(
            "{} answers: another process serves it",
            path.display()
        ));
    }
    let _ = std::fs::remove_file(path);
    let listener = UnixListener::bind(path).map_err(|e| format!("bind {}: {e}", path.display()))?;
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(SOCKET_MODE))
        .map_err(|e| format!("chmod {}: {e}", path.display()))?;
    if let Some(gid) = crate::sys::group_id(group) {
        std::os::unix::fs::chown(path, None, Some(gid))
            .map_err(|e| format!("chgrp {} {group}: {e}", path.display()))?;
    }
    Ok(listener)
}

/// Creates the first instance of the named pipe `path`, for SYSTEM, this user and `group` when it exists; a pipe another process serves there stays.
#[cfg(windows)]
pub(crate) fn bind(path: &Path, group: &str) -> Result<Listener, String> {
    let security = crate::sys::PipeSecurity::new(group)?;
    let next = security.create(path, true).map_err(|e| {
        if e.raw_os_error() == Some(ERROR_ACCESS_DENIED) {
            format!("{} answers: another process serves it", path.display())
        } else {
            format!("create pipe {}: {e}", path.display())
        }
    })?;
    Ok(Listener {
        path: path.to_owned(),
        security,
        next,
    })
}

/// The box's OS release, desktop and the screen size shuttle reports as JSON, with the fields it could read.
async fn desktop_info(shuttle: Option<&Client>) -> String {
    let mut info = json!({});
    if let Some(os) = std::fs::read_to_string(OS_RELEASE)
        .ok()
        .and_then(|r| os_name(&r))
        .or_else(|| {
            std::fs::read_to_string(SYSTEM_VERSION)
                .ok()
                .and_then(|v| product_name(&v))
        })
        .or_else(crate::sys::os_name)
    {
        info["os"] = os.into();
    }
    let desktop = std::env::var(DESKTOP_VAR).ok().or_else(session_desktop);
    if let Some(desktop) = desktop.as_deref().and_then(desktop_name) {
        info["desktop"] = desktop.into();
    }
    let call = Call {
        agent_id: "toolhost".to_owned(),
        tool: "desk_screen".to_owned(),
        args: json!({}),
    };
    if let Some(shuttle) = shuttle
        && let Ok(out) = shuttle.desk(&call, SCREEN_WAIT).await
        && let Ok(mut screen) = serde_json::from_str::<Value>(&out.text)
    {
        info["width"] = screen["width"].take();
        info["height"] = screen["height"].take();
    }
    info.to_string()
}

/// `NAME VERSION_ID` of an os-release file, else its `PRETTY_NAME`.
fn os_name(release: &str) -> Option<String> {
    let field = |key: &str| {
        release
            .lines()
            .find_map(|l| l.strip_prefix(key)?.strip_prefix('='))
            .map(|v| v.trim().trim_matches('"'))
            .filter(|v| !v.is_empty())
    };
    match (field("NAME"), field("VERSION_ID")) {
        (Some(name), Some(version)) => Some(format!("{name} {version}")),
        _ => field("PRETTY_NAME").map(str::to_owned),
    }
}

/// `ProductName ProductVersion` of a macOS SystemVersion.plist.
fn product_name(plist: &str) -> Option<String> {
    let field = |key: &str| {
        let rest = plist.split_once(&format!("<key>{key}</key>"))?.1;
        let value = rest.split_once("<string>")?.1.split_once("</string>")?.0;
        Some(value.trim()).filter(|v| !v.is_empty())
    };
    Some(format!(
        "{} {}",
        field("ProductName")?,
        field("ProductVersion")?
    ))
}

/// The desktop of a session such as `ubuntu:GNOME` in `XDG_CURRENT_DESKTOP`: its last name.
fn desktop_name(value: &str) -> Option<&str> {
    value.rsplit(':').next().filter(|d| !d.is_empty())
}

/// `XDG_CURRENT_DESKTOP` of a process of this user, such as the desktop session's.
fn session_desktop() -> Option<String> {
    let prefix = format!("{DESKTOP_VAR}=");
    std::fs::read_dir("/proc")
        .ok()?
        .flatten()
        .find_map(|entry| {
            let environ = std::fs::read(entry.path().join("environ")).ok()?;
            environ
                .split(|b| *b == 0)
                .find_map(|var| var.strip_prefix(prefix.as_bytes()))
                .map(|v| String::from_utf8_lossy(v).into_owned())
        })
}

/// The directory of the shell's scratch files: under `/tmp`, or on Windows under the temp directory, a path with a drive
/// that Git Bash reads as it is.
fn scratch_dir() -> PathBuf {
    let root = if cfg!(windows) {
        std::env::temp_dir()
    } else {
        PathBuf::from("/tmp")
    };
    root.join(SCRATCH_DIR)
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

/// The titles of the desktop's windows shuttle lists; none when it does not answer.
async fn window_titles(shuttle: &Client) -> Vec<String> {
    let call = Call {
        agent_id: "toolhost".to_owned(),
        tool: "desk_windows".to_owned(),
        args: json!({}),
    };
    let Ok(out) = shuttle.desk(&call, rpc::DESK_DEADLINE).await else {
        return Vec::new();
    };
    let windows: Vec<Value> = serde_json::from_str(&out.text).unwrap_or_default();
    windows
        .iter()
        .filter_map(|w| w["title"].as_str().map(str::to_owned))
        .collect()
}

/// shuttle's name for a screen tool: `screenshot` and `computer_screenshot` are `desk_look`, `computer_X` is `desk_computer_X`.
fn shuttle_tool(tool: &str) -> Option<String> {
    match tool {
        "screenshot" | "computer_screenshot" => Some("desk_look".to_owned()),
        "computer_state" | "computer_act" | "computer_end" => Some(format!("desk_{tool}")),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn bind_keeps_a_socket_another_process_serves_and_replaces_a_stale_one() {
        let dir = tempfile::tempdir().unwrap();
        let path = crate::testing::endpoint(dir.path(), "desk.sock");
        let live = bind(&path, "").unwrap();
        let Err(err) = bind(&path, "") else {
            panic!("a second bind of {} succeeded", path.display());
        };
        assert!(err.ends_with("another process serves it"), "{err}");
        drop(live);
        bind(&path, "").unwrap();
    }

    #[tokio::test]
    async fn every_tool_of_the_shuttle_service_spec_is_one_the_toolhost_answers() {
        let dir = tempfile::tempdir().unwrap();
        let desk = crate::testing::endpoint(dir.path(), "desk.sock");
        let mut shuttle = bind(&desk, "").unwrap();
        tokio::spawn(async move {
            while let Ok(conn) = accept(&mut shuttle).await {
                drop(conn);
            }
        });
        let cfg = ToolhostConfig {
            socket: crate::testing::endpoint(dir.path(), "toolhost.sock"),
            socket_group: String::new(),
            workspace: dir.path().into(),
            chrome_bin: "false".into(),
            chrome_profile: dir.path().join("chrome"),
            cdp_port: 1,
            session_save: "false".into(),
            home: dir.path().into(),
            automation: Automation::Desk,
            plugins: Vec::new(),
            shuttle_socket: Some(desk),
        };
        let host = Host::new(&cfg);
        host.flow_writes.send_modify(|w| *w += 1);
        let path =
            Path::new(env!("CARGO_MANIFEST_DIR")).join("../image/desk/service/cocoonbox.json");
        let spec: Value = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
        for t in spec["tools"].as_array().unwrap() {
            let tool = t["name"].as_str().unwrap();
            if ["exec", "state_read", "state_write"].contains(&tool) {
                continue;
            }
            if tool.starts_with("browser_") {
                assert!(browser::TOOLS.contains(&tool), "{tool} is no browser tool");
                continue;
            }
            let call = Call {
                agent_id: "t".into(),
                tool: tool.into(),
                args: json!({}),
            };
            let out = tokio::time::timeout(Duration::from_secs(30), host.dispatch(call))
                .await
                .unwrap_or_else(|_| panic!("{tool} gave no answer"));
            assert!(
                !out.text.starts_with("unknown tool "),
                "{tool}: {}",
                out.text
            );
        }
    }

    #[tokio::test]
    async fn the_shell_tool_keeps_its_state_between_calls() {
        let dir = tempfile::tempdir().unwrap();
        let cfg = ToolhostConfig {
            socket: crate::testing::endpoint(dir.path(), "toolhost.sock"),
            socket_group: String::new(),
            workspace: dir.path().into(),
            chrome_bin: "false".into(),
            chrome_profile: dir.path().join("chrome"),
            cdp_port: 1,
            session_save: dir.path().join("session-save"),
            home: dir.path().into(),
            automation: Automation::Minimal,
            plugins: Vec::new(),
            shuttle_socket: None,
        };
        let host = Host::new(&cfg);
        let agent = format!("shell-test-{}", std::process::id());
        let call = |tool: &str, command: &str| Call {
            agent_id: agent.clone(),
            tool: tool.into(),
            args: json!({"command": command}),
        };
        let out = host
            .dispatch(call("shell", "mkdir sub && cd sub && echo hi"))
            .await;
        assert_eq!(out.text, "hi\n[exit status 0]", "{out:?}");
        let out = host.dispatch(call("shell", "basename \"$PWD\"")).await;
        assert_eq!(out.text, "sub\n[exit status 0]", "{out:?}");
        host.dispatch(call("shell_close", "")).await;
    }

    #[test]
    fn the_os_is_its_name_and_version_and_the_desktop_its_last_name() {
        let ubuntu = "PRETTY_NAME=\"Ubuntu 22.04.5 LTS\"\nNAME=\"Ubuntu\"\nVERSION_ID=\"22.04\"\nID=ubuntu\n";
        assert_eq!(os_name(ubuntu).as_deref(), Some("Ubuntu 22.04"));
        assert_eq!(
            os_name("PRETTY_NAME=\"Debian GNU/Linux trixie/sid\"\nNAME=\"Debian GNU/Linux\"\n")
                .as_deref(),
            Some("Debian GNU/Linux trixie/sid")
        );
        assert_eq!(os_name("ID=x\n"), None);
        let mac = "<dict>\n\t<key>ProductName</key>\n\t<string>macOS</string>\n\t<key>ProductVersion</key>\n\t<string>26.5.2</string>\n</dict>";
        assert_eq!(product_name(mac).as_deref(), Some("macOS 26.5.2"));
        assert_eq!(product_name("<dict></dict>"), None);
        assert_eq!(desktop_name("ubuntu:GNOME"), Some("GNOME"));
        assert_eq!(desktop_name("XFCE"), Some("XFCE"));
        assert_eq!(desktop_name(""), None);
    }

    const SHUTTLE_ANDROID: [&str; 19] = [
        "desk_windows",
        "desk_wait",
        "desk_snapshot",
        "desk_find",
        "desk_read",
        "desk_press",
        "desk_act",
        "desk_at",
        "desk_key",
        "desk_type",
        "desk_click",
        "desk_focus",
        "desk_close",
        "desk_launch",
        "desk_screen",
        "desk_shot",
        "desk_computer_state",
        "desk_computer_act",
        "desk_computer_end",
    ];

    #[tokio::test]
    async fn an_android_box_sends_its_screen_desk_and_run_calls_to_shuttle() {
        let dir = tempfile::tempdir().unwrap();
        let desk = crate::testing::endpoint(dir.path(), "desk.sock");
        let (seen, mut tools) = tokio::sync::mpsc::unbounded_channel::<String>();
        let mut listener = bind(&desk, "").unwrap();
        tokio::spawn(async move {
            loop {
                let mut conn = accept(&mut listener).await.unwrap();
                let call: Call = rpc::read_frame(&mut conn).await.unwrap();
                let out = match call.tool.as_str() {
                    "desk_look" => Output {
                        image_b64: Some("aW1n".into()),
                        ..Output::text("720x1280 screen")
                    },
                    "gui_plan" => Output::text(r#"{"ok":true}"#),
                    "script_run" => Output::text(r#"{"result":null,"logs":"","duration_ms":1}"#),
                    tool if SHUTTLE_ANDROID.contains(&tool) => Output::text("{}"),
                    tool => Output::error(format!("unknown desk tool {tool}")),
                };
                seen.send(call.tool).unwrap();
                rpc::write_frame(&mut conn, &out).await.unwrap();
            }
        });
        let socket = crate::testing::endpoint(dir.path(), "toolhost.sock");
        let cfg = ToolhostConfig {
            socket: socket.clone(),
            socket_group: "no-such-group".into(),
            workspace: dir.path().into(),
            chrome_bin: "false".into(),
            chrome_profile: dir.path().join("chrome"),
            cdp_port: 1,
            session_save: dir.path().join("session-save"),
            home: dir.path().into(),
            automation: Automation::Android,
            plugins: Vec::new(),
            shuttle_socket: Some(desk),
        };
        tokio::spawn(run(cfg));
        crate::testing::served(&socket).await;
        let toolhost = Client::new(socket);
        let calls = [
            ("screenshot", json!({})),
            ("computer_state", json!({"session": "cu"})),
            (
                "computer_act",
                json!({"session": "cu", "action": "tap", "x": 1, "y": 2}),
            ),
            ("computer_end", json!({"session": "cu"})),
            ("computer_screenshot", json!({})),
            ("run_screenshot", json!({})),
            ("desk_type", json!({"text": "你好"})),
            ("desk", json!({"op": "windows"})),
            (
                "gui_plan",
                json!({"steps": [{"app": "x"}], "screenshot": false}),
            ),
            ("script_run", json!({"code": "export function main() {}"})),
            (
                "gui_plan",
                json!({"steps": [{"app": "x"}], "screenshot": true}),
            ),
        ];
        for (tool, args) in calls {
            let shot = args["screenshot"] != false && matches!(tool, "script_run" | "gui_plan");
            let out = toolhost
                .call(&Call {
                    agent_id: "ag".into(),
                    tool: tool.into(),
                    args,
                })
                .await
                .unwrap();
            assert!(!out.is_error, "{tool}: {}", out.text);
            if shot {
                assert_eq!(out.image_b64.as_deref(), Some("aW1n"), "{tool}");
            }
        }
        let mut got = Vec::new();
        while let Ok(tool) = tools.try_recv() {
            got.push(tool);
        }
        assert_eq!(
            got,
            [
                "desk_look",
                "desk_computer_state",
                "desk_computer_act",
                "desk_computer_end",
                "desk_look",
                "desk_look",
                "desk_type",
                "desk_windows",
                "gui_plan",
                "script_run",
                "desk_look",
                "gui_plan",
                "desk_look"
            ]
        );
    }

    #[tokio::test]
    async fn a_desk_box_with_shuttle_sends_its_computer_tools_and_the_window_titles_there() {
        let dir = tempfile::tempdir().unwrap();
        let desk = crate::testing::endpoint(dir.path(), "desk.sock");
        let (seen, mut tools) = tokio::sync::mpsc::unbounded_channel::<(String, Value)>();
        let mut listener = bind(&desk, "").unwrap();
        tokio::spawn(async move {
            loop {
                let mut conn = accept(&mut listener).await.unwrap();
                let call: Call = rpc::read_frame(&mut conn).await.unwrap();
                let out = match call.tool.as_str() {
                    "desk_look" => Output {
                        image_b64: Some("aW1n".into()),
                        ..Output::text("screenshot")
                    },
                    "desk_windows" => Output::text(r#"[{"title": "notes.txt - Mousepad"}]"#),
                    tool if tool.starts_with("desk_computer_") => Output::text("{}"),
                    tool => Output::error(format!("unknown desk tool {tool}")),
                };
                seen.send((call.tool, call.args)).unwrap();
                rpc::write_frame(&mut conn, &out).await.unwrap();
            }
        });
        let socket = crate::testing::endpoint(dir.path(), "toolhost.sock");
        let cfg = ToolhostConfig {
            socket: socket.clone(),
            socket_group: "no-such-group".into(),
            workspace: dir.path().into(),
            chrome_bin: "false".into(),
            chrome_profile: dir.path().join("chrome"),
            cdp_port: 1,
            session_save: dir.path().join("session-save"),
            home: dir.path().into(),
            automation: Automation::Desk,
            plugins: Vec::new(),
            shuttle_socket: Some(desk),
        };
        tokio::spawn(run(cfg));
        crate::testing::served(&socket).await;
        let toolhost = Client::new(socket);
        let calls = [
            ("computer_state", json!({"session": "cu"})),
            (
                "computer_act",
                json!({"session": "cu", "action": "click", "x": 1, "y": 2}),
            ),
            ("computer_end", json!({"session": "cu"})),
            ("screenshot", json!({"max_edge": 512})),
            ("computer_screenshot", json!({})),
            ("run_screenshot", json!({"max_edge": 640})),
            ("desk", json!({"op": "windows"})),
        ];
        for (tool, args) in calls {
            let out = toolhost
                .call(&Call {
                    agent_id: "ag".into(),
                    tool: tool.into(),
                    args,
                })
                .await
                .unwrap();
            assert!(!out.is_error, "{tool}: {}", out.text);
        }
        let wrote = toolhost
            .call(&Call {
                agent_id: "ag".into(),
                tool: "write_file".into(),
                args: json!({"path": "notes.txt", "content": "x"}),
            })
            .await
            .unwrap();
        assert!(
            wrote
                .text
                .contains("the window \"notes.txt - Mousepad\" has notes.txt open"),
            "{}",
            wrote.text
        );
        let mut got = Vec::new();
        while let Ok(call) = tools.try_recv() {
            got.push(call);
        }
        let names: Vec<&str> = got.iter().map(|(t, _)| t.as_str()).collect();
        assert_eq!(
            names,
            [
                "desk_computer_state",
                "desk_computer_act",
                "desk_computer_end",
                "desk_look",
                "desk_look",
                "desk_look",
                "desk_windows",
                "desk_windows"
            ]
        );
        assert_eq!(
            got[1].1,
            json!({"session": "cu", "action": "click", "x": 1, "y": 2})
        );
        assert_eq!(got[3].1["max_edge"], 512);
        assert_eq!(got[5].1["max_edge"], 640);
    }
}
