//! Toolhost wire format: one length-prefixed JSON request and response per connection;
//! state transfers add raw data frames between them, and `exec` streams data frames in
//! and tagged output frames out.

use std::path::{Path, PathBuf};

use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::net::UnixStream;
use zeroize::Zeroize;

use super::state::Stamp;

/// Output tag of a command's stdout data.
pub const STDOUT: u8 = 1;
/// Output tag of a command's stderr data.
pub const STDERR: u8 = 2;
/// Output tag of a command's exit code, a big-endian i32; the last frame.
pub const EXIT: u8 = 3;

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
        let mut stream = self.open(call).await?;
        read_frame(&mut stream).await
    }

    /// Streams a file as `user`; the stamp comes first, [`FileReader::finish`] returns the one taken after.
    pub async fn read_file(
        &self,
        path: &Path,
        sqlite_backup: bool,
    ) -> Result<(Stamp, FileReader), String> {
        let call = Call {
            agent_id: String::new(),
            tool: "state_read".to_owned(),
            args: json!({"path": path, "sqlite_backup": sqlite_backup}),
        };
        let mut stream = self.open(&call).await?;
        let stamp: Stamp = read_frame(&mut stream).await?;
        if let Some(e) = &stamp.error {
            return Err(e.clone());
        }
        Ok((stamp, FileReader { stream }))
    }

    /// Creates a file as `user` from data sent through the returned writer.
    pub async fn write_file(
        &self,
        path: &Path,
        mode: u32,
        mtime_ms: i64,
    ) -> Result<FileWriter, String> {
        let call = Call {
            agent_id: String::new(),
            tool: "state_write".to_owned(),
            args: json!({"path": path, "mode": mode, "mtime_ms": mtime_ms}),
        };
        Ok(FileWriter {
            stream: self.open(&call).await?,
        })
    }

    /// Starts `command` as `user`; the connection then takes its stdin as data frames,
    /// ended by an empty one, and yields its output as [`read_output`] frames. Closing
    /// the connection kills the command.
    pub async fn exec(&self, command: &str) -> Result<UnixStream, String> {
        let call = Call {
            agent_id: String::new(),
            tool: "exec".to_owned(),
            args: json!({"command": command}),
        };
        let mut stream = self.open(&call).await?;
        let started: Output = read_frame(&mut stream).await?;
        if started.is_error {
            Err(started.text)
        } else {
            Ok(stream)
        }
    }

    /// Sends `call` and hands back the connection for a streamed exchange.
    pub async fn open(&self, call: &Call) -> Result<UnixStream, String> {
        let mut stream = UnixStream::connect(&self.socket)
            .await
            .map_err(|e| format!("connect toolhost {}: {e}", self.socket.display()))?;
        write_frame(&mut stream, call).await?;
        Ok(stream)
    }
}

/// Data of a file being read by the toolhost.
pub struct FileReader {
    stream: UnixStream,
}

impl FileReader {
    /// Next block of the file; `None` at the end.
    pub async fn next(&mut self) -> Result<Option<Vec<u8>>, String> {
        read_data(&mut self.stream).await
    }

    /// The stamp taken after the whole file was sent.
    pub async fn finish(mut self) -> Result<Stamp, String> {
        read_frame(&mut self.stream).await
    }
}

/// A file being written by the toolhost.
pub struct FileWriter {
    stream: UnixStream,
}

impl FileWriter {
    /// Appends `data`.
    pub async fn send(&mut self, data: &[u8]) -> Result<(), String> {
        if data.is_empty() {
            return Ok(());
        }
        write_data(&mut self.stream, data).await
    }

    /// Ends the file and waits for the toolhost to set mode and mtime.
    pub async fn finish(mut self) -> Result<(), String> {
        write_data(&mut self.stream, &[]).await?;
        let out: Output = read_frame(&mut self.stream).await?;
        if out.is_error { Err(out.text) } else { Ok(()) }
    }
}

/// Writes `msg` as a big-endian u32 length followed by JSON.
pub async fn write_frame<W, T>(w: &mut W, msg: &T) -> Result<(), String>
where
    W: AsyncWrite + Unpin,
    T: Serialize,
{
    let mut body = serde_json::to_vec(msg).map_err(|e| format!("encode frame: {e}"))?;
    let len = u32::try_from(body.len()).map_err(|_| "frame too large".to_owned())?;
    let written = async {
        w.write_all(&len.to_be_bytes()).await?;
        w.write_all(&body).await?;
        w.flush().await
    }
    .await;
    body.zeroize();
    written.map_err(|e| format!("write frame: {e}"))
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
    let msg = serde_json::from_slice(&body);
    body.zeroize();
    msg.map_err(|e| format!("decode frame: {e}"))
}

/// Zeroes the string fields of `args`, which may carry a credential.
pub fn wipe(args: &mut Value) {
    if let Some(fields) = args.as_object_mut() {
        for v in fields.values_mut() {
            if let Value::String(s) = v {
                s.zeroize();
            }
        }
    }
}

/// Writes one raw data frame; an empty slice ends the data.
pub async fn write_data<W: AsyncWrite + Unpin>(w: &mut W, data: &[u8]) -> Result<(), String> {
    let len = u32::try_from(data.len()).map_err(|_| "data frame too large".to_owned())?;
    w.write_all(&len.to_be_bytes())
        .await
        .map_err(|e| format!("write data: {e}"))?;
    w.write_all(data)
        .await
        .map_err(|e| format!("write data: {e}"))
}

/// Reads one raw data frame; `None` marks the end of the data.
pub async fn read_data<R: AsyncRead + Unpin>(r: &mut R) -> Result<Option<Vec<u8>>, String> {
    let mut len = [0u8; 4];
    r.read_exact(&mut len)
        .await
        .map_err(|e| format!("read data length: {e}"))?;
    let len = u32::from_be_bytes(len) as usize;
    if len == 0 {
        return Ok(None);
    }
    if len > MAX_FRAME {
        return Err(format!("data frame of {len} bytes exceeds {MAX_FRAME}"));
    }
    let mut data = vec![0u8; len];
    r.read_exact(&mut data)
        .await
        .map_err(|e| format!("read data: {e}"))?;
    Ok(Some(data))
}

/// Writes one output frame of a command: the tag, a big-endian u32 length and the data.
pub async fn write_output<W: AsyncWrite + Unpin>(
    w: &mut W,
    tag: u8,
    data: &[u8],
) -> Result<(), String> {
    let len = u32::try_from(data.len()).map_err(|_| "output frame too large".to_owned())?;
    let mut head = [tag, 0, 0, 0, 0];
    head[1..].copy_from_slice(&len.to_be_bytes());
    w.write_all(&head)
        .await
        .map_err(|e| format!("write output: {e}"))?;
    w.write_all(data)
        .await
        .map_err(|e| format!("write output: {e}"))
}

/// Reads one frame written by [`write_output`].
pub async fn read_output<R: AsyncRead + Unpin>(r: &mut R) -> Result<(u8, Vec<u8>), String> {
    let mut head = [0u8; 5];
    r.read_exact(&mut head)
        .await
        .map_err(|e| format!("read output header: {e}"))?;
    let len = u32::from_be_bytes([head[1], head[2], head[3], head[4]]) as usize;
    if len > MAX_FRAME {
        return Err(format!("output frame of {len} bytes exceeds {MAX_FRAME}"));
    }
    let mut data = vec![0u8; len];
    r.read_exact(&mut data)
        .await
        .map_err(|e| format!("read output: {e}"))?;
    Ok((head[0], data))
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
