//! The state layer: snapshots of the user's home, /workspace and agent.db as encrypted chunks.

pub mod crypto;
pub mod manifest;

use std::collections::{BTreeSet, HashMap, HashSet, VecDeque};
use std::path::{Component, Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use bytes::{Bytes, BytesMut};
use fastcdc::v2020::FastCDC;
use futures_util::stream::{self, FuturesUnordered};
use futures_util::{Stream, StreamExt};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::{Mutex, OnceCell, OwnedRwLockWriteGuard, RwLock, Semaphore, mpsc};

use crate::store::Store;
use crate::toolhost::rpc::{self, Call, FileReader, FileWriter};
use crate::toolhost::state::{Entry, Excludes, Kind, Listed, Root, SQLITE_MAGIC};
use crate::{Preempt, new_id, pb};
use crypto::Keys;
use manifest::{Blob, FileRecord, Manifest};

pub const TRANSFERS: usize = 32;

const CHUNK_MIN: u32 = 256 << 10;
const CHUNK_AVG: u32 = 1 << 20;
const CHUNK_MAX: u32 = 4 << 20;
const MANIFEST_LIMIT: u64 = 256 << 20;
// restore memory: RESTORE_WRITES × (RESTORE_AHEAD + 1) plain chunks plus TRANSFERS × 3 in-flight bodies, each ≤ CHUNK_MAX (1152 MiB)
const RESTORE_WRITES: usize = 64;
const RESTORE_AHEAD: usize = 2;
const PLAN_READS: usize = 4;
const ATTEMPTS: u32 = 3;
const RETRY_BASE: Duration = Duration::from_millis(500);
const STORE_CONNECT_TIMEOUT: Duration = Duration::from_secs(10);
const STORE_READ_TIMEOUT: Duration = Duration::from_secs(180);
const PREEMPT_WAIT: Duration = Duration::from_secs(5);
const REUSE_AGE_MS: i64 = 1_000;
const STAGING_PREFIX: &str = ".cb-restore-";
const SQLITE_SIDECARS: [&str; 2] = ["-wal", "-journal"];

const MANIFEST_FILE: &str = "manifest";
const HOME_ROOT: &str = "home";
const WORKSPACE_ROOT: &str = "workspace";
const API_ROOTS: [(&str, &str); 2] = [(HOME_ROOT, "/home/user"), (WORKSPACE_ROOT, "/workspace")];
const RESTORED_DIR: &str = "Restored";
const KNOWN_FILE: &str = "known-chunks.json";

const EXCLUDED_PREFIXES: [&str; 2] = [".cache", ".local/share/Trash"];
#[cfg(not(any(target_os = "macos", windows)))]
const HOME_EXCLUDED: [&str; 0] = [];
// a live session holds Library and .Trash open, and TCC guards .Trash
#[cfg(target_os = "macos")]
const HOME_EXCLUDED: [&str; 2] = [".Trash", "Library"];
// a live session holds AppData open, with caches and the registry's own files
#[cfg(windows)]
const HOME_EXCLUDED: [&str; 1] = ["AppData"];
const EXCLUDED_NAMES: [&str; 17] = [
    "Cache",
    "Code Cache",
    "GPUCache",
    "GrShaderCache",
    "ShaderCache",
    "DawnCache",
    "DawnGraphiteCache",
    "DawnWebGPUCache",
    "CacheStorage",
    "ScriptCache",
    "Crashpad",
    "Crash Reports",
    "BrowserMetrics",
    "component_crx_cache",
    "extensions_crx_cache",
    "optimization_guide_model_store",
    "OptGuideOnDeviceModel",
];
#[cfg(not(windows))]
const EXCLUDED_NAME_PREFIXES: [&str; 4] = [
    "Singleton",
    STAGING_PREFIX,
    crate::toolhost::state::TRASH_PREFIX,
    ".org.chromium.",
];
// the registry hive and its logs, which a live session holds open
#[cfg(windows)]
const EXCLUDED_NAME_PREFIXES: [&str; 6] = [
    "Singleton",
    STAGING_PREFIX,
    crate::toolhost::state::TRASH_PREFIX,
    ".org.chromium.",
    "NTUSER.DAT",
    "ntuser.",
];
const EXCLUDED_NAME_SUFFIXES: [&str; 1] = ["-shm"];

/// What a quiesce pauses: the gate that every turn and MCP tool call holds shared, and the share calls it may interrupt.
pub struct Pause {
    pub turns: Arc<RwLock<()>>,
    pub guests: Arc<Preempt>,
    /// How long a quiesce waits before it interrupts the share calls.
    pub wait: Duration,
}

/// Why a state operation failed; `Busy` means a turn or an owner's MCP call kept running past the quiesce wait.
#[derive(Debug)]
pub enum StateError {
    Busy(String),
    Invalid(String),
    Failed(String),
}

impl From<String> for StateError {
    fn from(e: String) -> Self {
        Self::Failed(e)
    }
}

/// Snapshots, uploads and restores user state; one operation runs at a time.
pub struct StateEngine {
    toolhost: rpc::Client,
    store: Store,
    roots: Vec<Root>,
    excludes: Excludes,
    work: PathBuf,
    pause: Pause,
    desktop_unit: Option<&'static str>,
    save_required: bool,
    http: reqwest::Client,
    pinned: Mutex<Option<Pinned>>,
}

impl StateEngine {
    /// Creates an engine over `roots` that works under `work`, clearing what a killed operation left there; `pause` quiesces the loop and MCP, and Restore stops `desktop_unit` around the swap.
    pub async fn new(
        toolhost: rpc::Client,
        store: Store,
        roots: Vec<Root>,
        work: PathBuf,
        pause: Pause,
        desktop_unit: Option<&'static str>,
        save_required: bool,
    ) -> Result<Self, String> {
        if let Ok(mut dir) = tokio::fs::read_dir(&work).await {
            while let Ok(Some(e)) = dir.next_entry().await {
                if e.file_name() == KNOWN_FILE {
                    continue;
                }
                let _ = match e.file_type().await {
                    Ok(t) if t.is_dir() => tokio::fs::remove_dir_all(e.path()).await,
                    _ => tokio::fs::remove_file(e.path()).await,
                };
            }
        }
        Ok(Self {
            toolhost,
            store,
            roots,
            excludes: excludes(),
            work,
            pause,
            desktop_unit,
            save_required,
            http: store_client(STORE_CONNECT_TIMEOUT, STORE_READ_TIMEOUT)?,
            pinned: Mutex::new(None),
        })
    }

    /// Walks and chunks the state, spools new chunks and pins the snapshot for Upload.
    pub async fn plan(
        self: &Arc<Self>,
        req: pb::PlanRequest,
    ) -> Result<pb::PlanResponse, StateError> {
        let engine = Arc::clone(self);
        tokio::spawn(async move { engine.plan_inner(req).await })
            .await
            .map_err(|e| StateError::Failed(format!("plan task: {e}")))?
    }

    /// Uploads the listed chunks of the pinned snapshot; the call with the manifest writes it last and unpins, so an upload may span calls.
    pub async fn upload(
        &self,
        req: pb::UploadRequest,
        progress: mpsc::Sender<pb::TransferProgress>,
    ) -> Result<(), StateError> {
        let mut pinned = self.pinned.lock().await;
        let snap = pinned
            .as_ref()
            .filter(|p| p.snapshot_id == req.snapshot_id)
            .ok_or_else(|| {
                StateError::Invalid(format!(
                    "snapshot {} is not pinned; plan again",
                    req.snapshot_id
                ))
            })?;
        let mut jobs = Vec::with_capacity(req.puts.len() + 1);
        for put in req.puts {
            if !snap.known.chunks.contains(&put.chunk_id) {
                return Err(StateError::Invalid(format!(
                    "chunk {} is not part of snapshot {}",
                    put.chunk_id, snap.snapshot_id
                )));
            }
            let path = snap.spool.join(&put.chunk_id);
            jobs.push((put, path));
        }
        let has_manifest = req.manifest.is_some();
        if let Some(manifest) = req.manifest {
            jobs.push((manifest, snap.spool.join(MANIFEST_FILE)));
        }
        let mut total = 0;
        for (put, path) in &jobs {
            total += match tokio::fs::metadata(path).await {
                Ok(m) => m.len(),
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                    return Err(StateError::Invalid(format!(
                        "chunk {} was not spooled because the last upload held it; plan again without a parent",
                        put.chunk_id
                    )));
                }
                Err(e) => return Err(format!("stat {}: {e}", path.display()).into()),
            };
        }
        let mut meter = Meter::new(total, jobs.len(), progress);
        let manifest_job = if has_manifest { jobs.pop() } else { None };
        let mut puts = stream::iter(jobs.into_iter().map(|(put, path)| self.put(put, path)))
            .buffer_unordered(TRANSFERS);
        while let Some(done) = puts.next().await {
            meter.add(done?).await;
        }
        let Some((put, path)) = manifest_job else {
            meter.send().await;
            return Ok(());
        };
        meter.add(self.put(put, path).await?).await;
        self.save_known(&snap.known).await?;
        let _ = tokio::fs::remove_dir_all(&snap.spool).await;
        *pinned = None;
        meter.finish(&req.snapshot_id).await;
        Ok(())
    }

    /// Replaces the user state with the manifest's; chunk URLs arrive in pages and are fetched as they come, and the loop's writers pause for the swap.
    pub async fn restore<S>(
        &self,
        mut reqs: S,
        progress: mpsc::Sender<pb::TransferProgress>,
    ) -> Result<(), StateError>
    where
        S: Stream<Item = Result<pb::RestoreRequest, String>> + Unpin,
    {
        let _one = self.pinned.lock().await;
        let opened = self.open(&mut reqs).await?;
        let cache = self.cache().await?;
        let result = self.restore_from(opened, reqs, &cache, progress).await;
        let _ = tokio::fs::remove_dir_all(&cache).await;
        result
    }

    /// Copies the scope's paths of the manifest into a new folder under `~/Restored`, staged then renamed into place; nothing else is touched.
    pub async fn restore_paths<S>(
        &self,
        mut reqs: S,
        progress: mpsc::Sender<pb::TransferProgress>,
    ) -> Result<(), StateError>
    where
        S: Stream<Item = Result<pb::RestoreRequest, String>> + Unpin,
    {
        let _one = self.pinned.lock().await;
        let opened = self.open(&mut reqs).await?;
        let scope = opened
            .scope
            .as_ref()
            .ok_or_else(|| StateError::Invalid("missing scope".to_owned()))?;
        let records = select(&self.roots, &opened.manifest, scope)?;
        let home = self
            .roots
            .iter()
            .find(|r| r.name == HOME_ROOT)
            .ok_or_else(|| StateError::Invalid("no home root".to_owned()))?;
        let restored = home.path.join(RESTORED_DIR);
        let staging = restored.join(format!("{STAGING_PREFIX}{}", uuid::Uuid::new_v4().simple()));
        let mut targets = Vec::new();
        let (first, last) = stage(
            records.into_iter().map(|(root, r)| {
                (
                    staging.join(relative(api_path(root))).join(&r.entry.path),
                    r,
                )
            }),
            &staging,
            &mut targets,
        );
        self.tool("state_apply", first).await?;
        let cache = self.cache().await?;
        let filled = self
            .fill(&opened.keys, targets, opened.gets, reqs, &cache, progress)
            .await;
        let _ = tokio::fs::remove_dir_all(&cache).await;
        let mut meter = filled?.meter;
        self.tool("state_apply", last).await?;
        meter.msg.target = self
            .tool(
                "state_place",
                json!({"staging": staging, "dir": restored, "name": scope.target}),
            )
            .await?;
        meter.finish(&opened.manifest_id).await;
        Ok(())
    }

    async fn plan_inner(&self, req: pb::PlanRequest) -> Result<pb::PlanResponse, StateError> {
        let mut pinned = self.pinned.lock().await;
        let key = req
            .key
            .ok_or_else(|| StateError::Invalid("missing key".to_owned()))?;
        let keys = Arc::new(Keys::derive(&key.key).map_err(StateError::Invalid)?);
        if let Some(old) = pinned.take() {
            let _ = tokio::fs::remove_dir_all(&old.spool).await;
        }
        let quiesced = req.consistency == i32::from(pb::Consistency::Quiesced);
        let quiet = if quiesced {
            Some(self.quiesce().await?)
        } else {
            None
        };
        let result = self
            .snapshot(&keys, &key.key_id, &req.parent, quiesced)
            .await;
        if let Some(resume) = quiet.and_then(|q| self.resume(q, false)) {
            resume
                .await
                .map_err(|e| StateError::Failed(format!("resume task: {e}")))?;
        }
        let (snapshot, response) = result?;
        *pinned = Some(snapshot);
        Ok(response)
    }

    /// Reads the first message of a restore stream and opens the manifest it names.
    async fn open<S>(&self, reqs: &mut S) -> Result<Opened, StateError>
    where
        S: Stream<Item = Result<pb::RestoreRequest, String>> + Unpin,
    {
        let first = reqs
            .next()
            .await
            .transpose()?
            .ok_or_else(|| StateError::Invalid("empty restore stream".to_owned()))?;
        let key = first
            .key
            .ok_or_else(|| StateError::Invalid("missing key".to_owned()))?;
        let keys = Arc::new(Keys::derive(&key.key).map_err(StateError::Invalid)?);
        let get = first
            .manifest
            .ok_or_else(|| StateError::Invalid("missing manifest url".to_owned()))?;
        let sealed = get_retrying(&self.http, &get.url).await?;
        let plain = keys.open(&get.chunk_id, &sealed, MANIFEST_LIMIT)?;
        let manifest: Manifest =
            serde_json::from_slice(&plain).map_err(|e| format!("decode manifest: {e}"))?;
        if manifest.version != manifest::VERSION {
            return Err(StateError::Invalid(format!(
                "manifest version {} is not supported",
                manifest.version
            )));
        }
        Ok(Opened {
            keys,
            manifest,
            manifest_id: get.chunk_id,
            gets: first.gets,
            scope: first.scope,
        })
    }

    async fn cache(&self) -> Result<PathBuf, String> {
        let cache = self.work.join(new_id("restore"));
        tokio::fs::create_dir_all(&cache)
            .await
            .map_err(|e| format!("create {}: {e}", cache.display()))?;
        Ok(cache)
    }

    async fn restore_from<S>(
        &self,
        opened: Opened,
        reqs: S,
        cache: &Path,
        progress: mpsc::Sender<pb::TransferProgress>,
    ) -> Result<(), StateError>
    where
        S: Stream<Item = Result<pb::RestoreRequest, String>> + Unpin,
    {
        let Opened {
            keys,
            manifest,
            manifest_id,
            gets: queue,
            ..
        } = opened;
        let mut targets = Vec::new();
        let mut finals = Vec::with_capacity(self.roots.len());
        let mut swaps = Vec::with_capacity(self.roots.len());
        for root in &self.roots {
            let records: Vec<&FileRecord> = manifest
                .files
                .iter()
                .filter(|f| f.entry.root == root.name)
                .collect();
            let staging = root
                .path
                .join(format!("{STAGING_PREFIX}{}", uuid::Uuid::new_v4().simple()));
            let (first, last) = stage(
                records.iter().map(|r| (staging.join(&r.entry.path), *r)),
                &staging,
                &mut targets,
            );
            self.tool("state_apply", first).await?;
            finals.push(last);
            let names: BTreeSet<&str> = records
                .iter()
                .filter_map(|r| r.entry.path.split('/').next())
                .collect();
            let skipped = format!("{}/", root.name);
            let keep: Vec<&str> = self
                .excludes
                .prefixes
                .iter()
                .chain(&root.excluded)
                .map(String::as_str)
                .chain(
                    manifest
                        .skipped
                        .iter()
                        .filter_map(|k| k.strip_prefix(skipped.as_str())),
                )
                .filter(|p| !p.contains('/'))
                .collect();
            swaps
                .push(json!({"root": root.path, "staging": staging, "names": names, "keep": keep, "excludes": self.excludes}));
        }
        if let Some(db) = &manifest.agent_db {
            targets.push(Target {
                dest: Dest::Local(cache.join("agent.db")),
                chunks: &db.chunks,
            });
        }
        let mut filled = self
            .fill(&keys, targets, queue, reqs, cache, progress)
            .await?;
        for last in finals {
            self.tool("state_apply", last).await?;
        }
        let quiet = self.quiesce().await?;
        let result = self.swap_in(&manifest, cache, swaps).await;
        let _ = self.resume(quiet, result.is_ok());
        result?;
        if let Err(e) = self.seed_known(&manifest, &filled).await {
            eprintln!("state: seed the reuse cache: {e}");
        }
        filled.meter.finish(&manifest_id).await;
        Ok(())
    }

    /// Writes every target as its chunk urls arrive, fetching each chunk once (a reused chunk goes through `cache`); fails if a chunk never got a url.
    async fn fill<S>(
        &self,
        keys: &Arc<Keys>,
        targets: Vec<Target<'_>>,
        mut queue: Vec<pb::PresignedGet>,
        mut reqs: S,
        cache: &Path,
        progress: mpsc::Sender<pb::TransferProgress>,
    ) -> Result<Filled, StateError>
    where
        S: Stream<Item = Result<pb::RestoreRequest, String>> + Unpin,
    {
        let mut uses: HashMap<&str, usize> = HashMap::new();
        let mut waiting: HashMap<&str, Vec<usize>> = HashMap::new();
        let mut lacking = Vec::with_capacity(targets.len());
        let mut ready = VecDeque::new();
        for (i, t) in targets.iter().enumerate() {
            let distinct: HashSet<&str> = t.chunks.iter().map(String::as_str).collect();
            for id in t.chunks {
                *uses.entry(id).or_default() += 1;
            }
            for id in &distinct {
                waiting.entry(*id).or_default().push(i);
            }
            if distinct.is_empty() {
                ready.push_back(i);
            }
            lacking.push(distinct.len());
        }
        let shared: HashMap<&str, Arc<OnceCell<()>>> = uses
            .iter()
            .filter(|(_, n)| **n > 1)
            .map(|(id, _)| (*id, Arc::default()))
            .collect();
        let filled = Mutex::new(Filled {
            meter: Meter::new(0, uses.len(), progress),
            sizes: HashMap::new(),
            sqlite: HashSet::new(),
        });
        let mut targets: Vec<Option<Target>> = targets.into_iter().map(Some).collect();
        let mut urls: HashMap<String, String> = HashMap::new();
        let gets = Semaphore::new(TRANSFERS);
        let mut jobs = FuturesUnordered::new();
        let mut open = true;
        loop {
            for g in queue.drain(..) {
                let Some(ts) = waiting.remove(g.chunk_id.as_str()) else {
                    continue;
                };
                for t in ts {
                    lacking[t] -= 1;
                    if lacking[t] == 0 {
                        ready.push_back(t);
                    }
                }
                urls.insert(g.chunk_id, g.url);
            }
            while jobs.len() < RESTORE_WRITES
                && let Some(t) = ready.pop_front().and_then(|i| targets[i].take())
            {
                let parts: Vec<Part> = t
                    .chunks
                    .iter()
                    .map(|id| match shared.get(id.as_str()) {
                        Some(cell) => Part::Shared {
                            id: id.clone(),
                            url: urls.get(id).cloned().unwrap_or_default(),
                            cell: Arc::clone(cell),
                        },
                        None => Part::Once {
                            id: id.clone(),
                            url: urls.remove(id).unwrap_or_default(),
                        },
                    })
                    .collect();
                jobs.push(self.write_target(keys, t.dest, parts, cache, &gets, &filled));
            }
            if !open && ready.is_empty() && jobs.is_empty() {
                break;
            }
            tokio::select! {
                page = reqs.next(), if open && ready.is_empty() => {
                    match page.transpose()? {
                        Some(req) => queue = req.gets,
                        None => open = false,
                    }
                }
                Some(done) = jobs.next(), if !jobs.is_empty() => done?,
            }
        }
        if let Some(missing) = waiting.keys().min() {
            return Err(StateError::Invalid(format!("no url for chunk {missing}")));
        }
        drop(jobs);
        Ok(filled.into_inner())
    }

    async fn write_target(
        &self,
        keys: &Arc<Keys>,
        dest: Dest<'_>,
        parts: Vec<Part>,
        cache: &Path,
        gets: &Semaphore,
        filled: &Mutex<Filled>,
    ) -> Result<(), String> {
        let mut entry = match &dest {
            Dest::Staged(_, e) => Some(*e),
            Dest::Local(_) => None,
        };
        let mut sink = match dest {
            Dest::Staged(path, e) => {
                Sink::Remote(self.toolhost.write_file(&path, e.mode, e.mtime_ms).await?)
            }
            Dest::Local(path) => Sink::Local(
                tokio::fs::File::create(&path)
                    .await
                    .map_err(|e| format!("create {}: {e}", path.display()))?,
            ),
        };
        let mut data = stream::iter(parts)
            .map(|p| self.part(keys, p, cache, gets, filled))
            .buffered(RESTORE_AHEAD);
        while let Some(d) = data.next().await {
            let d = d?;
            if let Some(e) = entry.take()
                && d.starts_with(SQLITE_MAGIC)
            {
                filled
                    .lock()
                    .await
                    .sqlite
                    .insert(format!("{}/{}", e.root, e.path));
            }
            sink.send(&d).await?;
        }
        sink.finish().await
    }

    async fn part(
        &self,
        keys: &Arc<Keys>,
        part: Part,
        cache: &Path,
        gets: &Semaphore,
        filled: &Mutex<Filled>,
    ) -> Result<Vec<u8>, String> {
        match part {
            Part::Once { id, url } => self.fetch_chunk(keys, &id, &url, gets, filled).await,
            Part::Shared { id, url, cell } => {
                let mut first = None;
                cell.get_or_try_init(|| async {
                    let plain = self.fetch_chunk(keys, &id, &url, gets, filled).await?;
                    let path = cache.join(&id);
                    tokio::fs::write(&path, &plain)
                        .await
                        .map_err(|e| format!("write {}: {e}", path.display()))?;
                    first = Some(plain);
                    Ok::<_, String>(())
                })
                .await?;
                match first {
                    Some(plain) => Ok(plain),
                    None => tokio::fs::read(cache.join(&id))
                        .await
                        .map_err(|e| format!("read cached chunk {id}: {e}")),
                }
            }
        }
    }

    /// Stops the desktop session, swaps the staged roots in and restores agent.db, then starts it.
    async fn swap_in(
        &self,
        manifest: &Manifest,
        cache: &Path,
        swaps: Vec<Value>,
    ) -> Result<(), StateError> {
        self.desktop("stop").await?;
        let swapped = self.swap_staged(manifest, cache, swaps).await;
        let started = self.desktop("start").await;
        swapped?;
        started.map_err(StateError::Failed)
    }

    async fn swap_staged(
        &self,
        manifest: &Manifest,
        cache: &Path,
        swaps: Vec<Value>,
    ) -> Result<(), StateError> {
        for swap in swaps {
            self.tool("state_swap", swap).await?;
        }
        if manifest.agent_db.is_some() {
            self.store.restore_from(cache.join("agent.db")).await?;
        }
        Ok(())
    }

    /// Stops or starts the desktop session unit (the contract grants cbagent exactly this).
    async fn desktop(&self, verb: &str) -> Result<(), String> {
        let Some(unit) = self.desktop_unit else {
            return Ok(());
        };
        let status = tokio::process::Command::new("systemctl")
            .args([verb, unit])
            .status()
            .await
            .map_err(|e| format!("systemctl {verb} {unit}: {e}"))?;
        if status.success() {
            Ok(())
        } else {
            Err(format!("systemctl {verb} {unit}: {status}"))
        }
    }

    async fn snapshot(
        &self,
        keys: &Arc<Keys>,
        key_id: &str,
        parent: &str,
        quiesced: bool,
    ) -> Result<(Pinned, pb::PlanResponse), StateError> {
        let spool = self.work.join(new_id("spool"));
        tokio::fs::create_dir_all(&spool)
            .await
            .map_err(|e| format!("create {}: {e}", spool.display()))?;
        let result = self
            .spool_snapshot(keys, key_id, parent, quiesced, spool.clone())
            .await;
        if result.is_err() {
            let _ = tokio::fs::remove_dir_all(&spool).await;
        }
        result
    }

    async fn spool_snapshot(
        &self,
        keys: &Arc<Keys>,
        key_id: &str,
        parent: &str,
        quiesced: bool,
        spool: PathBuf,
    ) -> Result<(Pinned, pb::PlanResponse), StateError> {
        let mut last = if parent.is_empty() {
            Known::default()
        } else {
            self.load_known(key_id).await
        };
        let chunker = Chunker {
            keys: Arc::clone(keys),
            spool: &spool,
            known: &last.chunks,
            sizes: std::sync::Mutex::new(HashMap::new()),
        };
        let settled_ns = (crate::now_ms() - REUSE_AGE_MS) * 1_000_000;
        let entries = self.walk().await?;
        let roots: HashMap<&str, &Path> = self
            .roots
            .iter()
            .map(|r| (r.name.as_str(), r.path.as_path()))
            .collect();
        let mut files = Vec::with_capacity(entries.len());
        let mut skipped = Vec::new();
        let mut backed_up: HashSet<String> = HashSet::new();
        let mut reuse = HashMap::new();
        let (sidecars, rest): (Vec<Entry>, Vec<Entry>) = entries.into_iter().partition(|e| {
            e.kind == Kind::File && SQLITE_SIDECARS.iter().any(|s| e.path.ends_with(s))
        });
        let baseline = !last.key_id.is_empty();
        let mut before: HashMap<String, Vec<String>> = HashMap::new();
        let mut reread = Vec::new();
        let mut prepare = |mut entry: Entry| {
            let key = format!("{}/{}", entry.root, entry.path);
            let ctime_ns = entry.ctime_ns.take();
            let step = match roots.get(entry.root.as_str()) {
                Some(root) if entry.kind == Kind::File => match last.files.remove(&key) {
                    Some(r) if r.matches(&entry, ctime_ns) => Step::Reuse(r),
                    prev => {
                        if let Some(r) = prev {
                            before.insert(
                                key.clone(),
                                r.chunks.into_iter().map(|(id, _)| id).collect(),
                            );
                        }
                        Step::Read(root.join(&entry.path))
                    }
                },
                _ => Step::Keep,
            };
            (entry, key, ctime_ns, step)
        };
        let chunker = &chunker;
        for pass in [rest, sidecars] {
            let pass: Vec<Entry> = pass
                .into_iter()
                .filter(|e| {
                    !SQLITE_SIDECARS.iter().any(|s| {
                        e.path
                            .strip_suffix(s)
                            .is_some_and(|base| backed_up.contains(&format!("{}/{base}", e.root)))
                    })
                })
                .collect();
            let mut steps = stream::iter(pass)
                .map(&mut prepare)
                .map(|(entry, key, ctime_ns, step)| async move {
                    let step = match step {
                        Step::Read(path) => {
                            Step::Chunked(chunker.remote(&self.toolhost, &path).await?)
                        }
                        other => other,
                    };
                    Ok::<_, String>((entry, key, ctime_ns, step))
                })
                .buffered(PLAN_READS);
            while let Some(done) = steps.next().await {
                let (entry, key, ctime_ns, step) = done?;
                match step {
                    Step::Keep => files.push(FileRecord {
                        entry,
                        chunks: Vec::new(),
                    }),
                    Step::Reuse(r) => {
                        chunker.record(&r.chunks);
                        let chunks = r.chunks.iter().map(|(id, _)| id.clone()).collect();
                        reuse.insert(key, r);
                        files.push(FileRecord { entry, chunks });
                    }
                    Step::Chunked(Some((chunks, backup))) => {
                        if backup {
                            backed_up.insert(key.clone());
                        }
                        let settled = ctime_ns.filter(|c| *c < settled_ns);
                        let r = Reuse {
                            size: entry.size,
                            mtime_ms: entry.mtime_ms,
                            ctime_ns: settled.unwrap_or_default(),
                            chunks: chunker.sized(&chunks),
                            compare_only: backup || settled.is_none(),
                        };
                        reread.push((key.clone(), files.len()));
                        reuse.insert(key, r);
                        files.push(FileRecord { entry, chunks });
                    }
                    Step::Chunked(None) => skipped.push(key),
                    Step::Read(_) => {}
                }
            }
        }
        let changed = reread
            .iter()
            .filter(|(key, i)| before.get(key) != Some(&files[*i].chunks))
            .count()
            + last.files.len();
        let db_copy = self
            .work
            .join(format!("agent-db-{}.db", uuid::Uuid::new_v4().simple()));
        self.store.backup_to(db_copy.clone()).await?;
        let db = chunker.local(&db_copy).await;
        let _ = tokio::fs::remove_file(&db_copy).await;
        let (db_size, db_chunks) = db?;
        let taken_at_ms = crate::now_ms();
        let manifest = Manifest {
            version: manifest::VERSION,
            parent: parent.to_owned(),
            key_id: key_id.to_owned(),
            taken_at_ms,
            consistency: if quiesced { "quiesced" } else { "live" }.to_owned(),
            files,
            agent_db: Some(Blob {
                size: db_size,
                chunks: db_chunks,
            }),
            skipped,
        };
        let plain = encode_manifest(&manifest)?;
        let snapshot_id = keys.id(&plain)?;
        let sealed = keys.seal(&snapshot_id, &plain)?;
        tokio::fs::write(spool.join(MANIFEST_FILE), sealed)
            .await
            .map_err(|e| format!("write manifest: {e}"))?;
        let ids = referenced(&manifest);
        let sizes = chunker.sized(&ids);
        let chunks = sizes
            .into_iter()
            .map(|(id, size)| pb::ChunkRef { id, size })
            .collect();
        let response = pb::PlanResponse {
            snapshot_id: snapshot_id.clone(),
            chunks,
            files: manifest
                .files
                .iter()
                .filter(|f| f.entry.kind == Kind::File)
                .count() as u64,
            logical_bytes: manifest.files.iter().map(|f| f.entry.size).sum::<u64>() + db_size,
            taken_at_ms,
            changed_files: baseline.then_some(changed as u64),
        };
        let pinned = Pinned {
            snapshot_id,
            spool,
            known: Known {
                key_id: key_id.to_owned(),
                chunks: ids.into_iter().collect(),
                files: reuse,
            },
        };
        Ok((pinned, response))
    }

    async fn walk(&self) -> Result<Vec<Entry>, String> {
        #[cfg(unix)]
        let listed = self
            .roots
            .iter()
            .find(|r| r.name == HOME_ROOT)
            .map(|home| Listed {
                file: home.path.join(crate::offload::Layout::excludes_rel()),
                root: WORKSPACE_ROOT.to_owned(),
            });
        #[cfg(windows)]
        let listed: Option<Listed> = None;
        let walked = self
            .toolhost
            .walk(json!({"roots": self.roots, "excludes": self.excludes, "listed": listed}))
            .await?;
        serde_json::from_slice(&walked).map_err(|e| format!("decode walk: {e}"))
    }

    async fn seed_known(&self, manifest: &Manifest, filled: &Filled) -> Result<(), String> {
        let restored: HashMap<String, &FileRecord> = manifest
            .files
            .iter()
            .filter(|f| f.entry.kind == Kind::File)
            .map(|f| (format!("{}/{}", f.entry.root, f.entry.path), f))
            .collect();
        let mut files = HashMap::new();
        for e in self.walk().await? {
            let key = format!("{}/{}", e.root, e.path);
            let (Some(r), Some(ctime_ns)) = (restored.get(&key), e.ctime_ns) else {
                continue;
            };
            let unchanged = e.size == r.entry.size && e.mtime_ms == r.entry.mtime_ms;
            let chunks = r
                .chunks
                .iter()
                .map(|id| (id.clone(), filled.sizes.get(id).copied().unwrap_or(0)))
                .collect();
            let compare_only = !unchanged || filled.sqlite.contains(&key);
            files.insert(
                key,
                Reuse {
                    size: e.size,
                    mtime_ms: e.mtime_ms,
                    ctime_ns,
                    chunks,
                    compare_only,
                },
            );
        }
        self.save_known(&Known {
            key_id: manifest.key_id.clone(),
            chunks: referenced(manifest).into_iter().collect(),
            files,
        })
        .await
    }

    /// Takes the gate; past the wait it interrupts the share calls, since a guest must never hold off the owner.
    async fn quiesce(&self) -> Result<Quiet, StateError> {
        let gate = Arc::clone(&self.pause.turns).write_owned();
        tokio::pin!(gate);
        let gate = match tokio::time::timeout(self.pause.wait, &mut gate).await {
            Ok(gate) => gate,
            Err(_) => {
                self.pause.guests.fire();
                tokio::time::timeout(PREEMPT_WAIT, gate)
                    .await
                    .map_err(|_| {
                        StateError::Busy("a turn is still running; retry later".to_owned())
                    })?
            }
        };
        if let Err(e) = self.tool("desktop_save", json!({})).await {
            if self.save_required {
                return Err(StateError::Failed(format!("save the session: {e}")));
            }
            eprintln!("state: save the desktop session: {e}");
        }
        let closed = self.tool("browser_close", json!({})).await?;
        Ok(Quiet {
            _gate: gate,
            chrome: closed == "closed",
        })
    }

    /// Ends the pause; Chrome starts in the background, and a browser tool waits on its lock meanwhile.
    fn resume(&self, quiet: Quiet, restored: bool) -> Option<tokio::task::JoinHandle<()>> {
        let args = chrome_start(quiet.chrome, restored)?;
        let toolhost = self.toolhost.clone();
        Some(tokio::spawn(async move {
            let call = Call {
                agent_id: "state".to_owned(),
                tool: "browser_start".to_owned(),
                args,
            };
            match toolhost.call(&call).await {
                Ok(out) if out.is_error => eprintln!("state: restart chrome: {}", out.text),
                Ok(_) => {}
                Err(e) => eprintln!("state: restart chrome: {e}"),
            }
        }))
    }

    async fn tool(&self, name: &str, args: Value) -> Result<String, String> {
        let call = Call {
            agent_id: "state".to_owned(),
            tool: name.to_owned(),
            args,
        };
        let out = self.toolhost.call(&call).await?;
        if out.is_error {
            Err(format!("{name}: {}", out.text))
        } else {
            Ok(out.text)
        }
    }

    async fn put(&self, put: pb::PresignedPut, path: PathBuf) -> Result<u64, String> {
        let body = Bytes::from(
            tokio::fs::read(&path)
                .await
                .map_err(|e| format!("read {}: {e}", path.display()))?,
        );
        let size = body.len() as u64;
        send_retrying(&format!("put {}", put.chunk_id), || {
            self.http.put(&put.url).body(body.clone())
        })
        .await?;
        Ok(size)
    }

    async fn fetch_chunk(
        &self,
        keys: &Arc<Keys>,
        id: &str,
        url: &str,
        gets: &Semaphore,
        filled: &Mutex<Filled>,
    ) -> Result<Vec<u8>, String> {
        let slot = gets
            .acquire()
            .await
            .map_err(|e| format!("transfer slot: {e}"))?;
        let sealed = get_retrying(&self.http, url)
            .await
            .map_err(|e| format!("chunk {id}: {e}"))?;
        let size = sealed.len() as u64;
        let keys = Arc::clone(keys);
        let owned = id.to_owned();
        let (owned, plain) = tokio::task::spawn_blocking(move || {
            keys.open(&owned, &sealed, u64::from(CHUNK_MAX))
                .map(|p| (owned, p))
        })
        .await
        .map_err(|e| format!("open task: {e}"))??;
        drop(slot);
        let mut f = filled.lock().await;
        f.meter.add(size).await;
        f.sizes.insert(owned, plain.len() as u64);
        Ok(plain)
    }

    async fn load_known(&self, key_id: &str) -> Known {
        let Ok(raw) = tokio::fs::read(self.work.join(KNOWN_FILE)).await else {
            return Known::default();
        };
        match serde_json::from_slice::<Known>(&raw) {
            Ok(k) if k.key_id == key_id => k,
            _ => Known::default(),
        }
    }

    async fn save_known(&self, known: &Known) -> Result<(), String> {
        let raw = serde_json::to_vec(known).map_err(|e| format!("encode known chunks: {e}"))?;
        tokio::fs::write(self.work.join(KNOWN_FILE), raw)
            .await
            .map_err(|e| format!("write known chunks: {e}"))
    }
}

/// The first message of a restore stream with its manifest opened.
struct Opened {
    keys: Arc<Keys>,
    manifest: Manifest,
    manifest_id: String,
    gets: Vec<pb::PresignedGet>,
    scope: Option<pb::RestoreScope>,
}

/// The snapshot waiting for Upload.
struct Pinned {
    snapshot_id: String,
    spool: PathBuf,
    known: Known,
}

struct Filled {
    meter: Meter,
    sizes: HashMap<String, u64>,
    sqlite: HashSet<String>,
}

/// Chunk ids the store held after the last upload under one key, and the files that can reuse theirs.
#[derive(Default, Deserialize, Serialize)]
struct Known {
    key_id: String,
    chunks: HashSet<String>,
    files: HashMap<String, Reuse>,
}

/// A file's stamp at the last upload and its chunks with their sizes.
#[derive(Deserialize, Serialize)]
struct Reuse {
    size: u64,
    mtime_ms: i64,
    ctime_ns: i64,
    chunks: Vec<(String, u64)>,
    /// Read through an SQLite backup or changed within `REUSE_AGE_MS` of the walk: compared for `changed_files`, never reused.
    compare_only: bool,
}

impl Reuse {
    /// Size, mtime and the kernel-set ctime all match, so no write happened since the last walk.
    fn matches(&self, entry: &Entry, ctime_ns: Option<i64>) -> bool {
        !self.compare_only
            && self.size == entry.size
            && self.mtime_ms == entry.mtime_ms
            && ctime_ns == Some(self.ctime_ns)
    }
}

/// Turns are paused and Chrome is closed while this lives.
struct Quiet {
    _gate: OwnedRwLockWriteGuard<()>,
    chrome: bool,
}

struct Target<'a> {
    dest: Dest<'a>,
    chunks: &'a [String],
}

/// Where a restored file goes: a staged path written as `user`, or a local file.
enum Dest<'a> {
    Staged(PathBuf, &'a Entry),
    Local(PathBuf),
}

/// One chunk of a target and the url it comes from; a shared chunk is fetched once.
enum Part {
    Once {
        id: String,
        url: String,
    },
    Shared {
        id: String,
        url: String,
        cell: Arc<OnceCell<()>>,
    },
}

enum Sink {
    Remote(FileWriter),
    Local(tokio::fs::File),
}

impl Sink {
    async fn send(&mut self, data: &[u8]) -> Result<(), String> {
        match self {
            Self::Remote(w) => w.send(data).await,
            Self::Local(f) => f.write_all(data).await.map_err(|e| format!("write: {e}")),
        }
    }

    async fn finish(self) -> Result<(), String> {
        match self {
            Self::Remote(w) => w.finish().await,
            Self::Local(mut f) => f.flush().await.map_err(|e| format!("flush: {e}")),
        }
    }
}

/// What the plan does with one walked entry.
enum Step {
    Keep,
    Reuse(Reuse),
    Read(PathBuf),
    Chunked(Option<(Vec<String>, bool)>),
}

/// Splits byte streams into content-defined chunks and spools the ones the store lacks.
struct Chunker<'a> {
    keys: Arc<Keys>,
    spool: &'a Path,
    known: &'a HashSet<String>,
    sizes: std::sync::Mutex<HashMap<String, u64>>,
}

impl Chunker<'_> {
    /// Chunks a file read by the toolhost; `None` when it is unreadable or changed during two reads.
    async fn remote(
        &self,
        toolhost: &rpc::Client,
        path: &Path,
    ) -> Result<Option<(Vec<String>, bool)>, String> {
        for _ in 0..2 {
            let (before, mut reader) = toolhost.read_file(path, true).await?;
            if let Some(e) = before.error {
                eprintln!("state: skip {}: {e}", path.display());
                return Ok(None);
            }
            let chunks = self.feed(Source::Remote(&mut reader)).await?;
            let after = reader.finish().await?;
            if before.backup || (before.size == after.size && before.mtime_ms == after.mtime_ms) {
                return Ok(Some((chunks, before.backup)));
            }
        }
        Ok(None)
    }

    async fn local(&self, path: &Path) -> Result<(u64, Vec<String>), String> {
        let mut file = tokio::fs::File::open(path)
            .await
            .map_err(|e| format!("open {}: {e}", path.display()))?;
        let size = file
            .metadata()
            .await
            .map_err(|e| format!("stat {}: {e}", path.display()))?
            .len();
        Ok((size, self.feed(Source::Local(&mut file)).await?))
    }

    async fn feed(&self, mut source: Source<'_>) -> Result<Vec<String>, String> {
        let window = 2 * CHUNK_MAX as usize;
        let mut buf = BytesMut::new();
        let mut ids = Vec::new();
        let mut eof = false;
        loop {
            while !eof && buf.len() < window {
                match source.next().await? {
                    Some(block) => buf.extend_from_slice(&block),
                    None => eof = true,
                }
            }
            let end = buf.len();
            let cuts: Vec<(usize, usize)> = FastCDC::new(
                &buf,
                CHUNK_MIN as usize,
                CHUNK_AVG as usize,
                CHUNK_MAX as usize,
            )
            .map(|c| (c.offset, c.length))
            .collect();
            for (offset, length) in cuts {
                if !eof && offset + length == end {
                    break;
                }
                ids.push(self.emit(buf.split_to(length).freeze()).await?);
            }
            if eof {
                return Ok(ids);
            }
        }
    }

    /// Records the sizes of chunks a reused file brings.
    fn record(&self, chunks: &[(String, u64)]) {
        crate::lock(&self.sizes).extend(chunks.iter().cloned());
    }

    fn sized<'s>(&self, ids: impl IntoIterator<Item = &'s String>) -> Vec<(String, u64)> {
        let sizes = crate::lock(&self.sizes);
        ids.into_iter()
            .map(|id| (id.clone(), sizes.get(id).copied().unwrap_or(0)))
            .collect()
    }

    async fn emit(&self, chunk: Bytes) -> Result<String, String> {
        let keys = Arc::clone(&self.keys);
        let len = chunk.len() as u64;
        let (id, chunk) =
            tokio::task::spawn_blocking(move || keys.id(&chunk).map(|id| (id, chunk)))
                .await
                .map_err(|e| format!("id task: {e}"))??;
        if crate::lock(&self.sizes).insert(id.clone(), len).is_some() {
            return Ok(id);
        }
        if self.known.contains(&id) {
            return Ok(id);
        }
        let keys = Arc::clone(&self.keys);
        let (id, sealed) =
            tokio::task::spawn_blocking(move || keys.seal(&id, &chunk).map(|s| (id, s)))
                .await
                .map_err(|e| format!("seal task: {e}"))??;
        let path = self.spool.join(&id);
        tokio::fs::write(&path, sealed)
            .await
            .map_err(|e| format!("spool {}: {e}", path.display()))?;
        Ok(id)
    }
}

enum Source<'a> {
    Remote(&'a mut FileReader),
    Local(&'a mut tokio::fs::File),
}

impl Source<'_> {
    async fn next(&mut self) -> Result<Option<Vec<u8>>, String> {
        match self {
            Self::Remote(r) => r.next().await,
            Self::Local(f) => {
                let mut block = vec![0u8; 1 << 20];
                let n = f.read(&mut block).await.map_err(|e| format!("read: {e}"))?;
                block.truncate(n);
                Ok((n > 0).then_some(block))
            }
        }
    }
}

struct Meter {
    msg: pb::TransferProgress,
    tx: mpsc::Sender<pb::TransferProgress>,
}

impl Meter {
    fn new(total_bytes: u64, total_chunks: usize, tx: mpsc::Sender<pb::TransferProgress>) -> Self {
        Self {
            msg: pb::TransferProgress {
                total_bytes,
                total_chunks: u32::try_from(total_chunks).unwrap_or(u32::MAX),
                ..Default::default()
            },
            tx,
        }
    }

    async fn add(&mut self, bytes: u64) {
        self.msg.done_bytes += bytes;
        self.msg.done_chunks += 1;
        if self.msg.total_bytes < self.msg.done_bytes {
            self.msg.total_bytes = self.msg.done_bytes;
        }
        let _ = self.tx.send(self.msg.clone()).await;
    }

    async fn send(&mut self) {
        let _ = self.tx.send(self.msg.clone()).await;
    }

    async fn finish(&mut self, manifest_id: &str) {
        self.msg.finished = true;
        manifest_id.clone_into(&mut self.msg.manifest_id);
        let _ = self.tx.send(self.msg.clone()).await;
    }
}

enum Failure {
    Transient(String),
    Final(String),
}

/// The contract layout: the desktop user's home and the workspace.
pub fn default_roots(home: &Path, workspace: &Path) -> Vec<Root> {
    vec![
        Root {
            name: HOME_ROOT.to_owned(),
            path: home.to_owned(),
            excluded: HOME_EXCLUDED.map(str::to_owned).into(),
        },
        Root {
            name: WORKSPACE_ROOT.to_owned(),
            path: workspace.to_owned(),
            excluded: Vec::new(),
        },
    ]
}

/// Caches, browser runtime files and restore leftovers never go into a snapshot.
fn excludes() -> Excludes {
    let own = |v: &[&str]| v.iter().map(|s| (*s).to_owned()).collect();
    Excludes {
        prefixes: own(&EXCLUDED_PREFIXES),
        names: own(&EXCLUDED_NAMES),
        name_prefixes: own(&EXCLUDED_NAME_PREFIXES),
        name_suffixes: own(&EXCLUDED_NAME_SUFFIXES),
    }
}

fn stage<'a>(
    records: impl IntoIterator<Item = (PathBuf, &'a FileRecord)>,
    staging: &Path,
    targets: &mut Vec<Target<'a>>,
) -> (Value, Value) {
    let mut dirs = vec![json!({"path": staging, "mode": 0o700})];
    let mut links = Vec::new();
    let mut modes = Vec::new();
    for (path, r) in records {
        match r.entry.kind {
            Kind::Dir => {
                dirs.push(json!({"path": path, "mode": r.entry.mode | 0o700}));
                modes.push(json!({"path": path, "mode": r.entry.mode}));
            }
            Kind::Symlink => links.push(json!({
                "path": path,
                "target": r.entry.target.as_deref().unwrap_or_default(),
            })),
            Kind::File => targets.push(Target {
                dest: Dest::Staged(path, &r.entry),
                chunks: &r.chunks,
            }),
        }
    }
    let mut first = json!({"dirs": null, "symlinks": []});
    first["dirs"] = dirs.into();
    let mut last = json!({"dirs": [], "symlinks": null, "modes": null});
    last["symlinks"] = links.into();
    last["modes"] = modes.into();
    (first, last)
}

/// `path` without its prefix and root: `/home/user` is `home/user`, and `C:\Users\user` is `Users\user`.
fn relative(path: &Path) -> PathBuf {
    path.components()
        .filter(|c| matches!(c, Component::Normal(_)))
        .collect()
}

/// After a restore Chrome follows the restored profile's record of the save, otherwise the close.
fn chrome_start(closed: bool, restored: bool) -> Option<Value> {
    if restored {
        Some(json!({"if_was_running": true}))
    } else {
        closed.then(|| json!({}))
    }
}

/// The manifest's records under the scope's paths, each with its root; refuses a target that is not one plain folder name, a path outside the roots, and a path the manifest lacks.
fn select<'a>(
    roots: &'a [Root],
    manifest: &'a Manifest,
    scope: &pb::RestoreScope,
) -> Result<Vec<(&'a Root, &'a FileRecord)>, StateError> {
    let t = &scope.target;
    if t.is_empty() || t.starts_with('.') || t.contains(['/', '\0']) {
        return Err(StateError::Invalid(format!("invalid target {t:?}")));
    }
    if scope.paths.is_empty() {
        return Err(StateError::Invalid("no path to restore".to_owned()));
    }
    let wanted = scope
        .paths
        .iter()
        .map(|p| scope_path(roots, p))
        .collect::<Result<Vec<_>, _>>()?;
    let mut found = vec![false; wanted.len()];
    let mut out = Vec::new();
    for f in &manifest.files {
        let mut hit = None;
        for (i, (root, rel)) in wanted.iter().enumerate() {
            let under = rel.is_empty()
                || f.entry.path == *rel
                || f.entry
                    .path
                    .strip_prefix(rel.as_str())
                    .is_some_and(|r| r.starts_with('/'));
            if f.entry.root == root.name && under {
                found[i] = true;
                hit = Some(*root);
            }
        }
        if let Some(root) = hit {
            out.push((root, f));
        }
    }
    if let Some(i) = found.iter().position(|f| !f) {
        return Err(StateError::Invalid(format!(
            "{} is not in the snapshot",
            scope.paths[i]
        )));
    }
    Ok(out)
}

/// Splits an absolute box path into its root and the path relative to it; no `.`, `..` or empty parts.
fn scope_path<'a>(roots: &'a [Root], path: &str) -> Result<(&'a Root, String), StateError> {
    let invalid = || StateError::Invalid(format!("invalid path {path:?}"));
    let trimmed = path.strip_suffix('/').unwrap_or(path);
    let rest = trimmed.strip_prefix('/').ok_or_else(invalid)?;
    if rest
        .split('/')
        .any(|c| c.is_empty() || c == "." || c == "..")
    {
        return Err(invalid());
    }
    roots
        .iter()
        .find_map(|r| {
            let rel = Path::new(trimmed).strip_prefix(&r.path).ok()?;
            Some((r, rel.to_str()?.to_owned()))
        })
        .or_else(|| logical_path(roots, trimmed))
        .ok_or_else(invalid)
}

/// The root and relative path of a path in the api's layout of the roots, whatever their paths on this box.
fn logical_path<'a>(roots: &'a [Root], path: &str) -> Option<(&'a Root, String)> {
    API_ROOTS.iter().find_map(|(name, prefix)| {
        let rel = path.strip_prefix(prefix)?;
        let rel = if rel.is_empty() {
            rel
        } else {
            rel.strip_prefix('/')?
        };
        Some((roots.iter().find(|r| r.name == *name)?, rel.to_owned()))
    })
}

/// The path the api names `root` by.
fn api_path(root: &Root) -> &Path {
    API_ROOTS
        .iter()
        .find(|(name, _)| *name == root.name)
        .map_or(&root.path, |(_, p)| Path::new(p))
}

fn encode_manifest(manifest: &Manifest) -> Result<Vec<u8>, String> {
    let plain = serde_json::to_vec(manifest).map_err(|e| format!("encode manifest: {e}"))?;
    if plain.len() as u64 > MANIFEST_LIMIT {
        return Err(format!(
            "manifest of {} bytes exceeds the {MANIFEST_LIMIT} a restore opens",
            plain.len()
        ));
    }
    Ok(plain)
}

/// Chunk ids in manifest order, so Restore pages arrive in the order its files are written.
fn referenced(m: &Manifest) -> Vec<String> {
    let mut seen = HashSet::new();
    m.files
        .iter()
        .flat_map(|f| f.chunks.iter())
        .chain(m.agent_db.iter().flat_map(|b| b.chunks.iter()))
        .filter(|id| seen.insert(id.as_str()))
        .cloned()
        .collect()
}

fn store_client(connect: Duration, read: Duration) -> Result<reqwest::Client, String> {
    reqwest::Client::builder()
        .connect_timeout(connect)
        .read_timeout(read)
        .build()
        .map_err(|e| format!("build store client: {e}"))
}

async fn send_retrying(
    label: &str,
    build: impl Fn() -> reqwest::RequestBuilder,
) -> Result<reqwest::Response, String> {
    retrying(|| send_once(label, build())).await
}

async fn get_retrying(http: &reqwest::Client, url: &str) -> Result<Bytes, String> {
    retrying(|| async {
        send_once("get", http.get(url))
            .await?
            .bytes()
            .await
            .map_err(|e| Failure::Transient(format!("get body: {}", causes(&e))))
    })
    .await
}

async fn retrying<T, F>(attempt: impl Fn() -> F) -> Result<T, String>
where
    F: Future<Output = Result<T, Failure>>,
{
    let mut tries = 0;
    loop {
        let failure = match attempt().await {
            Ok(v) => return Ok(v),
            Err(Failure::Final(e)) => return Err(e),
            Err(Failure::Transient(e)) => e,
        };
        tries += 1;
        if tries >= ATTEMPTS {
            return Err(failure);
        }
        tokio::time::sleep(RETRY_BASE * 2u32.pow(tries - 1)).await;
    }
}

async fn send_once(
    label: &str,
    req: reqwest::RequestBuilder,
) -> Result<reqwest::Response, Failure> {
    match req.send().await {
        Ok(r) if r.status().is_success() => Ok(r),
        Ok(r) if !retryable(r.status()) => Err(Failure::Final(format!("{label}: {}", r.status()))),
        Ok(r) => Err(Failure::Transient(format!("{label}: {}", r.status()))),
        Err(e) => Err(Failure::Transient(format!("{label}: {}", causes(&e)))),
    }
}

fn retryable(status: reqwest::StatusCode) -> bool {
    status.is_server_error()
        || status == reqwest::StatusCode::REQUEST_TIMEOUT
        || status == reqwest::StatusCode::TOO_MANY_REQUESTS
}

fn causes(e: &dyn std::error::Error) -> String {
    let mut out = e.to_string();
    let mut next = e.source();
    while let Some(c) = next {
        out.push_str(": ");
        out.push_str(&c.to_string());
        next = c.source();
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn a_cancelled_plan_resumes_chrome_before_the_next_plan_even_after_failure() {
        for phase in ["browser_close", "state_walk"] {
            for failed in [false, true] {
                let dir = tempfile::tempdir().unwrap();
                let socket = crate::testing::endpoint(dir.path(), "toolhost.sock");
                let mut listener = crate::toolhost::bind(&socket, "").unwrap();
                let (seen, mut calls) = mpsc::channel(8);
                let release = Arc::new(tokio::sync::Notify::new());
                let proceed = Arc::clone(&release);
                let ready = Arc::new(tokio::sync::Notify::new());
                let chrome_ready = Arc::clone(&ready);
                let server = tokio::spawn(async move {
                    let mut jobs = tokio::task::JoinSet::new();
                    for i in 0..5 {
                        let mut s = crate::toolhost::accept(&mut listener).await.unwrap();
                        let seen = seen.clone();
                        let proceed = Arc::clone(&proceed);
                        let chrome_ready = Arc::clone(&chrome_ready);
                        jobs.spawn(async move {
                            let call: Call = rpc::read_frame(&mut s).await.unwrap();
                            seen.send(call.tool.clone()).await.unwrap();
                            if call.tool == phase && i < 3 {
                                proceed.notified().await;
                            }
                            if call.tool == "browser_start" {
                                assert_eq!(call.args, json!({}));
                                chrome_ready.notified().await;
                            }
                            if call.tool == "state_walk" {
                                rpc::write_data(&mut s, if failed { b"{" } else { b"[]" })
                                    .await
                                    .unwrap();
                                rpc::write_data(&mut s, b"").await.unwrap();
                            } else {
                                rpc::write_frame(&mut s, &rpc::Output::text("closed"))
                                    .await
                                    .unwrap();
                            }
                        });
                    }
                    while let Some(done) = jobs.join_next().await {
                        done.unwrap();
                    }
                });
                let engine = Arc::new(test_engine(dir.path(), socket).await);
                let request = pb::PlanRequest {
                    key: Some(pb::StateKey {
                        key_id: "key".to_owned(),
                        key: vec![7; 32].into(),
                    }),
                    consistency: pb::Consistency::Quiesced.into(),
                    ..Default::default()
                };
                let next_request = pb::PlanRequest {
                    consistency: pb::Consistency::Live.into(),
                    ..request.clone()
                };
                let caller = {
                    let engine = Arc::clone(&engine);
                    tokio::spawn(async move { engine.plan(request).await })
                };
                tokio::time::timeout(Duration::from_secs(5), async {
                    while calls.recv().await.as_deref() != Some(phase) {}
                })
                .await
                .unwrap();
                caller.abort();
                assert!(caller.await.unwrap_err().is_cancelled());
                assert!(engine.pause.turns.try_read().is_err());
                release.notify_one();
                tokio::time::timeout(Duration::from_secs(5), async {
                    while calls.recv().await.as_deref() != Some("browser_start") {}
                })
                .await
                .unwrap();
                assert!(engine.pause.turns.try_write().is_ok());
                let next = {
                    let engine = Arc::clone(&engine);
                    tokio::spawn(async move { engine.plan(next_request).await })
                };
                assert!(
                    tokio::time::timeout(Duration::from_millis(50), calls.recv())
                        .await
                        .is_err()
                );
                ready.notify_one();
                let planned = tokio::time::timeout(Duration::from_secs(5), next)
                    .await
                    .unwrap()
                    .unwrap();
                assert_eq!(planned.is_ok(), !failed);
                tokio::time::timeout(Duration::from_secs(5), server)
                    .await
                    .unwrap()
                    .unwrap();
                assert!(engine.pause.turns.try_write().is_ok());
                assert_eq!(engine.pinned.lock().await.is_some(), !failed);
            }
        }
    }

    #[cfg(windows)]
    #[test]
    fn a_windows_scope_path_names_its_root_by_name_or_by_path() {
        let roots = default_roots(Path::new(r"C:\Users\u"), Path::new(r"C:\workspace"));
        let (root, rel) = scope_path(&roots, "/workspace/gate/state.txt").unwrap();
        assert_eq!(
            (root.name.as_str(), rel.as_str()),
            ("workspace", "gate/state.txt")
        );
        let (root, rel) = scope_path(&roots, "/home/user/Documents").unwrap();
        assert_eq!((root.name.as_str(), rel.as_str()), ("home", "Documents"));
        assert!(scope_path(&roots, "/home/Documents").is_err());
        assert!(scope_path(&roots, "/elsewhere/x").is_err());
    }

    #[test]
    fn a_scope_path_in_the_api_layout_names_its_root_whatever_the_roots_path() {
        let roots = default_roots(Path::new("/Users/u"), Path::new("/workspace"));
        let (root, rel) = scope_path(&roots, "/home/user/Documents/a.txt").unwrap();
        assert_eq!(
            (root.name.as_str(), rel.as_str()),
            ("home", "Documents/a.txt")
        );
        let (root, rel) = scope_path(&roots, "/home/user/").unwrap();
        assert_eq!((root.name.as_str(), rel.as_str()), ("home", ""));
        let (root, rel) = scope_path(&roots, "/workspace/x").unwrap();
        assert_eq!((root.name.as_str(), rel.as_str()), ("workspace", "x"));
        for bad in ["/home/userx/a", "/home/Documents", "/elsewhere/x"] {
            assert!(scope_path(&roots, bad).is_err(), "{bad}");
        }
        assert_eq!(relative(api_path(&roots[0])), Path::new("home/user"));
    }

    #[test]
    fn a_restored_root_lands_under_its_path_without_prefix_or_root() {
        assert_eq!(relative(Path::new("/home/user")), Path::new("home/user"));
        #[cfg(windows)]
        assert_eq!(
            relative(Path::new(r"C:\Users\user")),
            Path::new(r"Users\user")
        );
    }

    #[test]
    fn a_manifest_a_restore_could_not_open_fails_the_plan() {
        let mut manifest = Manifest {
            version: manifest::VERSION,
            parent: String::new(),
            key_id: "k".into(),
            taken_at_ms: 0,
            consistency: "live".into(),
            files: Vec::new(),
            agent_db: None,
            skipped: vec!["a".repeat(MANIFEST_LIMIT as usize)],
        };
        let err = encode_manifest(&manifest).unwrap_err();
        assert!(err.contains("a restore opens"), "{err}");
        manifest.skipped = vec!["a".into()];
        assert!(encode_manifest(&manifest).is_ok());
    }

    #[tokio::test]
    async fn resume_returns_before_chrome_is_ready() {
        let dir = tempfile::tempdir().unwrap();
        let socket = crate::testing::endpoint(dir.path(), "toolhost.sock");
        let mut listener = crate::toolhost::bind(&socket, "").unwrap();
        let (seen, mut calls) = mpsc::channel(4);
        tokio::spawn(async move {
            while let Ok(mut s) = crate::toolhost::accept(&mut listener).await {
                let call: Call = rpc::read_frame(&mut s).await.unwrap();
                seen.send(call.args).await.unwrap();
                tokio::time::sleep(Duration::from_secs(1)).await;
                let out = rpc::Output::error("chrome did not expose CDP");
                rpc::write_frame(&mut s, &out).await.unwrap();
            }
        });
        let engine = test_engine(dir.path(), socket).await;
        let turns = Arc::clone(&engine.pause.turns);
        let gate = Arc::clone(&turns).write_owned().await;
        let _resume = engine.resume(
            Quiet {
                _gate: gate,
                chrome: true,
            },
            true,
        );
        assert!(turns.try_write().is_ok(), "turns run again at once");
        let args = tokio::time::timeout(Duration::from_secs(5), calls.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(args, json!({"if_was_running": true}));
    }

    #[tokio::test]
    async fn a_store_that_never_answers_fails_the_transfer() {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}/chunk", listener.local_addr().unwrap());
        let (held, mut accepted) = mpsc::channel(ATTEMPTS as usize);
        tokio::spawn(async move {
            while let Ok((conn, _)) = listener.accept().await {
                held.send(conn).await.unwrap();
            }
        });
        let http = store_client(Duration::from_secs(1), Duration::from_millis(200)).unwrap();
        let result = tokio::time::timeout(
            Duration::from_secs(10),
            send_retrying("get", || http.get(&url)),
        )
        .await
        .expect("a silent store stalls the transfer");
        assert!(result.is_err());
        for _ in 0..ATTEMPTS {
            assert!(accepted.recv().await.is_some());
        }
    }

    #[tokio::test]
    async fn a_body_cut_short_is_fetched_again() {
        let url = cutting_store(1).await;
        let http = store_client(Duration::from_secs(1), Duration::from_secs(1)).unwrap();
        let got = get_retrying(&http, &url).await.unwrap();
        assert_eq!(&got[..], b"sealed");
    }

    #[tokio::test]
    async fn a_body_cut_short_every_time_reports_why() {
        let url = cutting_store(ATTEMPTS).await;
        let http = store_client(Duration::from_secs(1), Duration::from_secs(1)).unwrap();
        let err = get_retrying(&http, &url).await.unwrap_err();
        assert!(
            err.starts_with("get body: error decoding response body: "),
            "{err}"
        );
    }

    #[test]
    fn a_chunk_keeps_its_transfer_slot_until_it_is_decrypted() {
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .max_blocking_threads(1)
            .build()
            .unwrap();
        runtime.block_on(async {
            let dir = tempfile::tempdir().unwrap();
            let engine = test_engine(dir.path(), dir.path().join("toolhost.sock")).await;
            let keys = Arc::new(Keys::derive(&[7u8; 32]).unwrap());
            let plain = b"chunk body".to_vec();
            let id = keys.id(&plain).unwrap();
            let sealed = keys.seal(&id, &plain).unwrap();
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
            let url = format!("http://{}/chunk", listener.local_addr().unwrap());
            let (received, body_received) = tokio::sync::oneshot::channel();
            tokio::spawn(async move {
                let (mut conn, _) = listener.accept().await.unwrap();
                let mut request = [0u8; 1024];
                let _ = conn.read(&mut request).await.unwrap();
                let head = format!(
                    "HTTP/1.1 200 OK\r\nconnection: close\r\ncontent-length: {}\r\n\r\n",
                    sealed.len()
                );
                conn.write_all(head.as_bytes()).await.unwrap();
                conn.write_all(&sealed).await.unwrap();
                while conn.read(&mut request).await.unwrap() > 0 {}
                received.send(()).unwrap();
            });
            let (release, held) = std::sync::mpsc::channel::<()>();
            let blocker = tokio::task::spawn_blocking(move || held.recv().unwrap());
            let (tx, _progress) = mpsc::channel(8);
            let filled = Mutex::new(Filled {
                meter: Meter::new(0, 1, tx),
                sizes: HashMap::new(),
                sqlite: HashSet::new(),
            });
            let gets = Semaphore::new(1);
            let mut fetch = std::pin::pin!(engine.fetch_chunk(&keys, &id, &url, &gets, &filled));
            tokio::select! {
                done = &mut fetch => panic!("the chunk finished before its decryption ran: {done:?}"),
                received = body_received => received.unwrap(),
            }
            let early = tokio::time::timeout(Duration::from_millis(200), &mut fetch).await;
            assert!(early.is_err(), "decryption has no thread to run on yet");
            assert_eq!(
                gets.available_permits(),
                0,
                "the slot is free before decryption"
            );
            release.send(()).unwrap();
            blocker.await.unwrap();
            assert_eq!(fetch.await.unwrap(), plain);
            assert_eq!(gets.available_permits(), 1);
        });
    }

    #[tokio::test]
    async fn a_new_engine_clears_what_a_killed_operation_left_and_keeps_the_known_chunks() {
        let dir = tempfile::tempdir().unwrap();
        let work = dir.path().join("work");
        for spool in ["spool-1", "restore-2"] {
            std::fs::create_dir_all(work.join(spool)).unwrap();
            std::fs::write(work.join(spool).join("chunk"), b"sealed").unwrap();
        }
        std::fs::write(work.join("agent-db-3.db"), b"db").unwrap();
        std::fs::write(work.join(KNOWN_FILE), b"{}").unwrap();
        test_engine(dir.path(), dir.path().join("toolhost.sock")).await;
        let left: Vec<_> = std::fs::read_dir(&work)
            .unwrap()
            .map(|e| e.unwrap().file_name())
            .collect();
        assert_eq!(left, [KNOWN_FILE]);
    }

    #[tokio::test]
    async fn a_toolhost_out_of_reach_fails_the_read_instead_of_skipping_the_file() {
        let dir = tempfile::tempdir().unwrap();
        let known = HashSet::new();
        let chunker = Chunker {
            keys: Arc::new(Keys::derive(&[7u8; 32]).unwrap()),
            spool: dir.path(),
            known: &known,
            sizes: std::sync::Mutex::new(HashMap::new()),
        };
        let toolhost = rpc::Client::new(dir.path().join("toolhost.sock"));
        let read = chunker
            .remote(&toolhost, Path::new("/home/user/a.txt"))
            .await;
        assert!(read.is_err(), "{read:?}");
    }

    #[test]
    fn chrome_comes_back_as_it_was_at_the_save() {
        assert_eq!(
            chrome_start(false, true),
            Some(json!({"if_was_running": true}))
        );
        assert_eq!(
            chrome_start(true, true),
            Some(json!({"if_was_running": true}))
        );
        assert_eq!(chrome_start(true, false), Some(json!({})));
        assert_eq!(chrome_start(false, false), None);
    }

    async fn test_engine(dir: &Path, socket: PathBuf) -> StateEngine {
        StateEngine::new(
            rpc::Client::new(socket),
            Store::open(&dir.join("agent.db")).unwrap(),
            default_roots(Path::new("/home/user"), Path::new("/workspace")),
            dir.join("work"),
            Pause {
                turns: Arc::default(),
                guests: Arc::default(),
                wait: Duration::from_secs(1),
            },
            None,
            false,
        )
        .await
        .unwrap()
    }

    async fn cutting_store(cuts: u32) -> String {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}/chunk", listener.local_addr().unwrap());
        tokio::spawn(async move {
            let mut served = 0;
            while let Ok((mut conn, _)) = listener.accept().await {
                let mut req = [0u8; 1024];
                let _ = conn.read(&mut req).await;
                let body: &[u8] = if served < cuts { b"sea" } else { b"sealed" };
                let head = "HTTP/1.1 200 OK\r\ncontent-length: 6\r\nconnection: close\r\n\r\n";
                conn.write_all(head.as_bytes()).await.unwrap();
                conn.write_all(body).await.unwrap();
                served += 1;
            }
        });
        url
    }
}
