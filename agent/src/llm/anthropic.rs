//! Anthropic Messages request body and SSE parser.

use std::collections::VecDeque;

use serde_json::{Value, json};

use super::{Delta, IMAGE_OMITTED, Message, Part, Request, Usage, b64, image_mime, sse};

/// Accumulates usage across `message_start` and `message_delta`.
#[derive(Default)]
pub struct Parser {
    usage: Usage,
}

impl Parser {
    /// Converts one SSE event into deltas.
    pub fn feed(&mut self, event: &sse::Event, out: &mut VecDeque<Delta>) -> Result<(), String> {
        if event.data.is_empty() {
            return Ok(());
        }
        let v: Value = serde_json::from_str(&event.data)
            .map_err(|e| format!("decode messages event: {e}: {}", event.data))?;
        match v["type"].as_str().unwrap_or_default() {
            "message_start" => {
                let u = &v["message"]["usage"];
                let fresh = u["input_tokens"].as_u64().unwrap_or(0);
                let read = u["cache_read_input_tokens"].as_u64().unwrap_or(0);
                let created = u["cache_creation_input_tokens"].as_u64().unwrap_or(0);
                self.usage.input = fresh + read + created;
                self.usage.cached = read;
                self.usage.output = u["output_tokens"].as_u64().unwrap_or(0);
            }
            "content_block_start" => {
                let index = index_of(&v);
                let block = &v["content_block"];
                match block["type"].as_str().unwrap_or_default() {
                    "tool_use" => out.push_back(Delta::ToolStart {
                        index,
                        id: block["id"].as_str().unwrap_or_default().to_owned(),
                        name: block["name"].as_str().unwrap_or_default().to_owned(),
                    }),
                    "text" => {
                        if let Some(t) = block["text"].as_str().filter(|t| !t.is_empty()) {
                            out.push_back(Delta::Text(t.to_owned()));
                        }
                    }
                    _ => {}
                }
            }
            "content_block_delta" => {
                let d = &v["delta"];
                match d["type"].as_str().unwrap_or_default() {
                    "text_delta" => push_str(out, Delta::Text, &d["text"]),
                    "thinking_delta" => push_str(out, Delta::Reasoning, &d["thinking"]),
                    "input_json_delta" => {
                        if let Some(f) = d["partial_json"].as_str().filter(|f| !f.is_empty()) {
                            out.push_back(Delta::ToolArgs {
                                index: index_of(&v),
                                fragment: f.to_owned(),
                            });
                        }
                    }
                    _ => {}
                }
            }
            "message_delta" => {
                if let Some(n) = v["usage"]["output_tokens"].as_u64() {
                    self.usage.output = n;
                }
            }
            "message_stop" => {
                out.push_back(Delta::Usage(self.usage));
                out.push_back(Delta::Done);
            }
            "error" => return Err(format!("model error: {}", v["error"])),
            _ => {}
        }
        Ok(())
    }
}

/// Builds the Messages request body with prompt caching on the system prompt and the tail.
pub fn body(req: &Request<'_>) -> Value {
    let mut messages: Vec<Value> = Vec::new();
    for m in req.messages {
        match m {
            Message::User(parts) => {
                let blocks = parts.iter().map(|p| part(p, req.vision)).collect();
                push_user_blocks(&mut messages, blocks);
            }
            Message::Assistant { text, calls } => {
                let mut content = Vec::new();
                if !text.is_empty() {
                    content.push(json!({"type": "text", "text": text}));
                }
                for c in calls {
                    let input: Value = serde_json::from_str(&c.arguments)
                        .ok()
                        .filter(Value::is_object)
                        .unwrap_or_else(|| json!({}));
                    let mut block = json!({"type": "tool_use", "id": c.id, "name": c.name});
                    block["input"] = input;
                    content.push(block);
                }
                if content.is_empty() {
                    continue;
                }
                let mut msg = json!({"role": "assistant"});
                msg["content"] = Value::Array(content);
                messages.push(msg);
            }
            Message::Tool {
                call_id,
                text,
                image,
                is_error,
            } => {
                let mut content = vec![
                    json!({"type": "text", "text": if text.is_empty() { "(no output)" } else { text }}),
                ];
                if let Some(img) = image {
                    content.push(part(&Part::Image(img.clone()), req.vision));
                }
                let mut block =
                    json!({"type": "tool_result", "tool_use_id": call_id, "is_error": is_error});
                block["content"] = Value::Array(content);
                push_user_blocks(&mut messages, vec![block]);
            }
        }
    }
    if let Some(last) = messages
        .last_mut()
        .and_then(|m| m["content"].as_array_mut())
        .and_then(|c| c.last_mut())
    {
        last["cache_control"] = json!({"type": "ephemeral"});
    }
    let mut body = json!({
        "model": req.model,
        "max_tokens": req.max_tokens,
        "stream": true,
        "system": [{"type": "text", "text": req.system, "cache_control": {"type": "ephemeral"}}],
    });
    body["messages"] = Value::Array(messages);
    if !req.tools.is_empty() {
        body["tools"] = req
            .tools
            .iter()
            .map(|t| json!({"name": t.name, "description": t.description, "input_schema": t.parameters}))
            .collect();
    }
    body
}

fn push_user_blocks(messages: &mut Vec<Value>, blocks: Vec<Value>) {
    if let Some(prev) = messages.last_mut().filter(|m| m["role"] == "user")
        && let Some(content) = prev["content"].as_array_mut()
    {
        content.extend(blocks);
        return;
    }
    let mut msg = json!({"role": "user"});
    msg["content"] = Value::Array(blocks);
    messages.push(msg);
}

fn part(p: &Part, vision: bool) -> Value {
    match p {
        Part::Text(t) => json!({"type": "text", "text": t}),
        Part::Image(img) if vision => {
            let mut block = json!({
                "type": "image",
                "source": {"type": "base64", "media_type": image_mime(img)},
            });
            block["source"]["data"] = b64(img).into();
            block
        }
        Part::Image(_) => json!({"type": "text", "text": IMAGE_OMITTED}),
    }
}

fn index_of(v: &Value) -> usize {
    v["index"]
        .as_u64()
        .and_then(|i| usize::try_from(i).ok())
        .unwrap_or(0)
}

fn push_str(out: &mut VecDeque<Delta>, make: fn(String) -> Delta, v: &Value) {
    if let Some(t) = v.as_str().filter(|t| !t.is_empty()) {
        out.push_back(make(t.to_owned()));
    }
}

#[cfg(test)]
mod tests {
    use bytes::Bytes;

    use super::*;
    use crate::llm::ToolCall;

    #[test]
    fn parses_stream_with_tool_use_and_usage() {
        let events = [
            r#"{"type":"message_start","message":{"usage":{"input_tokens":3,"cache_read_input_tokens":7,"output_tokens":1}}}"#,
            r#"{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}"#,
            r#"{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Looking"}}"#,
            r#"{"type":"content_block_stop","index":0}"#,
            r#"{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"computer_state","input":{}}}"#,
            r#"{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"pid\":"}}"#,
            r#"{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"1}"}}"#,
            r#"{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}"#,
            r#"{"type":"message_stop"}"#,
        ];
        let mut p = Parser::default();
        let mut out = VecDeque::new();
        for e in events {
            let event = sse::Event {
                event: String::new(),
                data: e.into(),
            };
            p.feed(&event, &mut out).unwrap();
        }
        let got: Vec<Delta> = out.into_iter().collect();
        assert_eq!(
            got,
            vec![
                Delta::Text("Looking".into()),
                Delta::ToolStart {
                    index: 1,
                    id: "toolu_1".into(),
                    name: "computer_state".into()
                },
                Delta::ToolArgs {
                    index: 1,
                    fragment: "{\"pid\":".into()
                },
                Delta::ToolArgs {
                    index: 1,
                    fragment: "1}".into()
                },
                Delta::Usage(Usage {
                    input: 10,
                    output: 20,
                    cached: 7
                }),
                Delta::Done,
            ]
        );
    }

    #[test]
    fn error_event_fails() {
        let mut p = Parser::default();
        let event = sse::Event {
            event: "error".into(),
            data: r#"{"type":"error","error":{"type":"overloaded_error"}}"#.into(),
        };
        assert!(p.feed(&event, &mut VecDeque::new()).is_err());
    }

    #[test]
    fn body_merges_tool_results_and_carries_images() {
        let messages = vec![
            Message::User(vec![Part::Text("task".into())]),
            Message::Assistant {
                text: "ok".into(),
                calls: vec![
                    ToolCall {
                        id: "t1".into(),
                        name: "computer_state".into(),
                        arguments: String::new(),
                    },
                    ToolCall {
                        id: "t2".into(),
                        name: "computer_act".into(),
                        arguments: r#"{"action":"click"}"#.into(),
                    },
                ],
            },
            Message::Tool {
                call_id: "t1".into(),
                text: "state".into(),
                image: Some(Bytes::from_static(b"\x89PNG")),
                is_error: false,
            },
            Message::Tool {
                call_id: "t2".into(),
                text: String::new(),
                image: None,
                is_error: true,
            },
        ];
        let req = Request {
            model: "anthropic/claude-sonnet-5",
            system: "sys",
            messages: &messages,
            tools: &[],
            max_tokens: 50,
            vision: true,
        };
        let b = body(&req);
        let msgs = b["messages"].as_array().unwrap();
        assert_eq!(msgs.len(), 3);
        assert_eq!(msgs[1]["content"][1]["input"], json!({}));
        assert_eq!(msgs[1]["content"][2]["input"]["action"], "click");
        let results = msgs[2]["content"].as_array().unwrap();
        assert_eq!(results.len(), 2);
        assert_eq!(results[0]["content"][1]["type"], "image");
        assert_eq!(results[1]["is_error"], true);
        assert_eq!(results[1]["cache_control"]["type"], "ephemeral");
        assert_eq!(b["system"][0]["text"], "sys");
    }
}
