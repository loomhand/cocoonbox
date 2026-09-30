//! Toolhost wire format: one length-prefixed JSON request and response per connection.

use std::path::PathBuf;

use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::net::UnixStream;

const MAX_FRAME: usize = 64 << 20;

/// A tool invocation forwarded from the agent loop.
#[derive(Debug, Deserialize, Serialize)]
pub struct Call {
    pub agent_id: String,
    pub tool: String,
    #[serde(default)]
    pub args: Value,
}

/// A tool result; `image_b64` carries a screenshot when the tool produced one.
#[derive(Debug, Default, Deserialize, Serialize)]
pub struct Output {
    pub text: String,
    #[serde(default)]
    pub is_error: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub image_b64: Option<String>,
}

impl Output {
    /// A successful text result.
    pub fn text(text: impl Into<String>) -> Self {
        Self {
            text: text.into(),
            ..Self::default()
        }
    }

    /// A failed result carrying `text` as the reason.
    pub fn error(text: impl Into<String>) -> Self {
        Self {
            text: text.into(),
            is_error: true,
            image_b64: None,
        }
    }
}

/// Client side of the toolhost socket.
#[derive(Clone, Debug)]
pub struct Client {
    socket: PathBuf,
}

impl Client {
    /// Creates a client for the socket at `socket`.
    pub fn new(socket: PathBuf) -> Self {
        Self { socket }
    }

    /// Runs one call; dropping the future closes the connection, which cancels the tool.
    pub async fn call(&self, call: &Call) -> Result<Output, String> {
        let mut stream = UnixStream::connect(&self.socket)
            .await
            .map_err(|e| format!("connect toolhost {}: {e}", self.socket.display()))?;
        write_frame(&mut stream, call).await?;
        read_frame(&mut stream).await
    }
}

/// Writes `msg` as a big-endian u32 length followed by JSON.
pub async fn write_frame<W, T>(w: &mut W, msg: &T) -> Result<(), String>
where
    W: AsyncWrite + Unpin,
    T: Serialize,
{
    let body = serde_json::to_vec(msg).map_err(|e| format!("encode frame: {e}"))?;
    let len = u32::try_from(body.len()).map_err(|_| "frame too large".to_owned())?;
    w.write_all(&len.to_be_bytes())
        .await
        .map_err(|e| format!("write frame: {e}"))?;
    w.write_all(&body)
        .await
        .map_err(|e| format!("write frame: {e}"))?;
    w.flush().await.map_err(|e| format!("flush frame: {e}"))
}

/// Reads one frame written by [`write_frame`].
pub async fn read_frame<R, T>(r: &mut R) -> Result<T, String>
where
    R: AsyncRead + Unpin,
    T: DeserializeOwned,
{
    let mut len = [0u8; 4];
    r.read_exact(&mut len)
        .await
        .map_err(|e| format!("read frame length: {e}"))?;
    let len = u32::from_be_bytes(len) as usize;
    if len > MAX_FRAME {
        return Err(format!("frame of {len} bytes exceeds {MAX_FRAME}"));
    }
    let mut body = vec![0u8; len];
    r.read_exact(&mut body)
        .await
        .map_err(|e| format!("read frame body: {e}"))?;
    serde_json::from_slice(&body).map_err(|e| format!("decode frame: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn frame_round_trip() {
        let (mut a, mut b) = tokio::io::duplex(1024);
        let call = Call {
            agent_id: "ag".into(),
            tool: "shell".into(),
            args: serde_json::json!({"command": "ls"}),
        };
        write_frame(&mut a, &call).await.unwrap();
        let got: Call = read_frame(&mut b).await.unwrap();
        assert_eq!(got.tool, "shell");
        assert_eq!(got.args["command"], "ls");
    }

    #[tokio::test]
    async fn oversized_frame_is_rejected() {
        let (mut a, mut b) = tokio::io::duplex(64);
        a.write_all(&u32::MAX.to_be_bytes()).await.unwrap();
        assert!(read_frame::<_, Output>(&mut b).await.is_err());
    }
}
