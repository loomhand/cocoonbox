//! The computer subagent: a vision model driving the desktop through cua-driver.

use serde_json::{Value, json};

use crate::llm::{Message, Part, Request, Usage};
use crate::runner::{Runtime, ToolOutput, TurnCtx};
use crate::toolhost::rpc::{self, Call};
use crate::{new_id, prompts};

const SUBAGENT: &str = "computer";
const MAX_STEPS: usize = 30;
const MAX_TOKENS: u32 = 2048;
const KEEP_IMAGES: usize = 3;
const IMAGE_DROPPED: &str = "[older screenshot removed]";

/// Runs `task` to completion in its own driver session and returns the report with a final screenshot.
pub async fn run(
    rt: &Runtime,
    ctx: &TurnCtx,
    task: &str,
    usage: &mut Usage,
) -> Result<ToolOutput, String> {
    let _screen = rt.screen.lock().await;
    let session = new_id("cu");
    let mut guard = EndOnDrop {
        toolhost: rt.toolhost().clone(),
        agent_id: ctx.agent_id.clone(),
        session: session.clone(),
        armed: true,
    };
    let result = drive(rt, ctx, task, &session, usage).await;
    guard.armed = false;
    let end = rt
        .tool(&ctx.agent_id, "computer_end", json!({"session": session}))
        .await;
    if end.is_error {
        eprintln!("end cua session {session}: {}", end.text);
    }
    result
}

/// Ends the driver session from a detached task when a cancelled turn drops the subagent.
struct EndOnDrop {
    toolhost: rpc::Client,
    agent_id: String,
    session: String,
    armed: bool,
}

impl Drop for EndOnDrop {
    fn drop(&mut self) {
        if !self.armed {
            return;
        }
        let call = Call {
            agent_id: std::mem::take(&mut self.agent_id),
            tool: "computer_end".to_owned(),
            args: json!({"session": std::mem::take(&mut self.session)}),
        };
        let toolhost = self.toolhost.clone();
        tokio::spawn(async move {
            let _ = toolhost.call(&call).await;
        });
    }
}

async fn drive(
    rt: &Runtime,
    ctx: &TurnCtx,
    task: &str,
    session: &str,
    usage: &mut Usage,
) -> Result<ToolOutput, String> {
    let mut messages = vec![Message::User(vec![Part::Text(task.to_owned())])];
    let mut report = None;
    for _ in 0..MAX_STEPS {
        prune_images(&mut messages, KEEP_IMAGES);
        let req = Request {
            model: &rt.cfg.model_computer,
            system: prompts::COMPUTER,
            messages: &messages,
            tools: &rt.computer_tools,
            max_tokens: MAX_TOKENS,
            vision: true,
        };
        let step = rt.step(ctx, &req, SUBAGENT).await?;
        usage.add(step.usage);
        if step.calls.is_empty() {
            report = Some(step.text);
            break;
        }
        let mut results = Vec::with_capacity(step.calls.len());
        for call in &step.calls {
            let out = match serde_json::from_str::<Value>(&call.arguments) {
                Ok(Value::Object(mut args)) => {
                    args.insert("session".to_owned(), session.into());
                    let args = Value::Object(args);
                    if call.name == "computer_act" {
                        rt.announce_action(ctx, &args);
                    }
                    rt.tool(&ctx.agent_id, &call.name, args).await
                }
                Ok(_) => ToolOutput::error("arguments must be a JSON object"),
                Err(e) => ToolOutput::error(format!("arguments are not valid JSON: {e}")),
            };
            results.push(rt.record_result(ctx, &call.id, out, true).await?);
        }
        messages.push(Message::Assistant {
            text: step.text,
            calls: step.calls,
        });
        messages.extend(results);
    }
    let shot = rt
        .tool(&ctx.agent_id, "screenshot", json!({"session": session}))
        .await;
    let (text, is_error) = match report {
        Some(r) if !r.trim().is_empty() => (r, false),
        Some(_) => (
            "The computer subagent finished without a report.".to_owned(),
            false,
        ),
        None => (
            format!(
                "The computer subagent stopped after {MAX_STEPS} steps; the task may be incomplete."
            ),
            true,
        ),
    };
    Ok(ToolOutput {
        text: format!("{text}\n[the final screenshot is attached to this result for the user]"),
        image: shot.image,
        is_error,
    })
}

/// Keeps only the newest `keep` screenshots in the conversation.
fn prune_images(messages: &mut [Message], keep: usize) {
    let mut seen = 0;
    for m in messages.iter_mut().rev() {
        if let Message::Tool { image, text, .. } = m
            && image.is_some()
        {
            seen += 1;
            if seen > keep {
                *image = None;
                text.push('\n');
                text.push_str(IMAGE_DROPPED);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use bytes::Bytes;

    use super::*;

    #[test]
    fn prune_keeps_newest_images() {
        let tool = |n: u8| Message::Tool {
            call_id: format!("c{n}"),
            text: String::new(),
            image: Some(Bytes::from(vec![n])),
            is_error: false,
        };
        let mut msgs: Vec<Message> = (0..5).map(tool).collect();
        prune_images(&mut msgs, 2);
        let kept: Vec<bool> = msgs
            .iter()
            .map(|m| matches!(m, Message::Tool { image: Some(_), .. }))
            .collect();
        assert_eq!(kept, [false, false, false, true, true]);
        prune_images(&mut msgs, 2);
        let Message::Tool { text, .. } = &msgs[0] else {
            unreachable!()
        };
        assert_eq!(text.matches(IMAGE_DROPPED).count(), 1);
    }
}
