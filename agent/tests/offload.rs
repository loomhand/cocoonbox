#![cfg(test)]

use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use cocoonbox_agent::journal::Journal;
use cocoonbox_agent::offload::inbox::Inbox;
use cocoonbox_agent::offload::{self, Args, Copy, Fail, Index, Layout, handover, packer};
use cocoonbox_agent::pb::{self, event};
use cocoonbox_agent::store::Store;
use serde_json::{Value, json};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::{UnixListener, UnixStream};
use tokio_util::sync::CancellationToken;

const REPO: &str = "0123456789ab";
const ID: &str = "a1b2c3d4e5f6";
const FAKE_TMUX: &str = r#"d=$(dirname "$0")
printf '%s\n' "$*" >> "$d/tmux.log"
case "$1" in
has-session) [ -e "$d/sessions/${3#=}" ] ;;
new-session) while [ $# -gt 0 ]; do [ "$1" = -s ] && { mkdir -p "$d/sessions"; touch "$d/sessions/$2"; }; shift; done ;;
kill-session) rm -f "$d/sessions/${3#=}" ;;
load-buffer) cat > "$d/buffer" ;;
display-message) echo 0 ;;
esac"#;

struct TestBox {
    _dir: tempfile::TempDir,
    layout: Layout,
    fake: PathBuf,
    stop: CancellationToken,
}

impl TestBox {
    /// A box with a copy of REPO held by ID and a running inbox over a fake tmux.
    async fn start(unreturned: bool) -> Self {
        let dir = tempfile::tempdir().unwrap();
        let fake = dir.path().join("fake");
        std::fs::create_dir_all(&fake).unwrap();
        let tmux = fake.join("tmux");
        std::fs::write(&tmux, format!("#!/bin/sh\n{FAKE_TMUX}\n")).unwrap();
        std::fs::set_permissions(&tmux, std::os::unix::fs::PermissionsExt::from_mode(0o755))
            .unwrap();
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
            Copy {
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
            _dir: dir,
            layout,
            fake,
            stop,
        }
    }

    async fn call(&self, req: Value) -> Value {
        offload::request(&self.layout.socket, &req).await.unwrap()
    }

    async fn start_session(&self, body: &str) -> Value {
        self.call(json!({
            "op": "send", "repo": REPO, "name": "gron", "cli": "claude",
            "offload_id": ID, "body": body, "start": true,
        }))
        .await
    }

    fn tmux_log(&self) -> String {
        std::fs::read_to_string(self.fake.join("tmux.log")).unwrap_or_default()
    }

    async fn status(&self) -> Value {
        let s = self.call(json!({"op": "status", "repo": REPO})).await;
        s["sessions"][0].clone()
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
    for _ in 0..100 {
        if ok() {
            return;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    panic!("timed out waiting for {what}");
}

fn valid_handover() -> String {
    let mut s = String::from("# Hand-over: add --root\n");
    for h in handover::HEADINGS {
        s.push_str(&format!("\n{h}\nnone\n"));
    }
    s
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
    let pasted = b
        .call(json!({"op": "send", "repo": REPO, "body": "THIRD"}))
        .await;
    assert_eq!(pasted["delivery"], "pasted", "{pasted}");
    assert_eq!(
        std::fs::read_to_string(b.fake.join("buffer")).unwrap(),
        "THIRD"
    );
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
    let name: String = b
        .layout
        .workspace
        .join("gron")
        .to_string_lossy()
        .chars()
        .map(|c| if c.is_ascii_alphanumeric() { c } else { '-' })
        .collect();
    let projects = b.layout.home.join(".claude/projects").join(name);
    std::fs::create_dir_all(&projects).unwrap();
    let transcript = projects.join("s.jsonl");
    let mut lines = [
        json!({"type": "user", "message": {"content": "FIRST"}}),
        json!({"type": "assistant", "message": {"content": [{"type": "tool_use"}]}}),
    ]
    .map(|l| format!("{l}\n"))
    .concat();
    std::fs::write(&transcript, &lines).unwrap();
    tokio::time::sleep(Duration::from_secs(6)).await;
    assert_eq!(b.status().await["status"], "working");
    lines.push_str(&format!(
        "{}\n{}\n",
        json!({"type": "assistant", "message": {"content": [{"type": "text", "text": "done"}]}}),
        json!({"type": "system", "subtype": "turn_duration"})
    ));
    std::fs::write(&transcript, &lines).unwrap();
    let enter = format!("send-keys -t =cb-off-{REPO}-claude: Enter");
    for _ in 0..240 {
        if b.tmux_log().contains(&enter) {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    let pasted = std::fs::read_to_string(b.fake.join("buffer")).unwrap();
    assert!(pasted.starts_with(handover::BACK), "{pasted}");
    let mut status = b.status().await["status"].clone();
    for _ in 0..100 {
        if status == "working" {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
        status = b.status().await["status"].clone();
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
        Store::open(&b._dir.path().join("agent.db")).unwrap(),
    ));
    let mut live = journal.bus.subscribe(0).unwrap().live;
    tokio::spawn(offload::inbox::relay(
        b.layout.socket.clone(),
        Arc::clone(&journal),
        b.stop.clone(),
    ));
    tokio::time::sleep(Duration::from_millis(300)).await;
    let posted = b
        .call(json!({"op": "post", "repo": REPO, "kind": "progress", "body": "SECRET-BODY"}))
        .await;
    assert_eq!(posted["seq"], 1, "{posted}");
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
            "entry": "progress", "seq": 1, "status": "working",
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
    assert_eq!(update.seq, 1);
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
    let got = tokio::time::timeout(Duration::from_secs(2), reader)
        .await
        .unwrap()
        .unwrap();
    assert!(started.elapsed() < Duration::from_secs(3));
    assert_eq!(got["entries"][0]["kind"], "question", "{got}");
    assert_eq!(got["entries"][0]["body"], "which flag?");
    assert_eq!(got["entries"][0]["offload_id"], ID);
    let after = b
        .call(json!({"op": "read", "repo": REPO, "since": 1}))
        .await;
    assert_eq!(after["entries"], json!([]));
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
    let Err(Fail::Refused(msg, _)) = packer::post(&b.layout, &a, &thin).await else {
        panic!("post took a hand-over without ## Decisions made");
    };
    assert!(msg.contains("## Decisions made"), "{msg}");
    let ok = packer::post(&b.layout, &a, &valid_handover())
        .await
        .unwrap();
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
    std::fs::create_dir_all(outbox.parent().unwrap()).unwrap();
    std::fs::write(
        &outbox,
        "{\"seq\":0,\"kind\":\"progress\",\"offload_id\":\"ffffffffffff\",\"cli\":\"claude\",\"time_ms\":1,\"body\":\"PREVIOUS-OFFLOAD\"}\n",
    )
    .unwrap();
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
    assert!(prompt.contains("NEW-HANDOVER") && !prompt.contains("OLD-HANDOVER"));
    std::fs::write(&outbox, "").unwrap();
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
    for _ in 0..20 {
        if restart.exists() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    let prompt = std::fs::read_to_string(&restart).expect("no restart within 1 s of the rescan");
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
async fn send_rebuilds_history_checks_s_places_secrets_and_starts_the_cli() {
    let b = TestBox::start(false).await;
    let mac = b._dir.path().join("mac");
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
            b._dir.path().display(),
            c = copy.display(),
        ),
    );
    let mut tar = tar::Builder::new(Vec::new());
    let mut add = |name: &str, data: &[u8]| {
        let mut h = tar::Header::new_ustar();
        h.set_size(data.len() as u64);
        h.set_mode(0o600);
        h.set_cksum();
        tar.append_data(&mut h, name, data).unwrap();
    };
    add("handover.md", valid_handover().as_bytes());
    add(
        "offload.bundle",
        &std::fs::read(b._dir.path().join("offload.bundle")).unwrap(),
    );
    add("secrets/file/.env", b"TOKEN=secret");
    add("secrets/aws", b"[default]");
    let stream = std::io::Cursor::new(tar.into_inner().unwrap());

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
    let status = |dir: &std::path::Path| {
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
    let mac = b._dir.path().join("mac");
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
            "rsync -a --exclude=/.git/objects/ ./ {}/ && echo drift > {}/a.txt \
         && git bundle create {}/offload.bundle --all 2>/dev/null",
            copy.display(),
            copy.display(),
            b._dir.path().display()
        ),
    );
    let mut tar = tar::Builder::new(Vec::new());
    for (name, data) in [
        ("handover.md", valid_handover().into_bytes()),
        (
            "offload.bundle",
            std::fs::read(b._dir.path().join("offload.bundle")).unwrap(),
        ),
        ("secrets/kube", b"cfg".to_vec()),
    ] {
        let mut h = tar::Header::new_ustar();
        h.set_size(data.len() as u64);
        h.set_cksum();
        tar.append_data(&mut h, name, data.as_slice()).unwrap();
    }
    let stream = std::io::Cursor::new(tar.into_inner().unwrap());
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

fn sh(dir: &std::path::Path, script: &str) -> String {
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
async fn a_second_send_takes_history_while_the_synced_refs_name_objects_only_the_bundle_has() {
    let b = TestBox::start(false).await;
    let mac = b._dir.path().join("mac");
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
            b._dir.path().display(),
            c = copy.display(),
        ),
    );
    let mut tar = tar::Builder::new(Vec::new());
    for (name, data) in [
        ("handover.md", valid_handover().into_bytes()),
        (
            "offload.bundle",
            std::fs::read(b._dir.path().join("offload.bundle")).unwrap(),
        ),
    ] {
        let mut h = tar::Header::new_ustar();
        h.set_size(data.len() as u64);
        h.set_cksum();
        tar.append_data(&mut h, name, data.as_slice()).unwrap();
    }
    let stream = std::io::Cursor::new(tar.into_inner().unwrap());
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
        std::fs::set_permissions(&path, std::os::unix::fs::PermissionsExt::from_mode(0o755))
            .unwrap();
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
    assert_eq!(out["findings"], json!([]));
    let index: Index =
        serde_json::from_slice(&std::fs::read(b.layout.state.join("index.json")).unwrap()).unwrap();
    assert_eq!(index[REPO].offload_id, "ffffffffffff");
}
