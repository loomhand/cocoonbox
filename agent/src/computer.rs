//! The computer subagent: a vision model driving the desktop through cua-driver.

use std::sync::Arc;

use serde_json::Value;
use tokio_util::task::TaskTracker;

use crate::dispatch::{Dispatcher, ToolOutput};
use crate::llm::{Message, Part, Request, ToolSpec, Usage};
use crate::new_id;
use crate::runner::{Runtime, TurnCtx};

const SUBAGENT: &str = "computer";
const MAX_STEPS: usize = 30;
const MAX_TOKENS: u32 = 2048;
const KEEP_IMAGES: usize = 3;
const PRUNE_AT: usize = 6;
const IMAGE_DROPPED: &str = "[older screenshot removed]";

/// Runs `task` to completion in its own driver session and returns the report with a final screenshot.
pub async fn run(
    rt: &Runtime,
    ctx: &TurnCtx,
    task: &str,
    usage: &mut Usage,
) -> Result<ToolOutput, String> {
    let _screen = rt.screen.lock().await;
    let driver = new_id("cu");
    let mut guard = EndOnDrop {
        dispatcher: Arc::clone(&rt.dispatcher),
        tasks: rt.tasks.clone(),
        owner: ctx.agent_id.clone(),
        driver: driver.clone(),
        armed: true,
    };
    let result = drive(rt, ctx, task, &driver, usage).await;
    guard.armed = false;
    let end = rt.dispatcher.end_driver(&ctx.agent_id, &driver).await;
    if end.is_error {
        eprintln!("end cua session {driver}: {}", end.text);
    }
    result
}

/// Ends the driver session from a detached task when a cancelled turn drops the subagent.
struct EndOnDrop {
    dispatcher: Arc<Dispatcher>,
    tasks: TaskTracker,
    owner: String,
    driver: String,
    armed: bool,
}

impl Drop for EndOnDrop {
    fn drop(&mut self) {
        if !self.armed {
            return;
        }
        let dispatcher = Arc::clone(&self.dispatcher);
        let owner = std::mem::take(&mut self.owner);
        let driver = std::mem::take(&mut self.driver);
        self.tasks.spawn(async move {
            dispatcher.end_driver(&owner, &driver).await;
        });
    }
}

async fn drive(
    rt: &Runtime,
    ctx: &TurnCtx,
    task: &str,
    driver: &str,
    usage: &mut Usage,
) -> Result<ToolOutput, String> {
    let caller = ctx.caller(SUBAGENT, Some(driver));
    let mut messages = vec![Message::User(vec![Part::Text(task.to_owned())])];
    let mut report = None;
    let tools: Vec<&ToolSpec> = rt.computer_tools.iter().collect();
    for _ in 0..MAX_STEPS {
        // prune in batches so the cached prompt prefix survives most steps
        if image_count(&messages) > PRUNE_AT {
            prune_images(&mut messages, KEEP_IMAGES);
        }
        let req = Request {
            model: &rt.cfg.model_computer,
            system: &rt.computer_prompt,
            messages: &messages,
            tools: &tools,
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
                Ok(args @ Value::Object(_)) => {
                    rt.dispatcher
                        .execute(&caller, &call.id, &call.name, args)
                        .await?
                }
                parsed => {
                    let out = ToolOutput::error(match parsed {
                        Ok(_) => "arguments must be a JSON object".to_owned(),
                        Err(e) => format!("arguments are not valid JSON: {e}"),
                    });
                    rt.dispatcher.record_result(&caller, &call.id, &out).await?;
                    out
                }
            };
            results.push(Message::Tool {
                call_id: call.id.clone(),
                text: out.text,
                image: out.image,
                is_error: out.is_error,
            });
        }
        messages.push(Message::Assistant {
            text: step.text,
            calls: step.calls,
        });
        messages.extend(results);
    }
    let shot = rt.dispatcher.capture(&ctx.agent_id, driver).await;
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

fn image_count(messages: &[Message]) -> usize {
    messages
        .iter()
        .filter(|m| matches!(m, Message::Tool { image: Some(_), .. }))
        .count()
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
