//! Raw RFB relay between a Screen stream and the box's loopback VNC server.

use bytes::BytesMut;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tonic::{Status, Streaming};

use crate::pb::{ScreenClientFrame, ScreenServerFrame, screen_client_frame};

const READ_CHUNK: usize = 64 << 10;
const OUT_FRAMES: usize = 16;

/// Stream of frames sent back to the client.
pub type ServerStream = ReceiverStream<Result<ScreenServerFrame, Status>>;

/// Waits for the `open` frame, dials `vnc_addr`, and pumps bytes both ways until either side closes.
pub async fn relay(
    vnc_addr: &str,
    mut inbound: Streaming<ScreenClientFrame>,
) -> Result<ServerStream, Status> {
    match inbound.message().await? {
        Some(ScreenClientFrame {
            body: Some(screen_client_frame::Body::Open(_)),
        }) => {}
        _ => {
            return Err(Status::invalid_argument(
                "the first Screen frame must be open",
            ));
        }
    }
    let tcp = TcpStream::connect(vnc_addr)
        .await
        .map_err(|e| Status::unavailable(format!("connect vnc {vnc_addr}: {e}")))?;
    let _ = tcp.set_nodelay(true);
    let (mut rd, mut wr) = tcp.into_split();
    let (tx, rx) = mpsc::channel(OUT_FRAMES);
    // detached pumps: each ends when its peer closes
    tokio::spawn(async move {
        while let Ok(Some(frame)) = inbound.message().await {
            if let Some(screen_client_frame::Body::Data(data)) = frame.body
                && wr.write_all(&data).await.is_err()
            {
                break;
            }
        }
        let _ = wr.shutdown().await;
    });
    tokio::spawn(async move {
        let mut buf = BytesMut::with_capacity(READ_CHUNK);
        loop {
            buf.reserve(READ_CHUNK);
            match rd.read_buf(&mut buf).await {
                Ok(0) | Err(_) => break,
                Ok(_) => {
                    let data = buf.split().freeze();
                    if tx.send(Ok(ScreenServerFrame { data })).await.is_err() {
                        break;
                    }
                }
            }
        }
    });
    Ok(ReceiverStream::new(rx))
}
