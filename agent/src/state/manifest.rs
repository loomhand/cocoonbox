//! The snapshot manifest: every file with its chunk list, plus agent.db.

use serde::{Deserialize, Serialize};

use crate::toolhost::state::Entry;

/// Manifest format version.
pub const VERSION: u32 = 1;

/// One snapshot; ownership is not stored because every file belongs to `user`.
#[derive(Debug, Default, Deserialize, Serialize)]
pub struct Manifest {
    pub version: u32,
    pub parent: String,
    pub key_id: String,
    pub taken_at_ms: i64,
    pub consistency: String,
    pub files: Vec<FileRecord>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub agent_db: Option<Blob>,
    /// Files left out because they kept changing while being read.
    #[serde(default)]
    pub skipped: Vec<String>,
}

/// A walked entry and, for regular files, its chunks in order.
#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct FileRecord {
    #[serde(flatten)]
    pub entry: Entry,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub chunks: Vec<String>,
}

/// A byte stream stored as chunks.
#[derive(Debug, Default, Deserialize, Serialize)]
pub struct Blob {
    pub size: u64,
    pub chunks: Vec<String>,
}
