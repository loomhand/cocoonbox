//! SQLite state: agents, turns, transcript entries and the flow library sync.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use prost::Message;
use rusqlite::types::Type;
use rusqlite::{Connection, OptionalExtension, params};

use crate::pb::{self, AgentStatus, TranscriptEntry, transcript_entry};
use crate::{new_id, now_ms};

const SCHEMA: &str = "
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
CREATE TABLE IF NOT EXISTS agents (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    main_model TEXT NOT NULL,
    status INTEGER NOT NULL,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS turns (
    id TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL,
    done INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS entries (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    agent_id TEXT NOT NULL,
    turn_id TEXT NOT NULL,
    subagent INTEGER NOT NULL,
    body BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS entries_by_agent ON entries (agent_id, seq);
CREATE TABLE IF NOT EXISTS flow_sync (
    name TEXT PRIMARY KEY,
    rev INTEGER NOT NULL,
    sha256 TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS flow_folder (
    id TEXT NOT NULL
);
";

const PAGE_BYTES: usize = 3 << 20;
const UNTITLED: &str = "agent";

/// Handle to the agent database; every call runs on the blocking pool.
#[derive(Clone)]
pub struct Store {
    conn: Arc<Mutex<Connection>>,
}

impl Store {
    /// Opens or creates the database at `path`.
    pub fn open(path: &Path) -> Result<Self, String> {
        let conn = Connection::open(path).map_err(|e| format!("open {}: {e}", path.display()))?;
        conn.execute_batch(SCHEMA)
            .map_err(|e| format!("apply schema: {e}"))?;
        Ok(Self {
            conn: Arc::new(Mutex::new(conn)),
        })
    }

    /// Creates an agent.
    pub async fn create_agent(
        &self,
        name: String,
        main_model: String,
    ) -> Result<pb::Agent, String> {
        self.call(move |c| {
            let now = now_ms();
            let mut agent = pb::Agent {
                id: new_id("ag"),
                name,
                status: AgentStatus::Idle.into(),
                main_model,
                created_at_ms: now,
                updated_at_ms: now,
            };
            c.execute(
                "INSERT INTO agents (id, name, main_model, status, created_at_ms, updated_at_ms)
                 VALUES (?1, ?2, ?3, ?4, ?5, ?5)",
                params![agent.id, agent.name, agent.main_model, agent.status, now],
            )?;
            agent.name = display_name(agent.name);
            Ok(agent)
        })
        .await
    }

    /// Returns every agent, oldest first.
    pub async fn list_agents(&self) -> Result<Vec<pb::Agent>, String> {
        self.call(|c| {
            let mut stmt = c.prepare(
                "SELECT id, name, main_model, status, created_at_ms, updated_at_ms
                 FROM agents ORDER BY created_at_ms, id",
            )?;
            let rows = stmt.query_map([], read_agent)?;
            rows.collect()
        })
        .await
    }

    /// Returns one agent.
    pub async fn agent(&self, id: String) -> Result<Option<pb::Agent>, String> {
        self.call(move |c| agent_row(c, &id)).await
    }

    /// Updates an agent's status and returns the new row.
    pub async fn set_status(
        &self,
        id: String,
        status: AgentStatus,
    ) -> Result<Option<pb::Agent>, String> {
        self.call(move |c| {
            c.execute(
                "UPDATE agents SET status = ?2, updated_at_ms = ?3 WHERE id = ?1",
                params![id, i32::from(status), now_ms()],
            )?;
            agent_row(c, &id)
        })
        .await
    }

    /// Sets an agent's name and returns the new row.
    pub async fn rename_agent(
        &self,
        id: String,
        name: String,
    ) -> Result<Option<pb::Agent>, String> {
        self.call(move |c| {
            c.execute(
                "UPDATE agents SET name = ?2, updated_at_ms = ?3 WHERE id = ?1",
                params![id, name, now_ms()],
            )?;
            agent_row(c, &id)
        })
        .await
    }

    /// Names an agent created without a name; returns the new row, or `None` when it already has one.
    pub async fn title_agent(
        &self,
        id: String,
        title: String,
    ) -> Result<Option<pb::Agent>, String> {
        self.call(move |c| {
            let changed = c.execute(
                "UPDATE agents SET name = ?2, updated_at_ms = ?3 WHERE id = ?1 AND name = ''",
                params![id, title, now_ms()],
            )?;
            if changed == 0 {
                return Ok(None);
            }
            agent_row(c, &id)
        })
        .await
    }

    /// Deletes an agent with its turns and entries; returns whether it existed.
    pub async fn delete_agent(&self, id: String) -> Result<bool, String> {
        self.call(move |c| {
            let tx = c.transaction()?;
            let existed = delete_rows(&tx, &id)?;
            tx.commit()?;
            Ok(existed)
        })
        .await
    }

    /// Records a user turn; `None` when the agent does not exist.
    pub async fn queue_turn(
        &self,
        agent_id: String,
        text: String,
    ) -> Result<Option<Queued>, String> {
        self.call(move |c| {
            let tx = c.transaction()?;
            let turn_id = new_id("turn");
            let queued = tx.execute(
                "INSERT INTO turns (id, agent_id)
                 SELECT ?1, ?2 WHERE EXISTS (SELECT 1 FROM agents WHERE id = ?2)",
                params![turn_id, agent_id],
            )?;
            if queued == 0 {
                return Ok(None);
            }
            let entry = TranscriptEntry {
                agent_id,
                turn_id: turn_id.clone(),
                role: pb::Role::User.into(),
                body: Some(transcript_entry::Body::Text(pb::TextBlock {
                    text,
                    ..Default::default()
                })),
                ..Default::default()
            };
            let entry = insert_entry(&tx, entry, false)?;
            tx.commit()?;
            Ok(Some(Queued { turn_id, entry }))
        })
        .await
    }

    /// Opens a turn with no user entry, for work that is not a user message.
    pub async fn open_turn(&self, agent_id: String) -> Result<String, String> {
        self.call(move |c| {
            let turn_id = new_id("turn");
            c.execute(
                "INSERT INTO turns (id, agent_id) VALUES (?1, ?2)",
                params![turn_id, agent_id],
            )?;
            Ok(turn_id)
        })
        .await
    }

    /// Deletes agents of main model `model` that are neither running nor waiting and were last
    /// updated before `before_ms`, with their turns and entries; returns their ids.
    pub async fn prune_agents(
        &self,
        model: &'static str,
        before_ms: i64,
    ) -> Result<Vec<String>, String> {
        self.call(move |c| {
            let tx = c.transaction()?;
            let ids: Vec<String> = {
                let mut stmt = tx.prepare(
                    "SELECT id FROM agents WHERE main_model = ?1
                     AND status NOT IN (?2, ?3) AND updated_at_ms < ?4",
                )?;
                let rows = stmt.query_map(
                    params![
                        model,
                        i32::from(AgentStatus::Running),
                        i32::from(AgentStatus::WaitingForUser),
                        before_ms
                    ],
                    |r| r.get(0),
                )?;
                rows.collect::<rusqlite::Result<_>>()?
            };
            for id in &ids {
                delete_rows(&tx, id)?;
            }
            tx.commit()?;
            Ok(ids)
        })
        .await
    }

    /// Writes a consistent copy of the database to `path` through the online backup API.
    pub async fn backup_to(&self, path: PathBuf) -> Result<(), String> {
        self.call(move |c| {
            c.backup(
                rusqlite::MAIN_DB,
                &path,
                None::<fn(rusqlite::backup::Progress)>,
            )
        })
        .await
    }

    /// Replaces the database contents with the database at `path`, adding the tables an older copy lacks.
    pub async fn restore_from(&self, path: PathBuf) -> Result<(), String> {
        self.call(move |c| {
            c.restore(
                rusqlite::MAIN_DB,
                &path,
                None::<fn(rusqlite::backup::Progress)>,
            )?;
            c.execute_batch(SCHEMA)
        })
        .await
    }

    /// The rev and SHA-256 each flow had when this box and the library last agreed on it.
    pub async fn flow_rows(&self) -> Result<HashMap<String, (u64, String)>, String> {
        self.call(|c| {
            let mut stmt = c.prepare("SELECT name, rev, sha256 FROM flow_sync")?;
            let rows = stmt.query_map([], |r| {
                Ok((r.get(0)?, (r.get::<_, i64>(1)?.cast_unsigned(), r.get(2)?)))
            })?;
            rows.collect()
        })
        .await
    }

    /// Records that this box and the library agree on flow `name` at `rev`.
    pub async fn set_flow_row(&self, name: String, rev: u64, sha256: String) -> Result<(), String> {
        self.call(move |c| {
            c.execute(
                "INSERT INTO flow_sync (name, rev, sha256) VALUES (?1, ?2, ?3)
                 ON CONFLICT (name) DO UPDATE SET rev = excluded.rev, sha256 = excluded.sha256",
                params![name, rev.cast_signed(), sha256],
            )
            .map(drop)
        })
        .await
    }

    /// Forgets flow `name`, which the library no longer holds.
    pub async fn drop_flow_row(&self, name: String) -> Result<(), String> {
        self.call(move |c| {
            c.execute("DELETE FROM flow_sync WHERE name = ?1", params![name])
                .map(drop)
        })
        .await
    }

    /// The id of the flows folder that the rows were agreed in.
    pub async fn flow_folder(&self) -> Result<Option<String>, String> {
        self.call(|c| {
            c.query_row("SELECT id FROM flow_folder", [], |r| r.get(0))
                .optional()
        })
        .await
    }

    /// Records `id` as the flows folder that the rows are agreed in.
    pub async fn set_flow_folder(&self, id: String) -> Result<(), String> {
        self.call(move |c| {
            let tx = c.transaction()?;
            tx.execute("DELETE FROM flow_folder", [])?;
            tx.execute("INSERT INTO flow_folder (id) VALUES (?1)", params![id])?;
            tx.commit()
        })
        .await
    }

    /// Folds the WAL back into the database file.
    pub async fn checkpoint(&self) -> Result<(), String> {
        self.call(|c| c.execute_batch("PRAGMA wal_checkpoint(TRUNCATE);"))
            .await
    }

    /// Persists an entry and returns it with its seq and timestamp; a `TurnEnd` finishes its turn in the same transaction.
    pub async fn append(
        &self,
        entry: TranscriptEntry,
        subagent: bool,
    ) -> Result<TranscriptEntry, String> {
        self.call(move |c| {
            let tx = c.transaction()?;
            if matches!(entry.body, Some(transcript_entry::Body::TurnEnd(_))) {
                tx.execute("UPDATE turns SET done = 1 WHERE id = ?1", [&entry.turn_id])?;
            }
            let entry = insert_entry(&tx, entry, subagent)?;
            tx.commit()?;
            Ok(entry)
        })
        .await
    }

    /// Returns unfinished turns as `(agent_id, turn_id)`, oldest first.
    pub async fn unfinished_turns(&self) -> Result<Vec<(String, String)>, String> {
        self.call(|c| {
            let mut stmt =
                c.prepare("SELECT agent_id, id FROM turns WHERE done = 0 ORDER BY rowid")?;
            let rows = stmt.query_map([], |r| Ok((r.get(0)?, r.get(1)?)))?;
            rows.collect()
        })
        .await
    }

    /// Returns a page of entries with seq > `after_seq`.
    pub async fn transcript(
        &self,
        agent_id: String,
        after_seq: u64,
        limit: u32,
    ) -> Result<pb::TranscriptPage, String> {
        self.call(move |c| {
            let mut stmt = c.prepare(
                "SELECT seq, body FROM entries WHERE agent_id = ?1 AND seq > ?2
                 ORDER BY seq LIMIT ?3",
            )?;
            let after = i64::try_from(after_seq).unwrap_or(i64::MAX);
            let mut rows = stmt.query(params![agent_id, after, i64::from(limit) + 1])?;
            let mut page = pb::TranscriptPage::default();
            let mut bytes = 0;
            while let Some(row) = rows.next()? {
                if page.entries.len() == limit as usize
                    || (!page.entries.is_empty() && bytes >= PAGE_BYTES)
                {
                    page.has_more = true;
                    break;
                }
                let entry = decode_entry(row)?;
                bytes += entry.encoded_len();
                page.entries.push(entry);
            }
            Ok(page)
        })
        .await
    }

    /// Returns the main agent's entries of turns queued up to `turn_id`, grouped by turn.
    pub async fn history(
        &self,
        agent_id: String,
        turn_id: String,
    ) -> Result<Vec<TranscriptEntry>, String> {
        self.call(move |c| {
            let mut stmt = c.prepare(
                "SELECT e.seq, e.body FROM entries e JOIN turns t ON e.turn_id = t.id
                 WHERE e.agent_id = ?1 AND e.subagent = 0
                   AND t.rowid <= (SELECT rowid FROM turns WHERE id = ?2)
                 ORDER BY t.rowid, e.seq",
            )?;
            let rows = stmt.query_map([agent_id, turn_id], decode_entry)?;
            rows.collect()
        })
        .await
    }

    async fn call<T, F>(&self, f: F) -> Result<T, String>
    where
        T: Send + 'static,
        F: FnOnce(&mut Connection) -> rusqlite::Result<T> + Send + 'static,
    {
        let conn = Arc::clone(&self.conn);
        tokio::task::spawn_blocking(move || {
            let mut guard = conn.lock().map_err(|_| "store mutex poisoned".to_owned())?;
            f(&mut guard).map_err(|e| format!("sqlite: {e}"))
        })
        .await
        .map_err(|e| format!("store task: {e}"))?
    }
}

/// Result of a Send: the turn id and the persisted user entry.
pub struct Queued {
    pub turn_id: String,
    pub entry: TranscriptEntry,
}

fn insert_entry(
    c: &Connection,
    mut entry: TranscriptEntry,
    subagent: bool,
) -> rusqlite::Result<TranscriptEntry> {
    entry.seq = 0;
    entry.at_ms = now_ms();
    c.execute(
        "INSERT INTO entries (agent_id, turn_id, subagent, body) VALUES (?1, ?2, ?3, ?4)",
        params![
            entry.agent_id,
            entry.turn_id,
            subagent,
            entry.encode_to_vec()
        ],
    )?;
    entry.seq = u64::try_from(c.last_insert_rowid()).unwrap_or_default();
    Ok(entry)
}

fn delete_rows(c: &Connection, id: &str) -> rusqlite::Result<bool> {
    c.execute("DELETE FROM entries WHERE agent_id = ?1", [id])?;
    c.execute("DELETE FROM turns WHERE agent_id = ?1", [id])?;
    Ok(c.execute("DELETE FROM agents WHERE id = ?1", [id])? > 0)
}

fn decode_entry(r: &rusqlite::Row<'_>) -> rusqlite::Result<TranscriptEntry> {
    let body = r
        .get_ref(1)?
        .as_blob()
        .map_err(|e| rusqlite::Error::FromSqlConversionFailure(1, Type::Blob, Box::new(e)))?;
    let mut entry = TranscriptEntry::decode(body)
        .map_err(|e| rusqlite::Error::FromSqlConversionFailure(1, Type::Blob, Box::new(e)))?;
    entry.seq = u64::try_from(r.get::<_, i64>(0)?).unwrap_or_default();
    Ok(entry)
}

fn agent_row(c: &Connection, id: &str) -> rusqlite::Result<Option<pb::Agent>> {
    c.query_row(
        "SELECT id, name, main_model, status, created_at_ms, updated_at_ms
         FROM agents WHERE id = ?1",
        [id],
        read_agent,
    )
    .optional()
}

fn read_agent(r: &rusqlite::Row<'_>) -> rusqlite::Result<pb::Agent> {
    Ok(pb::Agent {
        id: r.get(0)?,
        name: display_name(r.get(1)?),
        main_model: r.get(2)?,
        status: r.get(3)?,
        created_at_ms: r.get(4)?,
        updated_at_ms: r.get(5)?,
    })
}

fn display_name(name: String) -> String {
    if name.is_empty() {
        UNTITLED.to_owned()
    } else {
        name
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn open() -> (tempfile::TempDir, Store) {
        let dir = tempfile::tempdir().unwrap();
        let store = Store::open(&dir.path().join("agent.db")).unwrap();
        (dir, store)
    }

    async fn agent(store: &Store) -> String {
        store
            .create_agent(String::new(), "m".into())
            .await
            .unwrap()
            .id
    }

    #[tokio::test]
    async fn prune_takes_only_mcp_agents_whatever_an_owner_agent_is_named() {
        let (_dir, s) = open();
        let owner = s
            .create_agent("mcp: fix the build".into(), "m".into())
            .await
            .unwrap();
        let mcp = s
            .create_agent("mcp:client@sh-1".into(), "external".into())
            .await
            .unwrap();
        let pruned = s.prune_agents("external", i64::MAX).await.unwrap();
        assert_eq!(pruned, [mcp.id]);
        let left: Vec<String> = s
            .list_agents()
            .await
            .unwrap()
            .into_iter()
            .map(|a| a.id)
            .collect();
        assert_eq!(left, [owner.id]);
    }

    #[tokio::test]
    async fn a_turn_for_a_deleted_agent_is_not_queued() {
        let (_dir, store) = open();
        let id = agent(&store).await;
        assert!(store.delete_agent(id.clone()).await.unwrap());
        let queued = store.queue_turn(id.clone(), "hi".into()).await.unwrap();
        assert!(queued.is_none());
        assert!(store.unfinished_turns().await.unwrap().is_empty());
        assert!(
            store
                .transcript(id, 0, 10)
                .await
                .unwrap()
                .entries
                .is_empty()
        );
    }

    #[tokio::test]
    async fn transcript_seq_is_monotonic_and_paged() {
        let (_dir, store) = open();
        let ag = agent(&store).await;
        let q = store
            .queue_turn(ag.clone(), "u".into())
            .await
            .unwrap()
            .unwrap();
        for i in 0..5 {
            let entry = TranscriptEntry {
                agent_id: ag.clone(),
                turn_id: q.turn_id.clone(),
                role: pb::Role::Assistant.into(),
                body: Some(transcript_entry::Body::Text(pb::TextBlock {
                    text: format!("t{i}"),
                    ..Default::default()
                })),
                ..Default::default()
            };
            store.append(entry, false).await.unwrap();
        }
        let page = store.transcript(ag.clone(), 0, 4).await.unwrap();
        assert_eq!(page.entries.len(), 4);
        assert!(page.has_more);
        let seqs: Vec<u64> = page.entries.iter().map(|e| e.seq).collect();
        assert!(seqs.windows(2).all(|w| w[0] < w[1]), "seqs {seqs:?}");
        let rest = store
            .transcript(ag, *seqs.last().unwrap(), 10)
            .await
            .unwrap();
        assert_eq!(rest.entries.len(), 2);
        assert!(!rest.has_more);
    }

    #[tokio::test]
    async fn history_orders_by_turn_then_seq_and_skips_subagent() {
        let (_dir, store) = open();
        let ag = agent(&store).await;
        let t1 = store
            .queue_turn(ag.clone(), "first".into())
            .await
            .unwrap()
            .unwrap();
        let t2 = store
            .queue_turn(ag.clone(), "second".into())
            .await
            .unwrap()
            .unwrap();
        let reply = |turn: &str, text: &str| TranscriptEntry {
            agent_id: ag.clone(),
            turn_id: turn.into(),
            role: pb::Role::Assistant.into(),
            body: Some(transcript_entry::Body::Text(pb::TextBlock {
                text: text.into(),
                ..Default::default()
            })),
            ..Default::default()
        };
        store.append(reply(&t1.turn_id, "sub"), true).await.unwrap();
        store
            .append(reply(&t1.turn_id, "answer1"), false)
            .await
            .unwrap();
        let texts = |entries: Vec<TranscriptEntry>| -> Vec<String> {
            entries
                .into_iter()
                .filter_map(|e| match e.body {
                    Some(transcript_entry::Body::Text(t)) => Some(t.text),
                    _ => None,
                })
                .collect()
        };
        let upto_first = store.history(ag.clone(), t1.turn_id.clone()).await.unwrap();
        assert_eq!(texts(upto_first), ["first", "answer1"]);
        let upto_second = store.history(ag.clone(), t2.turn_id.clone()).await.unwrap();
        assert_eq!(texts(upto_second), ["first", "answer1", "second"]);
        assert_eq!(store.unfinished_turns().await.unwrap().len(), 2);
        let end = TranscriptEntry {
            agent_id: ag.clone(),
            turn_id: t2.turn_id,
            role: pb::Role::System.into(),
            body: Some(transcript_entry::Body::TurnEnd(pb::TurnEnd::default())),
            ..Default::default()
        };
        store.append(end, false).await.unwrap();
        assert_eq!(store.unfinished_turns().await.unwrap().len(), 1);
    }
}
