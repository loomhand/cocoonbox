//! The `desk` command: a desk verb of shuttle from a shell.

use serde_json::{Map, Value};

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

fn cli_args(rest: &[String]) -> Result<Value, String> {
    if let [one] = rest
        && one.trim_start().starts_with('{')
    {
        return serde_json::from_str(one).map_err(|e| format!("arguments: {e}"));
    }
    let mut args = Map::new();
    for pair in rest {
        let (k, v) = pair
            .split_once('=')
            .ok_or_else(|| format!("{pair:?} is not key=value"))?;
        let v = match k {
            "id" | "window" => Value::String(v.to_owned()),
            _ => serde_json::from_str(v).unwrap_or_else(|_| Value::String(v.to_owned())),
        };
        args.insert(k.to_owned(), v);
    }
    Ok(Value::Object(args))
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    #[test]
    fn cli_arguments_are_json_or_pairs_with_json_values_but_string_ids() {
        let pairs =
            cli_args(&["window=Save As".into(), "text=true".into(), "pid=7".into()]).unwrap();
        assert_eq!(pairs, json!({"window": "Save As", "text": true, "pid": 7}));
        assert_eq!(
            cli_args(&[r#"{"id": "1/2"}"#.into()]).unwrap(),
            json!({"id": "1/2"})
        );
        assert!(cli_args(&["nope".into()]).is_err());
        assert_eq!(
            cli_args(&["id=21".into(), "window=2048".into()]).unwrap(),
            json!({"id": "21", "window": "2048"})
        );
    }
}
