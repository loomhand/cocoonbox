//! Persistent bash sessions: one per agent, state (cwd, env) survives between calls.

use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::process::{Child, ChildStdin, ChildStdout, Command};

use super::rpc::Output;
use crate::sys;

const MARK: &str = "__CB_DONE_";
/// The box's API key exports, sourced before every command so a key added or deleted while the session lives reaches it.
const VAULT: &str = "/run/cocoonbox-vault/vault.sh";
const HEAD_BYTES: usize = 10_000;
const TAIL_BYTES: usize = 20_000;
const SLACK_BYTES: usize = 64 << 10;
const DEFAULT_TIMEOUT: Duration = Duration::from_secs(120);
const MAX_TIMEOUT: Duration = Duration::from_secs(600);
const EXIT_WAIT: Duration = Duration::from_secs(2);
#[cfg(unix)]
const BASH: &str = "bash";
/// Git for Windows' bash, which puts its tools on PATH; a bare `bash` can be WSL's launcher.
#[cfg(windows)]
const BASH: &str = r"C:\Program Files\Git\bin\bash.exe";

/// A live bash process; commands run by sourcing a script file so syntax errors stay contained.
pub struct Shell {
    child: Child,
    stdin: ChildStdin,
    stdout: ChildStdout,
    script: PathBuf,
    dead: bool,
    tree: sys::Tree,
}

impl Shell {
    /// Starts bash in `cwd`; `script` is the per-session scratch file for commands.
    pub async fn spawn(cwd: &Path, script: PathBuf) -> Result<Self, String> {
        let mut child = sys::new_group(bash(cwd).args(["--noprofile", "--norc"]))
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()
            .map_err(|e| format!("spawn bash: {e}"))?;
        let tree = sys::Tree::hold(&child).map_err(|e| format!("hold bash: {e}"))?;
        let (Some(mut stdin), Some(stdout)) = (child.stdin.take(), child.stdout.take()) else {
            return Err("bash pipes missing".to_owned());
        };
        stdin
            .write_all(b"exec 2>&1\n")
            .await
            .map_err(|e| format!("init bash: {e}"))?;
        Ok(Self {
            child,
            stdin,
            stdout,
            script,
            dead: false,
            tree,
        })
    }

    /// Whether the session must be replaced before the next command.
    pub fn is_dead(&self) -> bool {
        self.dead
    }

    /// Runs `command` and returns its merged output with the exit status; a non-zero status or a timeout is an error.
    pub async fn run(
        &mut self,
        command: &str,
        timeout: Option<Duration>,
    ) -> Result<Output, String> {
        let timeout = timeout.unwrap_or(DEFAULT_TIMEOUT).min(MAX_TIMEOUT);
        tokio::fs::write(&self.script, command)
            .await
            .map_err(|e| format!("write {}: {e}", self.script.display()))?;
        let nonce = uuid::Uuid::new_v4().simple().to_string();
        let marker = format!("\n{MARK}{nonce}:");
        let line = format!(
            "[ -r {VAULT} ] && . {VAULT}\n. '{}' </dev/null\nprintf '\\n{MARK}{nonce}:%s\\n' \"$?\"\n",
            bash_path(&self.script)
        );
        let mut guard = KillOnDrop {
            shell: self,
            armed: true,
        };
        let shell = &mut *guard.shell;
        shell
            .stdin
            .write_all(line.as_bytes())
            .await
            .map_err(|e| format!("write to bash: {e}"))?;
        let mut capture = Capture::default();
        let outcome = tokio::time::timeout(
            timeout,
            read_until(&mut shell.stdout, &marker, &mut capture),
        )
        .await;
        let result = match outcome {
            Ok(Ok(Some(code))) => {
                guard.armed = false;
                Ok(Output {
                    text: format!("{}\n[exit status {code}]", capture.render()),
                    is_error: code != 0,
                    ..Output::default()
                })
            }
            Ok(Ok(None)) => {
                // the command ran `exit` in the sourced script, which ends bash itself
                let status = tokio::time::timeout(EXIT_WAIT, shell.child.wait()).await;
                let status = match status.ok().and_then(Result::ok).map(|s| s.code()) {
                    Some(Some(code)) => format!("exit status {code}"),
                    Some(None) => "killed by a signal".to_owned(),
                    None => "exit status unknown".to_owned(),
                };
                Ok(Output::error(format!(
                    "{}\n[{status}; the shell exited, the next call starts a fresh session]",
                    capture.render()
                )))
            }
            Ok(Err(e)) => Err(e),
            Err(_) => Ok(Output::error(format!(
                "{}\n[timed out after {:.1}s; the shell session was reset, cwd and env are lost]",
                capture.render(),
                timeout.as_secs_f64()
            ))),
        };
        drop(guard);
        result
    }

    /// Kills bash and every process of its tree; the next call starts a fresh session.
    pub fn kill(&mut self) {
        self.dead = true;
        let _ = self.tree.kill();
    }
}

/// A note for a requested `timeout` over [`MAX_TIMEOUT`], which a command runs with instead.
pub fn capped_note(timeout: Option<Duration>) -> String {
    match timeout {
        Some(t) if t > MAX_TIMEOUT => format!(
            "\n[timeout_s {} is over the {} s maximum; ran with {} s]",
            t.as_secs(),
            MAX_TIMEOUT.as_secs(),
            MAX_TIMEOUT.as_secs()
        ),
        _ => String::new(),
    }
}

/// bash in `cwd` with the environment of the shell tool.
pub fn bash(cwd: &Path) -> Command {
    let mut cmd = Command::new(BASH);
    cmd.current_dir(cwd)
        .env("TERM", "dumb")
        .env("PAGER", "cat")
        .env("GIT_PAGER", "cat")
        .env("NO_COLOR", "1")
        .kill_on_drop(true);
    sys::unprotect(&mut cmd);
    cmd
}

struct KillOnDrop<'a> {
    shell: &'a mut Shell,
    armed: bool,
}

impl Drop for KillOnDrop<'_> {
    fn drop(&mut self) {
        if self.armed {
            self.shell.kill();
        }
    }
}

/// Head-and-tail capture of a command's output with the middle dropped.
#[derive(Default)]
struct Capture {
    buf: Vec<u8>,
    dropped: usize,
}

impl Capture {
    fn trim(&mut self) {
        if self.buf.len() > HEAD_BYTES + TAIL_BYTES + SLACK_BYTES {
            let cut = self.buf.len() - TAIL_BYTES;
            self.dropped += cut - HEAD_BYTES;
            self.buf.drain(HEAD_BYTES..cut);
        }
    }

    fn take_status(&mut self, from: usize, marker: &[u8]) -> Option<i32> {
        let at = from
            + self.buf[from..]
                .windows(marker.len())
                .position(|w| w == marker)?;
        let rest = &self.buf[at + marker.len()..];
        let end = rest.iter().position(|&b| b == b'\n')?;
        let code = String::from_utf8_lossy(&rest[..end])
            .trim()
            .parse()
            .unwrap_or(-1);
        self.buf.truncate(at);
        Some(code)
    }

    fn render(&self) -> String {
        let text = String::from_utf8_lossy(&self.buf);
        let text = text.trim_end_matches('\n');
        if self.dropped == 0 && text.len() <= HEAD_BYTES + TAIL_BYTES {
            return text.to_owned();
        }
        let head = floor_char(text, HEAD_BYTES);
        let tail = ceil_char(text, text.len().saturating_sub(TAIL_BYTES).max(head));
        let omitted = self.dropped + (tail - head);
        format!(
            "{}\n[... {omitted} bytes omitted ...]\n{}",
            &text[..head],
            &text[tail..]
        )
    }
}

async fn read_until(
    stdout: &mut ChildStdout,
    marker: &str,
    capture: &mut Capture,
) -> Result<Option<i32>, String> {
    let marker = marker.as_bytes();
    let mut chunk = vec![0u8; 16 << 10];
    let mut from = 0;
    loop {
        let n = stdout
            .read(&mut chunk)
            .await
            .map_err(|e| format!("read bash output: {e}"))?;
        if n == 0 {
            return Ok(None);
        }
        capture.buf.extend_from_slice(&chunk[..n]);
        if let Some(code) = capture.take_status(from, marker) {
            return Ok(Some(code));
        }
        capture.trim();
        from = capture.buf.len().saturating_sub(marker.len() + 32);
    }
}

fn floor_char(s: &str, mut i: usize) -> usize {
    i = i.min(s.len());
    while !s.is_char_boundary(i) {
        i -= 1;
    }
    i
}

fn ceil_char(s: &str, mut i: usize) -> usize {
    while i < s.len() && !s.is_char_boundary(i) {
        i += 1;
    }
    i
}

/// `path` as a bash command line names it.
#[cfg(unix)]
fn bash_path(path: &Path) -> String {
    path.display().to_string()
}

/// `path` as a bash command line names it: Git Bash reads `C:/x`, with forward slashes.
#[cfg(windows)]
fn bash_path(path: &Path) -> String {
    path.display().to_string().replace('\\', "/")
}

#[cfg(test)]
mod tests {
    use super::*;

    async fn shell(dir: &Path) -> Shell {
        Shell::spawn(dir, dir.join("cmd.sh")).await.unwrap()
    }

    #[tokio::test]
    async fn state_persists_and_status_is_reported() {
        let dir = tempfile::tempdir().unwrap();
        let mut sh = shell(dir.path()).await;
        let out = sh
            .run("mkdir sub && cd sub && export X=42", None)
            .await
            .unwrap();
        assert!(out.text.ends_with("[exit status 0]"), "{}", out.text);
        assert!(!out.is_error);
        let out = sh
            .run("echo $X; pwd; echo err >&2; false", None)
            .await
            .unwrap();
        assert!(out.text.starts_with("42\n"), "{}", out.text);
        assert!(out.text.contains("/sub\nerr"), "{}", out.text);
        assert!(out.text.ends_with("[exit status 1]"), "{}", out.text);
        assert!(out.is_error);
    }

    #[tokio::test]
    async fn syntax_errors_and_stdin_readers_do_not_wedge_the_session() {
        let dir = tempfile::tempdir().unwrap();
        let mut sh = shell(dir.path()).await;
        let out = sh.run("echo 'unterminated", None).await.unwrap().text;
        assert!(!out.ends_with("[exit status 0]"), "{out}");
        let out = sh.run("cat; echo after", None).await.unwrap().text;
        assert!(out.starts_with("after"), "{out}");
    }

    #[test]
    fn only_a_timeout_over_the_maximum_gets_a_note() {
        assert_eq!(
            capped_note(Some(Duration::from_secs(2400))),
            "\n[timeout_s 2400 is over the 600 s maximum; ran with 600 s]"
        );
        for quiet in [None, Some(MAX_TIMEOUT), Some(Duration::from_secs(30))] {
            assert_eq!(capped_note(quiet), "", "{quiet:?}");
        }
    }

    #[tokio::test]
    async fn timeout_kills_the_session() {
        let dir = tempfile::tempdir().unwrap();
        let mut sh = shell(dir.path()).await;
        let out = sh
            .run("echo start; sleep 30", Some(Duration::from_millis(300)))
            .await
            .unwrap();
        assert!(
            out.text.contains("start") && out.text.contains("timed out"),
            "{}",
            out.text
        );
        assert!(out.is_error);
        assert!(sh.is_dead());
    }

    #[cfg(target_os = "linux")]
    #[tokio::test]
    async fn a_session_does_not_inherit_the_oom_protection_of_the_toolhost() {
        let adj = "/proc/self/oom_score_adj";
        let own = std::fs::read_to_string(adj).unwrap();
        std::fs::write(adj, "500").unwrap();
        let dir = tempfile::tempdir().unwrap();
        let mut sh = shell(dir.path()).await;
        let out = sh.run("cat /proc/self/oom_score_adj", None).await;
        std::fs::write(adj, own.trim()).unwrap();
        let out = out.unwrap().text;
        assert!(out.starts_with("0\n"), "{out}");
    }

    #[tokio::test]
    async fn exit_marks_the_session_dead() {
        let dir = tempfile::tempdir().unwrap();
        let mut sh = shell(dir.path()).await;
        let out = sh.run("echo bye; exit 3", None).await.unwrap();
        assert!(out.text.starts_with("bye\n"), "{}", out.text);
        assert!(out.text.contains("[exit status 3;"), "{}", out.text);
        assert!(out.text.contains("shell exited"), "{}", out.text);
        assert!(out.is_error);
        assert!(sh.is_dead());
    }

    #[tokio::test]
    async fn large_output_keeps_head_and_tail() {
        let dir = tempfile::tempdir().unwrap();
        let mut sh = shell(dir.path()).await;
        let out = sh
            .run("seq 1 200000; echo last-line", None)
            .await
            .unwrap()
            .text;
        assert!(out.starts_with("1\n2\n"), "{}", &out[..50]);
        assert!(out.contains("bytes omitted"));
        assert!(out.contains("last-line\n[exit status 0]"));
        assert!(out.len() < HEAD_BYTES + TAIL_BYTES + 200);
    }
}
