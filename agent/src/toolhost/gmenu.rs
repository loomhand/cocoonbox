//! Menu toggle states of a GtkApplication over D-Bus: AT-SPI gives a GMenuModel item no checked state,
//! but org.gtk.Menus names its action and org.gtk.Actions holds the action's state.

use std::collections::HashMap;
use std::process::Stdio;
use std::time::Duration;

use serde_json::Value;
use tokio::process::Command;

const BUSCTL: &str = "busctl";
const BUSCTL_WAIT: Duration = Duration::from_secs(2);
const MENU_GROUPS: u32 = 64;

/// The checked state of every stateful menu bar item of the app with process `pid`, by [`label_key`];
/// empty when the app exports no menus or a call fails.
pub async fn toggles(pid: i64) -> HashMap<String, bool> {
    let Ok(comm) = tokio::fs::read_to_string(format!("/proc/{pid}/comm")).await else {
        return HashMap::new();
    };
    let comm = comm.trim().to_lowercase();
    let names = call(&[
        "org.freedesktop.DBus",
        "/org/freedesktop/DBus",
        "org.freedesktop.DBus",
        "ListNames",
    ])
    .await;
    let buses: Vec<String> = names
        .as_ref()
        .and_then(|v| v["data"][0].as_array())
        .into_iter()
        .flatten()
        .filter_map(Value::as_str)
        .filter(|n| {
            n.rsplit('.')
                .next()
                .is_some_and(|last| last.eq_ignore_ascii_case(&comm))
        })
        .map(str::to_owned)
        .collect();
    for bus in buses {
        let found = app_toggles(&bus).await;
        if !found.is_empty() {
            return found;
        }
    }
    HashMap::new()
}

async fn app_toggles(bus: &str) -> HashMap<String, bool> {
    let root = format!("/{}", bus.replace('.', "/"));
    let menubar = format!("{root}/menus/menubar");
    let count = MENU_GROUPS.to_string();
    let groups: Vec<String> = (0..MENU_GROUPS).map(|g| g.to_string()).collect();
    let mut start = vec![bus, &menubar, "org.gtk.Menus", "Start", "au", &count];
    start.extend(groups.iter().map(String::as_str));
    let Some(menus) = call(&start).await else {
        return HashMap::new();
    };
    let items = menu_items(&menus);
    if items.is_empty() {
        return HashMap::new();
    }
    let app = describe(bus, &root).await;
    let windows = format!("{root}/window");
    let listed = call(&[
        bus,
        &windows,
        "org.freedesktop.DBus.Introspectable",
        "Introspect",
    ])
    .await;
    let mut win: HashMap<String, Option<Value>> = HashMap::new();
    for node in listed.as_ref().map(child_nodes).unwrap_or_default() {
        for (name, state) in describe(bus, &format!("{windows}/{node}")).await {
            let agreed = win.entry(name).or_insert_with(|| Some(state.clone()));
            if agreed.as_ref() != Some(&state) {
                *agreed = None;
            }
        }
    }
    items
        .into_iter()
        .filter_map(|(label, action, target)| {
            let state = match action.split_once('.')? {
                ("app", name) => app.get(name)?.clone(),
                ("win", name) => win.get(name)?.clone()?,
                _ => return None,
            };
            let checked = match &target {
                Some(t) => state["data"] == t["data"],
                None if state["type"] == "b" => state["data"].as_bool()?,
                None => return None,
            };
            Some((label_key(&label), checked))
        })
        .collect()
}

/// `(label, action, target)` of every menu item with an action in an org.gtk.Menus Start reply.
fn menu_items(reply: &Value) -> Vec<(String, String, Option<Value>)> {
    let mut out = Vec::new();
    for group in reply["data"][0].as_array().into_iter().flatten() {
        for item in group[2].as_array().into_iter().flatten() {
            if let (Some(label), Some(action)) = (
                item["label"]["data"].as_str(),
                item["action"]["data"].as_str(),
            ) {
                let target = item.get("target").cloned();
                out.push((label.to_owned(), action.to_owned(), target));
            }
        }
    }
    out
}

/// Each action of the org.gtk.Actions object at `path` with its state, for stateful actions.
async fn describe(bus: &str, path: &str) -> HashMap<String, Value> {
    let reply = call(&[bus, path, "org.gtk.Actions", "DescribeAll"]).await;
    reply
        .as_ref()
        .and_then(|v| v["data"][0].as_object())
        .into_iter()
        .flatten()
        .filter_map(|(name, d)| Some((name.clone(), d[2].as_array()?.first()?.clone())))
        .collect()
}

/// The child node names in an Introspect reply.
fn child_nodes(reply: &Value) -> Vec<String> {
    let xml = reply["data"][0].as_str().unwrap_or_default();
    xml.split("<node name=\"")
        .skip(1)
        .filter_map(|s| s.split_once('"').map(|(n, _)| n.to_owned()))
        .collect()
}

/// A menu label without mnemonic underscores, accelerator padding or case, to match the accessible name.
pub fn label_key(label: &str) -> String {
    label
        .replace('_', "")
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ")
        .to_lowercase()
}

/// One session-bus method call through busctl, as its JSON reply.
async fn call(args: &[&str]) -> Option<Value> {
    let run = Command::new(BUSCTL)
        .args(["--user", "--json=short", "call"])
        .args(args)
        .stdin(Stdio::null())
        .kill_on_drop(true)
        .output();
    let out = tokio::time::timeout(BUSCTL_WAIT, run).await.ok()?.ok()?;
    if !out.status.success() {
        return None;
    }
    serde_json::from_slice(&out.stdout).ok()
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    #[test]
    fn menu_items_carry_their_action_and_target() {
        let reply = json!({"type": "a(uuaa{sv})", "data": [[
            [0, 0, [{"label": {"type": "s", "data": "_Document"}, ":submenu": {"type": "(uu)", "data": [5, 0]}}]],
            [5, 0, [
                {"action": {"type": "s", "data": "app.preferences.view.word-wrap"}, "label": {"type": "s", "data": "_Word Wrap"}},
                {"action": {"type": "s", "data": "win.document.filetype"}, "label": {"type": "s", "data": "Plain _Text"}, "target": {"type": "s", "data": "plain-text"}}
            ]]
        ]]});
        let items = menu_items(&reply);
        assert_eq!(items.len(), 2);
        assert_eq!(items[0].0, "_Word Wrap");
        assert_eq!(items[1].2.as_ref().unwrap()["data"], "plain-text");
        assert_eq!(label_key("_Word Wrap"), label_key("Word Wrap      "));
        let introspect = json!({"type": "s", "data": ["<node>\n  <node name=\"1\"/>\n  <node name=\"2\"/>\n</node>\n"]});
        assert_eq!(child_nodes(&introspect), ["1", "2"]);
    }
}
