//! The environment step: exact Mac versions through mise, pinned per session, then the lockfile fetches.

use std::collections::BTreeMap;
use std::path::Path;
use std::time::{Duration, Instant};

use serde::{Deserialize, Serialize};
use tokio::io::AsyncWriteExt;

use super::{Layout, login_shell};

const STATE_FILE: &str = "env-state.json";
const SPEC_FILE: &str = "env.json";
const LOG_FILE: &str = "env.log";
const WAIT_POLL: Duration = Duration::from_millis(500);
// an inbox restart mid-step leaves the report unfinished for good
const WAIT_LIMIT: Duration = Duration::from_secs(900);
const PROBE_TIMEOUT: Duration = Duration::from_secs(20);
const MISE_FILES: [&str; 3] = [".mise.toml", "mise.toml", ".tool-versions"];
const HINT_FILES: [&str; 3] = [".devcontainer/devcontainer.json", "flake.nix", "shell.nix"];
const FETCHES: [(&str, &str); 6] = [
    ("go.sum", "go mod download"),
    ("Cargo.lock", "cargo fetch --locked"),
    ("package-lock.json", "npm ci"),
    ("pnpm-lock.yaml", "pnpm install --frozen-lockfile"),
    ("yarn.lock", "yarn install --immutable"),
    ("uv.lock", "uv sync --frozen"),
];

/// `env.json`, written by the Mac packer.
#[derive(Debug, Default, Deserialize, Serialize)]
pub struct Spec {
    #[serde(default)]
    pub v: u32,
    #[serde(default)]
    pub tools: BTreeMap<String, String>,
    #[serde(default)]
    pub package_managers: BTreeMap<String, String>,
    #[serde(default)]
    pub files: Vec<String>,
    #[serde(default)]
    pub lockfiles: Vec<String>,
}

impl Spec {
    /// Reads the repo's `env.json`; `None` when it has none.
    pub async fn load(layout: &Layout, repo: &str) -> Result<Option<Self>, String> {
        let path = layout.repo_dir(repo).join(SPEC_FILE);
        match tokio::fs::read(&path).await {
            Ok(raw) => serde_json::from_slice(&raw)
                .map(Some)
                .map_err(|e| format!("decode {}: {e}", path.display())),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(e) => Err(format!("read {}: {e}", path.display())),
        }
    }
}

/// Where a selected version comes from.
#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Source {
    /// The exact version the Mac ran.
    Mac,
    /// The version a declarative file in the repo names.
    File,
    /// The newest patch of the Mac's minor version.
    SameMinor,
}

/// The step's progress, read by `offload env`.
#[derive(Debug, Default, Deserialize, Serialize)]
pub struct Report {
    pub done: bool,
    pub tools: Vec<Tool>,
    pub fetches: Vec<Fetch>,
    pub hints: Vec<String>,
}

impl Report {
    /// The pins for the tmux environment: `MISE_<TOOL>_VERSION` per selected tool.
    pub fn pins(&self) -> Vec<(String, String)> {
        self.tools
            .iter()
            .filter_map(|t| {
                let v = t.selected.as_ref()?;
                let name: String = t
                    .tool
                    .chars()
                    .map(|c| {
                        if c.is_ascii_alphanumeric() {
                            c.to_ascii_uppercase()
                        } else {
                            '_'
                        }
                    })
                    .collect();
                Some((format!("MISE_{name}_VERSION"), v.clone()))
            })
            .collect()
    }

    async fn save(&self, layout: &Layout, repo: &str) -> Result<(), String> {
        let raw = serde_json::to_vec(self).map_err(|e| format!("encode env report: {e}"))?;
        super::write_atomic(&layout.repo_dir(repo).join(STATE_FILE), &raw).await
    }
}

/// One tool of the step.
#[derive(Debug, Deserialize, Serialize)]
pub struct Tool {
    pub tool: String,
    pub wanted: String,
    pub selected: Option<String>,
    pub source: Option<Source>,
    /// `pending`, `ready`, `missing` or `failed`.
    pub outcome: String,
}

/// One lockfile fetch of the step.
#[derive(Debug, Deserialize, Serialize)]
pub struct Fetch {
    pub lockfile: String,
    pub command: String,
    /// `pending`, `ok` or `failed`.
    pub outcome: String,
    pub ms: u64,
}

/// Picks the version to install: the Mac's exact one, else the one a declarative file names,
/// else the newest release with the Mac's major and minor.
pub fn select(
    wanted: &str,
    available: &[String],
    declared: Option<&str>,
) -> Option<(String, Source)> {
    if available.iter().any(|v| v == wanted) {
        return Some((wanted.to_owned(), Source::Mac));
    }
    if let Some(d) = declared {
        if available.iter().any(|v| v == d) {
            return Some((d.to_owned(), Source::File));
        }
        if let Some(v) = newest(available, d) {
            return Some((v, Source::File));
        }
    }
    let mut parts = wanted.split('.');
    let minor = format!("{}.{}", parts.next()?, parts.next()?);
    newest(available, &minor).map(|v| (v, Source::SameMinor))
}

/// Selects every tool's version and records the selection; installs nothing.
pub async fn plan(layout: &Layout, repo: &str, dir: &Path, spec: &Spec) -> Report {
    let wanted: Vec<(&String, &String)> = spec
        .tools
        .iter()
        .chain(spec.package_managers.iter())
        .collect();
    let picks = futures_util::future::join_all(wanted.iter().map(|(tool, version)| async move {
        if mise_has(dir, tool, version).await {
            return Some(((*version).clone(), Source::Mac));
        }
        let available = mise_remote(dir, tool).await;
        select(version, &available, declared(dir, tool).await.as_deref())
    }))
    .await;
    let tools = wanted
        .into_iter()
        .zip(picks)
        .map(|((tool, version), pick)| Tool {
            tool: tool.clone(),
            wanted: version.clone(),
            outcome: if pick.is_some() { "pending" } else { "missing" }.to_owned(),
            source: pick.as_ref().map(|p| p.1),
            selected: pick.map(|p| p.0),
        })
        .collect();
    let fetches = FETCHES
        .iter()
        .filter(|(lock, _)| spec.lockfiles.iter().any(|l| l == lock))
        .map(|(lock, cmd)| Fetch {
            lockfile: (*lock).to_owned(),
            command: (*cmd).to_owned(),
            outcome: "pending".to_owned(),
            ms: 0,
        })
        .collect();
    let hints = HINT_FILES
        .iter()
        .filter(|f| spec.files.iter().any(|g| g == *f))
        .map(|f| format!("{f} exists; it is a hint only"))
        .collect();
    let report = Report {
        done: false,
        tools,
        fetches,
        hints,
    };
    if let Err(e) = report.save(layout, repo).await {
        eprintln!("offload: {e}");
    }
    report
}

/// Installs the selected versions, runs `mise install` for declarative files and the lockfile
/// fetches, saving the report after each step; output goes to `env.log`.
pub async fn run(layout: &Layout, repo: &str, dir: &Path, spec: &Spec, mut report: Report) {
    let log = layout.repo_dir(repo).join(LOG_FILE);
    let pins = report.pins();
    for i in 0..report.tools.len() {
        let t = &report.tools[i];
        let Some(v) = &t.selected else { continue };
        let cmd = format!("mise install {}@{v}", t.tool);
        let ok = logged(&log, dir, &pins, &cmd).await;
        report.tools[i].outcome = if ok { "ready" } else { "failed" }.to_owned();
        let _ = report.save(layout, repo).await;
    }
    if MISE_FILES.iter().any(|f| spec.files.iter().any(|g| g == f)) {
        logged(&log, dir, &pins, "mise install").await;
    }
    for i in 0..report.fetches.len() {
        let started = Instant::now();
        let ok = logged(&log, dir, &pins, &report.fetches[i].command).await;
        let f = &mut report.fetches[i];
        f.outcome = if ok { "ok" } else { "failed" }.to_owned();
        f.ms = u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX);
        let _ = report.save(layout, repo).await;
    }
    report.done = true;
    if let Err(e) = report.save(layout, repo).await {
        eprintln!("offload: {e}");
    }
}

/// `offload env`: the step's report, after it finished (or `WAIT_LIMIT` passed) unless `no_wait`.
pub async fn show(layout: &Layout, repo: &str, no_wait: bool) -> Result<Option<Report>, String> {
    let path = layout.repo_dir(repo).join(STATE_FILE);
    let deadline = Instant::now() + WAIT_LIMIT;
    loop {
        let report: Option<Report> = match tokio::fs::read(&path).await {
            Ok(raw) => {
                Some(serde_json::from_slice(&raw).map_err(|e| format!("decode env report: {e}"))?)
            }
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => None,
            Err(e) => return Err(format!("read {}: {e}", path.display())),
        };
        if no_wait || report.as_ref().is_none_or(|r| r.done) || Instant::now() >= deadline {
            return Ok(report);
        }
        tokio::time::sleep(WAIT_POLL).await;
    }
}

/// The report as text for the box agent.
pub fn render(report: Option<&Report>) -> String {
    let Some(r) = report else {
        return "no environment step for this repo (no env.json)\n".to_owned();
    };
    let mut out = String::new();
    if !r.done {
        out.push_str("the environment step is still running\n");
    }
    for t in &r.tools {
        let source = match t.source {
            Some(Source::Mac) => "the Mac's version",
            Some(Source::File) => "a declarative file",
            Some(Source::SameMinor) => "the newest patch of the Mac's minor",
            None => "none available",
        };
        out.push_str(&format!(
            "{}: wanted {} (Mac), selected {} from {source}: {}\n",
            t.tool,
            t.wanted,
            t.selected.as_deref().unwrap_or("-"),
            t.outcome
        ));
    }
    for f in &r.fetches {
        out.push_str(&format!(
            "{}: `{}` {} in {} ms\n",
            f.lockfile, f.command, f.outcome, f.ms
        ));
    }
    for h in &r.hints {
        out.push_str(h);
        out.push('\n');
    }
    out
}

/// The version a declarative file in `dir` names for `tool`.
pub async fn declared(dir: &Path, tool: &str) -> Option<String> {
    let read = |name: &str| {
        let path = dir.join(name);
        async move { tokio::fs::read_to_string(path).await.ok() }
    };
    if let Some(v) = read(".tool-versions")
        .await
        .and_then(|s| tool_versions(&s, tool))
    {
        return Some(v);
    }
    for f in [".mise.toml", "mise.toml"] {
        if let Some(v) = read(f).await.and_then(|s| mise_toml(&s, tool)) {
            return Some(v);
        }
    }
    let first_line = |s: String| {
        s.lines()
            .map(str::trim)
            .find(|l| !l.is_empty() && !l.starts_with('#'))
            .map(|l| l.trim_start_matches('v').to_owned())
    };
    match tool {
        "go" => {
            if let Some(v) = read("go.mod").await.and_then(|s| go_mod(&s)) {
                return Some(v);
            }
            read(".go-version").await.and_then(first_line)
        }
        "node" => match read(".nvmrc").await.and_then(first_line) {
            Some(v) => Some(v),
            None => read(".node-version").await.and_then(first_line),
        },
        "python" => read(".python-version").await.and_then(first_line),
        "rust" => match read("rust-toolchain.toml")
            .await
            .and_then(|s| toml_value(&s, "channel"))
        {
            Some(v) => Some(v),
            None => read("rust-toolchain").await.and_then(first_line),
        },
        _ => None,
    }
}

fn tool_versions(s: &str, tool: &str) -> Option<String> {
    let alias = match tool {
        "go" => "golang",
        "node" => "nodejs",
        other => other,
    };
    s.lines().find_map(|l| {
        let mut w = l.split_whitespace();
        let name = w.next()?;
        (name == tool || name == alias).then(|| w.next().map(str::to_owned))?
    })
}

fn mise_toml(s: &str, tool: &str) -> Option<String> {
    let mut in_tools = false;
    for l in s.lines().map(str::trim) {
        if l.starts_with('[') {
            in_tools = l == "[tools]";
            continue;
        }
        if in_tools && let Some(v) = toml_value(l, tool) {
            return Some(v);
        }
    }
    None
}

fn toml_value(s: &str, key: &str) -> Option<String> {
    s.lines().find_map(|l| {
        let (k, v) = l.split_once('=')?;
        (k.trim().trim_matches('"') == key)
            .then(|| v.trim().trim_matches(|c| c == '"' || c == '\'').to_owned())
            .filter(|v| !v.is_empty())
    })
}

fn go_mod(s: &str) -> Option<String> {
    let line = |prefix: &str| {
        s.lines()
            .find_map(|l| l.trim().strip_prefix(prefix).map(|v| v.trim().to_owned()))
    };
    line("toolchain go").or_else(|| line("go "))
}

/// The newest plain release in `available` equal to `prefix` or under `prefix.`.
fn newest(available: &[String], prefix: &str) -> Option<String> {
    available
        .iter()
        .filter(|v| {
            v.split('.')
                .all(|p| !p.is_empty() && p.bytes().all(|b| b.is_ascii_digit()))
        })
        .filter(|v| {
            *v == prefix
                || v.strip_prefix(prefix)
                    .is_some_and(|rest| rest.starts_with('.'))
        })
        .max_by_key(|v| {
            v.split('.')
                .map(|p| p.parse::<u64>().unwrap_or(0))
                .collect::<Vec<_>>()
        })
        .cloned()
}

async fn mise_has(dir: &Path, tool: &str, version: &str) -> bool {
    probe(dir, &format!("mise where {tool}@{version}"))
        .await
        .is_some()
}

async fn mise_remote(dir: &Path, tool: &str) -> Vec<String> {
    probe(dir, &format!("mise ls-remote {tool}"))
        .await
        .map(|out| out.lines().map(|l| l.trim().to_owned()).collect())
        .unwrap_or_default()
}

/// A selection probe's stdout; `None` on failure or past `PROBE_TIMEOUT`, since the inbox waits on it.
async fn probe(dir: &Path, cmd: &str) -> Option<String> {
    tokio::time::timeout(PROBE_TIMEOUT, login_shell(dir, cmd))
        .await
        .ok()?
        .ok()
}

/// Runs `cmd` in a login shell in `dir` with the pins, appending its output to `log`.
async fn logged(log: &Path, dir: &Path, pins: &[(String, String)], cmd: &str) -> bool {
    let started = Instant::now();
    let out = tokio::process::Command::new("bash")
        .args(["-lc", cmd])
        .current_dir(dir)
        .envs(pins.iter().map(|(k, v)| (k, v)))
        .env("MISE_TRUSTED_CONFIG_PATHS", dir)
        .env("MISE_YES", "1")
        .stdin(std::process::Stdio::null())
        .output()
        .await;
    let (ok, text) = match out {
        Ok(o) => (
            o.status.success(),
            format!(
                "{}{}exit: {}\n",
                String::from_utf8_lossy(&o.stdout),
                String::from_utf8_lossy(&o.stderr),
                o.status
            ),
        ),
        Err(e) => (false, format!("spawn bash: {e}\n")),
    };
    let entry = format!(
        "$ {cmd}\n{text}took {} ms\n\n",
        started.elapsed().as_millis()
    );
    if let Ok(mut f) = tokio::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(log)
        .await
    {
        let _ = f.write_all(entry.as_bytes()).await;
    }
    ok
}

#[cfg(test)]
mod tests {
    use super::*;

    fn list(v: &[&str]) -> Vec<String> {
        v.iter().map(|s| (*s).to_owned()).collect()
    }

    #[test]
    fn the_macs_exact_version_wins() {
        let available = list(&["1.26.4", "1.26.5", "1.26.8", "1.27.0"]);
        assert_eq!(
            select("1.26.5", &available, Some("1.26.1")),
            Some(("1.26.5".to_owned(), Source::Mac))
        );
    }

    #[test]
    fn a_declared_version_comes_before_the_newest_patch() {
        let available = list(&["1.26.1", "1.26.8", "1.27.0"]);
        assert_eq!(
            select("1.26.5", &available, Some("1.26.1")),
            Some(("1.26.1".to_owned(), Source::File))
        );
        assert_eq!(
            select("22.11.0", &list(&["22.9.0", "22.12.0"]), Some("22")),
            Some(("22.12.0".to_owned(), Source::File))
        );
    }

    #[test]
    fn otherwise_the_newest_patch_of_the_same_minor_and_never_a_prerelease() {
        let available = list(&["1.88.0", "1.88.1", "1.88.10", "1.89.0", "1.88.11-beta.1"]);
        assert_eq!(
            select("1.88.2", &available, Some("9.9")),
            Some(("1.88.10".to_owned(), Source::SameMinor))
        );
        assert_eq!(select("1.87.0", &available, None), None);
        assert_eq!(select("3", &available, None), None);
    }

    #[test]
    fn declarative_files_name_versions() {
        assert_eq!(
            tool_versions("golang 1.26.1\nnodejs 22.1.0\n", "go").as_deref(),
            Some("1.26.1")
        );
        assert_eq!(
            mise_toml("[env]\ngo = \"x\"\n[tools]\ngo = \"1.26\"\n", "go").as_deref(),
            Some("1.26")
        );
        assert_eq!(
            go_mod("module x\n\ngo 1.25.0\n\ntoolchain go1.26.5\n").as_deref(),
            Some("1.26.5")
        );
        assert_eq!(go_mod("module x\ngo 1.25.0\n").as_deref(), Some("1.25.0"));
        assert_eq!(
            toml_value("[toolchain]\nchannel = \"1.88.0\"\n", "channel").as_deref(),
            Some("1.88.0")
        );
    }

    #[test]
    fn pins_name_each_selected_tool() {
        let report = Report {
            tools: vec![
                Tool {
                    tool: "go".into(),
                    wanted: "1.26.5".into(),
                    selected: Some("1.26.5".into()),
                    source: Some(Source::Mac),
                    outcome: "ready".into(),
                },
                Tool {
                    tool: "node".into(),
                    wanted: "22.11.0".into(),
                    selected: None,
                    source: None,
                    outcome: "missing".into(),
                },
            ],
            ..Report::default()
        };
        assert_eq!(
            report.pins(),
            [("MISE_GO_VERSION".to_owned(), "1.26.5".to_owned())]
        );
    }
}
