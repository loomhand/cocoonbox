//! boxgate: a test harness that drives a box's serve directly, as the edge and the api would, for gates without them.

use std::collections::HashMap;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::time::Duration;

use base64::Engine;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use ed25519_dalek::{Signer, SigningKey};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::mpsc;
use tokio_stream::StreamExt;
use tokio_stream::wrappers::ReceiverStream;
use tonic::Request;
use tonic::transport::Channel;

use cocoonbox_agent::pb::session_client::SessionClient;
use cocoonbox_agent::pb::state_client::StateClient;
use cocoonbox_agent::pb::{self, exec_client_frame, exec_server_frame, screen_client_frame};

const USAGE: &str =
    "usage: boxgate <identity|cred|hello|origin|fill|exec|snap|store|plan|upload|restore|mcp> [args]
  env: BOXGATE_SEED (64 hex: the edge key), BOXGATE_SERVE (default http://127.0.0.1:17443)
  identity [--wake W] [--box B]          print the identity file the api would write
  cred [--wake W] [--box B]              print an owner credential
  hello                                  call Session.Hello
  origin                                 call Session.ActiveOrigin
  fill --origin O --user U --password P [--submit]
                                         call Session.FillCredential
  exec [--tty RxC] [--resize-after MS --resize RxC] [--int-after MS] -- COMMAND
                                         run COMMAND through Session.Exec; stdin is forwarded
  snap --out PNG (--screen | --tcp ADDR) read one full RFB frame and write it as a PNG
  store --listen ADDR --dir DIR          serve PUT and GET of DIR/<name> for presigned URLs
  plan [--quiesced] --out FILE           call State.Plan and save the response
  upload --plan FILE --dir DIR --url URL call State.Upload for the chunks DIR lacks
  restore --manifest ID --dir DIR --url URL [--paths P,P --target NAME]
                                         call State.Restore, or RestorePaths with --paths
  mcp METHOD [PARAMS_JSON]               open an MCP session at serve and call METHOD";
const DEFAULT_SERVE: &str = "http://127.0.0.1:17443";
const DEFAULT_BOX: &str = "cb-box";
const DEFAULT_WAKE: &str = "w1";
const CREDENTIAL_HEADER: &str = "x-cocoonbox-credential";
const CREDENTIAL_TTL_SECS: i64 = 3600;
const STATE_KEY_ID: &str = "boxgate";
const EXEC_CHUNK: usize = 32 << 10;
const RFB_VERSION: &[u8; 12] = b"RFB 003.008\n";
const RFB_RAW: i32 = 0;
const MAX_BODY: usize = 256 << 20;

/// The edge's signing key and the box it signs for.
struct Edge {
    key: SigningKey,
    box_id: String,
    wake_id: String,
}

impl Edge {
    fn from_env(args: &mut Args) -> Result<Self, String> {
        let seed =
            std::env::var("BOXGATE_SEED").map_err(|_| "set BOXGATE_SEED to 64 hex".to_owned())?;
        let bytes: [u8; 32] = hex::decode(seed.trim())
            .map_err(|e| format!("decode BOXGATE_SEED: {e}"))?
            .try_into()
            .map_err(|_| "BOXGATE_SEED must be 32 bytes".to_owned())?;
        Ok(Self {
            key: SigningKey::from_bytes(&bytes),
            box_id: args.take("--box").unwrap_or_else(|| DEFAULT_BOX.to_owned()),
            wake_id: args
                .take("--wake")
                .unwrap_or_else(|| DEFAULT_WAKE.to_owned()),
        })
    }

    fn key_id(&self) -> String {
        hex::encode(Sha256::digest(self.key.verifying_key().to_bytes()))[..16].to_owned()
    }

    fn identity(&self) -> Value {
        json!({
            "box_id": self.box_id,
            "wake_id": self.wake_id,
            "key_id": self.key_id(),
            "public_key": URL_SAFE_NO_PAD.encode(self.key.verifying_key().to_bytes()),
        })
    }

    fn owner(&self) -> String {
        let exp = cocoonbox_agent::now_ms() / 1000 + CREDENTIAL_TTL_SECS;
        let fields = format!("{}|{}|{}|{exp}", self.key_id(), self.box_id, self.wake_id);
        let signed = format!("v1.{}", URL_SAFE_NO_PAD.encode(fields));
        let sig = URL_SAFE_NO_PAD.encode(self.key.sign(signed.as_bytes()).to_bytes());
        format!("{signed}.{sig}")
    }

    fn state_key(&self) -> pb::StateKey {
        pb::StateKey {
            key: Sha256::digest([self.key.to_bytes().as_slice(), b"state"].concat())
                .to_vec()
                .into(),
            key_id: STATE_KEY_ID.to_owned(),
        }
    }

    fn signed<T>(&self, msg: T) -> Result<Request<T>, String> {
        let mut req = Request::new(msg);
        let value = self
            .owner()
            .parse()
            .map_err(|e| format!("credential header: {e}"))?;
        req.metadata_mut().insert(CREDENTIAL_HEADER, value);
        Ok(req)
    }
}

/// The command line after the subcommand.
struct Args(Vec<String>);

impl Args {
    fn take(&mut self, flag: &str) -> Option<String> {
        let i = self.0.iter().position(|a| a == flag)?;
        if i + 1 >= self.0.len() {
            return None;
        }
        let value = self.0.remove(i + 1);
        self.0.remove(i);
        Some(value)
    }

    fn need(&mut self, flag: &str) -> Result<String, String> {
        self.take(flag).ok_or_else(|| format!("missing {flag}"))
    }

    fn has(&mut self, flag: &str) -> bool {
        let found = self.0.iter().position(|a| a == flag);
        if let Some(i) = found {
            self.0.remove(i);
        }
        found.is_some()
    }

    fn rest_after_dashes(&mut self) -> Vec<String> {
        match self.0.iter().position(|a| a == "--") {
            Some(i) => self.0.split_off(i).into_iter().skip(1).collect(),
            None => std::mem::take(&mut self.0),
        }
    }
}

#[tokio::main(flavor = "multi_thread", worker_threads = 2)]
async fn main() {
    let mut argv = std::env::args().skip(1);
    let cmd = argv.next().unwrap_or_default();
    let mut args = Args(argv.collect());
    let code = match run(&cmd, &mut args).await {
        Ok(code) => i32::from(code),
        Err(e) => {
            eprintln!("boxgate {cmd}: {e}");
            1
        }
    };
    let _ = std::io::stdout().flush();
    // exits at once: a runtime shutdown waits for the blocking stdin reader of exec
    std::process::exit(code);
}

async fn run(cmd: &str, args: &mut Args) -> Result<u8, String> {
    if cmd == "store" {
        let listen = args.need("--listen")?;
        let dir = PathBuf::from(args.need("--dir")?);
        store(&listen, &dir).await?;
        return Ok(0);
    }
    let edge = Edge::from_env(args)?;
    let serve = std::env::var("BOXGATE_SERVE").unwrap_or_else(|_| DEFAULT_SERVE.to_owned());
    match cmd {
        "identity" => println!("{}", edge.identity()),
        "cred" => println!("{}", edge.owner()),
        "hello" => {
            let mut session = SessionClient::new(channel(&serve).await?);
            let r = session
                .hello(edge.signed(pb::HelloRequest::default())?)
                .await
                .map_err(|e| format!("hello: {e}"))?
                .into_inner();
            println!(
                "{}",
                json!({
                    "agent_version": r.agent_version,
                    "screen": format!("{}x{}", r.screen_width, r.screen_height),
                    "automation": pb::Automation::try_from(r.automation).map_or("unknown", |a| a.as_str_name()),
                    "models": r.models,
                    "default_main_model": r.default_main_model,
                })
            );
        }
        "origin" => {
            let mut session = SessionClient::new(channel(&serve).await?);
            let r = session
                .active_origin(edge.signed(pb::ActiveOriginRequest {})?)
                .await
                .map_err(|e| format!("active origin: {e}"))?
                .into_inner();
            println!("{}", json!({"origin": r.origin}));
        }
        "fill" => {
            let req = pb::FillCredentialRequest {
                origin: args.need("--origin")?,
                username: args.need("--user")?,
                submit: args.has("--submit"),
                secret: Some(pb::fill_credential_request::Secret::Password(
                    args.need("--password")?,
                )),
            };
            let mut session = SessionClient::new(channel(&serve).await?);
            let r = session
                .fill_credential(edge.signed(req)?)
                .await
                .map_err(|e| format!("fill credential: {e}"))?
                .into_inner();
            println!("{}", json!({"filled": r.filled, "submitted": r.submitted}));
        }
        "exec" => return exec(&edge, &serve, args).await,
        "snap" => {
            let out = PathBuf::from(args.need("--out")?);
            let (w, h, name) = if args.has("--screen") {
                let (mut rd, mut wr) = screen_pipe(&edge, &serve).await?;
                snap(&mut rd, &mut wr, &out).await?
            } else {
                let tcp = TcpStream::connect(args.need("--tcp")?)
                    .await
                    .map_err(|e| format!("connect: {e}"))?;
                let (mut rd, mut wr) = tcp.into_split();
                snap(&mut rd, &mut wr, &out).await?
            };
            println!(
                "{}",
                json!({"width": w, "height": h, "name": name, "png": out})
            );
        }
        "plan" => {
            let out = PathBuf::from(args.need("--out")?);
            let consistency = if args.has("--quiesced") {
                pb::Consistency::Quiesced
            } else {
                pb::Consistency::Live
            };
            let mut state = StateClient::new(channel(&serve).await?);
            let r = state
                .plan(edge.signed(pb::PlanRequest {
                    key: Some(edge.state_key()),
                    consistency: consistency as i32,
                    parent: String::new(),
                })?)
                .await
                .map_err(|e| format!("plan: {e}"))?
                .into_inner();
            let saved = json!({
                "snapshot_id": r.snapshot_id,
                "chunks": r.chunks.iter().map(|c| json!({"id": c.id, "size": c.size})).collect::<Vec<_>>(),
                "files": r.files,
                "logical_bytes": r.logical_bytes,
            });
            std::fs::write(&out, saved.to_string())
                .map_err(|e| format!("write {}: {e}", out.display()))?;
            println!(
                "{}",
                json!({"snapshot_id": r.snapshot_id, "chunks": r.chunks.len(), "files": r.files, "logical_bytes": r.logical_bytes})
            );
        }
        "upload" => upload(&edge, &serve, args).await?,
        "restore" => restore(&edge, &serve, args).await?,
        "mcp" => mcp(&edge, &serve, args).await?,
        _ => {
            eprintln!("{USAGE}");
            return Ok(2);
        }
    }
    Ok(0)
}

async fn channel(serve: &str) -> Result<Channel, String> {
    Channel::from_shared(serve.to_owned())
        .map_err(|e| format!("endpoint {serve}: {e}"))?
        .connect_timeout(Duration::from_secs(10))
        .connect()
        .await
        .map_err(|e| format!("connect {serve}: {e}"))
}

async fn exec(edge: &Edge, serve: &str, args: &mut Args) -> Result<u8, String> {
    let tty = args.take("--tty").map(|s| window(&s)).transpose()?;
    let resize_after = args
        .take("--resize-after")
        .map(|s| s.parse::<u64>())
        .transpose()
        .map_err(|e| format!("--resize-after: {e}"))?;
    let resize_to = args.take("--resize").map(|s| window(&s)).transpose()?;
    let int_after = args
        .take("--int-after")
        .map(|s| s.parse::<u64>())
        .transpose()
        .map_err(|e| format!("--int-after: {e}"))?;
    let command = args.rest_after_dashes().join(" ");
    let (tx, rx) = mpsc::channel(8);
    let start = pb::ExecStart {
        command,
        tty: tty.map(|w| pb::ExecTty {
            term: "xterm-256color".to_owned(),
            window: Some(w),
            terminfo: String::new(),
        }),
    };
    tx.send(frame(exec_client_frame::Body::Start(start)))
        .await
        .map_err(|e| format!("send start: {e}"))?;
    let feeder = tx.clone();
    // detached: forwards local stdin until EOF, then sends the eof frame
    tokio::spawn(async move {
        let mut stdin = tokio::io::stdin();
        let mut buf = vec![0; EXEC_CHUNK];
        loop {
            match stdin.read(&mut buf).await {
                Ok(0) | Err(_) => break,
                Ok(n) => {
                    if feeder
                        .send(frame(exec_client_frame::Body::Stdin(
                            buf[..n].to_vec().into(),
                        )))
                        .await
                        .is_err()
                    {
                        return;
                    }
                }
            }
        }
        let _ = feeder
            .send(frame(exec_client_frame::Body::Eof(pb::ExecEof {})))
            .await;
    });
    if let (Some(ms), Some(w)) = (resize_after, resize_to) {
        let t = tx.clone();
        tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(ms)).await;
            let _ = t.send(frame(exec_client_frame::Body::Resize(w))).await;
        });
    }
    if let Some(ms) = int_after {
        let t = tx.clone();
        tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(ms)).await;
            let _ = t
                .send(frame(exec_client_frame::Body::Signal(
                    pb::ExecSignal::Int as i32,
                )))
                .await;
        });
    }
    drop(tx);
    let mut session = SessionClient::new(channel(serve).await?);
    let mut out = session
        .exec(edge.signed(ReceiverStream::new(rx))?)
        .await
        .map_err(|e| format!("exec: {e}"))?
        .into_inner();
    let mut stdout = std::io::stdout();
    let mut stderr = std::io::stderr();
    while let Some(f) = out.next().await {
        match f.map_err(|e| format!("exec stream: {e}"))?.body {
            Some(exec_server_frame::Body::Stdout(b)) => {
                stdout.write_all(&b).map_err(|e| format!("stdout: {e}"))?
            }
            Some(exec_server_frame::Body::Stderr(b)) => {
                stderr.write_all(&b).map_err(|e| format!("stderr: {e}"))?
            }
            Some(exec_server_frame::Body::ExitCode(code)) => {
                let _ = stdout.flush();
                eprintln!("boxgate exec: exit {code}");
                return Ok(u8::try_from(code).unwrap_or(1));
            }
            None => {}
        }
    }
    Err("exec stream ended without an exit code".to_owned())
}

fn frame(body: exec_client_frame::Body) -> pb::ExecClientFrame {
    pb::ExecClientFrame { body: Some(body) }
}

fn window(s: &str) -> Result<pb::ExecWindow, String> {
    let (rows, cols) = s
        .split_once('x')
        .ok_or_else(|| format!("window {s}: want ROWSxCOLS"))?;
    Ok(pb::ExecWindow {
        rows: rows.parse().map_err(|e| format!("rows {rows}: {e}"))?,
        cols: cols.parse().map_err(|e| format!("cols {cols}: {e}"))?,
    })
}

/// Bridges Session.Screen to a pair of local byte pipes.
async fn screen_pipe(
    edge: &Edge,
    serve: &str,
) -> Result<(tokio::io::DuplexStream, tokio::io::DuplexStream), String> {
    let (tx, rx) = mpsc::channel(16);
    tx.send(pb::ScreenClientFrame {
        body: Some(screen_client_frame::Body::Open(pb::ScreenOpen {})),
    })
    .await
    .map_err(|e| format!("send open: {e}"))?;
    let mut session = SessionClient::new(channel(serve).await?);
    let mut inbound = session
        .screen(edge.signed(ReceiverStream::new(rx))?)
        .await
        .map_err(|e| format!("screen: {e}"))?
        .into_inner();
    let (mut down_w, down_r) = tokio::io::duplex(1 << 20);
    let (up_w, mut up_r) = tokio::io::duplex(1 << 20);
    // detached pumps: each ends when its side closes
    tokio::spawn(async move {
        while let Some(Ok(f)) = inbound.next().await {
            if down_w.write_all(&f.data).await.is_err() {
                break;
            }
        }
    });
    tokio::spawn(async move {
        let mut buf = vec![0; EXEC_CHUNK];
        while let Ok(n) = up_r.read(&mut buf).await {
            if n == 0
                || tx
                    .send(pb::ScreenClientFrame {
                        body: Some(screen_client_frame::Body::Data(buf[..n].to_vec().into())),
                    })
                    .await
                    .is_err()
            {
                break;
            }
        }
    });
    Ok((down_r, up_w))
}

/// Runs an RFB 3.8 handshake with security None, asks for one full Raw frame and writes it as a PNG.
async fn snap<R, W>(rd: &mut R, wr: &mut W, out: &Path) -> Result<(u16, u16, String), String>
where
    R: tokio::io::AsyncRead + Unpin,
    W: tokio::io::AsyncWrite + Unpin,
{
    let io = |e: std::io::Error| format!("rfb: {e}");
    let mut version = [0; 12];
    rd.read_exact(&mut version).await.map_err(io)?;
    wr.write_all(RFB_VERSION).await.map_err(io)?;
    let count = rd.read_u8().await.map_err(io)?;
    let mut types = vec![0; usize::from(count)];
    rd.read_exact(&mut types).await.map_err(io)?;
    if !types.contains(&1) {
        return Err(format!(
            "server {} offers no security None: {types:?}",
            String::from_utf8_lossy(&version).trim()
        ));
    }
    wr.write_u8(1).await.map_err(io)?;
    if rd.read_u32().await.map_err(io)? != 0 {
        return Err("security None refused".to_owned());
    }
    wr.write_u8(1).await.map_err(io)?;
    let width = rd.read_u16().await.map_err(io)?;
    let height = rd.read_u16().await.map_err(io)?;
    let mut format = [0; 16];
    rd.read_exact(&mut format).await.map_err(io)?;
    let name_len = rd.read_u32().await.map_err(io)?;
    let mut name = vec![0; usize::try_from(name_len).map_err(|e| e.to_string())?];
    rd.read_exact(&mut name).await.map_err(io)?;
    let set_format = vec![
        0, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0,
    ];
    wr.write_all(&set_format).await.map_err(io)?;
    let mut encodings = vec![2, 0, 0, 1];
    encodings.extend_from_slice(&RFB_RAW.to_be_bytes());
    wr.write_all(&encodings).await.map_err(io)?;
    let mut request = vec![3, 0, 0, 0, 0, 0];
    request.extend_from_slice(&width.to_be_bytes());
    request.extend_from_slice(&height.to_be_bytes());
    wr.write_all(&request).await.map_err(io)?;
    let (w, h) = (usize::from(width), usize::from(height));
    let mut rgba = vec![0u8; w * h * 4];
    let mut covered = 0usize;
    while covered < w * h {
        let kind = rd.read_u8().await.map_err(io)?;
        if kind != 0 {
            return Err(format!("unexpected server message {kind} before the frame"));
        }
        rd.read_u8().await.map_err(io)?;
        let rects = rd.read_u16().await.map_err(io)?;
        for _ in 0..rects {
            let x = usize::from(rd.read_u16().await.map_err(io)?);
            let y = usize::from(rd.read_u16().await.map_err(io)?);
            let rw = usize::from(rd.read_u16().await.map_err(io)?);
            let rh = usize::from(rd.read_u16().await.map_err(io)?);
            let enc = rd.read_i32().await.map_err(io)?;
            if enc != RFB_RAW {
                return Err(format!("server sent encoding {enc}; asked for Raw only"));
            }
            let mut px = vec![0; rw * rh * 4];
            rd.read_exact(&mut px).await.map_err(io)?;
            for row in 0..rh {
                for col in 0..rw {
                    let (sx, dx) = ((row * rw + col) * 4, ((y + row) * w + x + col) * 4);
                    if dx + 3 < rgba.len() {
                        rgba[dx] = px[sx + 2];
                        rgba[dx + 1] = px[sx + 1];
                        rgba[dx + 2] = px[sx];
                        rgba[dx + 3] = 255;
                    }
                }
            }
            covered += rw * rh;
        }
    }
    let file = std::fs::File::create(out).map_err(|e| format!("create {}: {e}", out.display()))?;
    let mut encoder = png::Encoder::new(
        std::io::BufWriter::new(file),
        u32::from(width),
        u32::from(height),
    );
    encoder.set_color(png::ColorType::Rgba);
    encoder.set_depth(png::BitDepth::Eight);
    encoder
        .write_header()
        .and_then(|mut w| w.write_image_data(&rgba))
        .map_err(|e| format!("write png: {e}"))?;
    Ok((width, height, String::from_utf8_lossy(&name).into_owned()))
}

/// Serves PUT and GET of `dir/<name>` over HTTP/1.1, one request per connection.
async fn store(listen: &str, dir: &Path) -> Result<(), String> {
    std::fs::create_dir_all(dir).map_err(|e| format!("create {}: {e}", dir.display()))?;
    let listener = TcpListener::bind(listen)
        .await
        .map_err(|e| format!("bind {listen}: {e}"))?;
    eprintln!("boxgate store: {listen} -> {}", dir.display());
    loop {
        let (conn, _) = listener
            .accept()
            .await
            .map_err(|e| format!("accept: {e}"))?;
        let dir = dir.to_owned();
        // detached: one connection, one request
        tokio::spawn(async move {
            if let Err(e) = serve_one(conn, &dir).await {
                eprintln!("boxgate store: {e}");
            }
        });
    }
}

async fn serve_one(conn: TcpStream, dir: &Path) -> Result<(), String> {
    let mut rd = BufReader::new(conn);
    let mut line = String::new();
    rd.read_line(&mut line)
        .await
        .map_err(|e| format!("read request: {e}"))?;
    let mut parts = line.split_whitespace();
    let (method, target) = (
        parts.next().unwrap_or_default().to_owned(),
        parts.next().unwrap_or_default().to_owned(),
    );
    let mut length = 0usize;
    loop {
        let mut h = String::new();
        rd.read_line(&mut h)
            .await
            .map_err(|e| format!("read header: {e}"))?;
        if h.trim().is_empty() {
            break;
        }
        if let Some((k, v)) = h.split_once(':')
            && k.eq_ignore_ascii_case("content-length")
        {
            length = v
                .trim()
                .parse()
                .map_err(|e| format!("content-length: {e}"))?;
        }
    }
    let name = target.rsplit('/').next().unwrap_or_default();
    let safe = !name.is_empty()
        && name
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_');
    let path = dir.join(name);
    let (status, body) = match method.as_str() {
        _ if !safe => {
            eprintln!("boxgate store: refused {method} {target}");
            ("400 Bad Request", Vec::new())
        }
        "PUT" if length <= MAX_BODY => {
            let mut body = vec![0; length];
            rd.read_exact(&mut body)
                .await
                .map_err(|e| format!("read body: {e}"))?;
            std::fs::write(&path, body).map_err(|e| format!("write {}: {e}", path.display()))?;
            ("200 OK", Vec::new())
        }
        "GET" => match std::fs::read(&path) {
            Ok(b) => ("200 OK", b),
            Err(_) => ("404 Not Found", Vec::new()),
        },
        _ => ("405 Method Not Allowed", Vec::new()),
    };
    let mut conn = rd.into_inner();
    let head = format!(
        "HTTP/1.1 {status}\r\ncontent-length: {}\r\nconnection: close\r\n\r\n",
        body.len()
    );
    conn.write_all(head.as_bytes())
        .await
        .map_err(|e| format!("write response: {e}"))?;
    conn.write_all(&body)
        .await
        .map_err(|e| format!("write body: {e}"))?;
    conn.shutdown().await.map_err(|e| format!("close: {e}"))
}

async fn upload(edge: &Edge, serve: &str, args: &mut Args) -> Result<(), String> {
    let plan: Value = read_json(&PathBuf::from(args.need("--plan")?))?;
    let dir = PathBuf::from(args.need("--dir")?);
    let url = args.need("--url")?;
    let snapshot_id = plan["snapshot_id"].as_str().unwrap_or_default().to_owned();
    let puts = plan["chunks"]
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|c| c["id"].as_str())
        .filter(|id| !dir.join(id).exists())
        .map(|id| pb::PresignedPut {
            chunk_id: id.to_owned(),
            url: format!("{url}/{id}"),
            headers: HashMap::new(),
        })
        .collect::<Vec<_>>();
    let sent = puts.len();
    let mut state = StateClient::new(channel(serve).await?);
    let mut progress = state
        .upload(edge.signed(pb::UploadRequest {
            manifest: Some(pb::PresignedPut {
                url: format!("{url}/{snapshot_id}"),
                chunk_id: snapshot_id.clone(),
                headers: HashMap::new(),
            }),
            snapshot_id,
            key: Some(edge.state_key()),
            puts,
        })?)
        .await
        .map_err(|e| format!("upload: {e}"))?
        .into_inner();
    let last = drain(&mut progress).await?;
    println!(
        "{}",
        json!({"manifest_id": last.manifest_id, "puts": sent, "done_bytes": last.done_bytes, "finished": last.finished})
    );
    Ok(())
}

async fn restore(edge: &Edge, serve: &str, args: &mut Args) -> Result<(), String> {
    let manifest = args.need("--manifest")?;
    let dir = PathBuf::from(args.need("--dir")?);
    let url = args.need("--url")?;
    let scope = args.take("--paths").map(|p| pb::RestoreScope {
        paths: p.split(',').map(str::to_owned).collect(),
        target: args.take("--target").unwrap_or_default(),
    });
    let gets = std::fs::read_dir(&dir)
        .map_err(|e| format!("read {}: {e}", dir.display()))?
        .flatten()
        .filter_map(|e| e.file_name().into_string().ok())
        .filter(|n| *n != manifest)
        .map(|n| pb::PresignedGet {
            url: format!("{url}/{n}"),
            chunk_id: n,
        })
        .collect::<Vec<_>>();
    let paths = scope.is_some();
    let first = pb::RestoreRequest {
        key: Some(edge.state_key()),
        manifest: Some(pb::PresignedGet {
            url: format!("{url}/{manifest}"),
            chunk_id: manifest,
        }),
        gets,
        scope,
    };
    let reqs = tokio_stream::iter([first]);
    let mut state = StateClient::new(channel(serve).await?);
    let mut progress = if paths {
        state.restore_paths(edge.signed(reqs)?).await
    } else {
        state.restore(edge.signed(reqs)?).await
    }
    .map_err(|e| format!("restore: {e}"))?
    .into_inner();
    let last = drain(&mut progress).await?;
    println!(
        "{}",
        json!({"manifest_id": last.manifest_id, "target": last.target, "done_bytes": last.done_bytes, "finished": last.finished})
    );
    Ok(())
}

async fn drain(
    progress: &mut tonic::Streaming<pb::TransferProgress>,
) -> Result<pb::TransferProgress, String> {
    let mut last = pb::TransferProgress::default();
    while let Some(p) = progress.next().await {
        last = p.map_err(|e| format!("transfer: {e}"))?;
    }
    if !last.finished {
        return Err(format!(
            "transfer ended unfinished at {} of {} bytes",
            last.done_bytes, last.total_bytes
        ));
    }
    Ok(last)
}

async fn mcp(edge: &Edge, serve: &str, args: &mut Args) -> Result<(), String> {
    let method = args.0.first().cloned().ok_or("missing METHOD")?;
    let params: Value = match args.0.get(1) {
        Some(p) => serde_json::from_str(p).map_err(|e| format!("params: {e}"))?,
        None => json!({}),
    };
    let http = reqwest::Client::new();
    let url = format!("{serve}{}", cocoonbox_agent::mcp::PATH);
    let init = json!({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "boxgate", "version": "0"}}});
    let resp = post_mcp(&http, &url, edge, None, &init).await?;
    let session = resp.0.ok_or("initialize returned no Mcp-Session-Id")?;
    let note = json!({"jsonrpc": "2.0", "method": "notifications/initialized"});
    post_mcp(&http, &url, edge, Some(&session), &note).await?;
    let call = json!({"jsonrpc": "2.0", "id": 2, "method": method, "params": params});
    let (_, result) = post_mcp(&http, &url, edge, Some(&session), &call).await?;
    println!("{result}");
    Ok(())
}

async fn post_mcp(
    http: &reqwest::Client,
    url: &str,
    edge: &Edge,
    session: Option<&str>,
    body: &Value,
) -> Result<(Option<String>, Value), String> {
    let mut req = http
        .post(url)
        .header(CREDENTIAL_HEADER, edge.owner())
        .header("accept", "application/json, text/event-stream")
        .json(body);
    if let Some(s) = session {
        req = req.header("mcp-session-id", s);
    }
    let resp = req.send().await.map_err(|e| format!("mcp post: {e}"))?;
    let status = resp.status();
    let session = resp
        .headers()
        .get("mcp-session-id")
        .and_then(|v| v.to_str().ok())
        .map(str::to_owned);
    let sse = resp
        .headers()
        .get("content-type")
        .and_then(|v| v.to_str().ok())
        .is_some_and(|v| v.starts_with("text/event-stream"));
    let text = resp.text().await.map_err(|e| format!("mcp body: {e}"))?;
    if !status.is_success() {
        return Err(format!("mcp {status}: {text}"));
    }
    if text.trim().is_empty() {
        return Ok((session, Value::Null));
    }
    let payload = if sse {
        text.lines()
            .filter_map(|l| l.strip_prefix("data:"))
            .map(str::trim)
            .rfind(|d| d.contains("\"id\""))
            .unwrap_or_default()
            .to_owned()
    } else {
        text
    };
    let value = serde_json::from_str(&payload).map_err(|e| format!("mcp json: {e}: {payload}"))?;
    Ok((session, value))
}

fn read_json(path: &Path) -> Result<Value, String> {
    let text =
        std::fs::read_to_string(path).map_err(|e| format!("read {}: {e}", path.display()))?;
    serde_json::from_str(&text).map_err(|e| format!("parse {}: {e}", path.display()))
}
