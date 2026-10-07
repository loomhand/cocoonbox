//! Tier router of a Script box: asks gw's decisions model which tool tier fits a task.

use std::time::Duration;

use serde_json::Value;

const ROUTE: &str = include_str!("../../image/script/route.json");
const TASK: &str = "\"__TASK__\"";
const TIMEOUT: Duration = Duration::from_secs(2);
const ERROR_BODY_CHARS: usize = 300;

/// Lowest confidence at which a route is followed rather than left to the caller.
pub const CONFIDENT: f64 = 0.8;

/// The tiers with what each one means for the tools to use.
pub const TIERS: [(&str, &str); 5] = [
    ("command", "use shell and the file tools"),
    (
        "cua",
        "a single look or click: screenshot, computer_state, computer_act (in the box's own agent: delegate_computer)",
    ),
    (
        "browser",
        "a web page: the browser_* tools, also for a form filled several times",
    ),
    (
        "script",
        "write one script and run it with script_run",
    ),
    (
        "jev",
        "a pure judgment from the given text: no action on the computer is needed",
    ),
];

/// Client of gw's `/v1/decisions` with the V4 tier prompt.
pub struct Router {
    http: reqwest::Client,
    url: String,
    api_key: String,
}

impl Router {
    /// Creates the router for gw at `base_url` (ending in `/v1`).
    pub fn new(base_url: &str, api_key: &str) -> Result<Self, String> {
        let http = reqwest::Client::builder()
            .timeout(TIMEOUT)
            .build()
            .map_err(|e| format!("build router client: {e}"))?;
        Ok(Self {
            http,
            url: format!("{}/decisions", base_url.trim_end_matches('/')),
            api_key: api_key.to_owned(),
        })
    }

    /// Routes `task`; a failure comes back as an uncertain route that carries the error.
    pub async fn route(&self, task: &str) -> Route {
        match self.decide(task).await {
            Ok(route) => route,
            Err(e) => Route {
                tier: String::new(),
                confidence: 0.0,
                error: Some(e),
            },
        }
    }

    async fn decide(&self, task: &str) -> Result<Route, String> {
        let task = serde_json::to_string(task).map_err(|e| format!("encode task: {e}"))?;
        let resp = self
            .http
            .post(&self.url)
            .bearer_auth(&self.api_key)
            .header("content-type", "application/json")
            .body(ROUTE.replacen(TASK, &task, 1))
            .send()
            .await
            .map_err(|e| format!("post {}: {e}", self.url))?;
        let status = resp.status();
        let text = resp
            .text()
            .await
            .map_err(|e| format!("read decisions reply: {e}"))?;
        if !status.is_success() {
            let text: String = text.chars().take(ERROR_BODY_CHARS).collect();
            return Err(format!("gw decisions returned {status}: {text}"));
        }
        let reply: Value =
            serde_json::from_str(&text).map_err(|e| format!("decode decisions reply: {e}"))?;
        let answer = &reply["answers"]["tier"];
        let tier = answer["choice"]
            .as_str()
            .filter(|t| meaning(t).is_some())
            .ok_or_else(|| format!("decisions reply has no known tier: {answer}"))?
            .to_owned();
        Ok(Route {
            tier,
            confidence: answer["confidence"].as_f64().unwrap_or(0.0),
            error: None,
        })
    }
}

/// One routing decision.
#[derive(Debug)]
pub struct Route {
    pub tier: String,
    pub confidence: f64,
    pub error: Option<String>,
}

impl Route {
    /// Whether the route is confident enough to follow.
    pub fn confident(&self) -> bool {
        self.error.is_none() && self.confidence >= CONFIDENT
    }
}

/// Whether tool `tool` leads the offered tools when a turn is routed to `tier`.
pub fn leads(tier: &str, tool: &str) -> bool {
    match tier {
        "command" => matches!(
            tool,
            "shell" | "read_file" | "write_file" | "edit_file" | "list_dir"
        ),
        "cua" => matches!(tool, "delegate_computer" | "screenshot"),
        "browser" => tool.starts_with("browser_"),
        "script" => tool == "script_run",
        _ => false,
    }
}

/// What tier `tier` means for the tools to use.
pub fn meaning(tier: &str) -> Option<&'static str> {
    TIERS.iter().find(|(t, _)| *t == tier).map(|(_, m)| *m)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_v4_prompt_offers_exactly_the_tiers_and_takes_one_task() {
        assert_eq!(ROUTE.matches(TASK).count(), 1);
        let body: Value = serde_json::from_str(&ROUTE.replacen(TASK, "\"x\"", 1)).unwrap();
        assert_eq!(body["model"], "typesafe/jev-1.13");
        assert_eq!(body["state"]["task"], "x");
        let criteria = body["questions"]["tier"]["criteria"].as_object().unwrap();
        let mut offered: Vec<&str> = criteria.keys().map(String::as_str).collect();
        let mut known: Vec<&str> = TIERS.iter().map(|(t, _)| *t).collect();
        offered.sort_unstable();
        known.sort_unstable();
        assert_eq!(offered, known);
    }

    #[test]
    fn a_failed_or_weak_route_is_not_confident() {
        let failed = Route {
            tier: "script".into(),
            confidence: 0.99,
            error: Some("gw decisions returned 429".into()),
        };
        let weak = Route {
            tier: "cua".into(),
            confidence: 0.55,
            error: None,
        };
        let strong = Route {
            tier: "browser".into(),
            confidence: CONFIDENT,
            error: None,
        };
        assert!(!failed.confident() && !weak.confident() && strong.confident());
    }
}
