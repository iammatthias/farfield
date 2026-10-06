//! A signed-in view of one profile: endpoints, credentials, cache and drafts,
//! plus the cached-read path every workspace uses.

use crate::profile::{origin, validate_base, Profile};
use crate::secret::{Credential, SecretKey, SecretStore};
use crate::store::{now_ms, Cache, CacheEntry, Drafts, Scope};
use crate::transport::{ApiError, Fetched, ServiceClient};
use parking_lot::Mutex;
use serde::de::DeserializeOwned;
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;

/// How current a value is — what the UI shows beside it.
#[derive(Debug, Clone, PartialEq)]
pub enum Freshness {
    /// Just confirmed with the server (200, or 304 against the cache).
    Live,
    /// From the cache because the service could not be reached; `error` is
    /// why, `age_ms` how old the copy is.
    Stale { age_ms: u64, error: ApiError },
}

#[derive(Debug, Clone)]
pub struct Loaded<T> {
    pub value: T,
    pub etag: Option<String>,
    pub freshness: Freshness,
}

pub struct Session {
    pub profile: Profile,
    data_dir: PathBuf,
    secrets: Arc<dyn SecretStore>,
    cache: Arc<Cache>,
    clients: Mutex<HashMap<String, ServiceClient>>,
}

impl Session {
    pub fn new(profile: Profile, data_dir: PathBuf, secrets: Arc<dyn SecretStore>) -> Self {
        Session {
            profile,
            data_dir,
            secrets,
            cache: Arc::new(Cache::new(256, 128 << 20)),
            clients: Mutex::new(HashMap::new()),
        }
    }

    pub fn data_dir(&self) -> &PathBuf {
        &self.data_dir
    }

    fn secret_key(&self, service: &str) -> Result<SecretKey, ApiError> {
        let ep = self.profile.endpoint(service).ok_or(ApiError::NotFound)?;
        Ok(SecretKey { profile: self.profile.id.clone(), service: service.into(), origin: origin(&validate_base(&ep.api)?) })
    }

    /// Store (or replace) the key for a service. It is bound to the
    /// service's current origin.
    pub fn set_credential(&self, service: &str, cred: &Credential) -> Result<(), String> {
        let k = self.secret_key(service).map_err(|e| e.to_string())?;
        self.secrets.set(&k, cred)?;
        self.clients.lock().remove(service);
        Ok(())
    }

    pub fn forget_credential(&self, service: &str) -> Result<(), String> {
        let k = self.secret_key(service).map_err(|e| e.to_string())?;
        self.secrets.delete(&k)?;
        self.clients.lock().remove(service);
        Ok(())
    }

    pub fn has_credential(&self, service: &str) -> bool {
        self.client(service).map(|c| c.has_credential()).unwrap_or(false)
    }

    /// The client for a service, with its credential when one is stored for
    /// exactly this origin.
    pub fn client(&self, service: &str) -> Result<ServiceClient, ApiError> {
        if let Some(c) = self.clients.lock().get(service) {
            return Ok(c.clone());
        }
        let ep = self.profile.endpoint(service).ok_or(ApiError::NotFound)?;
        let k = self.secret_key(service)?;
        let cred = self.secrets.get(&k).map_err(ApiError::Unavailable)?;
        let c = ServiceClient::new(service, &ep.api, cred)?;
        self.clients.lock().insert(service.into(), c.clone());
        Ok(c)
    }

    /// Public base for share links, if the service has one.
    pub fn public_base(&self, service: &str) -> Option<String> {
        self.profile.endpoint(service)?.public.clone()
    }

    pub fn scope(&self, service: &str) -> Result<Scope, ApiError> {
        Ok(Scope::new(&self.data_dir, &self.profile.id, &self.client(service)?.identity()))
    }

    /// Drafts for a service. A draft area may be a sub-kind of a service
    /// (`content-series`); it is scoped by the service it belongs to.
    pub fn drafts(&self, area: &str) -> Result<Drafts, ApiError> {
        let service = area.split('-').next().unwrap_or(area);
        Ok(Drafts::new(self.scope(service)?))
    }

    /// Read through the cache: revalidate with the stored ETag; on 304 use
    /// the cached copy; when the service is unreachable fall back to it and
    /// say so. Auth failures are never papered over with cached data.
    pub async fn load<T: DeserializeOwned>(&self, service: &str, path: &str) -> Result<Loaded<T>, ApiError> {
        let c = self.client(service)?;
        let scope = self.scope(service)?;
        let cached = self.cache.get(&scope, service, path);
        let inm = cached.as_ref().and_then(|e| e.etag.clone());
        match c.get_json::<serde_json::Value>(path, inm.as_deref()).await {
            Ok(Fetched::Fresh(v)) => {
                self.cache.put(
                    &scope,
                    service,
                    path,
                    CacheEntry { etag: v.etag.clone(), fetched_ms: now_ms(), body: v.value.clone() },
                );
                let value = serde_json::from_value(v.value).map_err(|e| ApiError::Decode(e.to_string()))?;
                Ok(Loaded { value, etag: v.etag, freshness: Freshness::Live })
            }
            Ok(Fetched::NotModified) => {
                let e = cached.ok_or_else(|| ApiError::Decode("304 without a cached copy".into()))?;
                self.cache.touch(&scope, service, path);
                let value = serde_json::from_value(e.body).map_err(|e| ApiError::Decode(e.to_string()))?;
                Ok(Loaded { value, etag: e.etag, freshness: Freshness::Live })
            }
            Err(err @ (ApiError::Offline(_) | ApiError::Server { .. } | ApiError::RateLimited { .. })) => match cached {
                Some(e) => {
                    let value = serde_json::from_value(e.body).map_err(|e| ApiError::Decode(e.to_string()))?;
                    Ok(Loaded { value, etag: e.etag, freshness: Freshness::Stale { age_ms: now_ms().saturating_sub(e.fetched_ms), error: err } })
                }
                None => Err(err),
            },
            Err(e) => {
                if matches!(e, ApiError::Unauthorized(_) | ApiError::NotFound) {
                    self.cache.forget(&scope, service, path);
                }
                Err(e)
            }
        }
    }

    /// Drop a cached read (after a mutation that changes it).
    pub fn invalidate(&self, service: &str, path: &str) {
        if let Ok(scope) = self.scope(service) {
            self.cache.forget(&scope, service, path);
        }
    }
}

/// Discards stale responses: each request takes a ticket, and only the
/// newest ticket's result is applied — a slow page-1 load cannot overwrite
/// the page-2 the person has already moved to.
#[derive(Default, Debug)]
pub struct Latest {
    n: std::sync::atomic::AtomicU64,
}

impl Latest {
    pub fn ticket(&self) -> u64 {
        self.n.fetch_add(1, std::sync::atomic::Ordering::SeqCst) + 1
    }
    pub fn is_current(&self, t: u64) -> bool {
        self.n.load(std::sync::atomic::Ordering::SeqCst) == t
    }
}
