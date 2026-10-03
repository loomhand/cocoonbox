//! `exec`: one command as `user`, its stdin, stdout and stderr carried over the toolhost connection.

use std::os::fd::OwnedFd;
use std::os::unix::process::ExitStatusExt;
use std::path::Path;
use std::process::{ExitStatus, Stdio};
use std::time::Duration;

use serde_json::Value;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt, Interest};
use tokio::net::unix::{OwnedReadHalf, OwnedWriteHalf, pipe};
use tokio::process::{Child, Command};

use super::rpc::{self, Output};
use super::shell;
use crate::sys;

const READ_CHUNK: usize = 64 << 10;
const HANGUP_POLL: Duration = Duration::from_millis(200);
const DEFAULT_TERM: &str = "xterm-256color";
const TERMINFO_TIMEOUT: Duration = Duration::from_secs(5);

type Reader = Box<dyn AsyncRead + Send + Unpin>;
type Writer = Box<dyn AsyncWrite + Send + Unpin>;

/// A started command: its stdin, its output, and the terminal it runs on, if any.
struct Started {
    child: Child,
    stdin: Writer,
    stdout: Reader,
    stderr: Option<Reader>,
    tty: Option<OwnedFd>,
}

/// Runs `args.command` with a login bash in `cwd` until it exits; the agent closing the
/// connection first kills its process group.
pub async fn run(cwd: &Path, home: &Path, args: &Value, rd: OwnedReadHalf, mut wr: OwnedWriteHalf) {
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
        mut child,
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
    let pid = child.id();
    let mut started = rpc::write_frame(&mut wr, &Output::text("started")).await;
    if let (Ok(()), Some(notice)) = (&started, notice) {
        started = rpc::write_tagged(&mut wr, rpc::STDERR, notice.as_bytes()).await;
    }
    if started.is_err() {
        kill(pid);
        return;
    }
    let output = async {
        drain(stdout, stderr, &mut wr).await?;
        let status = child
            .wait()
            .await
            .map_err(|e| format!("wait for bash: {e}"))?;
        rpc::write_tagged(&mut wr, rpc::EXIT, &exit_code(status).to_be_bytes()).await
    };
    tokio::select! {
        () = feed(rd, stdin, tty, pid) => kill(pid),
        r = output => if let Err(e) = r {
            eprintln!("toolhost: exec: {e}");
            kill(pid);
        },
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
        let mut child = cmd
            .process_group(0)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .map_err(|e| format!("spawn bash: {e}"))?;
        let (Some(stdin), Some(stdout), Some(stderr)) =
            (child.stdin.take(), child.stdout.take(), child.stderr.take())
        else {
            return Err("bash pipes missing".to_owned());
        };
        return Ok(Started {
            child,
            stdin: Box::new(stdin),
            stdout: Box::new(stdout),
            stderr: Some(Box::new(stderr)),
            tty: None,
        });
    }
    let size = |key: &str| u16::try_from(tty[key].as_u64().unwrap_or(0)).unwrap_or(u16::MAX);
    let dup = |fd: &OwnedFd| fd.try_clone().map_err(|e| format!("dup pty: {e}"));
    let (master, slave) =
        sys::openpty(size("rows"), size("cols")).map_err(|e| format!("openpty: {e}"))?;
    let (out, err, reader, resizer) = (dup(&slave)?, dup(&slave)?, dup(&master)?, dup(&master)?);
    cmd.env("TERM", term)
        .env("HOME", home)
        .env_remove("PAGER")
        .env_remove("GIT_PAGER")
        .env_remove("NO_COLOR")
        .stdin(slave)
        .stdout(out)
        .stderr(err);
    sys::take_terminal(&mut cmd);
    let child = cmd.spawn().map_err(|e| format!("spawn bash: {e}"))?;
    drop(cmd);
    let pty = |e| format!("pty: {e}");
    Ok(Started {
        child,
        stdin: Box::new(pipe::Sender::from_owned_fd_unchecked(master).map_err(pty)?),
        stdout: Box::new(pipe::Receiver::from_owned_fd_unchecked(reader).map_err(pty)?),
        stderr: None,
        tty: Some(resizer),
    })
}

/// The TERM of a terminal command: `term` when the box has its terminfo or `source` compiles
/// it into `~/.terminfo` as this user, else the default and a notice for the command's stderr.
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

/// Whether the terminfo database the command reads, `~/.terminfo` first, has `term`.
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
async fn feed(mut rd: OwnedReadHalf, stdin: Writer, tty: Option<OwnedFd>, pid: Option<u32>) {
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
                if let (Some(pid), Ok(signal)) = (pid, <[u8; 4]>::try_from(data.as_slice())) {
                    let _ = sys::signal_group(pid, i32::from_be_bytes(signal));
                }
            }
            rpc::RESIZE => {
                if let (Some(fd), [r0, r1, c0, c1]) = (&tty, data.as_slice()) {
                    let _ = sys::set_window(
                        fd,
                        u16::from_be_bytes([*r0, *r1]),
                        u16::from_be_bytes([*c0, *c1]),
                    );
                }
            }
            _ => {}
        }
    }
}

/// Resolves once the agent closed its end, which frames queued behind a blocked stdin write would hide from a read.
async fn hung_up(rd: &OwnedReadHalf) {
    loop {
        match rd.ready(Interest::READABLE).await {
            Ok(ready) if !ready.is_read_closed() => tokio::time::sleep(HANGUP_POLL).await,
            _ => return,
        }
    }
}

/// Sends stdout and stderr as they arrive until both close; a terminal's end reads as a close.
async fn drain(
    mut stdout: Reader,
    stderr: Option<Reader>,
    wr: &mut OwnedWriteHalf,
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

fn exit_code(status: ExitStatus) -> i32 {
    status
        .code()
        .unwrap_or_else(|| 128 + status.signal().unwrap_or(0))
}

fn kill(pid: Option<u32>) {
    if let Some(pid) = pid {
        let _ = sys::kill_group(pid);
    }
}
