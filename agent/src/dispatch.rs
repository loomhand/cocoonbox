//! The tool dispatcher: the one path from every transport and the loop down to the tools.

use std::sync::Arc;

use base64::Engine;
use base64::engine::general_purpose::STANDARD as B64;
use bytes::Bytes;
use serde_json::{Value, json};

use crate::journal::Journal;
use crate::pb::{self, Role, event, transcript_entry::Body};
use crate::toolhost::rpc::{self, Call};

const DRIVER_TOOLS: [&str; 3] = ["computer_state", "computer_act", "screenshot"];

/// Executes tools on the box; the toolhost RPC client is the production implementation.
#[tonic::async_trait]
pub trait Tools: Send + Sync {
    /// Runs tool `name`; `owner` keys per-agent state such as the shell session.
    async fn run(&self, owner: &str, name: &str, args: Value) -> ToolOutput;
}

#[tonic::async_trait]
impl Tools for rpc::Client {
    async fn run(&self, owner: &str, name: &str, args: Value) -> ToolOutput {
        let call = Call {
            agent_id: owner.to_owned(),
            tool: name.to_owned(),
            args,
        };
        match self.call(&call).await {
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

/// Where a call's entries go and which desktop session it drives.
pub struct Caller<'a> {
    pub agent_id: &'a str,
    pub turn_id: &'a str,
    pub subagent: &'a str,
    pub driver: Option<&'a str>,
}

/// Records every call and result and routes execution to [`Tools`]; it never knows who asked.
pub struct Dispatcher {
    tools: Arc<dyn Tools>,
    journal: Arc<Journal>,
}

impl Dispatcher {
    /// Creates a dispatcher over `tools` that writes history to `journal`.
    pub fn new(tools: Arc<dyn Tools>, journal: Arc<Journal>) -> Self {
        Self { tools, journal }
    }

    /// Records a call and runs it.
    pub async fn call(
        &self,
        c: &Caller<'_>,
        call_id: &str,
        name: &str,
        args: Value,
    ) -> Result<ToolOutput, String> {
        self.record_call(c, call_id, name, &args.to_string())
            .await?;
        self.execute(c, call_id, name, args).await
    }

    /// Records a requested call before it runs.
    pub async fn record_call(
        &self,
        c: &Caller<'_>,
        call_id: &str,
        name: &str,
        arguments: &str,
    ) -> Result<(), String> {
        let body = Body::ToolCall(pb::ToolCall {
            call_id: call_id.to_owned(),
            name: name.to_owned(),
            arguments_json: arguments.to_owned(),
            subagent: c.subagent.to_owned(),
        });
        self.journal
            .record(
                c.agent_id,
                c.turn_id,
                Role::Assistant,
                body,
                !c.subagent.is_empty(),
            )
            .await
    }

    /// Runs a recorded call and records its result.
    pub async fn execute(
        &self,
        c: &Caller<'_>,
        call_id: &str,
        name: &str,
        args: Value,
    ) -> Result<ToolOutput, String> {
        let out = self.run(c.agent_id, c.driver, name, args).await;
        self.record_result(c, call_id, &out).await?;
        Ok(out)
    }

    /// Runs a call outside any transcript in desktop session `driver`; `owner` keys its shell session.
    pub async fn run(
        &self,
        owner: &str,
        driver: Option<&str>,
        name: &str,
        mut args: Value,
    ) -> ToolOutput {
        if name == "computer_act" {
            self.announce(owner, &args);
        }
        if let Some(label) = driver
            && uses_driver(name)
            && let Some(obj) = args.as_object_mut()
        {
            obj.insert("session".to_owned(), label.into());
        }
        self.tools.run(owner, name, args).await
    }

    /// Records the result of a call.
    pub async fn record_result(
        &self,
        c: &Caller<'_>,
        call_id: &str,
        out: &ToolOutput,
    ) -> Result<(), String> {
        let body = Body::ToolResult(pb::ToolResult {
            call_id: call_id.to_owned(),
            is_error: out.is_error,
            text: out.text.clone(),
            image: out.image.clone().unwrap_or_default(),
        });
        self.journal
            .record(
                c.agent_id,
                c.turn_id,
                Role::Tool,
                body,
                !c.subagent.is_empty(),
            )
            .await
    }

    /// Screenshot in desktop session `driver`, outside any transcript.
    pub async fn capture(&self, owner: &str, driver: &str) -> ToolOutput {
        self.tools
            .run(owner, "screenshot", json!({"session": driver}))
            .await
    }

    /// Ends desktop session `driver`, releasing its input devices.
    pub async fn end_driver(&self, owner: &str, driver: &str) -> ToolOutput {
        self.tools
            .run(owner, "computer_end", json!({"session": driver}))
            .await
    }

    /// Ends the shell session of `owner` and every process it started in its group.
    pub async fn close_shell(&self, owner: &str) -> ToolOutput {
        self.tools.run(owner, "shell_close", json!({})).await
    }

    /// Closes the browser window of `owner`.
    pub async fn close_window(&self, owner: &str) -> ToolOutput {
        self.tools.run(owner, "browser_end", json!({})).await
    }

    /// Writes `data` to `path` in the workspace and returns the absolute path.
    pub async fn put_file(&self, owner: &str, path: &str, data: &[u8]) -> ToolOutput {
        self.tools
            .run(
                owner,
                "upload",
                json!({"path": path, "data_b64": B64.encode(data)}),
            )
            .await
    }

    fn announce(&self, agent_id: &str, args: &Value) {
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
        self.journal
            .bus
            .publish(event::Body::ComputerAction(pb::ComputerAction {
                agent_id: agent_id.to_owned(),
                action: args["action"].as_str().unwrap_or_default().to_owned(),
                x: coord("x"),
                y: coord("y"),
                text,
            }));
    }
}

/// Whether tool `name` runs in a desktop driver session.
pub fn uses_driver(name: &str) -> bool {
    DRIVER_TOOLS.contains(&name)
}

#[cfg(test)]
mod tests {
    use std::sync::Mutex;

    use super::*;
    use crate::store::Store;

    #[derive(Default)]
    struct MockTools {
        calls: Mutex<Vec<(String, String, Value)>>,
    }

    #[tonic::async_trait]
    impl Tools for MockTools {
        async fn run(&self, owner: &str, name: &str, args: Value) -> ToolOutput {
            self.calls
                .lock()
                .unwrap()
                .push((owner.to_owned(), name.to_owned(), args));
            ToolOutput {
                text: format!("ran {name}"),
                image: None,
                is_error: false,
            }
        }
    }

    fn setup() -> (tempfile::TempDir, Arc<MockTools>, Arc<Journal>, Dispatcher) {
        let dir = tempfile::tempdir().unwrap();
        let journal = Arc::new(Journal::new(Store::open(&dir.path().join("a.db")).unwrap()));
        let tools = Arc::new(MockTools::default());
        let dispatcher = Dispatcher::new(tools.clone(), Arc::clone(&journal));
        (dir, tools, journal, dispatcher)
    }

    #[tokio::test]
    async fn call_records_both_entries_and_injects_the_driver_only_for_desktop_tools() {
        let (_dir, tools, journal, dispatcher) = setup();
        let turn = journal.store.open_turn("ag".into()).await.unwrap();
        let mut live = journal.bus.subscribe(0).unwrap().live;
        let caller = Caller {
            agent_id: "ag",
            turn_id: &turn,
            subagent: "computer",
            driver: Some("cu-1"),
        };
        dispatcher
            .call(&caller, "c1", "shell", json!({"command": "ls"}))
            .await
            .unwrap();
        dispatcher
            .call(
                &caller,
                "c2",
                "computer_act",
                json!({"action": "click", "x": 3, "y": 4}),
            )
            .await
            .unwrap();
        let calls = tools.calls.lock().unwrap().clone();
        assert_eq!(
            calls[0],
            ("ag".into(), "shell".into(), json!({"command": "ls"}))
        );
        assert_eq!(calls[1].2["session"], "cu-1");

        let page = journal.store.transcript("ag".into(), 0, 10).await.unwrap();
        let kinds: Vec<&str> = page
            .entries
            .iter()
            .map(|e| match &e.body {
                Some(Body::ToolCall(c)) if c.subagent == "computer" => "call",
                Some(Body::ToolResult(_)) => "result",
                _ => "other",
            })
            .collect();
        assert_eq!(kinds, ["call", "result", "call", "result"]);
        let history = journal.store.history("ag".into(), turn).await.unwrap();
        assert!(history.is_empty(), "subagent entries stay out of history");

        let mut actions = Vec::new();
        while let Ok(ev) = live.try_recv() {
            if let Some(event::Body::ComputerAction(a)) = &ev.body {
                actions.push((a.action.clone(), a.x, a.y));
            }
        }
        assert_eq!(actions, [("click".to_owned(), 3, 4)]);
    }
}
