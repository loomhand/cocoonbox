//! Per-agent turn queue and the main agent loop.

use std::collections::HashMap;
use std::fmt::Write as _;
use std::sync::{Arc, Mutex, MutexGuard};

use base64::Engine;
use base64::engine::general_purpose::STANDARD as B64;
use bytes::Bytes;
use serde_json::{Value, json};
use tokio::sync::{mpsc, oneshot};
use tokio_util::sync::CancellationToken;
use tonic::Status;

use crate::bus::Bus;
use crate::config::ServeConfig;
use crate::llm::{self, Delta, Message, Part, Request, ToolCall, ToolSpec, Usage};
use crate::pb::{self, AgentStatus, Role, TranscriptEntry, event, transcript_entry::Body};
use crate::store::Store;
use crate::toolhost::rpc::{self, Call};
use crate::{computer, new_id, prompts, tools};

const QUEUE_DEPTH: usize = 32;
const MAIN_MAX_STEPS: usize = 40;
const MAIN_MAX_TOKENS: u32 = 4096;
const INTERRUPTED: &str = "(no result: the turn was interrupted)";

/// Shared state of `serve`: store, event bus, model client and per-agent queues.
pub struct Runtime {
    pub cfg: ServeConfig,
    pub store: Store,
    pub bus: Bus,
    pub(crate) llm: llm::Client,
    pub(crate) computer_tools: Vec<ToolSpec>,
    pub(crate) screen: tokio::sync::Mutex<()>,
    toolhost: rpc::Client,
    main_tools: Vec<ToolSpec>,
    slots: Mutex<HashMap<String, Arc<Slot>>>,
}

impl Runtime {
    /// Opens the store, closes turns left open by a previous process, and builds the runtime.
    pub async fn open(cfg: ServeConfig) -> Result<Arc<Self>, String> {
        std::fs::create_dir_all(&cfg.state_dir)
            .map_err(|e| format!("create {}: {e}", cfg.state_dir.display()))?;
        let store = Store::open(&cfg.state_dir.join("agent.db"))?;
        let rt = Arc::new(Self {
            llm: llm::Client::new(&cfg.gw_base_url, &cfg.gw_api_key)?,
            toolhost: rpc::Client::new(cfg.toolhost_socket.clone()),
            store,
            bus: Bus::new(),
            main_tools: tools::main_tools(),
            computer_tools: tools::computer_tools(),
            screen: tokio::sync::Mutex::new(()),
            slots: Mutex::new(HashMap::new()),
            cfg,
        });
        for (agent_id, turn_id) in rt.store.unfinished_turns().await? {
            let end = pb::TurnEnd {
                ok: false,
                error: "interrupted: the agent restarted".to_owned(),
                usage: None,
            };
            rt.record_raw(&agent_id, &turn_id, Role::System, Body::TurnEnd(end), false)
                .await?;
            rt.store.finish_turn(turn_id).await?;
            rt.set_status(&agent_id, AgentStatus::Failed).await;
        }
        Ok(rt)
    }

    /// Creates an agent; an empty model selects the box default.
    pub async fn create_agent(&self, req: pb::CreateAgentRequest) -> Result<pb::Agent, Status> {
        let model = if req.main_model.is_empty() {
            self.cfg.model_main.clone()
        } else {
            req.main_model
        };
        let name = if req.name.is_empty() {
            "agent".to_owned()
        } else {
            req.name
        };
        let agent = self
            .store
            .create_agent(name, model, req.client_nonce)
            .await
            .map_err(Status::internal)?;
        self.bus.publish(event::Body::Agent(agent.clone()));
        Ok(agent)
    }

    /// Queues a user turn and returns its id; duplicate nonces return the first turn.
    pub async fn send(self: &Arc<Self>, req: pb::SendRequest) -> Result<String, Status> {
        if self
            .store
            .agent(req.agent_id.clone())
            .await
            .map_err(Status::internal)?
            .is_none()
        {
            return Err(Status::not_found(format!(
                "agent {} not found",
                req.agent_id
            )));
        }
        let mut text = req.text;
        for a in req.attachments {
            let path = format!("uploads/{}", sanitize(&a.name));
            let call = Call {
                agent_id: req.agent_id.clone(),
                tool: "upload".to_owned(),
                args: json!({"path": path, "data_b64": B64.encode(&a.data)}),
            };
            let out = self
                .toolhost
                .call(&call)
                .await
                .map_err(Status::unavailable)?;
            if out.is_error {
                return Err(Status::internal(out.text));
            }
            let _ = write!(
                text,
                "\n[attached file: {} ({}, {} bytes)]",
                out.text,
                a.mime_type,
                a.data.len()
            );
        }
        let queued = self
            .store
            .queue_turn(req.agent_id.clone(), text, req.client_nonce)
            .await
            .map_err(Status::internal)?;
        let Some(entry) = queued.entry else {
            return Ok(queued.turn_id);
        };
        self.bus.publish(event::Body::Transcript(entry));
        let slot = self.slot(&req.agent_id);
        if slot.queue.try_send(queued.turn_id.clone()).is_err() {
            let end = pb::TurnEnd {
                ok: false,
                error: "turn queue is full".to_owned(),
                usage: None,
            };
            self.record_raw(
                &req.agent_id,
                &queued.turn_id,
                Role::System,
                Body::TurnEnd(end),
                false,
            )
            .await
            .map_err(Status::internal)?;
            self.store
                .finish_turn(queued.turn_id)
                .await
                .map_err(Status::internal)?;
            return Err(Status::resource_exhausted("turn queue is full"));
        }
        Ok(queued.turn_id)
    }

    /// Delivers the user's answer to a pending approval.
    pub fn respond(&self, req: pb::RespondRequest) -> Result<(), Status> {
        let slot = self.existing_slot(&req.agent_id);
        let tx = slot
            .and_then(|s| lock(&s.state).approvals.remove(&req.approval_id))
            .ok_or_else(|| {
                Status::not_found(format!("approval {} is not pending", req.approval_id))
            })?;
        tx.send(Reply {
            approve: req.approve,
            text: req.text,
        })
        .map_err(|_| Status::failed_precondition("the turn waiting for this approval has ended"))
    }

    /// Aborts the agent's running turn, if any.
    pub fn cancel(&self, agent_id: &str) {
        if let Some(slot) = self.existing_slot(agent_id)
            && let Some(token) = &lock(&slot.state).cancel
        {
            token.cancel();
        }
    }

    /// Runs one tool in the toolhost; transport failures become tool errors.
    pub async fn tool(&self, agent_id: &str, name: &str, args: Value) -> ToolOutput {
        let call = Call {
            agent_id: agent_id.to_owned(),
            tool: name.to_owned(),
            args,
        };
        match self.toolhost.call(&call).await {
            Ok(out) => ToolOutput {
                image: out
                    .image_b64
                    .and_then(|b| B64.decode(b).ok())
                    .map(Bytes::from),
                text: out.text,
                is_error: out.is_error,
            },
            Err(e) => ToolOutput::error(e),
        }
    }

    /// Streams one model step, recording blocks and tool calls as they complete.
    pub async fn step(
        &self,
        ctx: &TurnCtx,
        req: &Request<'_>,
        subagent: &str,
    ) -> Result<StepOut, String> {
        let mut stream = self.llm.stream(req).await?;
        let mut text = String::new();
        let mut pending = String::new();
        let mut reasoning = String::new();
        let mut calls: Vec<(usize, ToolCall)> = Vec::new();
        let mut usage = Usage::default();
        while let Some(d) = stream.next().await? {
            match d {
                Delta::Reasoning(t) => reasoning.push_str(&t),
                Delta::Text(t) => {
                    self.flush_reasoning(ctx, &mut reasoning, subagent).await?;
                    pending.push_str(&t);
                }
                Delta::ToolStart { index, id, name } => {
                    self.flush_reasoning(ctx, &mut reasoning, subagent).await?;
                    self.flush_text(ctx, &mut pending, &mut text, subagent)
                        .await?;
                    calls.push((
                        index,
                        ToolCall {
                            id,
                            name,
                            arguments: String::new(),
                        },
                    ));
                }
                Delta::ToolArgs { index, fragment } => {
                    if let Some((_, c)) = calls.iter_mut().rev().find(|(i, _)| *i == index) {
                        c.arguments.push_str(&fragment);
                    }
                }
                Delta::Usage(u) => usage = u,
                Delta::Done => break,
            }
        }
        self.flush_reasoning(ctx, &mut reasoning, subagent).await?;
        self.flush_text(ctx, &mut pending, &mut text, subagent)
            .await?;
        let mut out = Vec::with_capacity(calls.len());
        for (n, (_, mut call)) in calls.into_iter().enumerate() {
            if call.id.is_empty() {
                call.id = format!(
                    "call_{}_{n}",
                    &ctx.turn_id[ctx.turn_id.len().saturating_sub(8)..]
                );
            }
            if call.arguments.trim().is_empty() {
                "{}".clone_into(&mut call.arguments);
            }
            let block = pb::ToolCall {
                call_id: call.id.clone(),
                name: call.name.clone(),
                arguments_json: call.arguments.clone(),
                subagent: subagent.to_owned(),
            };
            self.record(
                ctx,
                Role::Assistant,
                Body::ToolCall(block),
                !subagent.is_empty(),
            )
            .await?;
            out.push(call);
        }
        Ok(StepOut {
            text,
            calls: out,
            usage,
        })
    }

    /// Persists an entry of the current turn and publishes it.
    pub async fn record(
        &self,
        ctx: &TurnCtx,
        role: Role,
        body: Body,
        subagent: bool,
    ) -> Result<(), String> {
        self.record_raw(&ctx.agent_id, &ctx.turn_id, role, body, subagent)
            .await
    }

    /// Publishes a ComputerAction event for an input the subagent is about to inject.
    pub fn announce_action(&self, ctx: &TurnCtx, args: &Value) {
        let coord = |k: &str| args[k].as_f64().map_or(0, |v| v.round() as i32);
        let text = ["text", "key", "app"]
            .iter()
            .find_map(|k| args[*k].as_str().map(str::to_owned))
            .or_else(|| {
                args["keys"].as_array().map(|ks| {
                    ks.iter()
                        .filter_map(Value::as_str)
                        .collect::<Vec<_>>()
                        .join("+")
                })
            })
            .or_else(|| args["element"].as_i64().map(|i| format!("element {i}")))
            .unwrap_or_default();
        self.bus
            .publish(event::Body::ComputerAction(pb::ComputerAction {
                agent_id: ctx.agent_id.clone(),
                action: args["action"].as_str().unwrap_or_default().to_owned(),
                x: coord("x"),
                y: coord("y"),
                text,
            }));
    }

    fn slot(self: &Arc<Self>, agent_id: &str) -> Arc<Slot> {
        let mut slots = lock(&self.slots);
        if let Some(s) = slots.get(agent_id) {
            return Arc::clone(s);
        }
        let (tx, rx) = mpsc::channel(QUEUE_DEPTH);
        let slot = Arc::new(Slot {
            queue: tx,
            state: Mutex::new(SlotState::default()),
        });
        slots.insert(agent_id.to_owned(), Arc::clone(&slot));
        // detached worker: runs this agent's turns in order for the life of the process
        tokio::spawn(worker(
            Arc::clone(self),
            agent_id.to_owned(),
            Arc::clone(&slot),
            rx,
        ));
        slot
    }

    fn existing_slot(&self, agent_id: &str) -> Option<Arc<Slot>> {
        lock(&self.slots).get(agent_id).cloned()
    }

    async fn run_turn(&self, agent_id: &str, slot: Arc<Slot>, turn_id: String) {
        let model = match self.store.agent(agent_id.to_owned()).await {
            Ok(Some(a)) => a.main_model,
            _ => self.cfg.model_main.clone(),
        };
        let cancel = CancellationToken::new();
        lock(&slot.state).cancel = Some(cancel.clone());
        let ctx = TurnCtx {
            agent_id: agent_id.to_owned(),
            turn_id,
            model,
            slot,
        };
        self.set_status(agent_id, AgentStatus::Running).await;
        let mut usage = Usage::default();
        let result = tokio::select! {
            r = self.main_loop(&ctx, &mut usage) => r,
            () = cancel.cancelled() => Err("cancelled".to_owned()),
        };
        {
            let mut state = lock(&ctx.slot.state);
            state.cancel = None;
            state.approvals.clear();
        }
        let (ok, error) = match result {
            Ok(()) => (true, String::new()),
            Err(e) => (false, e),
        };
        let end = pb::TurnEnd {
            ok,
            error,
            usage: Some(pb::Usage {
                input_tokens: usage.input,
                output_tokens: usage.output,
                cached_input_tokens: usage.cached,
            }),
        };
        if let Err(e) = self
            .record(&ctx, Role::System, Body::TurnEnd(end), false)
            .await
        {
            eprintln!("record turn end {}: {e}", ctx.turn_id);
        }
        if let Err(e) = self.store.finish_turn(ctx.turn_id.clone()).await {
            eprintln!("finish turn {}: {e}", ctx.turn_id);
        }
        let status = if ok {
            AgentStatus::Idle
        } else {
            AgentStatus::Failed
        };
        self.set_status(agent_id, status).await;
    }

    async fn main_loop(&self, ctx: &TurnCtx, usage: &mut Usage) -> Result<(), String> {
        let history = self
            .store
            .history(ctx.agent_id.clone(), ctx.turn_id.clone())
            .await?;
        let mut messages = history_messages(history);
        for _ in 0..MAIN_MAX_STEPS {
            let req = Request {
                model: &ctx.model,
                system: prompts::MAIN,
                messages: &messages,
                tools: &self.main_tools,
                max_tokens: MAIN_MAX_TOKENS,
                vision: false,
            };
            let step = self.step(ctx, &req, "").await?;
            usage.add(step.usage);
            if step.calls.is_empty() {
                return Ok(());
            }
            let mut results = Vec::with_capacity(step.calls.len());
            for call in &step.calls {
                let out = self.main_tool(ctx, call, usage).await;
                results.push(self.record_result(ctx, &call.id, out, false).await?);
            }
            messages.push(Message::Assistant {
                text: step.text,
                calls: step.calls,
            });
            messages.extend(results);
        }
        Err(format!(
            "stopped after {MAIN_MAX_STEPS} steps without a final answer"
        ))
    }

    async fn main_tool(&self, ctx: &TurnCtx, call: &ToolCall, usage: &mut Usage) -> ToolOutput {
        let args: Value = match serde_json::from_str(&call.arguments) {
            Ok(v) => v,
            Err(e) => return ToolOutput::error(format!("arguments are not valid JSON: {e}")),
        };
        match call.name.as_str() {
            "delegate_computer" => {
                let task = args["task"].as_str().unwrap_or_default();
                if task.is_empty() {
                    return ToolOutput::error("delegate_computer needs a task");
                }
                computer::run(self, ctx, task, usage)
                    .await
                    .unwrap_or_else(|e| ToolOutput::error(format!("computer subagent failed: {e}")))
            }
            "request_take_over" => {
                let reason = args["reason"].as_str().unwrap_or("The agent needs you.");
                self.take_over(ctx, reason)
                    .await
                    .unwrap_or_else(ToolOutput::error)
            }
            name => self.tool(&ctx.agent_id, name, args).await,
        }
    }

    /// Records a tool result and returns the message form for the model.
    pub async fn record_result(
        &self,
        ctx: &TurnCtx,
        call_id: &str,
        out: ToolOutput,
        subagent: bool,
    ) -> Result<Message, String> {
        let block = pb::ToolResult {
            call_id: call_id.to_owned(),
            is_error: out.is_error,
            text: out.text.clone(),
            image: out.image.clone().unwrap_or_default(),
        };
        self.record(ctx, Role::Tool, Body::ToolResult(block), subagent)
            .await?;
        Ok(Message::Tool {
            call_id: call_id.to_owned(),
            text: out.text,
            image: out.image,
            is_error: out.is_error,
        })
    }

    async fn take_over(&self, ctx: &TurnCtx, reason: &str) -> Result<ToolOutput, String> {
        let approval_id = new_id("ap");
        let (tx, rx) = oneshot::channel();
        lock(&ctx.slot.state)
            .approvals
            .insert(approval_id.clone(), tx);
        let block = pb::ApprovalRequest {
            approval_id,
            summary: reason.to_owned(),
            kind: "take_over".to_owned(),
        };
        self.record(ctx, Role::Assistant, Body::Approval(block), false)
            .await?;
        self.set_status(&ctx.agent_id, AgentStatus::WaitingForUser)
            .await;
        let reply = rx
            .await
            .map_err(|_| "the take-over was abandoned".to_owned())?;
        self.set_status(&ctx.agent_id, AgentStatus::Running).await;
        let mut text = if reply.approve {
            "The user finished the take-over; continue from the current screen state.".to_owned()
        } else {
            "The user declined the take-over.".to_owned()
        };
        if !reply.text.is_empty() {
            let _ = write!(text, " User note: {}", reply.text);
        }
        Ok(ToolOutput {
            text,
            image: None,
            is_error: !reply.approve,
        })
    }

    async fn flush_reasoning(
        &self,
        ctx: &TurnCtx,
        reasoning: &mut String,
        subagent: &str,
    ) -> Result<(), String> {
        if reasoning.is_empty() {
            return Ok(());
        }
        let text = std::mem::take(reasoning);
        if !subagent.is_empty() {
            return Ok(());
        }
        self.record(
            ctx,
            Role::Assistant,
            Body::Reasoning(pb::ReasoningBlock { text }),
            false,
        )
        .await
    }

    async fn flush_text(
        &self,
        ctx: &TurnCtx,
        pending: &mut String,
        text: &mut String,
        subagent: &str,
    ) -> Result<(), String> {
        if pending.trim().is_empty() {
            text.push_str(pending);
            pending.clear();
            return Ok(());
        }
        text.push_str(pending);
        let block = std::mem::take(pending);
        if !subagent.is_empty() {
            return Ok(());
        }
        self.record(
            ctx,
            Role::Assistant,
            Body::Text(pb::TextBlock { text: block }),
            false,
        )
        .await
    }

    async fn record_raw(
        &self,
        agent_id: &str,
        turn_id: &str,
        role: Role,
        body: Body,
        subagent: bool,
    ) -> Result<(), String> {
        let entry = TranscriptEntry {
            agent_id: agent_id.to_owned(),
            turn_id: turn_id.to_owned(),
            role: role.into(),
            body: Some(body),
            ..Default::default()
        };
        let entry = self.store.append(entry, subagent).await?;
        self.bus.publish(event::Body::Transcript(entry));
        Ok(())
    }

    async fn set_status(&self, agent_id: &str, status: AgentStatus) {
        match self.store.set_status(agent_id.to_owned(), status).await {
            Ok(Some(agent)) => {
                self.bus.publish(event::Body::Agent(agent));
            }
            Ok(None) => {}
            Err(e) => eprintln!("set status of {agent_id}: {e}"),
        }
    }
}

/// Result of a tool as recorded in the transcript.
pub struct ToolOutput {
    pub text: String,
    pub image: Option<Bytes>,
    pub is_error: bool,
}

impl ToolOutput {
    /// A failed tool result.
    pub fn error(text: impl Into<String>) -> Self {
        Self {
            text: text.into(),
            image: None,
            is_error: true,
        }
    }
}

/// Identity of the running turn.
pub struct TurnCtx {
    pub agent_id: String,
    pub turn_id: String,
    model: String,
    slot: Arc<Slot>,
}

/// One model step: its text, requested tool calls and usage.
pub struct StepOut {
    pub text: String,
    pub calls: Vec<ToolCall>,
    pub usage: Usage,
}

struct Slot {
    queue: mpsc::Sender<String>,
    state: Mutex<SlotState>,
}

#[derive(Default)]
struct SlotState {
    cancel: Option<CancellationToken>,
    approvals: HashMap<String, oneshot::Sender<Reply>>,
}

struct Reply {
    approve: bool,
    text: String,
}

/// Rebuilds the main agent's model messages from its transcript.
pub fn history_messages(entries: Vec<TranscriptEntry>) -> Vec<Message> {
    let mut out: Vec<Message> = Vec::new();
    let mut open: Vec<String> = Vec::new();
    for e in entries {
        let user = e.role == i32::from(Role::User);
        match e.body {
            Some(Body::Text(t)) if user => {
                close_open(&mut out, &mut open);
                out.push(Message::User(vec![Part::Text(t.text)]));
            }
            Some(Body::Text(t)) => {
                close_open(&mut out, &mut open);
                match out.last_mut() {
                    Some(Message::Assistant { text, calls }) if calls.is_empty() => {
                        text.push_str(&t.text);
                    }
                    _ => out.push(Message::Assistant {
                        text: t.text,
                        calls: Vec::new(),
                    }),
                }
            }
            Some(Body::ToolCall(c)) => {
                open.push(c.call_id.clone());
                let call = ToolCall {
                    id: c.call_id,
                    name: c.name,
                    arguments: c.arguments_json,
                };
                match out.last_mut() {
                    Some(Message::Assistant { calls, .. }) => calls.push(call),
                    _ => out.push(Message::Assistant {
                        text: String::new(),
                        calls: vec![call],
                    }),
                }
            }
            Some(Body::ToolResult(r)) => {
                if let Some(pos) = open.iter().position(|id| *id == r.call_id) {
                    open.remove(pos);
                    out.push(Message::Tool {
                        call_id: r.call_id,
                        text: r.text,
                        image: None,
                        is_error: r.is_error,
                    });
                }
            }
            _ => {}
        }
    }
    close_open(&mut out, &mut open);
    out
}

fn close_open(out: &mut Vec<Message>, open: &mut Vec<String>) {
    for call_id in open.drain(..) {
        out.push(Message::Tool {
            call_id,
            text: INTERRUPTED.to_owned(),
            image: None,
            is_error: true,
        });
    }
}

async fn worker(
    rt: Arc<Runtime>,
    agent_id: String,
    slot: Arc<Slot>,
    mut rx: mpsc::Receiver<String>,
) {
    while let Some(turn_id) = rx.recv().await {
        rt.run_turn(&agent_id, Arc::clone(&slot), turn_id).await;
    }
}

fn lock<T>(m: &Mutex<T>) -> MutexGuard<'_, T> {
    m.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
}

fn sanitize(name: &str) -> String {
    let base = name.rsplit('/').next().unwrap_or(name);
    let clean: String = base
        .chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || "._-".contains(c) {
                c
            } else {
                '_'
            }
        })
        .collect();
    if clean.is_empty() || clean.starts_with('.') {
        format!("file{clean}")
    } else {
        clean
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn entry(role: Role, body: Body) -> TranscriptEntry {
        TranscriptEntry {
            role: role.into(),
            body: Some(body),
            ..Default::default()
        }
    }

    fn text(s: &str) -> Body {
        Body::Text(pb::TextBlock { text: s.into() })
    }

    fn call(id: &str) -> Body {
        Body::ToolCall(pb::ToolCall {
            call_id: id.into(),
            name: "shell".into(),
            arguments_json: "{}".into(),
            subagent: String::new(),
        })
    }

    #[test]
    fn history_groups_steps_and_closes_interrupted_calls() {
        let entries = vec![
            entry(Role::User, text("do it")),
            entry(Role::Assistant, text("ok")),
            entry(Role::Assistant, call("c1")),
            entry(Role::Assistant, call("c2")),
            entry(
                Role::Tool,
                Body::ToolResult(pb::ToolResult {
                    call_id: "c1".into(),
                    text: "done".into(),
                    ..Default::default()
                }),
            ),
            entry(Role::System, Body::TurnEnd(pb::TurnEnd::default())),
            entry(Role::User, text("next")),
        ];
        let msgs = history_messages(entries);
        assert_eq!(msgs.len(), 5, "{msgs:?}");
        let Message::Assistant { text, calls } = &msgs[1] else {
            panic!("want assistant, got {:?}", msgs[1]);
        };
        assert_eq!(text, "ok");
        assert_eq!(calls.len(), 2);
        assert!(
            matches!(&msgs[2], Message::Tool { call_id, is_error: false, .. } if call_id == "c1")
        );
        assert!(
            matches!(&msgs[3], Message::Tool { call_id, is_error: true, .. } if call_id == "c2")
        );
        assert!(matches!(&msgs[4], Message::User(_)));
    }

    #[test]
    fn sanitize_strips_paths_and_hidden_names() {
        assert_eq!(sanitize("../../etc/passwd"), "passwd");
        assert_eq!(sanitize(".bashrc"), "file.bashrc");
        assert_eq!(sanitize("a b.png"), "a_b.png");
    }
}
