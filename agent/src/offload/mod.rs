//! `cocoonbox-agent offload <verb>`: the box side of offload (design §27, contracts.md "Offload"),
//! run as `user`. Every verb prints one JSON line and exits 0, 1 `failed`, 2 `usage`, 3 `refused`
//! or 4 `inbox`.

pub mod env;
pub mod handover;
pub mod inbox;
pub mod packer;

use std::collections::BTreeMap;
use std::io::Read;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::UnixStream;

const SOCKET: &str = "/run/cocoonbox/offload.sock";
const SECRETS: &str = "/run/cocoonbox-offload/secrets";
const WORKSPACE: &str = "/workspace";
const STATE_REL: &str = ".local/share/cocoonbox/offload";
const INDEX_FILE: &str = "index.json";
const INDEX_LOCK: &str = "index.lock";
const EXCLUDES_FILE: &str = "excludes";
const MAX_WAIT_S: u64 = 600;
const KINDS: [&str; 3] = ["claude", "codex", "cursor"];
const USAGE: &str = "usage: cocoonbox-agent offload \
<begin|send|read|status|end|wipe-secrets|restart|post|env|event|serve> [flags]";

/// The box paths offload works in.
#[derive(Clone, Debug)]
pub struct Layout {
    /// `~/.local/share/cocoonbox/offload`: the index, excludes and one directory per repo key.
    pub state: PathBuf,
    pub socket: PathBuf,
    /// The tmpfs directory holding each repo's secrets.
    pub secrets: PathBuf,
    pub workspace: PathBuf,
    pub home: PathBuf,
    /// The tmux binary; tests put a fake in its place.
    pub tmux: PathBuf,
}

impl Layout {
    /// The contract paths under `$HOME`.
    pub fn from_env() -> Result<Self, String> {
        let home = PathBuf::from(std::env::var("HOME").map_err(|e| format!("read HOME: {e}"))?);
        Ok(Self {
            state: home.join(STATE_REL),
            socket: SOCKET.into(),
            secrets: SECRETS.into(),
            workspace: WORKSPACE.into(),
            home,
            tmux: "tmux".into(),
        })
    }

    /// The per-repo directory: inbox, outbox, session record, hooks, prompts, stage.
    pub fn repo_dir(&self, repo: &str) -> PathBuf {
        self.state.join(repo)
    }

    /// The file the state layer reads its `/workspace` excludes from, relative to the home root.
    pub fn excludes_rel() -> PathBuf {
        Path::new(STATE_REL).join(EXCLUDES_FILE)
    }

    fn excludes(&self) -> PathBuf {
        self.state.join(EXCLUDES_FILE)
    }

    fn secrets_dir(&self, repo: &str) -> PathBuf {
        self.secrets.join(repo)
    }
}

/// Why a verb did not finish; each maps to a `code` and an exit status.
#[derive(Debug)]
pub enum Fail {
    Usage(String),
    /// Nothing changed; `extra` adds fields such as `findings`.
    Refused(String, Value),
    Failed(String),
    Inbox(String),
}

impl Fail {
    fn refused(msg: impl Into<String>) -> Self {
        Self::Refused(msg.into(), Value::Null)
    }

    fn output(self) -> (u8, Value) {
        let (exit, code, error, extra) = match self {
            Self::Usage(e) => (2, "usage", e, Value::Null),
            Self::Refused(e, extra) => (3, "refused", e, extra),
            Self::Failed(e) => (1, "failed", e, Value::Null),
            Self::Inbox(e) => (4, "inbox", e, Value::Null),
        };
        let mut out = json!({"ok": false, "code": code, "error": one_line(&error)});
        if let Value::Object(fields) = extra {
            for (k, v) in fields {
                out[k] = v;
            }
        }
        (exit, out)
    }
}

impl From<String> for Fail {
    fn from(e: String) -> Self {
        Self::Failed(e)
    }
}

/// One repo key's entry in `index.json`.
#[derive(Clone, Debug, Default, Deserialize, Serialize)]
pub struct Copy {
    pub name: String,
    pub dir: PathBuf,
    /// The offload that holds the copy; empty when released.
    #[serde(default)]
    pub offload_id: String,
    /// A CLI session started on the copy since the last `end`.
    #[serde(default)]
    pub unreturned: bool,
}

/// Repo key → its box copy.
pub type Index = BTreeMap<String, Copy>;

/// Runs `cocoonbox-agent offload <args>`; returns the exit status.
pub async fn main(args: Vec<String>) -> u8 {
    let result = match Layout::from_env() {
        Ok(layout) => dispatch(&layout, args).await,
        Err(e) => Err(Fail::Failed(e)),
    };
    let (code, out) = match result {
        Ok(Some(v)) => (0, v),
        Ok(None) => return 0,
        Err(e) => e.output(),
    };
    println!("{out}");
    code
}

/// Parsed flags: `--name value` pairs, bare `--flag`s, and positional words.
#[derive(Debug, Default)]
pub struct Args {
    values: BTreeMap<String, String>,
    flags: Vec<String>,
    positional: Vec<String>,
}

impl Args {
    const VALUED: [&str; 9] = [
        "repo", "name", "kind", "id", "since", "wait", "cli", "hook", "box",
    ];

    /// Parses `args`; a valued flag without a value is a usage error.
    pub fn parse(args: impl IntoIterator<Item = String>) -> Result<Self, Fail> {
        let mut out = Self::default();
        let mut it = args.into_iter();
        while let Some(arg) = it.next() {
            let Some(name) = arg.strip_prefix("--") else {
                out.positional.push(arg);
                continue;
            };
            if Self::VALUED.contains(&name) {
                let v = it
                    .next()
                    .ok_or_else(|| Fail::Usage(format!("--{name} needs a value")))?;
                out.values.insert(name.to_owned(), v);
            } else {
                out.flags.push(name.to_owned());
            }
        }
        Ok(out)
    }

    fn flag(&self, name: &str) -> bool {
        self.flags.iter().any(|f| f == name)
    }

    fn value(&self, name: &str) -> Option<&str> {
        self.values.get(name).map(String::as_str)
    }

    fn required(&self, name: &str) -> Result<String, Fail> {
        self.value(name)
            .map(str::to_owned)
            .ok_or_else(|| Fail::Usage(format!("--{name} is required")))
    }

    fn number(&self, name: &str) -> Result<Option<u64>, Fail> {
        self.value(name)
            .map(|v| {
                v.parse()
                    .map_err(|_| Fail::Usage(format!("--{name} takes a number")))
            })
            .transpose()
    }

    fn repo(&self) -> Result<String, Fail> {
        let repo = self.required("repo")?;
        valid_key(&repo)?;
        Ok(repo)
    }

    fn repo_or_env(&self) -> Result<String, Fail> {
        match self.value("repo") {
            Some(_) => self.repo(),
            None => {
                let repo = std::env::var("COCOONBOX_OFFLOAD_REPO").map_err(|_| {
                    Fail::Usage("--repo or COCOONBOX_OFFLOAD_REPO is required".into())
                })?;
                valid_key(&repo)?;
                Ok(repo)
            }
        }
    }

    fn kind(&self) -> Result<String, Fail> {
        let kind = self.required("kind")?;
        if KINDS.contains(&kind.as_str()) {
            Ok(kind)
        } else {
            Err(Fail::Usage(format!(
                "--kind is one of {}",
                KINDS.join(", ")
            )))
        }
    }
}

/// A repo key: 12 lowercase hex, or `agent-` and 8 lowercase hex.
pub fn valid_key(key: &str) -> Result<(), Fail> {
    let hex = |s: &str, n: usize| {
        s.len() == n && s.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
    };
    if hex(key, 12) || key.strip_prefix("agent-").is_some_and(|k| hex(k, 8)) {
        Ok(())
    } else {
        Err(Fail::Usage(format!("invalid repo key {key:?}")))
    }
}

/// Sends one request to the inbox and returns its response; a refusal or failure becomes a `Fail`.
pub async fn call(layout: &Layout, req: &Value) -> Result<Value, Fail> {
    let resp = request(&layout.socket, req).await?;
    if resp["ok"].as_bool() == Some(true) {
        return Ok(resp);
    }
    let error = resp["error"]
        .as_str()
        .unwrap_or("the inbox failed")
        .to_owned();
    Err(match resp["code"].as_str() {
        Some("invalid") => Fail::Usage(error),
        Some("refused") => Fail::refused(error),
        _ => Fail::Failed(error),
    })
}

/// One request line and its response line over `socket`.
pub async fn request(socket: &Path, req: &Value) -> Result<Value, Fail> {
    let not_running = |e: std::io::Error| Fail::Inbox(format!("the inbox is not running: {e}"));
    let stream = UnixStream::connect(socket).await.map_err(not_running)?;
    let (rd, mut wr) = stream.into_split();
    let mut line = serde_json::to_vec(req).map_err(|e| format!("encode request: {e}"))?;
    line.push(b'\n');
    wr.write_all(&line).await.map_err(not_running)?;
    let mut resp = String::new();
    BufReader::new(rd)
        .read_line(&mut resp)
        .await
        .map_err(not_running)?;
    if resp.is_empty() {
        return Err(Fail::Inbox("the inbox closed the connection".to_owned()));
    }
    serde_json::from_str(&resp).map_err(|e| Fail::Failed(format!("decode inbox response: {e}")))
}

/// Runs `f` with the state directory under the index lock, which also guards `excludes`.
pub async fn locked<T, F>(layout: &Layout, f: F) -> Result<T, Fail>
where
    F: FnOnce(&Path) -> Result<T, Fail> + Send + 'static,
    T: Send + 'static,
{
    let state = layout.state.clone();
    tokio::task::spawn_blocking(move || {
        create_private(&state)?;
        let lock = std::fs::OpenOptions::new()
            .create(true)
            .truncate(false)
            .write(true)
            .open(state.join(INDEX_LOCK))
            .map_err(|e| format!("open index lock: {e}"))?;
        lock.lock().map_err(|e| format!("lock index: {e}"))?;
        f(&state)
    })
    .await
    .map_err(|e| Fail::Failed(format!("index task: {e}")))?
}

/// Reads the index under its lock, lets `f` change it, and writes it back when `f` succeeds.
pub async fn with_index<T, F>(layout: &Layout, f: F) -> Result<T, Fail>
where
    F: FnOnce(&mut Index) -> Result<T, Fail> + Send + 'static,
    T: Send + 'static,
{
    locked(layout, move |state| {
        let path = state.join(INDEX_FILE);
        let mut index = read_index_file(&path)?;
        let out = f(&mut index)?;
        let raw = serde_json::to_vec_pretty(&index).map_err(|e| format!("encode index: {e}"))?;
        let tmp = path.with_extension("json.tmp");
        std::fs::write(&tmp, raw).map_err(|e| format!("write {}: {e}", tmp.display()))?;
        std::fs::rename(&tmp, &path).map_err(|e| format!("replace {}: {e}", path.display()))?;
        Ok(out)
    })
    .await
}

/// The copy's state: `busy` while its session is not stopped, else `unreturned`, `mirror` or `absent`.
pub fn copy_state(copy: Option<&Copy>, running: bool) -> &'static str {
    match copy {
        _ if running => "busy",
        Some(c) if c.unreturned => "unreturned",
        Some(c) if c.dir.exists() => "mirror",
        _ => "absent",
    }
}

/// The index as it is now, without the lock.
pub async fn read_index(layout: &Layout) -> Result<Index, String> {
    let path = layout.state.join(INDEX_FILE);
    tokio::task::spawn_blocking(move || read_index_file(&path))
        .await
        .map_err(|e| format!("index task: {e}"))?
}

/// Creates `dir` and its parents, the last one 0700.
pub fn create_private(dir: &Path) -> Result<(), String> {
    use std::os::unix::fs::{DirBuilderExt, PermissionsExt};
    std::fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(dir)
        .map_err(|e| format!("create {}: {e}", dir.display()))?;
    std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))
        .map_err(|e| format!("chmod {}: {e}", dir.display()))
}

/// Writes `data` to a temporary sibling, then renames it over `path`.
pub async fn write_atomic(path: &Path, data: &[u8]) -> Result<(), String> {
    let tmp = path.with_extension(format!("tmp-{}", std::process::id()));
    tokio::fs::write(&tmp, data)
        .await
        .map_err(|e| format!("write {}: {e}", tmp.display()))?;
    tokio::fs::rename(&tmp, path)
        .await
        .map_err(|e| format!("replace {}: {e}", path.display()))
}

/// Runs `cmd` in a login bash in `dir`; its stdout on success, else its stderr.
pub async fn login_shell(dir: &Path, cmd: &str) -> Result<String, String> {
    shell(tokio::process::Command::new("bash").current_dir(dir), cmd).await
}

/// Runs `cmd` in a login bash of the user whose home is `home`, in that home.
pub async fn home_shell(home: &Path, cmd: &str) -> Result<String, String> {
    shell(
        tokio::process::Command::new("bash")
            .current_dir(home)
            .env("HOME", home),
        cmd,
    )
    .await
}

async fn shell(bash: &mut tokio::process::Command, cmd: &str) -> Result<String, String> {
    let out = bash
        .args(["-lc", cmd])
        .stdin(std::process::Stdio::null())
        .output()
        .await
        .map_err(|e| format!("spawn bash: {e}"))?;
    if out.status.success() {
        Ok(String::from_utf8_lossy(&out.stdout).into_owned())
    } else {
        Err(one_line(&format!(
            "{cmd}: {}: {}",
            out.status,
            String::from_utf8_lossy(&out.stderr).trim()
        )))
    }
}

/// Runs git in `dir`; its trimmed stdout on success.
pub async fn git(dir: &Path, args: &[&str]) -> Result<String, String> {
    git_env(dir, args, &[]).await
}

/// Runs git in `dir` with extra environment; its trimmed stdout on success.
pub async fn git_env(dir: &Path, args: &[&str], envs: &[(&str, &Path)]) -> Result<String, String> {
    let out = tokio::process::Command::new("git")
        .args(args)
        .current_dir(dir)
        .envs(envs.iter().copied())
        .stdin(std::process::Stdio::null())
        .output()
        .await
        .map_err(|e| format!("spawn git: {e}"))?;
    if out.status.success() {
        Ok(String::from_utf8_lossy(&out.stdout).trim_end().to_owned())
    } else {
        Err(one_line(&format!(
            "git {}: {}",
            args.join(" "),
            String::from_utf8_lossy(&out.stderr).trim()
        )))
    }
}

async fn dispatch(layout: &Layout, args: Vec<String>) -> Result<Option<Value>, Fail> {
    let mut args = args.into_iter();
    let verb = args.next().unwrap_or_default();
    let a = Args::parse(args)?;
    let done = match verb.as_str() {
        "begin" => packer::begin(layout, &a).await?,
        "send" => {
            if a.flag("message") || a.flag("back") {
                packer::message(layout, &a, &stdin_text().await?).await?
            } else {
                let stdin = std::io::stdin();
                packer::send(layout, &a, stdin).await?
            }
        }
        "read" => {
            let wait = a.number("wait")?.unwrap_or(0);
            if wait > MAX_WAIT_S {
                return Err(Fail::Usage(format!("--wait is at most {MAX_WAIT_S}")));
            }
            let mut req = json!({
                "op": "read",
                "repo": a.repo()?,
                "since": a.number("since")?.unwrap_or(0),
            });
            if wait > 0 {
                req["wait_ms"] = (wait * 1000).into();
            }
            call(layout, &req).await?
        }
        "status" => {
            let mut req = json!({"op": "status"});
            if let Some(r) = a.value("repo") {
                req["repo"] = r.into();
            }
            call(layout, &req).await?
        }
        "end" => packer::end(layout, &a.repo()?).await?,
        "wipe-secrets" => {
            let repo = a.repo()?;
            packer::wipe_secrets(layout, &repo).await?;
            json!({"ok": true, "repo": repo})
        }
        "restart" => call(layout, &json!({"op": "restart", "repo": a.repo()?})).await?,
        "post" => packer::post(layout, &a, &stdin_text().await?).await?,
        "env" => {
            let repo = a.repo_or_env()?;
            let report = if a.flag("no-wait") {
                env::load(layout, &repo).await?
            } else {
                let wait_ms = u64::try_from(env::WAIT_LIMIT.as_millis()).unwrap_or(u64::MAX);
                let resp = call(
                    layout,
                    &json!({"op": "env", "repo": repo, "wait_ms": wait_ms}),
                )
                .await?;
                serde_json::from_value(resp["report"].clone())
                    .map_err(|e| format!("decode env report: {e}"))?
            };
            if a.flag("json") {
                json!({"ok": true, "env": report})
            } else {
                print!("{}", env::render(report.as_ref()));
                return Ok(None);
            }
        }
        "event" => {
            let payload = match a.positional.first() {
                Some(p) => p.clone(),
                None => stdin_text().await?,
            };
            let payload: Value = serde_json::from_str(&payload).unwrap_or(Value::Null);
            let req = json!({
                "op": "event",
                "repo": a.repo()?,
                "cli": a.required("cli")?,
                "hook": a.required("hook")?,
                "payload": payload,
            });
            call(layout, &req).await?
        }
        "serve" => {
            inbox::serve(layout.clone()).await?;
            return Ok(None);
        }
        _ => return Err(Fail::Usage(USAGE.to_owned())),
    };
    Ok(Some(done))
}

fn read_index_file(path: &Path) -> Result<Index, String> {
    match std::fs::read(path) {
        Ok(raw) => {
            serde_json::from_slice(&raw).map_err(|e| format!("decode {}: {e}", path.display()))
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(Index::new()),
        Err(e) => Err(format!("read {}: {e}", path.display())),
    }
}

async fn stdin_text() -> Result<String, Fail> {
    tokio::task::spawn_blocking(|| {
        let mut s = String::new();
        std::io::stdin()
            .take(handover::MAX_BODY as u64 + 1)
            .read_to_string(&mut s)
            .map(|_| s)
            .map_err(|e| Fail::Usage(format!("read stdin: {e}")))
    })
    .await
    .map_err(|e| Fail::Failed(format!("stdin task: {e}")))?
}

fn one_line(s: &str) -> String {
    s.split_whitespace().collect::<Vec<_>>().join(" ")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn keys_are_twelve_hex_or_an_agent_key() {
        assert!(valid_key("0123456789ab").is_ok());
        assert!(valid_key("agent-0123abcd").is_ok());
        assert!(valid_key("0123456789AB").is_err());
        assert!(valid_key("../etc").is_err());
        assert!(valid_key("agent-0123abc").is_err());
    }

    #[test]
    fn flags_values_and_positionals_parse() {
        let a = Args::parse(
            ["--repo", "0123456789ab", "--back", "--wait", "5", "{}"].map(String::from),
        )
        .unwrap();
        assert_eq!(a.value("repo"), Some("0123456789ab"));
        assert!(a.flag("back") && !a.flag("message"));
        assert_eq!(a.number("wait").unwrap(), Some(5));
        assert_eq!(a.positional, ["{}"]);
        assert!(matches!(
            Args::parse(["--repo".to_owned()]),
            Err(Fail::Usage(_))
        ));
    }

    #[test]
    fn a_refusal_carries_its_extra_fields_and_exit_three() {
        let (code, out) =
            Fail::Refused("blocked\nby two".into(), json!({"findings": [1]})).output();
        assert_eq!(code, 3);
        assert_eq!(
            out,
            json!({"ok": false, "code": "refused", "error": "blocked by two", "findings": [1]})
        );
    }
}
