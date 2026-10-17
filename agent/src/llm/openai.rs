//! OpenAI chat-completions request body and SSE parser.

use std::collections::{HashSet, VecDeque};

use serde_json::{Value, json};

use super::{Delta, IMAGE_OMITTED, Message, Part, Request, Usage, b64, image_mime, sse};

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
            return Err(format!("model error: {err}"));
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
    if cache
        && let Some(part) = messages
            .iter_mut()
            .rev()
            .find(|m| m["role"] == "user")
            .and_then(|m| m["content"].as_array_mut())
            .and_then(|c| c.last_mut())
    {
        part["cache_control"] = json!({"type": "ephemeral"});
    }
    let mut body = json!({
        "model": req.model,
        "stream": true,
        "stream_options": {"include_usage": true},
        "max_tokens": req.max_tokens,
    });
    body["messages"] = Value::Array(messages);
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
    if let Some(effort) = req.effort {
        body["reasoning"] = json!({"effort": effort});
    }
    body
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
    fn error_chunk_fails() {
        let mut p = Parser::default();
        let event = sse::Event {
            event: String::new(),
            data: r#"{"error":{"message":"quota"}}"#.into(),
        };
        assert!(p.feed(&event, &mut VecDeque::new()).is_err());
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
        assert!(text_body.get("reasoning").is_none());
        req.forbid_calls = true;
        let report_body = body(&req);
        assert_eq!(report_body["tool_choice"], "none");
        assert_eq!(report_body["tools"], text_body["tools"]);
        req.forbid_calls = false;
        req.vision = true;
        req.model = "anthropic/claude-sonnet-5";
        req.effort = Some("low");
        let vision_body = body(&req);
        assert_eq!(vision_body["reasoning"], json!({"effort": "low"}));
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
}
