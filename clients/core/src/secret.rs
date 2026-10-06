//! Credentials: scoped API keys (env keys or minted `ffk_` tokens), held in the
//! platform secret store and bound to the one origin they were entered for.
//!
//! A credential never prints: its Debug and Display show a hint only, so a
//! log line, a panic message or an error report cannot carry it anywhere.

use parking_lot::Mutex;
use sha2::{Digest, Sha256};
use std::collections::HashMap;
use std::fmt;

/// An API key. Opaque on purpose.
#[derive(Clone, PartialEq, Eq)]
pub struct Credential(String);

impl Credential {
    pub fn new(s: impl Into<String>) -> Option<Self> {
        let s = s.into().trim().to_string();
        (!s.is_empty() && !s.contains(char::is_whitespace)).then_some(Credential(s))
    }
    /// The secret itself, for the one place that sends it (the transport).
    pub(crate) fn expose(&self) -> &str {
        &self.0
    }
    /// A short identity for the key that is safe to store and show: which
    /// key, never the key. Caches are partitioned by it, so two keys with
    /// different visibility never read each other's responses.
    pub fn fingerprint(&self) -> String {
        hex::encode(&Sha256::digest(self.0.as_bytes())[..8])
    }
    /// The last four characters, the way the keys console shows a hint.
    pub fn hint(&self) -> String {
        if self.0.chars().count() <= 12 {
            return "…".into();
        }
        let tail: String = self.0.chars().rev().take(4).collect::<Vec<_>>().into_iter().rev().collect();
        if self.0.starts_with("ffk_") {
            format!("ffk_…{tail}")
        } else {
            format!("…{tail}")
        }
    }
}

impl fmt::Debug for Credential {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "Credential({})", self.hint())
    }
}

impl fmt::Display for Credential {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.hint())
    }
}

/// Where a credential lives: one per profile, service and origin. The origin
/// is part of the key, so a key entered for one host is never offered to
/// another — editing an endpoint's host makes its old key unreachable rather
/// than sending it somewhere new.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct SecretKey {
    pub profile: String,
    pub service: String,
    pub origin: String,
}

impl SecretKey {
    fn account(&self) -> String {
        format!("{}/{}@{}", self.profile, self.service, self.origin)
    }
}

pub trait SecretStore: Send + Sync {
    fn get(&self, key: &SecretKey) -> Result<Option<Credential>, String>;
    fn set(&self, key: &SecretKey, cred: &Credential) -> Result<(), String>;
    fn delete(&self, key: &SecretKey) -> Result<(), String>;
}

/// The Keychain (macOS/iOS) or Secret Service (Linux).
pub struct PlatformStore {
    service: String,
}

impl PlatformStore {
    pub fn new() -> Self {
        PlatformStore { service: "systems.farfield.desktop".into() }
    }
}

impl Default for PlatformStore {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(any(target_os = "macos", target_os = "ios", target_os = "linux"))]
impl SecretStore for PlatformStore {
    fn get(&self, key: &SecretKey) -> Result<Option<Credential>, String> {
        let e = keyring::Entry::new(&self.service, &key.account()).map_err(|e| e.to_string())?;
        match e.get_password() {
            Ok(s) => Ok(Credential::new(s)),
            Err(keyring::Error::NoEntry) => Ok(None),
            Err(e) => Err(e.to_string()),
        }
    }
    fn set(&self, key: &SecretKey, cred: &Credential) -> Result<(), String> {
        let e = keyring::Entry::new(&self.service, &key.account()).map_err(|e| e.to_string())?;
        e.set_password(cred.expose()).map_err(|e| e.to_string())
    }
    fn delete(&self, key: &SecretKey) -> Result<(), String> {
        let e = keyring::Entry::new(&self.service, &key.account()).map_err(|e| e.to_string())?;
        match e.delete_credential() {
            Ok(()) | Err(keyring::Error::NoEntry) => Ok(()),
            Err(e) => Err(e.to_string()),
        }
    }
}

/// An in-memory store, for tests and for a session that must not persist.
#[derive(Default)]
pub struct MemoryStore {
    m: Mutex<HashMap<SecretKey, Credential>>,
}

impl SecretStore for MemoryStore {
    fn get(&self, key: &SecretKey) -> Result<Option<Credential>, String> {
        Ok(self.m.lock().get(key).cloned())
    }
    fn set(&self, key: &SecretKey, cred: &Credential) -> Result<(), String> {
        self.m.lock().insert(key.clone(), cred.clone());
        Ok(())
    }
    fn delete(&self, key: &SecretKey) -> Result<(), String> {
        self.m.lock().remove(key);
        Ok(())
    }
}

/// Mask anything that looks like a credential in free text (an error body, a
/// URL with a `?t=` token) before it is logged or shown.
pub fn redact(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    let mut rest = s;
    while !rest.is_empty() {
        if let Some(i) = rest.find("ffk_") {
            out.push_str(&rest[..i]);
            let tail = &rest[i..];
            let n = tail.find(|c: char| !(c.is_ascii_alphanumeric() || c == '_')).unwrap_or(tail.len());
            out.push_str("ffk_[redacted]");
            rest = &tail[n..];
        } else {
            out.push_str(rest);
            break;
        }
    }
    // query-string secrets: token=, t=, key=
    for k in ["token=", "?t=", "&t=", "key="] {
        while let Some(i) = out.find(k) {
            let start = i + k.len();
            let end =
                out[start..].find(|c: char| c == '&' || c.is_whitespace() || c == '"').map_or(out.len(), |j| start + j);
            if out[start..end] == *"[redacted]" {
                break;
            }
            out.replace_range(start..end, "[redacted]");
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn credential_never_prints() {
        let c = Credential::new("ffk_abcdefghijklmnop").unwrap();
        assert_eq!(format!("{c:?}"), "Credential(ffk_…mnop)");
        assert_eq!(format!("{c}"), "ffk_…mnop");
        assert!(!format!("{c:?}").contains("abcdefgh"));
    }

    #[test]
    fn redaction() {
        assert_eq!(redact("bad key ffk_abc123_x9 here"), "bad key ffk_[redacted] here");
        assert_eq!(redact("GET /x/raw?t=s3cret&y=1"), "GET /x/raw?t=[redacted]&y=1");
        assert_eq!(redact("token: abc"), "token: abc");
        assert_eq!(redact("token=abc def"), "token=[redacted] def");
    }

    #[test]
    fn origin_is_part_of_the_key() {
        let s = MemoryStore::default();
        let a = SecretKey { profile: "p".into(), service: "feed".into(), origin: "https://a:1".into() };
        let b = SecretKey { origin: "https://b:1".into(), ..a.clone() };
        s.set(&a, &Credential::new("k").unwrap()).unwrap();
        assert!(s.get(&b).unwrap().is_none());
    }
}
