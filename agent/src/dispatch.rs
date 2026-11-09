//! The tool dispatcher: the one path from every transport and the loop down to the tools.

use std::sync::Arc;

use base64::Engine;
use base64::engine::general_purpose::STANDARD as B64;
use bytes::Bytes;
use serde_json::{Value, json};

use crate::flows::{self, Syncer};
use crate::journal::Journal;
use crate::pb::{self, Role, event, transcript_entry::Body};
use crate::secrets::SECRETS;
use crate::toolhost::rpc::{self, Call, Output};

const DRIVER_TOOLS: [&str; 3] = ["computer_state", "computer_act", "screenshot"];

/// Executes tools on the box; production results never hold a card value a tool typed from a file.
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
        tool_output(self.call(&call).await)
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
    flows: Option<Arc<Syncer>>,
    /// Plugin script tools, which read and write flows as `script_run` does.
    plugins: Vec<String>,
    scaled: bool,
}

impl Dispatcher {
    /// Creates a dispatcher over `tools` that writes history to `journal` and syncs `flows` around the calls that read or write them.
    pub fn new(
        tools: Arc<dyn Tools>,
        journal: Arc<Journal>,
        flows: Option<Arc<Syncer>>,
        plugins: Vec<String>,
        scaled: bool,
    ) -> Self {
        Self {
            tools,
            journal,
            flows,
            plugins,
            scaled,
        }
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
        if name == "computer_act" && !self.scaled {
            match args["actions"].as_array() {
                Some(actions) => actions.iter().for_each(|a| self.announce(owner, a)),
                None => self.announce(owner, &args),
            }
        }
        if let Some(label) = driver
            && uses_driver(name)
            && let Some(obj) = args.as_object_mut()
        {
            obj.insert("session".to_owned(), label.into());
        }
        let Some(sync) = &self.flows else {
            return self.tools.run(owner, name, args).await;
        };
        let plugin = self.plugins.iter().any(|p| p == name);
        let pulled = if flows::reads(name, &args) || (plugin && flows::runs_flow(&args)) {
            sync.pull().await
        } else {
            Ok(())
        };
        let mut out = self.tools.run(owner, name, args).await;
        if out.is_error
            && let Err(e) = pulled
        {
            out.text = format!(
                "{}\n(syncing the flow library failed, so this box's flows may be out of date: {e})",
                out.text
            );
        }
        if flows::writes(name) || plugin {
            sync.kick();
        }
        out
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
            .publish_live(event::Body::ComputerAction(pb::ComputerAction {
                agent_id: agent_id.to_owned(),
                action: args["action"].as_str().unwrap_or_default().to_owned(),
                x: coord("x"),
                y: coord("y"),
                text,
            }));
    }
}

/// The transcript form of a tool host's answer: card fields it typed are registered, and scrubbed from its text.
pub fn tool_output(answer: Result<Output, String>) -> ToolOutput {
    match answer {
        Ok(mut out) => {
            for (field, value) in &out.secrets {
                SECRETS.register(field, value);
            }
            SECRETS.scrub(&mut out.text);
            ToolOutput {
                image: out
                    .image_b64
                    .and_then(|b| B64.decode(b).ok())
                    .map(Bytes::from),
                text: out.text,
                is_error: out.is_error,
            }
        }
        Err(e) => ToolOutput::error(e),
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
        failing: Mutex<Option<String>>,
    }

    #[tonic::async_trait]
    impl Tools for MockTools {
        async fn run(&self, owner: &str, name: &str, args: Value) -> ToolOutput {
            self.calls
                .lock()
                .unwrap()
                .push((owner.to_owned(), name.to_owned(), args));
            if self.failing.lock().unwrap().as_deref() == Some(name) {
                return ToolOutput::error(format!("{name} failed"));
            }
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
        let dispatcher = Dispatcher::new(
            Arc::clone(&tools) as Arc<dyn Tools>,
            Arc::clone(&journal),
            None,
            Vec::new(),
            false,
        );
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
        let mut first = u64::MAX;
        while let Ok(ev) = live.try_recv() {
            first = first.min(ev.offset);
            if let Some(event::Body::ComputerAction(a)) = &ev.body {
                actions.push((a.action.clone(), a.x, a.y));
            }
        }
        assert_eq!(actions, [("click".to_owned(), 3, 4)]);
        let replay = journal.bus.subscribe(first - 1).unwrap().replay;
        assert!(
            !replay
                .iter()
                .any(|e| matches!(e.body, Some(event::Body::ComputerAction(_)))),
            "a live highlight entered the replay ring"
        );
    }

    #[tokio::test]
    async fn a_flow_call_that_fails_after_a_failed_pull_says_the_library_sync_failed() {
        let (dir, tools, journal, _) = setup();
        let store = Store::open(&dir.path().join("flows.db")).unwrap();
        let sync = Syncer::new("http://127.0.0.1:9/mcp", store, tools.clone()).unwrap();
        let dispatcher = Dispatcher::new(
            tools.clone(),
            journal,
            Some(Arc::new(sync)),
            Vec::new(),
            false,
        );
        let flow = json!({"script": "flows/a.mts"});

        let out = dispatcher.run("ag", None, "script_run", flow.clone()).await;
        assert!(!out.is_error);
        assert_eq!(out.text, "ran script_run");

        *tools.failing.lock().unwrap() = Some("script_run".into());
        let out = dispatcher.run("ag", None, "script_run", flow).await;
        assert!(out.is_error);
        assert!(out.text.starts_with("script_run failed\n"), "{}", out.text);
        assert!(
            out.text.contains("syncing the flow library failed"),
            "{}",
            out.text
        );
    }

    #[tokio::test]
    async fn registered_card_values_stay_out_of_the_transcript() {
        let (_dir, _tools, journal, dispatcher) = setup();
        let turn = journal.store.open_turn("ag".into()).await.unwrap();
        SECRETS.register("number", "6011000990139424");
        let caller = Caller {
            agent_id: "ag",
            turn_id: &turn,
            subagent: "computer",
            driver: None,
        };
        dispatcher
            .call(
                &caller,
                "c1",
                "computer_act",
                json!({"action": "type", "text": "6011 0009 9013 9424"}),
            )
            .await
            .unwrap();
        let text = Body::Text(pb::TextBlock {
            text: "The Card number field is now filled with 6011000990139424.".into(),
            ..Default::default()
        });
        journal
            .record("ag", &turn, Role::Assistant, text, true)
            .await
            .unwrap();
        let page = journal.store.transcript("ag".into(), 0, 10).await.unwrap();
        let all = format!("{:?}", page.entries);
        assert!(
            !all.contains("6011000990139424") && !all.contains("6011 0009"),
            "{all}"
        );
        assert_eq!(all.matches("[card number]").count(), 2, "{all}");
    }
}
