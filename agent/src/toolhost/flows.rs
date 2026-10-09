//! The flow files of `/workspace/flows` that the library sync reads and writes; never MCP tools.

use std::path::{Path, PathBuf};

use serde::Deserialize;
use serde_json::{Value, json};
use sha2::{Digest, Sha256};

use super::files;

/// The largest flow the library takes, in bytes.
pub const MAX_BYTES: u64 = 128 << 10;
const DIR: &str = "flows";
const MAX_NAME: usize = 96;

#[derive(Deserialize)]
struct NameArgs {
    name: String,
}

#[derive(Deserialize)]
struct WriteArgs {
    name: String,
    content: String,
}

/// Whether `name` is a flow file the library syncs: a `.json` or `.mts` name of letters, digits, `.`, `_` and `-`.
pub fn valid(name: &str) -> bool {
    name.len() <= MAX_NAME
        && name
            .chars()
            .next()
            .is_some_and(|c| c.is_ascii_alphanumeric())
        && name
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '_' | '-'))
        && (name.ends_with(".json") || name.ends_with(".mts"))
}

/// Hex SHA-256 of `data`.
pub fn digest(data: &[u8]) -> String {
    hex::encode(Sha256::digest(data))
}

/// `flows_scan`: JSON `{name: sha256}` of the regular flow files of at most [`MAX_BYTES`] that are UTF-8 text.
pub async fn scan(workspace: &Path) -> Result<String, String> {
    let dir = workspace.join(DIR);
    let mut out = serde_json::Map::new();
    let mut entries = match tokio::fs::read_dir(&dir).await {
        Ok(entries) => entries,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok("{}".to_owned()),
        Err(e) => return Err(format!("list {}: {e}", dir.display())),
    };
    while let Some(entry) = entries
        .next_entry()
        .await
        .map_err(|e| format!("list {}: {e}", dir.display()))?
    {
        let name = entry.file_name().to_string_lossy().into_owned();
        let Ok(meta) = entry.metadata().await else {
            continue;
        };
        if !valid(&name) || !meta.is_file() || meta.len() > MAX_BYTES {
            continue;
        }
        let Ok(data) = tokio::fs::read(entry.path()).await else {
            continue;
        };
        if std::str::from_utf8(&data).is_ok() {
            out.insert(name, digest(&data).into());
        }
    }
    Ok(Value::Object(out).to_string())
}

/// `flows_read {name}`: the flow's text.
pub async fn read(workspace: &Path, args: Value) -> Result<String, String> {
    let NameArgs { name } = parse(args)?;
    let path = path_of(workspace, &name)?;
    tokio::fs::read_to_string(&path)
        .await
        .map_err(|e| format!("read {}: {e}", path.display()))
}

/// `flows_write {name, content}`: replaces the flow through a temporary file, so a reader never sees half of it.
pub async fn write(workspace: &Path, args: Value) -> Result<String, String> {
    let WriteArgs { name, content } = parse(args)?;
    let path = path_of(workspace, &name)?;
    let tmp = path.with_file_name(format!(".{name}.sync"));
    files::write_bytes(&tmp, content.as_bytes()).await?;
    tokio::fs::rename(&tmp, &path)
        .await
        .map_err(|e| format!("rename {}: {e}", path.display()))?;
    Ok(json!({"written": name}).to_string())
}

/// `flows_remove {name}`: deletes the flow; a missing one is no error.
pub async fn remove(workspace: &Path, args: Value) -> Result<String, String> {
    let NameArgs { name } = parse(args)?;
    let path = path_of(workspace, &name)?;
    match tokio::fs::remove_file(&path).await {
        Ok(()) => Ok(json!({"removed": name}).to_string()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            Ok(json!({"removed": name}).to_string())
        }
        Err(e) => Err(format!("remove {}: {e}", path.display())),
    }
}

fn path_of(workspace: &Path, name: &str) -> Result<PathBuf, String> {
    if !valid(name) {
        return Err(format!("{name:?} is no flow name"));
    }
    Ok(workspace.join(DIR).join(name))
}

fn parse<T: serde::de::DeserializeOwned>(args: Value) -> Result<T, String> {
    serde_json::from_value(args).map_err(|e| format!("invalid arguments: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn valid_takes_plain_json_and_mts_names_only() {
        for name in ["mousepad-save.json", "save_note.mts", "A1.b.json"] {
            assert!(valid(name), "{name}");
        }
        for name in [
            ".pending.json",
            ".runs.jsonl",
            "INDEX",
            "x.txt",
            "a/b.json",
            "../a.json",
            "-a.json",
        ] {
            assert!(!valid(name), "{name}");
        }
        assert!(!valid(&format!("{}.json", "a".repeat(92))));
    }

    #[tokio::test]
    async fn scan_write_read_remove_round_trip() {
        let dir = tempfile::tempdir().unwrap();
        let ws = dir.path();
        assert_eq!(scan(ws).await.unwrap(), "{}");
        write(ws, json!({"name": "a.json", "content": "{\"v\":1}"}))
            .await
            .unwrap();
        std::fs::write(ws.join("flows/INDEX"), "a()").unwrap();
        std::fs::write(ws.join("flows/bin.json"), [0xff, 0xfe]).unwrap();
        std::fs::write(
            ws.join("flows/big.mts"),
            vec![b'a'; usize::try_from(MAX_BYTES).unwrap() + 1],
        )
        .unwrap();
        let scanned: Value = serde_json::from_str(&scan(ws).await.unwrap()).unwrap();
        assert_eq!(scanned, json!({"a.json": digest(b"{\"v\":1}")}));
        assert_eq!(
            read(ws, json!({"name": "a.json"})).await.unwrap(),
            "{\"v\":1}"
        );
        assert!(read(ws, json!({"name": "../x.json"})).await.is_err());
        remove(ws, json!({"name": "a.json"})).await.unwrap();
        remove(ws, json!({"name": "a.json"})).await.unwrap();
        assert!(!ws.join("flows/a.json").exists());
    }
}
