//! A throwaway farfield fleet for integration tests: the real Go services,
//! built from this checkout and started on ephemeral loopback ports with
//! fresh data directories — the same environment scripts/devfleet.sh gives
//! `make dev`, so these tests exercise exactly what ships.

#![allow(dead_code)]

use farfield_core::profile::{Endpoint, Profile};
use farfield_core::secret::{Credential, MemoryStore, SecretStore};
use farfield_core::Session;
use std::collections::{BTreeMap, HashMap};
use std::net::TcpListener;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

pub const PASSWORD: &str = "demo";

pub fn repo() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../..").canonicalize().unwrap()
}

/// Build each app the tests use, once per test process (go caches; it is quick).
fn bin_dir() -> &'static PathBuf {
    static DIR: OnceLock<PathBuf> = OnceLock::new();
    DIR.get_or_init(|| repo().join("clients/target/fleet-bin"))
}

fn build(apps: &[&str]) {
    static BUILT: OnceLock<Mutex<std::collections::HashSet<String>>> = OnceLock::new();
    let built = BUILT.get_or_init(Default::default);
    let mut built = built.lock().unwrap();
    let dir = bin_dir();
    std::fs::create_dir_all(dir).unwrap();
    {
        for app in apps.iter().copied().filter(|a| !built.contains(*a)).collect::<Vec<_>>() {
            let st = Command::new("go")
                .current_dir(repo())
                .args(["build", "-o"])
                .arg(dir.join(app))
                .arg(format!("./apps/{app}"))
                .status()
                .expect("go build");
            assert!(st.success(), "go build ./apps/{app} failed");
            built.insert(app.to_string());
        }
    }
}

fn free_port() -> u16 {
    TcpListener::bind("127.0.0.1:0").unwrap().local_addr().unwrap().port()
}

pub struct Fleet {
    pub data: tempfile::TempDir,
    pub ports: HashMap<String, u16>,
    procs: Mutex<HashMap<String, Child>>,
    env: HashMap<String, Vec<(String, String)>>,
}

impl Drop for Fleet {
    fn drop(&mut self) {
        for (_, mut c) in self.procs.lock().unwrap().drain() {
            let _ = c.kill();
            let _ = c.wait();
        }
    }
}

pub fn key(app: &str) -> String {
    format!("test-{app}-write-key")
}
pub fn read_key(app: &str) -> String {
    format!("test-{app}-read-key")
}

impl Fleet {
    /// Start the named apps (blobs is added when anything depends on it).
    pub fn start(apps: &[&str]) -> Fleet {
        let bins = bin_dir();
        let data = tempfile::tempdir().unwrap();
        let mut want: Vec<&str> = apps.to_vec();
        if want.iter().any(|a| matches!(*a, "feed" | "content" | "library")) && !want.contains(&"blobs") {
            want.insert(0, "blobs");
        }
        build(&want);
        let ports: HashMap<String, u16> = want.iter().map(|a| (a.to_string(), free_port())).collect();
        let d = data.path();
        let blobs_url = ports.get("blobs").map(|p| format!("http://127.0.0.1:{p}")).unwrap_or_default();
        let content_url = ports.get("content").map(|p| format!("http://127.0.0.1:{p}")).unwrap_or_default();
        let mut env = HashMap::new();
        for app in &want {
            let up = app.to_uppercase();
            let port = ports[*app];
            let vars: Vec<(String, String)> = vec![
                ("HOST".into(), "127.0.0.1".into()),
                ("PASSWORD".into(), PASSWORD.into()),
                ("COOKIE_SECURE".into(), "false".into()),
                ("FARFIELD_FLEET".into(), "local".into()),
                ("SESSION_SECRET".into(), "test-fleet-secret".into()),
                (format!("{up}_PORT"), port.to_string()),
                (format!("{up}_DB_PATH"), d.join(format!("{app}.sqlite")).display().to_string()),
                (format!("{up}_API_KEY"), key(app)),
                (format!("{up}_READ_KEY"), read_key(app)),
                ("KEYS_DB_PATH".into(), d.join("keys.sqlite").display().to_string()),
                ("BLOBS_BACKEND".into(), "local".into()),
                ("BLOBS_DIR".into(), d.join("blobs-data").display().to_string()),
                ("BLOBS_SPOOL_DIR".into(), d.join("blob-spool").display().to_string()),
                ("SIDELOAD_DIR".into(), d.join("sideload").display().to_string()),
                ("LIBRARY_TUS_DIR".into(), d.join("tus").display().to_string()),
                ("BLOBS_URL".into(), blobs_url.clone()),
                ("BLOBS_API_KEY".into(), key("blobs")),
                ("BLOBS_PUBLIC_URL".into(), blobs_url.clone()),
                ("CONTENT_URL".into(), content_url.clone()),
                ("CONTENT_API_KEY".into(), key("content")),
                ("CONTENT_PUBLIC_URL".into(), content_url.clone()),
                ("PULSE_READ_KEY".into(), read_key("pulse")),
            ];
            env.insert(app.to_string(), vars);
        }
        let fleet = Fleet { data, ports, procs: Mutex::new(HashMap::new()), env };
        for app in &want {
            fleet.spawn(app, bins);
        }
        for app in &want {
            fleet.wait_up(app);
        }
        fleet
    }

    fn spawn(&self, app: &str, bins: &Path) {
        let log = std::fs::File::create(self.data.path().join(format!("{app}.log"))).unwrap();
        let child = Command::new(bins.join(app))
            .arg("serve")
            .env_clear()
            .env("PATH", std::env::var("PATH").unwrap_or_default())
            .env("HOME", self.data.path())
            .envs(self.env[app].iter().cloned())
            .stdout(Stdio::from(log.try_clone().unwrap()))
            .stderr(Stdio::from(log))
            .spawn()
            .unwrap_or_else(|e| panic!("start {app}: {e}"));
        self.procs.lock().unwrap().insert(app.into(), child);
    }

    pub fn url(&self, app: &str) -> String {
        format!("http://127.0.0.1:{}", self.ports[app])
    }

    fn wait_up(&self, app: &str) {
        let deadline = Instant::now() + Duration::from_secs(20);
        let url = format!("{}/status", self.url(app));
        while Instant::now() < deadline {
            if let Ok(out) = Command::new("curl").args(["-sf", "-m", "1", &url]).output() {
                if out.status.success() && String::from_utf8_lossy(&out.stdout).contains("\"ok\":true") {
                    return;
                }
            }
            std::thread::sleep(Duration::from_millis(100));
        }
        let log = std::fs::read_to_string(self.data.path().join(format!("{app}.log"))).unwrap_or_default();
        panic!("{app} did not come up:\n{log}");
    }

    /// Stop a service (an outage).
    pub fn stop(&self, app: &str) {
        if let Some(mut c) = self.procs.lock().unwrap().remove(app) {
            let _ = c.kill();
            let _ = c.wait();
        }
    }

    /// Bring it back on the same port and data.
    pub fn restart(&self, app: &str) {
        self.stop(app);
        self.spawn(app, bin_dir());
        self.wait_up(app);
    }

    /// A profile pointing at this fleet.
    pub fn profile(&self) -> Profile {
        let endpoints: BTreeMap<String, Endpoint> = self
            .ports
            .iter()
            .map(|(a, p)| (a.clone(), Endpoint { api: format!("http://127.0.0.1:{p}"), public: Some(format!("http://127.0.0.1:{p}")) }))
            .collect();
        Profile { id: "test".into(), name: "test fleet".into(), endpoints }
    }

    /// A session holding the given credential per app (None = no key).
    pub fn session_with(&self, creds: &[(&str, &str)]) -> (Session, Arc<MemoryStore>) {
        let store = Arc::new(MemoryStore::default());
        let s = Session::new(self.profile(), self.data.path().join("client"), store.clone() as Arc<dyn SecretStore>);
        for (app, k) in creds {
            s.set_credential(app, &Credential::new(*k).unwrap()).unwrap();
        }
        (s, store)
    }

    /// A session with the write key for every running app.
    pub fn admin(&self) -> Session {
        let creds: Vec<(String, String)> = self.ports.keys().map(|a| (a.clone(), key(a))).collect();
        let refs: Vec<(&str, &str)> = creds.iter().map(|(a, k)| (a.as_str(), k.as_str())).collect();
        self.session_with(&refs).0
    }

    /// Mint an ffk_ key through the keys console, the way a person does:
    /// sign in, submit the form, read the token off the one page that shows
    /// it. Returns (token, key id).
    pub fn mint(&self, name: &str, app: &str, scope: &str) -> (String, String) {
        let keys = self.url("keys");
        let jar = self.data.path().join("cookies.txt");
        let ok = Command::new("curl")
            .args(["-s", "-o", "/dev/null", "-c"])
            .arg(&jar)
            .args(["-d", &format!("password={PASSWORD}"), &format!("{keys}/login")])
            .status()
            .unwrap()
            .success();
        assert!(ok, "keys login");
        let out = Command::new("curl")
            .args(["-s", "-b"])
            .arg(&jar)
            .args(["-d", &format!("name={name}&app={app}&scope={scope}"), &format!("{keys}/keys")])
            .output()
            .unwrap();
        let html = String::from_utf8_lossy(&out.stdout);
        let token = between(&html, "<code class=\"token\">", "</code>").unwrap_or_else(|| panic!("no token in {html}"));
        let id = between(&html, "<a href=\"/keys/", "\"").expect("key id");
        (token, id)
    }

    pub fn revoke(&self, id: &str) {
        let keys = self.url("keys");
        let jar = self.data.path().join("cookies.txt");
        let ok = Command::new("curl")
            .args(["-s", "-o", "/dev/null", "-b"])
            .arg(&jar)
            .args(["-X", "POST", &format!("{keys}/keys/{id}/revoke")])
            .status()
            .unwrap()
            .success();
        assert!(ok, "revoke");
    }
}

fn between(s: &str, a: &str, b: &str) -> Option<String> {
    let i = s.find(a)? + a.len();
    let j = s[i..].find(b)? + i;
    Some(s[i..j].trim().to_string())
}

/// Run a future on the client's runtime from a plain #[test].
pub fn block<F: std::future::Future>(f: F) -> F::Output {
    farfield_core::runtime().block_on(f)
}
