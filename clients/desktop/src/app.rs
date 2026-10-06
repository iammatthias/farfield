//! Application state: the active profile and session, service health,
//! preferences, notifications, and a structured event log.

use crate::theme::Mode;
use farfield_core::profile::Profile;
use farfield_core::secret::{PlatformStore, SecretStore};
use farfield_core::store::{read_json, write_json};
use farfield_core::{ApiError, Session};
use gpui::{App, Global, SharedString};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::io::Write;
use std::path::PathBuf;
use std::sync::Arc;

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(default)]
pub struct Prefs {
    pub mode: Mode,
    pub reduced_motion: bool,
    pub nav_width: f32,
    pub inspector_width: f32,
    pub inspector_open: bool,
    pub nav_open: bool,
    pub workspace: String,
    pub profile: String,
    /// First-run setup has been completed (or skipped).
    pub onboarded: bool,
}

impl Default for Prefs {
    fn default() -> Self {
        Prefs {
            mode: Mode::System,
            reduced_motion: false,
            nav_width: 196.0,
            inspector_width: 300.0,
            inspector_open: true,
            nav_open: true,
            workspace: "content".into(),
            profile: String::new(),
            onboarded: false,
        }
    }
}

#[derive(Clone, Debug, PartialEq)]
pub enum Health {
    Unknown,
    Up,
    /// Up, but the stored key was refused (or none is stored).
    NoAuth,
    Down(String),
}

impl Health {
    pub fn word(&self) -> &'static str {
        match self {
            Health::Unknown => "unknown",
            Health::Up => "online",
            Health::NoAuth => "needs key",
            Health::Down(_) => "offline",
        }
    }
}

#[derive(Clone, Debug)]
pub struct Toast {
    pub id: u64,
    pub text: SharedString,
    pub bad: bool,
}

pub struct AppState {
    pub data_dir: PathBuf,
    pub profiles: Vec<Profile>,
    pub session: Arc<Session>,
    pub secrets: Arc<dyn SecretStore>,
    pub prefs: Prefs,
    pub health: BTreeMap<String, Health>,
    pub toasts: Vec<Toast>,
    next_toast: u64,
}

impl Global for AppState {}

pub fn data_dir() -> PathBuf {
    if let Ok(d) = std::env::var("FARFIELD_DESKTOP_DATA") {
        return PathBuf::from(d);
    }
    directories::ProjectDirs::from("systems", "farfield", "Farfield")
        .map(|p| p.data_dir().to_path_buf())
        .unwrap_or_else(|| PathBuf::from(".farfield-desktop"))
}

impl AppState {
    pub fn load() -> Self {
        let data_dir = data_dir();
        let _ = std::fs::create_dir_all(&data_dir);
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let _ = std::fs::set_permissions(&data_dir, std::fs::Permissions::from_mode(0o700));
        }
        let prefs: Prefs = read_json(&data_dir.join("prefs.json")).unwrap_or_default();
        let mut profiles: Vec<Profile> = read_json(&data_dir.join("profiles.json")).unwrap_or_default();
        profiles.retain(|p| p.validate().is_ok());
        if !profiles.iter().any(|p| p.id == "local") {
            profiles.push(Profile::local());
        }
        // FARFIELD_PROFILE=<id> picks a profile for this launch (scripts, demos)
        let want = std::env::var("FARFIELD_PROFILE").ok().unwrap_or_else(|| prefs.profile.clone());
        let active = profiles.iter().find(|p| p.id == want).cloned().unwrap_or_else(|| profiles[0].clone());
        // FARFIELD_SECRETS=memory keeps keys in memory for this launch only,
        // seeded from FARFIELD_KEY_<SERVICE> — for scripted development runs,
        // which must never write to (or prompt for) the real Keychain.
        let memory = std::env::var("FARFIELD_SECRETS").as_deref() == Ok("memory");
        let secrets: Arc<dyn SecretStore> = if memory {
            Arc::new(farfield_core::secret::MemoryStore::default())
        } else {
            Arc::new(PlatformStore::new())
        };
        let session = Arc::new(Session::new(active, data_dir.clone(), secrets.clone()));
        if memory {
            for s in farfield_core::registry::services() {
                if let Some(k) = std::env::var(format!("FARFIELD_KEY_{}", s.name.to_uppercase()))
                    .ok()
                    .and_then(farfield_core::secret::Credential::new)
                {
                    let _ = session.set_credential(&s.name, &k);
                }
            }
        }
        let health = farfield_core::registry::services().iter().map(|s| (s.name.clone(), Health::Unknown)).collect();
        AppState { data_dir, profiles, session, secrets, prefs, health, toasts: Vec::new(), next_toast: 0 }
    }

    pub fn save_prefs(&self) {
        let _ = write_json(&self.data_dir.join("prefs.json"), &self.prefs);
    }

    pub fn save_profiles(&self) {
        let _ = write_json(&self.data_dir.join("profiles.json"), &self.profiles);
    }

    /// Switch profile: a new session (new cache and draft scope), health
    /// reset. Nothing from the previous profile is visible afterwards.
    pub fn activate(&mut self, id: &str) {
        if let Some(p) = self.profiles.iter().find(|p| p.id == id).cloned() {
            self.session = Arc::new(Session::new(p, self.data_dir.clone(), self.secrets.clone()));
            self.prefs.profile = id.into();
            for h in self.health.values_mut() {
                *h = Health::Unknown;
            }
            self.save_prefs();
            log("profile", &[("id", id)]);
        }
    }

    pub fn upsert_profile(&mut self, p: Profile) {
        match self.profiles.iter_mut().find(|x| x.id == p.id) {
            Some(x) => *x = p,
            None => self.profiles.insert(0, p),
        }
        self.save_profiles();
    }

    pub fn toast(&mut self, text: impl Into<SharedString>, bad: bool) -> u64 {
        self.next_toast += 1;
        let text = text.into();
        log("toast", &[("text", &text), ("bad", if bad { "1" } else { "0" })]);
        self.toasts.push(Toast { id: self.next_toast, text, bad });
        if self.toasts.len() > 4 {
            self.toasts.remove(0);
        }
        self.next_toast
    }
}

pub fn state(cx: &App) -> &AppState {
    cx.global::<AppState>()
}

pub fn session(cx: &App) -> Arc<Session> {
    cx.global::<AppState>().session.clone()
}

/// Show an error the way the person needs it: offline and auth problems are
/// states, not exceptions.
pub fn describe(e: &ApiError) -> String {
    match e {
        ApiError::Offline(_) => "Offline — the service can't be reached. Your work is kept on this Mac.".into(),
        ApiError::Uncertain(_) => {
            "The connection dropped mid-save. It's kept as pending and will be checked before anything is resent."
                .into()
        }
        ApiError::Unauthorized(s) => format!("Not signed in to {s}: add or replace its key in Settings → Keys."),
        ApiError::Precondition { .. } => "Changed on the server since you opened it.".into(),
        ApiError::Unavailable(m) => format!("Unavailable: {m}"),
        other => other.to_string(),
    }
}

/// Append one structured event (JSON line) to the session log — the
/// evidence trail for publishing, recovery, conflicts and outages. Never
/// carries credentials (values pass through redaction).
pub fn log(event: &str, fields: &[(&str, &str)]) {
    let mut m = serde_json::Map::new();
    m.insert("t".into(), farfield_core::store::now_ms().into());
    m.insert("event".into(), event.into());
    for (k, v) in fields {
        m.insert((*k).into(), farfield_core::secret::redact(v).into());
    }
    let line = serde_json::Value::Object(m).to_string();
    let path = data_dir().join("events.jsonl");
    if let Ok(mut f) = std::fs::OpenOptions::new().create(true).append(true).open(path) {
        let _ = writeln!(f, "{line}");
    }
    if std::env::var("FARFIELD_DESKTOP_TRACE").is_ok() {
        eprintln!("{line}");
    }
}
