//! `exec`: one command as `user`, its stdin, stdout and stderr carried over the toolhost connection.

use std::io;
#[cfg(unix)]
use std::os::fd::OwnedFd;
#[cfg(unix)]
use std::os::unix::process::ExitStatusExt;
use std::path::Path;
use std::pin::Pin;
use std::process::{ExitStatus, Stdio};
#[cfg(windows)]
use std::sync::Arc;
#[cfg(windows)]
use std::task::{Context, Poll};
use std::time::Duration;

use serde_json::Value;
#[cfg(unix)]
use tokio::io::Interest;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
#[cfg(windows)]
use tokio::io::{ReadBuf, ReadHalf, WriteHalf};
#[cfg(unix)]
use tokio::net::unix::{OwnedReadHalf, OwnedWriteHalf, pipe};
#[cfg(windows)]
use tokio::net::windows::named_pipe::NamedPipeServer;
use tokio::process::{Child, Command};

use super::rpc::{self, Output};
use super::shell;
use crate::sys;

const READ_CHUNK: usize = 64 << 10;
const HANGUP_POLL: Duration = Duration::from_millis(200);
const DEFAULT_TERM: &str = "xterm-256color";
#[cfg(unix)]
const TERMINFO_TIMEOUT: Duration = Duration::from_secs(5);

type Reader = Box<dyn AsyncRead + Send + Unpin>;
type Writer = Box<dyn AsyncWrite + Send + Unpin>;
type Exit = Pin<Box<dyn Future<Output = io::Result<i32>> + Send>>;

/// The agent's read end of an exec connection.
#[cfg(unix)]
pub type AgentRead = OwnedReadHalf;
/// The agent's write end of an exec connection.
#[cfg(unix)]
pub type AgentWrite = OwnedWriteHalf;
/// The agent's write end of an exec connection.
#[cfg(windows)]
pub type AgentWrite = WriteHalf<NamedPipeServer>;
/// The terminal a command runs on, which takes its resizes.
#[cfg(unix)]
type Terminal = OwnedFd;
/// The terminal a command runs on, which takes its resizes.
#[cfg(windows)]
type Terminal = Arc<sys::Pty>;

/// The agent's read end of an exec connection, with the pipe whose hang-up it watches.
#[cfg(windows)]
pub struct AgentRead {
    pub half: ReadHalf<NamedPipeServer>,
    pub pipe: sys::PipeEnd,
}

#[cfg(windows)]
impl AsyncRead for AgentRead {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        Pin::new(&mut self.half).poll_read(cx, buf)
    }
}

/// A started command: its exit code to come, its process tree, its stdin, its output, and the terminal it runs on, if any.
struct Started {
    exit: Exit,
    tree: sys::Tree,
    stdin: Writer,
    stdout: Reader,
    stderr: Option<Reader>,
    tty: Option<Terminal>,
}

/// Runs `args.command` with a login bash in `cwd` until it exits; the agent closing the
/// connection first kills its process tree.
pub async fn run(cwd: &Path, home: &Path, args: &Value, rd: AgentRead, mut wr: AgentWrite) {
    let tty = &args["tty"];
    let (term, notice) = if tty.is_null() {
        (String::new(), None)
    } else {
        terminal_type(
            home,
            tty["term"].as_str().unwrap_or_default(),
            tty["terminfo"].as_str().unwrap_or_default(),
        )
        .await
    };
    let Started {
        exit,
        tree,
        stdin,
        stdout,
        stderr,
        tty,
    } = match spawn(cwd, home, args, &term) {
        Ok(started) => started,
        Err(e) => {
            let _ = rpc::write_frame(&mut wr, &Output::error(e)).await;
            return;
        }
    };
    let mut started = rpc::write_frame(&mut wr, &Output::text("started")).await;
    if let (Ok(()), Some(notice)) = (&started, notice) {
        started = rpc::write_tagged(&mut wr, rpc::STDERR, notice.as_bytes()).await;
    }
    if started.is_err() {
        let _ = tree.kill();
        return;
    }
    #[cfg(windows)]
    let console = tty.clone();
    let output = async {
        let exited = async {
            let code = exit.await.map_err(|e| format!("wait for bash: {e}"));
            // a pseudo console holds its output open until it is closed
            #[cfg(windows)]
            if let Some(console) = console {
                let _ = tokio::task::spawn_blocking(move || console.close()).await;
            }
            code
        };
        let ((), code) = tokio::try_join!(drain(stdout, stderr, &mut wr), exited)?;
        rpc::write_tagged(&mut wr, rpc::EXIT, &code.to_be_bytes()).await
    };
    tokio::select! {
        () = feed(rd, stdin, tty.as_ref(), &tree) => {
            let _ = tree.kill();
        }
        r = output => if let Err(e) = r {
            eprintln!("toolhost: exec: {e}");
            let _ = tree.kill();
        },
    }
    #[cfg(windows)]
    if let Some(console) = tty {
        let _ = tokio::task::spawn_blocking(move || console.close()).await;
    }
}

fn spawn(cwd: &Path, home: &Path, args: &Value, term: &str) -> Result<Started, String> {
    let command = args["command"]
        .as_str()
        .ok_or("exec needs a command string")?;
    let mut cmd = shell::bash(cwd);
    cmd.args(["-lc", command]);
    let tty = &args["tty"];
    if tty.is_null() {
        return piped(cmd);
    }
    let size = |key: &str| u16::try_from(tty[key].as_u64().unwrap_or(0)).unwrap_or(u16::MAX);
    cmd.env("TERM", term)
        .env("HOME", home)
        .env_remove("PAGER")
        .env_remove("GIT_PAGER")
        .env_remove("NO_COLOR");
    on_terminal(cmd, size("rows"), size("cols"))
}

/// Starts `cmd` with piped stdio in a process tree of its own.
fn piped(mut cmd: Command) -> Result<Started, String> {
    let mut child = sys::new_group(&mut cmd)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|e| format!("spawn bash: {e}"))?;
    let tree = sys::Tree::hold(&child).map_err(|e| format!("hold bash: {e}"))?;
    let (Some(stdin), Some(stdout), Some(stderr)) =
        (child.stdin.take(), child.stdout.take(), child.stderr.take())
    else {
        return Err("bash pipes missing".to_owned());
    };
    Ok(Started {
        exit: exit_of(child),
        tree,
        stdin: Box::new(stdin),
        stdout: Box::new(stdout),
        stderr: Some(Box::new(stderr)),
        tty: None,
    })
}

/// Starts `cmd` on a new pseudo-terminal of `rows` by `cols`, as the leader of its session.
#[cfg(unix)]
fn on_terminal(mut cmd: Command, rows: u16, cols: u16) -> Result<Started, String> {
    let dup = |fd: &OwnedFd| fd.try_clone().map_err(|e| format!("dup pty: {e}"));
    let (master, slave) = sys::openpty(rows, cols).map_err(|e| format!("openpty: {e}"))?;
    let (out, err, reader, resizer) = (dup(&slave)?, dup(&slave)?, dup(&master)?, dup(&master)?);
    cmd.stdin(slave).stdout(out).stderr(err);
    sys::take_terminal(&mut cmd);
    let child = cmd.spawn().map_err(|e| format!("spawn bash: {e}"))?;
    drop(cmd);
    let tree = sys::Tree::hold(&child).map_err(|e| format!("hold bash: {e}"))?;
    let pty = |e| format!("pty: {e}");
    Ok(Started {
        exit: exit_of(child),
        tree,
        stdin: Box::new(pipe::Sender::from_owned_fd_unchecked(master).map_err(pty)?),
        stdout: Box::new(pipe::Receiver::from_owned_fd_unchecked(reader).map_err(pty)?),
        stderr: None,
        tty: Some(resizer),
    })
}

/// Starts `cmd` on a new pseudo console of `rows` by `cols`; its pipes are read and written on blocking threads.
#[cfg(windows)]
fn on_terminal(cmd: Command, rows: u16, cols: u16) -> Result<Started, String> {
    let on = sys::spawn_on_pty(cmd.as_std(), rows, cols)
        .map_err(|e| format!("spawn bash on a console: {e}"))?;
    Ok(Started {
        exit: Box::pin(async move { on.process.wait().await.map(msys_code) }),
        tree: on.tree,
        stdin: Box::new(tokio::fs::File::from_std(on.input)),
        stdout: Box::new(tokio::fs::File::from_std(on.output)),
        stderr: None,
        tty: Some(Arc::new(on.pty)),
    })
}

fn exit_of(mut child: Child) -> Exit {
    Box::pin(async move { child.wait().await.map(exit_code) })
}

/// The TERM of a terminal command: `term` when the box has its terminfo or `source` compiles
/// it into `~/.terminfo` as this user, else the default and a notice for the command's stderr.
#[cfg(unix)]
async fn terminal_type(home: &Path, term: &str, source: &str) -> (String, Option<String>) {
    if term.is_empty() {
        return (DEFAULT_TERM.to_owned(), None);
    }
    if known(home, term).await {
        return (term.to_owned(), None);
    }
    let why = if source.is_empty() {
        "the client sent no terminfo".to_owned()
    } else {
        match compile(home, source).await {
            Ok(()) if known(home, term).await => return (term.to_owned(), None),
            Ok(()) => "the terminfo sent does not define it".to_owned(),
            Err(e) => e,
        }
    };
    let notice = format!(
        "cocoonbox: no terminfo for {term} in the box ({why}); using TERM={DEFAULT_TERM}\r\n"
    );
    (DEFAULT_TERM.to_owned(), Some(notice))
}

/// The TERM of a terminal command: the default, as the pseudo console writes its own xterm sequences whatever the client is.
#[cfg(windows)]
async fn terminal_type(_home: &Path, _term: &str, _source: &str) -> (String, Option<String>) {
    (DEFAULT_TERM.to_owned(), None)
}

/// Whether the terminfo database the command reads, `~/.terminfo` first, has `term`.
#[cfg(unix)]
async fn known(home: &Path, term: &str) -> bool {
    let check = Command::new("infocmp")
        .args(["--", term])
        .env("HOME", home)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .kill_on_drop(true)
        .status();
    matches!(tokio::time::timeout(TERMINFO_TIMEOUT, check).await, Ok(Ok(s)) if s.success())
}

/// Compiles the terminfo `source` into `~/.terminfo` with `tic -x`.
#[cfg(unix)]
async fn compile(home: &Path, source: &str) -> Result<(), String> {
    let dir = home.join(".terminfo");
    tokio::fs::create_dir_all(&dir)
        .await
        .map_err(|e| format!("create {}: {e}", dir.display()))?;
    let mut child = Command::new("tic")
        .args(["-x", "-o"])
        .arg(&dir)
        .arg("-")
        .env("HOME", home)
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .kill_on_drop(true)
        .spawn()
        .map_err(|e| format!("tic: {e}"))?;
    let mut input = child.stdin.take().ok_or("tic: no stdin")?;
    let feed = async move {
        let _ = input.write_all(source.as_bytes()).await;
    };
    let ran = tokio::time::timeout(TERMINFO_TIMEOUT, async {
        tokio::join!(feed, child.wait_with_output()).1
    })
    .await
    .map_err(|_| format!("tic ran past {}s", TERMINFO_TIMEOUT.as_secs()))?
    .map_err(|e| format!("tic: {e}"))?;
    if ran.status.success() {
        return Ok(());
    }
    let err = String::from_utf8_lossy(&ran.stderr);
    let last = err.lines().rfind(|l| !l.trim().is_empty());
    Err(format!("tic: {}", last.unwrap_or("failed").trim()))
}

/// Applies the agent's frames to the command and returns only when the agent hangs up;
/// stdin after the command closed it is dropped, and so is a terminal's end of stdin.
async fn feed(mut rd: AgentRead, stdin: Writer, tty: Option<&Terminal>, tree: &sys::Tree) {
    let mut stdin = Some(stdin);
    loop {
        let Ok((tag, data)) = rpc::read_tagged(&mut rd).await else {
            return;
        };
        match tag {
            rpc::STDIN => {
                if let Some(pipe) = stdin.as_mut() {
                    tokio::select! {
                        r = pipe.write_all(&data) => if r.is_err() {
                            stdin = None;
                        },
                        () = hung_up(&rd) => return,
                    }
                }
            }
            rpc::EOF if tty.is_none() => stdin = None,
            rpc::SIGNAL => {
                if let Ok(signal) = <[u8; 4]>::try_from(data.as_slice()) {
                    let input = stdin.as_mut().filter(|_| tty.is_some());
                    interrupt(tree, input, i32::from_be_bytes(signal)).await;
                }
            }
            rpc::RESIZE => {
                if let (Some(tty), [r0, r1, c0, c1]) = (tty, data.as_slice()) {
                    let _ = resize(
                        tty,
                        u16::from_be_bytes([*r0, *r1]),
                        u16::from_be_bytes([*c0, *c1]),
                    );
                }
            }
            _ => {}
        }
    }
}

/// Sends `signal` to the command's process group.
#[cfg(unix)]
async fn interrupt(tree: &sys::Tree, _terminal_input: Option<&mut Writer>, signal: i32) {
    let _ = tree.signal(signal);
}

/// Types Ctrl-C into a terminal command's console, which turns it into CTRL_C_EVENT; any other signal ends the tree.
#[cfg(windows)]
async fn interrupt(tree: &sys::Tree, terminal_input: Option<&mut Writer>, signal: i32) {
    match terminal_input {
        Some(input) if signal == libc::SIGINT => {
            let _ = input.write_all(b"\x03").await;
        }
        _ => {
            let _ = tree.signal(signal);
        }
    }
}

#[cfg(unix)]
fn resize(tty: &Terminal, rows: u16, cols: u16) -> io::Result<()> {
    sys::set_window(tty, rows, cols)
}

#[cfg(windows)]
fn resize(tty: &Terminal, rows: u16, cols: u16) -> io::Result<()> {
    tty.resize(rows, cols)
}

/// Resolves once the agent closed its end, which frames queued behind a blocked stdin write would hide from a read.
#[cfg(unix)]
async fn hung_up(rd: &AgentRead) {
    loop {
        match rd.ready(Interest::READABLE).await {
            Ok(ready) if !ready.is_read_closed() => tokio::time::sleep(HANGUP_POLL).await,
            _ => return,
        }
    }
}

/// Resolves once the agent closed its end, which frames queued behind a blocked stdin write would hide from a read.
#[cfg(windows)]
async fn hung_up(rd: &AgentRead) {
    while !rd.pipe.closed() {
        tokio::time::sleep(HANGUP_POLL).await;
    }
}

/// Sends stdout and stderr as they arrive until both close; a terminal's end reads as a close.
async fn drain(
    mut stdout: Reader,
    stderr: Option<Reader>,
    wr: &mut AgentWrite,
) -> Result<(), String> {
    let mut out = vec![0u8; READ_CHUNK];
    let mut err = vec![0u8; READ_CHUNK];
    let (mut out_open, mut err_open) = (true, stderr.is_some());
    let mut stderr = stderr.unwrap_or_else(|| Box::new(tokio::io::empty()));
    while out_open || err_open {
        let (tag, read) = tokio::select! {
            r = stdout.read(&mut out), if out_open => (rpc::STDOUT, r),
            r = stderr.read(&mut err), if err_open => (rpc::STDERR, r),
        };
        let (buf, open) = if tag == rpc::STDOUT {
            (&out, &mut out_open)
        } else {
            (&err, &mut err_open)
        };
        match read {
            Ok(0) | Err(_) => *open = false,
            Ok(n) => rpc::write_tagged(wr, tag, &buf[..n]).await?,
        }
    }
    Ok(())
}

#[cfg(unix)]
fn exit_code(status: ExitStatus) -> i32 {
    status
        .code()
        .unwrap_or_else(|| 128 + status.signal().unwrap_or(0))
}

#[cfg(windows)]
fn exit_code(status: ExitStatus) -> i32 {
    msys_code(status.code().unwrap_or(-1))
}

/// The exit code of a Git Bash process as bash reports it: the MSYS runtime ends a process a signal killed
/// with `signal << 8`, which becomes `128 + signal`.
#[cfg(windows)]
fn msys_code(code: i32) -> i32 {
    match (code >> 8, code & 0xff) {
        (signal @ 1..=0x7f, 0) => 128 + signal,
        _ => code,
    }
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;
    use crate::config::{Automation, ToolhostConfig};
    use crate::toolhost::rpc::Client;

    const DEADLINE: Duration = Duration::from_secs(30);

    async fn toolhost(dir: &Path) -> Client {
        let socket = crate::testing::endpoint(dir, "toolhost.sock");
        let cfg = ToolhostConfig {
            socket: socket.clone(),
            socket_group: String::new(),
            workspace: dir.into(),
            chrome_bin: "false".into(),
            chrome_profile: dir.join("chrome"),
            cdp_port: 1,
            session_save: dir.join("session-save"),
            home: dir.into(),
            automation: Automation::Minimal,
            plugins: Vec::new(),
            shuttle_socket: None,
        };
        tokio::spawn(super::super::run(cfg));
        crate::testing::served(&socket).await;
        Client::new(socket)
    }

    fn tty(rows: u16, cols: u16) -> Option<Value> {
        Some(json!({"term": DEFAULT_TERM, "rows": rows, "cols": cols}))
    }

    /// Reads frames until the exit frame; stdout, stderr and the exit code.
    async fn finish(conn: &mut rpc::Stream) -> (String, String, i32) {
        let (mut out, mut err) = (Vec::new(), Vec::new());
        loop {
            let (tag, data) = rpc::read_tagged(conn).await.unwrap();
            match tag {
                rpc::STDOUT => out.extend(data),
                rpc::STDERR => err.extend(data),
                rpc::EXIT => {
                    let code = i32::from_be_bytes(data.try_into().unwrap());
                    return (
                        String::from_utf8_lossy(&out).into_owned(),
                        String::from_utf8_lossy(&err).into_owned(),
                        code,
                    );
                }
                other => panic!("unexpected tag {other}"),
            }
        }
    }

    #[tokio::test]
    async fn a_command_gets_its_stdin_and_returns_stdout_stderr_and_its_exit_code() {
        let dir = tempfile::tempdir().unwrap();
        let client = toolhost(dir.path()).await;
        let mut conn = client
            .exec(
                "echo out; echo err >&2; read line; echo got $line; exit 3",
                None,
            )
            .await
            .unwrap();
        rpc::write_tagged(&mut conn, rpc::STDIN, b"hi\n")
            .await
            .unwrap();
        rpc::write_tagged(&mut conn, rpc::EOF, &[]).await.unwrap();
        let (out, err, code) = tokio::time::timeout(DEADLINE, finish(&mut conn))
            .await
            .unwrap();
        assert_eq!(
            (out.as_str(), err.as_str(), code),
            ("out\ngot hi\n", "err\n", 3)
        );
    }

    #[tokio::test]
    async fn a_terminal_command_runs_at_the_size_asked_and_follows_a_resize() {
        let dir = tempfile::tempdir().unwrap();
        let client = toolhost(dir.path()).await;
        let mut conn = client
            .exec("stty size; sleep 2; stty size", tty(24, 80))
            .await
            .unwrap();
        tokio::time::sleep(Duration::from_millis(700)).await;
        let size = [40u16.to_be_bytes(), 120u16.to_be_bytes()].concat();
        rpc::write_tagged(&mut conn, rpc::RESIZE, &size)
            .await
            .unwrap();
        let (out, _, code) = tokio::time::timeout(DEADLINE, finish(&mut conn))
            .await
            .unwrap();
        assert!(out.contains("24 80") && out.contains("40 120"), "{out:?}");
        assert_eq!(code, 0, "{out:?}");
    }

    #[tokio::test]
    async fn an_interrupt_ends_a_terminal_command_with_130_and_a_term_ends_a_piped_one_with_143() {
        let dir = tempfile::tempdir().unwrap();
        let client = toolhost(dir.path()).await;
        for (tty, signal, want) in [(tty(24, 80), libc::SIGINT, 130), (None, libc::SIGTERM, 143)] {
            let mut conn = client.exec("sleep 30", tty).await.unwrap();
            tokio::time::sleep(Duration::from_millis(700)).await;
            rpc::write_tagged(&mut conn, rpc::SIGNAL, &signal.to_be_bytes())
                .await
                .unwrap();
            let (out, _, code) = tokio::time::timeout(Duration::from_secs(10), finish(&mut conn))
                .await
                .unwrap();
            assert_eq!(code, want, "signal {signal}: {out:?}");
        }
    }

    #[tokio::test]
    async fn hanging_up_with_stdin_queued_for_a_command_that_never_reads_it_ends_the_command() {
        let dir = tempfile::tempdir().unwrap();
        let client = toolhost(dir.path()).await;
        let conn = client
            .exec("(sleep 2; touch late) & sleep 30", None)
            .await
            .unwrap();
        let (rd, mut wr) = tokio::io::split(conn);
        let writer = tokio::spawn(async move {
            let block = vec![b'x'; 1 << 20];
            for _ in 0..16 {
                if rpc::write_tagged(&mut wr, rpc::STDIN, &block)
                    .await
                    .is_err()
                {
                    return;
                }
            }
        });
        tokio::time::sleep(Duration::from_millis(500)).await;
        writer.abort();
        let _ = writer.await;
        drop(rd);
        tokio::time::sleep(Duration::from_secs(4)).await;
        assert!(
            !dir.path().join("late").exists(),
            "the command outlived the hang-up"
        );
    }

    #[cfg(windows)]
    #[test]
    fn a_git_bash_exit_code_a_signal_gave_reads_as_bash_reports_it() {
        assert_eq!(msys_code(2 << 8), 130);
        assert_eq!(msys_code(15 << 8), 143);
        for code in [0, 3, 143, 0x1_0300, -1_073_741_510] {
            assert_eq!(msys_code(code), code);
        }
    }

    #[tokio::test]
    async fn hanging_up_on_a_terminal_command_mid_output_leaves_the_toolhost_serving() {
        let dir = tempfile::tempdir().unwrap();
        let client = toolhost(dir.path()).await;
        let mut conn = client
            .exec("while :; do echo 0123456789abcdef; done", tty(24, 80))
            .await
            .unwrap();
        let (tag, _) = rpc::read_tagged(&mut conn).await.unwrap();
        assert_eq!(tag, rpc::STDOUT);
        drop(conn);
        let mut next = client.exec("echo ok", None).await.unwrap();
        let (out, _, code) = tokio::time::timeout(DEADLINE, finish(&mut next))
            .await
            .unwrap();
        assert_eq!((out.as_str(), code), ("ok\n", 0));
    }

    #[tokio::test]
    async fn hanging_up_ends_the_command_and_everything_it_started() {
        let dir = tempfile::tempdir().unwrap();
        let client = toolhost(dir.path()).await;
        let mut conn = client
            .exec("(sleep 2; touch late) & echo started; sleep 30", None)
            .await
            .unwrap();
        let (tag, data) = rpc::read_tagged(&mut conn).await.unwrap();
        assert_eq!(
            (tag, data.as_slice()),
            (rpc::STDOUT, b"started\n".as_slice())
        );
        drop(conn);
        tokio::time::sleep(Duration::from_secs(4)).await;
        assert!(
            !dir.path().join("late").exists(),
            "the background job outlived the hang-up"
        );
    }
}
