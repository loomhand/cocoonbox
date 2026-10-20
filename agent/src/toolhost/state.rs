//! State-layer file access as `user`: walk, streamed reads and writes, directory setup and atomic swaps.

use std::collections::HashSet;
use std::fs::{self, FileTimes};
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};

use super::rpc::{self, Output, Stamp};
use crate::sys;

pub const SQLITE_MAGIC: &[u8; 16] = b"SQLite format 3\0";

const READ_BLOCK: usize = 1 << 20;
const TRASH_PREFIX: &str = ".cb-trash-";
const PLACE_TRIES: u32 = 100;

/// A tree to walk, named in the manifest.
#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Root {
    pub name: String,
    pub path: PathBuf,
    /// Relative path prefixes left out of this root alone, e.g. `Library` of a macOS home.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub excluded: Vec<String>,
}

/// Paths left out of a snapshot, matched against `/`-separated paths relative to a root.
#[derive(Clone, Debug, Default, Deserialize, Serialize)]
pub struct Excludes {
    /// Relative path prefixes, e.g. `.cache`.
    pub prefixes: Vec<String>,
    /// Exact path components anywhere, e.g. `Code Cache`.
    pub names: Vec<String>,
    /// Leading characters of a file name, e.g. `Singleton`.
    pub name_prefixes: Vec<String>,
    /// Trailing characters of a file name, e.g. `-shm`.
    #[serde(default)]
    pub name_suffixes: Vec<String>,
}

impl Excludes {
    /// Whether `rel` is excluded.
    pub fn matches(&self, rel: &str) -> bool {
        self.prefixes.iter().any(|p| under(rel, p))
            || rel.split('/').any(|c| self.names.iter().any(|n| n == c))
            || rel.rsplit('/').next().is_some_and(|f| {
                self.name_prefixes.iter().any(|p| f.starts_with(p.as_str()))
                    || self.name_suffixes.iter().any(|x| f.ends_with(x.as_str()))
            })
    }
}

/// One walked entry.
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
pub struct Entry {
    pub root: String,
    pub path: String,
    pub kind: Kind,
    pub mode: u32,
    pub mtime_ms: i64,
    pub size: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub target: Option<String>,
    /// Change time of a file; the plan's reuse key, kept out of the manifest.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ctime_ns: Option<i64>,
}

/// Kinds of entries a snapshot keeps; sockets, fifos and devices are skipped.
#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Kind {
    File,
    Dir,
    Symlink,
}

/// A file listing absolute paths to leave out, one per line; only lines under the root named `root` count.
#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Listed {
    pub file: PathBuf,
    pub root: String,
}

#[derive(Deserialize)]
struct WalkArgs {
    roots: Vec<Root>,
    #[serde(default)]
    excludes: Excludes,
    #[serde(default)]
    listed: Option<Listed>,
}

#[derive(Deserialize)]
struct ReadArgs {
    path: PathBuf,
    #[serde(default)]
    sqlite_backup: bool,
}

#[derive(Deserialize)]
struct WriteArgs {
    path: PathBuf,
    mode: u32,
    mtime_ms: i64,
}

#[derive(Deserialize)]
struct ApplyArgs {
    #[serde(default)]
    dirs: Vec<DirSpec>,
    #[serde(default)]
    symlinks: Vec<LinkSpec>,
    /// Final directory modes, set last and deepest first so a read-only directory never blocks its contents.
    #[serde(default)]
    modes: Vec<DirSpec>,
}

#[derive(Deserialize)]
struct DirSpec {
    path: PathBuf,
    mode: u32,
}

#[derive(Deserialize)]
struct LinkSpec {
    path: PathBuf,
    target: String,
}

#[derive(Deserialize)]
struct SwapArgs {
    root: PathBuf,
    staging: PathBuf,
    names: Vec<String>,
    #[serde(default)]
    keep: Vec<String>,
}

#[derive(Deserialize)]
struct PlaceArgs {
    staging: PathBuf,
    dir: PathBuf,
    name: String,
}

/// `state_walk`: every file, directory and symlink under the roots, sorted, minus exclusions.
pub async fn walk(args: Value) -> Result<String, String> {
    let a: WalkArgs = rpc::parse(args)?;
    let entries = tokio::task::spawn_blocking(move || {
        let mut out = Vec::new();
        for root in &a.roots {
            let skip = a
                .listed
                .as_ref()
                .filter(|l| l.root == root.name)
                .map(|l| listed_paths(&l.file, &root.path))
                .unwrap_or_default();
            walk_root(root, &a.excludes, &skip, &mut out);
        }
        out
    })
    .await
    .map_err(|e| format!("walk task: {e}"))?;
    serde_json::to_string(&entries).map_err(|e| format!("encode walk: {e}"))
}

/// `state_read`: a stamp frame, the file as data frames, then a stamp taken after the read.
/// With `sqlite_backup`, an SQLite database is streamed from an online backup instead.
pub async fn read<W: AsyncWrite + Unpin>(args: Value, w: &mut W) -> Result<(), String> {
    let a: ReadArgs = rpc::parse(args)?;
    if a.sqlite_backup
        && let Some(copy) = sqlite_backup(&a.path).await
    {
        let result = stream_backup(&copy, w).await;
        let _ = tokio::fs::remove_file(&copy).await;
        return result;
    }
    let mut file = match tokio::fs::File::open(&a.path).await {
        Ok(f) => f,
        Err(e) => {
            let stamp = Stamp {
                error: Some(format!("open {}: {e}", a.path.display())),
                ..Stamp::default()
            };
            return rpc::write_frame(w, &stamp).await;
        }
    };
    rpc::write_frame(w, &stamp_of(&a.path)).await?;
    send_blocks(&mut file, &a.path, w).await?;
    rpc::write_frame(w, &stamp_of(&a.path)).await?;
    w.flush().await.map_err(|e| format!("flush: {e}"))
}

async fn stream_backup<W: AsyncWrite + Unpin>(copy: &Path, w: &mut W) -> Result<(), String> {
    let mut stamp = stamp_of(copy);
    stamp.backup = true;
    rpc::write_frame(w, &stamp).await?;
    let mut file = tokio::fs::File::open(copy)
        .await
        .map_err(|e| format!("open {}: {e}", copy.display()))?;
    send_blocks(&mut file, copy, w).await?;
    rpc::write_frame(w, &stamp).await?;
    w.flush().await.map_err(|e| format!("flush: {e}"))
}

async fn send_blocks<W: AsyncWrite + Unpin>(
    file: &mut tokio::fs::File,
    path: &Path,
    w: &mut W,
) -> Result<(), String> {
    let mut buf = vec![0u8; READ_BLOCK];
    loop {
        let n = file
            .read(&mut buf)
            .await
            .map_err(|e| format!("read {}: {e}", path.display()))?;
        if n == 0 {
            break;
        }
        rpc::write_data(w, &buf[..n]).await?;
    }
    rpc::write_data(w, &[]).await
}

/// Copies an SQLite database through the online backup API; `None` when `path` is not
/// SQLite or the database is locked, so the caller falls back to a plain read.
async fn sqlite_backup(path: &Path) -> Option<PathBuf> {
    let mut head = [0u8; 16];
    let mut f = tokio::fs::File::open(path).await.ok()?;
    f.read_exact(&mut head).await.ok()?;
    if &head != SQLITE_MAGIC {
        return None;
    }
    let src = path.to_path_buf();
    let dst = std::env::temp_dir().join(crate::new_id("cb-sqlite"));
    let copy = dst.clone();
    let done = tokio::task::spawn_blocking(move || {
        let flags =
            rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY | rusqlite::OpenFlags::SQLITE_OPEN_NO_MUTEX;
        let conn = rusqlite::Connection::open_with_flags(&src, flags)?;
        conn.busy_timeout(Duration::from_millis(500))?;
        conn.backup(rusqlite::MAIN_DB, &copy, None)
    })
    .await
    .ok()?;
    match done {
        Ok(()) => Some(dst),
        Err(_) => {
            let _ = tokio::fs::remove_file(&dst).await;
            None
        }
    }
}

/// `state_write`: data frames into a new file at `path`, then mode and mtime.
pub async fn write<R: AsyncRead + Unpin>(args: Value, r: &mut R) -> Output {
    let a: WriteArgs = match rpc::parse(args) {
        Ok(a) => a,
        Err(e) => return Output::error(e),
    };
    match write_file(&a, r).await {
        Ok(n) => Output::text(format!("wrote {n} bytes")),
        Err(e) => Output::error(e),
    }
}

/// `state_apply`: creates directories and symlinks, then sets directory modes.
pub async fn apply(args: Value) -> Result<String, String> {
    let mut a: ApplyArgs = rpc::parse(args)?;
    tokio::task::spawn_blocking(move || {
        for d in &a.dirs {
            fs::create_dir_all(&d.path).map_err(|e| format!("mkdir {}: {e}", d.path.display()))?;
            sys::set_mode(&d.path, d.mode)
                .map_err(|e| format!("chmod {}: {e}", d.path.display()))?;
        }
        for l in &a.symlinks {
            if let Some(parent) = l.path.parent() {
                fs::create_dir_all(parent)
                    .map_err(|e| format!("mkdir {}: {e}", parent.display()))?;
            }
            let _ = fs::remove_file(&l.path);
            sys::symlink(Path::new(&l.target), &l.path)
                .map_err(|e| format!("symlink {}: {e}", l.path.display()))?;
        }
        a.modes
            .sort_by_key(|m| std::cmp::Reverse(m.path.components().count()));
        for m in &a.modes {
            sys::set_mode(&m.path, m.mode)
                .map_err(|e| format!("chmod {}: {e}", m.path.display()))?;
        }
        Ok(format!(
            "{} dirs, {} symlinks, {} modes",
            a.dirs.len(),
            a.symlinks.len(),
            a.modes.len()
        ))
    })
    .await
    .map_err(|e| format!("apply task: {e}"))?
}

/// `state_swap`: replaces each top-level entry of `root` by its staged copy with one rename,
/// removes entries absent from the snapshot except `keep`, then deletes the staging dir.
pub async fn swap(args: Value) -> Result<String, String> {
    let a: SwapArgs = rpc::parse(args)?;
    tokio::task::spawn_blocking(move || swap_blocking(&a))
        .await
        .map_err(|e| format!("swap task: {e}"))?
}

fn swap_blocking(a: &SwapArgs) -> Result<String, String> {
    let trash = a.root.join(format!("{TRASH_PREFIX}{}", std::process::id()));
    fs::create_dir_all(&trash).map_err(|e| format!("mkdir {}: {e}", trash.display()))?;
    let staging_name = a
        .staging
        .file_name()
        .map(|n| n.to_string_lossy().into_owned());
    let trash_name = trash.file_name().map(|n| n.to_string_lossy().into_owned());
    let current = fs::read_dir(&a.root).map_err(|e| format!("list {}: {e}", a.root.display()))?;
    let mut removed = 0;
    for entry in current.flatten() {
        let name = entry.file_name().to_string_lossy().into_owned();
        let special = Some(&name) == staging_name.as_ref() || Some(&name) == trash_name.as_ref();
        if special || a.keep.contains(&name) || a.names.contains(&name) {
            continue;
        }
        fs::rename(entry.path(), trash.join(&name))
            .map_err(|e| format!("move aside {name}: {e}"))?;
        removed += 1;
    }
    for name in &a.names {
        let target = a.root.join(name);
        if fs::symlink_metadata(&target).is_ok() {
            fs::rename(&target, trash.join(name)).map_err(|e| format!("move aside {name}: {e}"))?;
        }
        fs::rename(a.staging.join(name), &target).map_err(|e| format!("swap in {name}: {e}"))?;
    }
    remove_tree(&trash).map_err(|e| format!("remove {}: {e}", trash.display()))?;
    let _ = remove_tree(&a.staging);
    Ok(format!(
        "swapped {} entries, removed {removed}",
        a.names.len()
    ))
}

/// Removes the tree at `path`; directories without owner write, such as a Go module cache, get it first.
fn remove_tree(path: &Path) -> std::io::Result<()> {
    match fs::remove_dir_all(path) {
        Err(e) if e.kind() == std::io::ErrorKind::PermissionDenied => {
            open_up(path);
            fs::remove_dir_all(path)
        }
        other => other,
    }
}

fn open_up(dir: &Path) {
    let Ok(meta) = fs::symlink_metadata(dir) else {
        return;
    };
    if !meta.is_dir() {
        return;
    }
    let mode = sys::mode(&meta);
    if mode & 0o700 != 0o700 {
        let _ = sys::set_mode(dir, mode | 0o700);
    }
    if let Ok(entries) = fs::read_dir(dir) {
        for entry in entries.flatten() {
            open_up(&entry.path());
        }
    }
}

/// `state_place`: renames `staging` to `dir/name`, or `name-2`, `name-3`, ... when that exists,
/// and returns the name it took; it never replaces an existing entry.
pub async fn place(args: Value) -> Result<String, String> {
    let a: PlaceArgs = rpc::parse(args)?;
    tokio::task::spawn_blocking(move || {
        for n in 1..=PLACE_TRIES {
            let name = if n == 1 {
                a.name.clone()
            } else {
                format!("{}-{n}", a.name)
            };
            let target = a.dir.join(&name);
            match fs::create_dir(&target) {
                // rename(2) replaces only an empty directory, so the one just created claims the name
                Ok(()) => {
                    return fs::rename(&a.staging, &target).map(|()| name).map_err(|e| {
                        let _ = fs::remove_dir(&target);
                        format!(
                            "rename {} to {}: {e}",
                            a.staging.display(),
                            target.display()
                        )
                    });
                }
                Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {}
                Err(e) => return Err(format!("mkdir {}: {e}", target.display())),
            }
        }
        Err(format!("{} and {PLACE_TRIES} variants exist", a.name))
    })
    .await
    .map_err(|e| format!("place task: {e}"))?
}

async fn write_file<R: AsyncRead + Unpin>(a: &WriteArgs, r: &mut R) -> Result<u64, String> {
    if let Some(parent) = a.path.parent() {
        tokio::fs::create_dir_all(parent)
            .await
            .map_err(|e| format!("mkdir {}: {e}", parent.display()))?;
    }
    let mut file = tokio::fs::File::create(&a.path)
        .await
        .map_err(|e| format!("create {}: {e}", a.path.display()))?;
    let mut n = 0u64;
    while let Some(data) = rpc::read_data(r).await? {
        file.write_all(&data)
            .await
            .map_err(|e| format!("write {}: {e}", a.path.display()))?;
        n += data.len() as u64;
    }
    file.flush()
        .await
        .map_err(|e| format!("flush {}: {e}", a.path.display()))?;
    let file = file.into_std().await;
    sys::set_file_mode(&file, a.mode).map_err(|e| format!("chmod {}: {e}", a.path.display()))?;
    let mtime = UNIX_EPOCH + Duration::from_millis(u64::try_from(a.mtime_ms).unwrap_or(0));
    file.set_times(FileTimes::new().set_modified(mtime))
        .map_err(|e| format!("set mtime {}: {e}", a.path.display()))?;
    Ok(n)
}

/// The lines of `file` that name a path strictly under `root`, relative to it; a missing file lists nothing.
fn listed_paths(file: &Path, root: &Path) -> HashSet<String> {
    let Ok(text) = fs::read_to_string(file) else {
        return HashSet::new();
    };
    text.lines()
        .filter_map(|l| Path::new(l.trim_end_matches('/')).strip_prefix(root).ok())
        .filter(|rel| {
            rel.components().next().is_some()
                && rel
                    .components()
                    .all(|c| matches!(c, std::path::Component::Normal(_)))
        })
        .filter_map(|rel| rel.to_str().map(str::to_owned))
        .collect()
}

fn walk_root(root: &Root, excludes: &Excludes, skip: &HashSet<String>, out: &mut Vec<Entry>) {
    let mut stack = vec![String::new()];
    while let Some(rel) = stack.pop() {
        let dir = if rel.is_empty() {
            root.path.clone()
        } else {
            root.path.join(&rel)
        };
        let Ok(read) = fs::read_dir(&dir) else {
            continue;
        };
        let mut children: Vec<_> = read.flatten().collect();
        children.sort_by_key(fs::DirEntry::file_name);
        for child in children.into_iter().rev() {
            let name = child.file_name().to_string_lossy().into_owned();
            let path = if rel.is_empty() {
                name
            } else {
                format!("{rel}/{name}")
            };
            if excludes.matches(&path)
                || root.excluded.iter().any(|p| under(&path, p))
                || skip.contains(&path)
            {
                continue;
            }
            let Ok(meta) = fs::symlink_metadata(child.path()) else {
                continue;
            };
            let ft = meta.file_type();
            let (kind, target) = if ft.is_symlink() {
                let target = fs::read_link(child.path())
                    .ok()
                    .map(|t| t.to_string_lossy().into_owned());
                (Kind::Symlink, target)
            } else if ft.is_dir() {
                stack.push(path.clone());
                (Kind::Dir, None)
            } else if ft.is_file() {
                (Kind::File, None)
            } else {
                continue;
            };
            out.push(Entry {
                root: root.name.clone(),
                path,
                kind,
                mode: sys::mode(&meta),
                mtime_ms: mtime_ms(meta.modified().ok()),
                size: if kind == Kind::File { meta.len() } else { 0 },
                target,
                ctime_ns: (kind == Kind::File).then(|| sys::ctime_ns(&meta)),
            });
        }
    }
    out.sort_by(|a, b| (&a.root, &a.path).cmp(&(&b.root, &b.path)));
}

fn stamp_of(path: &Path) -> Stamp {
    match fs::metadata(path) {
        Ok(m) => Stamp {
            size: m.len(),
            mtime_ms: mtime_ms(m.modified().ok()),
            error: None,
            backup: false,
        },
        Err(e) => Stamp {
            error: Some(format!("stat {}: {e}", path.display())),
            ..Stamp::default()
        },
    }
}

/// Whether the relative path `rel` is `prefix` or lies under it.
fn under(rel: &str, prefix: &str) -> bool {
    rel.strip_prefix(prefix)
        .is_some_and(|r| r.is_empty() || r.starts_with('/'))
}

fn mtime_ms(t: Option<SystemTime>) -> i64 {
    t.and_then(|t| t.duration_since(UNIX_EPOCH).ok())
        .map_or(0, |d| i64::try_from(d.as_millis()).unwrap_or(i64::MAX))
}

#[cfg(all(test, unix))]
mod tests {
    use std::os::unix::fs::symlink;

    use super::*;

    fn excludes() -> Excludes {
        Excludes {
            prefixes: vec![".cache".into()],
            names: vec!["Code Cache".into()],
            name_prefixes: vec!["Singleton".into()],
            name_suffixes: vec!["-shm".into()],
        }
    }

    #[test]
    fn excludes_match_prefixes_components_and_name_prefixes() {
        let e = excludes();
        assert!(e.matches(".cache"));
        assert!(e.matches(".cache/fontconfig/x"));
        assert!(!e.matches(".cachex"));
        assert!(e.matches(".config/chrome/Default/Code Cache/js/1"));
        assert!(e.matches(".config/chrome/SingletonLock"));
        assert!(!e.matches(".config/chrome/Default/Cookies"));
        assert!(e.matches(".config/chrome/Default/Cookies-shm"));
    }

    #[tokio::test]
    async fn walk_lists_files_dirs_and_symlinks_without_excluded_paths() {
        let dir = tempfile::tempdir().unwrap();
        let home = dir.path().join("home");
        fs::create_dir_all(home.join(".cache/x")).unwrap();
        fs::create_dir_all(home.join("docs/Code Cache")).unwrap();
        fs::write(home.join(".cache/x/big"), b"cache").unwrap();
        fs::write(home.join("docs/a.txt"), b"hello").unwrap();
        fs::write(home.join("docs/Code Cache/c"), b"c").unwrap();
        symlink("a.txt", home.join("docs/link")).unwrap();
        let args = serde_json::json!({
            "roots": [{"name": "home", "path": home}],
            "excludes": excludes(),
        });
        let entries: Vec<Entry> = serde_json::from_str(&walk(args).await.unwrap()).unwrap();
        let paths: Vec<(&str, Kind)> = entries.iter().map(|e| (e.path.as_str(), e.kind)).collect();
        assert_eq!(
            paths,
            [
                ("docs", Kind::Dir),
                ("docs/a.txt", Kind::File),
                ("docs/link", Kind::Symlink)
            ]
        );
        assert_eq!(entries[1].size, 5);
        assert_eq!(entries[2].target.as_deref(), Some("a.txt"));
        assert!(entries[1].ctime_ns.is_some());
        assert!(entries[0].ctime_ns.is_none() && entries[2].ctime_ns.is_none());
    }

    #[tokio::test]
    async fn a_root_excludes_its_own_prefixes_and_no_other_root_does() {
        let dir = tempfile::tempdir().unwrap();
        let (home, workspace) = (dir.path().join("home"), dir.path().join("workspace"));
        for root in [&home, &workspace] {
            fs::create_dir_all(root.join("Library/x")).unwrap();
            fs::write(root.join("Library/x/f"), b"f").unwrap();
        }
        fs::create_dir_all(home.join("Libraryx")).unwrap();
        let args = serde_json::json!({
            "roots": [
                {"name": "home", "path": home, "excluded": ["Library"]},
                {"name": "workspace", "path": workspace},
            ],
        });
        let entries: Vec<Entry> = serde_json::from_str(&walk(args).await.unwrap()).unwrap();
        let paths: Vec<(&str, &str)> = entries
            .iter()
            .map(|e| (e.root.as_str(), e.path.as_str()))
            .collect();
        assert_eq!(
            paths,
            [
                ("home", "Libraryx"),
                ("workspace", "Library"),
                ("workspace", "Library/x"),
                ("workspace", "Library/x/f")
            ]
        );
    }

    #[tokio::test]
    async fn swap_replaces_top_level_entries_and_keeps_listed_ones() {
        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().join("root");
        let staging = root.join(".cb-restore");
        fs::create_dir_all(root.join("old")).unwrap();
        fs::create_dir_all(root.join("docs")).unwrap();
        fs::create_dir_all(root.join(".cache")).unwrap();
        fs::write(root.join("docs/stale"), b"s").unwrap();
        fs::create_dir_all(staging.join("docs")).unwrap();
        fs::write(staging.join("docs/new"), b"n").unwrap();
        fs::write(staging.join("f"), b"f").unwrap();
        let args = serde_json::json!({
            "root": root, "staging": staging, "names": ["docs", "f"], "keep": [".cache"],
        });
        swap(args).await.unwrap();
        let mut names: Vec<String> = fs::read_dir(&root)
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .collect();
        names.sort();
        assert_eq!(names, [".cache", "docs", "f"]);
        assert!(root.join("docs/new").exists());
        assert!(!root.join("docs/stale").exists());
    }

    #[tokio::test]
    async fn swap_removes_a_replaced_tree_that_holds_read_only_dirs() {
        use std::os::unix::fs::PermissionsExt;

        let dir = tempfile::tempdir().unwrap();
        let root = dir.path().join("root");
        let staging = root.join(".cb-restore");
        let module = root.join("go/pkg/mod/example.com/m@v1.0.0");
        fs::create_dir_all(&module).unwrap();
        fs::write(module.join("go.mod"), b"module m").unwrap();
        for d in [module.as_path(), module.parent().unwrap()] {
            fs::set_permissions(d, fs::Permissions::from_mode(0o555)).unwrap();
        }
        fs::create_dir_all(staging.join("go")).unwrap();
        fs::write(staging.join("go/new"), b"n").unwrap();
        let args =
            serde_json::json!({"root": root, "staging": staging, "names": ["go"], "keep": []});
        swap(args).await.unwrap();
        let names: Vec<String> = fs::read_dir(&root)
            .unwrap()
            .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
            .collect();
        assert_eq!(names, ["go"]);
        assert!(root.join("go/new").exists());
    }
}
