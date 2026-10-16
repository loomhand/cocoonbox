//! The box packer verbs: `begin`, the `send` stream, messages, `post`, `end` and `wipe-secrets`.

use std::collections::BTreeMap;
use std::io::Read;
use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::path::{Component, Path, PathBuf};
use std::time::{Duration, Instant};

use serde_json::{Value, json};
use tokio::io::AsyncBufReadExt;

use super::env::{self, Spec};
use super::handover::{self, MAX_BODY};
use super::{
    Args, Copy, Fail, Layout, call, git, git_env, home_shell, is_hex, locked, read_index,
    snapshot_ref, tmux_session, with_index, write_atomic,
};

/// How long `send` waits for the CLI to take its first prompt.
pub const ACCEPT_WAIT: Duration = Duration::from_secs(30);

const MAX_NAME: usize = 64;
const PROBE_TIMEOUT: Duration = Duration::from_secs(20);
const ACCEPT_POLL: Duration = Duration::from_millis(250);
const QUIT_WAIT: Duration = Duration::from_secs(10);
const TERM_WAIT: Duration = Duration::from_secs(2);
const QUIT_POLL: Duration = Duration::from_millis(200);
const AGENT_WAIT: Duration = Duration::from_secs(5);
const STAGE_DIR: &str = "stage";
const BUNDLE: &str = "offload.bundle";
const AGENTS_DIR: &str = "cocoonbox-agents";
const ATTACHED_LINES: usize = 200;
const SSH_SOCKET: &str = "ssh-agent.sock";
const SNAPSHOT_IDENTITY: [&str; 4] = [
    "-c",
    "user.name=cocoonbox offload",
    "-c",
    "user.email=offload@cocoonbox.invalid",
];
const CLAUDE_MANAGED: &str = "/etc/claude-code";

/// The box half of the preflight; on no `block`, takes the copy for the offload id and makes its directory.
pub async fn begin(layout: &Layout, a: &Args) -> Result<Value, Fail> {
    let repo = a.repo()?;
    let kind = a.kind()?;
    let id = offload_id(a)?;
    let wanted = a.required("name")?;
    let no_repo = a.flag("no-repo");
    if !no_repo {
        valid_name(&wanted)?;
    }
    let status = call(layout, &json!({"op": "status"})).await?;
    let sessions = status["sessions"].as_array().cloned().unwrap_or_default();
    let mine_running = sessions
        .iter()
        .any(|s| s["repo"] == repo.as_str() && s["status"] != "stopped");
    let running = sessions.iter().filter(|s| s["status"] != "stopped").count();
    let index = read_index(layout).await?;
    let mut findings = Vec::new();
    let mut block = |check: &str, detail: String| {
        findings.push(json!({"check": check, "outcome": "block", "detail": detail}));
    };
    let missing = home_shell(
        &layout.home,
        "for t in git rsync tmux mise; do command -v \"$t\" >/dev/null || echo \"missing:$t\"; done",
    )
    .await
    .unwrap_or_else(|e| e);
    let missing: Vec<&str> = missing
        .lines()
        .filter_map(|l| l.trim().strip_prefix("missing:"))
        .collect();
    if !missing.is_empty() {
        block(
            "box_tools",
            format!(
                "the box lacks {}; upgrade the box first",
                missing.join(", ")
            ),
        );
    }
    let cli = probe_cli(&layout.home, &kind).await;
    if cli["version"].as_str().is_none_or(str::is_empty) {
        block(
            "box_cli",
            format!("{kind} is not installed in the box; Install in box"),
        );
    } else if cli["signed_in"] != true {
        block(
            "box_cli",
            format!("{kind} is signed out in the box; Sign in in the box"),
        );
    }
    match super::copy_state(index.get(&repo), mine_running) {
        "unreturned" | "busy" => block(
            "copy",
            "the box holds unreturned work for this repo; bring the last offload back first"
                .to_owned(),
        ),
        _ if running >= super::inbox::MAX_SESSIONS => block(
            "sessions",
            format!(
                "{running} box CLI sessions run in this box; at most {}",
                super::inbox::MAX_SESSIONS
            ),
        ),
        _ => {}
    }
    if !findings.is_empty() {
        return Err(Fail::Refused(
            "the box preflight blocks this offload".to_owned(),
            json!({"findings": findings, "cli": cli}),
        ));
    }
    let workspace = layout.workspace.clone();
    let agents = layout.home.join(AGENTS_DIR).join(&kind);
    let key = repo.clone();
    let (copy, was) = with_index(layout, move |index| {
        let was = match index.get(&key) {
            Some(c) if c.unreturned => {
                return Err(Fail::refused("the box holds unreturned work for this repo"));
            }
            Some(c) if c.dir.exists() => "mirror",
            _ => "absent",
        };
        if !index.contains_key(&key) {
            let (name, dir) = if no_repo {
                (wanted, agents)
            } else {
                let name = free_name(index, &key, &wanted, &workspace);
                let dir = workspace.join(&name);
                (name, dir)
            };
            index.insert(
                key.clone(),
                Copy {
                    name,
                    dir,
                    ..Copy::default()
                },
            );
        }
        let copy = index
            .get_mut(&key)
            .ok_or_else(|| Fail::Failed("index entry vanished".to_owned()))?;
        copy.offload_id = id;
        Ok((copy.clone(), was))
    })
    .await?;
    wipe_secrets(layout, &repo).await?;
    let _ = tokio::fs::remove_dir_all(layout.repo_dir(&repo).join(STAGE_DIR)).await;
    tokio::fs::create_dir_all(&copy.dir)
        .await
        .map_err(|e| format!("create {}: {e}", copy.dir.display()))?;
    let refs = copy_refs(&copy.dir).await;
    let ignored = if copy.dir.join(".git").exists() {
        ignored_dirs(&copy.dir).await.unwrap_or_default()
    } else {
        Vec::new()
    };
    Ok(json!({
        "ok": true,
        "dir": copy.dir,
        "name": copy.name,
        "copy": was,
        "refs": refs,
        "ignored": ignored,
        "cli": cli,
        "findings": [],
    }))
}

/// The offload stream: history, environment, secrets, the check against S, and the first prompt.
pub async fn send<R: Read + Send + 'static>(
    layout: &Layout,
    a: &Args,
    input: R,
) -> Result<Value, Fail> {
    let repo = a.repo()?;
    let kind = a.kind()?;
    let id = offload_id(a)?;
    let prepare = a.flag("prepare");
    let no_repo = a.flag("no-repo");
    let index = read_index(layout).await?;
    let copy = index
        .get(&repo)
        .filter(|c| c.offload_id == id)
        .cloned()
        .ok_or_else(|| Fail::refused(format!("the copy is not held by offload {id}; run begin")))?;
    if copy.unreturned {
        return Err(Fail::refused(
            "the box holds unreturned work for this repo; bring the last offload back first",
        ));
    }
    let job = Offload {
        layout,
        repo: &repo,
        kind: &kind,
        id: &id,
        copy: &copy,
        prepare,
        no_repo,
    };
    let mut timings = serde_json::Map::new();
    let stage = layout.repo_dir(&repo).join(STAGE_DIR);
    let result = job.run(input, &stage, &mut timings).await;
    let _ = tokio::fs::remove_dir_all(&stage).await;
    let mut out = match result {
        Ok(out) => out,
        Err(e) => {
            job.roll_back().await;
            return Err(e);
        }
    };
    out["timings_ms"] = Value::Object(timings);
    Ok(out)
}

/// A message or BACK for the repo's running session.
pub async fn message(layout: &Layout, a: &Args, text: &str) -> Result<Value, Fail> {
    let repo = a.repo()?;
    if text.len() > MAX_BODY {
        return Err(Fail::Usage(format!(
            "a message is at most {MAX_BODY} bytes"
        )));
    }
    call(
        layout,
        &json!({
            "op": "send",
            "repo": repo,
            "body": text,
            "back": a.flag("back"),
            "interrupt": a.flag("interrupt"),
        }),
    )
    .await
}

/// An outbox entry from the box agent; `--back` also records S'.
pub async fn post(layout: &Layout, a: &Args, body: &str) -> Result<Value, Fail> {
    let repo = a.repo_or_env()?;
    let kinds: Vec<&str> = ["progress", "question", "back"]
        .into_iter()
        .filter(|k| a.flag(k))
        .collect();
    let &[kind] = kinds.as_slice() else {
        return Err(Fail::Usage(
            "post takes one of --progress, --question, --back".to_owned(),
        ));
    };
    if kind == "question" {
        if body.len() > MAX_BODY {
            return Err(Fail::Usage(format!(
                "a question is at most {MAX_BODY} bytes"
            )));
        }
    } else {
        handover::check(body).map_err(Fail::refused)?;
    }
    let copy = read_index(layout)
        .await?
        .remove(&repo)
        .filter(|c| c.dir.join(".git").exists());
    if let Some(copy) = &copy {
        write_excludes(layout, &copy.dir).await?;
    }
    if kind == "back"
        && let Some(copy) = &copy
    {
        let tree = snapshot_tree(&copy.dir).await?;
        let mut args: Vec<&str> = SNAPSHOT_IDENTITY.to_vec();
        args.extend(["commit-tree", &tree]);
        let head = git(&copy.dir, &["rev-parse", "-q", "--verify", "HEAD"])
            .await
            .ok();
        if let Some(h) = &head {
            args.extend(["-p", h.as_str()]);
        }
        let msg = format!("cocoonbox back {}", copy.offload_id);
        args.extend(["-m", &msg]);
        let commit = git(&copy.dir, &args).await?;
        let name = format!("refs/cocoonbox/back/{}", copy.offload_id);
        git(&copy.dir, &["update-ref", &name, &commit]).await?;
    }
    let resp = call(
        layout,
        &json!({"op": "post", "repo": repo, "kind": kind, "body": body}),
    )
    .await?;
    Ok(json!({
        "ok": true,
        "seq": resp["seq"],
        "status": if kind == "back" { "ready" } else { "posted" },
    }))
}

/// Ends the repo's CLI, wipes its secrets and marks the copy returned.
pub async fn end(layout: &Layout, repo: &str) -> Result<Value, Fail> {
    let status = call(layout, &json!({"op": "status", "repo": repo})).await?;
    if let Some(s) = status["sessions"].as_array().and_then(|s| s.first()) {
        let cli = s["cli"].as_str().unwrap_or_default();
        let session = tmux_session(repo, cli);
        if s["tmux_alive"] == true {
            quit(layout, &session, cli).await;
        }
        call(
            layout,
            &json!({"op": "event", "repo": repo, "cli": cli, "hook": "SessionEnd", "payload": {}}),
        )
        .await?;
    }
    wipe_secrets(layout, repo).await?;
    if let Some(copy) = read_index(layout).await?.get(repo)
        && copy.dir.join(".git").exists()
    {
        write_excludes(layout, &copy.dir).await?;
    }
    let key = repo.to_owned();
    with_index(layout, move |index| {
        if let Some(c) = index.get_mut(&key) {
            c.unreturned = false;
        }
        Ok(())
    })
    .await?;
    Ok(json!({"ok": true, "repo": repo, "copy": "mirror"}))
}

/// Removes the repo's secrets and stops its ssh-agent.
pub async fn wipe_secrets(layout: &Layout, repo: &str) -> Result<(), Fail> {
    let dir = layout.secrets_dir(repo);
    if dir.join(SSH_SOCKET).exists() {
        let _ = user_systemctl(layout, &["stop", &ssh_unit(repo)]).await;
    }
    match tokio::fs::remove_dir_all(&dir).await {
        Ok(()) => Ok(()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(Fail::Failed(format!("remove {}: {e}", dir.display()))),
    }
}

/// One `send` stream's work.
struct Offload<'a> {
    layout: &'a Layout,
    repo: &'a str,
    kind: &'a str,
    id: &'a str,
    copy: &'a Copy,
    prepare: bool,
    no_repo: bool,
}

impl Offload<'_> {
    async fn run<R: Read + Send + 'static>(
        &self,
        input: R,
        stage: &Path,
        timings: &mut serde_json::Map<String, Value>,
    ) -> Result<Value, Fail> {
        let mut clock = Instant::now();
        let mut lap = |name: &str, timings: &mut serde_json::Map<String, Value>| {
            timings.insert(name.to_owned(), (clock.elapsed().as_millis() as u64).into());
            clock = Instant::now();
        };
        let dir = &self.copy.dir;
        super::create_private(stage)?;
        let secrets = self.layout.secrets_dir(self.repo);
        let unpacked = {
            let stage = stage.to_path_buf();
            let secrets = secrets.clone();
            tokio::task::spawn_blocking(move || unpack(input, &stage, &secrets))
                .await
                .map_err(|e| format!("stream task: {e}"))??
        };
        lap("stream", timings);
        let handover = match (&unpacked.handover, self.prepare) {
            (Some(h), false) => {
                handover::check(h).map_err(Fail::refused)?;
                Some(h.as_str())
            }
            (None, false) => return Err(Fail::Usage("the stream lacks handover.md".to_owned())),
            (_, true) => None,
        };
        let spec = match &unpacked.env {
            Some(raw) if !self.no_repo => {
                let spec: Spec = serde_json::from_slice(raw)
                    .map_err(|e| Fail::Usage(format!("decode env.json: {e}")))?;
                write_atomic(&self.layout.repo_dir(self.repo).join("env.json"), raw).await?;
                Some(spec)
            }
            _ => None,
        };
        if !self.no_repo {
            self.unbundle(stage, &unpacked).await?;
            lap("unbundle", timings);
            let _ = git(
                dir,
                &[
                    "-c",
                    "core.fsmonitor=false",
                    "update-index",
                    "-q",
                    "--refresh",
                ],
            )
            .await;
            lap("refresh", timings);
            self.check_snapshot().await?;
            lap("check", timings);
        }
        self.place_secrets(&secrets, &unpacked.secrets).await?;
        lap("secrets", timings);
        if !self.no_repo {
            write_excludes(self.layout, dir).await?;
            lap("excludes", timings);
        }
        if self.prepare {
            let report = match &spec {
                Some(s) => {
                    let report = env::plan(self.layout, self.repo, dir, s).await;
                    env::run(self.layout, self.repo, dir, s, report).await;
                    env::load(self.layout, self.repo).await?
                }
                None => None,
            };
            lap("env", timings);
            return Ok(json!({"ok": true, "accepted": false, "env": report}));
        }
        trust(&self.layout.home, self.kind, dir).await?;
        lap("trust", timings);
        let base = snapshot_ref(self.id);
        let mut prompt = handover::preamble(dir, self.repo, &self.layout.state, &base);
        prompt.push_str(handover.unwrap_or_default());
        if !self.no_repo {
            prompt.push_str(&self.attached(&base, spec.as_ref()).await);
        }
        let started_ms = crate::now_ms();
        let resp = call(
            self.layout,
            &json!({
                "op": "send",
                "repo": self.repo,
                "name": self.copy.name,
                "cli": self.kind,
                "offload_id": self.id,
                "body": prompt,
                "start": true,
            }),
        )
        .await?;
        lap("start", timings);
        let accepted = self.wait_accepted(started_ms).await;
        lap("accept", timings);
        Ok(json!({
            "ok": true,
            "delivery": "started",
            "accepted": accepted,
            "tmux": resp["tmux"],
        }))
    }

    /// Releases the copy and wipes what the stream left, after a failure before the inbox accepted.
    async fn roll_back(&self) {
        if let Err(e) = wipe_secrets(self.layout, self.repo).await {
            eprintln!("offload send: roll back: {e:?}");
        }
        let (key, id) = (self.repo.to_owned(), self.id.to_owned());
        let released = with_index(self.layout, move |index| {
            if let Some(c) = index.get_mut(&key).filter(|c| c.offload_id == id) {
                c.offload_id.clear();
                c.unreturned = false;
            }
            Ok(())
        })
        .await;
        if let Err(e) = released {
            eprintln!("offload send: roll back: {e:?}");
        }
    }

    /// Fills `.git` from the stream: objects from the bundle, the bundle's refs, a linked worktree's HEAD and index.
    async fn unbundle(&self, stage: &Path, unpacked: &Unpacked) -> Result<(), Fail> {
        let dir = &self.copy.dir;
        let gitdir = dir.join(".git");
        if unpacked.worktree && !gitdir.exists() {
            git(dir, &["init", "-q"]).await?;
        }
        if !gitdir.is_dir() {
            return Err(Fail::Failed(format!(
                "{} has no .git directory; rsync the repo first",
                dir.display()
            )));
        }
        for sub in ["objects/pack", "objects/info"] {
            tokio::fs::create_dir_all(gitdir.join(sub))
                .await
                .map_err(|e| format!("create .git/{sub}: {e}"))?;
        }
        if unpacked.bundle {
            for (oid, name) in index_bundle(dir, &stage.join(BUNDLE)).await? {
                git(dir, &["update-ref", &name, &oid]).await?;
            }
        }
        if unpacked.worktree {
            for f in ["HEAD", "index"] {
                tokio::fs::copy(stage.join(f), gitdir.join(f))
                    .await
                    .map_err(|e| format!("place .git/{f}: {e}"))?;
            }
        }
        Ok(())
    }

    /// The copy equals S: its snapshot's tree is S's tree and HEAD is S's parent.
    async fn check_snapshot(&self) -> Result<(), Fail> {
        let dir = &self.copy.dir;
        let s = snapshot_ref(self.id);
        let want = git(
            dir,
            &["rev-parse", "-q", "--verify", &format!("{s}^{{tree}}")],
        )
        .await
        .map_err(|_| Fail::Failed(format!("{s} is missing from the copy")))?;
        let parent = git(dir, &["rev-parse", "-q", "--verify", &format!("{s}^1")])
            .await
            .ok();
        let head = git(dir, &["rev-parse", "-q", "--verify", "HEAD"])
            .await
            .ok();
        if head != parent {
            return Err(Fail::Failed(format!(
                "the copy's HEAD {} is not the parent of {s}",
                head.as_deref().unwrap_or("(unborn)")
            )));
        }
        let tree = snapshot_tree(dir).await?;
        if tree != want {
            return Err(Fail::Failed(format!(
                "the copy's files differ from {s} (tree {tree}, want {want})"
            )));
        }
        Ok(())
    }

    /// Links file secrets into the copy, loads ssh keys, and writes the secrets' variables.
    async fn place_secrets(&self, secrets: &Path, entries: &[String]) -> Result<(), Fail> {
        let mut vars = BTreeMap::new();
        let mut keys = Vec::new();
        for entry in entries {
            let held = secrets.join(entry);
            if let Some(rel) = entry.strip_prefix("file/") {
                let link = self.copy.dir.join(rel);
                if let Some(parent) = link.parent() {
                    tokio::fs::create_dir_all(parent)
                        .await
                        .map_err(|e| format!("create {}: {e}", parent.display()))?;
                }
                let _ = tokio::fs::remove_file(&link).await;
                tokio::fs::symlink(&held, &link)
                    .await
                    .map_err(|e| format!("link {}: {e}", link.display()))?;
            } else if entry == "aws" {
                vars.insert("AWS_SHARED_CREDENTIALS_FILE", held);
            } else if entry == "kube" {
                vars.insert("KUBECONFIG", held);
            } else if entry.starts_with("gcloud/") {
                vars.insert("CLOUDSDK_CONFIG", secrets.join("gcloud"));
            } else if entry.starts_with("ssh/") {
                keys.push(held);
            }
        }
        if !keys.is_empty() {
            let socket = secrets.join(SSH_SOCKET);
            start_ssh_agent(self.layout, self.repo, &socket, &keys).await?;
            vars.insert("SSH_AUTH_SOCK", socket);
        }
        if vars.is_empty() {
            return Ok(());
        }
        let text: String = vars
            .iter()
            .map(|(k, v)| format!("{k}={}\n", v.display()))
            .collect();
        write_atomic(&secrets.join("env"), text.as_bytes()).await?;
        Ok(())
    }

    /// The packer's facts for the box agent, after the hand-over.
    async fn attached(&self, base: &str, spec: Option<&Spec>) -> String {
        let dir = &self.copy.dir;
        let branch = git(dir, &["branch", "--show-current"])
            .await
            .ok()
            .filter(|b| !b.is_empty())
            .unwrap_or_else(|| "(detached)".to_owned());
        let s = git(dir, &["rev-parse", "--short", base])
            .await
            .unwrap_or_default();
        let status = git(dir, &["status", "--short"]).await.unwrap_or_default();
        let status = head_lines(&status, ATTACHED_LINES);
        let log = match git(dir, &["log", "--oneline", "@{upstream}..HEAD"]).await {
            Ok(l) => head_lines(&l, ATTACHED_LINES),
            Err(_) => git(dir, &["log", "--oneline", "-5"])
                .await
                .unwrap_or_default(),
        };
        let mut mac_only = Vec::new();
        if let Ok(h) = git(dir, &["config", "--get-all", "credential.helper"]).await
            && h.contains("osxkeychain")
        {
            mac_only.push("credential.helper osxkeychain".to_owned());
        }
        if let Ok(p) = git(dir, &["config", "--get", "core.hooksPath"]).await
            && p.starts_with('/')
        {
            mac_only.push(format!("core.hooksPath {p}"));
        }
        let env = spec.map_or_else(|| "none".to_owned(), short_spec);
        format!(
            "\n\n## Attached by the packer\n\n\
- Branch: {branch}; the Mac's snapshot S is {base} ({s}); HEAD is its parent.\n\
- `git status --short`:\n```\n{status}\n```\n\
- Commits (`git log --oneline @{{upstream}}..HEAD`, else the last 5):\n```\n{log}\n```\n\
- Mac-only settings in .git/config: {}\n\
- env.json: {env}\n",
            if mac_only.is_empty() {
                "none".to_owned()
            } else {
                mac_only.join("; ")
            }
        )
    }

    /// The CLI took the prompt: its prompt hook fired, or a transcript changed, since `since_ms`.
    async fn wait_accepted(&self, since_ms: i64) -> bool {
        let deadline = Instant::now() + ACCEPT_WAIT;
        let req = json!({"op": "status", "repo": self.repo});
        while Instant::now() < deadline {
            if let Ok(status) = call(self.layout, &req).await
                && let Some(s) = status["sessions"].as_array().and_then(|s| s.first())
                && (s["hook_at_ms"].as_i64() >= Some(since_ms)
                    || s["transcript_mtime_ms"].as_i64() >= Some(since_ms))
            {
                return true;
            }
            tokio::time::sleep(ACCEPT_POLL).await;
        }
        false
    }
}

/// What the stream carried.
#[derive(Debug, Default)]
struct Unpacked {
    handover: Option<String>,
    env: Option<Vec<u8>>,
    bundle: bool,
    worktree: bool,
    /// Secret entries relative to `secrets/`, already on the tmpfs.
    secrets: Vec<String>,
}

/// Reads the tar stream in order: small members into memory, the bundle and a worktree's
/// HEAD and index into `stage`, secrets straight onto the tmpfs.
fn unpack<R: Read>(input: R, stage: &Path, secrets: &Path) -> Result<Unpacked, Fail> {
    let mut out = Unpacked::default();
    let mut archive = tar::Archive::new(input);
    let entries = archive
        .entries()
        .map_err(|e| Fail::Usage(format!("read the stream: {e}")))?;
    for entry in entries {
        let mut entry = entry.map_err(|e| Fail::Usage(format!("read the stream: {e}")))?;
        let kind = entry.header().entry_type();
        if kind.is_dir() {
            continue;
        }
        if !kind.is_file() {
            return Err(Fail::Usage(format!("the stream holds a {kind:?} member")));
        }
        let path = entry
            .path()
            .map_err(|e| Fail::Usage(format!("member name: {e}")))?
            .to_string_lossy()
            .into_owned();
        match path.as_str() {
            "handover.md" => {
                let mut s = String::new();
                entry
                    .by_ref()
                    .take(MAX_BODY as u64 + 1)
                    .read_to_string(&mut s)
                    .map_err(|e| Fail::Usage(format!("read handover.md: {e}")))?;
                out.handover = Some(s);
            }
            "env.json" => {
                let mut raw = Vec::new();
                entry
                    .by_ref()
                    .take(MAX_BODY as u64)
                    .read_to_end(&mut raw)
                    .map_err(|e| Fail::Usage(format!("read env.json: {e}")))?;
                out.env = Some(raw);
            }
            BUNDLE | "HEAD" | "index" => {
                copy_out(&mut entry, &stage.join(&path), 0o600)?;
                out.bundle |= path == BUNDLE;
                out.worktree |= path != BUNDLE;
            }
            other => {
                let Some(rel) = other.strip_prefix("secrets/").filter(|r| safe_rel(r)) else {
                    return Err(Fail::Usage(format!("unexpected stream member {other:?}")));
                };
                super::create_private(secrets)?;
                let dest = secrets.join(rel);
                if let Some(parent) = dest.parent() {
                    super::create_private(parent)?;
                }
                copy_out(&mut entry, &dest, 0o600)?;
                out.secrets.push(rel.to_owned());
            }
        }
    }
    Ok(out)
}

fn copy_out(src: &mut impl Read, dest: &Path, mode: u32) -> Result<(), Fail> {
    use std::os::unix::fs::OpenOptionsExt;
    let mut f = std::fs::OpenOptions::new()
        .create(true)
        .truncate(true)
        .write(true)
        .mode(mode)
        .open(dest)
        .map_err(|e| format!("create {}: {e}", dest.display()))?;
    std::io::copy(src, &mut f).map_err(|e| format!("write {}: {e}", dest.display()))?;
    Ok(())
}

/// A relative path of plain components.
fn safe_rel(rel: &str) -> bool {
    !rel.is_empty()
        && Path::new(rel)
            .components()
            .all(|c| matches!(c, Component::Normal(_)))
}

/// Indexes a bundle's pack into the copy and returns the bundle's refs. `git bundle unbundle`
/// refuses here: the refs rsync brought already name objects that only the bundle holds.
async fn index_bundle(dir: &Path, bundle: &Path) -> Result<Vec<(String, String)>, String> {
    let file = tokio::fs::File::open(bundle)
        .await
        .map_err(|e| format!("open the bundle: {e}"))?;
    let mut reader = tokio::io::BufReader::new(file);
    let mut line = String::new();
    reader
        .read_line(&mut line)
        .await
        .map_err(|e| format!("read the bundle: {e}"))?;
    if !matches!(line.trim_end(), "# v2 git bundle" | "# v3 git bundle") {
        return Err(format!("not a git bundle: {:?}", line.trim_end()));
    }
    let mut refs = Vec::new();
    loop {
        line.clear();
        let n = reader
            .read_line(&mut line)
            .await
            .map_err(|e| format!("read the bundle: {e}"))?;
        let l = line.trim_end();
        if n == 0 || l.is_empty() {
            break;
        }
        if l.starts_with(['-', '@']) {
            continue;
        }
        if let Some((oid, name)) = l.split_once(' ')
            && name.starts_with("refs/")
        {
            refs.push((oid.to_owned(), name.to_owned()));
        }
    }
    let mut child = tokio::process::Command::new("git")
        .args(["index-pack", "--stdin", "--fix-thin"])
        .current_dir(dir)
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::piped())
        .kill_on_drop(true)
        .spawn()
        .map_err(|e| format!("spawn git index-pack: {e}"))?;
    if let Some(mut stdin) = child.stdin.take() {
        tokio::io::copy(&mut reader, &mut stdin)
            .await
            .map_err(|e| format!("feed git index-pack: {e}"))?;
    }
    let out = child
        .wait_with_output()
        .await
        .map_err(|e| format!("wait for git index-pack: {e}"))?;
    if !out.status.success() {
        return Err(format!(
            "git index-pack: {}",
            String::from_utf8_lossy(&out.stderr).trim()
        ));
    }
    Ok(refs)
}

/// The tree of a temporary-index snapshot of the work tree (`git add -A`); the real index is untouched.
async fn snapshot_tree(dir: &Path) -> Result<String, String> {
    let gitdir = dir.join(".git");
    let tmp = gitdir.join(format!("cocoonbox-snapshot-{}", std::process::id()));
    if tokio::fs::metadata(gitdir.join("index")).await.is_ok() {
        tokio::fs::copy(gitdir.join("index"), &tmp)
            .await
            .map_err(|e| format!("copy the index: {e}"))?;
    }
    let env = [("GIT_INDEX_FILE", tmp.as_path())];
    let tree = match git_env(dir, &["-c", "core.fsmonitor=false", "add", "-A"], &env).await {
        Ok(_) => git_env(dir, &["write-tree"], &env).await,
        Err(e) => Err(e),
    };
    let _ = tokio::fs::remove_file(&tmp).await;
    tree
}

/// Version, sign-in and account of the box CLI of `kind`, from a login shell.
async fn probe_cli(home: &Path, kind: &str) -> Value {
    let (version, auth) = match kind {
        "claude" => ("claude --version", "claude auth status"),
        "codex" => ("codex --version", "codex login status"),
        _ => ("cursor-agent --version", "cursor-agent status"),
    };
    let run = |cmd: &'static str| async move {
        tokio::time::timeout(PROBE_TIMEOUT, home_shell(home, cmd))
            .await
            .unwrap_or_else(|_| Err(format!("{cmd}: timed out")))
    };
    let version = run(version).await.ok().map(|v| v.trim().to_owned());
    let auth = match &version {
        Some(_) => run(auth).await,
        None => Err(String::new()),
    };
    let (signed_in, account) = match &auth {
        Ok(out) => sign_in(out),
        Err(_) => (false, String::new()),
    };
    let hooks_blocked = match kind {
        "claude" => hooks_blocked(&claude_settings(home).await).await,
        _ => None,
    };
    json!({
        "kind": kind,
        "version": version.unwrap_or_default(),
        "signed_in": signed_in,
        "account": account,
        "hooks_blocked": hooks_blocked,
    })
}

/// The Claude Code settings files that can turn hooks off: managed, cached remote-managed, user.
async fn claude_settings(home: &Path) -> Vec<PathBuf> {
    let managed = Path::new(CLAUDE_MANAGED);
    let mut files = vec![managed.join("managed-settings.json")];
    if let Ok(mut rd) = tokio::fs::read_dir(managed.join("managed-settings.d")).await {
        while let Ok(Some(e)) = rd.next_entry().await {
            files.push(e.path());
        }
    }
    files.push(home.join(".claude/remote-settings.json"));
    files.push(home.join(".claude/settings.json"));
    files
}

/// The first of `files` that sets `allowManagedHooksOnly` or `disableAllHooks`, which drop the inbox's hooks.
async fn hooks_blocked(files: &[PathBuf]) -> Option<PathBuf> {
    for file in files {
        let Ok(raw) = tokio::fs::read(file).await else {
            continue;
        };
        let Ok(v) = serde_json::from_slice::<Value>(&raw) else {
            continue;
        };
        if v["allowManagedHooksOnly"] == true || v["disableAllHooks"] == true {
            return Some(file.clone());
        }
    }
    None
}

/// Whether a CLI's successful status output says it is signed in, and the account it names:
/// JSON (`claude auth status`) by `loggedIn` and `email` or `authMethod`, text by its first line.
fn sign_in(out: &str) -> (bool, String) {
    if let Ok(v) = serde_json::from_str::<Value>(out) {
        let account = ["email", "account", "authMethod"]
            .iter()
            .find_map(|k| v[*k].as_str())
            .unwrap_or_default();
        return (v["loggedIn"] != false, account.to_owned());
    }
    let first = out.lines().map(str::trim).find(|l| !l.is_empty());
    (true, first.unwrap_or_default().to_owned())
}

/// Presets the CLI's trust for `dir` and nothing else of its config.
async fn trust(home: &Path, kind: &str, dir: &Path) -> Result<(), Fail> {
    let dir = dir.to_string_lossy();
    match kind {
        "claude" => {
            let path = home.join(".claude.json");
            let mut v: Value = match tokio::fs::read(&path).await {
                Ok(raw) => serde_json::from_slice(&raw)
                    .map_err(|e| format!("decode {}: {e}", path.display()))?,
                Err(_) => json!({}),
            };
            let Some(top) = v.as_object_mut() else {
                return Err(Fail::Failed(format!("{} is not an object", path.display())));
            };
            top.insert("hasCompletedOnboarding".to_owned(), true.into());
            let projects = top.entry("projects").or_insert_with(|| json!({}));
            if !projects.is_object() {
                *projects = json!({});
            }
            let project = projects
                .as_object_mut()
                .map(|p| p.entry(dir.into_owned()).or_insert_with(|| json!({})));
            match project {
                Some(Value::Object(p)) => {
                    p.insert("hasTrustDialogAccepted".to_owned(), true.into());
                }
                Some(other) => *other = json!({"hasTrustDialogAccepted": true}),
                None => {}
            }
            let raw = serde_json::to_vec_pretty(&v).map_err(|e| format!("encode: {e}"))?;
            write_atomic(&path, &raw).await?;
            tokio::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600))
                .await
                .map_err(|e| format!("chmod {}: {e}", path.display()))?;
        }
        "codex" => {
            let path = home.join(".codex/config.toml");
            let header = format!("[projects.\"{dir}\"]");
            let text = tokio::fs::read_to_string(&path).await.unwrap_or_default();
            if !text.lines().any(|l| l.trim() == header) {
                if let Some(parent) = path.parent() {
                    tokio::fs::create_dir_all(parent)
                        .await
                        .map_err(|e| format!("create {}: {e}", parent.display()))?;
                }
                let sep = if text.is_empty() || text.ends_with('\n') {
                    ""
                } else {
                    "\n"
                };
                let text = format!("{text}{sep}\n{header}\ntrust_level = \"trusted\"\n");
                write_atomic(&path, text.as_bytes()).await?;
            }
        }
        _ => {}
    }
    Ok(())
}

/// Asks the CLI to quit, sends it SIGTERM after `QUIT_WAIT`, then ends the tmux session.
async fn quit(layout: &Layout, session: &str, cli: &str) {
    let bin = layout.tmux.as_path();
    let target = format!("={session}:");
    let exact = format!("={session}");
    let word = if cli == "codex" { "/quit" } else { "/exit" };
    run_tmux(bin, &["send-keys", "-t", &target, "Escape"]).await;
    run_tmux(bin, &["send-keys", "-t", &target, "-l", word]).await;
    run_tmux(bin, &["send-keys", "-t", &target, "Enter"]).await;
    if gone(bin, &exact, QUIT_WAIT).await {
        return;
    }
    let pid = run_tmux(
        bin,
        &["display-message", "-p", "-t", &target, "#{pane_pid}"],
    )
    .await
    .and_then(|p| p.parse::<u32>().ok());
    if let Some(pid) = pid {
        let _ = crate::sys::signal_process(pid, libc::SIGTERM);
    }
    if !gone(bin, &exact, TERM_WAIT).await {
        run_tmux(bin, &["kill-session", "-t", &exact]).await;
    }
}

/// Whether the tmux session ends within `wait`.
async fn gone(bin: &Path, exact: &str, wait: Duration) -> bool {
    let deadline = Instant::now() + wait;
    while Instant::now() < deadline {
        if run_tmux(bin, &["has-session", "-t", exact]).await.is_none() {
            return true;
        }
        tokio::time::sleep(QUIT_POLL).await;
    }
    false
}

/// Runs tmux; its trimmed stdout when it succeeds.
async fn run_tmux(bin: &Path, args: &[&str]) -> Option<String> {
    tokio::process::Command::new(bin)
        .args(args)
        .stdin(std::process::Stdio::null())
        .output()
        .await
        .ok()
        .filter(|o| o.status.success())
        .map(|o| String::from_utf8_lossy(&o.stdout).trim().to_owned())
}

/// Replaces the copy's lines in `excludes` with its git-ignored directories.
async fn write_excludes(layout: &Layout, dir: &Path) -> Result<(), Fail> {
    let ignored: Vec<PathBuf> = ignored_dirs(dir)
        .await?
        .iter()
        .map(|d| dir.join(d.trim_end_matches('/')))
        .collect();
    let file = layout.excludes();
    let dir = dir.to_path_buf();
    locked(layout, move |_| {
        let old = std::fs::read_to_string(&file).unwrap_or_default();
        let mut text: String = old
            .lines()
            .filter(|l| !Path::new(l).starts_with(&dir))
            .map(|l| format!("{l}\n"))
            .collect();
        for p in ignored {
            text.push_str(&format!("{}\n", p.display()));
        }
        let tmp = file.with_extension("tmp");
        std::fs::write(&tmp, text).map_err(|e| format!("write {}: {e}", tmp.display()))?;
        std::fs::rename(&tmp, &file).map_err(|e| format!("replace {}: {e}", file.display()))?;
        Ok(())
    })
    .await
}

/// The copy's git-ignored directories as `git ls-files -oi --exclude-standard --directory` lists
/// them, relative and with their trailing `/`.
async fn ignored_dirs(dir: &Path) -> Result<Vec<String>, String> {
    let listed = git(
        dir,
        &["ls-files", "-z", "-oi", "--exclude-standard", "--directory"],
    )
    .await?;
    Ok(listed
        .split('\0')
        .filter(|l| l.ends_with('/'))
        .map(str::to_owned)
        .collect())
}

/// Starts the repo's ssh-agent as a transient user unit with its socket on the tmpfs, adds the
/// keys and deletes them.
async fn start_ssh_agent(
    layout: &Layout,
    repo: &str,
    socket: &Path,
    keys: &[PathBuf],
) -> Result<(), Fail> {
    let _ = user_systemctl(layout, &["stop", &ssh_unit(repo)]).await;
    let _ = tokio::fs::remove_file(socket).await;
    let sock = socket.to_string_lossy();
    let unit = format!("--unit={}", ssh_unit(repo));
    user_command(
        layout,
        "systemd-run",
        &[
            "--user",
            "--quiet",
            "--collect",
            &unit,
            "ssh-agent",
            "-D",
            "-a",
            &sock,
        ],
    )
    .await?;
    let deadline = Instant::now() + AGENT_WAIT;
    while !socket.exists() {
        if Instant::now() >= deadline {
            return Err(Fail::Failed("ssh-agent did not start".to_owned()));
        }
        tokio::time::sleep(QUIT_POLL).await;
    }
    for key in keys {
        let out = tokio::process::Command::new("ssh-add")
            .arg(key)
            .env("SSH_AUTH_SOCK", socket)
            .stdin(std::process::Stdio::null())
            .output()
            .await
            .map_err(|e| format!("spawn ssh-add: {e}"))?;
        let _ = tokio::fs::remove_file(key).await;
        if !out.status.success() {
            return Err(Fail::Failed(format!(
                "ssh-add refused a key: {}",
                String::from_utf8_lossy(&out.stderr).trim()
            )));
        }
    }
    Ok(())
}

fn ssh_unit(repo: &str) -> String {
    format!("cb-off-ssh-{repo}")
}

async fn user_systemctl(layout: &Layout, args: &[&str]) -> Result<(), String> {
    user_command(layout, "systemctl", &[&["--user"], args].concat()).await
}

/// Runs a user-manager command; an rsh command lacks `XDG_RUNTIME_DIR`, so it is set from the home's owner.
async fn user_command(layout: &Layout, bin: &str, args: &[&str]) -> Result<(), String> {
    let uid = std::fs::metadata(&layout.home)
        .map(|m| m.uid())
        .map_err(|e| format!("stat {}: {e}", layout.home.display()))?;
    let out = tokio::process::Command::new(bin)
        .args(args)
        .env("XDG_RUNTIME_DIR", format!("/run/user/{uid}"))
        .stdin(std::process::Stdio::null())
        .output()
        .await
        .map_err(|e| format!("spawn {bin}: {e}"))?;
    if out.status.success() {
        Ok(())
    } else {
        Err(format!(
            "{bin} {}: {}",
            args.join(" "),
            String::from_utf8_lossy(&out.stderr).trim()
        ))
    }
}

/// The copy's refs and their objects; empty when it has no history yet.
async fn copy_refs(dir: &Path) -> Value {
    if !dir.join(".git/objects").is_dir() {
        return json!({});
    }
    let listed = git(dir, &["for-each-ref", "--format=%(refname) %(objectname)"])
        .await
        .unwrap_or_default();
    let refs: serde_json::Map<String, Value> = listed
        .lines()
        .filter_map(|l| l.split_once(' '))
        .map(|(r, o)| (r.to_owned(), o.into()))
        .collect();
    Value::Object(refs)
}

/// `wanted`, or `wanted-2`, `wanted-3`, …: the first name no other key holds and no foreign directory has.
fn free_name(index: &super::Index, key: &str, wanted: &str, workspace: &Path) -> String {
    (1..)
        .map(|n| {
            if n == 1 {
                wanted.to_owned()
            } else {
                let suffix = format!("-{n}");
                let mut base = wanted.to_owned();
                base.truncate(MAX_NAME - suffix.len());
                base + &suffix
            }
        })
        .find(|name| {
            !index.iter().any(|(k, c)| k != key && c.name == *name)
                && !workspace.join(name).exists()
        })
        .unwrap_or_else(|| wanted.to_owned())
}

/// The Mac's name rules: `A-Za-z0-9._-`, no leading dot, at most 64 characters.
fn valid_name(name: &str) -> Result<(), Fail> {
    let ok = !name.is_empty()
        && name.len() <= MAX_NAME
        && !name.starts_with('.')
        && name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'.' | b'_' | b'-'));
    if ok {
        Ok(())
    } else {
        Err(Fail::Usage(format!("invalid name {name:?}")))
    }
}

fn offload_id(a: &Args) -> Result<String, Fail> {
    let id = a.required("id")?;
    if is_hex(&id, 12) {
        Ok(id)
    } else {
        Err(Fail::Usage(format!("invalid offload id {id:?}")))
    }
}

/// The first `n` lines of `text`, and a count of the rest.
fn head_lines(text: &str, n: usize) -> String {
    let total = text.lines().count();
    let mut out: Vec<&str> = text.lines().take(n).collect();
    let more = format!("… {} more lines", total.saturating_sub(n));
    if total > n {
        out.push(&more);
    }
    out.join("\n")
}

fn short_spec(spec: &Spec) -> String {
    let list = |m: &BTreeMap<String, String>| {
        m.iter()
            .map(|(k, v)| format!("{k} {v}"))
            .collect::<Vec<_>>()
            .join(", ")
    };
    let mut parts = Vec::new();
    if !spec.tools.is_empty() {
        parts.push(format!("tools {}", list(&spec.tools)));
    }
    if !spec.package_managers.is_empty() {
        parts.push(format!("package managers {}", list(&spec.package_managers)));
    }
    if !spec.lockfiles.is_empty() {
        parts.push(format!("lockfiles {}", spec.lockfiles.join(", ")));
    }
    if parts.is_empty() {
        "nothing".to_owned()
    } else {
        parts.join("; ")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn settings_that_drop_non_managed_hooks_are_named() {
        let dir = tempfile::tempdir().unwrap();
        let files: Vec<PathBuf> = ["a.json", "b.json", "c.json", "d.json"]
            .iter()
            .map(|n| dir.path().join(n))
            .collect();
        std::fs::write(&files[1], r#"{"hooks": {}}"#).unwrap();
        std::fs::write(&files[2], "not json").unwrap();
        assert_eq!(hooks_blocked(&files).await, None);
        std::fs::write(&files[3], r#"{"allowManagedHooksOnly": true}"#).unwrap();
        assert_eq!(hooks_blocked(&files).await, Some(files[3].clone()));
        std::fs::write(&files[1], r#"{"disableAllHooks": true}"#).unwrap();
        assert_eq!(hooks_blocked(&files).await, Some(files[1].clone()));
    }

    #[test]
    fn sign_in_reads_json_and_text_status() {
        let claude =
            r#"{"loggedIn": true, "authMethod": "oauth_token", "apiProvider": "firstParty"}"#;
        assert_eq!(sign_in(claude), (true, "oauth_token".to_owned()));
        let out = r#"{"loggedIn": false}"#;
        assert_eq!(sign_in(out), (false, String::new()));
        assert_eq!(
            sign_in("\nLogged in using ChatGPT\n"),
            (true, "Logged in using ChatGPT".to_owned())
        );
    }

    #[test]
    fn free_names_skip_other_keys_and_foreign_directories() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::create_dir(dir.path().join("gron-2")).unwrap();
        let mut index = super::super::Index::new();
        index.insert(
            "aaaaaaaaaaaa".into(),
            Copy {
                name: "gron".into(),
                ..Copy::default()
            },
        );
        assert_eq!(
            free_name(&index, "bbbbbbbbbbbb", "gron", dir.path()),
            "gron-3"
        );
        assert_eq!(
            free_name(&index, "aaaaaaaaaaaa", "gron", dir.path()),
            "gron"
        );
        assert!(valid_name("my.repo_1-x").is_ok());
        assert!(valid_name(".hidden").is_err() && valid_name("a/b").is_err());
    }
}
