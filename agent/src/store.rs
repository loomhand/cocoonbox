//! SQLite state: agents, turns, transcript entries and client-nonce dedupe.

use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use prost::Message;
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
CREATE TABLE IF NOT EXISTS nonces (
    scope TEXT NOT NULL,
    nonce TEXT NOT NULL,
    value TEXT NOT NULL,
    PRIMARY KEY (scope, nonce)
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

    /// Creates an agent, or returns the one created earlier with the same nonce.
    pub async fn create_agent(
        &self,
        name: String,
        main_model: String,
        nonce: String,
    ) -> Result<pb::Agent, String> {
        self.call(move |c| {
            let tx = c.transaction()?;
            if let Some(id) = nonce_value(&tx, "create", &nonce)? {
                let agent = agent_row(&tx, &id)?;
                return Ok(agent);
            }
            let now = now_ms();
            let mut agent = pb::Agent {
                id: new_id("ag"),
                name,
                status: AgentStatus::Idle.into(),
                main_model,
                created_at_ms: now,
                updated_at_ms: now,
            };
            tx.execute(
                "INSERT INTO agents (id, name, main_model, status, created_at_ms, updated_at_ms)
                 VALUES (?1, ?2, ?3, ?4, ?5, ?5)",
                params![agent.id, agent.name, agent.main_model, agent.status, now],
            )?;
            put_nonce(&tx, "create", &nonce, &agent.id)?;
            tx.commit()?;
            agent.name = display_name(agent.name);
            Ok(Some(agent))
        })
        .await?
        .ok_or_else(|| "agent for nonce vanished".to_owned())
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

    /// Deletes an agent with its turns, entries and nonces; returns whether it existed.
    pub async fn delete_agent(&self, id: String) -> Result<bool, String> {
        self.call(move |c| {
            let tx = c.transaction()?;
            let existed = delete_rows(&tx, &id)?;
            tx.commit()?;
            Ok(existed)
        })
        .await
    }

    /// Records a user turn, deduplicated by `(agent, nonce)`.
    pub async fn queue_turn(
        &self,
        agent_id: String,
        text: String,
        nonce: String,
    ) -> Result<Queued, String> {
        self.call(move |c| {
            let tx = c.transaction()?;
            let scope = format!("send:{agent_id}");
            if let Some(turn_id) = nonce_value(&tx, &scope, &nonce)? {
                return Ok(Queued {
                    turn_id,
                    entry: None,
                });
            }
            let turn_id = new_id("turn");
            tx.execute(
                "INSERT INTO turns (id, agent_id) VALUES (?1, ?2)",
                params![turn_id, agent_id],
            )?;
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
            put_nonce(&tx, &scope, &nonce, &turn_id)?;
            tx.commit()?;
            Ok(Queued {
                turn_id,
                entry: Some(entry),
            })
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

    /// Deletes agents named `prefix*` that are neither running nor waiting and were last
    /// updated before `before_ms`, with their turns, entries and nonces; returns their ids.
    pub async fn prune_agents(
        &self,
        prefix: String,
        before_ms: i64,
    ) -> Result<Vec<String>, String> {
        self.call(move |c| {
            let tx = c.transaction()?;
            let ids: Vec<String> = {
                let mut stmt = tx.prepare(
                    "SELECT id FROM agents WHERE substr(name, 1, length(?1)) = ?1
                     AND status NOT IN (?2, ?3) AND updated_at_ms < ?4",
                )?;
                let rows = stmt.query_map(
                    params![
                        prefix,
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
        self.call(move |c| c.backup(rusqlite::MAIN_DB, &path, None))
            .await
    }

    /// Replaces the database contents with the database at `path`.
    pub async fn restore_from(&self, path: PathBuf) -> Result<(), String> {
        self.call(move |c| {
            c.restore(
                rusqlite::MAIN_DB,
                &path,
                None::<fn(rusqlite::backup::Progress)>,
            )
        })
        .await
    }

    /// Folds the WAL back into the database file.
    pub async fn checkpoint(&self) -> Result<(), String> {
        self.call(|c| c.execute_batch("PRAGMA wal_checkpoint(TRUNCATE);"))
            .await
    }

    /// Persists an entry and returns it with its seq and timestamp.
    pub async fn append(
        &self,
        entry: TranscriptEntry,
        subagent: bool,
    ) -> Result<TranscriptEntry, String> {
        self.call(move |c| insert_entry(c, entry, subagent)).await
    }

    /// Marks a turn finished.
    pub async fn finish_turn(&self, turn_id: String) -> Result<(), String> {
        self.call(move |c| {
            c.execute("UPDATE turns SET done = 1 WHERE id = ?1", [turn_id])
                .map(drop)
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
                let entry = decode_entry(row.get(0)?, &row.get::<_, Vec<u8>>(1)?)?;
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
            let rows = stmt.query_map([agent_id, turn_id], |r| {
                decode_entry(r.get(0)?, &r.get::<_, Vec<u8>>(1)?)
            })?;
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

/// Result of a Send: the turn id and, for a new turn, the persisted user entry.
pub struct Queued {
    pub turn_id: String,
    pub entry: Option<TranscriptEntry>,
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
    c.execute(
        "DELETE FROM nonces WHERE scope = 'send:' || ?1 OR (scope = 'create' AND value = ?1)",
        [id],
    )?;
    Ok(c.execute("DELETE FROM agents WHERE id = ?1", [id])? > 0)
}

fn decode_entry(seq: i64, body: &[u8]) -> rusqlite::Result<TranscriptEntry> {
    let mut entry = TranscriptEntry::decode(body)
        .map_err(|e| rusqlite::Error::ToSqlConversionFailure(Box::new(e)))?;
    entry.seq = u64::try_from(seq).unwrap_or_default();
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

fn nonce_value(c: &Connection, scope: &str, nonce: &str) -> rusqlite::Result<Option<String>> {
    if nonce.is_empty() {
        return Ok(None);
    }
    c.query_row(
        "SELECT value FROM nonces WHERE scope = ?1 AND nonce = ?2",
        [scope, nonce],
        |r| r.get(0),
    )
    .optional()
}

fn put_nonce(c: &Connection, scope: &str, nonce: &str, value: &str) -> rusqlite::Result<()> {
    if nonce.is_empty() {
        return Ok(());
    }
    c.execute(
        "INSERT INTO nonces (scope, nonce, value) VALUES (?1, ?2, ?3)",
        [scope, nonce, value],
    )
    .map(drop)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn open() -> (tempfile::TempDir, Store) {
        let dir = tempfile::tempdir().unwrap();
        let store = Store::open(&dir.path().join("agent.db")).unwrap();
        (dir, store)
    }

    #[tokio::test]
    async fn create_dedupes_by_nonce() {
        let (_dir, store) = open();
        let a = store
            .create_agent("a".into(), "m".into(), "n1".into())
            .await
            .unwrap();
        let b = store
            .create_agent("other".into(), "m".into(), "n1".into())
            .await
            .unwrap();
        let c = store
            .create_agent("a".into(), "m".into(), String::new())
            .await
            .unwrap();
        assert_eq!(a.id, b.id);
        assert_ne!(a.id, c.id);
        assert_eq!(store.list_agents().await.unwrap().len(), 2);
    }

    #[tokio::test]
    async fn send_dedupes_by_nonce_per_agent() {
        let (_dir, store) = open();
        let first = store
            .queue_turn("ag1".into(), "hi".into(), "n".into())
            .await
            .unwrap();
        let again = store
            .queue_turn("ag1".into(), "hi".into(), "n".into())
            .await
            .unwrap();
        let other = store
            .queue_turn("ag2".into(), "hi".into(), "n".into())
            .await
            .unwrap();
        assert_eq!(first.turn_id, again.turn_id);
        assert!(first.entry.is_some());
        assert!(again.entry.is_none());
        assert_ne!(first.turn_id, other.turn_id);
        let page = store.transcript("ag1".into(), 0, 10).await.unwrap();
        assert_eq!(page.entries.len(), 1);
    }

    #[tokio::test]
    async fn transcript_seq_is_monotonic_and_paged() {
        let (_dir, store) = open();
        let q = store
            .queue_turn("ag".into(), "u".into(), String::new())
            .await
            .unwrap();
        for i in 0..5 {
            let entry = TranscriptEntry {
                agent_id: "ag".into(),
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
        let page = store.transcript("ag".into(), 0, 4).await.unwrap();
        assert_eq!(page.entries.len(), 4);
        assert!(page.has_more);
        let seqs: Vec<u64> = page.entries.iter().map(|e| e.seq).collect();
        assert!(seqs.windows(2).all(|w| w[0] < w[1]), "seqs {seqs:?}");
        let rest = store
            .transcript("ag".into(), *seqs.last().unwrap(), 10)
            .await
            .unwrap();
        assert_eq!(rest.entries.len(), 2);
        assert!(!rest.has_more);
    }

    #[tokio::test]
    async fn history_orders_by_turn_then_seq_and_skips_subagent() {
        let (_dir, store) = open();
        let t1 = store
            .queue_turn("ag".into(), "first".into(), String::new())
            .await
            .unwrap();
        let t2 = store
            .queue_turn("ag".into(), "second".into(), String::new())
            .await
            .unwrap();
        let reply = |turn: &str, text: &str| TranscriptEntry {
            agent_id: "ag".into(),
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
        let upto_first = store
            .history("ag".into(), t1.turn_id.clone())
            .await
            .unwrap();
        assert_eq!(texts(upto_first), ["first", "answer1"]);
        let upto_second = store
            .history("ag".into(), t2.turn_id.clone())
            .await
            .unwrap();
        assert_eq!(texts(upto_second), ["first", "answer1", "second"]);
        assert_eq!(store.unfinished_turns().await.unwrap().len(), 2);
        store.finish_turn(t2.turn_id).await.unwrap();
        assert_eq!(store.unfinished_turns().await.unwrap().len(), 1);
    }
}
