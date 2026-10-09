//! File tools. Relative paths resolve under the workspace; absolute paths are
//! used as given, so the toolhost's uid (`user`) is the access boundary.
//! A call with `workspace_only` (a read-only share) reads only beneath the workspace, and never in its hidden folder.

use std::fmt::Write as _;
use std::fs::File;
use std::os::fd::AsRawFd;
use std::path::{Path, PathBuf};

use serde::Deserialize;
use tokio::io::AsyncReadExt;

use crate::sys;

const READ_LINES: usize = 2000;
const LINE_CHARS: usize = 2000;
const READ_BYTES: usize = 100_000;
const LIST_ENTRIES: usize = 500;

#[derive(Deserialize)]
struct ReadArgs {
    path: String,
    #[serde(default)]
    offset: Option<usize>,
    #[serde(default)]
    limit: Option<usize>,
    #[serde(default)]
    workspace_only: bool,
}

#[derive(Deserialize)]
struct WriteArgs {
    path: String,
    content: String,
}

#[derive(Deserialize)]
struct EditArgs {
    path: String,
    old_string: String,
    new_string: String,
    #[serde(default)]
    replace_all: bool,
}

#[derive(Deserialize)]
struct ListArgs {
    #[serde(default)]
    path: Option<String>,
    #[serde(default)]
    workspace_only: bool,
}

/// Resolves a model-supplied path against `workspace`.
pub fn resolve(workspace: &Path, path: &str) -> PathBuf {
    let p = Path::new(path);
    if p.is_absolute() {
        p.to_path_buf()
    } else {
        workspace.join(p)
    }
}

/// `read_file`: numbered lines from `offset` (1-based), capped.
pub async fn read(
    workspace: &Path,
    hidden: Option<&Path>,
    args: serde_json::Value,
) -> Result<String, String> {
    let a: ReadArgs = parse(args)?;
    let path = resolve(workspace, &a.path);
    let data = if a.workspace_only {
        let mut data = Vec::new();
        tokio::fs::File::from_std(open_beneath(
            workspace,
            hidden,
            &path,
            libc::O_RDONLY | libc::O_NONBLOCK,
        )?)
        .read_to_end(&mut data)
        .await
        .map(|_| data)
    } else {
        tokio::fs::read(&path).await
    }
    .map_err(|e| format!("read {}: {e}", path.display()))?;
    if data.iter().take(8192).any(|&b| b == 0) {
        return Ok(format!(
            "{} is a binary file of {} bytes",
            path.display(),
            data.len()
        ));
    }
    let text = String::from_utf8_lossy(&data);
    let start = a.offset.unwrap_or(1).max(1);
    let limit = a.limit.unwrap_or(READ_LINES).clamp(1, READ_LINES);
    let mut out = String::new();
    let mut shown = 0;
    let mut total = 0;
    for (i, line) in text.lines().enumerate() {
        total = i + 1;
        if total < start || shown >= limit || out.len() >= READ_BYTES {
            continue;
        }
        let line: String = line.chars().take(LINE_CHARS).collect();
        let _ = writeln!(out, "{total:>6}\t{line}");
        shown += 1;
    }
    if shown == 0 {
        return Ok(format!(
            "{} has {total} lines; nothing at offset {start}",
            path.display()
        ));
    }
    let last = start + shown - 1;
    if last < total {
        let _ = writeln!(
            out,
            "[showing lines {start}-{last} of {total}; pass offset to read more]"
        );
    }
    Ok(out)
}

/// `write_file`: creates parent directories and replaces the file.
pub async fn write(workspace: &Path, args: serde_json::Value) -> Result<String, String> {
    let a: WriteArgs = parse(args)?;
    let path = resolve(workspace, &a.path);
    write_bytes(&path, a.content.as_bytes()).await?;
    Ok(format!(
        "wrote {} bytes to {}",
        a.content.len(),
        path.display()
    ))
}

/// `edit_file`: exact string replacement; the match must be unique unless `replace_all`.
pub async fn edit(workspace: &Path, args: serde_json::Value) -> Result<String, String> {
    let a: EditArgs = parse(args)?;
    if a.old_string.is_empty() {
        return Err("old_string must not be empty".to_owned());
    }
    let path = resolve(workspace, &a.path);
    let text = tokio::fs::read_to_string(&path)
        .await
        .map_err(|e| format!("read {}: {e}", path.display()))?;
    let count = text.matches(&a.old_string).count();
    let updated = match count {
        0 => return Err(format!("old_string not found in {}", path.display())),
        1 => text.replacen(&a.old_string, &a.new_string, 1),
        _ if a.replace_all => text.replace(&a.old_string, &a.new_string),
        n => {
            return Err(format!(
                "old_string occurs {n} times in {}; add surrounding context or set replace_all",
                path.display()
            ));
        }
    };
    write_bytes(&path, updated.as_bytes()).await?;
    Ok(format!(
        "replaced {count} occurrence(s) in {}",
        path.display()
    ))
}

/// `list_dir`: sorted entries with type and size.
pub async fn list(
    workspace: &Path,
    hidden: Option<&Path>,
    args: serde_json::Value,
) -> Result<String, String> {
    let a: ListArgs = parse(args)?;
    let path = resolve(workspace, a.path.as_deref().unwrap_or("."));
    let beneath = if a.workspace_only {
        Some(open_beneath(workspace, hidden, &path, libc::O_DIRECTORY)?)
    } else {
        None
    };
    let source = beneath.as_ref().map_or_else(
        || path.clone(),
        |d| PathBuf::from(format!("/proc/self/fd/{}", d.as_raw_fd())),
    );
    let mut dir = tokio::fs::read_dir(&source)
        .await
        .map_err(|e| format!("list {}: {e}", path.display()))?;
    let mut entries = Vec::new();
    while let Some(entry) = dir
        .next_entry()
        .await
        .map_err(|e| format!("list {}: {e}", path.display()))?
    {
        let name = entry.file_name().to_string_lossy().into_owned();
        let line = match entry.metadata().await {
            Ok(m) if m.is_dir() => format!("{name}/"),
            Ok(m) => format!("{name}  ({} bytes)", m.len()),
            Err(_) => name,
        };
        entries.push(line);
    }
    entries.sort();
    let total = entries.len();
    entries.truncate(LIST_ENTRIES);
    let mut out = format!("{}:\n", path.display());
    for e in &entries {
        out.push_str(e);
        out.push('\n');
    }
    if total > LIST_ENTRIES {
        let _ = writeln!(out, "[{} more entries not shown]", total - LIST_ENTRIES);
    }
    Ok(out)
}

/// Writes `data` to `path`, creating parent directories.
pub async fn write_bytes(path: &Path, data: &[u8]) -> Result<(), String> {
    if let Some(parent) = path.parent() {
        tokio::fs::create_dir_all(parent)
            .await
            .map_err(|e| format!("create {}: {e}", parent.display()))?;
    }
    tokio::fs::write(path, data)
        .await
        .map_err(|e| format!("write {}: {e}", path.display()))
}

/// Opens `path` so that it resolves beneath `workspace` and outside `hidden`, `..` and symlinks included.
fn open_beneath(
    workspace: &Path,
    hidden: Option<&Path>,
    path: &Path,
    flags: libc::c_int,
) -> Result<File, String> {
    let refused = || {
        format!(
            "{} is refused: a read-only share reads files only under {}",
            path.display(),
            workspace.display()
        )
    };
    let rel = path.strip_prefix(workspace).map_err(|_| refused())?;
    let rel = if rel.as_os_str().is_empty() {
        Path::new(".")
    } else {
        rel
    };
    let root = File::open(workspace).map_err(|e| format!("open {}: {e}", workspace.display()))?;
    let file = sys::open_beneath(&root, rel, flags).map_err(|e| match e.raw_os_error() {
        Some(libc::EXDEV) => refused(),
        _ => format!("open {}: {e}", path.display()),
    })?;
    if let Some(hidden) = hidden
        && std::fs::read_link(format!("/proc/self/fd/{}", file.as_raw_fd()))
            .is_ok_and(|real| real.starts_with(hidden))
    {
        return Err(format!(
            "{} is refused: a read-only share does not read saved flows",
            path.display()
        ));
    }
    Ok(file)
}

fn parse<T: serde::de::DeserializeOwned>(args: serde_json::Value) -> Result<T, String> {
    serde_json::from_value(args).map_err(|e| format!("invalid arguments: {e}"))
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    #[tokio::test]
    async fn write_read_edit_list_round_trip() {
        let dir = tempfile::tempdir().unwrap();
        let ws = dir.path();
        write(ws, json!({"path": "a/b.txt", "content": "one\ntwo\ntwo\n"}))
            .await
            .unwrap();
        let out = read(
            ws,
            None,
            json!({"path": "a/b.txt", "offset": 2, "limit": 1}),
        )
        .await
        .unwrap();
        assert!(out.starts_with("     2\ttwo\n"), "{out}");
        assert!(out.contains("showing lines 2-2 of 3"), "{out}");
        let err = edit(
            ws,
            json!({"path": "a/b.txt", "old_string": "two", "new_string": "2"}),
        )
        .await
        .unwrap_err();
        assert!(err.contains("occurs 2 times"), "{err}");
        edit(
            ws,
            json!({"path": "a/b.txt", "old_string": "one", "new_string": "1"}),
        )
        .await
        .unwrap();
        edit(
            ws,
            json!({"path": "a/b.txt", "old_string": "two", "new_string": "2", "replace_all": true}),
        )
        .await
        .unwrap();
        assert_eq!(
            std::fs::read_to_string(ws.join("a/b.txt")).unwrap(),
            "1\n2\n2\n"
        );
        let listing = list(ws, None, json!({})).await.unwrap();
        assert!(listing.contains("a/\n"), "{listing}");
        let listing = list(
            ws,
            None,
            json!({"path": ws.join("a").display().to_string()}),
        )
        .await
        .unwrap();
        assert!(listing.contains("b.txt  (6 bytes)"), "{listing}");
    }

    #[cfg(target_os = "linux")]
    #[tokio::test]
    async fn a_read_only_share_does_not_read_the_hidden_folder() {
        let dir = tempfile::tempdir().unwrap();
        let ws = dir.path();
        let hidden = ws.join("flows");
        write(ws, json!({"path": "flows/a.json", "content": "{}"}))
            .await
            .unwrap();
        write(ws, json!({"path": "notes.txt", "content": "hi"}))
            .await
            .unwrap();
        std::os::unix::fs::symlink("flows", ws.join("link")).unwrap();
        for path in ["flows/a.json", "link/a.json"] {
            let err = read(
                ws,
                Some(&hidden),
                json!({"path": path, "workspace_only": true}),
            )
            .await
            .unwrap_err();
            assert!(err.contains("does not read saved flows"), "{err}");
        }
        let err = list(
            ws,
            Some(&hidden),
            json!({"path": "flows", "workspace_only": true}),
        )
        .await
        .unwrap_err();
        assert!(err.contains("does not read saved flows"), "{err}");
        let out = read(
            ws,
            Some(&hidden),
            json!({"path": "notes.txt", "workspace_only": true}),
        )
        .await
        .unwrap();
        assert!(out.contains("hi"), "{out}");
        let owner = read(ws, Some(&hidden), json!({"path": "flows/a.json"}))
            .await
            .unwrap();
        assert!(owner.contains("{}"), "{owner}");
    }

    #[test]
    fn relative_paths_resolve_under_workspace() {
        assert_eq!(
            resolve(Path::new("/workspace"), "x/y"),
            Path::new("/workspace/x/y")
        );
        assert_eq!(
            resolve(Path::new("/workspace"), "/tmp/z"),
            Path::new("/tmp/z")
        );
    }
}
