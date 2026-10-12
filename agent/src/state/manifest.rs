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

#[cfg(test)]
mod tests {
    use base64::Engine;
    use base64::engine::general_purpose::STANDARD as B64;
    use serde_json::Value;

    use super::*;
    use crate::state::crypto::Keys;

    const FIXTURE: &str = include_str!("../../../docs/fixtures/state-v1.json");

    #[test]
    fn the_contract_fixture_opens_and_serializes_byte_for_byte() {
        let f: Value = serde_json::from_str(FIXTURE).unwrap();
        let keys = Keys::derive(&hex::decode(f["key_hex"].as_str().unwrap()).unwrap()).unwrap();
        let object = |name: &str, plain: Vec<u8>| {
            let id = f[name]["id"].as_str().unwrap();
            let sealed = B64.decode(f[name]["sealed_b64"].as_str().unwrap()).unwrap();
            assert_eq!(keys.id(&plain).unwrap(), id, "{name}");
            assert_eq!(keys.open(id, &sealed, 1 << 20).unwrap(), plain, "{name}");
            id.to_owned()
        };
        let plain = f["manifest"]["plain"].as_str().unwrap().as_bytes().to_vec();
        object("manifest", plain.clone());
        let chunk = object(
            "chunk",
            B64.decode(f["chunk"]["plain_b64"].as_str().unwrap())
                .unwrap(),
        );
        let m: Manifest = serde_json::from_slice(&plain).unwrap();
        assert_eq!(m.version, VERSION);
        assert_eq!(m.files[2].chunks, [chunk]);
        assert_eq!(serde_json::to_vec(&m).unwrap(), plain);
    }
}
