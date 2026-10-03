//! `exec`: one command as `user`, its stdin, stdout and stderr carried over the toolhost connection.

use std::os::fd::OwnedFd;
use std::os::unix::process::ExitStatusExt;
use std::path::Path;
use std::process::{ExitStatus, Stdio};
use std::time::Duration;

use serde_json::Value;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt, Interest};
use tokio::net::unix::{OwnedReadHalf, OwnedWriteHalf, pipe};
use tokio::process::Child;

use super::rpc::{self, Output};
use super::shell;
use crate::sys;

const READ_CHUNK: usize = 64 << 10;
const HANGUP_POLL: Duration = Duration::from_millis(200);
const DEFAULT_TERM: &str = "xterm-256color";

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
pub async fn run(cwd: &Path, args: &Value, rd: OwnedReadHalf, mut wr: OwnedWriteHalf) {
    let Started {
        mut child,
        stdin,
        stdout,
        stderr,
        tty,
    } = match spawn(cwd, args) {
        Ok(started) => started,
        Err(e) => {
            let _ = rpc::write_frame(&mut wr, &Output::error(e)).await;
            return;
        }
    };
    let pid = child.id();
    if rpc::write_frame(&mut wr, &Output::text("started"))
        .await
        .is_err()
    {
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

fn spawn(cwd: &Path, args: &Value) -> Result<Started, String> {
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
    cmd.env(
        "TERM",
        tty["term"]
            .as_str()
            .filter(|t| !t.is_empty())
            .unwrap_or(DEFAULT_TERM),
    )
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
