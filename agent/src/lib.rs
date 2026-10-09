//! cocoonbox-agent: the in-box agent loop (`serve`) and the tool host (`toolhost`).

pub mod bus;
pub mod computer;
pub mod config;
pub mod connect;
pub mod dispatch;
pub mod exec;
pub mod flows;
pub mod identity;
pub mod journal;
pub mod llm;
pub mod mcp;
pub mod offload;
pub mod prompts;
pub mod route;
pub mod runner;
pub mod screen;
pub mod server;
pub mod state;
pub mod store;
pub mod sys;
pub mod toolhost;
pub mod tools;

/// Generated types of `cocoonbox.agent.v1`.
#[allow(clippy::all, clippy::pedantic)]
pub mod pb {
    tonic::include_proto!("cocoonbox.agent.v1");
}

/// Current wall-clock time in unix milliseconds.
pub fn now_ms() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| i64::try_from(d.as_millis()).unwrap_or(i64::MAX))
}

/// Random identifier with a readable prefix.
pub fn new_id(prefix: &str) -> String {
    format!("{prefix}-{}", uuid::Uuid::new_v4().simple())
}
