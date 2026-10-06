//! Local state on disk: the response cache and recoverable drafts.
//!
//! Layout under the data directory (0700, files 0600):
//!
//! ```text
//! profiles.json
//! p/<profile>/<identity>/cache/<service>/<sha>.json   revalidated by ETag
//! p/<profile>/<identity>/drafts/<service>/<key>.json  local work, never evicted
//! ```
//!
//! `<identity>` is a hash of the endpoint origin and the credential's
//! fingerprint, so a response fetched with one key is never served to a
//! session holding another, and switching profile cannot cross-read either.
//! Every write is atomic (temp file, fsync, rename): a crash mid-save leaves
//! the previous version, never a torn one.

use parking_lot::Mutex;
use serde::{de::DeserializeOwned, Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::collections::HashMap;
use std::fs;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};

pub fn now_ms() -> u64 {
    SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_millis() as u64).unwrap_or(0)
}

fn private_dir(p: &Path) -> std::io::Result<()> {
    fs::create_dir_all(p)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(p, fs::Permissions::from_mode(0o700))?;
    }
    Ok(())
}

/// Write `bytes` to `path` atomically, readable only by this user.
pub fn write_atomic(path: &Path, bytes: &[u8]) -> std::io::Result<()> {
    let dir = path.parent().expect("path has a parent");
    private_dir(dir)?;
    let tmp = dir.join(format!(".{}.{}.tmp", path.file_name().unwrap().to_string_lossy(), std::process::id()));
    {
        let mut f = fs::OpenOptions::new().write(true).create(true).truncate(true).open(&tmp)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            f.set_permissions(fs::Permissions::from_mode(0o600))?;
        }
        f.write_all(bytes)?;
        f.sync_all()?;
    }
    fs::rename(&tmp, path)?;
    // make the rename itself durable
    if let Ok(d) = fs::File::open(dir) {
        let _ = d.sync_all();
    }
    Ok(())
}

fn hash(s: &str) -> String {
    hex::encode(&Sha256::digest(s.as_bytes())[..12])
}

/// A file-safe key: readable when it already is, hashed otherwise.
fn safe(s: &str) -> String {
    if !s.is_empty() && s.len() <= 80 && s.chars().all(|c| c.is_ascii_alphanumeric() || c == '-' || c == '_') {
        s.to_string()
    } else {
        hash(s)
    }
}

/// The root of one profile+identity's state.
#[derive(Clone, Debug)]
pub struct Scope {
    root: PathBuf,
}

impl Scope {
    pub fn new(data_dir: &Path, profile: &str, identity: &str) -> Self {
        Scope { root: data_dir.join("p").join(safe(profile)).join(hash(identity)) }
    }
    pub fn root(&self) -> &Path {
        &self.root
    }
}

// ── cache ────────────────────────────────────────────────────────────────

#[derive(Serialize, Deserialize, Clone, Debug)]
pub struct CacheEntry {
    pub etag: Option<String>,
    pub fetched_ms: u64,
    pub body: serde_json::Value,
}

/// A bounded response cache: a small in-memory LRU in front of per-identity
/// files, with total disk size capped (oldest evicted first).
/// Entries by path with their last-use tick, and the tick counter.
type MemCache = (HashMap<PathBuf, (u64, CacheEntry)>, u64);

pub struct Cache {
    mem: Mutex<MemCache>,
    mem_cap: usize,
    disk_cap: u64,
}

impl Cache {
    pub fn new(mem_cap: usize, disk_cap: u64) -> Self {
        Cache { mem: Mutex::new((HashMap::new(), 0)), mem_cap, disk_cap }
    }

    fn path(scope: &Scope, service: &str, key: &str) -> PathBuf {
        scope.root.join("cache").join(safe(service)).join(format!("{}.json", hash(key)))
    }

    pub fn get(&self, scope: &Scope, service: &str, key: &str) -> Option<CacheEntry> {
        let p = Self::path(scope, service, key);
        {
            let mut g = self.mem.lock();
            g.1 += 1;
            let tick = g.1;
            if let Some(e) = g.0.get_mut(&p) {
                e.0 = tick;
                return Some(e.1.clone());
            }
        }
        let e: CacheEntry = serde_json::from_slice(&fs::read(&p).ok()?).ok()?;
        self.remember(p, e.clone());
        Some(e)
    }

    pub fn put(&self, scope: &Scope, service: &str, key: &str, entry: CacheEntry) {
        let p = Self::path(scope, service, key);
        if let Ok(b) = serde_json::to_vec(&entry) {
            let _ = write_atomic(&p, &b);
        }
        self.remember(p, entry);
        self.evict_disk(scope);
    }

    /// Mark an entry fresh again after a 304.
    pub fn touch(&self, scope: &Scope, service: &str, key: &str) {
        if let Some(mut e) = self.get(scope, service, key) {
            e.fetched_ms = now_ms();
            self.put(scope, service, key, e);
        }
    }

    pub fn forget(&self, scope: &Scope, service: &str, key: &str) {
        let p = Self::path(scope, service, key);
        self.mem.lock().0.remove(&p);
        let _ = fs::remove_file(p);
    }

    fn remember(&self, p: PathBuf, e: CacheEntry) {
        let mut g = self.mem.lock();
        g.1 += 1;
        let tick = g.1;
        g.0.insert(p, (tick, e));
        while g.0.len() > self.mem_cap {
            let oldest = g.0.iter().min_by_key(|(_, (t, _))| *t).map(|(k, _)| k.clone());
            match oldest {
                Some(k) => {
                    g.0.remove(&k);
                }
                None => break,
            }
        }
    }

    fn evict_disk(&self, scope: &Scope) {
        let dir = scope.root.join("cache");
        let mut files = Vec::new();
        let mut total = 0u64;
        for svc in fs::read_dir(&dir).into_iter().flatten().flatten() {
            for f in fs::read_dir(svc.path()).into_iter().flatten().flatten() {
                if let Ok(m) = f.metadata() {
                    total += m.len();
                    files.push((m.modified().unwrap_or(UNIX_EPOCH), m.len(), f.path()));
                }
            }
        }
        if total <= self.disk_cap {
            return;
        }
        files.sort();
        for (_, len, p) in files {
            if total <= self.disk_cap * 9 / 10 {
                break;
            }
            let _ = fs::remove_file(&p);
            self.mem.lock().0.remove(&p);
            total -= len;
        }
    }
}

// ── drafts ───────────────────────────────────────────────────────────────

/// Where a draft stands relative to the server.
#[derive(Serialize, Deserialize, Clone, Copy, Debug, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub enum SaveState {
    /// Only on this Mac; the server has not seen these edits.
    Local,
    /// A save is in flight or its outcome was lost — must be reconciled
    /// before anything is retried.
    Pending,
    /// The server has exactly this (`base` is what it returned).
    Saved,
    /// The server changed underneath; `remote` holds its copy.
    Conflict,
}

/// One unit of local work: the record as last seen on the server (`base`,
/// with its ETag), and the user's version (`local`). Kept until the user
/// saves or discards it — never evicted.
#[derive(Serialize, Deserialize, Clone, Debug, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct Draft {
    pub service: String,
    pub key: String,
    pub base: Option<serde_json::Value>,
    pub base_etag: Option<String>,
    pub local: serde_json::Value,
    pub state: SaveState,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub remote: Option<serde_json::Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub remote_etag: Option<String>,
    pub updated_ms: u64,
}

pub struct Drafts {
    scope: Scope,
}

impl Drafts {
    pub fn new(scope: Scope) -> Self {
        Drafts { scope }
    }
    fn path(&self, service: &str, key: &str) -> PathBuf {
        self.scope.root.join("drafts").join(safe(service)).join(format!("{}.json", safe(key)))
    }
    pub fn save(&self, d: &Draft) -> std::io::Result<()> {
        write_atomic(&self.path(&d.service, &d.key), &serde_json::to_vec_pretty(d).map_err(std::io::Error::other)?)
    }
    pub fn load(&self, service: &str, key: &str) -> Option<Draft> {
        serde_json::from_slice(&fs::read(self.path(service, key)).ok()?).ok()
    }
    pub fn discard(&self, service: &str, key: &str) -> std::io::Result<()> {
        match fs::remove_file(self.path(service, key)) {
            Err(e) if e.kind() != std::io::ErrorKind::NotFound => Err(e),
            _ => Ok(()),
        }
    }
    /// Every draft for a service, newest first — what "recover" offers.
    pub fn list(&self, service: &str) -> Vec<Draft> {
        let dir = self.scope.root.join("drafts").join(safe(service));
        let mut out: Vec<Draft> = fs::read_dir(dir)
            .into_iter()
            .flatten()
            .flatten()
            .filter(|f| f.path().extension().is_some_and(|e| e == "json"))
            .filter_map(|f| serde_json::from_slice(&fs::read(f.path()).ok()?).ok())
            .collect();
        out.sort_by_key(|d: &Draft| std::cmp::Reverse(d.updated_ms));
        out
    }
    /// Re-key a draft (a new record got its server slug).
    pub fn rename(&self, service: &str, from: &str, to: &str) -> std::io::Result<()> {
        if let Some(mut d) = self.load(service, from) {
            d.key = to.into();
            self.save(&d)?;
            self.discard(service, from)?;
        }
        Ok(())
    }
}

/// Generic JSON file (profiles, preferences) with atomic replace.
pub fn read_json<T: DeserializeOwned>(path: &Path) -> Option<T> {
    serde_json::from_slice(&fs::read(path).ok()?).ok()
}

pub fn write_json<T: Serialize>(path: &Path, v: &T) -> std::io::Result<()> {
    write_atomic(path, &serde_json::to_vec_pretty(v).map_err(std::io::Error::other)?)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn identities_do_not_share_cache() {
        let dir = tempfile::tempdir().unwrap();
        let cache = Cache::new(8, 1 << 20);
        let a = Scope::new(dir.path(), "home", "https://h:1#aaaa");
        let b = Scope::new(dir.path(), "home", "https://h:1#bbbb");
        cache.put(&a, "bookmarks", "/api/admin/bookmarks", CacheEntry { etag: None, fetched_ms: 1, body: json!(1) });
        assert!(cache.get(&a, "bookmarks", "/api/admin/bookmarks").is_some());
        assert!(cache.get(&b, "bookmarks", "/api/admin/bookmarks").is_none());
        let other = Scope::new(dir.path(), "local", "https://h:1#aaaa");
        assert!(cache.get(&other, "bookmarks", "/api/admin/bookmarks").is_none());
    }

    #[test]
    fn cache_is_bounded() {
        let dir = tempfile::tempdir().unwrap();
        let cache = Cache::new(4, 2_000);
        let s = Scope::new(dir.path(), "p", "i");
        for i in 0..50 {
            cache.put(
                &s,
                "x",
                &format!("k{i}"),
                CacheEntry { etag: None, fetched_ms: i, body: json!("y".repeat(100)) },
            );
        }
        assert!(cache.mem.lock().0.len() <= 4);
        let total: u64 =
            fs::read_dir(s.root().join("cache/x")).unwrap().flatten().map(|f| f.metadata().unwrap().len()).sum();
        assert!(total <= 2_000, "disk {total}");
    }

    #[test]
    fn drafts_survive_and_are_private() {
        let dir = tempfile::tempdir().unwrap();
        let s = Scope::new(dir.path(), "p", "i");
        let d = Drafts::new(s.clone());
        let draft = Draft {
            service: "content".into(),
            key: "123-hello".into(),
            base: None,
            base_etag: None,
            local: json!({"title": "Hello", "body": "![](blob://bafk)"}),
            state: SaveState::Local,
            remote: None,
            remote_etag: None,
            updated_ms: now_ms(),
        };
        d.save(&draft).unwrap();
        // a fresh handle (a relaunch) finds it
        let again = Drafts::new(Scope::new(dir.path(), "p", "i"));
        assert_eq!(again.load("content", "123-hello"), Some(draft));
        assert_eq!(again.list("content").len(), 1);
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let f = s.root().join("drafts/content/123-hello.json");
            assert_eq!(fs::metadata(&f).unwrap().permissions().mode() & 0o777, 0o600);
            assert_eq!(fs::metadata(f.parent().unwrap()).unwrap().permissions().mode() & 0o777, 0o700);
        }
        // no temp files left behind
        let leftovers: Vec<_> = fs::read_dir(s.root().join("drafts/content"))
            .unwrap()
            .flatten()
            .filter(|f| f.file_name().to_string_lossy().ends_with(".tmp"))
            .collect();
        assert!(leftovers.is_empty());
    }
}
