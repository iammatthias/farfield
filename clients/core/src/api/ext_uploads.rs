//! Extra client functions for the uploads workspaces (see misc.rs for the base).
//!
//! Library uploads are resumable (tus): each in-flight upload's state is kept
//! as JSON under the session's scope, so an upload interrupted by a quit, a
//! crash or an outage is listed again on the next launch and continues from
//! the offset the server reports. Sideload helpers: public links and the
//! provisioning-profile expiry.
#![allow(unused_imports)]
use crate::session::{Loaded, Session};
use crate::store::{read_json, write_json};
use crate::transport::{ApiError, ServiceClient, Versioned};
use crate::upload::{tus_upload_with, Progress, TusOutcome, TusState, TUS_CHUNK};
use reqwest::Method;
use std::path::{Path, PathBuf};

pub mod library_uploads {
    use super::*;
    use crate::api::misc::library::SERVICE;

    /// One upload remembered on disk.
    #[derive(Clone, Debug)]
    pub struct Pending {
        /// Stable key: a hash of the file's path (one upload per file).
        pub key: String,
        pub state: TusState,
    }

    /// What the server holds for an upload, from a tus HEAD.
    #[derive(Clone, Debug, PartialEq)]
    pub struct ServerState {
        pub offset: u64,
        pub length: u64,
        /// open | finalizing | done | error
        pub status: String,
        pub cid: Option<String>,
        pub error: Option<String>,
    }

    /// `<scope>/uploads/library` — per profile and key, like drafts.
    pub fn dir(s: &Session) -> Result<PathBuf, ApiError> {
        Ok(s.scope(SERVICE)?.root().join("uploads").join("library"))
    }

    pub fn key_for(file: &Path) -> String {
        use sha2::Digest;
        hex::encode(&sha2::Sha256::digest(file.to_string_lossy().as_bytes())[..12])
    }

    /// Write (atomically) the state of an in-flight upload.
    pub fn remember(s: &Session, st: &TusState) -> Result<String, ApiError> {
        let key = key_for(&st.file);
        let path = dir(s)?.join(format!("{key}.json"));
        write_json(&path, st).map_err(|e| ApiError::BadRequest(format!("save upload state: {e}")))?;
        Ok(key)
    }

    pub fn forget(s: &Session, key: &str) {
        if let Ok(d) = dir(s) {
            let _ = std::fs::remove_file(d.join(format!("{key}.json")));
        }
    }

    /// Every upload a previous run left unfinished, oldest file name first.
    pub fn interrupted(s: &Session) -> Vec<Pending> {
        let Ok(d) = dir(s) else { return Vec::new() };
        let Ok(rd) = std::fs::read_dir(&d) else { return Vec::new() };
        let mut out: Vec<Pending> = rd
            .flatten()
            .filter_map(|e| {
                let p = e.path();
                if p.extension().and_then(|x| x.to_str()) != Some("json") {
                    return None;
                }
                let key = p.file_stem()?.to_string_lossy().to_string();
                let state: TusState = read_json(&p)?;
                Some(Pending { key, state })
            })
            .collect();
        out.sort_by(|a, b| a.state.file.cmp(&b.state.file));
        out
    }

    /// Ask the server how far an upload got (tus HEAD). None: the server no
    /// longer knows it (expired or discarded) — it starts over.
    pub async fn server_state(s: &Session, location: &str) -> Result<Option<ServerState>, ApiError> {
        let c = s.client(SERVICE)?;
        match c.send(c.request(Method::HEAD, location)?.header("Tus-Resumable", "1.0.0"), false).await {
            Ok(r) => {
                let h = |n: &str| r.headers().get(n).and_then(|v| v.to_str().ok()).map(|v| v.to_string());
                Ok(Some(ServerState {
                    offset: h("Upload-Offset").and_then(|v| v.parse().ok()).unwrap_or(0),
                    length: h("Upload-Length").and_then(|v| v.parse().ok()).unwrap_or(0),
                    status: h("X-Library-Status").unwrap_or_default(),
                    cid: h("X-Library-Cid").filter(|v| !v.is_empty()),
                    error: h("X-Library-Error").filter(|v| !v.is_empty()),
                }))
            }
            Err(ApiError::NotFound) => Ok(None),
            Err(e) => Err(e),
        }
    }

    /// Run (or resume) one upload, keeping its state on disk while it is in
    /// flight. The state is forgotten once the server settles it (done or
    /// failed); on an error or a pause it stays, to resume later.
    pub async fn run(s: &Session, mut st: TusState, progress: &Progress) -> Result<TusOutcome, ApiError> {
        run_chunked(s, &mut st, progress, TUS_CHUNK).await
    }

    pub async fn run_chunked(
        s: &Session,
        st: &mut TusState,
        progress: &Progress,
        chunk: u64,
    ) -> Result<TusOutcome, ApiError> {
        let c = s.client(SERVICE)?;
        let key = remember(s, st)?;
        let r = tus_upload_with(&c, st, progress, chunk, |st| {
            let _ = remember(s, st);
        })
        .await;
        if r.is_ok() {
            forget(s, &key);
            s.invalidate(SERVICE, "/api/admin/books");
        }
        r
    }

    /// Give up on an upload: the server drops its partial bytes and this Mac
    /// forgets it.
    pub async fn discard(s: &Session, st: &TusState) -> Result<(), ApiError> {
        drop_partial(s, st).await?;
        forget(s, &key_for(&st.file));
        Ok(())
    }

    /// Ask the server to drop an upload's partial bytes, keeping local state
    /// alone (a fresh upload of the same file may already own it).
    pub async fn drop_partial(s: &Session, st: &TusState) -> Result<(), ApiError> {
        if let Some(loc) = &st.location {
            let c = s.client(SERVICE)?;
            match c.send(c.request(Method::DELETE, loc)?.header("Tus-Resumable", "1.0.0"), true).await {
                Ok(_) | Err(ApiError::NotFound) => {}
                Err(e) => return Err(e),
            }
        }
        Ok(())
    }
}

pub mod sideload_links {
    use super::*;
    use crate::api::misc::sideload::{Share, SERVICE};

    /// A share's landing page on the public host.
    pub fn share_url(s: &Session, sh: &Share) -> String {
        match s.public_base(SERVICE) {
            Some(b) => format!("{}/s/{}", b.trim_end_matches('/'), sh.token),
            None => sh.share_url.clone(),
        }
    }

    #[derive(Clone, Copy, Debug, PartialEq)]
    pub enum Expiry {
        /// No profile date in the build.
        Unknown,
        Expired,
        /// Within 7 days: whole days left (0 = today).
        Soon(i64),
        Ok(i64),
    }

    /// Classify a profile expiry (RFC 3339) against now.
    pub fn expiry(s: &str) -> Expiry {
        expiry_at(s, time::OffsetDateTime::now_utc())
    }

    pub fn expiry_at(s: &str, now: time::OffsetDateTime) -> Expiry {
        let Ok(t) = time::OffsetDateTime::parse(s.trim(), &time::format_description::well_known::Rfc3339) else {
            return Expiry::Unknown;
        };
        let left = t - now;
        if left.is_negative() {
            Expiry::Expired
        } else if left.whole_days() < 7 {
            Expiry::Soon(left.whole_days())
        } else {
            Expiry::Ok(left.whole_days())
        }
    }

    /// Seconds until an RFC 3339 time (negative once past); None if unparseable.
    pub fn seconds_until(s: &str) -> Option<i64> {
        let t = time::OffsetDateTime::parse(s.trim(), &time::format_description::well_known::Rfc3339).ok()?;
        Some((t - time::OffsetDateTime::now_utc()).whole_seconds())
    }

    #[cfg(test)]
    mod tests {
        use super::*;
        #[test]
        fn expiry_buckets() {
            let now = time::macros::datetime!(2026-10-05 12:00 UTC);
            assert_eq!(expiry_at("", now), Expiry::Unknown);
            assert_eq!(expiry_at("2026-10-01T00:00:00Z", now), Expiry::Expired);
            assert_eq!(expiry_at("2026-10-08T00:00:00Z", now), Expiry::Soon(2));
            assert_eq!(expiry_at("2026-12-05T12:00:00Z", now), Expiry::Ok(61));
        }
    }
}
