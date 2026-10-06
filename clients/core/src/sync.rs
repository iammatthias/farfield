//! Saving local drafts to the server, safely.
//!
//! A draft is saved to disk on every edit (that is "saved on this Mac"); a
//! server save is a separate, explicit act. Server saves are conditional
//! (If-Match on the version the draft was edited from), so a save never
//! overwrites someone else's change: it lands, or it comes back as a
//! conflict with the server's copy beside the local one.
//!
//! A save whose response was lost is *uncertain*. It is not retried blindly;
//! it is reconciled by reading the server: if the server already has exactly
//! the local version the save landed; if the server is still at the base
//! version it did not; anything else is a conflict.

use crate::api::content::{self, Entry};
use crate::api::feed;
use crate::merge::{keep_refs, merge3};
use crate::session::Session;
use crate::store::{now_ms, Draft, SaveState};
use crate::transport::{ApiError, Versioned};
use serde_json::Value;

/// What happened to a save.
#[derive(Debug, Clone, PartialEq)]
pub enum SaveOutcome {
    Saved,
    Conflict,
    /// Not sent or not applied; still local. The error says why.
    NotSaved(ApiError),
}

/// A record kind the sync layer can save.
pub trait Kind {
    const SERVICE: &'static str;
    /// Where its drafts live (defaults to the service).
    const DRAFTS: &'static str = Self::SERVICE;
    /// The field holding the document text (merged line by line).
    const TEXT: &'static str = "body";
    /// The fields a person edits — what "the server has my version" compares.
    const EDITABLE: &'static [&'static str];
    fn key_of(v: &Value) -> String;
    fn get(s: &Session, key: &str) -> impl std::future::Future<Output = Result<Versioned<Value>, ApiError>> + Send;
    fn put(
        s: &Session,
        key: &str,
        v: &Value,
        if_match: Option<&str>,
    ) -> impl std::future::Future<Output = Result<Versioned<Value>, ApiError>> + Send;
    fn post(s: &Session, v: &Value) -> impl std::future::Future<Output = Result<Versioned<Value>, ApiError>> + Send;
}

pub fn same_edits<K: Kind>(a: &Value, b: &Value) -> bool {
    K::EDITABLE.iter().all(|f| norm(a.get(*f)) == norm(b.get(*f)))
}

/// As `same_edits`, ignoring the fields the server names on create (a new
/// record's slug is stamped by the server, so the draft never had it).
fn same_created<K: Kind>(server: &Value, local: &Value) -> bool {
    // a requested slug comes back stamped: "<ms>-<slug>"
    let want = local.get("slug").and_then(|v| v.as_str()).unwrap_or("");
    let got = server.get("slug").and_then(|v| v.as_str()).unwrap_or("");
    (want.is_empty() || got == want || got.ends_with(&format!("-{want}")))
        && K::EDITABLE.iter().filter(|f| **f != "slug").all(|f| norm(server.get(*f)) == norm(local.get(*f)))
}

/// A field value with "absent", null, "", [] and false treated alike.
pub fn norm(v: Option<&Value>) -> Value {
    match v {
        None | Some(Value::Null) => Value::Null,
        Some(Value::String(s)) if s.is_empty() => Value::Null,
        Some(Value::Array(a)) if a.is_empty() => Value::Null,
        Some(Value::Bool(false)) => Value::Null,
        Some(v) => v.clone(),
    }
}

fn to_value<T: serde::Serialize>(v: Versioned<T>) -> Result<Versioned<Value>, ApiError> {
    Ok(Versioned { value: serde_json::to_value(v.value).map_err(|e| ApiError::Decode(e.to_string()))?, etag: v.etag })
}

pub struct ContentEntry;

impl Kind for ContentEntry {
    const SERVICE: &'static str = content::SERVICE;
    const EDITABLE: &'static [&'static str] = &["collection", "title", "excerpt", "body", "tags", "published", "slug"];
    fn key_of(v: &Value) -> String {
        v["slug"].as_str().unwrap_or("").to_string()
    }
    async fn get(s: &Session, key: &str) -> Result<Versioned<Value>, ApiError> {
        let l = content::entry(s, key).await?;
        to_value(Versioned { value: l.value, etag: l.etag })
    }
    async fn put(s: &Session, key: &str, v: &Value, if_match: Option<&str>) -> Result<Versioned<Value>, ApiError> {
        let e: Entry = serde_json::from_value(v.clone()).map_err(|e| ApiError::Decode(e.to_string()))?;
        to_value(content::update(s, key, &e, if_match).await?)
    }
    async fn post(s: &Session, v: &Value) -> Result<Versioned<Value>, ApiError> {
        let e: Entry = serde_json::from_value(v.clone()).map_err(|e| ApiError::Decode(e.to_string()))?;
        to_value(content::create(s, &e).await?)
    }
}

pub struct FeedPost;

impl Kind for FeedPost {
    const SERVICE: &'static str = feed::SERVICE;
    const EDITABLE: &'static [&'static str] = &["body", "tags"];
    fn key_of(v: &Value) -> String {
        v["slug"].as_str().unwrap_or("").to_string()
    }
    async fn get(s: &Session, key: &str) -> Result<Versioned<Value>, ApiError> {
        let l = feed::post(s, key).await?;
        to_value(Versioned { value: l.value, etag: l.etag })
    }
    async fn put(s: &Session, key: &str, v: &Value, if_match: Option<&str>) -> Result<Versioned<Value>, ApiError> {
        let tags: Vec<String> = serde_json::from_value(v["tags"].clone()).unwrap_or_default();
        to_value(feed::update(s, key, v["body"].as_str().unwrap_or(""), &tags, if_match).await?)
    }
    async fn post(s: &Session, v: &Value) -> Result<Versioned<Value>, ApiError> {
        let tags: Vec<String> = serde_json::from_value(v["tags"].clone()).unwrap_or_default();
        to_value(feed::create(s, v["body"].as_str().unwrap_or(""), &tags).await?)
    }
}

/// A new, never-saved draft's key: stable on disk until the server names it.
pub fn local_key() -> String {
    format!("new-{}", now_ms())
}

pub fn is_new(d: &Draft) -> bool {
    d.base.is_none()
}

/// Save a draft to the server. The draft on disk is updated to reflect the
/// outcome before this returns, so a crash right after cannot lose it.
pub async fn save<K: Kind>(s: &Session, d: &mut Draft) -> Result<SaveOutcome, ApiError> {
    let drafts = s.drafts(K::DRAFTS)?;
    if d.state == SaveState::Pending {
        // a previous attempt's outcome is unknown: settle that first
        reconcile::<K>(s, d).await?;
        drafts.save(d).map_err(|e| ApiError::BadRequest(e.to_string()))?;
        if d.state != SaveState::Local {
            return Ok(if d.state == SaveState::Saved { SaveOutcome::Saved } else { SaveOutcome::Conflict });
        }
    }
    if d.state == SaveState::Conflict {
        return Ok(SaveOutcome::Conflict);
    }
    d.state = SaveState::Pending;
    d.updated_ms = now_ms();
    drafts.save(d).map_err(|e| ApiError::BadRequest(e.to_string()))?;

    let result = if is_new(d) {
        K::post(s, &d.local).await
    } else {
        let key = K::key_of(d.base.as_ref().unwrap());
        K::put(s, &key, &d.local, d.base_etag.as_deref()).await
    };
    let outcome = match result {
        Ok(v) => {
            let old_key = d.key.clone();
            accept(d, v);
            if is_local_key(&old_key) && d.key != old_key {
                drafts.discard(K::DRAFTS, &old_key).map_err(|e| ApiError::BadRequest(e.to_string()))?;
            }
            SaveOutcome::Saved
        }
        Err(ApiError::Precondition { current, etag }) => {
            d.state = SaveState::Conflict;
            d.remote = Some(current);
            d.remote_etag = etag;
            SaveOutcome::Conflict
        }
        Err(e @ ApiError::Uncertain(_)) => {
            // leave Pending; the next save (or recovery) reconciles
            drafts.save(d).map_err(|e| ApiError::BadRequest(e.to_string()))?;
            return Ok(SaveOutcome::NotSaved(e));
        }
        Err(e) => {
            d.state = SaveState::Local;
            drafts.save(d).map_err(|e| ApiError::BadRequest(e.to_string()))?;
            return Ok(SaveOutcome::NotSaved(e));
        }
    };
    d.updated_ms = now_ms();
    drafts.save(d).map_err(|e| ApiError::BadRequest(e.to_string()))?;
    Ok(outcome)
}

fn is_local_key(k: &str) -> bool {
    k.starts_with("new-")
}

/// The server accepted `v`: it becomes the new base, and the local copy
/// takes the server's bookkeeping (slug, cid, timestamps) while keeping the
/// person's edits.
fn accept(d: &mut Draft, v: Versioned<Value>) {
    let mut local = v.value.clone();
    // keep anything the person typed after the save started (none here, but
    // a UI may call accept after further edits): only bookkeeping is taken
    if let (Some(l), Some(srv)) = (local.as_object_mut(), v.value.as_object()) {
        for (k, val) in srv {
            l.insert(k.clone(), val.clone());
        }
    }
    let key = local["slug"].as_str().or(local["id"].as_str()).unwrap_or(&d.key).to_string();
    d.key = key;
    d.local = local;
    d.base = Some(v.value);
    d.base_etag = v.etag;
    d.state = SaveState::Saved;
    d.remote = None;
    d.remote_etag = None;
}

/// Settle a Pending draft by reading the server.
pub async fn reconcile<K: Kind>(s: &Session, d: &mut Draft) -> Result<(), ApiError> {
    if is_new(d) {
        // A create whose answer was lost. Creates are not idempotent, so
        // look for it rather than posting a duplicate: content and feed
        // list newest first, and a match on every edited field is it.
        if let Some(found) = find_created::<K>(s, &d.local).await? {
            accept(d, found);
        } else {
            d.state = SaveState::Local;
        }
        return Ok(());
    }
    let key = K::key_of(d.base.as_ref().unwrap());
    match K::get(s, &key).await {
        Ok(srv) => {
            if same_edits::<K>(&srv.value, &d.local) {
                accept(d, srv);
            } else if srv.etag.is_some() && srv.etag == d.base_etag {
                d.state = SaveState::Local; // never applied
            } else {
                d.state = SaveState::Conflict;
                d.remote = Some(srv.value);
                d.remote_etag = srv.etag;
            }
            Ok(())
        }
        Err(ApiError::NotFound) => {
            // deleted on the server meanwhile: keep the work, as new
            d.state = SaveState::Conflict;
            d.remote = Some(Value::Null);
            d.remote_etag = None;
            Ok(())
        }
        Err(e) => Err(e),
    }
}

async fn find_created<K: Kind>(s: &Session, local: &Value) -> Result<Option<Versioned<Value>>, ApiError> {
    let candidates: Vec<Value> = match K::DRAFTS {
        content::SERVICE => {
            let col = local["collection"].as_str();
            let p = content::entries(s, col, content::Status::All, 1, 20).await?;
            p.value.items.into_iter().filter_map(|e| serde_json::to_value(e).ok()).collect()
        }
        "content-series" => {
            let l = content::series_list(s).await?;
            l.value.into_iter().filter_map(|e| serde_json::to_value(e).ok()).collect()
        }
        feed::SERVICE => {
            let p = feed::posts(s, None, 20).await?;
            p.value.0.into_iter().filter_map(|e| serde_json::to_value(e).ok()).collect()
        }
        _ => vec![],
    };
    for c in candidates {
        // list rows may be slim (no body): fetch the full record to compare
        let key = K::key_of(&c);
        if key.is_empty() {
            continue;
        }
        if c.get("title") != local.get("title") && K::DRAFTS == content::SERVICE {
            continue;
        }
        let full = K::get(s, &key).await?;
        if same_created::<K>(&full.value, local) {
            return Ok(Some(full));
        }
    }
    Ok(None)
}

/// How to settle a conflict.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Resolution {
    /// Save the local version over the server's (explicitly).
    KeepMine,
    /// Take the server's version; local-only uploads are kept in the body.
    TakeTheirs,
    /// Merge both into the editor for review; nothing is sent yet.
    Merge,
}

/// Apply a resolution. KeepMine re-saves against the server's version;
/// TakeTheirs and Merge leave a Local draft for the person to review.
pub async fn resolve<K: Kind>(s: &Session, d: &mut Draft, how: Resolution) -> Result<SaveOutcome, ApiError> {
    let remote = d.remote.clone().unwrap_or(Value::Null);
    let base = d.base.clone().unwrap_or(Value::Null);
    match how {
        Resolution::KeepMine => {
            if remote.is_null() {
                // deleted on the server: recreate
                d.base = None;
                d.base_etag = None;
            } else {
                d.base = Some(remote);
                d.base_etag = d.remote_etag.clone();
            }
            d.state = SaveState::Local;
            d.remote = None;
            d.remote_etag = None;
            save::<K>(s, d).await
        }
        Resolution::TakeTheirs => {
            let mut theirs = remote.clone();
            keep_refs(&base, &d.local, &mut theirs, K::TEXT);
            d.local = theirs;
            d.base = Some(remote);
            d.base_etag = d.remote_etag.take();
            d.remote = None;
            d.state = if d.base.as_ref().is_some_and(|b| same_edits::<K>(b, &d.local)) {
                SaveState::Saved
            } else {
                SaveState::Local
            };
            s.drafts(K::DRAFTS)?.save(d).map_err(|e| ApiError::BadRequest(e.to_string()))?;
            Ok(if d.state == SaveState::Saved {
                SaveOutcome::Saved
            } else {
                SaveOutcome::NotSaved(ApiError::Cancelled)
            })
        }
        Resolution::Merge => {
            let m = merge3(&base, &d.local, &remote, K::TEXT);
            d.local = m.value;
            d.base = Some(remote);
            d.base_etag = d.remote_etag.take();
            d.remote = None;
            d.state = SaveState::Local;
            s.drafts(K::DRAFTS)?.save(d).map_err(|e| ApiError::BadRequest(e.to_string()))?;
            Ok(SaveOutcome::NotSaved(ApiError::Cancelled))
        }
    }
}

/// Start editing a server record: load it and its version into a draft —
/// or return the existing draft, which always wins over the server copy
/// (local work is never silently replaced).
pub async fn open<K: Kind>(s: &Session, key: &str) -> Result<Draft, ApiError> {
    let drafts = s.drafts(K::DRAFTS)?;
    if let Some(d) = drafts.load(K::DRAFTS, key) {
        return Ok(d);
    }
    let v = K::get(s, key).await?;
    Ok(Draft {
        service: K::DRAFTS.into(),
        key: key.into(),
        base: Some(v.value.clone()),
        base_etag: v.etag,
        local: v.value,
        state: SaveState::Saved,
        remote: None,
        remote_etag: None,
        updated_ms: now_ms(),
    })
}

/// A fresh draft for a record not yet on the server.
pub fn new_draft(service: &str, local: Value) -> Draft {
    Draft {
        service: service.into(),
        key: local_key(),
        base: None,
        base_etag: None,
        local,
        state: SaveState::Local,
        remote: None,
        remote_etag: None,
        updated_ms: now_ms(),
    }
}

/// Record a local edit: persist immediately (atomic), mark it local.
pub fn edit(s: &Session, d: &mut Draft, local: Value) -> Result<(), ApiError> {
    if d.local == local {
        return Ok(());
    }
    d.local = local;
    if d.state == SaveState::Saved {
        d.state = SaveState::Local;
    }
    d.updated_ms = now_ms();
    s.drafts(&d.service)?.save(d).map_err(|e| ApiError::BadRequest(e.to_string()))
}

/// Drop local work for a record that is saved (or that the person chose to
/// throw away).
pub fn close(s: &Session, d: &Draft) -> Result<(), ApiError> {
    s.drafts(&d.service)?.discard(&d.service, &d.key).map_err(|e| ApiError::BadRequest(e.to_string()))
}

pub struct ContentSeries;

impl Kind for ContentSeries {
    const SERVICE: &'static str = content::SERVICE;
    const DRAFTS: &'static str = "content-series";
    const EDITABLE: &'static [&'static str] = &["title", "body"];
    fn key_of(v: &Value) -> String {
        v["slug"].as_str().unwrap_or("").to_string()
    }
    async fn get(s: &Session, key: &str) -> Result<Versioned<Value>, ApiError> {
        let l = content::series(s, key).await?;
        to_value(Versioned { value: l.value, etag: l.etag })
    }
    async fn put(s: &Session, key: &str, v: &Value, if_match: Option<&str>) -> Result<Versioned<Value>, ApiError> {
        to_value(
            content::update_series(
                s,
                key,
                v["title"].as_str().unwrap_or(""),
                v["body"].as_str().unwrap_or(""),
                if_match,
            )
            .await?,
        )
    }
    async fn post(s: &Session, v: &Value) -> Result<Versioned<Value>, ApiError> {
        to_value(
            content::create_series(
                s,
                v["title"].as_str().unwrap_or(""),
                v["slug"].as_str().unwrap_or(""),
                v["body"].as_str().unwrap_or(""),
            )
            .await?,
        )
    }
}
