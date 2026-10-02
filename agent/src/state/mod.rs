//! The state layer: snapshots of the user's home, /workspace and agent.db as encrypted chunks.

pub mod crypto;
pub mod manifest;

use std::collections::{BTreeSet, HashMap, HashSet, VecDeque};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use fastcdc::v2020::FastCDC;
use futures_util::stream::{self, FuturesUnordered};
use futures_util::{Stream, StreamExt};
use serde::{Deserialize, Serialize};
use serde_json::json;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::{Mutex, OnceCell, OwnedRwLockWriteGuard, RwLock, Semaphore, mpsc};

use crate::pb;
use crate::store::Store;
use crate::toolhost::rpc::{self, Call, FileReader, FileWriter};
use crate::toolhost::state::{Entry, Excludes, Kind, Root};
use crypto::Keys;
use manifest::{Blob, FileRecord, Manifest};

const CHUNK_MIN: u32 = 256 << 10;
const CHUNK_AVG: u32 = 1 << 20;
const CHUNK_MAX: u32 = 4 << 20;
const MANIFEST_LIMIT: u64 = 256 << 20;
const TRANSFERS: usize = 8;
const RESTORE_WRITES: usize = 8;
const RESTORE_AHEAD: usize = 4;
const PLAN_READS: usize = 4;
const ATTEMPTS: u32 = 3;
const RETRY_BASE: Duration = Duration::from_millis(500);
const QUIESCE_WAIT: Duration = Duration::from_secs(30);
const REUSE_AGE_MS: i64 = 1_000;
const STAGING_PREFIX: &str = ".cb-restore-";
const SQLITE_SIDECARS: [&str; 2] = ["-wal", "-journal"];

const MANIFEST_FILE: &str = "manifest";
const KNOWN_FILE: &str = "known-chunks.json";

const EXCLUDED_PREFIXES: [&str; 2] = [".cache", ".local/share/Trash"];
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
const EXCLUDED_NAME_PREFIXES: [&str; 4] =
    ["Singleton", STAGING_PREFIX, ".cb-trash-", ".org.chromium."];
const EXCLUDED_NAME_SUFFIXES: [&str; 1] = ["-shm"];

/// Snapshots, uploads and restores user state; one operation runs at a time.
pub struct StateEngine {
    toolhost: rpc::Client,
    store: Store,
    roots: Vec<Root>,
    excludes: Excludes,
    work: PathBuf,
    turns: Arc<RwLock<()>>,
    desktop_unit: Option<String>,
    http: reqwest::Client,
    pinned: Mutex<Option<Pinned>>,
}

/// Why a state operation failed; `Busy` means a turn kept running past the quiesce wait.
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

impl StateEngine {
    /// Creates an engine over `roots`, keeping its spool under `work`; `turns` pauses the loop
    /// and `desktop_unit` is the session unit Restore stops around the swap.
    pub fn new(
        toolhost: rpc::Client,
        store: Store,
        roots: Vec<Root>,
        work: PathBuf,
        turns: Arc<RwLock<()>>,
        desktop_unit: Option<String>,
    ) -> Self {
        Self {
            toolhost,
            store,
            roots,
            excludes: excludes(),
            work,
            turns,
            desktop_unit,
            http: reqwest::Client::new(),
            pinned: Mutex::new(None),
        }
    }

    /// Walks and chunks the state, spools new chunks and pins the snapshot for Upload.
    pub async fn plan(&self, req: pb::PlanRequest) -> Result<pb::PlanResponse, StateError> {
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
        if let Some(q) = quiet {
            self.resume(q, false);
        }
        let (snapshot, response) = result?;
        *pinned = Some(snapshot);
        Ok(response)
    }

    /// Uploads the listed chunks of the pinned snapshot; the call that carries the manifest
    /// writes it last and unpins the snapshot, so a large upload may span several calls.
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
            if !path.exists() {
                return Err(StateError::Invalid(format!(
                    "chunk {} was not spooled because the last upload held it; plan again without a parent",
                    put.chunk_id
                )));
            }
            jobs.push((put, path));
        }
        let has_manifest = req.manifest.is_some();
        if let Some(manifest) = req.manifest {
            jobs.push((manifest, snap.spool.join(MANIFEST_FILE)));
        }
        let mut total = 0;
        for (_, path) in &jobs {
            total += tokio::fs::metadata(path)
                .await
                .map_err(|e| format!("stat {}: {e}", path.display()))?
                .len();
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
        let id = snap.snapshot_id.clone();
        *pinned = None;
        meter.finish(&id).await;
        Ok(())
    }

    /// Replaces the user state with the manifest's; the first message names the manifest, chunk
    /// URLs arrive in pages and are fetched as they come, and the loop's writers pause for the swap.
    pub async fn restore<S>(
        &self,
        mut reqs: S,
        progress: mpsc::Sender<pb::TransferProgress>,
    ) -> Result<(), StateError>
    where
        S: Stream<Item = Result<pb::RestoreRequest, String>> + Unpin,
    {
        let _one = self.pinned.lock().await;
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
        let sealed = self.get(&get.url).await?;
        let plain = keys.open(&get.chunk_id, &sealed, MANIFEST_LIMIT)?;
        let manifest: Manifest =
            serde_json::from_slice(&plain).map_err(|e| format!("decode manifest: {e}"))?;
        if manifest.version != manifest::VERSION {
            return Err(StateError::Invalid(format!(
                "manifest version {} is not supported",
                manifest.version
            )));
        }
        let cache = self
            .work
            .join(format!("restore-{}", uuid::Uuid::new_v4().simple()));
        tokio::fs::create_dir_all(&cache)
            .await
            .map_err(|e| format!("create {}: {e}", cache.display()))?;
        let result = self
            .restore_from(
                &keys,
                &manifest,
                first.gets,
                reqs,
                &cache,
                progress,
                &get.chunk_id,
            )
            .await;
        let _ = tokio::fs::remove_dir_all(&cache).await;
        result
    }

    #[allow(clippy::too_many_arguments)]
    async fn restore_from<S>(
        &self,
        keys: &Arc<Keys>,
        manifest: &Manifest,
        queue: Vec<pb::PresignedGet>,
        reqs: S,
        cache: &Path,
        progress: mpsc::Sender<pb::TransferProgress>,
        manifest_id: &str,
    ) -> Result<(), StateError>
    where
        S: Stream<Item = Result<pb::RestoreRequest, String>> + Unpin,
    {
        let mut targets = Vec::new();
        let mut links = Vec::with_capacity(self.roots.len());
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
            let mut dirs = vec![json!({"path": staging, "mode": 0o700})];
            let mut root_links = Vec::new();
            for r in &records {
                let path = staging.join(&r.entry.path);
                match r.entry.kind {
                    Kind::Dir => dirs.push(json!({"path": path, "mode": r.entry.mode})),
                    Kind::Symlink => root_links.push(json!({
                        "path": path,
                        "target": r.entry.target.clone().unwrap_or_default(),
                    })),
                    Kind::File => targets.push(Target {
                        dest: Dest::Staged(path, &r.entry),
                        chunks: &r.chunks,
                    }),
                }
            }
            self.tool("state_apply", json!({"dirs": dirs, "symlinks": []}))
                .await?;
            links.push(root_links);
            let names: BTreeSet<&str> = records
                .iter()
                .filter_map(|r| r.entry.path.split('/').next())
                .collect();
            let keep: Vec<&str> = self
                .excludes
                .prefixes
                .iter()
                .map(String::as_str)
                .filter(|p| !p.contains('/'))
                .collect();
            swaps
                .push(json!({"root": root.path, "staging": staging, "names": names, "keep": keep}));
        }
        if let Some(db) = &manifest.agent_db {
            targets.push(Target {
                dest: Dest::Local(cache.join("agent.db")),
                chunks: &db.chunks,
            });
        }
        let mut meter = self
            .fill(keys, targets, queue, reqs, cache, progress)
            .await?;
        for symlinks in links {
            self.tool("state_apply", json!({"dirs": [], "symlinks": symlinks}))
                .await?;
        }
        let quiet = self.quiesce().await?;
        let result = self.swap_in(manifest, cache, swaps).await;
        self.resume(quiet, result.is_ok());
        result?;
        meter.finish(manifest_id).await;
        Ok(())
    }

    /// Writes every target as its chunk urls arrive, fetching each chunk once; a chunk used more
    /// than once goes through `cache`. Fails if a chunk never got a url.
    async fn fill<S>(
        &self,
        keys: &Arc<Keys>,
        targets: Vec<Target<'_>>,
        mut queue: Vec<pb::PresignedGet>,
        mut reqs: S,
        cache: &Path,
        progress: mpsc::Sender<pb::TransferProgress>,
    ) -> Result<Meter, StateError>
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
        let meter = Mutex::new(Meter::new(0, uses.len(), progress));
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
                jobs.push(self.write_target(keys, t.dest, parts, cache, &gets, &meter));
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
        Ok(meter.into_inner())
    }

    async fn write_target(
        &self,
        keys: &Arc<Keys>,
        dest: Dest<'_>,
        parts: Vec<Part>,
        cache: &Path,
        gets: &Semaphore,
        meter: &Mutex<Meter>,
    ) -> Result<(), String> {
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
            .map(|p| self.part(keys, p, cache, gets, meter))
            .buffered(RESTORE_AHEAD);
        while let Some(d) = data.next().await {
            sink.send(&d?).await?;
        }
        sink.finish().await
    }

    async fn part(
        &self,
        keys: &Arc<Keys>,
        part: Part,
        cache: &Path,
        gets: &Semaphore,
        meter: &Mutex<Meter>,
    ) -> Result<Vec<u8>, String> {
        match part {
            Part::Once { id, url } => self.fetch_chunk(keys, &id, &url, gets, meter).await,
            Part::Shared { id, url, cell } => {
                cell.get_or_try_init(|| async {
                    let plain = self.fetch_chunk(keys, &id, &url, gets, meter).await?;
                    let path = cache.join(&id);
                    tokio::fs::write(&path, plain)
                        .await
                        .map_err(|e| format!("write {}: {e}", path.display()))
                })
                .await?;
                read_cached(cache, &id).await
            }
        }
    }

    /// Stops the desktop session, swaps the staged roots in and restores agent.db, then starts it.
    async fn swap_in(
        &self,
        manifest: &Manifest,
        cache: &Path,
        swaps: Vec<serde_json::Value>,
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
        swaps: Vec<serde_json::Value>,
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
        let Some(unit) = &self.desktop_unit else {
            return Ok(());
        };
        let status = tokio::process::Command::new("systemctl")
            .args([verb, unit.as_str()])
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
        let spool = self
            .work
            .join(format!("spool-{}", uuid::Uuid::new_v4().simple()));
        tokio::fs::create_dir_all(&spool)
            .await
            .map_err(|e| format!("create {}: {e}", spool.display()))?;
        let mut last = if parent.is_empty() {
            Known::default()
        } else {
            self.load_known(key_id).await
        };
        let chunker = Chunker {
            keys: Arc::clone(keys),
            spool: spool.clone(),
            known: &last.chunks,
            sizes: std::sync::Mutex::new(HashMap::new()),
        };
        let settled_ns = (crate::now_ms() - REUSE_AGE_MS) * 1_000_000;
        let walked = self
            .tool(
                "state_walk",
                json!({"roots": self.roots, "excludes": self.excludes}),
            )
            .await?;
        let entries: Vec<Entry> =
            serde_json::from_str(&walked).map_err(|e| format!("decode walk: {e}"))?;
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
        let mut prepare = |mut entry: Entry| {
            let key = format!("{}/{}", entry.root, entry.path);
            let ctime_ns = entry.ctime_ns.take();
            let step = if entry.kind != Kind::File {
                Step::Keep
            } else if let Some(root) = roots.get(entry.root.as_str()) {
                last.files
                    .remove(&key)
                    .filter(|r| r.matches(&entry, ctime_ns))
                    .map_or_else(|| Step::Read(root.join(&entry.path)), Step::Reuse)
            } else {
                Step::Drop
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
                        chunker.record(&r.chunks)?;
                        let chunks = r.chunks.iter().map(|(id, _)| id.clone()).collect();
                        reuse.insert(key, r);
                        files.push(FileRecord { entry, chunks });
                    }
                    Step::Chunked(Some((chunks, backup))) => {
                        if backup {
                            backed_up.insert(key);
                        } else if let Some(ctime_ns) = ctime_ns.filter(|c| *c < settled_ns) {
                            let r = Reuse {
                                size: entry.size,
                                mtime_ms: entry.mtime_ms,
                                ctime_ns,
                                chunks: chunker.sized(&chunks)?,
                            };
                            reuse.insert(key, r);
                        }
                        files.push(FileRecord { entry, chunks });
                    }
                    Step::Chunked(None) => skipped.push(key),
                    Step::Read(_) | Step::Drop => {}
                }
            }
        }
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
        let plain = serde_json::to_vec(&manifest).map_err(|e| format!("encode manifest: {e}"))?;
        let snapshot_id = keys.id(&plain)?;
        let sealed = keys.seal(&snapshot_id, &plain)?;
        tokio::fs::write(spool.join(MANIFEST_FILE), sealed)
            .await
            .map_err(|e| format!("write manifest: {e}"))?;
        let ids = referenced(&manifest);
        let sizes = chunker.sized(&ids)?;
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

    async fn quiesce(&self) -> Result<Quiet, StateError> {
        let gate = tokio::time::timeout(QUIESCE_WAIT, Arc::clone(&self.turns).write_owned())
            .await
            .map_err(|_| StateError::Busy("a turn is still running; retry later".to_owned()))?;
        if let Err(e) = self.tool("desktop_save", json!({})).await {
            eprintln!("state: save the desktop session: {e}");
        }
        let closed = self.tool("browser_close", json!({})).await?;
        Ok(Quiet {
            _gate: gate,
            chrome: closed == "closed",
        })
    }

    /// Ends the pause; Chrome starts in the background, and a browser tool waits on its lock meanwhile.
    fn resume(&self, quiet: Quiet, restored: bool) {
        let Some(args) = chrome_start(quiet.chrome, restored) else {
            return;
        };
        let toolhost = self.toolhost.clone();
        // detached: the start reports only to the log
        tokio::spawn(async move {
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
        });
    }

    async fn tool(&self, name: &str, args: serde_json::Value) -> Result<String, String> {
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
        let mut attempt = 0;
        loop {
            let mut req = self.http.put(&put.url).body(body.clone());
            for (k, v) in &put.headers {
                req = req.header(k, v);
            }
            let failure = match req.send().await {
                Ok(r) if r.status().is_success() => return Ok(size),
                Ok(r) if !retryable(r.status()) => {
                    return Err(format!("put {}: {}", put.chunk_id, r.status()));
                }
                Ok(r) => format!("put {}: {}", put.chunk_id, r.status()),
                Err(e) => format!("put {}: {e}", put.chunk_id),
            };
            attempt += 1;
            if attempt >= ATTEMPTS {
                return Err(failure);
            }
            tokio::time::sleep(RETRY_BASE * 2u32.pow(attempt - 1)).await;
        }
    }

    async fn get(&self, url: &str) -> Result<Vec<u8>, String> {
        let mut attempt = 0;
        loop {
            let failure = match self.http.get(url).send().await {
                Ok(r) if r.status().is_success() => {
                    return r
                        .bytes()
                        .await
                        .map(|b| b.to_vec())
                        .map_err(|e| format!("get body: {e}"));
                }
                Ok(r) if !retryable(r.status()) => return Err(format!("get: {}", r.status())),
                Ok(r) => format!("get: {}", r.status()),
                Err(e) => format!("get: {e}"),
            };
            attempt += 1;
            if attempt >= ATTEMPTS {
                return Err(failure);
            }
            tokio::time::sleep(RETRY_BASE * 2u32.pow(attempt - 1)).await;
        }
    }

    async fn fetch_chunk(
        &self,
        keys: &Arc<Keys>,
        id: &str,
        url: &str,
        gets: &Semaphore,
        meter: &Mutex<Meter>,
    ) -> Result<Vec<u8>, String> {
        let sealed = {
            let _slot = gets
                .acquire()
                .await
                .map_err(|e| format!("transfer slot: {e}"))?;
            self.get(url)
                .await
                .map_err(|e| format!("chunk {id}: {e}"))?
        };
        let size = sealed.len() as u64;
        let keys = Arc::clone(keys);
        let owned = id.to_owned();
        let plain =
            tokio::task::spawn_blocking(move || keys.open(&owned, &sealed, u64::from(CHUNK_MAX)))
                .await
                .map_err(|e| format!("open task: {e}"))??;
        meter.lock().await.add(size).await;
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

/// The snapshot waiting for Upload.
struct Pinned {
    snapshot_id: String,
    spool: PathBuf,
    known: Known,
}

/// Chunk ids the store held after the last upload under one key, and the files that can reuse theirs.
#[derive(Default, Deserialize, Serialize)]
struct Known {
    key_id: String,
    chunks: HashSet<String>,
    #[serde(default)]
    files: HashMap<String, Reuse>,
}

/// A file's stamp at the last upload and its chunks with their sizes.
#[derive(Deserialize, Serialize)]
struct Reuse {
    size: u64,
    mtime_ms: i64,
    ctime_ns: i64,
    chunks: Vec<(String, u64)>,
}

impl Reuse {
    /// Size, mtime and the kernel-set ctime all match, so no write happened since the last read.
    fn matches(&self, entry: &Entry, ctime_ns: Option<i64>) -> bool {
        self.size == entry.size
            && self.mtime_ms == entry.mtime_ms
            && ctime_ns == Some(self.ctime_ns)
    }
}

/// Turns are paused and Chrome is closed while this lives.
struct Quiet {
    _gate: OwnedRwLockWriteGuard<()>,
    chrome: bool,
}

/// A file Restore writes and the chunks it is made of.
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

/// The open file a target is written into.
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
    Drop,
    Reuse(Reuse),
    Read(PathBuf),
    Chunked(Option<(Vec<String>, bool)>),
}

/// Splits byte streams into content-defined chunks and spools the ones the store lacks.
struct Chunker<'a> {
    keys: Arc<Keys>,
    spool: PathBuf,
    known: &'a HashSet<String>,
    sizes: std::sync::Mutex<HashMap<String, u64>>,
}

impl Chunker<'_> {
    /// Chunks a file read by the toolhost; `None` when it changed during two reads.
    async fn remote(
        &self,
        toolhost: &rpc::Client,
        path: &Path,
    ) -> Result<Option<(Vec<String>, bool)>, String> {
        for _ in 0..2 {
            let (before, mut reader) = match toolhost.read_file(path, true).await {
                Ok(r) => r,
                Err(e) => {
                    eprintln!("state: skip {}: {e}", path.display());
                    return Ok(None);
                }
            };
            let chunks = self.feed(Source::Remote(&mut reader)).await?;
            let after = reader.finish().await?;
            if before.backup || (before.size == after.size && before.mtime_ms == after.mtime_ms) {
                return Ok(Some((chunks, before.backup)));
            }
        }
        Ok(None)
    }

    /// Chunks a local file; returns its size and chunks.
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
        let mut buf: Vec<u8> = Vec::new();
        let mut ids = Vec::new();
        let mut eof = false;
        loop {
            while !eof && buf.len() < window {
                match source.next().await? {
                    Some(block) => buf.extend_from_slice(&block),
                    None => eof = true,
                }
            }
            let mut consumed = 0;
            let cuts: Vec<(usize, usize)> = FastCDC::new(&buf, CHUNK_MIN, CHUNK_AVG, CHUNK_MAX)
                .map(|c| (c.offset, c.length))
                .collect();
            for (offset, length) in cuts {
                if !eof && offset + length == buf.len() {
                    break;
                }
                ids.push(self.emit(buf[offset..offset + length].to_vec()).await?);
                consumed = offset + length;
            }
            buf.drain(..consumed);
            if eof {
                return Ok(ids);
            }
        }
    }

    /// Records the sizes of chunks a reused file brings.
    fn record(&self, chunks: &[(String, u64)]) -> Result<(), String> {
        self.lock()?.extend(chunks.iter().cloned());
        Ok(())
    }

    /// Pairs each chunk id with its size.
    fn sized<'s>(
        &self,
        ids: impl IntoIterator<Item = &'s String>,
    ) -> Result<Vec<(String, u64)>, String> {
        let sizes = self.lock()?;
        Ok(ids
            .into_iter()
            .map(|id| (id.clone(), sizes.get(id).copied().unwrap_or(0)))
            .collect())
    }

    fn lock(&self) -> Result<std::sync::MutexGuard<'_, HashMap<String, u64>>, String> {
        self.sizes
            .lock()
            .map_err(|_| "chunk sizes lock poisoned".to_owned())
    }

    async fn emit(&self, chunk: Vec<u8>) -> Result<String, String> {
        let keys = Arc::clone(&self.keys);
        let len = chunk.len() as u64;
        let (id, chunk) =
            tokio::task::spawn_blocking(move || keys.id(&chunk).map(|id| (id, chunk)))
                .await
                .map_err(|e| format!("id task: {e}"))??;
        if self.lock()?.insert(id.clone(), len).is_some() {
            return Ok(id);
        }
        if self.known.contains(&id) {
            return Ok(id);
        }
        let keys = Arc::clone(&self.keys);
        let seal_id = id.clone();
        let sealed = tokio::task::spawn_blocking(move || keys.seal(&seal_id, &chunk))
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

/// Reports transfer progress to the caller.
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

/// The contract layout: the desktop user's home and /workspace.
pub fn default_roots() -> Vec<Root> {
    vec![
        Root {
            name: "home".to_owned(),
            path: PathBuf::from("/home/user"),
        },
        Root {
            name: "workspace".to_owned(),
            path: PathBuf::from("/workspace"),
        },
    ]
}

/// Caches, browser runtime files and restore leftovers never go into a snapshot.
pub fn excludes() -> Excludes {
    let own = |v: &[&str]| v.iter().map(|s| (*s).to_owned()).collect();
    Excludes {
        prefixes: own(&EXCLUDED_PREFIXES),
        names: own(&EXCLUDED_NAMES),
        name_prefixes: own(&EXCLUDED_NAME_PREFIXES),
        name_suffixes: own(&EXCLUDED_NAME_SUFFIXES),
    }
}

/// After a restore Chrome follows the restored profile's record of the save, otherwise the close.
fn chrome_start(closed: bool, restored: bool) -> Option<serde_json::Value> {
    if restored {
        Some(json!({"if_was_running": true}))
    } else {
        closed.then(|| json!({}))
    }
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

async fn read_cached(cache: &Path, id: &str) -> Result<Vec<u8>, String> {
    tokio::fs::read(cache.join(id))
        .await
        .map_err(|e| format!("read cached chunk {id}: {e}"))
}

fn retryable(status: reqwest::StatusCode) -> bool {
    status.is_server_error()
        || status == reqwest::StatusCode::REQUEST_TIMEOUT
        || status == reqwest::StatusCode::TOO_MANY_REQUESTS
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn resume_returns_before_chrome_is_ready() {
        let dir = tempfile::tempdir().unwrap();
        let socket = dir.path().join("toolhost.sock");
        let listener = tokio::net::UnixListener::bind(&socket).unwrap();
        let (seen, mut calls) = mpsc::channel(4);
        tokio::spawn(async move {
            while let Ok((mut s, _)) = listener.accept().await {
                let call: Call = rpc::read_frame(&mut s).await.unwrap();
                seen.send(call.args).await.unwrap();
                tokio::time::sleep(Duration::from_secs(1)).await;
                let out = rpc::Output::error("chrome did not expose CDP");
                rpc::write_frame(&mut s, &out).await.unwrap();
            }
        });
        let turns = Arc::new(RwLock::new(()));
        let engine = StateEngine::new(
            rpc::Client::new(socket),
            Store::open(&dir.path().join("agent.db")).unwrap(),
            default_roots(),
            dir.path().join("work"),
            Arc::clone(&turns),
            None,
        );
        let gate = Arc::clone(&turns).write_owned().await;
        let started = tokio::time::Instant::now();
        engine.resume(
            Quiet {
                _gate: gate,
                chrome: true,
            },
            true,
        );
        assert!(started.elapsed() < Duration::from_millis(200));
        assert!(turns.try_write().is_ok(), "turns run again at once");
        let args = tokio::time::timeout(Duration::from_secs(5), calls.recv())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(args, json!({"if_was_running": true}));
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
}
