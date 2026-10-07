//! `script_run`: one stateless script run with the image's `script` CLI as `user`.

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
const RELEASE_TIMEOUT: Duration = Duration::from_secs(10);
const MAX_RESULT_BYTES: u64 = 256 << 10;
const KEPT_SHOTS: usize = 20;
const ERROR_LINES: usize = 12;
const APP_SCOPE: &str = "/app.slice/script-app-";
const NOISE: &str = "script: warning: OPENROUTER_API_KEY is not set";
const ENTRY: &str = r#"import { writeFileSync } from 'node:fs'
import { pathToFileURL } from 'node:url'
const env = process.env
const mod = await import(pathToFileURL(env.CB_SCRIPT_SCRIPT).href)
if (typeof mod.main === 'function') {
  const value = await mod.main(JSON.parse(env.CB_SCRIPT_PARAMS))
  writeFileSync(env.CB_SCRIPT_RESULT, JSON.stringify(value ?? null) ?? 'null')
}
if (globalThis.$snapshot !== undefined) writeFileSync(env.CB_SCRIPT_SNAPSHOT, String(globalThis.$snapshot))
"#;
// X keycodes of Control, Shift, Alt and Super, left and right
const RELEASE: &str = r#"import { Button, Direction, Machine } from '@vendor/script-js'
const m = Machine.local()
for (const code of [37, 105, 50, 62, 64, 108, 133, 134]) m.keyRaw(code, Direction.Release)
for (const button of [Button.Left, Button.Middle, Button.Right]) m.mouseButton(button, Direction.Release)
"#;

/// Runs scripts with the `script` CLI at `bin`, one scratch directory per run under `dir`.
pub struct Script {
    bin: PathBuf,
    dir: PathBuf,
}

impl Script {
    /// Creates the runner of `bin` with its scratch root `dir`.
    pub fn new(bin: PathBuf, dir: PathBuf) -> Self {
        Self { bin, dir }
    }

    /// Runs one script and reports it as JSON text, with a screenshot taken after the run unless turned off.
    pub async fn run(&self, cua: &Cua, workspace: &Path, args: Value) -> Result<Output, String> {
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
        let timeout = timeout_s
            .map_or(DEFAULT_TIMEOUT, Duration::from_secs)
            .clamp(Duration::from_secs(1), MAX_TIMEOUT);
        let id = uuid::Uuid::new_v4().simple().to_string();
        let run_dir = self.dir.join(&id);
        tokio::fs::create_dir_all(&run_dir)
            .await
            .map_err(|e| format!("create {}: {e}", run_dir.display()))?;
        let _ = tokio::fs::set_permissions(&self.dir, std::fs::Permissions::from_mode(0o700)).await;
        let params = Value::Object(params.unwrap_or_default()).to_string();
        let report = self
            .execute(script, &params, workspace, &run_dir, timeout)
            .await;
        if report.as_ref().is_ok_and(|r| r.get("error").is_some()) {
            let started = Instant::now();
            match self.release(workspace, &run_dir).await {
                Ok(()) => eprintln!(
                    "script: released held input after a failed run in {} ms",
                    started.elapsed().as_millis()
                ),
                Err(e) => eprintln!("script: release held input after a failed run: {e}"),
            }
            timing::mark("release");
        }
        let _ = tokio::fs::remove_dir_all(&run_dir).await;
        let mut report = report?;
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

    async fn execute(
        &self,
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
        let mut child = Command::new(&self.bin)
            .arg("run")
            .arg(&entry)
            .current_dir(workspace)
            .env("CB_SCRIPT_SCRIPT", &script)
            .env("CB_SCRIPT_PARAMS", params)
            .env("CB_SCRIPT_RESULT", &result_path)
            .env("CB_SCRIPT_SNAPSHOT", &snapshot_path)
            .process_group(0)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true)
            .spawn()
            .map_err(|e| format!("start {}: {e}", self.bin.display()))?;
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
            let (_, _, status) = tokio::join!(
                drain(stdout, &mut logs),
                drain(stderr, &mut errs),
                child.wait()
            );
            status
        })
        .await;
        let status = match ran {
            Ok(status) => Some(status.map_err(|e| format!("wait for script: {e}"))?),
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
        let stderr = errs
            .render()
            .lines()
            .filter(|l| !l.starts_with(NOISE))
            .collect::<Vec<_>>()
            .join("\n");
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

    /// Releases the modifier keys and mouse buttons a failed run may have left held in the X server.
    async fn release(&self, workspace: &Path, run_dir: &Path) -> Result<(), String> {
        let script = run_dir.join("release.mjs");
        files::write_bytes(&script, RELEASE.as_bytes()).await?;
        let mut child = Command::new(&self.bin)
            .arg("run")
            .arg(&script)
            .current_dir(workspace)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .kill_on_drop(true)
            .spawn()
            .map_err(|e| format!("start {}: {e}", self.bin.display()))?;
        let status = tokio::time::timeout(RELEASE_TIMEOUT, child.wait())
            .await
            .map_err(|_| format!("no exit in {}s", RELEASE_TIMEOUT.as_secs()))?
            .map_err(|e| format!("wait for script: {e}"))?;
        if !status.success() {
            return Err(format!("script {status}"));
        }
        Ok(())
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

enum Script {
    Code(String),
    File(PathBuf),
}

/// Signals the run's process group on drop unless disarmed, sparing the apps it launched.
struct RunGuard {
    pgid: Option<u32>,
    armed: bool,
}

impl RunGuard {
    fn signal(&self, signal: i32) {
        if let Some(pgid) = self.pgid {
            signal_run(pgid, signal);
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

/// Signals every process of group `pgid` except the apps in their own `script-app` scopes.
fn signal_run(pgid: u32, signal: i32) {
    let Ok(entries) = std::fs::read_dir("/proc") else {
        let _ = sys::signal_group(pgid, signal);
        return;
    };
    for pid in entries
        .flatten()
        .filter_map(|e| e.file_name().to_str()?.parse::<u32>().ok())
    {
        let in_group = std::fs::read_to_string(format!("/proc/{pid}/stat"))
            .ok()
            .and_then(|stat| process_group(&stat))
            == Some(pgid);
        let launched = std::fs::read_to_string(format!("/proc/{pid}/cgroup"))
            .is_ok_and(|cgroup| cgroup.contains(APP_SCOPE));
        if in_group && !launched {
            let _ = sys::signal_process(pid, signal);
        }
    }
}

/// The process group field of a `/proc/<pid>/stat` line, read after the parenthesized command name.
fn process_group(stat: &str) -> Option<u32> {
    stat.rsplit_once(')')?
        .1
        .split_whitespace()
        .nth(2)?
        .parse()
        .ok()
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

    #[test]
    fn process_group_reads_past_a_command_name_with_spaces_and_parens() {
        assert_eq!(
            process_group("4242 (node (x) y) S 1 777 777 0 -1"),
            Some(777)
        );
        assert_eq!(process_group("garbage"), None);
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
