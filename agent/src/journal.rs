//! Transcript and status persistence with event fan-out, shared by every layer that writes history.

use crate::bus::Bus;
use crate::pb::{AgentStatus, Role, TranscriptEntry, event, transcript_entry::Body};
use crate::store::Store;

/// Store plus event bus: every write is persisted, then published.
pub struct Journal {
    pub store: Store,
    pub bus: Bus,
}

impl Journal {
    /// Creates a journal over `store` with a fresh bus.
    pub fn new(store: Store) -> Self {
        Self {
            store,
            bus: Bus::new(),
        }
    }

    /// Persists a transcript entry and publishes it.
    pub async fn record(
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

    /// Updates an agent's status and publishes the new row.
    pub async fn set_status(&self, agent_id: &str, status: AgentStatus) {
        match self.store.set_status(agent_id.to_owned(), status).await {
            Ok(Some(agent)) => {
                self.bus.publish(event::Body::Agent(agent));
            }
            Ok(None) => {}
            Err(e) => eprintln!("set status of {agent_id}: {e}"),
        }
    }

    /// Names an agent created without a name and publishes the new row.
    pub async fn title(&self, agent_id: &str, title: String) {
        match self.store.title_agent(agent_id.to_owned(), title).await {
            Ok(Some(agent)) => {
                self.bus.publish(event::Body::Agent(agent));
            }
            Ok(None) => {}
            Err(e) => eprintln!("title {agent_id}: {e}"),
        }
    }
}
