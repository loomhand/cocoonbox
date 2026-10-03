//! Session.Exec: relays a command's stdin, stdout and stderr between the stream and the toolhost.

use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tokio_util::sync::CancellationToken;
use tonic::{Status, Streaming};

use crate::pb::{ExecClientFrame, ExecServerFrame, exec_client_frame, exec_server_frame};
use crate::toolhost::rpc;

const OUT_FRAMES: usize = 16;

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
    let command = match inbound.message().await? {
        Some(ExecClientFrame {
            body: Some(exec_client_frame::Body::Start(start)),
        }) => start.command,
        _ => {
            return Err(Status::invalid_argument(
                "the first Exec frame must be start",
            ));
        }
    };
    let (mut rd, mut wr) = tools
        .exec(&command)
        .await
        .map_err(Status::unavailable)?
        .into_split();
    let (tx, rx) = mpsc::channel(OUT_FRAMES);
    let done = CancellationToken::new();
    let feeding = done.clone();
    // detached pumps: the stdin side keeps the connection open until the output side is done
    tokio::spawn(async move {
        loop {
            let frame = tokio::select! {
                f = inbound.message() => f,
                () = feeding.cancelled() => return,
            };
            let data = match frame {
                Ok(Some(ExecClientFrame {
                    body: Some(exec_client_frame::Body::Stdin(data)),
                })) if !data.is_empty() => data,
                Ok(Some(_)) => continue,
                Ok(None) => break,
                Err(_) => {
                    feeding.cancel();
                    return;
                }
            };
            let written = tokio::select! {
                r = rpc::write_data(&mut wr, &data) => r,
                () = feeding.cancelled() => return,
            };
            if written.is_err() {
                return;
            }
        }
        if rpc::write_data(&mut wr, &[]).await.is_ok() {
            feeding.cancelled().await;
        }
    });
    tokio::spawn(async move {
        loop {
            let next = tokio::select! {
                r = rpc::read_output(&mut rd) => r,
                () = tx.closed() => break,
                () = done.cancelled() => break,
                () = stop.cancelled() => break,
            };
            let (body, last) = match next {
                Ok((rpc::STDOUT, data)) => (exec_server_frame::Body::Stdout(data.into()), false),
                Ok((rpc::STDERR, data)) => (exec_server_frame::Body::Stderr(data.into()), false),
                Ok((rpc::EXIT, data)) => match <[u8; 4]>::try_from(data.as_slice()) {
                    Ok(code) => (
                        exec_server_frame::Body::ExitCode(i32::from_be_bytes(code)),
                        true,
                    ),
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
