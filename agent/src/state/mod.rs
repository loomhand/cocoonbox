//! The state layer: snapshots of the user's home, /workspace and agent.db as encrypted chunks.

pub mod crypto;
pub mod manifest;

use std::collections::{BTreeSet, HashMap, HashSet};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use fastcdc::v2020::FastCDC;
use futures_util::StreamExt;
use futures_util::stream::{self, FuturesUnordered};
use serde::{Deserialize, Serialize};
use serde_json::json;
use tokio::io::AsyncReadExt;
use tokio::sync::{Mutex, OwnedRwLockWriteGuard, RwLock, mpsc};

use crate::pb;
use crate::store::Store;
use crate::toolhost::rpc::{self, Call, FileReader};
use crate::toolhost::state::{Entry, Excludes, Kind, Root};
use crypto::Keys;
use manifest::{Blob, FileRecord, Manifest};

const CHUNK_MIN: u32 = 256 << 10;
const CHUNK_AVG: u32 = 1 << 20;
const CHUNK_MAX: u32 = 4 << 20;
const MANIFEST_LIMIT: u64 = 256 << 20;
const TRANSFERS: usize = 8;
const RESTORE_WRITES: usize = 4;
const ATTEMPTS: u32 = 3;
const RETRY_BASE: Duration = Duration::from_millis(500);
const QUIESCE_WAIT: Duration = Duration::from_secs(30);
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
    /// Creates an engine over `roots`, keeping its spool under `work`; `turns` pauses the loop.
    pub fn new(
        toolhost: rpc::Client,
        store: Store,
        roots: Vec<Root>,
        work: PathBuf,
        turns: Arc<RwLock<()>>,
    ) -> Self {
        Self {
            toolhost,
            store,
            roots,
            excludes: excludes(),
            work,
            turns,
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
            self.resume(q).await;
        }
        let (snapshot, response) = result?;
        *pinned = Some(snapshot);
        Ok(response)
    }

    /// Uploads the listed chunks and the manifest of the pinned snapshot.
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
        let manifest = req
            .manifest
            .ok_or_else(|| StateError::Invalid("missing manifest url".to_owned()))?;
        let mut jobs = Vec::with_capacity(req.puts.len() + 1);
        for put in req.puts {
            if !snap.chunks.contains(&put.chunk_id) {
                return Err(StateError::Invalid(format!(
                    "chunk {} is not part of snapshot {}",
                    put.chunk_id, snap.snapshot_id
                )));
            }
            let path = snap.spool.join(&put.chunk_id);
            if !path.exists() {
                return Err(StateError::Invalid(format!(
                    "chunk {} was not spooled because the last upload held it; plan without a parent",
                    put.chunk_id
                )));
            }
            jobs.push((put, path));
        }
        jobs.push((manifest, snap.spool.join(MANIFEST_FILE)));
        let mut total = 0;
        for (_, path) in &jobs {
            total += tokio::fs::metadata(path)
                .await
                .map_err(|e| format!("stat {}: {e}", path.display()))?
                .len();
        }
        let mut meter = Meter::new(total, jobs.len(), progress);
        let manifest_job = jobs.pop();
        let mut puts = stream::iter(jobs.into_iter().map(|(put, path)| self.put(put, path)))
            .buffer_unordered(TRANSFERS);
        while let Some(done) = puts.next().await {
            meter.add(done?).await;
        }
        if let Some((put, path)) = manifest_job {
            meter.add(self.put(put, path).await?).await;
        }
        self.save_known(&snap.key_id, &snap.chunks).await?;
        let _ = tokio::fs::remove_dir_all(&snap.spool).await;
        let id = snap.snapshot_id.clone();
        *pinned = None;
        meter.finish(&id).await;
        Ok(())
    }

    /// Replaces the user state with the manifest's; the loop's writers pause for the swap.
    pub async fn restore(
        &self,
        req: pb::RestoreRequest,
        progress: mpsc::Sender<pb::TransferProgress>,
    ) -> Result<(), StateError> {
        let _one = self.pinned.lock().await;
        let key = req
            .key
            .ok_or_else(|| StateError::Invalid("missing key".to_owned()))?;
        let keys = Arc::new(Keys::derive(&key.key).map_err(StateError::Invalid)?);
        let get = req
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
        let urls: HashMap<String, String> =
            req.gets.into_iter().map(|g| (g.chunk_id, g.url)).collect();
        let needed = referenced(&manifest);
        if let Some(missing) = needed.iter().find(|id| !urls.contains_key(*id)) {
            return Err(StateError::Invalid(format!("no url for chunk {missing}")));
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
                &needed,
                &urls,
                &cache,
                progress,
                &get.chunk_id,
            )
            .await;
        let _ = tokio::fs::remove_dir_all(&cache).await;
        result
    }

    #[allow(clippy::too_many_arguments)]
    async fn restore_from(
        &self,
        keys: &Arc<Keys>,
        manifest: &Manifest,
        needed: &BTreeSet<String>,
        urls: &HashMap<String, String>,
        cache: &Path,
        progress: mpsc::Sender<pb::TransferProgress>,
        manifest_id: &str,
    ) -> Result<(), StateError> {
        let mut meter = Meter::new(0, needed.len(), progress);
        let mut fetches = FuturesUnordered::new();
        for id in needed {
            let url = urls.get(id).cloned().unwrap_or_default();
            fetches.push(self.fetch_chunk(Arc::clone(keys), id.clone(), url, cache.join(id)));
            if fetches.len() >= TRANSFERS
                && let Some(done) = fetches.next().await
            {
                meter.add(done?).await;
            }
        }
        while let Some(done) = fetches.next().await {
            meter.add(done?).await;
        }
        let quiet = self.quiesce().await?;
        let result = self.rebuild(manifest, cache).await;
        self.resume(quiet).await;
        result?;
        meter.finish(manifest_id).await;
        Ok(())
    }

    async fn rebuild(&self, manifest: &Manifest, cache: &Path) -> Result<(), StateError> {
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
            let mut links = Vec::new();
            for r in &records {
                let path = staging.join(&r.entry.path);
                match r.entry.kind {
                    Kind::Dir => dirs.push(json!({"path": path, "mode": r.entry.mode})),
                    Kind::Symlink => links.push(json!({
                        "path": path,
                        "target": r.entry.target.clone().unwrap_or_default(),
                    })),
                    Kind::File => {}
                }
            }
            self.tool("state_apply", json!({"dirs": dirs, "symlinks": []}))
                .await?;
            let mut writes = FuturesUnordered::new();
            for r in &records {
                if r.entry.kind != Kind::File {
                    continue;
                }
                writes.push(self.write_record(&staging, r, cache));
                if writes.len() >= RESTORE_WRITES
                    && let Some(done) = writes.next().await
                {
                    done?;
                }
            }
            while let Some(done) = writes.next().await {
                done?;
            }
            self.tool("state_apply", json!({"dirs": [], "symlinks": links}))
                .await?;
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
            self.tool(
                "state_swap",
                json!({"root": root.path, "staging": staging, "names": names, "keep": keep}),
            )
            .await?;
        }
        if let Some(db) = &manifest.agent_db {
            let path = cache.join("agent.db");
            let mut out = Vec::with_capacity(usize::try_from(db.size).unwrap_or(0));
            for id in &db.chunks {
                out.extend(read_cached(cache, id).await?);
            }
            tokio::fs::write(&path, &out)
                .await
                .map_err(|e| format!("write {}: {e}", path.display()))?;
            self.store.restore_from(path).await?;
        }
        Ok(())
    }

    async fn write_record(
        &self,
        staging: &Path,
        r: &FileRecord,
        cache: &Path,
    ) -> Result<(), String> {
        let path = staging.join(&r.entry.path);
        let mut w = self
            .toolhost
            .write_file(&path, r.entry.mode, r.entry.mtime_ms)
            .await?;
        for id in &r.chunks {
            w.send(&read_cached(cache, id).await?).await?;
        }
        w.finish().await
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
        let known = self.load_known(key_id).await;
        let mut chunker = Chunker {
            keys: Arc::clone(keys),
            spool: spool.clone(),
            known: &known,
            sizes: HashMap::new(),
        };
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
        for entry in entries {
            let key = format!("{}/{}", entry.root, entry.path);
            if SQLITE_SIDECARS.iter().any(|s| {
                key.strip_suffix(s)
                    .is_some_and(|base| backed_up.contains(base))
            }) {
                continue;
            }
            if entry.kind != Kind::File {
                files.push(FileRecord {
                    entry,
                    chunks: Vec::new(),
                });
                continue;
            }
            let Some(root) = roots.get(entry.root.as_str()) else {
                continue;
            };
            let path = root.join(&entry.path);
            match chunker.remote(&self.toolhost, &path).await? {
                Some((chunks, backup)) => {
                    if backup {
                        backed_up.insert(key);
                    }
                    files.push(FileRecord { entry, chunks });
                }
                None => skipped.push(key),
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
        let chunks = ids
            .iter()
            .map(|id| pb::ChunkRef {
                id: id.clone(),
                size: chunker.sizes.get(id).copied().unwrap_or(0),
            })
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
            key_id: key_id.to_owned(),
            spool,
            chunks: ids.into_iter().collect(),
        };
        Ok((pinned, response))
    }

    async fn quiesce(&self) -> Result<Quiet, StateError> {
        let gate = tokio::time::timeout(QUIESCE_WAIT, Arc::clone(&self.turns).write_owned())
            .await
            .map_err(|_| StateError::Busy("a turn is still running; retry later".to_owned()))?;
        let closed = self.tool("browser_close", json!({})).await?;
        Ok(Quiet {
            _gate: gate,
            chrome: closed == "closed",
        })
    }

    async fn resume(&self, quiet: Quiet) {
        if quiet.chrome
            && let Err(e) = self.tool("browser_start", json!({})).await
        {
            eprintln!("state: restart chrome: {e}");
        }
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
        keys: Arc<Keys>,
        id: String,
        url: String,
        dest: PathBuf,
    ) -> Result<u64, String> {
        let sealed = self
            .get(&url)
            .await
            .map_err(|e| format!("chunk {id}: {e}"))?;
        let size = sealed.len() as u64;
        let plain =
            tokio::task::spawn_blocking(move || keys.open(&id, &sealed, u64::from(CHUNK_MAX)))
                .await
                .map_err(|e| format!("open task: {e}"))??;
        tokio::fs::write(&dest, plain)
            .await
            .map_err(|e| format!("write {}: {e}", dest.display()))?;
        Ok(size)
    }

    async fn load_known(&self, key_id: &str) -> HashSet<String> {
        let Ok(raw) = tokio::fs::read(self.work.join(KNOWN_FILE)).await else {
            return HashSet::new();
        };
        match serde_json::from_slice::<Known>(&raw) {
            Ok(k) if k.key_id == key_id => k.chunks,
            _ => HashSet::new(),
        }
    }

    async fn save_known(&self, key_id: &str, chunks: &HashSet<String>) -> Result<(), String> {
        let known = Known {
            key_id: key_id.to_owned(),
            chunks: chunks.clone(),
        };
        let raw = serde_json::to_vec(&known).map_err(|e| format!("encode known chunks: {e}"))?;
        tokio::fs::write(self.work.join(KNOWN_FILE), raw)
            .await
            .map_err(|e| format!("write known chunks: {e}"))
    }
}

/// The snapshot waiting for Upload.
struct Pinned {
    snapshot_id: String,
    key_id: String,
    spool: PathBuf,
    chunks: HashSet<String>,
}

/// Chunk ids the store held after the last upload under one key.
#[derive(Deserialize, Serialize)]
struct Known {
    key_id: String,
    chunks: HashSet<String>,
}

/// Turns are paused and Chrome is closed while this lives.
struct Quiet {
    _gate: OwnedRwLockWriteGuard<()>,
    chrome: bool,
}

/// Splits byte streams into content-defined chunks and spools the ones the store lacks.
struct Chunker<'a> {
    keys: Arc<Keys>,
    spool: PathBuf,
    known: &'a HashSet<String>,
    sizes: HashMap<String, u64>,
}

impl Chunker<'_> {
    /// Chunks a file read by the toolhost; `None` when it changed during two reads.
    async fn remote(
        &mut self,
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
    async fn local(&mut self, path: &Path) -> Result<(u64, Vec<String>), String> {
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

    async fn feed(&mut self, mut source: Source<'_>) -> Result<Vec<String>, String> {
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

    async fn emit(&mut self, chunk: Vec<u8>) -> Result<String, String> {
        let keys = Arc::clone(&self.keys);
        let len = chunk.len() as u64;
        let (id, chunk) =
            tokio::task::spawn_blocking(move || keys.id(&chunk).map(|id| (id, chunk)))
                .await
                .map_err(|e| format!("id task: {e}"))??;
        if self.sizes.contains_key(&id) {
            return Ok(id);
        }
        self.sizes.insert(id.clone(), len);
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

fn referenced(m: &Manifest) -> BTreeSet<String> {
    m.files
        .iter()
        .flat_map(|f| f.chunks.iter())
        .chain(m.agent_db.iter().flat_map(|b| b.chunks.iter()))
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
