//! Extra client functions for the lists workspaces (see misc.rs for the base):
//! single-record admin reads, the public views a person shares, exports, the
//! page images a bookmark points at, and the time arithmetic expiry needs.
#![allow(unused_imports)]
use crate::session::{Loaded, Session};
use crate::transport::{ApiError, ServiceClient, Versioned, USER_AGENT};
use bytes::Bytes;
use std::path::Path;
use std::sync::OnceLock;
use std::time::Duration;

fn enc(s: &str) -> String {
    url::form_urlencoded::byte_serialize(s.as_bytes()).collect()
}

// ── bookmarks ────────────────────────────────────────────────────────────

pub mod bookmarks {
    use super::*;
    use crate::api::bookmarks::{Bookmark, SERVICE};
    use serde::Deserialize;

    #[derive(Deserialize)]
    struct List {
        bookmarks: Vec<Bookmark>,
    }

    /// The public list — what a reader key sees (public bookmarks only, no
    /// admin notes). For checking what a visitor gets.
    pub async fn public_list(s: &Session) -> Result<Vec<Bookmark>, ApiError> {
        Ok(s.client(SERVICE)?.get::<List>("/api/bookmarks").await?.value.bookmarks)
    }

    /// The server's copy carried by a 412, if it is a bookmark.
    pub fn from_conflict(current: &serde_json::Value) -> Option<Bookmark> {
        serde_json::from_value(current.clone()).ok().filter(|b: &Bookmark| !b.id.is_empty())
    }
}

// ── qr ───────────────────────────────────────────────────────────────────

pub mod qr {
    use super::*;
    use crate::api::qr::{Code, SERVICE};

    /// One code as stored, private or disabled (admin route).
    pub async fn get(s: &Session, id: &str) -> Result<Loaded<Code>, ApiError> {
        s.load(SERVICE, &format!("/api/admin/codes/{}", enc(id))).await
    }

    /// The server's copy carried by a 412, if it is a code.
    pub fn from_conflict(current: &serde_json::Value) -> Option<Code> {
        serde_json::from_value(current.clone()).ok().filter(|c: &Code| !c.id.is_empty())
    }

    /// The public scan image (`/qr/{id}.png`) — only for public, enabled
    /// codes; anything else is `NotFound`.
    pub fn public_png_url(s: &Session, id: &str) -> Option<String> {
        s.public_base(SERVICE).map(|b| format!("{}/qr/{}.png", b.trim_end_matches('/'), enc(id)))
    }

    /// Export sizes offered for the PNG (requested px; the server renders
    /// whole modules, so the file is at or just under this).
    pub const PNG_SIZES: [u32; 4] = [256, 512, 1024, 2048];
}

// ── scrap ────────────────────────────────────────────────────────────────

pub mod scrap {
    use super::*;

    /// Whether an `expiresAt` (RFC 3339, "" = never) has passed at `now`.
    /// Unparseable timestamps are treated as not expired.
    pub fn is_expired(expires_at: &str, now: time::OffsetDateTime) -> bool {
        parse(expires_at).is_some_and(|t| t <= now)
    }

    /// Now, for comparing several expiries against one instant.
    pub fn now() -> time::OffsetDateTime {
        time::OffsetDateTime::now_utc()
    }

    pub fn expired_now(expires_at: &str) -> bool {
        is_expired(expires_at, time::OffsetDateTime::now_utc())
    }

    /// "in 3h", "in 6d", "expired" — or "" for never.
    pub fn expiry_label(expires_at: &str, now: time::OffsetDateTime) -> String {
        let Some(t) = parse(expires_at) else { return String::new() };
        let s = (t - now).whole_seconds();
        if s <= 0 {
            return "expired".into();
        }
        match s {
            0..=59 => format!("in {s}s"),
            60..=3599 => format!("in {}m", s / 60),
            3600..=86399 => format!("in {}h", s / 3600),
            _ => format!("in {}d", s / 86400),
        }
    }

    fn parse(s: &str) -> Option<time::OffsetDateTime> {
        if s.is_empty() {
            return None;
        }
        time::OffsetDateTime::parse(s, &time::format_description::well_known::Rfc3339).ok()
    }

    /// Languages offered in the picker (the server accepts any; these are
    /// ones its highlighter knows well). "" = plain text.
    pub const LANGS: [&str; 16] =
        ["", "go", "rust", "ts", "js", "py", "sh", "json", "yaml", "toml", "sql", "html", "css", "md", "swift", "diff"];
}

// ── shared: page images and exports ─────────────────────────────────────

/// A credential-less client for images on the open web (a bookmark's
/// og:image and favicon). Never carries a fleet key; follows a few
/// redirects since these are public assets.
fn web_client() -> reqwest::Client {
    static C: OnceLock<reqwest::Client> = OnceLock::new();
    C.get_or_init(|| {
        reqwest::Client::builder()
            .user_agent(USER_AGENT)
            .redirect(reqwest::redirect::Policy::limited(3))
            .connect_timeout(Duration::from_secs(6))
            .timeout(Duration::from_secs(15))
            .build()
            .expect("web client")
    })
    .clone()
}

/// The most an image preview may weigh.
pub const MAX_REMOTE_IMAGE: usize = 4 << 20;

/// Fetch a public image (http/https only, size-capped).
pub async fn remote_image(url: &str) -> Result<Bytes, ApiError> {
    let u = url::Url::parse(url).map_err(|e| ApiError::BadRequest(e.to_string()))?;
    if !matches!(u.scheme(), "http" | "https") {
        return Err(ApiError::BadRequest(format!("not a web URL: {}", u.scheme())));
    }
    let r = web_client().get(u).send().await.map_err(|e| ApiError::Offline(crate::secret::redact(&e.to_string())))?;
    if !r.status().is_success() {
        return Err(if r.status().as_u16() == 404 {
            ApiError::NotFound
        } else {
            ApiError::Server { status: r.status().as_u16(), message: "image fetch failed".into() }
        });
    }
    ServiceClient::body(r, MAX_REMOTE_IMAGE).await
}

/// Write a user-chosen export atomically (temp file beside it, fsync,
/// rename) with ordinary permissions — unlike the app's private stores,
/// the folder and file belong to the person.
pub fn export_file(path: &Path, bytes: &[u8]) -> std::io::Result<()> {
    use std::io::Write;
    let dir = path.parent().filter(|p| !p.as_os_str().is_empty()).unwrap_or(Path::new("."));
    let name = path.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_else(|| "export".into());
    let tmp = dir.join(format!(".{name}.{}.part", std::process::id()));
    let res = (|| {
        let mut f = std::fs::File::create(&tmp)?;
        f.write_all(bytes)?;
        f.sync_all()?;
        std::fs::rename(&tmp, path)
    })();
    if res.is_err() {
        let _ = std::fs::remove_file(&tmp);
    }
    res
}

#[cfg(test)]
mod tests {
    use super::*;
    use time::macros::datetime;

    #[test]
    fn expiry_is_compared_in_time_not_text() {
        let now = datetime!(2026-10-05 12:00:00 UTC);
        assert!(scrap::is_expired("2026-10-05T11:59:59Z", now));
        assert!(!scrap::is_expired("2026-10-05T12:00:01Z", now));
        assert!(!scrap::is_expired("", now));
        assert!(!scrap::is_expired("garbage", now));
        assert_eq!(scrap::expiry_label("2026-10-05T15:00:00Z", now), "in 3h");
        assert_eq!(scrap::expiry_label("2026-10-04T15:00:00Z", now), "expired");
        assert_eq!(scrap::expiry_label("", now), "");
    }

    #[test]
    fn export_is_atomic_and_leaves_no_temp() {
        let d = tempfile::tempdir().unwrap();
        let p = d.path().join("code.svg");
        export_file(&p, b"one").unwrap();
        export_file(&p, b"two").unwrap();
        assert_eq!(std::fs::read(&p).unwrap(), b"two");
        assert_eq!(std::fs::read_dir(d.path()).unwrap().count(), 1);
    }
}
