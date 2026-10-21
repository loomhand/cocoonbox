//! The desk on shuttle: its point lookups for grounding, and the `desk` command.

use serde_json::{Value, json};

use desk_core::text::cli_args;

use super::rpc::{Call, Client};

const DEFAULT_SOCKET: &str = "/run/cocoonbox/desk.sock";
const CLI_USAGE: &str = "usage: desk VERB [key=value ...|JSON]; verbs: windows, snapshot, find, read, press, act, at, key, type, click, focus, close, launch, shot, screen";

/// The desk shuttle serves.
pub struct Desk {
    shuttle: Client,
}

impl Desk {
    /// A desk whose point lookups go to `shuttle`.
    pub fn new(shuttle: Client) -> Self {
        Self { shuttle }
    }

    /// The element of the focused window at the screen point `(x, y)`, as the `at` op answers.
    pub async fn at_point(&self, x: i32, y: i32) -> Result<Value, String> {
        let call = Call {
            agent_id: String::new(),
            tool: "desk_at".to_owned(),
            args: json!({"x": x, "y": y}),
        };
        let out = self.shuttle.desk(&call, super::rpc::DESK_DEADLINE).await?;
        if out.is_error {
            return Err(out.text);
        }
        serde_json::from_str(&out.text).map_err(|e| format!("at: {e}"))
    }
}

/// `cocoonbox-agent desk VERB [key=value ...|JSON]`: calls shuttle's `desk_VERB` at `SHUTTLE_SOCKET` and prints its result.
pub async fn cli(args: Vec<String>) -> u8 {
    let Some((verb, rest)) = args.split_first() else {
        eprintln!("{CLI_USAGE}");
        return 2;
    };
    let args = match cli_args(rest) {
        Ok(a) => a,
        Err(e) => {
            eprintln!("desk: {e}\n{CLI_USAGE}");
            return 2;
        }
    };
    let socket = std::env::var("SHUTTLE_SOCKET")
        .ok()
        .filter(|s| !s.is_empty())
        .unwrap_or_else(|| DEFAULT_SOCKET.to_owned());
    let call = Call {
        agent_id: "desk".to_owned(),
        tool: format!("desk_{verb}"),
        args,
    };
    match Client::new(socket.into()).call(&call).await {
        Ok(out) if !out.is_error => {
            println!("{}", out.text);
            0
        }
        Ok(out) => {
            eprintln!("desk {verb}: {}", out.text);
            1
        }
        Err(e) => {
            eprintln!("desk {verb}: {e}");
            1
        }
    }
}
