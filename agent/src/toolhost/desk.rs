//! The `desk` command: a desk verb of shuttle from a shell.

use desk_core::text::cli_args;

use super::rpc::{Call, Client};

const DEFAULT_SOCKET: &str = "/run/cocoonbox/desk.sock";
const CLI_USAGE: &str = "usage: desk VERB [key=value ...|JSON]; verbs: windows, unanswered, wait, snapshot, find, read, press, act, at, key, type, click, scroll, drag, still, focus, close, launch, shot, screen, web";

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
