//! The script tool: one stateless script run as `user` by Node; `gui_plan` runs the image's plan executor that way.

use std::os::unix::fs::PermissionsExt;
use std::os::unix::process::ExitStatusExt;
use std::path::{Path, PathBuf};
use std::process::{ExitStatus, Stdio};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use base64::Engine;
use base64::engine::general_purpose::STANDARD as B64;
use serde::Deserialize;
use serde_json::{Map, Value, json};
use tokio::io::{AsyncRead, AsyncReadExt};
use tokio::process::Command;

use super::cua::Cua;
use super::files;
use super::rpc::Output;
use super::shell::Capture;
use super::timing;
use crate::llm::image_mime;
use crate::sys;

const DEFAULT_TIMEOUT: Duration = Duration::from_secs(90);
const MAX_TIMEOUT: Duration = Duration::from_secs(110);
const TERM_GRACE: Duration = Duration::from_secs(2);
const PIPE_GRACE: Duration = Duration::from_millis(500);
const RELEASE_TIMEOUT: Duration = Duration::from_secs(10);
const MAX_RESULT_BYTES: u64 = 256 << 10;
const KEPT_SHOTS: usize = 20;
const ERROR_LINES: usize = 12;
const DESK_PLAN: &str = "/opt/desk/lib/gui_plan.mts";
const FLOW_INDEX: &str = "flows/INDEX";
const GAMES_PATH: &str = ":/usr/local/games:/usr/games";
const ENTRY: &str = r#"import { writeFileSync } from 'node:fs'
import { pathToFileURL } from 'node:url'
const env = process.env
const mod = await import(pathToFileURL(env.CB_SCRIPT).href)
if (typeof mod.main === 'function') {
  const value = await mod.main(JSON.parse(env.CB_PARAMS))
  writeFileSync(env.CB_RESULT, JSON.stringify(value ?? null) ?? 'null')
}
if (globalThis.$snapshot !== undefined) writeFileSync(env.CB_SNAPSHOT, String(globalThis.$snapshot))
setTimeout(() => process.exit(), 500).unref()
"#;

/// Runs scripts with Node at `bin`, one scratch directory per run under `dir`.
pub struct Scripts {
    bin: PathBuf,
    dir: PathBuf,
}

impl Scripts {
    /// Creates the runner of `bin` with its scratch root `dir`.
    pub fn new(bin: PathBuf, dir: PathBuf) -> Self {
        Self { bin, dir }
    }

    /// Runs one script and reports it as JSON text, with a screenshot taken after the run unless turned off.
    pub async fn run(&self, cua: &Cua, workspace: &Path, args: Value) -> Result<Output, String> {
        self.run_with(&self.bin, cua, workspace, args).await
    }

    /// Runs one script as [`Scripts::run`] does, with the program at `bin` in place of Node.
    pub async fn run_with(
        &self,
        bin: &Path,
        cua: &Cua,
        workspace: &Path,
        args: Value,
    ) -> Result<Output, String> {
        let RunArgs {
            code,
            script,
            params,
            timeout_s,
            screenshot,
            max_edge,
        } = serde_json::from_value(args).map_err(|e| format!("invalid arguments: {e}"))?;
        let script = match (code, script) {
            (Some(code), None) => Script::Code(code),
            (None, Some(path)) => Script::File(files::resolve(workspace, &path)),
            _ => return Err("pass exactly one of code or script".to_owned()),
        };
        let id = uuid::Uuid::new_v4().simple().to_string();
        let params = Value::Object(params.unwrap_or_default()).to_string();
        let mut report = self
            .run_script(bin, script, &params, workspace, timeout_of(timeout_s), &id)
            .await?;
        let mut out = Output::default();
        if screenshot {
            match self.screenshot(cua, max_edge, &id).await {
                Ok(Some((path, image_b64))) => {
                    report["screenshot_path"] = path.display().to_string().into();
                    out.image_b64 = Some(image_b64);
                }
                Ok(None) => {}
                Err(e) => report["screenshot_error"] = e.into(),
            }
        }
        out.is_error = report.get("error").is_some();
        out.text =
            serde_json::to_string_pretty(&report).map_err(|e| format!("encode report: {e}"))?;
        Ok(out)
    }

    /// Runs the plan executor and reports its result as compact JSON, with a screenshot only after an escalation or on request.
    pub async fn plan(&self, cua: &Cua, workspace: &Path, args: Value) -> Result<Output, String> {
        let PlanArgs {
            params,
            timeout_s,
            screenshot,
            max_edge,
        } = serde_json::from_value(args).map_err(|e| format!("invalid arguments: {e}"))?;
        if params.get("flows") == Some(&Value::Bool(true)) {
            let index = Self::flows(workspace).await;
            return Ok(Output::text(if index.trim().is_empty() {
                "no saved flows yet".to_owned()
            } else {
                index
            }));
        }
        let id = uuid::Uuid::new_v4().simple().to_string();
        let mut report = self
            .run_script(
                &self.bin,
                Script::File(PathBuf::from(DESK_PLAN)),
                &Value::Object(params).to_string(),
                workspace,
                timeout_of(timeout_s),
                &id,
            )
            .await?;
        let mut out = Output::default();
        let body = match report.get_mut("error") {
            Some(error) => {
                out.is_error = true;
                json!({"error": error.take()})
            }
            None => report["result"].take(),
        };
        if screenshot.unwrap_or(body["ok"] != true) {
            out.image_b64 = cua.screenshot_once(max_edge).await?;
        }
        out.text = body.to_string();
        Ok(out)
    }

    /// Rebuilds the flow index with the plan executor after the library sync changed flows.
    pub async fn reindex(&self, workspace: &Path) -> Result<Output, String> {
        let id = uuid::Uuid::new_v4().simple().to_string();
        let report = self
            .run_script(
                &self.bin,
                Script::File(PathBuf::from(DESK_PLAN)),
                r#"{"reindex":true}"#,
                workspace,
                RELEASE_TIMEOUT,
                &id,
            )
            .await?;
        if let Some(error) = report.get("error") {
            return Err(format!("reindex flows: {error}"));
        }
        Ok(Output::text(report["result"].to_string()))
    }

    /// The saved flows' catalog, one `name(params)` line each, newest first; empty before the first save.
    pub async fn flows(workspace: &Path) -> String {
        tokio::fs::read_to_string(workspace.join(FLOW_INDEX))
            .await
            .unwrap_or_default()
    }

    /// Runs one script in its own scratch directory.
    async fn run_script(
        &self,
        bin: &Path,
        script: Script,
        params: &str,
        workspace: &Path,
        timeout: Duration,
        id: &str,
    ) -> Result<Value, String> {
        let run_dir = self.dir.join(id);
        tokio::fs::create_dir_all(&run_dir)
            .await
            .map_err(|e| format!("create {}: {e}", run_dir.display()))?;
        let _ = tokio::fs::set_permissions(&self.dir, std::fs::Permissions::from_mode(0o700)).await;
        let report = self
            .execute(bin, script, params, workspace, &run_dir, timeout)
            .await;
        let _ = tokio::fs::remove_dir_all(&run_dir).await;
        report
    }

    async fn execute(
        &self,
        bin: &Path,
        script: Script,
        params: &str,
        workspace: &Path,
        run_dir: &Path,
        timeout: Duration,
    ) -> Result<Value, String> {
        let script = match script {
            Script::Code(code) => {
                let path = run_dir.join("script.mts");
                files::write_bytes(&path, code.as_bytes()).await?;
                path
            }
            Script::File(path) => {
                if !tokio::fs::try_exists(&path).await.unwrap_or(false) {
                    return Err(format!("script {} does not exist", path.display()));
                }
                path
            }
        };
        let entry = run_dir.join("entry.mjs");
        files::write_bytes(&entry, ENTRY.as_bytes()).await?;
        let (result_path, snapshot_path) =
            (run_dir.join("result.json"), run_dir.join("snapshot.txt"));
        let started = Instant::now();
        timing::mark("prepare");
        let path = std::env::var("PATH").unwrap_or_default() + GAMES_PATH;
        let mut node = Command::new(bin);
        node.arg(&entry)
            .current_dir(workspace)
            .env("PATH", path)
            .env("CB_SCRIPT", &script)
            .env("CB_PARAMS", params)
            .env("CB_RESULT", &result_path)
            .env("CB_SNAPSHOT", &snapshot_path)
            .process_group(0)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true);
        sys::unprotect(&mut node);
        let mut child = node
            .spawn()
            .map_err(|e| format!("start {}: {e}", bin.display()))?;
        timing::mark("spawn");
        let mut guard = RunGuard {
            pgid: child.id(),
            armed: true,
        };
        let (Some(stdout), Some(stderr)) = (child.stdout.take(), child.stderr.take()) else {
            return Err("script pipes missing".to_owned());
        };
        let (mut logs, mut errs) = (Capture::default(), Capture::default());
        let ran = tokio::time::timeout(timeout, async {
            let drained = async {
                tokio::join!(drain(stdout, &mut logs), drain(stderr, &mut errs));
            };
            tokio::pin!(drained);
            let mut open = true;
            let status = tokio::select! {
                status = child.wait() => status,
                () = &mut drained => {
                    open = false;
                    child.wait().await
                }
            };
            if open
                && tokio::time::timeout(PIPE_GRACE, &mut drained)
                    .await
                    .is_err()
            {
                guard.signal(libc::SIGKILL);
                let _ = tokio::time::timeout(PIPE_GRACE, &mut drained).await;
            }
            status
        })
        .await;
        let status = match ran {
            Ok(status) => Some(status.map_err(|e| format!("wait for the script: {e}"))?),
            Err(_) => {
                guard.signal(libc::SIGTERM);
                if tokio::time::timeout(TERM_GRACE, child.wait())
                    .await
                    .is_err()
                {
                    guard.signal(libc::SIGKILL);
                    let _ = child.wait().await;
                }
                None
            }
        };
        guard.armed = false;
        timing::mark("run");
        let stderr = errs.render();
        let mut report = json!({
            "duration_ms": u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX),
            "exit_code": Value::Null,
        });
        report["result"] = match read_capped(&result_path).await {
            None => Value::Null,
            Some(raw) => serde_json::from_slice(&raw)
                .unwrap_or_else(|_| String::from_utf8_lossy(&raw).into_owned().into()),
        };
        report["logs"] = logs.render().into();
        match status {
            None => {
                report["error"] = json!({"kind": "timeout", "message": format!("the run passed {}s and was stopped", timeout.as_secs())});
            }
            Some(status) => {
                report["exit_code"] = status.code().into();
                if let Some(error) = failure(status, &stderr) {
                    report["error"] = error;
                }
            }
        }
        report["stderr"] = stderr.into();
        if let Some(snapshot) = read_capped(&snapshot_path).await {
            report["snapshot"] = String::from_utf8_lossy(&snapshot).into_owned().into();
        }
        Ok(report)
    }

    async fn screenshot(
        &self,
        cua: &Cua,
        max_edge: Option<u32>,
        id: &str,
    ) -> Result<Option<(PathBuf, String)>, String> {
        let Some(image_b64) = cua.screenshot_once(max_edge).await? else {
            return Ok(None);
        };
        let data = B64
            .decode(&image_b64)
            .map_err(|e| format!("decode screenshot: {e}"))?;
        let ext = image_mime(&data).trim_start_matches("image/");
        let shots = self.dir.join("shots");
        let ms = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_or(0, |d| d.as_millis());
        let path = shots.join(format!("{ms:013}-{id}.{ext}"));
        files::write_bytes(&path, &data).await?;
        prune(&shots).await;
        Ok(Some((path, image_b64)))
    }
}

#[derive(Deserialize)]
struct RunArgs {
    #[serde(default)]
    code: Option<String>,
    #[serde(default)]
    script: Option<String>,
    #[serde(default)]
    params: Option<Map<String, Value>>,
    #[serde(default)]
    timeout_s: Option<u64>,
    #[serde(default = "default_screenshot")]
    screenshot: bool,
    #[serde(default)]
    max_edge: Option<u32>,
}

#[derive(Deserialize)]
struct PlanArgs {
    #[serde(flatten)]
    params: Map<String, Value>,
    #[serde(default)]
    timeout_s: Option<u64>,
    #[serde(default)]
    screenshot: Option<bool>,
    #[serde(default)]
    max_edge: Option<u32>,
}

enum Script {
    Code(String),
    File(PathBuf),
}

/// Signals the run's process group on drop unless disarmed.
struct RunGuard {
    pgid: Option<u32>,
    armed: bool,
}

impl RunGuard {
    fn signal(&self, signal: i32) {
        if let Some(pgid) = self.pgid {
            let _ = sys::signal_group(pgid, signal);
        }
    }
}

impl Drop for RunGuard {
    fn drop(&mut self) {
        if self.armed {
            self.signal(libc::SIGKILL);
        }
    }
}

fn default_screenshot() -> bool {
    true
}

fn timeout_of(timeout_s: Option<u64>) -> Duration {
    timeout_s
        .map_or(DEFAULT_TIMEOUT, Duration::from_secs)
        .clamp(Duration::from_secs(1), MAX_TIMEOUT)
}

async fn drain(mut r: impl AsyncRead + Unpin, capture: &mut Capture) {
    let mut chunk = vec![0u8; 16 << 10];
    while let Ok(n) = r.read(&mut chunk).await {
        if n == 0 {
            break;
        }
        capture.push(&chunk[..n]);
    }
}

/// The file's bytes, or a notice in their place past the size cap; `None` when the script wrote none.
async fn read_capped(path: &Path) -> Option<Vec<u8>> {
    let file = tokio::fs::File::open(path).await.ok()?;
    let mut raw = Vec::new();
    file.take(MAX_RESULT_BYTES + 1)
        .read_to_end(&mut raw)
        .await
        .ok()?;
    if raw.len() as u64 > MAX_RESULT_BYTES {
        let notice = format!(
            "\"more than {MAX_RESULT_BYTES} bytes; write it to a file and return the path\""
        );
        return Some(notice.into_bytes());
    }
    Some(raw)
}

fn failure(status: ExitStatus, stderr: &str) -> Option<Value> {
    if status.success() {
        return None;
    }
    if let Some(signal) = status.signal() {
        return Some(
            json!({"kind": "interrupted", "message": format!("the run was killed by signal {signal}")}),
        );
    }
    let lines: Vec<&str> = stderr
        .lines()
        .filter(|l| !l.trim().is_empty() && !l.starts_with("[info]") && !l.starts_with("Node.js v"))
        .collect();
    let mut error = json!({"kind": "exception"});
    error["message"] = lines[lines.len().saturating_sub(ERROR_LINES)..]
        .join("\n")
        .into();
    Some(error)
}

async fn prune(shots: &Path) {
    let Ok(mut dir) = tokio::fs::read_dir(shots).await else {
        return;
    };
    let mut names = Vec::new();
    while let Ok(Some(e)) = dir.next_entry().await {
        names.push(e.path());
    }
    names.sort_unstable();
    for old in names.iter().rev().skip(KEPT_SHOTS) {
        let _ = tokio::fs::remove_file(old).await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn a_run_ends_when_main_returns_though_a_timer_or_server_stays_open() {
        let Some(node) = std::env::var_os("NODE_BIN") else {
            eprintln!("SKIP: no Node; set NODE_BIN to run");
            return;
        };
        let dir = tempfile::tempdir().unwrap();
        let scripts = Scripts::new(node.into(), dir.path().into());
        for code in [
            "setInterval(() => {}, 1000)\nexport function main(p) { console.log('tick'); return p.n }",
            "import { createServer } from 'node:net'\ncreateServer().listen(0)\nexport async function main(p) { console.log('tick'); return p.n }",
        ] {
            let started = Instant::now();
            let report = scripts
                .run_script(
                    &scripts.bin,
                    Script::Code(code.to_owned()),
                    r#"{"n":7}"#,
                    dir.path(),
                    Duration::from_secs(20),
                    "run",
                )
                .await
                .unwrap();
            assert!(started.elapsed() < Duration::from_secs(5), "{report}");
            assert_eq!(report["result"], 7, "{report}");
            assert_eq!(report["logs"], "tick", "{report}");
            assert_eq!(report["exit_code"], 0, "{report}");
            assert!(report.get("error").is_none(), "{report}");
        }
    }

    #[test]
    fn failure_keeps_the_tail_of_stderr_without_info_lines() {
        let stderr = "[info] Opening app Mousepad\nfile:///tmp/s.mts:20\nError: Instance has no visible top-level window\n    at x\n\nNode.js v24.21.0";
        let error = failure(ExitStatus::from_raw(1 << 8), stderr).unwrap();
        assert_eq!(error["kind"], "exception");
        assert_eq!(
            error["message"],
            "file:///tmp/s.mts:20\nError: Instance has no visible top-level window\n    at x"
        );
        assert_eq!(
            failure(ExitStatus::from_raw(9), "").unwrap()["kind"],
            "interrupted"
        );
        assert!(failure(ExitStatus::from_raw(0), "").is_none());
    }
}
