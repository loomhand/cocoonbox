//! Streaming model client for gw: OpenAI chat completions and Anthropic Messages.

pub mod anthropic;
pub mod openai;
pub mod sse;

use std::borrow::Cow;
use std::collections::{BTreeMap, VecDeque};
use std::time::Duration;

use base64::Engine;
use base64::engine::general_purpose::STANDARD as B64;
use bytes::Bytes;
use futures_util::StreamExt;
use futures_util::stream::BoxStream;
use serde_json::Value;

/// Placeholder text for images sent to a text-only model.
pub const IMAGE_OMITTED: &str = "[screenshot omitted: this model has no vision]";

const VISION_PREFIXES: [&str; 4] = [
    "anthropic/",
    "google/gemini",
    "openai/gpt-4o",
    "openai/gpt-5",
];

const CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const READ_TIMEOUT: Duration = Duration::from_secs(180);
const ERROR_BODY_BYTES: usize = 2048;

/// A tool the model may call; connector tools carry names and descriptions of their own.
pub struct ToolSpec {
    pub name: Cow<'static, str>,
    pub description: Cow<'static, str>,
    pub parameters: Value,
}

/// Content of a user message.
#[derive(Clone, Debug)]
pub enum Part {
    Text(String),
    Image(Bytes),
}

/// A tool call requested by the model.
#[derive(Clone, Debug, Default)]
pub struct ToolCall {
    pub id: String,
    pub name: String,
    pub arguments: String,
}

/// One conversation message in wire-neutral form.
#[derive(Clone, Debug)]
pub enum Message {
    User(Vec<Part>),
    Assistant {
        text: String,
        calls: Vec<ToolCall>,
    },
    Tool {
        call_id: String,
        text: String,
        image: Option<Bytes>,
        is_error: bool,
    },
}

/// Which wire protocol a model speaks through gw.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Wire {
    Chat,
    Messages,
}

impl Wire {
    /// Chat completions for every model; Messages for `anthropic/` models only when enabled.
    pub fn for_model(model: &str, anthropic_messages: bool) -> Self {
        if anthropic_messages && model.starts_with("anthropic/") {
            Self::Messages
        } else {
            Self::Chat
        }
    }
}

/// A single model request.
pub struct Request<'a> {
    pub model: &'a str,
    pub system: &'a str,
    pub messages: &'a [Message],
    pub tools: &'a [&'a ToolSpec],
    pub max_tokens: u32,
    pub vision: bool,
    /// Offers `tools` but forbids calling them, so the cached prompt prefix still matches.
    pub forbid_calls: bool,
}

/// Token usage of one or more requests.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Usage {
    pub input: u64,
    pub output: u64,
    pub cached: u64,
}

impl Usage {
    /// Adds `other` into `self`.
    pub fn add(&mut self, other: Self) {
        self.input += other.input;
        self.output += other.output;
        self.cached += other.cached;
    }
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Tally(BTreeMap<String, (u64, Usage)>);

impl Tally {
    pub fn add(&mut self, model: &str, calls: u64, usage: Usage) {
        let (n, sum) = self.0.entry(model.to_owned()).or_default();
        *n += calls;
        sum.add(usage);
    }

    pub fn add_reported(&mut self, report: &str) {
        let Ok(report) = serde_json::from_str::<Value>(report) else {
            return;
        };
        for (model, u) in report["usage"].as_object().into_iter().flatten() {
            let usage = Usage {
                input: u["input_tokens"].as_u64().unwrap_or(0),
                output: u["output_tokens"].as_u64().unwrap_or(0),
                cached: u["cached_tokens"].as_u64().unwrap_or(0),
            };
            self.add(model, u["calls"].as_u64().unwrap_or(0), usage);
        }
    }

    pub fn total(&self) -> Usage {
        let mut total = Usage::default();
        for (_, u) in self.0.values() {
            total.add(*u);
        }
        total
    }

    pub fn iter(&self) -> impl Iterator<Item = (&str, u64, Usage)> {
        self.0.iter().map(|(m, (n, u))| (m.as_str(), *n, *u))
    }
}

/// A normalized streaming increment.
#[derive(Debug, PartialEq, Eq)]
pub enum Delta {
    Text(String),
    Reasoning(String),
    ToolStart {
        index: usize,
        id: String,
        name: String,
    },
    ToolArgs {
        index: usize,
        fragment: String,
    },
    Usage(Usage),
    /// The reply stopped at its output token limit.
    Cut,
    Done,
}

/// What the loop sees of a model: one streaming request at a time.
#[tonic::async_trait]
pub trait Model: Send + Sync {
    /// Starts a streaming request.
    async fn stream(&self, req: &Request<'_>) -> Result<Box<dyn Deltas>, String>;
}

/// A stream of normalized deltas for one request.
#[tonic::async_trait]
pub trait Deltas: Send {
    /// Returns the next delta; `None` after `Delta::Done` or end of body.
    async fn next(&mut self) -> Result<Option<Delta>, String>;
}

/// HTTP client bound to one gw endpoint; the production [`Model`].
#[derive(Clone)]
pub struct Client {
    http: reqwest::Client,
    base_url: String,
    api_key: String,
    anthropic_messages: bool,
}

impl Client {
    /// Creates a client for `base_url` (ending in `/v1`).
    pub fn new(base_url: &str, api_key: &str, anthropic_messages: bool) -> Result<Self, String> {
        let http = reqwest::Client::builder()
            .connect_timeout(CONNECT_TIMEOUT)
            .read_timeout(READ_TIMEOUT)
            .build()
            .map_err(|e| format!("build http client: {e}"))?;
        Ok(Self {
            http,
            base_url: base_url.trim_end_matches('/').to_owned(),
            api_key: api_key.to_owned(),
            anthropic_messages,
        })
    }

    /// Starts a streaming request and returns its delta stream.
    pub async fn start(&self, req: &Request<'_>) -> Result<Stream, String> {
        let wire = Wire::for_model(req.model, self.anthropic_messages);
        let (path, body) = match wire {
            Wire::Chat => ("chat/completions", openai::body(req)),
            Wire::Messages => ("messages", anthropic::body(req)),
        };
        let url = format!("{}/{path}", self.base_url);
        let body = serde_json::to_vec(&body).map_err(|e| format!("encode request: {e}"))?;
        let resp = self
            .http
            .post(&url)
            .bearer_auth(&self.api_key)
            .header("x-api-key", &self.api_key)
            .header("anthropic-version", "2023-06-01")
            .header("content-type", "application/json")
            .header("accept", "text/event-stream")
            .body(body)
            .send()
            .await
            .map_err(|e| format!("post {url}: {e}"))?;
        let status = resp.status();
        if !status.is_success() {
            let text = resp.text().await.unwrap_or_default();
            let text: String = text.chars().take(ERROR_BODY_BYTES).collect();
            return Err(format!("gw {path} returned {status}: {text}"));
        }
        Ok(Stream {
            body: resp.bytes_stream().boxed(),
            decoder: sse::Decoder::default(),
            parser: match wire {
                Wire::Chat => Parser::Chat(openai::Parser::default()),
                Wire::Messages => Parser::Messages(anthropic::Parser::default()),
            },
            pending: VecDeque::new(),
            finished: false,
        })
    }
}

#[tonic::async_trait]
impl Model for Client {
    async fn stream(&self, req: &Request<'_>) -> Result<Box<dyn Deltas>, String> {
        Ok(Box::new(self.start(req).await?))
    }
}

/// Pull-based stream of deltas for one request.
pub struct Stream {
    body: BoxStream<'static, reqwest::Result<Bytes>>,
    decoder: sse::Decoder,
    parser: Parser,
    pending: VecDeque<Delta>,
    finished: bool,
}

impl Stream {
    /// Returns the next delta; `None` after `Delta::Done` or end of body.
    pub async fn next(&mut self) -> Result<Option<Delta>, String> {
        loop {
            if let Some(d) = self.pending.pop_front() {
                if d == Delta::Done {
                    self.finished = true;
                }
                return Ok(Some(d));
            }
            if self.finished {
                return Ok(None);
            }
            if let Some(event) = self.decoder.next_event() {
                self.feed(&event)?;
                continue;
            }
            match self.body.next().await {
                Some(chunk) => {
                    let chunk = chunk.map_err(|e| format!("read model stream: {e}"))?;
                    self.decoder.push(&chunk);
                }
                None => {
                    if let Some(event) = self.decoder.finish() {
                        self.feed(&event)?;
                    }
                    self.pending.push_back(Delta::Done);
                }
            }
        }
    }

    fn feed(&mut self, event: &sse::Event) -> Result<(), String> {
        match &mut self.parser {
            Parser::Chat(p) => p.feed(event, &mut self.pending),
            Parser::Messages(p) => p.feed(event, &mut self.pending),
        }
    }
}

#[tonic::async_trait]
impl Deltas for Stream {
    async fn next(&mut self) -> Result<Option<Delta>, String> {
        Stream::next(self).await
    }
}

enum Parser {
    Chat(openai::Parser),
    Messages(anthropic::Parser),
}

/// Whether a gw model slug accepts image input.
pub fn has_vision(model: &str) -> bool {
    VISION_PREFIXES.iter().any(|p| model.starts_with(p)) || model.contains("-vl")
}

/// Returns the MIME type of an encoded image by its magic bytes.
pub fn image_mime(data: &[u8]) -> &'static str {
    if data.starts_with(&[0xff, 0xd8]) {
        "image/jpeg"
    } else if data.starts_with(b"RIFF") && data.get(8..12) == Some(b"WEBP") {
        "image/webp"
    } else {
        "image/png"
    }
}

/// Encodes image bytes as base64.
pub fn b64(data: &[u8]) -> String {
    B64.encode(data)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_tally_sums_each_model_and_the_usage_a_gui_plan_report_carries() {
        let mut tally = Tally::default();
        let step = Usage {
            input: 100,
            output: 10,
            cached: 40,
        };
        tally.add("anthropic/claude-sonnet-5", 1, step);
        tally.add("anthropic/claude-sonnet-5", 1, step);
        tally.add_reported(r#"{"ok":true,"usage":{"typesafe/jev-1.13":{"calls":3,"input_tokens":900,"output_tokens":30,"cached_tokens":0},"anthropic/claude-sonnet-5":{"calls":1,"input_tokens":1500,"output_tokens":20,"cached_tokens":0}}}"#);
        tally.add_reported(r#"{"ok":true,"done":2}"#);
        tally.add_reported("not json");
        let got: Vec<_> = tally.iter().collect();
        assert_eq!(
            got,
            [
                (
                    "anthropic/claude-sonnet-5",
                    3,
                    Usage {
                        input: 1700,
                        output: 40,
                        cached: 80
                    }
                ),
                (
                    "typesafe/jev-1.13",
                    3,
                    Usage {
                        input: 900,
                        output: 30,
                        cached: 0
                    }
                ),
            ]
        );
        assert_eq!(
            tally.total(),
            Usage {
                input: 2600,
                output: 70,
                cached: 80
            }
        );
    }
}
