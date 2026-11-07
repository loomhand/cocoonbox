//! The toolhost's part of shuttle's runs: the plan executor's locate and reindex, and run screenshots.

use std::path::{Path, PathBuf};
use std::time::Duration;

use base64::Engine;
use base64::engine::general_purpose::STANDARD as B64;
use serde_json::{Value, json};

use super::files;
use super::rpc::{self, Call, Client, Output};
use crate::llm::image_mime;
use crate::runs::{self, Shot};

const REINDEX_TIMEOUT: Duration = Duration::from_secs(10);
const LOCATE_TIMEOUT: Duration = Duration::from_secs(20);
const KEPT_SHOTS: usize = 20;

/// Runs the plan executor's own jobs on shuttle and takes run screenshots, keeping them under `dir` when asked.
pub struct Scripts {
    dir: PathBuf,
    shuttle: Client,
}

impl Scripts {
    /// Creates the runner whose jobs go to `shuttle` and whose kept screenshots go under `dir`.
    pub fn new(dir: PathBuf, shuttle: Client) -> Self {
        Self { dir, shuttle }
    }

    /// The plan executor's resolve of `target` in the focused window, pressed by action when `press`; its result.
    pub async fn locate(&self, target: &str, press: bool) -> Result<Value, String> {
        let params = json!({"locate": {"target": target, "press": press}});
        let mut report = runs::plan_report(&self.shuttle, params, LOCATE_TIMEOUT).await?;
        if let Some(error) = report.get("error") {
            return Err(format!("locate: {error}"));
        }
        Ok(report["result"].take())
    }

    /// Rebuilds the flow index with the plan executor after the library sync changed flows.
    pub async fn reindex(&self) -> Result<Output, String> {
        let _index = super::flows::INDEX_LOCK.lock().await;
        let report =
            runs::plan_report(&self.shuttle, json!({"reindex": true}), REINDEX_TIMEOUT).await?;
        if let Some(error) = report.get("error") {
            return Err(format!("reindex flows: {error}"));
        }
        Ok(Output::text(report["result"].to_string()))
    }

    /// One screenshot of shuttle's screen, its long edge at most `max_edge`; with `keep`, also kept as a file.
    pub async fn shot(&self, max_edge: Option<u32>, keep: bool) -> Result<Option<Shot>, String> {
        let call = Call {
            agent_id: "toolhost".to_owned(),
            tool: "desk_look".to_owned(),
            args: json!({"max_edge": max_edge}),
        };
        let out = self.shuttle.desk(&call, rpc::DESK_DEADLINE).await?;
        if out.is_error {
            return Err(out.text);
        }
        let Some(image_b64) = out.image_b64 else {
            return Ok(None);
        };
        if !keep {
            return Ok(Some(Shot {
                image_b64,
                path: None,
            }));
        }
        let data = B64
            .decode(&image_b64)
            .map_err(|e| format!("decode screenshot: {e}"))?;
        let ext = image_mime(&data).trim_start_matches("image/");
        let shots = self.dir.join("shots");
        let id = uuid::Uuid::new_v4().simple();
        let path = shots.join(format!("{:013}-{id}.{ext}", crate::now_ms()));
        files::write_bytes(&path, &data).await?;
        prune(&shots).await;
        Ok(Some(Shot {
            image_b64,
            path: Some(path),
        }))
    }
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
