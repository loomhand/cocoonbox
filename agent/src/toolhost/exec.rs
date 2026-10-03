//! `exec`: one command as `user`, its stdin, stdout and stderr carried over the toolhost connection.

use std::os::unix::process::ExitStatusExt;
use std::path::Path;
use std::process::{ExitStatus, Stdio};
use std::time::Duration;

use serde_json::Value;
use tokio::io::{AsyncReadExt, AsyncWriteExt, Interest};
use tokio::net::unix::{OwnedReadHalf, OwnedWriteHalf};
use tokio::process::{Child, ChildStderr, ChildStdin, ChildStdout};

use super::rpc::{self, Output};
use super::shell;
use crate::sys;

const READ_CHUNK: usize = 64 << 10;
const HANGUP_POLL: Duration = Duration::from_millis(200);

/// Runs `args.command` with bash in `cwd` until it exits; the agent closing the connection
/// first kills its process group.
pub async fn run(cwd: &Path, args: &Value, rd: OwnedReadHalf, mut wr: OwnedWriteHalf) {
    let mut child = match spawn(cwd, args) {
        Ok(child) => child,
        Err(e) => {
            let _ = rpc::write_frame(&mut wr, &Output::error(e)).await;
            return;
        }
    };
    let pid = child.id();
    let (Some(stdin), Some(stdout), Some(stderr)) =
        (child.stdin.take(), child.stdout.take(), child.stderr.take())
    else {
        let _ = rpc::write_frame(&mut wr, &Output::error("bash pipes missing")).await;
        return;
    };
    if rpc::write_frame(&mut wr, &Output::text("started"))
        .await
        .is_err()
    {
        kill(pid);
        return;
    }
    let output = async {
        drain(stdout, stderr, &mut wr).await?;
        let code = exit_code(
            child
                .wait()
                .await
                .map_err(|e| format!("wait for bash: {e}"))?,
        );
        rpc::write_output(&mut wr, rpc::EXIT, &code.to_be_bytes()).await
    };
    tokio::select! {
        () = feed(rd, stdin) => kill(pid),
        r = output => if let Err(e) = r {
            eprintln!("toolhost: exec: {e}");
            kill(pid);
        },
    }
}

fn spawn(cwd: &Path, args: &Value) -> Result<Child, String> {
    let command = args["command"]
        .as_str()
        .ok_or("exec needs a command string")?;
    shell::bash(cwd)
        .args(["-c", command])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|e| format!("spawn bash: {e}"))
}

/// Copies stdin frames to the command and returns only when the agent hangs up;
/// data after the command closed its stdin is dropped.
async fn feed(mut rd: OwnedReadHalf, stdin: ChildStdin) {
    let mut stdin = Some(stdin);
    loop {
        match rpc::read_data(&mut rd).await {
            Ok(Some(data)) => {
                if let Some(pipe) = stdin.as_mut() {
                    tokio::select! {
                        r = pipe.write_all(&data) => if r.is_err() {
                            stdin = None;
                        },
                        () = hung_up(&rd) => return,
                    }
                }
            }
            Ok(None) => stdin = None,
            Err(_) => return,
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

/// Sends stdout and stderr as they arrive until both close.
async fn drain(
    mut stdout: ChildStdout,
    mut stderr: ChildStderr,
    wr: &mut OwnedWriteHalf,
) -> Result<(), String> {
    let mut out = vec![0u8; READ_CHUNK];
    let mut err = vec![0u8; READ_CHUNK];
    let (mut out_open, mut err_open) = (true, true);
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
            Ok(n) => rpc::write_output(wr, tag, &buf[..n]).await?,
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
