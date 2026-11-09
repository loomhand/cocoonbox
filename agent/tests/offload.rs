#![cfg(test)]
#![cfg(unix)]

use std::io::Cursor;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use cocoonbox_agent::journal::Journal;
use cocoonbox_agent::offload::inbox::Inbox;
use cocoonbox_agent::offload::{self, Args, Fail, Index, Layout, RepoCopy, handover, packer};
use cocoonbox_agent::pb::{self, event};
use cocoonbox_agent::store::Store;
use serde_json::{Value, json};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::{UnixListener, UnixStream};
use tokio_util::sync::CancellationToken;

const REPO: &str = "0123456789ab";
const ID: &str = "a1b2c3d4e5f6";
const OTHER: &str = "ba9876543210";
const FAKE_TMUX: &str = r#"d=$(dirname "$0")
printf '%s\n' "$*" >> "$d/tmux.log"
case "$1" in
has-session) [ -e "$d/sessions/${3#=}" ] ;;
new-session) while [ $# -gt 0 ]; do [ "$1" = -s ] && { mkdir -p "$d/sessions"; touch "$d/sessions/$2"; }; shift; done ;;
kill-session) rm -f "$d/sessions/${3#=}" ;;
load-buffer) cat > "$d/buffer" ;;
display-message) case "$*" in *pane_pid*) ;; *) echo 0 ;; esac ;;
esac"#;

struct TestBox {
    dir: tempfile::TempDir,
    layout: Layout,
    fake: PathBuf,
    stop: CancellationToken,
}

impl TestBox {
    async fn start(unreturned: bool) -> Self {
        let dir = tempfile::tempdir().unwrap();
        let fake = dir.path().join("fake");
        std::fs::create_dir_all(&fake).unwrap();
        let tmux = fake.join("tmux");
        std::fs::write(&tmux, format!("#!/bin/sh\n{FAKE_TMUX}\n")).unwrap();
        std::fs::set_permissions(&tmux, PermissionsExt::from_mode(0o755)).unwrap();
        let home = dir.path().join("home");
        let workspace = dir.path().join("workspace");
        std::fs::create_dir_all(workspace.join("gron")).unwrap();
        let layout = Layout {
            state: home.join(".local/share/cocoonbox/offload"),
            socket: dir.path().join("offload.sock"),
            secrets: dir.path().join("tmpfs/secrets"),
            workspace: workspace.clone(),
            home,
            tmux,
        };
        let mut index = Index::new();
        index.insert(
            REPO.to_owned(),
            RepoCopy {
                name: "gron".into(),
                dir: workspace.join("gron"),
                offload_id: ID.into(),
                unreturned,
            },
        );
        std::fs::create_dir_all(&layout.state).unwrap();
        std::fs::write(
            layout.state.join("index.json"),
            serde_json::to_vec(&index).unwrap(),
        )
        .unwrap();
        let stop = CancellationToken::new();
        let listener = UnixListener::bind(&layout.socket).unwrap();
        let inbox = Inbox::open(layout.clone()).await.unwrap();
        tokio::spawn(inbox.run(listener, stop.clone()));
        Self {
            dir,
            layout,
            fake,
            stop,
        }
    }

    async fn call(&self, req: Value) -> Value {
        offload::request(&self.layout.socket, &req).await.unwrap()
    }

    async fn hook(&self, cli: &str, hook: &str, payload: Value) -> Value {
        self.call(json!({
            "op": "event", "repo": REPO, "cli": cli, "hook": hook, "payload": payload,
        }))
        .await
    }

    async fn start_session(&self, body: &str) -> Value {
        self.call(json!({
            "op": "send", "repo": REPO, "cli": "claude",
            "offload_id": ID, "body": body, "start": true,
        }))
        .await
    }

    fn tmux_log(&self) -> String {
        std::fs::read_to_string(self.fake.join("tmux.log")).unwrap_or_default()
    }

    async fn status(&self) -> Value {
        let mut s = self.call(json!({"op": "status", "repo": REPO})).await;
        s["sessions"][0].take()
    }
}

impl Drop for TestBox {
    fn drop(&mut self) {
        self.stop.cancel();
    }
}

fn args(v: &[&str]) -> Args {
    Args::parse(v.iter().map(|s| (*s).to_owned())).unwrap()
}

async fn until(what: &str, mut ok: impl FnMut() -> bool) {
    for _ in 0..240 {
        if ok() {
            return;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    panic!("timed out waiting for {what}");
}

fn valid_handover() -> String {
    let headings: String = handover::HEADINGS
        .iter()
        .map(|h| format!("\n{h}\nnone\n"))
        .collect();
    format!("# Hand-over: add --root\n{headings}")
}

fn tar_of(entries: &[(&str, &[u8])]) -> Cursor<Vec<u8>> {
    let mut tar = tar::Builder::new(Vec::new());
    for (name, data) in entries {
        let mut h = tar::Header::new_ustar();
        h.set_size(data.len() as u64);
        h.set_mode(0o600);
        h.set_cksum();
        tar.append_data(&mut h, name, *data).unwrap();
    }
    Cursor::new(tar.into_inner().unwrap())
}

fn sh(dir: &Path, script: &str) -> String {
    let out = std::process::Command::new("sh")
        .args(["-c", script])
        .current_dir(dir)
        .output()
        .unwrap();
    assert!(
        out.status.success(),
        "{script}: {}",
        String::from_utf8_lossy(&out.stderr)
    );
    String::from_utf8_lossy(&out.stdout).trim_end().to_owned()
}

#[tokio::test]
async fn send_to_an_absent_session_starts_it_with_the_message_as_its_first_prompt() {
    let b = TestBox::start(false).await;
    let resp = b.start_session("FIRST-PROMPT").await;
    assert_eq!(resp["ok"], true, "{resp}");
    assert_eq!(resp["delivery"], "started");
    assert_eq!(resp["tmux"], format!("cb-off-{REPO}-claude"));
    let log = b.tmux_log();
    let dir = b.layout.workspace.join("gron");
    assert!(
        log.contains(&format!(
            "new-session -d -s cb-off-{REPO}-claude -x 200 -y 50 -c {} -e COCOONBOX_OFFLOAD_REPO={REPO}",
            dir.display()
        )),
        "{log}"
    );
    let prompt = b.layout.state.join(REPO).join("prompt.md");
    let hooks = b.layout.state.join(REPO).join("hooks.json");
    assert!(
        log.contains(&format!(
            "send-keys -t =cb-off-{REPO}-claude: exec claude --dangerously-skip-permissions --settings '{}' \"$(cat '{}')\" Enter",
            hooks.display(),
            prompt.display()
        )),
        "{log}"
    );
    assert_eq!(std::fs::read_to_string(&prompt).unwrap(), "FIRST-PROMPT");
    let settings: Value = serde_json::from_slice(&std::fs::read(&hooks).unwrap()).unwrap();
    assert_eq!(settings["skipDangerousModePermissionPrompt"], true);
    assert_eq!(
        settings["hooks"]["Stop"][0]["hooks"][0]["command"],
        format!("cocoonbox-agent offload event --repo {REPO} --cli claude --hook Stop >/dev/null")
    );
    let s = b.status().await;
    assert_eq!(s["status"], "working");
    assert_eq!(s["tmux_alive"], true);
    assert_eq!(s["copy"], "busy");
    let index: Index =
        serde_json::from_slice(&std::fs::read(b.layout.state.join("index.json")).unwrap()).unwrap();
    assert!(
        index[REPO].unreturned,
        "a started session makes the copy unreturned"
    );
    let again = b.start_session("x").await;
    assert_eq!(again["code"], "refused", "{again}");
}

#[tokio::test]
async fn a_first_prompt_past_one_argument_is_refused() {
    let b = TestBox::start(false).await;
    let over = b.start_session(&"x".repeat(handover::MAX_PROMPT + 1)).await;
    assert_eq!(over["code"], "invalid", "{over}");
    assert!(!b.tmux_log().contains("new-session"));
    let at = b.start_session(&"x".repeat(handover::MAX_PROMPT)).await;
    assert_eq!(at["delivery"], "started", "{at}");
}

#[tokio::test]
async fn a_session_whose_tmux_ended_without_a_hook_is_reported_stopped() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    std::fs::remove_file(b.fake.join(format!("sessions/cb-off-{REPO}-claude"))).unwrap();
    let s = b.status().await;
    assert_eq!(s["status"], "stopped", "{s}");
    assert_eq!(s["tmux_alive"], false);
    assert_eq!(s["copy"], "unreturned");
}

#[tokio::test]
async fn a_busy_send_is_queued_until_the_idle_hook() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    let resp = b
        .call(json!({"op": "send", "repo": REPO, "body": "SECOND\n"}))
        .await;
    assert_eq!(resp["delivery"], "queued", "{resp}");
    assert!(!b.tmux_log().contains("load-buffer"));
    let ev = b
        .call(json!({"op": "event", "repo": REPO, "cli": "claude", "hook": "Stop", "payload": {}}))
        .await;
    assert_eq!(ev["status"], "idle", "{ev}");
    until("the queued message to be pasted", || {
        b.tmux_log()
            .contains(&format!("send-keys -t =cb-off-{REPO}-claude: Enter"))
    })
    .await;
    let log = b.tmux_log();
    assert!(
        log.contains(&format!(
            "paste-buffer -d -p -b cb-off-{REPO}-claude -t =cb-off-{REPO}-claude:"
        )),
        "{log}"
    );
    assert_eq!(
        std::fs::read_to_string(b.fake.join("buffer")).unwrap(),
        "SECOND"
    );
    assert_eq!(b.status().await["status"], "working");
    let idle = b
        .call(json!({"op": "event", "repo": REPO, "cli": "claude", "hook": "Stop", "payload": {}}))
        .await;
    assert_eq!(idle["status"], "idle");
    let send = b.call(json!({"op": "send", "repo": REPO, "body": "THIRD"}));
    let submit = async {
        until("THIRD to be pasted", || {
            std::fs::read_to_string(b.fake.join("buffer")).unwrap_or_default() == "THIRD"
        })
        .await;
        b.hook("claude", "UserPromptSubmit", json!({"prompt": "THIRD"}))
            .await
    };
    let (pasted, _) = tokio::join!(send, submit);
    assert_eq!(pasted["delivery"], "accepted", "{pasted}");
    assert_eq!(
        std::fs::read_to_string(b.fake.join("buffer")).unwrap(),
        "THIRD"
    );
}

#[tokio::test]
async fn codex_pastes_without_waiting_for_a_claude_hook() {
    let b = TestBox::start(false).await;
    b.call(json!({
        "op": "send", "repo": REPO, "name": "gron", "cli": "codex",
        "offload_id": ID, "body": "FIRST", "start": true,
    }))
    .await;
    b.hook("codex", "notify", json!({})).await;
    let response = tokio::time::timeout(
        Duration::from_secs(2),
        b.call(json!({"op": "send", "repo": REPO, "body": "SECOND"})),
    )
    .await
    .unwrap();
    assert_eq!(response["delivery"], "pasted", "{response}");
}

#[tokio::test]
async fn a_pasted_back_without_a_submit_hook_reports_unconfirmed_delivery() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    b.hook("claude", "Stop", json!({})).await;
    let response = b
        .call(json!({"op": "send", "repo": REPO, "body": "", "back": true}))
        .await;
    assert_eq!(response["ok"], false, "{response}");
    assert_eq!(response["seq"], 2, "{response}");
    assert_eq!(response["delivery"], "unconfirmed", "{response}");
    assert!(
        response["error"]
            .as_str()
            .unwrap()
            .contains("delivery is unconfirmed")
    );
    assert_eq!(b.status().await["status"], "blocked");
    assert_eq!(b.tmux_log().matches("load-buffer").count(), 1);
    let out = b.call(json!({"op": "read", "repo": REPO})).await;
    assert_eq!(out["entries"][0]["kind"], "blocked");
    let prompt = std::fs::read_to_string(b.fake.join("buffer")).unwrap();
    let late = b
        .hook("claude", "UserPromptSubmit", json!({"prompt": prompt}))
        .await;
    assert_eq!(late["status"], "working");
}

#[tokio::test]
async fn only_the_submitted_prompt_confirms_a_delivery() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    b.hook("claude", "Stop", json!({})).await;
    let mut send =
        std::pin::pin!(b.call(json!({"op": "send", "repo": REPO, "body": "\n\n   SECOND\n"})));
    assert!(
        tokio::time::timeout(Duration::from_secs(1), send.as_mut())
            .await
            .is_err()
    );
    b.hook("claude", "UserPromptSubmit", json!({"prompt": "FIRST"}))
        .await;
    assert!(
        tokio::time::timeout(Duration::from_millis(100), send.as_mut())
            .await
            .is_err()
    );
    let prompt = "<pasted_content id=\"1\">\n\n\n   SECOND\n</pasted_content id=\"1\">";
    b.hook("claude", "UserPromptSubmit", json!({"prompt": prompt}))
        .await;
    let response = tokio::time::timeout(Duration::from_secs(1), send)
        .await
        .unwrap();
    assert_eq!(response["delivery"], "accepted", "{response}");
    assert_eq!(b.tmux_log().matches("load-buffer").count(), 1);
}

#[tokio::test]
async fn a_restored_session_with_same_millisecond_hooks_still_requires_confirmation() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    b.hook("claude", "Stop", json!({})).await;
    let path = b.layout.repo_dir(REPO).join("session.json");
    let mut record: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
    record["hook_at_ms"] = record["started_at_ms"].clone();
    std::fs::write(&path, serde_json::to_vec(&record).unwrap()).unwrap();
    assert_eq!(b.call(json!({"op": "rescan"})).await["ok"], true);
    let response = b
        .call(json!({"op": "send", "repo": REPO, "body": "SECOND"}))
        .await;
    assert_eq!(response["ok"], false, "{response}");
    assert_eq!(b.status().await["status"], "blocked");
}

#[tokio::test]
async fn an_old_delivery_timeout_does_not_block_a_newer_confirmed_prompt() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    b.hook("claude", "Stop", json!({})).await;
    let mut old = std::pin::pin!(b.call(json!({"op": "send", "repo": REPO, "body": "OLD"})));
    assert!(
        tokio::time::timeout(Duration::from_secs(1), old.as_mut())
            .await
            .is_err()
    );
    let mut new = std::pin::pin!(
        b.call(json!({"op": "send", "repo": REPO, "body": "NEW", "interrupt": true}))
    );
    assert!(
        tokio::time::timeout(Duration::from_millis(1500), new.as_mut())
            .await
            .is_err()
    );
    b.hook("claude", "UserPromptSubmit", json!({"prompt": "NEW"}))
        .await;
    let response = tokio::time::timeout(Duration::from_secs(1), new)
        .await
        .unwrap();
    assert_eq!(response["delivery"], "accepted", "{response}");
    assert_eq!(old.await["ok"], false);
    assert_eq!(b.status().await["status"], "working");
    let out = b.call(json!({"op": "read", "repo": REPO})).await;
    assert!(out["entries"].as_array().unwrap().is_empty(), "{out}");
}

#[tokio::test]
async fn a_queued_back_without_confirmation_wakes_outbox_readers() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    let queued = b
        .call(json!({"op": "send", "repo": REPO, "body": "", "back": true}))
        .await;
    assert_eq!(queued["delivery"], "queued");
    b.call(json!({"op": "send", "repo": REPO, "body": "AFTER"}))
        .await;
    b.hook("claude", "Stop", json!({})).await;
    let out = tokio::time::timeout(
        Duration::from_secs(15),
        b.call(json!({"op": "read", "repo": REPO, "wait_ms": 60000})),
    )
    .await
    .unwrap();
    assert_eq!(out["entries"][0]["kind"], "blocked", "{out}");
    assert_eq!(out["status"], "blocked");
    assert_eq!(b.tmux_log().matches("load-buffer").count(), 1);
    let record: Value = serde_json::from_slice(
        &std::fs::read(b.layout.repo_dir(REPO).join("session.json")).unwrap(),
    )
    .unwrap();
    assert_eq!(record["queue"], json!(["AFTER"]));
}

#[tokio::test]
async fn an_unconfirmed_paste_holds_the_queue_until_the_session_takes_a_prompt() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    b.call(json!({"op": "send", "repo": REPO, "body": "", "back": true}))
        .await;
    b.call(json!({"op": "send", "repo": REPO, "body": "AFTER"}))
        .await;
    b.hook("claude", "Stop", json!({})).await;
    until("the BACK to go unconfirmed", || {
        std::fs::read_to_string(b.layout.repo_dir(REPO).join("session.json"))
            .is_ok_and(|r| r.contains("\"unconfirmed\":true"))
    })
    .await;
    let waiting = b
        .hook(
            "claude",
            "Notification",
            json!({"message": "Claude is waiting for your input"}),
        )
        .await;
    assert_eq!(waiting["status"], "blocked", "{waiting}");
    tokio::time::sleep(Duration::from_secs(2)).await;
    assert_eq!(b.tmux_log().matches("load-buffer").count(), 1);
    b.hook(
        "claude",
        "UserPromptSubmit",
        json!({"prompt": "the BACK and what was typed"}),
    )
    .await;
    b.hook("claude", "Stop", json!({})).await;
    until("AFTER to be pasted", || {
        std::fs::read_to_string(b.fake.join("buffer")).unwrap_or_default() == "AFTER"
    })
    .await;
}

#[tokio::test]
async fn a_fresh_cli_of_the_repo_starts_without_the_last_unconfirmed_paste() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    b.hook("claude", "Stop", json!({})).await;
    let unconfirmed = b
        .call(json!({"op": "send", "repo": REPO, "body": "SECOND"}))
        .await;
    assert_eq!(unconfirmed["delivery"], "unconfirmed", "{unconfirmed}");
    std::fs::remove_file(b.fake.join(format!("sessions/cb-off-{REPO}-claude"))).unwrap();
    let restarted = b.call(json!({"op": "restart", "repo": REPO})).await;
    assert_eq!(restarted["delivery"], "started", "{restarted}");
    let record: Value = serde_json::from_slice(
        &std::fs::read(b.layout.repo_dir(REPO).join("session.json")).unwrap(),
    )
    .unwrap();
    assert_eq!(record["unconfirmed"], false, "{record}");
}

#[tokio::test]
async fn a_session_whose_hooks_never_run_goes_idle_from_its_transcript_and_takes_one_back() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    for _ in 0..2 {
        let resp = b
            .call(json!({"op": "send", "repo": REPO, "body": "", "back": true}))
            .await;
        assert_eq!(resp["delivery"], "queued", "{resp}");
    }
    let transcript_of = |repo_dir: &str| {
        let name: String = b
            .layout
            .workspace
            .join(repo_dir)
            .to_string_lossy()
            .chars()
            .map(|c| if c.is_ascii_alphanumeric() { c } else { '-' })
            .collect();
        let projects = b.layout.home.join(".claude/projects").join(name);
        std::fs::create_dir_all(&projects).unwrap();
        projects.join("s.jsonl")
    };
    let transcript = transcript_of("gron");
    let mut lines = [
        json!({"type": "user", "message": {"content": "FIRST"}}),
        json!({"type": "assistant", "message": {"content": [{"type": "tool_use"}]}}),
    ]
    .map(|l| format!("{l}\n"))
    .concat();
    std::fs::write(&transcript, &lines).unwrap();
    let ended = format!(
        "{}\n{}\n",
        json!({"type": "assistant", "message": {"content": [{"type": "text", "text": "done"}]}}),
        json!({"type": "system", "subtype": "turn_duration"})
    );
    let index_path = b.layout.state.join("index.json");
    let mut index: Index = serde_json::from_slice(&std::fs::read(&index_path).unwrap()).unwrap();
    index.insert(
        OTHER.to_owned(),
        RepoCopy {
            name: "other".into(),
            dir: b.layout.workspace.join("other"),
            offload_id: ID.into(),
            unreturned: false,
        },
    );
    std::fs::write(&index_path, serde_json::to_vec(&index).unwrap()).unwrap();
    std::fs::create_dir_all(b.layout.workspace.join("other")).unwrap();
    let other = b
        .call(json!({
            "op": "send", "repo": OTHER, "cli": "claude",
            "offload_id": ID, "body": "OTHER", "start": true,
        }))
        .await;
    assert_eq!(other["delivery"], "started", "{other}");
    let other_transcript = transcript_of("other");
    let status_of = |sessions: &Value, repo: &str| {
        sessions
            .as_array()
            .into_iter()
            .flatten()
            .find(|s| s["repo"] == repo)
            .map(|s| s["status"].clone())
            .unwrap_or_default()
    };
    let mut sessions = Value::Null;
    for _ in 0..300 {
        std::fs::write(&other_transcript, &ended).unwrap();
        sessions = b.call(json!({"op": "status"})).await["sessions"].take();
        if status_of(&sessions, OTHER) == "idle" {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    assert_eq!(status_of(&sessions, OTHER), "idle", "{sessions}");
    assert_eq!(status_of(&sessions, REPO), "working", "{sessions}");
    lines.push_str(&ended);
    std::fs::write(&transcript, &lines).unwrap();
    let enter = format!("send-keys -t =cb-off-{REPO}-claude: Enter");
    until("the back prompt to be pasted", || {
        b.tmux_log().contains(&enter)
    })
    .await;
    let pasted = std::fs::read_to_string(b.fake.join("buffer")).unwrap();
    assert!(pasted.starts_with(handover::BACK), "{pasted}");
    let mut status = b.status().await["status"].take();
    for _ in 0..100 {
        if status == "working" {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
        status = b.status().await["status"].take();
    }
    assert_eq!(status, "working");
    b.call(json!({"op": "event", "repo": REPO, "cli": "claude", "hook": "Stop", "payload": {}}))
        .await;
    tokio::time::sleep(Duration::from_secs(1)).await;
    assert_eq!(b.tmux_log().matches("load-buffer").count(), 1);
}

#[tokio::test]
async fn post_emits_an_event_with_no_content() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    let watch = UnixStream::connect(&b.layout.socket).await.unwrap();
    let (rd, mut wr) = watch.into_split();
    wr.write_all(b"{\"op\":\"watch\"}\n").await.unwrap();
    let mut lines = BufReader::new(rd).lines();
    let journal = Arc::new(Journal::new(
        Store::open(&b.dir.path().join("agent.db")).unwrap(),
    ));
    let mut live = journal.bus.subscribe(0).unwrap().live;
    tokio::spawn(offload::inbox::relay(
        b.layout.socket.clone(),
        Arc::clone(&journal),
        b.stop.clone(),
    ));
    for probe in 1.. {
        let posted = b
            .call(json!({"op": "post", "repo": REPO, "kind": "progress", "body": "probe"}))
            .await;
        let seen = tokio::time::timeout(Duration::from_millis(500), async {
            let seq = posted["seq"].as_u64().unwrap();
            loop {
                let line = lines.next_line().await.unwrap().unwrap();
                if serde_json::from_str::<Value>(&line).unwrap()["seq"] == seq {
                    break;
                }
            }
            loop {
                let ev = live.recv().await.unwrap();
                if matches!(&ev.body, Some(event::Body::Offload(u)) if u.seq == seq) {
                    break;
                }
            }
        })
        .await;
        if seen.is_ok() {
            break;
        }
        assert!(probe < 20, "the watch or the relay saw no post");
    }
    let posted = b
        .call(json!({"op": "post", "repo": REPO, "kind": "progress", "body": "SECRET-BODY"}))
        .await;
    let seq = posted["seq"].as_u64().unwrap();
    let line = tokio::time::timeout(Duration::from_secs(5), lines.next_line())
        .await
        .unwrap()
        .unwrap()
        .unwrap();
    assert!(
        !line.contains("SECRET-BODY") && !line.contains("body"),
        "{line}"
    );
    let change: Value = serde_json::from_str(&line).unwrap();
    assert_eq!(
        change,
        json!({
            "repo": REPO, "name": "gron", "cli": "claude", "offload_id": ID,
            "entry": "progress", "seq": seq, "status": "working",
        })
    );
    let ev = tokio::time::timeout(Duration::from_secs(5), live.recv())
        .await
        .unwrap()
        .unwrap();
    let Some(event::Body::Offload(update)) = &ev.body else {
        panic!("not an offload event: {ev:?}");
    };
    assert_eq!(update.entry, pb::OffloadEntry::Progress as i32);
    assert_eq!(update.seq, seq);
    assert_eq!(update.status, pb::OffloadStatus::Working as i32);
    let encoded = prost::Message::encode_to_vec(ev.as_ref());
    assert!(
        !encoded
            .windows(b"SECRET-BODY".len())
            .any(|w| w == b"SECRET-BODY")
    );
}

#[tokio::test]
async fn read_wait_returns_on_post() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    let socket = b.layout.socket.clone();
    let started = tokio::time::Instant::now();
    let reader = tokio::spawn(async move {
        offload::request(
            &socket,
            &json!({"op": "read", "repo": REPO, "since": 0, "wait_ms": 20_000}),
        )
        .await
        .unwrap()
    });
    tokio::time::sleep(Duration::from_millis(300)).await;
    assert!(
        !reader.is_finished(),
        "read waits while the outbox is empty"
    );
    b.call(json!({"op": "post", "repo": REPO, "kind": "question", "body": "which flag?"}))
        .await;
    let got = reader.await.unwrap();
    assert!(
        started.elapsed() < Duration::from_secs(20),
        "read returned at its wait limit, not on the post"
    );
    assert_eq!(got["entries"][0]["kind"], "question", "{got}");
    assert_eq!(got["entries"][0]["body"], "which flag?");
    assert_eq!(got["entries"][0]["offload_id"], ID);
    let after = b
        .call(json!({"op": "read", "repo": REPO, "since": 1}))
        .await;
    assert_eq!(after["entries"], json!([]));
}

#[tokio::test]
async fn read_wait_returns_stopped_once_the_cli_tmux_is_gone() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    let socket = b.layout.socket.clone();
    let reader = tokio::spawn(async move {
        offload::request(
            &socket,
            &json!({"op": "read", "repo": REPO, "since": 1, "wait_ms": 30_000}),
        )
        .await
        .unwrap()
    });
    tokio::time::sleep(Duration::from_millis(300)).await;
    std::fs::remove_file(b.fake.join(format!("sessions/cb-off-{REPO}-claude"))).unwrap();
    let got = tokio::time::timeout(Duration::from_secs(10), reader)
        .await
        .expect("read waited past the CLI's end")
        .unwrap();
    assert_eq!(got["status"], "stopped", "{got}");
    assert_eq!(got["entries"], json!([]));
}

#[tokio::test]
async fn begin_refuses_an_unreturned_copy() {
    let b = TestBox::start(true).await;
    let before = std::fs::read(b.layout.state.join("index.json")).unwrap();
    let a = args(&[
        "--repo",
        REPO,
        "--name",
        "gron",
        "--kind",
        "claude",
        "--id",
        "ffffffffffff",
    ]);
    let Err(Fail::Refused(_, extra)) = packer::begin(&b.layout, &a).await else {
        panic!("begin did not refuse");
    };
    let copy: Vec<&Value> = extra["findings"]
        .as_array()
        .unwrap()
        .iter()
        .filter(|f| f["check"] == "copy")
        .collect();
    assert_eq!(copy.len(), 1, "{extra}");
    assert_eq!(copy[0]["outcome"], "block");
    assert_eq!(
        std::fs::read(b.layout.state.join("index.json")).unwrap(),
        before,
        "a refused begin changes nothing"
    );
}

#[tokio::test]
async fn post_refuses_a_hand_over_without_every_heading() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    let a = args(&["--repo", REPO, "--progress"]);
    let thin = valid_handover().replace("## Decisions made\n", "");
    let Err(Fail::Refused(msg, _)) = packer::post(&b.layout, &a, thin).await else {
        panic!("post took a hand-over without ## Decisions made");
    };
    assert!(msg.contains("## Decisions made"), "{msg}");
    let ok = packer::post(&b.layout, &a, valid_handover()).await.unwrap();
    assert_eq!(ok["status"], "posted");
    assert_eq!(ok["seq"], 1);
}

#[tokio::test]
async fn a_usage_limit_after_the_last_prompt_writes_a_blocked_entry() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    let ev = b
        .call(json!({
            "op": "event", "repo": REPO, "cli": "claude", "hook": "StopFailure",
            "payload": {"last_assistant_message": "API Error: 400 daily cost budget exhausted"},
        }))
        .await;
    assert_eq!(ev["status"], "blocked", "{ev}");
    let read = b.call(json!({"op": "read", "repo": REPO})).await;
    assert_eq!(read["entries"][0]["kind"], "blocked");
    assert_eq!(
        read["entries"][0]["body"],
        "API Error: 400 daily cost budget exhausted"
    );
    assert_eq!(read["status"], "blocked");
    let idle_notice = b
        .call(json!({"op": "event", "repo": REPO, "cli": "claude", "hook": "Notification", "payload": {}}))
        .await;
    assert_eq!(
        idle_notice["status"], "blocked",
        "blocked lasts until the next prompt"
    );
    let clear = b
        .call(json!({"op": "event", "repo": REPO, "cli": "claude", "hook": "SessionEnd", "payload": {"reason": "clear"}}))
        .await;
    assert_eq!(clear["status"], "blocked", "/clear keeps the CLI running");
    b.call(json!({"op": "post", "repo": REPO, "kind": "progress", "body": valid_handover()}))
        .await;
    let ev = b
        .call(json!({
            "op": "event", "repo": REPO, "cli": "claude", "hook": "Stop",
            "payload": {"last_assistant_message": "API Error: 500"},
        }))
        .await;
    assert_eq!(
        ev["status"], "idle",
        "a progress post since the prompt means it was not stuck"
    );
}

#[tokio::test]
async fn a_lost_session_restarts_from_the_newest_progress_hand_over() {
    let b = TestBox::start(false).await;
    let outbox = b.layout.state.join(REPO).join("outbox.jsonl");
    let previous = r#"{"seq":1,"kind":"progress","offload_id":"ffffffffffff","cli":"claude","time_ms":1,"body":"PREVIOUS-OFFLOAD"}
"#;
    b.start_session("FIRST").await;
    b.call(json!({"op": "post", "repo": REPO, "kind": "progress", "body": "OLD-HANDOVER"}))
        .await;
    b.call(json!({"op": "post", "repo": REPO, "kind": "progress", "body": "NEW-HANDOVER"}))
        .await;
    let live = b.call(json!({"op": "restart", "repo": REPO})).await;
    assert_eq!(
        live["code"], "refused",
        "a running session is not restarted: {live}"
    );
    std::fs::remove_file(b.fake.join(format!("sessions/cb-off-{REPO}-claude"))).unwrap();
    let resp = b.call(json!({"op": "restart", "repo": REPO})).await;
    assert_eq!(resp["delivery"], "started", "{resp}");
    let prompt = std::fs::read_to_string(b.layout.state.join(REPO).join("restart.md")).unwrap();
    assert!(prompt.starts_with(handover::RESTARTED), "{prompt}");
    assert!(prompt.contains("NEW-HANDOVER"), "{prompt}");
    assert!(!prompt.contains("OLD-HANDOVER"), "{prompt}");
    std::fs::write(&outbox, previous).unwrap();
    std::fs::remove_file(b.fake.join(format!("sessions/cb-off-{REPO}-claude"))).unwrap();
    b.call(json!({"op": "restart", "repo": REPO})).await;
    let prompt = std::fs::read_to_string(b.layout.state.join(REPO).join("restart.md")).unwrap();
    assert!(
        prompt.ends_with("FIRST") && !prompt.contains("PREVIOUS-OFFLOAD"),
        "without a progress entry of this offload the first prompt is used: {prompt}"
    );
    assert!(b.tmux_log().contains("restart.md"));
    assert_eq!(b.status().await["tmux_alive"], true);
}

#[tokio::test]
async fn a_rescan_after_a_restore_restarts_the_restored_sessions_at_once() {
    let b = TestBox::start(true).await;
    let staged = b.layout.state.with_extension("restored");
    std::fs::create_dir_all(staged.join(REPO)).unwrap();
    std::fs::copy(b.layout.state.join("index.json"), staged.join("index.json")).unwrap();
    let record = json!({
        "repo": REPO, "name": "gron", "cli": "claude", "offload_id": ID,
        "dir": b.layout.workspace.join("gron"), "status": "working",
        "inbox_seq": 1, "outbox_seq": 0, "reported": false, "queue": [],
        "hook_at_ms": 0, "started_at_ms": 1,
    });
    std::fs::write(
        staged.join(REPO).join("session.json"),
        serde_json::to_vec(&record).unwrap(),
    )
    .unwrap();
    std::fs::write(staged.join(REPO).join("prompt.md"), "FIRST").unwrap();
    std::fs::rename(&b.layout.state, b.layout.state.with_extension("old")).unwrap();
    std::fs::rename(&staged, &b.layout.state).unwrap();

    offload::inbox::rescan(&b.layout.socket).await.unwrap();
    let restart = b.layout.state.join(REPO).join("restart.md");
    until("the restart prompt", || restart.exists()).await;
    let prompt = std::fs::read_to_string(&restart).unwrap();
    assert!(
        prompt.starts_with(handover::RESTARTED) && prompt.ends_with("FIRST"),
        "{prompt}"
    );
    assert_eq!(b.status().await["tmux_alive"], true);
}

#[tokio::test]
async fn env_waits_for_the_inbox_step_and_refuses_a_step_it_does_not_run() {
    let b = TestBox::start(false).await;
    std::fs::create_dir_all(b.layout.state.join(REPO)).unwrap();
    std::fs::write(
        b.layout.state.join(REPO).join("env.json"),
        r#"{"v":1,"lockfiles":["go.sum"]}"#,
    )
    .unwrap();
    b.start_session("FIRST").await;
    let resp = b
        .call(json!({"op": "env", "repo": REPO, "wait_ms": 60_000}))
        .await;
    assert_eq!(resp["ok"], true, "{resp}");
    assert_eq!(resp["report"]["done"], true, "{resp}");
    assert_eq!(resp["report"]["fetches"][0]["lockfile"], "go.sum");
    assert_ne!(resp["report"]["fetches"][0]["outcome"], "pending");

    let state = b.layout.state.join(REPO).join("env-state.json");
    let mut report: Value = serde_json::from_slice(&std::fs::read(&state).unwrap()).unwrap();
    report["done"] = false.into();
    std::fs::write(&state, serde_json::to_vec(&report).unwrap()).unwrap();
    let started = std::time::Instant::now();
    let resp = b
        .call(json!({"op": "env", "repo": REPO, "wait_ms": 60_000}))
        .await;
    assert_eq!(resp["code"], "failed", "{resp}");
    assert!(
        started.elapsed() < Duration::from_secs(1),
        "{:?}",
        started.elapsed()
    );
}

#[tokio::test]
async fn end_stops_the_session_and_returns_the_copy() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    let secrets = b.layout.secrets.join(REPO);
    std::fs::create_dir_all(&secrets).unwrap();
    std::fs::write(secrets.join("aws"), "k").unwrap();
    std::fs::remove_file(b.fake.join(format!("sessions/cb-off-{REPO}-claude"))).unwrap();
    let out = packer::end(&b.layout, REPO).await.unwrap();
    assert_eq!(out["copy"], "mirror");
    assert!(!secrets.exists(), "end wipes the secrets");
    let s = b.status().await;
    assert_eq!(s["status"], "stopped");
    assert_eq!(s["copy"], "mirror");
}

#[tokio::test]
async fn a_new_offload_starts_with_an_empty_inbox_and_outbox_and_seqs_go_on() {
    let b = TestBox::start(false).await;
    b.start_session("FIRST").await;
    b.call(json!({"op": "post", "repo": REPO, "kind": "progress", "body": "OLD"}))
        .await;
    std::fs::remove_file(b.fake.join(format!("sessions/cb-off-{REPO}-claude"))).unwrap();
    packer::end(&b.layout, REPO).await.unwrap();
    b.start_session("SECOND").await;
    let read = b.call(json!({"op": "read", "repo": REPO})).await;
    assert_eq!(read["entries"], json!([]), "{read}");
    let inbox = std::fs::read_to_string(b.layout.state.join(REPO).join("inbox.jsonl")).unwrap();
    assert!(
        !inbox.contains("FIRST") && inbox.contains("SECOND"),
        "{inbox}"
    );
    let posted = b
        .call(json!({"op": "post", "repo": REPO, "kind": "progress", "body": "NEW"}))
        .await;
    assert_eq!(posted["seq"], 2, "{posted}");
}

#[tokio::test]
async fn send_rebuilds_history_checks_s_places_secrets_and_starts_the_cli() {
    let b = TestBox::start(false).await;
    let mac = b.dir.path().join("mac");
    std::fs::create_dir_all(&mac).unwrap();
    sh(
        &mac,
        "git init -q -b main && git config user.email t@t && git config user.name t \
        && printf 'target/\\n.env\\n' > .gitignore && echo one > a.txt && git add -A \
        && git commit -qm one && echo two >> a.txt && echo staged > c.txt && git add c.txt \
        && echo untracked > b.txt && mkdir -p target/debug && echo bin > target/debug/x \
        && echo TOKEN=mac > .env",
    );
    let s = sh(
        &mac,
        &format!(
            "cp .git/index .git/snap && export GIT_INDEX_FILE=.git/snap && git add -A \
         && s=$(git commit-tree $(git write-tree) -p HEAD -m S) && rm .git/snap \
         && git update-ref refs/cocoonbox/offload/{ID} $s && echo $s"
        ),
    );
    let copy = b.layout.workspace.join("gron");
    sh(
        &mac,
        &format!(
            "rsync -a --exclude=/.git/objects/ --exclude=/target/ --exclude=/.env ./ {c}/ \
         && mkdir -p {c}/target && echo box-built > {c}/target/y \
         && git bundle create {}/offload.bundle --all 2>/dev/null",
            b.dir.path().display(),
            c = copy.display(),
        ),
    );
    let bundle = std::fs::read(b.dir.path().join("offload.bundle")).unwrap();
    let stream = tar_of(&[
        ("handover.md", valid_handover().as_bytes()),
        ("offload.bundle", &bundle),
        ("secrets/file/.env", b"TOKEN=secret"),
        ("secrets/aws", b"[default]"),
    ]);

    let socket = b.layout.socket.clone();
    let session = b.fake.join(format!("sessions/cb-off-{REPO}-claude"));
    let hook = tokio::spawn(async move {
        until("the CLI session", || session.exists()).await;
        offload::request(
            &socket,
            &json!({"op": "event", "repo": REPO, "cli": "claude", "hook": "UserPromptSubmit", "payload": {}}),
        )
        .await
        .unwrap()
    });
    let a = args(&["--repo", REPO, "--kind", "claude", "--id", ID]);
    let out = packer::send(&b.layout, &a, stream).await.unwrap();
    hook.await.unwrap();
    assert_eq!(out["delivery"], "started", "{out}");
    assert_eq!(out["accepted"], true, "{out}");
    for step in [
        "stream", "unbundle", "refresh", "check", "secrets", "excludes", "trust", "start", "accept",
    ] {
        assert!(out["timings_ms"][step].is_u64(), "{step} in {out}");
    }
    let status = |dir: &Path| {
        sh(
            dir,
            "git status --porcelain=v2 --untracked-files=all | grep -v ' .env$'",
        )
    };
    assert_eq!(
        status(&copy),
        status(&mac),
        "the copy has the Mac's uncommitted state"
    );
    assert_eq!(
        sh(&copy, &format!("git rev-parse refs/cocoonbox/offload/{ID}")),
        s
    );
    let env = b.layout.secrets.join(REPO);
    assert_eq!(
        std::fs::read_link(copy.join(".env")).unwrap(),
        env.join("file/.env")
    );
    assert_eq!(
        std::fs::read_to_string(copy.join(".env")).unwrap(),
        "TOKEN=secret"
    );
    assert_eq!(
        std::fs::read_to_string(env.join("env")).unwrap(),
        format!(
            "AWS_SHARED_CREDENTIALS_FILE={}\n",
            env.join("aws").display()
        )
    );
    let excludes = std::fs::read_to_string(b.layout.state.join("excludes")).unwrap();
    assert_eq!(excludes, format!("{}\n", copy.join("target").display()));
    let prompt = std::fs::read_to_string(b.layout.state.join(REPO).join("prompt.md")).unwrap();
    assert!(
        prompt.contains("# Hand-over: add --root") && prompt.contains("## Attached by the packer")
    );
    assert!(
        prompt.contains("?? b.txt") && prompt.contains("A  c.txt"),
        "{prompt}"
    );
    assert!(
        !b.layout.state.join(REPO).join("stage").exists(),
        "stage is emptied"
    );
    let claude: Value =
        serde_json::from_slice(&std::fs::read(b.layout.home.join(".claude.json")).unwrap())
            .unwrap();
    assert_eq!(
        claude["projects"][copy.to_string_lossy().as_ref()]["hasTrustDialogAccepted"],
        true
    );
}

#[tokio::test]
async fn send_fails_and_releases_the_copy_when_it_differs_from_s() {
    let b = TestBox::start(false).await;
    let mac = b.dir.path().join("mac");
    std::fs::create_dir_all(&mac).unwrap();
    sh(
        &mac,
        &format!(
            "git init -q -b main && git config user.email t@t && git config user.name t \
         && echo one > a.txt && git add -A && git commit -qm one \
         && git update-ref refs/cocoonbox/offload/{ID} $(git commit-tree HEAD^{{tree}} -p HEAD -m S)"
        ),
    );
    let copy = b.layout.workspace.join("gron");
    sh(
        &mac,
        &format!(
            "rsync -a --exclude=/.git/objects/ ./ {c}/ && echo drift > {c}/a.txt \
         && git bundle create {t}/offload.bundle --all 2>/dev/null",
            c = copy.display(),
            t = b.dir.path().display()
        ),
    );
    let bundle = std::fs::read(b.dir.path().join("offload.bundle")).unwrap();
    let stream = tar_of(&[
        ("handover.md", valid_handover().as_bytes()),
        ("offload.bundle", &bundle),
        ("secrets/kube", b"cfg"),
    ]);
    let a = args(&["--repo", REPO, "--kind", "claude", "--id", ID]);
    let Err(Fail::Failed(msg)) = packer::send(&b.layout, &a, stream).await else {
        panic!("send took a copy that differs from S");
    };
    assert!(msg.contains("differ"), "{msg}");
    assert!(
        !b.layout.secrets.join(REPO).exists(),
        "a failed send wipes the secrets"
    );
    let index: Index =
        serde_json::from_slice(&std::fs::read(b.layout.state.join("index.json")).unwrap()).unwrap();
    assert_eq!(
        index[REPO].offload_id, "",
        "a failed send releases the copy"
    );
    assert!(!b.tmux_log().contains("new-session"));
}

#[tokio::test]
async fn a_second_send_takes_history_while_the_synced_refs_name_objects_only_the_bundle_has() {
    let b = TestBox::start(false).await;
    let mac = b.dir.path().join("mac");
    std::fs::create_dir_all(&mac).unwrap();
    let copy = b.layout.workspace.join("gron");
    sh(
        &mac,
        &format!(
            "git init -q -b main && git config user.email t@t && git config user.name t \
         && echo one > a.txt && git add -A && git commit -qm one \
         && rsync -ac ./ {c}/ && echo second-version > a.txt && git commit -qam two \
         && git update-ref refs/cocoonbox/offload/{ID} $(git commit-tree HEAD^{{tree}} -p HEAD -m S) \
         && rsync -ac --delete --exclude=/.git/objects/ ./ {c}/ \
         && git bundle create {}/offload.bundle --all ^$(git rev-parse HEAD~1) 2>/dev/null",
            b.dir.path().display(),
            c = copy.display(),
        ),
    );
    let bundle = std::fs::read(b.dir.path().join("offload.bundle")).unwrap();
    let stream = tar_of(&[
        ("handover.md", valid_handover().as_bytes()),
        ("offload.bundle", &bundle),
    ]);
    let socket = b.layout.socket.clone();
    let session = b.fake.join(format!("sessions/cb-off-{REPO}-claude"));
    tokio::spawn(async move {
        until("the CLI session", || session.exists()).await;
        offload::request(
            &socket,
            &json!({"op": "event", "repo": REPO, "cli": "claude", "hook": "UserPromptSubmit", "payload": {}}),
        )
        .await
    });
    let a = args(&["--repo", REPO, "--kind", "claude", "--id", ID]);
    let out = packer::send(&b.layout, &a, stream).await.unwrap();
    assert_eq!(out["delivery"], "started", "{out}");
    assert_eq!(sh(&copy, "git log --format=%s -1"), "two");
    assert_eq!(sh(&copy, "git status --porcelain"), "");
}

#[tokio::test]
async fn begin_takes_the_copy_and_reports_its_ignored_directories() {
    let b = TestBox::start(false).await;
    let fakebin = b.layout.home.join("fakebin");
    std::fs::create_dir_all(&fakebin).unwrap();
    for (name, body) in [
        ("mise", "exit 0"),
        (
            "claude",
            r#"case "$1" in --version) echo "2.1.288 (Claude Code)";; auth) echo '{"loggedIn": true, "email": "u@example.com"}';; esac"#,
        ),
    ] {
        let path = fakebin.join(name);
        std::fs::write(&path, format!("#!/bin/sh\n{body}\n")).unwrap();
        std::fs::set_permissions(&path, PermissionsExt::from_mode(0o755)).unwrap();
    }
    std::fs::write(
        b.layout.home.join(".profile"),
        "PATH=\"$HOME/fakebin:$PATH\"\n",
    )
    .unwrap();
    let copy = b.layout.workspace.join("gron");
    sh(
        &copy,
        "git init -q && printf 'target/\\nnode_modules/\\n*.log\\n' > .gitignore \
        && mkdir -p target/debug node_modules/x && touch target/debug/a node_modules/x/b a.log",
    );
    let a = args(&[
        "--repo",
        REPO,
        "--name",
        "gron",
        "--kind",
        "claude",
        "--id",
        "ffffffffffff",
    ]);
    let out = packer::begin(&b.layout, &a).await.unwrap();
    assert_eq!(out["copy"], "mirror", "{out}");
    assert_eq!(out["dir"], copy.to_string_lossy().as_ref());
    assert_eq!(out["ignored"], json!(["node_modules/", "target/"]));
    assert_eq!(out["cli"]["signed_in"], true);
    assert_eq!(out["cli"]["account"], "u@example.com");
    let index: Index =
        serde_json::from_slice(&std::fs::read(b.layout.state.join("index.json")).unwrap()).unwrap();
    assert_eq!(index[REPO].offload_id, "ffffffffffff");
}
