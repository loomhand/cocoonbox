//! OpenAI chat-completions request body and SSE parser.

use std::collections::{HashSet, VecDeque};

use serde_json::{Value, json};

use super::{
    Delta, IMAGE_OMITTED, Message, Part, Request, Usage, b64, image_mime, sse, stream_error,
};

/// Tracks which tool-call indexes already started.
#[derive(Default)]
pub struct Parser {
    started: HashSet<usize>,
}

impl Parser {
    /// Converts one SSE event into deltas.
    pub fn feed(&mut self, event: &sse::Event, out: &mut VecDeque<Delta>) -> Result<(), String> {
        if event.data == "[DONE]" {
            out.push_back(Delta::Done);
            return Ok(());
        }
        let v: Value = serde_json::from_str(&event.data)
            .map_err(|e| format!("decode chat chunk: {e}: {}", event.data))?;
        if let Some(err) = v.get("error") {
            return Err(stream_error(err));
        }
        if let Some(choice) = v["choices"].get(0) {
            let delta = &choice["delta"];
            for key in ["reasoning_content", "reasoning"] {
                if let Some(t) = delta[key].as_str().filter(|t| !t.is_empty()) {
                    out.push_back(Delta::Reasoning(t.to_owned()));
                    break;
                }
            }
            if let Some(t) = delta["content"].as_str().filter(|t| !t.is_empty()) {
                out.push_back(Delta::Text(t.to_owned()));
            }
            for call in delta["tool_calls"].as_array().into_iter().flatten() {
                self.tool_call(call, out);
            }
            if choice["finish_reason"] == "length" {
                out.push_back(Delta::Cut);
            }
        }
        if let Some(u) = v.get("usage").filter(|u| u.is_object()) {
            out.push_back(Delta::Usage(Usage {
                input: u["prompt_tokens"].as_u64().unwrap_or(0),
                output: u["completion_tokens"].as_u64().unwrap_or(0),
                cached: u["prompt_tokens_details"]["cached_tokens"]
                    .as_u64()
                    .unwrap_or(0),
            }));
        }
        Ok(())
    }

    fn tool_call(&mut self, call: &Value, out: &mut VecDeque<Delta>) {
        let index = call["index"]
            .as_u64()
            .and_then(|i| usize::try_from(i).ok())
            .unwrap_or(0);
        let func = &call["function"];
        if self.started.insert(index) {
            out.push_back(Delta::ToolStart {
                index,
                id: call["id"].as_str().unwrap_or_default().to_owned(),
                name: func["name"].as_str().unwrap_or_default().to_owned(),
            });
        }
        if let Some(args) = func["arguments"].as_str().filter(|a| !a.is_empty()) {
            out.push_back(Delta::ToolArgs {
                index,
                fragment: args.to_owned(),
            });
        }
    }
}

/// Builds the chat-completions request body.
pub fn body(req: &Request<'_>) -> Value {
    let cache = req.model.starts_with("anthropic/");
    let mut messages = vec![if cache {
        json!({"role": "system", "content": [{"type": "text", "text": req.system, "cache_control": {"type": "ephemeral"}}]})
    } else {
        json!({"role": "system", "content": req.system})
    }];
    let mut shots: Vec<Part> = Vec::new();
    for m in req.messages {
        if !matches!(m, Message::Tool { .. }) {
            flush_shots(&mut messages, &mut shots);
        }
        match m {
            Message::User(parts) => messages.push(user(parts, req.vision)),
            Message::Assistant { text, calls } => {
                if text.is_empty() && calls.is_empty() {
                    continue;
                }
                let mut msg = json!({"role": "assistant", "content": text});
                if !calls.is_empty() {
                    msg["tool_calls"] = calls
                        .iter()
                        .map(|c| {
                            json!({
                                "id": c.id,
                                "type": "function",
                                "function": {"name": c.name, "arguments": c.arguments},
                            })
                        })
                        .collect();
                }
                messages.push(msg);
            }
            Message::Tool {
                call_id,
                text,
                image,
                ..
            } => {
                let mut content = text.clone();
                if image.is_some() && !req.vision {
                    content.push('\n');
                    content.push_str(IMAGE_OMITTED);
                }
                let mut msg = json!({"role": "tool", "tool_call_id": call_id});
                msg["content"] = content.into();
                messages.push(msg);
                if let Some(img) = image.as_ref().filter(|_| req.vision) {
                    shots.push(Part::Text(format!(
                        "Screenshot returned by tool call {call_id}:"
                    )));
                    shots.push(Part::Image(img.clone()));
                }
            }
        }
    }
    flush_shots(&mut messages, &mut shots);
    if cache && let Some(last) = messages.last_mut() {
        cache_tail(&mut last["content"]);
    }
    let mut body = json!({
        "model": req.model,
        "stream": true,
        "stream_options": {"include_usage": true},
        "max_tokens": req.max_tokens,
    });
    body["messages"] = Value::Array(messages);
    if let Some(effort) = req.effort {
        body["reasoning_effort"] = effort.into();
    }
    if !req.tools.is_empty() {
        body["tools"] = req
            .tools
            .iter()
            .map(|t| {
                json!({
                    "type": "function",
                    "function": {"name": t.name, "description": t.description, "parameters": t.parameters},
                })
            })
            .collect();
        if req.forbid_calls {
            body["tool_choice"] = "none".into();
        }
    }
    body
}

/// Marks the last part of `content` as a prompt-cache breakpoint; text content becomes one text part.
fn cache_tail(content: &mut Value) {
    if let Value::String(text) = content
        && !text.is_empty()
    {
        let mut part = json!({"type": "text"});
        part["text"] = std::mem::take(text).into();
        *content = Value::Array(vec![part]);
    }
    if let Some(part) = content.as_array_mut().and_then(|c| c.last_mut()) {
        part["cache_control"] = json!({"type": "ephemeral"});
    }
}

fn flush_shots(messages: &mut Vec<Value>, shots: &mut Vec<Part>) {
    if !shots.is_empty() {
        messages.push(user(shots, true));
        shots.clear();
    }
}

fn user(parts: &[Part], vision: bool) -> Value {
    let content: Vec<Value> = parts
        .iter()
        .map(|p| match p {
            Part::Text(t) => json!({"type": "text", "text": t}),
            Part::Image(img) if vision => {
                let mut part = json!({"type": "image_url", "image_url": {}});
                part["image_url"]["url"] =
                    format!("data:{};base64,{}", image_mime(img), b64(img)).into();
                part
            }
            Part::Image(_) => json!({"type": "text", "text": IMAGE_OMITTED}),
        })
        .collect();
    let mut msg = json!({"role": "user"});
    msg["content"] = Value::Array(content);
    msg
}

#[cfg(test)]
mod tests {
    use bytes::Bytes;

    use super::*;
    use crate::llm::{ToolCall, ToolSpec};

    fn feed_all(data: &[&str]) -> Vec<Delta> {
        let mut p = Parser::default();
        let mut out = VecDeque::new();
        for d in data {
            let event = sse::Event {
                event: String::new(),
                data: (*d).to_owned(),
            };
            p.feed(&event, &mut out).unwrap();
        }
        out.into_iter().collect()
    }

    #[test]
    fn a_reply_stopped_at_its_limit_is_cut() {
        let got = feed_all(&[
            r#"{"choices":[{"delta":{"reasoning_content":"think"}}]}"#,
            r#"{"choices":[{"delta":{},"finish_reason":"length"}]}"#,
            "[DONE]",
        ]);
        assert_eq!(
            got,
            vec![Delta::Reasoning("think".into()), Delta::Cut, Delta::Done]
        );
    }

    #[test]
    fn parses_text_reasoning_tool_calls_and_usage() {
        let got = feed_all(&[
            r#"{"choices":[{"delta":{"reasoning_content":"think"}}]}"#,
            r#"{"choices":[{"delta":{"content":"Hi"}}]}"#,
            r#"{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":""}}]}}]}"#,
            r#"{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}}]}"#,
            r#"{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},"finish_reason":"tool_calls"}]}"#,
            r#"{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4}}}"#,
            "[DONE]",
        ]);
        assert_eq!(
            got,
            vec![
                Delta::Reasoning("think".into()),
                Delta::Text("Hi".into()),
                Delta::ToolStart {
                    index: 0,
                    id: "call_1".into(),
                    name: "shell".into()
                },
                Delta::ToolArgs {
                    index: 0,
                    fragment: "{\"command\":".into()
                },
                Delta::ToolArgs {
                    index: 0,
                    fragment: "\"ls\"}".into()
                },
                Delta::Usage(Usage {
                    input: 10,
                    output: 5,
                    cached: 4
                }),
                Delta::Done,
            ]
        );
    }

    #[test]
    fn an_error_chunk_fails_and_gw_quota_says_the_model_credits_are_used_up() {
        let mut p = Parser::default();
        let mut feed = |data: &str| {
            let event = sse::Event {
                event: String::new(),
                data: data.into(),
            };
            p.feed(&event, &mut VecDeque::new()).unwrap_err()
        };
        assert_eq!(
            feed(r#"{"error":{"message":"quota"}}"#),
            r#"model error: {"message":"quota"}"#
        );
        assert_eq!(
            feed(
                r#"{"error":{"message":"daily cost budget exhausted for user u-1","type":"invalid_request_error","param":null,"code":"service_quota_exceeded_exception"}}"#
            ),
            "model credits are used up: the gw stream returned service_quota_exceeded_exception: daily cost budget exhausted for user u-1; add credit or upgrade the plan, then try again"
        );
    }

    #[test]
    fn body_maps_tools_and_drops_images_for_text_models() {
        let messages = vec![
            Message::User(vec![Part::Text("go".into())]),
            Message::Assistant {
                text: String::new(),
                calls: vec![
                    ToolCall {
                        id: "c1".into(),
                        name: "shell".into(),
                        arguments: "{}".into(),
                    },
                    ToolCall {
                        id: "c2".into(),
                        name: "shell".into(),
                        arguments: "{}".into(),
                    },
                ],
            },
            Message::Tool {
                call_id: "c1".into(),
                text: "ok".into(),
                image: Some(Bytes::from_static(b"\x89PNG")),
                is_error: false,
            },
            Message::Tool {
                call_id: "c2".into(),
                text: "ok".into(),
                image: None,
                is_error: false,
            },
        ];
        let shell = ToolSpec {
            name: "shell".into(),
            description: "run".into(),
            parameters: json!({"type": "object"}),
        };
        let tools = [&shell];
        let mut req = Request {
            model: "deepseek/x",
            system: "sys",
            messages: &messages,
            tools: &tools,
            max_tokens: 100,
            vision: false,
            forbid_calls: false,
            effort: None,
        };
        let text_body = body(&req);
        let msgs = text_body["messages"].as_array().unwrap();
        assert_eq!(msgs.len(), 5);
        assert_eq!(msgs[2]["tool_calls"][0]["function"]["name"], "shell");
        assert!(msgs[3]["content"].as_str().unwrap().contains("omitted"));
        assert_eq!(text_body["tools"][0]["function"]["name"], "shell");
        assert!(text_body.get("tool_choice").is_none());
        assert!(!text_body.to_string().contains("cache_control"));
        assert!(text_body.get("reasoning_effort").is_none());
        let effort = body(&Request {
            effort: Some("medium"),
            ..req
        });
        assert_eq!(effort["reasoning_effort"], "medium");
        req.forbid_calls = true;
        let report_body = body(&req);
        assert_eq!(report_body["tool_choice"], "none");
        assert_eq!(report_body["tools"], text_body["tools"]);
        req.forbid_calls = false;
        req.vision = true;
        req.model = "anthropic/claude-sonnet-5";
        let vision_body = body(&req);
        assert_eq!(
            vision_body["messages"][0]["content"][0]["cache_control"]["type"],
            "ephemeral"
        );
        assert_eq!(
            vision_body["messages"][5]["content"][1]["cache_control"]["type"],
            "ephemeral"
        );
        let msgs = vision_body["messages"].as_array().unwrap();
        let roles: Vec<&str> = msgs.iter().map(|m| m["role"].as_str().unwrap()).collect();
        assert_eq!(
            roles,
            ["system", "user", "assistant", "tool", "tool", "user"]
        );
        assert!(
            msgs[5]["content"][1]["image_url"]["url"]
                .as_str()
                .unwrap()
                .starts_with("data:image/png;base64,")
        );
    }

    #[test]
    fn a_claude_request_caches_its_last_message_even_when_it_is_a_text_tool_result() {
        let messages = vec![
            Message::User(vec![Part::Text("go".into())]),
            Message::Assistant {
                text: String::new(),
                calls: vec![ToolCall {
                    id: "c1".into(),
                    name: "shell".into(),
                    arguments: "{}".into(),
                }],
            },
            Message::Tool {
                call_id: "c1".into(),
                text: "ok".into(),
                image: None,
                is_error: false,
            },
        ];
        let req = Request {
            model: "anthropic/claude-opus-5-5",
            system: "sys",
            messages: &messages,
            tools: &[],
            max_tokens: 100,
            vision: false,
            forbid_calls: false,
            effort: None,
        };
        let b = body(&req);
        let msgs = b["messages"].as_array().unwrap();
        assert_eq!(
            msgs[3],
            json!({"role": "tool", "tool_call_id": "c1", "content": [{"type": "text", "text": "ok", "cache_control": {"type": "ephemeral"}}]})
        );
        assert_eq!(
            msgs[1],
            json!({"role": "user", "content": [{"type": "text", "text": "go"}]})
        );
        assert_eq!(b.to_string().matches("cache_control").count(), 2);
    }
}
