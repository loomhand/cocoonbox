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
const BROWSERS: [&str; 3] = ["Chrome", "Chromium", "Firefox"];
const NAME_EDGES: [char; 13] = [
    '/', ' ', '\t', '\n', '\'', '"', '=', '(', ')', ';', ',', '>', '<',
];
const READERS: [&str; 22] = [
    "cat",
    "head",
    "tail",
    "less",
    "more",
    "grep",
    "egrep",
    "fgrep",
    "rg",
    "wc",
    "file",
    "stat",
    "ls",
    "du",
    "diff",
    "cmp",
    "md5sum",
    "sha256sum",
    "xxd",
    "hexdump",
    "od",
    "strings",
];
const WRAPPERS: [&str; 5] = ["sudo", "env", "command", "time", "nohup"];

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

/// A note for a call that changes, as `changes` tells from a file name, a file an app window shows in its title
/// among `titles`: the app keeps its own copy, and its next save overwrites a change made outside it.
pub fn held_note(titles: &[String], changes: impl Fn(&str) -> bool) -> String {
    let mut note = String::new();
    for (title, name) in titles
        .iter()
        .filter_map(|t| Some((t, document(t)?)))
        .filter(|(_, name)| changes(name))
    {
        let _ = write!(
            note,
            "\nnote: the window \"{title}\" has {name} open, and its next save overwrites a change made here: close the file there before you change it, or reload it there after"
        );
    }
    note
}

/// Whether shell `command` can write the file `name`: a part of it names the file as a redirection target, or runs `tee`,
/// `sed -i`, `cp` or `mv` onto it, or a program outside [`READERS`] that names it.
pub fn writes(command: &str, name: &str) -> bool {
    command
        .split(['|', ';', '&', '\n'])
        .filter(|part| names(part, name))
        .any(|part| {
            let words: Vec<&str> = part.split_whitespace().collect();
            let program = words
                .iter()
                .map(|w| w.trim_start_matches(['(', '{', '!', '`', '$']))
                .find(|w| !w.is_empty() && !w.contains('=') && !WRAPPERS.contains(w))
                .map_or("", |w| w.rsplit('/').next().unwrap_or(w));
            redirected(part, name)
                || match program {
                    "tee" => true,
                    "sed" => words
                        .iter()
                        .any(|w| w.starts_with("-i") || w.starts_with("--in-place")),
                    "cp" | "mv" => last(part, name),
                    p => !READERS.contains(&p),
                }
        })
}

/// Whether `text` holds `name` as a whole path segment or word.
pub fn names(text: &str, name: &str) -> bool {
    spans(text, name).next().is_some()
}

/// The file name in the title of a document window outside a browser, such as `answers.xlsx` of
/// `answers.xlsx - LibreOffice Calc` or `*~/notes.txt - Mousepad`.
fn document(title: &str) -> Option<&str> {
    let (doc, app) = title.rsplit_once(" - ")?;
    if BROWSERS.iter().any(|b| app.contains(b)) {
        return None;
    }
    let path = doc.split(" (").next()?.trim().trim_start_matches('*');
    let name = path.rsplit('/').next()?;
    let (stem, ext) = name.rsplit_once('.')?;
    let file = !stem.is_empty()
        && (1..=5).contains(&ext.len())
        && ext.chars().all(|c| c.is_ascii_alphanumeric());
    file.then_some(name)
}

/// Where `text` holds `name` as a whole path segment or word.
fn spans<'a>(text: &'a str, name: &'a str) -> impl Iterator<Item = usize> + 'a {
    text.match_indices(name).map(|(i, _)| i).filter(move |&i| {
        let before = text[..i].chars().next_back();
        let after = text[i + name.len()..].chars().next();
        before.is_none_or(|c| NAME_EDGES.contains(&c))
            && after.is_none_or(|c| NAME_EDGES.contains(&c))
    })
}

/// Whether `name` in `part` follows a `>` past only its folders, quotes and spaces.
fn redirected(part: &str, name: &str) -> bool {
    spans(part, name).any(|i| {
        part[..i]
            .trim_end_matches(|c: char| !c.is_whitespace() && c != '>' && c != '"' && c != '\'')
            .trim_end_matches(['"', '\'', ' ', '\t'])
            .ends_with('>')
    })
}

/// Whether `part` ends with `name` as a whole path segment or word, past closing quotes and slashes.
fn last(part: &str, name: &str) -> bool {
    let tail = part.trim_end().trim_end_matches(['"', '\'', '/']);
    tail.strip_suffix(name).is_some_and(|head| {
        head.chars()
            .next_back()
            .is_none_or(|c| NAME_EDGES.contains(&c))
    })
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

    #[test]
    fn a_call_that_changes_a_file_an_app_window_shows_gets_a_note() {
        let titles: Vec<String> = [
            "answers.xlsx - LibreOffice Calc",
            "*~/notes.txt - Mousepad",
            "report.md (~/Desktop) - gedit",
            "example.com - Google Chrome",
            "Desktop - Thunar",
            "",
        ]
        .map(str::to_owned)
        .into();
        let shell = |command: &str| held_note(&titles, |name| writes(command, name));
        assert_eq!(
            shell(
                "python3 -c \"import openpyxl; wb = openpyxl.load_workbook('/home/user/answers.xlsx')\""
            ),
            "\nnote: the window \"answers.xlsx - LibreOffice Calc\" has answers.xlsx open, and its next save overwrites a change made here: close the file there before you change it, or reload it there after"
        );
        assert!(
            held_note(&titles, |name| names("/home/user/notes.txt", name))
                .contains("\"*~/notes.txt - Mousepad\"")
        );
        assert!(shell("sed -i s/a/b/ report.md").contains("report.md open"));
        for quiet in [
            "curl -s example.com",
            "ls Desktop",
            "cat old_answers.xlsx",
            "cp answers.xlsx.bak /tmp",
            "cat ~/notes.txt",
            "",
        ] {
            assert_eq!(shell(quiet), "", "{quiet}");
        }
    }

    #[test]
    fn only_a_shell_command_that_can_write_a_file_changes_it() {
        for command in [
            "echo hi >> notes.txt",
            "echo hi>/home/user/notes.txt",
            "cat a | tee -a notes.txt",
            "sed -i.bak s/a/b/ ~/notes.txt",
            "cp /tmp/new.txt notes.txt",
            "mv draft.txt '/home/user/notes.txt'",
            "python3 fix.py notes.txt",
            "cd ~ && rm notes.txt",
            "X=1 sudo truncate -s0 notes.txt",
        ] {
            assert!(writes(command, "notes.txt"), "{command}");
        }
        for command in [
            "cat notes.txt",
            "grep -n todo /home/user/notes.txt | head -5",
            "head -c 100 notes.txt 2>&1",
            "sed -n 1,5p notes.txt",
            "cat notes.txt > /tmp/copy.txt",
            "cp notes.txt /tmp/",
            "mv notes.txt backup/",
            "wc -l notes.txt && ls -la",
            "echo hi > notes.txt.bak",
        ] {
            assert!(!writes(command, "notes.txt"), "{command}");
        }
        assert!(writes(
            "printf x > \"/home/user/my notes.txt\"",
            "my notes.txt"
        ));
        assert!(!writes("cat \"my notes.txt\"", "my notes.txt"));
    }

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
