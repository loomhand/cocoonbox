//! Session.Exec: relays a command's stdin, stdout and stderr between the stream and the toolhost.

use bytes::Bytes;
use serde_json::json;
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tokio_util::sync::CancellationToken;
use tonic::{Status, Streaming};

use crate::pb::{
    ExecClientFrame, ExecServerFrame, ExecSignal, exec_client_frame::Body as In,
    exec_server_frame::Body as Out,
};
use crate::toolhost::rpc;

const OUT_FRAMES: usize = 16;
const MAX_TERMINFO: usize = 256 << 10;

/// Stream of frames sent back to the client.
pub type ServerStream = ReceiverStream<Result<ExecServerFrame, Status>>;

/// Waits for the `start` frame, starts the command through the toolhost and pumps both ways
/// until it exits. The client going away or the agent stopping closes the toolhost connection,
/// which kills the command.
pub async fn relay(
    tools: &rpc::Client,
    mut inbound: Streaming<ExecClientFrame>,
    stop: CancellationToken,
) -> Result<ServerStream, Status> {
    let Some(ExecClientFrame {
        body: Some(In::Start(start)),
    }) = inbound.message().await?
    else {
        return Err(Status::invalid_argument(
            "the first Exec frame must be start",
        ));
    };
    if start
        .tty
        .as_ref()
        .is_some_and(|tty| tty.terminfo.len() > MAX_TERMINFO)
    {
        return Err(Status::invalid_argument(format!(
            "the terminfo source exceeds {MAX_TERMINFO} bytes"
        )));
    }
    let tty = start.tty.map(|tty| {
        let window = tty.window.unwrap_or_default();
        json!({"term": tty.term, "terminfo": tty.terminfo, "rows": window.rows, "cols": window.cols})
    });
    let (mut rd, mut wr) = tokio::io::split(
        tools
            .exec(&start.command, tty)
            .await
            .map_err(Status::unavailable)?,
    );
    let (tx, rx) = mpsc::channel(OUT_FRAMES);
    let done = CancellationToken::new();
    let feeding = done.clone();
    // detached pumps: the input side keeps the connection open until the output side is done
    tokio::spawn(async move {
        let mut open = true;
        loop {
            let frame = tokio::select! {
                f = inbound.message(), if open => f,
                () = feeding.cancelled() => return,
            };
            let (tag, data) = match frame {
                Ok(Some(ExecClientFrame { body: Some(body) })) => match to_toolhost(body) {
                    Some(framed) => framed,
                    None => continue,
                },
                Ok(Some(_)) => continue,
                Ok(None) => {
                    open = false;
                    (rpc::EOF, Bytes::new())
                }
                Err(_) => {
                    feeding.cancel();
                    return;
                }
            };
            let written = tokio::select! {
                r = rpc::write_tagged(&mut wr, tag, &data) => r,
                () = feeding.cancelled() => return,
            };
            if written.is_err() {
                return;
            }
        }
    });
    tokio::spawn(async move {
        loop {
            let next = tokio::select! {
                r = rpc::read_tagged(&mut rd) => r,
                () = tx.closed() => break,
                () = done.cancelled() => break,
                () = stop.cancelled() => break,
            };
            let (body, last) = match next {
                Ok((rpc::STDOUT, data)) => (Out::Stdout(data.into()), false),
                Ok((rpc::STDERR, data)) => (Out::Stderr(data.into()), false),
                Ok((rpc::EXIT, data)) => match <[u8; 4]>::try_from(data.as_slice()) {
                    Ok(code) => (Out::ExitCode(i32::from_be_bytes(code)), true),
                    Err(_) => {
                        let _ = tx.send(Err(Status::internal("bad exit frame"))).await;
                        break;
                    }
                },
                Ok((tag, _)) => {
                    let msg = format!("unknown toolhost output tag {tag}");
                    let _ = tx.send(Err(Status::internal(msg))).await;
                    break;
                }
                Err(e) => {
                    let _ = tx.send(Err(Status::unavailable(e))).await;
                    break;
                }
            };
            let sent = tx.send(Ok(ExecServerFrame { body: Some(body) })).await;
            if sent.is_err() || last {
                break;
            }
        }
        done.cancel();
    });
    Ok(ReceiverStream::new(rx))
}

/// The toolhost frame for one client frame; `None` for a frame it does not take.
fn to_toolhost(body: In) -> Option<(u8, Bytes)> {
    match body {
        In::Stdin(data) if !data.is_empty() => Some((rpc::STDIN, data)),
        In::Eof(_) => Some((rpc::EOF, Bytes::new())),
        In::Signal(signal) => {
            let signal = match ExecSignal::try_from(signal).ok()? {
                ExecSignal::Int => libc::SIGINT,
                ExecSignal::Term => libc::SIGTERM,
                ExecSignal::Unspecified => return None,
            };
            Some((rpc::SIGNAL, Bytes::copy_from_slice(&signal.to_be_bytes())))
        }
        In::Resize(window) => {
            let clamp = |n: u32| u16::try_from(n).unwrap_or(u16::MAX).to_be_bytes();
            Some((
                rpc::RESIZE,
                [clamp(window.rows), clamp(window.cols)].concat().into(),
            ))
        }
        In::Start(_) | In::Stdin(_) => None,
    }
}
