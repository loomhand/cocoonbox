//! The inbox (`offload serve`, a user unit of `user`): owns each repo's box CLI session in tmux,
//! keeps its inbox and outbox, follows the CLI's hooks, and restarts a lost session.

use std::collections::{HashMap, HashSet};
use std::os::unix::fs::MetadataExt;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncSeekExt, AsyncWriteExt, BufReader};
use tokio::net::UnixStream;
use tokio::net::unix::OwnedWriteHalf;
use tokio::sync::{Mutex, broadcast, watch};
use tokio_util::sync::CancellationToken;

use super::env::{self, Spec};
use super::handover::{self, MAX_BODY};
use super::packer::ACCEPT_WAIT;
use super::{Copy, Layout, read_index, with_index, write_atomic};
use crate::journal::Journal;
use crate::pb::{self, event};

/// At most this many box CLI sessions are not stopped at a time.
pub const MAX_SESSIONS: usize = 3;
const BLOCKED_BODY: usize = 2 << 10;
const PASTE_SETTLE: Duration = Duration::from_millis(500);
const DELIVERY_DELAY: Duration = Duration::from_millis(500);
const RESCAN: Duration = Duration::from_secs(5);
const RELAY_RETRY: Duration = Duration::from_secs(2);
const CHANGES: usize = 256;
const TRANSCRIPT_TAIL: u64 = 256 << 10;
const MAX_WAIT_MS: u64 = 600_000;
const SOCKET_GROUP: &str = "cbagent";
const RECORD_FILE: &str = "session.json";
const INBOX_FILE: &str = "inbox.jsonl";
const OUTBOX_FILE: &str = "outbox.jsonl";
const PROMPT_FILE: &str = "prompt.md";
const RESTART_FILE: &str = "restart.md";
const HOOKS_FILE: &str = "hooks.json";
// Claude Code ends a turn that an API error stopped with StopFailure, not Stop
const HOOKS: [&str; 5] = [
    "UserPromptSubmit",
    "Stop",
    "StopFailure",
    "Notification",
    "SessionEnd",
];
// the start of a turn's last message when an API error or a usage limit ended it
const BLOCKING_STARTS: [&str; 7] = [
    "API Error",
    "Claude AI usage limit reached",
    "5-hour limit reached",
    "Weekly limit reached",
    "Session limit reached",
    "Opus weekly limit reached",
    "You've hit your",
];

/// A box CLI session's status, from its hooks.
#[derive(Clone, Copy, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Status {
    Working,
    Idle,
    WaitingForUser,
    Blocked,
    #[default]
    Stopped,
}

impl Status {
    /// The CLI waits for input, so a message is pasted rather than queued.
    fn deliverable(self) -> bool {
        matches!(self, Self::Idle | Self::WaitingForUser | Self::Blocked)
    }

    fn pb(self) -> pb::OffloadStatus {
        match self {
            Self::Working => pb::OffloadStatus::Working,
            Self::Idle => pb::OffloadStatus::Idle,
            Self::WaitingForUser => pb::OffloadStatus::WaitingForUser,
            Self::Blocked => pb::OffloadStatus::Blocked,
            Self::Stopped => pb::OffloadStatus::Stopped,
        }
    }
}

/// The kind of an outbox entry; `blocked` is written by the inbox only.
#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum EntryKind {
    Progress,
    Question,
    Back,
    Blocked,
}

impl EntryKind {
    fn pb(self) -> pb::OffloadEntry {
        match self {
            Self::Progress => pb::OffloadEntry::Progress,
            Self::Question => pb::OffloadEntry::Question,
            Self::Back => pb::OffloadEntry::Back,
            Self::Blocked => pb::OffloadEntry::Blocked,
        }
    }
}

/// One outbox entry.
#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Entry {
    pub seq: u64,
    pub kind: EntryKind,
    pub offload_id: String,
    pub cli: String,
    pub time_ms: i64,
    pub body: String,
}

/// A change of a session, as a `watch` line; it never carries a body.
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct Change {
    pub repo: String,
    pub name: String,
    pub cli: String,
    pub offload_id: String,
    /// Unset for a status change.
    pub entry: Option<EntryKind>,
    /// The outbox seq of `entry`; 0 for a status change.
    pub seq: u64,
    pub status: Status,
}

impl Change {
    /// The `Session.Events` form.
    pub fn update(&self) -> pb::OffloadUpdate {
        pb::OffloadUpdate {
            repo: self.repo.clone(),
            name: self.name.clone(),
            cli: self.cli.clone(),
            offload_id: self.offload_id.clone(),
            entry: self
                .entry
                .map_or(pb::OffloadEntry::Unspecified, EntryKind::pb) as i32,
            seq: self.seq,
            status: self.status.pb() as i32,
        }
    }
}

/// The inbox's state: the session records behind one lock, the change feed, and the repos whose environment step runs.
pub struct Inbox {
    layout: Layout,
    records: Mutex<HashMap<String, Record>>,
    changes: broadcast::Sender<Change>,
    steps: watch::Sender<HashSet<String>>,
}

impl Inbox {
    /// Loads the session records under `layout.state`.
    pub async fn open(layout: Layout) -> Result<Arc<Self>, String> {
        super::create_private(&layout.state)?;
        let records = load_records(&layout.state).await;
        Ok(Arc::new(Self {
            layout,
            records: Mutex::new(records),
            changes: broadcast::channel(CHANGES).0,
            steps: watch::channel(HashSet::new()).0,
        }))
    }

    /// Serves connections on `listener` until `stop`; restarts lost sessions first and again on
    /// each `rescan`, which the agent sends after a restore replaced the state directory.
    pub async fn run(self: Arc<Self>, listener: tokio::net::UnixListener, stop: CancellationToken) {
        let rescan = Arc::clone(&self);
        let rescan_stop = stop.clone();
        // detached: ends with `stop`
        tokio::spawn(async move {
            rescan.restart_lost().await;
            loop {
                tokio::select! {
                    () = tokio::time::sleep(RESCAN) => {}
                    () = rescan_stop.cancelled() => return,
                }
                rescan.idle_without_hooks().await;
            }
        });
        loop {
            let accepted = tokio::select! {
                a = listener.accept() => a,
                () = stop.cancelled() => return,
            };
            match accepted {
                // detached: one task per connection
                Ok((stream, _)) => {
                    tokio::spawn(Arc::clone(&self).connection(stream));
                }
                Err(e) => eprintln!("offload inbox: accept: {e}"),
            }
        }
    }

    async fn connection(self: Arc<Self>, stream: UnixStream) {
        let (rd, mut wr) = stream.into_split();
        let mut lines = BufReader::new(rd).take(MAX_BODY as u64 * 2).lines();
        while let Ok(Some(line)) = lines.next_line().await {
            let req = match serde_json::from_str::<Request>(&line) {
                Ok(Request::Watch) => {
                    self.watch(&mut wr).await;
                    return;
                }
                Ok(req) => req,
                Err(e) => {
                    let _ = reply(
                        &mut wr,
                        &Refusal::invalid(format!("bad request: {e}")).json(),
                    )
                    .await;
                    continue;
                }
            };
            let out = self.handle(req).await.unwrap_or_else(|r| r.json());
            if reply(&mut wr, &out).await.is_err() {
                return;
            }
            lines.get_mut().set_limit(MAX_BODY as u64 * 2);
        }
    }

    async fn handle(self: &Arc<Self>, req: Request) -> Result<Value, Refusal> {
        match req {
            Request::Send(s) => self.send(s).await,
            Request::Post { repo, kind, body } => self.post(&repo, kind, body).await,
            Request::Read {
                repo,
                since,
                wait_ms,
            } => self.read(&repo, since, wait_ms).await,
            Request::Env { repo, wait_ms } => self.env(&repo, wait_ms).await,
            Request::Status { repo } => self.status(repo.as_deref()).await,
            Request::Restart { repo } => self.restart(&repo).await,
            Request::Event {
                repo,
                cli,
                hook,
                payload,
            } => self.event(&repo, &cli, &hook, &payload).await,
            Request::Rescan => {
                *self.records.lock().await = load_records(&self.layout.state).await;
                self.restart_lost().await;
                Ok(json!({"ok": true}))
            }
            Request::Watch => Err(Refusal::invalid("watch is served per connection")),
        }
    }

    async fn send(self: &Arc<Self>, req: SendRequest) -> Result<Value, Refusal> {
        if req.body.len() > MAX_BODY {
            return Err(Refusal::invalid(format!(
                "a body is at most {MAX_BODY} bytes"
            )));
        }
        super::valid_key(&req.repo).map_err(|_| Refusal::invalid("invalid repo key"))?;
        let mut recs = self.records.lock().await;
        if req.start {
            return self.start(&mut recs, req).await;
        }
        let Some(rec) = recs
            .get_mut(&req.repo)
            .filter(|r| r.status != Status::Stopped)
        else {
            return Err(Refusal::refused("no box CLI session runs for this repo"));
        };
        if !self.alive(&rec.tmux()).await {
            return Err(Refusal::refused(
                "the box CLI's tmux session is gone; run offload restart",
            ));
        }
        let mut body = req.body;
        if req.back {
            rec.queue.retain(|q| !q.starts_with(handover::BACK));
            let back = handover::back(&rec.repo, &self.layout.state);
            body = if body.trim().is_empty() {
                back
            } else {
                format!("{back}\n\n{body}")
            };
        }
        let seq = self
            .note_inbox(rec, if req.back { "back" } else { "message" }, &body)
            .await?;
        let before = rec.status;
        let delivery = if req.interrupt {
            self.tmux(&["send-keys", "-t", &pane(rec), "Escape"], None)
                .await?;
            tokio::time::sleep(PASTE_SETTLE).await;
            self.deliver(rec, &body).await?;
            "pasted"
        } else if rec.status.deliverable() && rec.queue.is_empty() {
            self.deliver(rec, &body).await?;
            "pasted"
        } else {
            rec.queue.push(body);
            if rec.status.deliverable() {
                self.deliver_later(rec.repo.clone());
            }
            "queued"
        };
        self.save(rec).await?;
        if rec.status != before {
            self.publish(rec, None);
        }
        Ok(json!({"ok": true, "seq": seq, "delivery": delivery}))
    }

    async fn start(
        &self,
        recs: &mut HashMap<String, Record>,
        req: SendRequest,
    ) -> Result<Value, Refusal> {
        if !matches!(req.cli.as_str(), "claude" | "codex") {
            return Err(Refusal::refused(format!(
                "the box CLI {:?} is not supported yet",
                req.cli
            )));
        }
        if recs
            .get(&req.repo)
            .is_some_and(|r| r.status != Status::Stopped)
        {
            return Err(Refusal::refused("a box CLI session runs for this repo"));
        }
        let running = recs
            .values()
            .filter(|r| r.status != Status::Stopped)
            .count();
        if running >= MAX_SESSIONS {
            return Err(Refusal::refused(format!(
                "{running} box CLI sessions run in this box; at most {MAX_SESSIONS}"
            )));
        }
        let repo = req.repo.clone();
        let offload_id = req.offload_id.clone();
        let copy: Copy = with_index(&self.layout, move |index| {
            let copy = index
                .get_mut(&repo)
                .filter(|c| c.offload_id == offload_id && !offload_id.is_empty())
                .ok_or_else(|| {
                    super::Fail::refused("the copy is not held by this offload; run begin")
                })?;
            copy.unreturned = true;
            Ok(copy.clone())
        })
        .await
        .map_err(|e| match e {
            super::Fail::Refused(m, _) => Refusal::refused(m),
            other => Refusal::from(other),
        })?;
        let mut rec = recs.remove(&req.repo).unwrap_or_default();
        rec.repo = req.repo;
        rec.name = copy.name;
        rec.cli = req.cli;
        rec.offload_id = req.offload_id;
        rec.dir = copy.dir;
        let started = self.first_prompt(&mut rec, &req.body).await;
        let saved = self.save(&rec).await;
        self.publish(&rec, None);
        let tmux = rec.tmux();
        recs.insert(rec.repo.clone(), rec);
        let seq = started?;
        saved?;
        Ok(json!({"ok": true, "seq": seq, "delivery": "started", "tmux": tmux}))
    }

    async fn first_prompt(&self, rec: &mut Record, body: &str) -> Result<u64, String> {
        let seq = self.note_inbox(rec, "start", body).await?;
        let prompt = self.layout.repo_dir(&rec.repo).join(PROMPT_FILE);
        tokio::fs::write(&prompt, body)
            .await
            .map_err(|e| format!("write {}: {e}", prompt.display()))?;
        self.launch(rec, &prompt).await?;
        Ok(seq)
    }

    /// Starts the CLI in a fresh tmux session with `prompt` as its first prompt; the
    /// environment step selects the versions first and installs in the background.
    async fn launch(&self, rec: &mut Record, prompt: &Path) -> Result<(), String> {
        let session = rec.tmux();
        if self.alive(&session).await {
            self.tmux(&["kill-session", "-t", &format!("={session}")], None)
                .await?;
        }
        let mut envs = vec![("COCOONBOX_OFFLOAD_REPO".to_owned(), rec.repo.clone())];
        let spec = Spec::load(&self.layout, &rec.repo).await?;
        let report = match &spec {
            Some(s) => Some(env::plan(&self.layout, &rec.repo, &rec.dir, s).await),
            None => None,
        };
        envs.extend(report.iter().flat_map(env::Report::pins));
        envs.extend(secret_env(&self.layout, &rec.repo).await);
        let hooks = self.layout.repo_dir(&rec.repo).join(HOOKS_FILE);
        write_hooks(&hooks, &rec.repo).await?;
        let command = cli_command(rec, prompt, &hooks)?;
        let dir = rec.dir.to_string_lossy().into_owned();
        let mut args = vec![
            "new-session".to_owned(),
            "-d".to_owned(),
            "-s".to_owned(),
            session.clone(),
            "-x".to_owned(),
            "200".to_owned(),
            "-y".to_owned(),
            "50".to_owned(),
            "-c".to_owned(),
            dir,
        ];
        for (k, v) in envs {
            args.push("-e".to_owned());
            args.push(format!("{k}={v}"));
        }
        let args: Vec<&str> = args.iter().map(String::as_str).collect();
        self.tmux(&args, None).await?;
        self.tmux(
            &[
                "send-keys",
                "-t",
                &format!("={session}:"),
                &command,
                "Enter",
            ],
            None,
        )
        .await?;
        rec.status = Status::Working;
        rec.reported = false;
        rec.queue.clear();
        rec.started_at_ms = crate::now_ms();
        rec.prompt_at_ms = rec.started_at_ms;
        if let (Some(spec), Some(report)) = (spec, report) {
            let layout = self.layout.clone();
            let repo = rec.repo.clone();
            let dir = rec.dir.clone();
            let steps = self.steps.clone();
            steps.send_modify(|s| {
                s.insert(repo.clone());
            });
            // detached: the installs and fetches report through env-state.json
            tokio::spawn(async move {
                env::run(&layout, &repo, &dir, &spec, report).await;
                steps.send_modify(|s| {
                    s.remove(&repo);
                });
            });
        }
        Ok(())
    }

    /// Restarts the repo's session from this offload's newest progress hand-over, else its first prompt.
    async fn relaunch(&self, rec: &mut Record) -> Result<(), String> {
        let dir = self.layout.repo_dir(&rec.repo);
        let progress = read_entries(&dir.join(OUTBOX_FILE), 0)
            .await?
            .into_iter()
            .rev()
            .find(|e| e.kind == EntryKind::Progress && e.offload_id == rec.offload_id);
        let rest = match progress {
            Some(e) => format!(
                "{}{}",
                handover::preamble(
                    &rec.dir,
                    &rec.repo,
                    &self.layout.state,
                    &format!("refs/cocoonbox/offload/{}", rec.offload_id)
                ),
                e.body
            ),
            None => tokio::fs::read_to_string(dir.join(PROMPT_FILE))
                .await
                .map_err(|e| format!("read the first prompt: {e}"))?,
        };
        let prompt = format!("{}\n\n{rest}", handover::RESTARTED);
        self.note_inbox(rec, "restart", &prompt).await?;
        let path = dir.join(RESTART_FILE);
        tokio::fs::write(&path, &prompt)
            .await
            .map_err(|e| format!("write {}: {e}", path.display()))?;
        self.launch(rec, &path).await
    }

    async fn restart(&self, repo: &str) -> Result<Value, Refusal> {
        let index = read_index(&self.layout).await?;
        let mut recs = self.records.lock().await;
        let Some(rec) = recs.get_mut(repo) else {
            return Err(Refusal::refused("no box CLI session for this repo"));
        };
        if !index.get(repo).is_some_and(|c| c.unreturned) {
            return Err(Refusal::refused(
                "the copy was returned; offload again instead",
            ));
        }
        if self.alive(&rec.tmux()).await {
            return Err(Refusal::refused(format!(
                "{} still runs; end its CLI first",
                rec.tmux()
            )));
        }
        let relaunched = self.relaunch(rec).await;
        self.save(rec).await?;
        self.publish(rec, None);
        relaunched?;
        Ok(json!({"ok": true, "delivery": "started", "tmux": rec.tmux()}))
    }

    /// Restarts every session that is not stopped, whose copy is unreturned and whose tmux session is gone.
    async fn restart_lost(&self) {
        let index = match read_index(&self.layout).await {
            Ok(i) => i,
            Err(e) => {
                eprintln!("offload inbox: {e}");
                return;
            }
        };
        let mut recs = self.records.lock().await;
        for rec in recs.values_mut() {
            let lost = rec.status != Status::Stopped
                && index.get(&rec.repo).is_some_and(|c| c.unreturned)
                && !self.alive(&rec.tmux()).await;
            if !lost {
                continue;
            }
            eprintln!(
                "offload inbox: restarting {} from its hand-over",
                rec.tmux()
            );
            if let Err(e) = self.relaunch(rec).await {
                eprintln!("offload inbox: restart {}: {e}", rec.tmux());
            }
            if let Err(e) = self.save(rec).await {
                eprintln!("offload inbox: {e}");
            }
            self.publish(rec, None);
        }
    }

    async fn post(&self, repo: &str, kind: EntryKind, body: String) -> Result<Value, Refusal> {
        if kind == EntryKind::Blocked {
            return Err(Refusal::invalid("blocked entries come from the inbox only"));
        }
        if body.len() > MAX_BODY {
            return Err(Refusal::invalid(format!(
                "a body is at most {MAX_BODY} bytes"
            )));
        }
        let mut recs = self.records.lock().await;
        let Some(rec) = recs.get_mut(repo) else {
            return Err(Refusal::refused("no box CLI session for this repo"));
        };
        let seq = self.note_outbox(rec, kind, body).await?;
        if matches!(kind, EntryKind::Progress | EntryKind::Back) {
            rec.reported = true;
        }
        self.save(rec).await?;
        Ok(json!({"ok": true, "seq": seq}))
    }

    async fn read(&self, repo: &str, since: u64, wait_ms: u64) -> Result<Value, Refusal> {
        let mut changes = self.changes.subscribe();
        let deadline =
            tokio::time::Instant::now() + Duration::from_millis(wait_ms.min(MAX_WAIT_MS));
        let path = self.layout.repo_dir(repo).join(OUTBOX_FILE);
        loop {
            let (entries, status) = {
                let recs = self.records.lock().await;
                let status = recs.get(repo).map_or(Status::Stopped, |r| r.status);
                (read_entries(&path, since).await?, status)
            };
            if !entries.is_empty() || tokio::time::Instant::now() >= deadline {
                return Ok(json!({"ok": true, "entries": entries, "status": status}));
            }
            loop {
                match tokio::time::timeout_at(deadline, changes.recv()).await {
                    Ok(Ok(c)) if c.repo != repo || c.entry.is_none() => {}
                    Ok(Err(broadcast::error::RecvError::Closed)) | Err(_) => break,
                    Ok(_) => break,
                }
            }
        }
    }

    async fn env(&self, repo: &str, wait_ms: u64) -> Result<Value, Refusal> {
        let mut steps = self.steps.subscribe();
        let deadline =
            tokio::time::Instant::now() + Duration::from_millis(wait_ms).min(env::WAIT_LIMIT);
        loop {
            let running = steps.borrow_and_update().contains(repo);
            let report = env::load(&self.layout, repo).await?;
            match &report {
                Some(r) if !r.done && !running => {
                    return Err(Refusal::from(
                        "the environment step stopped before it ended (the inbox restarted)"
                            .to_owned(),
                    ));
                }
                Some(r) if !r.done && tokio::time::Instant::now() < deadline => {}
                _ => return Ok(json!({"ok": true, "report": report})),
            }
            let _ = tokio::time::timeout_at(deadline, steps.changed()).await;
        }
    }

    async fn status(&self, repo: Option<&str>) -> Result<Value, Refusal> {
        let index = read_index(&self.layout).await?;
        let recs = self.records.lock().await;
        let mut sessions = Vec::new();
        let mut keys: Vec<&String> = recs
            .keys()
            .filter(|k| repo.is_none_or(|r| r == *k))
            .collect();
        keys.sort();
        for key in keys {
            let rec = &recs[key];
            let session = rec.tmux();
            let alive = self.alive(&session).await;
            let activity = if alive {
                self.tmux(
                    &[
                        "display-message",
                        "-p",
                        "-t",
                        &pane(rec),
                        "#{window_activity}",
                    ],
                    None,
                )
                .await
                .ok()
                .and_then(|s| s.trim().parse::<i64>().ok())
                .map_or(0, |s| s * 1000)
            } else {
                0
            };
            sessions.push(json!({
                "repo": rec.repo,
                "name": rec.name,
                "cli": rec.cli,
                "offload_id": rec.offload_id,
                "status": rec.status,
                "copy": super::copy_state(index.get(key), rec.status != Status::Stopped),
                "tmux_alive": alive,
                "hook_at_ms": rec.hook_at_ms,
                "hooks_silent": rec.hooks_silent(),
                "transcript_mtime_ms": newest_transcript(&self.layout.home, rec).await.map_or(0, |t| t.1),
                "window_activity_ms": activity,
                "inbox_seq": rec.inbox_seq,
                "outbox_seq": rec.outbox_seq,
            }));
        }
        Ok(json!({"ok": true, "sessions": sessions}))
    }

    async fn event(
        self: &Arc<Self>,
        repo: &str,
        cli: &str,
        hook: &str,
        payload: &Value,
    ) -> Result<Value, Refusal> {
        if !HOOKS.contains(&hook) && hook != "notify" {
            return Err(Refusal::invalid(format!("unknown hook {hook:?}")));
        }
        let mut recs = self.records.lock().await;
        let Some(rec) = recs.get_mut(repo).filter(|r| r.cli == cli) else {
            return Ok(json!({"ok": true}));
        };
        rec.hook_at_ms = crate::now_ms();
        self.settle(rec, hook, payload).await?;
        Ok(json!({"ok": true, "status": rec.status}))
    }

    /// Applies a hook, or the transcript's end of turn, to the session's status.
    async fn settle(
        self: &Arc<Self>,
        rec: &mut Record,
        hook: &str,
        payload: &Value,
    ) -> Result<(), String> {
        let before = rec.status;
        let blocked = if matches!(hook, "Stop" | "StopFailure" | "notify") && !rec.reported {
            blocking_message(payload).await
        } else {
            None
        };
        rec.status = match hook {
            "UserPromptSubmit" => Status::Working,
            "Notification" if before == Status::Blocked => Status::Blocked,
            "Notification" => Status::WaitingForUser,
            "SessionEnd" if payload["reason"] == "clear" => before,
            "SessionEnd" => Status::Stopped,
            _ if blocked.is_some() => Status::Blocked,
            _ => Status::Idle,
        };
        if let Some(text) = blocked {
            self.note_outbox(rec, EntryKind::Blocked, cut(&text, BLOCKED_BODY))
                .await?;
        }
        if rec.status == Status::Stopped {
            rec.queue.clear();
        }
        self.save(rec).await?;
        if rec.status != before {
            self.publish(rec, None);
        }
        if rec.status.deliverable() && !rec.queue.is_empty() {
            self.deliver_later(rec.repo.clone());
        }
        Ok(())
    }

    /// Applies a transcript's end of turn after the last prompt as `Stop` to each claude session whose hooks never ran.
    async fn idle_without_hooks(self: &Arc<Self>) {
        let mut recs = self.records.lock().await;
        for rec in recs.values_mut() {
            if rec.cli != "claude"
                || rec.status != Status::Working
                || rec.hook_at_ms >= rec.started_at_ms
            {
                continue;
            }
            let Some((path, mtime)) = newest_transcript(&self.layout.home, rec).await else {
                continue;
            };
            if mtime <= rec.prompt_at_ms || !turn_ended(&path).await {
                continue;
            }
            let payload = json!({"transcript_path": path});
            if let Err(e) = self.settle(rec, "Stop", &payload).await {
                eprintln!("offload inbox: {}: {e}", rec.tmux());
            }
        }
    }

    /// Pastes the queue's head once the hook that made the CLI idle has returned.
    fn deliver_later(self: &Arc<Self>, repo: String) {
        let inbox = Arc::clone(self);
        // detached: one queued message, checked again under the lock
        tokio::spawn(async move {
            tokio::time::sleep(DELIVERY_DELAY).await;
            let mut recs = inbox.records.lock().await;
            let Some(rec) = recs.get_mut(&repo) else {
                return;
            };
            if !rec.status.deliverable() || rec.queue.is_empty() {
                return;
            }
            let body = rec.queue.remove(0);
            if let Err(e) = inbox.deliver(rec, &body).await {
                eprintln!("offload inbox: deliver to {}: {e}", rec.tmux());
                rec.queue.insert(0, body);
            }
            if let Err(e) = inbox.save(rec).await {
                eprintln!("offload inbox: {e}");
            }
            inbox.publish(rec, None);
        });
    }

    /// Pastes `body` into the CLI and presses Enter; the session is then working on it.
    async fn deliver(&self, rec: &mut Record, body: &str) -> Result<(), String> {
        let buffer = rec.tmux();
        let target = pane(rec);
        self.tmux(
            &["load-buffer", "-b", &buffer, "-"],
            Some(body.trim_end().as_bytes()),
        )
        .await?;
        self.tmux(
            &["paste-buffer", "-d", "-p", "-b", &buffer, "-t", &target],
            None,
        )
        .await?;
        tokio::time::sleep(PASTE_SETTLE).await;
        self.tmux(&["send-keys", "-t", &target, "Enter"], None)
            .await?;
        rec.status = Status::Working;
        rec.reported = false;
        rec.prompt_at_ms = crate::now_ms();
        Ok(())
    }

    async fn watch(&self, wr: &mut OwnedWriteHalf) {
        let mut changes = self.changes.subscribe();
        loop {
            match changes.recv().await {
                Ok(c) => {
                    let Ok(line) = serde_json::to_value(&c) else {
                        continue;
                    };
                    if reply(wr, &line).await.is_err() {
                        return;
                    }
                }
                Err(broadcast::error::RecvError::Lagged(_)) => {}
                Err(broadcast::error::RecvError::Closed) => return,
            }
        }
    }

    async fn note_inbox(&self, rec: &mut Record, kind: &str, body: &str) -> Result<u64, String> {
        rec.inbox_seq += 1;
        let line = json!({
            "seq": rec.inbox_seq,
            "kind": kind,
            "offload_id": rec.offload_id,
            "time_ms": crate::now_ms(),
            "body": body,
        });
        append(&self.layout.repo_dir(&rec.repo).join(INBOX_FILE), &line).await?;
        Ok(rec.inbox_seq)
    }

    async fn note_outbox(
        &self,
        rec: &mut Record,
        kind: EntryKind,
        body: String,
    ) -> Result<u64, String> {
        rec.outbox_seq += 1;
        let entry = Entry {
            seq: rec.outbox_seq,
            kind,
            offload_id: rec.offload_id.clone(),
            cli: rec.cli.clone(),
            time_ms: crate::now_ms(),
            body,
        };
        let line = serde_json::to_value(&entry).map_err(|e| format!("encode entry: {e}"))?;
        append(&self.layout.repo_dir(&rec.repo).join(OUTBOX_FILE), &line).await?;
        self.publish(rec, Some((kind, entry.seq)));
        Ok(entry.seq)
    }

    fn publish(&self, rec: &Record, entry: Option<(EntryKind, u64)>) {
        let _ = self.changes.send(Change {
            repo: rec.repo.clone(),
            name: rec.name.clone(),
            cli: rec.cli.clone(),
            offload_id: rec.offload_id.clone(),
            entry: entry.map(|e| e.0),
            seq: entry.map_or(0, |e| e.1),
            status: rec.status,
        });
    }

    async fn save(&self, rec: &Record) -> Result<(), String> {
        let dir = self.layout.repo_dir(&rec.repo);
        tokio::fs::create_dir_all(&dir)
            .await
            .map_err(|e| format!("create {}: {e}", dir.display()))?;
        let raw = serde_json::to_vec(rec).map_err(|e| format!("encode record: {e}"))?;
        write_atomic(&dir.join(RECORD_FILE), &raw).await
    }

    async fn alive(&self, session: &str) -> bool {
        self.tmux(&["has-session", "-t", &format!("={session}")], None)
            .await
            .is_ok()
    }

    async fn tmux(&self, args: &[&str], stdin: Option<&[u8]>) -> Result<String, String> {
        let mut child = tokio::process::Command::new(&self.layout.tmux)
            .args(args)
            .stdin(if stdin.is_some() {
                Stdio::piped()
            } else {
                Stdio::null()
            })
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true)
            .spawn()
            .map_err(|e| format!("spawn tmux: {e}"))?;
        if let (Some(data), Some(mut w)) = (stdin, child.stdin.take()) {
            w.write_all(data)
                .await
                .map_err(|e| format!("write to tmux: {e}"))?;
        }
        let out = child
            .wait_with_output()
            .await
            .map_err(|e| format!("wait for tmux: {e}"))?;
        if out.status.success() {
            Ok(String::from_utf8_lossy(&out.stdout).into_owned())
        } else {
            Err(format!(
                "tmux {}: {}",
                args.first().copied().unwrap_or_default(),
                String::from_utf8_lossy(&out.stderr).trim()
            ))
        }
    }
}

/// A repo's box CLI session, persisted as `session.json` by the inbox alone.
#[derive(Clone, Debug, Default, Deserialize, Serialize)]
struct Record {
    repo: String,
    name: String,
    cli: String,
    offload_id: String,
    dir: PathBuf,
    status: Status,
    inbox_seq: u64,
    outbox_seq: u64,
    /// A `progress` or `back` entry arrived since the last delivered prompt.
    reported: bool,
    /// Messages waiting for the CLI's next idle.
    queue: Vec<String>,
    hook_at_ms: i64,
    started_at_ms: i64,
    /// The last prompt the inbox gave the CLI.
    #[serde(default)]
    prompt_at_ms: i64,
}

impl Record {
    fn tmux(&self) -> String {
        format!("cb-off-{}-{}", self.repo, self.cli)
    }

    /// No hook of the claude session arrived within `ACCEPT_WAIT` of its start.
    fn hooks_silent(&self) -> bool {
        self.cli == "claude"
            && self.status != Status::Stopped
            && self.hook_at_ms < self.started_at_ms
            && crate::now_ms() - self.started_at_ms > ACCEPT_WAIT.as_millis() as i64
    }
}

#[derive(Deserialize)]
#[serde(tag = "op", rename_all = "snake_case")]
enum Request {
    Send(SendRequest),
    Post {
        repo: String,
        kind: EntryKind,
        body: String,
    },
    Read {
        repo: String,
        #[serde(default)]
        since: u64,
        #[serde(default)]
        wait_ms: u64,
    },
    Env {
        repo: String,
        #[serde(default)]
        wait_ms: u64,
    },
    Status {
        #[serde(default)]
        repo: Option<String>,
    },
    Restart {
        repo: String,
    },
    Event {
        repo: String,
        cli: String,
        hook: String,
        #[serde(default)]
        payload: Value,
    },
    Rescan,
    Watch,
}

#[derive(Deserialize)]
struct SendRequest {
    repo: String,
    #[serde(default)]
    cli: String,
    #[serde(default)]
    offload_id: String,
    body: String,
    #[serde(default)]
    start: bool,
    #[serde(default)]
    back: bool,
    #[serde(default)]
    interrupt: bool,
}

/// An error response: `invalid`, `refused` or `failed`.
struct Refusal(&'static str, String);

impl Refusal {
    fn invalid(e: impl Into<String>) -> Self {
        Self("invalid", e.into())
    }

    fn refused(e: impl Into<String>) -> Self {
        Self("refused", e.into())
    }

    fn json(&self) -> Value {
        json!({"ok": false, "code": self.0, "error": self.1})
    }
}

impl From<String> for Refusal {
    fn from(e: String) -> Self {
        Self("failed", e)
    }
}

impl From<super::Fail> for Refusal {
    fn from(e: super::Fail) -> Self {
        Self(
            "failed",
            e.output().1["error"]
                .as_str()
                .unwrap_or_default()
                .to_owned(),
        )
    }
}

/// Binds the inbox socket (`user:cbagent` 0660) and serves until SIGTERM or SIGINT.
pub async fn serve(layout: Layout) -> Result<(), String> {
    let listener = crate::toolhost::bind(&layout.socket, SOCKET_GROUP)?;
    eprintln!("offload inbox listening on {}", layout.socket.display());
    let inbox = Inbox::open(layout).await?;
    let stop = CancellationToken::new();
    let signal = stop.clone();
    // detached: turns the first SIGTERM or SIGINT into a stop
    tokio::spawn(async move {
        crate::server::shutdown_signal().await;
        signal.cancel();
    });
    inbox.run(listener, stop).await;
    Ok(())
}

/// Follows the inbox's change feed and publishes each change as an `offload` event until `stop`;
/// reconnects while the inbox is down.
pub async fn relay(socket: PathBuf, journal: Arc<Journal>, stop: CancellationToken) {
    loop {
        let fed = tokio::select! {
            r = follow(&socket, &journal) => r,
            () = stop.cancelled() => return,
        };
        if let Err(e) = fed
            && socket.exists()
        {
            eprintln!("offload relay: {e}");
        }
        tokio::select! {
            () = tokio::time::sleep(RELAY_RETRY) => {}
            () = stop.cancelled() => return,
        }
    }
}

async fn follow(socket: &Path, journal: &Journal) -> Result<(), String> {
    let stream = UnixStream::connect(socket)
        .await
        .map_err(|e| format!("connect {}: {e}", socket.display()))?;
    let (rd, mut wr) = stream.into_split();
    wr.write_all(b"{\"op\":\"watch\"}\n")
        .await
        .map_err(|e| format!("watch: {e}"))?;
    let mut lines = BufReader::new(rd).lines();
    while let Some(line) = lines.next_line().await.map_err(|e| format!("read: {e}"))? {
        match serde_json::from_str::<Change>(&line) {
            Ok(c) => {
                journal.bus.publish(event::Body::Offload(c.update()));
            }
            Err(e) => eprintln!("offload relay: bad change: {e}"),
        }
    }
    Err("the inbox closed the feed".to_owned())
}

/// Tells the inbox that a restore replaced its state directory; the restart check it runs is not awaited.
pub async fn rescan(socket: &Path) -> Result<(), String> {
    let mut stream = UnixStream::connect(socket)
        .await
        .map_err(|e| format!("connect {}: {e}", socket.display()))?;
    stream
        .write_all(b"{\"op\":\"rescan\"}\n")
        .await
        .map_err(|e| format!("rescan: {e}"))
}

/// The first prompt's file and hooks file go on the command line; the CLI replaces the login shell.
fn cli_command(rec: &Record, prompt: &Path, hooks: &Path) -> Result<String, String> {
    let prompt = quote(&prompt.to_string_lossy());
    match rec.cli.as_str() {
        "claude" => Ok(format!(
            "exec claude --dangerously-skip-permissions --settings {} \"$(cat {prompt})\"",
            quote(&hooks.to_string_lossy())
        )),
        "codex" => Ok(format!(
            "exec codex --dangerously-bypass-approvals-and-sandbox -c 'notify=[\"cocoonbox-agent\",\"offload\",\"event\",\"--repo\",\"{}\",\"--cli\",\"codex\",\"--hook\",\"notify\"]' \"$(cat {prompt})\"",
            rec.repo
        )),
        other => Err(format!("no command line for the box CLI {other:?}")),
    }
}

/// Writes the Claude Code settings file for `--settings`: hooks that report to `offload event`,
/// and no bypass-mode dialog, which would hold the first prompt.
async fn write_hooks(path: &Path, repo: &str) -> Result<(), String> {
    let mut hooks = serde_json::Map::new();
    for hook in HOOKS {
        let command = format!(
            "cocoonbox-agent offload event --repo {repo} --cli claude --hook {hook} >/dev/null"
        );
        hooks.insert(
            hook.to_owned(),
            json!([{"hooks": [{"type": "command", "command": command}]}]),
        );
    }
    let settings = json!({"skipDangerousModePermissionPrompt": true, "hooks": hooks});
    let raw = serde_json::to_vec_pretty(&settings).map_err(|e| format!("encode hooks: {e}"))?;
    write_atomic(path, &raw).await
}

/// The secrets' variables, `KEY=VALUE` lines the packer wrote next to them on the tmpfs.
async fn secret_env(layout: &Layout, repo: &str) -> Vec<(String, String)> {
    let Ok(text) = tokio::fs::read_to_string(layout.secrets_dir(repo).join("env")).await else {
        return Vec::new();
    };
    text.lines()
        .filter_map(|l| l.split_once('='))
        .map(|(k, v)| (k.to_owned(), v.to_owned()))
        .collect()
}

/// The text of the turn's last message when it is an API error or a usage limit.
async fn blocking_message(payload: &Value) -> Option<String> {
    let given = ["last_assistant_message", "last-assistant-message"]
        .iter()
        .find_map(|k| payload[*k].as_str().map(str::to_owned));
    let text = match (given, payload["transcript_path"].as_str()) {
        (Some(t), _) => t,
        (None, Some(path)) => last_assistant_text(Path::new(path)).await?,
        (None, None) => return None,
    };
    let t = text.trim_start();
    BLOCKING_STARTS
        .iter()
        .any(|s| t.starts_with(s))
        .then(|| t.to_owned())
}

/// The text of the last assistant entry of a Claude Code transcript that has any.
async fn last_assistant_text(path: &Path) -> Option<String> {
    transcript_tail(path).await?.lines().rev().find_map(|line| {
        let v: Value = serde_json::from_str(line).ok()?;
        if v["type"] != "assistant" {
            return None;
        }
        let text: Vec<&str> = v["message"]["content"]
            .as_array()?
            .iter()
            .filter(|b| b["type"] == "text")
            .filter_map(|b| b["text"].as_str())
            .collect();
        (!text.is_empty()).then(|| text.join("\n"))
    })
}

/// Whether a Claude Code transcript's last user, assistant or `turn_duration` entry is a `turn_duration`.
async fn turn_ended(path: &Path) -> bool {
    let Some(tail) = transcript_tail(path).await else {
        return false;
    };
    tail.lines()
        .rev()
        .filter_map(|l| serde_json::from_str::<Value>(l).ok())
        .find_map(|v| match v["type"].as_str() {
            Some("user" | "assistant") => Some(false),
            Some("system") if v["subtype"] == "turn_duration" => Some(true),
            _ => None,
        })
        .unwrap_or(false)
}

/// The last `TRANSCRIPT_TAIL` bytes of a transcript.
async fn transcript_tail(path: &Path) -> Option<String> {
    let mut f = tokio::fs::File::open(path).await.ok()?;
    let len = f.metadata().await.ok()?.len();
    f.seek(std::io::SeekFrom::Start(
        len.saturating_sub(TRANSCRIPT_TAIL),
    ))
    .await
    .ok()?;
    let mut raw = Vec::new();
    f.take(TRANSCRIPT_TAIL).read_to_end(&mut raw).await.ok()?;
    Some(String::from_utf8_lossy(&raw).into_owned())
}

/// The CLI's newest transcript for the session's directory and its mtime in unix ms.
async fn newest_transcript(home: &Path, rec: &Record) -> Option<(PathBuf, i64)> {
    let dir = match rec.cli.as_str() {
        "claude" => {
            let name: String = rec
                .dir
                .to_string_lossy()
                .chars()
                .map(|c| if c.is_ascii_alphanumeric() { c } else { '-' })
                .collect();
            home.join(".claude/projects").join(name)
        }
        "codex" => {
            let mut dir = home.join(".codex/sessions");
            for _ in 0..3 {
                match newest_child(&dir).await {
                    Some(d) => dir = d,
                    None => return None,
                }
            }
            dir
        }
        _ => return None,
    };
    let mut rd = tokio::fs::read_dir(&dir).await.ok()?;
    let mut newest = None;
    while let Ok(Some(e)) = rd.next_entry().await {
        let path = e.path();
        if !path.extension().is_some_and(|x| x == "jsonl") {
            continue;
        }
        if rec.cli == "codex" && codex_cwd(&path).await.as_deref() != Some(rec.dir.as_path()) {
            continue;
        }
        if let Ok(m) = e.metadata().await {
            let mtime = m.mtime() * 1000 + m.mtime_nsec() / 1_000_000;
            if newest.as_ref().is_none_or(|(_, n)| mtime > *n) {
                newest = Some((path, mtime));
            }
        }
    }
    newest
}

/// The working directory a Codex rollout's first line (`session_meta`) names.
async fn codex_cwd(path: &Path) -> Option<PathBuf> {
    let f = tokio::fs::File::open(path).await.ok()?;
    let mut line = String::new();
    BufReader::new(f)
        .take(TRANSCRIPT_TAIL)
        .read_line(&mut line)
        .await
        .ok()?;
    let v: Value = serde_json::from_str(&line).ok()?;
    v["payload"]["cwd"].as_str().map(PathBuf::from)
}

async fn newest_child(dir: &Path) -> Option<PathBuf> {
    let mut rd = tokio::fs::read_dir(dir).await.ok()?;
    let mut best: Option<std::ffi::OsString> = None;
    while let Ok(Some(e)) = rd.next_entry().await {
        let name = e.file_name();
        if best.as_ref().is_none_or(|b| name > *b) {
            best = Some(name);
        }
    }
    best.map(|b| dir.join(b))
}

async fn load_records(state: &Path) -> HashMap<String, Record> {
    let mut out = HashMap::new();
    let Ok(mut rd) = tokio::fs::read_dir(state).await else {
        return out;
    };
    while let Ok(Some(e)) = rd.next_entry().await {
        let Ok(raw) = tokio::fs::read(e.path().join(RECORD_FILE)).await else {
            continue;
        };
        match serde_json::from_slice::<Record>(&raw) {
            Ok(r) => {
                out.insert(r.repo.clone(), r);
            }
            Err(err) => eprintln!("offload inbox: {}: {err}", e.path().display()),
        }
    }
    out
}

async fn read_entries(path: &Path, since: u64) -> Result<Vec<Entry>, String> {
    let text = match tokio::fs::read_to_string(path).await {
        Ok(t) => t,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(e) => return Err(format!("read {}: {e}", path.display())),
    };
    Ok(text
        .lines()
        .filter_map(|l| serde_json::from_str::<Entry>(l).ok())
        .filter(|e| e.seq > since)
        .collect())
}

async fn append(path: &Path, line: &Value) -> Result<(), String> {
    if let Some(parent) = path.parent() {
        tokio::fs::create_dir_all(parent)
            .await
            .map_err(|e| format!("create {}: {e}", parent.display()))?;
    }
    let mut raw = serde_json::to_vec(line).map_err(|e| format!("encode line: {e}"))?;
    raw.push(b'\n');
    let mut f = tokio::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(path)
        .await
        .map_err(|e| format!("open {}: {e}", path.display()))?;
    f.write_all(&raw)
        .await
        .map_err(|e| format!("append {}: {e}", path.display()))?;
    f.flush()
        .await
        .map_err(|e| format!("append {}: {e}", path.display()))
}

async fn reply(wr: &mut OwnedWriteHalf, v: &Value) -> std::io::Result<()> {
    let mut line = serde_json::to_vec(v).map_err(std::io::Error::other)?;
    line.push(b'\n');
    wr.write_all(&line).await
}

/// The tmux target of the session's only pane, matched exactly.
fn pane(rec: &Record) -> String {
    format!("={}:", rec.tmux())
}

fn quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', r"'\''"))
}

/// At most `max` bytes of `s`, cut at a character boundary.
fn cut(s: &str, max: usize) -> String {
    let mut end = s.len().min(max);
    while !s.is_char_boundary(end) {
        end -= 1;
    }
    s[..end].to_owned()
}

#[cfg(test)]
mod tests {
    use std::io::{Read, Write};
    use std::os::unix::fs::OpenOptionsExt;

    use super::*;

    #[tokio::test]
    async fn append_returns_only_once_its_line_is_written() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(OUTBOX_FILE);
        let made = std::process::Command::new("mkfifo")
            .arg(&path)
            .status()
            .unwrap();
        assert!(made.success());
        let mut reader = std::fs::OpenOptions::new()
            .read(true)
            .custom_flags(libc::O_NONBLOCK)
            .open(&path)
            .unwrap();
        let mut filler = std::fs::OpenOptions::new()
            .write(true)
            .custom_flags(libc::O_NONBLOCK)
            .open(&path)
            .unwrap();
        let mut filled = 0;
        while let Ok(n) = filler.write(&[b'x'; 4096]) {
            filled += n;
        }
        let line = json!({"seq": 1});
        let want = b"{\"seq\":1}\n";
        let mut appending = std::pin::pin!(append(&path, &line));
        assert!(
            tokio::time::timeout(Duration::from_millis(200), appending.as_mut())
                .await
                .is_err(),
            "append returned while its line still waited for room in the pipe"
        );
        let drain = std::thread::spawn(move || {
            let mut got = Vec::new();
            let mut buf = [0u8; 8192];
            while got.len() < filled + want.len() {
                match reader.read(&mut buf) {
                    Ok(n) => got.extend_from_slice(&buf[..n]),
                    Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                        std::thread::sleep(Duration::from_millis(5));
                    }
                    Err(e) => panic!("drain: {e}"),
                }
            }
            got
        });
        appending.await.unwrap();
        let got = drain.join().unwrap();
        drop(filler);
        assert_eq!(&got[filled..], want);
    }

    #[tokio::test]
    async fn api_errors_and_usage_limits_block_and_ordinary_text_does_not() {
        for (cli, text) in [
            (
                "claude 2.1.288",
                "API Error: 400 daily cost budget exhausted",
            ),
            ("claude 2.1.x", "Claude AI usage limit reached|1759500000"),
            ("claude 2.1.x", "5-hour limit reached ∙ resets 3pm"),
            ("codex 0.4x", "You've hit your usage limit. Upgrade to Pro"),
        ] {
            let payload = json!({"last-assistant-message": text});
            assert_eq!(
                blocking_message(&payload).await.as_deref(),
                Some(text),
                "{cli}"
            );
        }
        let payload = json!({"last_assistant_message": "Done. The API Error test passes."});
        assert_eq!(blocking_message(&payload).await, None);
    }

    #[tokio::test]
    async fn the_last_assistant_text_of_a_transcript_is_read_from_its_tail() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("t.jsonl");
        let lines = [
            json!({"type": "user", "message": {"content": "go"}}),
            json!({"type": "assistant", "message": {"content": [{"type": "text", "text": "working"}]}}),
            json!({"type": "assistant", "isApiErrorMessage": true, "message": {"content": [{"type": "text", "text": "API Error: 400 daily cost budget exhausted"}]}}),
            json!({"type": "system", "subtype": "stop_hook_summary"}),
        ];
        let text: String = lines.iter().map(|l| format!("{l}\n")).collect();
        std::fs::write(&path, text).unwrap();
        let payload = json!({"transcript_path": path});
        assert_eq!(
            blocking_message(&payload).await.as_deref(),
            Some("API Error: 400 daily cost budget exhausted")
        );
    }

    #[tokio::test]
    async fn a_turn_ends_at_turn_duration_and_not_at_a_pending_tool_use() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("t.jsonl");
        let write = |lines: &[Value]| {
            let text: String = lines.iter().map(|l| format!("{l}\n")).collect();
            std::fs::write(&path, text).unwrap();
        };
        let prompt = json!({"type": "user", "message": {"content": "go"}});
        let tool = json!({"type": "assistant", "message": {"content": [{"type": "tool_use"}]}});
        let done = json!({"type": "system", "subtype": "turn_duration", "durationMs": 1});
        let after = json!({"type": "file-history-snapshot"});
        write(&[prompt.clone(), tool.clone()]);
        assert!(!turn_ended(&path).await);
        write(&[prompt.clone(), tool.clone(), done.clone(), after]);
        assert!(turn_ended(&path).await);
        write(&[prompt.clone(), tool, done, prompt]);
        assert!(!turn_ended(&path).await);
        assert!(!turn_ended(&dir.path().join("none.jsonl")).await);
    }

    #[test]
    fn cut_keeps_character_boundaries() {
        assert_eq!(cut("aé", 2), "a");
        assert_eq!(cut("abc", 10), "abc");
    }

    #[test]
    fn command_lines_follow_the_contract() {
        let rec = Record {
            repo: "0123456789ab".into(),
            cli: "codex".into(),
            ..Record::default()
        };
        let cmd = cli_command(&rec, Path::new("/p/prompt.md"), Path::new("/p/hooks.json")).unwrap();
        assert_eq!(
            cmd,
            "exec codex --dangerously-bypass-approvals-and-sandbox -c 'notify=[\"cocoonbox-agent\",\"offload\",\"event\",\"--repo\",\"0123456789ab\",\"--cli\",\"codex\",\"--hook\",\"notify\"]' \"$(cat '/p/prompt.md')\""
        );
        assert_eq!(rec.tmux(), "cb-off-0123456789ab-codex");
    }
}
