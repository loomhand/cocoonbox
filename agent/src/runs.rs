//! serve's `script_run`, plugin tools and `gui_plan` on shuttle, shaped as the box's tools give them.

use std::future::Future;
use std::path::{Path, PathBuf};
use std::time::Duration;

use serde::Deserialize;
use serde_json::{Map, Value, json};

use crate::toolhost::files;
use crate::toolhost::rpc::{self, Call, Client, Output};

const MAX_RESULT_BYTES: u64 = 256 << 10;
const DEFAULT_TIMEOUT: Duration = Duration::from_secs(90);
const MAX_TIMEOUT: Duration = Duration::from_secs(110);
const RUN_MARGIN: Duration = Duration::from_secs(15);

/// Where a run result's screenshot comes from.
pub trait Shots {
    /// One screenshot as base64, its long edge at most `max_edge`; with `keep`, also the path of the file that holds it.
    fn shot(
        &self,
        max_edge: Option<u32>,
        keep: bool,
    ) -> impl Future<Output = Result<Option<Shot>, String>> + Send;
}

/// A screenshot of a run result.
pub struct Shot {
    pub image_b64: String,
    pub path: Option<PathBuf>,
}

/// The arguments of `script_run` and plugin tools.
#[derive(Deserialize)]
struct RunArgs {
    code: Option<String>,
    script: Option<String>,
    params: Option<Map<String, Value>>,
    timeout_s: Option<u64>,
    #[serde(default = "default_screenshot")]
    screenshot: bool,
    max_edge: Option<u32>,
}

/// The arguments of `gui_plan`: the plan executor's parameters and the result's own.
#[derive(Deserialize)]
struct PlanArgs {
    #[serde(flatten)]
    params: Map<String, Value>,
    timeout_s: Option<u64>,
    screenshot: Option<bool>,
    max_edge: Option<u32>,
}

/// A script to run: module source, or a file.
enum Script {
    Code(String),
    File(PathBuf),
}

impl Script {
    /// The script of `code` or `script`, a path resolved against `workspace`.
    fn of(code: Option<String>, script: Option<String>, workspace: &Path) -> Result<Self, String> {
        match (code, script) {
            (Some(code), None) => Ok(Self::Code(code)),
            (None, Some(path)) => Ok(Self::File(files::resolve(workspace, &path))),
            _ => Err("pass exactly one of code or script".to_owned()),
        }
    }
}

/// `script_run`, or plugin tool `tool`, on shuttle.
pub async fn script(
    shuttle: &Client,
    shots: &impl Shots,
    workspace: &Path,
    tool: &str,
    args: Value,
) -> Result<Output, String> {
    let a: RunArgs = rpc::parse(args)?;
    let script = Script::of(a.code, a.script, workspace)?;
    let params = Value::Object(a.params.unwrap_or_default());
    let report = run_report(shuttle, tool, script, params, timeout_of(a.timeout_s)).await?;
    finish_run(report, a.screenshot, a.max_edge, shots).await
}

/// `gui_plan` on shuttle; the saved-flows listing is the caller's.
pub async fn plan(shuttle: &Client, shots: &impl Shots, args: Value) -> Result<Output, String> {
    let a: PlanArgs = rpc::parse(args)?;
    let report = plan_report(shuttle, Value::Object(a.params), timeout_of(a.timeout_s)).await?;
    finish_plan(report, a.screenshot, a.max_edge, shots).await
}

/// The report of shuttle's plan executor with `params`: the plan's result only.
pub async fn plan_report(
    shuttle: &Client,
    mut params: Value,
    timeout: Duration,
) -> Result<Value, String> {
    params["timeout_s"] = timeout.as_secs().into();
    let out = relay(shuttle, "gui_plan", params, timeout).await?;
    let len = out.text.len() as u64;
    let mut report = if out.is_error {
        failed(out.text)?
    } else {
        let mut report = json!({});
        report["result"] =
            serde_json::from_str(&out.text).map_err(|e| format!("decode the plan result: {e}"))?;
        report
    };
    if len > MAX_RESULT_BYTES {
        cap(&mut report["result"]);
    }
    Ok(report)
}

/// A run's report as compact JSON, with a kept screenshot unless turned off; an error in the report is an error result.
async fn finish_run(
    mut report: Value,
    screenshot: bool,
    max_edge: Option<u32>,
    shots: &impl Shots,
) -> Result<Output, String> {
    let mut out = Output::default();
    if screenshot {
        match shots.shot(max_edge, true).await {
            Ok(Some(shot)) => {
                if let Some(path) = shot.path {
                    report["screenshot_path"] = path.display().to_string().into();
                }
                out.image_b64 = Some(shot.image_b64);
            }
            Ok(None) => {}
            Err(e) => report["screenshot_error"] = e.into(),
        }
    }
    out.is_error = !report["error"].is_null();
    out.text = report.to_string();
    Ok(out)
}

/// A plan's result as compact JSON, with a screenshot only after an escalation or on request.
async fn finish_plan(
    mut report: Value,
    screenshot: Option<bool>,
    max_edge: Option<u32>,
    shots: &impl Shots,
) -> Result<Output, String> {
    let mut out = Output::default();
    let mut body = match report.get_mut("error").filter(|e| !e.is_null()) {
        Some(error) => {
            out.is_error = true;
            json!({"error": error.take()})
        }
        None => report["result"].take(),
    };
    if screenshot.unwrap_or(body["ok"] != true) {
        match shots.shot(max_edge, false).await {
            Ok(shot) => out.image_b64 = shot.map(|s| s.image_b64),
            Err(e) if body.is_object() => body["screenshot_error"] = e.into(),
            Err(e) => return Err(e),
        }
    }
    out.text = body.to_string();
    Ok(out)
}

/// The run timeout of `timeout_s`, 90 s by default, from 1 to 110 s.
fn timeout_of(timeout_s: Option<u64>) -> Duration {
    timeout_s
        .map_or(DEFAULT_TIMEOUT, Duration::from_secs)
        .clamp(Duration::from_secs(1), MAX_TIMEOUT)
}

/// The report of shuttle's run of `script` as tool `tool`.
async fn run_report(
    shuttle: &Client,
    tool: &str,
    script: Script,
    params: Value,
    timeout: Duration,
) -> Result<Value, String> {
    let mut args = json!({"timeout_s": timeout.as_secs()});
    match script {
        Script::Code(code) => args["code"] = code.into(),
        Script::File(path) => {
            if let Ok(false) = tokio::fs::try_exists(&path).await {
                return Err(format!("script {} does not exist", path.display()));
            }
            args["script"] = path.display().to_string().into();
        }
    }
    args["params"] = params;
    let out = relay(shuttle, tool, args, timeout).await?;
    let len = out.text.len() as u64;
    let mut report: Value = if out.is_error {
        failed(out.text)?
    } else {
        serde_json::from_str(&out.text).map_err(|e| format!("decode the run report: {e}"))?
    };
    if len > MAX_RESULT_BYTES {
        cap(&mut report["result"]);
    }
    Ok(report)
}

/// shuttle's answer to a run of `tool` with `args` and `timeout`.
async fn relay(
    shuttle: &Client,
    tool: &str,
    args: Value,
    timeout: Duration,
) -> Result<Output, String> {
    let call = Call {
        agent_id: String::new(),
        tool: tool.to_owned(),
        args,
    };
    shuttle.desk(&call, timeout + RUN_MARGIN).await
}

/// The report of a failed run that shuttle answers as an error; an error answer that is no such report is an error.
fn failed(text: String) -> Result<Value, String> {
    match serde_json::from_str::<Value>(&text) {
        Ok(report) if report.is_object() && !report["error"].is_null() => Ok(report),
        _ => Err(text),
    }
}

/// Replaces a `result` past the size cap with the notice a local run gives.
fn cap(result: &mut Value) {
    if serde_json::to_vec(result).is_ok_and(|raw| raw.len() as u64 > MAX_RESULT_BYTES) {
        *result =
            format!("more than {MAX_RESULT_BYTES} bytes; write it to a file and return the path")
                .into();
    }
}

fn default_screenshot() -> bool {
    true
}

#[cfg(test)]
mod tests {
    use super::*;

    struct NoShots;

    impl Shots for NoShots {
        async fn shot(&self, _: Option<u32>, _: bool) -> Result<Option<Shot>, String> {
            Ok(None)
        }
    }

    struct NoScreen;

    impl Shots for NoScreen {
        async fn shot(&self, _: Option<u32>, _: bool) -> Result<Option<Shot>, String> {
            Err("no screen".to_owned())
        }
    }

    #[test]
    fn a_relayed_result_past_the_cap_becomes_the_local_notice() {
        let mut small = json!({"n": 1});
        cap(&mut small);
        assert_eq!(small, json!({"n": 1}));
        let mut big = Value::String("x".repeat(MAX_RESULT_BYTES as usize));
        cap(&mut big);
        assert_eq!(
            big,
            "more than 262144 bytes; write it to a file and return the path"
        );
    }

    #[tokio::test]
    async fn a_run_report_is_compact_json() {
        let report = json!({"result": {"rows": [1, 2]}, "logs": ["a"]});
        let out = finish_run(report, false, None, &NoShots).await.unwrap();
        assert_eq!(out.text, r#"{"result":{"rows":[1,2]},"logs":["a"]}"#);
        assert!(!out.is_error);
    }

    #[tokio::test]
    async fn a_failed_run_shuttle_answers_as_an_error_keeps_its_report_and_shape() {
        let dir = tempfile::tempdir().unwrap();
        let socket = crate::testing::endpoint(dir.path(), "desk.sock");
        let mut listener = crate::toolhost::bind(&socket, "").unwrap();
        let (seen, mut calls) = tokio::sync::mpsc::unbounded_channel::<Call>();
        tokio::spawn(async move {
            loop {
                let mut conn = crate::toolhost::accept(&mut listener).await.unwrap();
                let call: Call = rpc::read_frame(&mut conn).await.unwrap();
                let failed = json!({"duration_ms": 2, "error": {"kind": "exception", "message": "boom"}, "result": null});
                let out = match call.args["code"].as_str().or(call.args["case"].as_str()) {
                    Some("fail") => Output::error(failed.to_string()),
                    Some("busy") => {
                        Output::error("desk busy: another run or action is still going")
                    }
                    Some("null") => Output::text(r#"{"duration_ms":1,"error":null,"result":3}"#),
                    _ => Output::text(r#"{"ok":true,"steps":1}"#),
                };
                seen.send(call).unwrap();
                rpc::write_frame(&mut conn, &out).await.unwrap();
            }
        });
        let shuttle = Client::new(socket);
        let run = |args| script(&shuttle, &NoScreen, dir.path(), "script_run", args);

        let out = run(json!({"code": "fail", "screenshot": false, "timeout_s": 500}))
            .await
            .unwrap();
        assert!(
            out.is_error && out.text.contains(r#""message":"boom""#),
            "{}",
            out.text
        );
        let got = calls.recv().await.unwrap();
        assert_eq!(
            (&got.args["timeout_s"], &got.args["params"]),
            (&json!(110), &json!({}))
        );
        let busy = run(json!({"code": "busy", "screenshot": false})).await;
        assert_eq!(
            busy.unwrap_err(),
            "desk busy: another run or action is still going"
        );
        let out = run(json!({"code": "null", "screenshot": false}))
            .await
            .unwrap();
        assert!(!out.is_error, "{}", out.text);

        let out = plan(
            &shuttle,
            &NoScreen,
            json!({"steps": [{"app": "x"}], "screenshot": false}),
        )
        .await
        .unwrap();
        assert_eq!(
            (out.is_error, out.text.as_str()),
            (false, r#"{"ok":true,"steps":1}"#)
        );
        calls.recv().await.unwrap();
        calls.recv().await.unwrap();
        assert_eq!(
            calls.recv().await.unwrap().args,
            json!({"steps": [{"app": "x"}], "timeout_s": 90})
        );
        let out = plan(
            &shuttle,
            &NoScreen,
            json!({"case": "fail", "screenshot": false}),
        )
        .await
        .unwrap();
        let body: Value = serde_json::from_str(&out.text).unwrap();
        assert!(
            out.is_error && body["error"]["kind"] == "exception",
            "{body}"
        );
        let out = plan(&shuttle, &NoScreen, json!({"case": "fail"}))
            .await
            .unwrap();
        let body: Value = serde_json::from_str(&out.text).unwrap();
        assert!(out.is_error && out.image_b64.is_none(), "{body}");
        assert_eq!(
            (&body["error"]["kind"], &body["screenshot_error"]),
            (&json!("exception"), &json!("no screen"))
        );
    }

    #[test]
    fn a_timeout_is_clamped_to_one_and_110_seconds() {
        assert_eq!(timeout_of(None), Duration::from_secs(90));
        assert_eq!(timeout_of(Some(0)), Duration::from_secs(1));
        assert_eq!(timeout_of(Some(500)), Duration::from_secs(110));
    }
}
