//! Per-call phase timing: each toolhost call logs one line with its phases when it ends.

use std::cell::RefCell;
use std::fmt::{Display, Write as _};
use std::time::Instant;

tokio::task_local! {
    static PHASES: RefCell<Phases>;
}

struct Phases {
    tool: String,
    started: Instant,
    last: Instant,
    fields: String,
}

impl Drop for Phases {
    fn drop(&mut self) {
        eprintln!(
            "toolhost: timing tool={}{} total_ms={}",
            self.tool,
            self.fields,
            self.started.elapsed().as_millis()
        );
    }
}

/// Runs `call` as one call of `tool`; its line is logged when it ends or is cancelled.
pub async fn scope<F: Future>(tool: &str, call: F) -> F::Output {
    let now = Instant::now();
    let phases = Phases {
        tool: tool.to_owned(),
        started: now,
        last: now,
        fields: String::new(),
    };
    PHASES.scope(RefCell::new(phases), call).await
}

/// Records the time since the previous phase, or since the call started, as `<phase>_ms`.
pub fn mark(phase: &str) {
    let _ = PHASES.try_with(|p| {
        let mut p = p.borrow_mut();
        let now = Instant::now();
        let ms = now.duration_since(p.last).as_millis();
        p.last = now;
        let _ = write!(p.fields, " {phase}_ms={ms}");
    });
}

/// Records `key=value` in the current call's line.
pub fn note(key: &str, value: impl Display) {
    let _ = PHASES.try_with(|p| {
        let _ = write!(p.borrow_mut().fields, " {key}={value}");
    });
}
