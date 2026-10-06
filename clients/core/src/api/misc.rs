//! The smaller workspaces: bookmarks, qr, scrap, library, sideload, daily,
//! pulse, switchboard, backup, apex. Private data (private bookmarks,
//! disabled codes, paste management, the library catalog, share links,
//! message logs, snapshots) comes from each app's `/api/admin/*` routes, which
//! answer only the write key and only over private ingress.

use crate::session::{Loaded, Session};
use crate::transport::{ApiError, ServiceClient, Versioned};
use crate::upload::{cancelled_or, file_body, Progress};
use reqwest::{header, Method};
use serde::{Deserialize, Serialize};
use serde_json::{json, Map, Value};
use std::path::Path;

fn enc(s: &str) -> String {
    url::form_urlencoded::byte_serialize(s.as_bytes()).collect()
}

// ── bookmarks ────────────────────────────────────────────────────────────

pub mod bookmarks {
    use super::*;
    pub const SERVICE: &str = "bookmarks";

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(rename_all = "camelCase")]
    pub struct Bookmark {
        #[serde(default)]
        pub id: String,
        pub url: String,
        #[serde(default)]
        pub title: String,
        #[serde(default)]
        pub description: String,
        #[serde(default)]
        pub category: String,
        #[serde(default)]
        pub public: bool,
        #[serde(default)]
        pub admin_notes: String,
        #[serde(default)]
        pub og_title: String,
        #[serde(default)]
        pub og_description: String,
        #[serde(default)]
        pub og_image: String,
        #[serde(default)]
        pub og_site_name: String,
        #[serde(default)]
        pub favicon: String,
        #[serde(default)]
        pub cid: String,
        #[serde(default)]
        pub created_at: String,
        #[serde(default)]
        pub updated_at: String,
        #[serde(flatten)]
        pub extra: Map<String, Value>,
    }

    #[derive(Deserialize)]
    struct List {
        bookmarks: Vec<Bookmark>,
    }

    /// Every bookmark, private ones and notes included (admin route).
    pub async fn all(s: &Session) -> Result<Loaded<Vec<Bookmark>>, ApiError> {
        let l = s.load::<List>(SERVICE, "/api/admin/bookmarks").await?;
        Ok(Loaded { value: l.value.bookmarks, etag: l.etag, freshness: l.freshness })
    }

    pub async fn get(s: &Session, id: &str) -> Result<Loaded<Bookmark>, ApiError> {
        s.load(SERVICE, &format!("/api/admin/bookmarks/{}", enc(id))).await
    }

    pub async fn create(s: &Session, b: &Bookmark) -> Result<Versioned<Bookmark>, ApiError> {
        let mut v = serde_json::to_value(b).map_err(|e| ApiError::Decode(e.to_string()))?;
        for k in ["id", "cid", "createdAt", "updatedAt"] {
            v.as_object_mut().unwrap().remove(k);
        }
        let r = s.client(SERVICE)?.send_json(Method::POST, "/api/bookmarks", Some(&v), None).await;
        s.invalidate(SERVICE, "/api/admin/bookmarks");
        r
    }

    /// Partial update: only the fields given change on the server.
    pub async fn update(s: &Session, id: &str, fields: &Value, if_match: Option<&str>) -> Result<Versioned<Bookmark>, ApiError> {
        let r = s.client(SERVICE)?.send_json(Method::PUT, &format!("/api/bookmarks/{}", enc(id)), Some(fields), if_match).await;
        s.invalidate(SERVICE, "/api/admin/bookmarks");
        s.invalidate(SERVICE, &format!("/api/admin/bookmarks/{}", enc(id)));
        r
    }

    pub async fn delete(s: &Session, id: &str, if_match: Option<&str>) -> Result<(), ApiError> {
        let r = s.client(SERVICE)?.send_json::<Value>(Method::DELETE, &format!("/api/bookmarks/{}", enc(id)), None, if_match).await;
        s.invalidate(SERVICE, "/api/admin/bookmarks");
        r.map(|_| ())
    }

    /// Re-fetch the page's title, description and image now.
    pub async fn refresh(s: &Session, id: &str) -> Result<Versioned<Bookmark>, ApiError> {
        let r = s
            .client(SERVICE)?
            .send_json(Method::POST, &format!("/api/admin/bookmarks/{}/refresh", enc(id)), None, None)
            .await;
        s.invalidate(SERVICE, "/api/admin/bookmarks");
        r
    }

    /// Categories in use, for the picker (categories are plain strings).
    pub fn categories(all: &[Bookmark]) -> Vec<String> {
        let mut c: Vec<String> = all.iter().map(|b| b.category.clone()).filter(|c| !c.is_empty()).collect();
        c.sort();
        c.dedup();
        c
    }
}

// ── qr ───────────────────────────────────────────────────────────────────

pub mod qr {
    use super::*;
    pub const SERVICE: &str = "qr";

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(rename_all = "camelCase")]
    pub struct Code {
        #[serde(default)]
        pub id: String,
        #[serde(default)]
        pub label: String,
        /// "direct" encodes the target; "proxy" encodes a /r/{id} redirect
        /// whose destination can change later.
        #[serde(default)]
        pub mode: String,
        #[serde(default)]
        pub target: String,
        #[serde(default)]
        pub ec: String,
        #[serde(default)]
        pub public: bool,
        #[serde(default)]
        pub enabled: bool,
        #[serde(default)]
        pub admin_notes: String,
        #[serde(default)]
        pub cid: String,
        #[serde(default)]
        pub created_at: String,
        #[serde(default)]
        pub updated_at: String,
        #[serde(flatten)]
        pub extra: Map<String, Value>,
    }

    #[derive(Deserialize)]
    struct List {
        codes: Vec<Code>,
    }

    pub async fn all(s: &Session) -> Result<Loaded<Vec<Code>>, ApiError> {
        let l = s.load::<List>(SERVICE, "/api/admin/codes").await?;
        Ok(Loaded { value: l.value.codes, etag: l.etag, freshness: l.freshness })
    }

    pub async fn create(s: &Session, c: &Code) -> Result<Versioned<Code>, ApiError> {
        let mut v = serde_json::to_value(c).map_err(|e| ApiError::Decode(e.to_string()))?;
        for k in ["id", "cid", "createdAt", "updatedAt"] {
            v.as_object_mut().unwrap().remove(k);
        }
        let r = s.client(SERVICE)?.send_json(Method::POST, "/api/codes", Some(&v), None).await;
        s.invalidate(SERVICE, "/api/admin/codes");
        r
    }

    pub async fn update(s: &Session, id: &str, fields: &Value, if_match: Option<&str>) -> Result<Versioned<Code>, ApiError> {
        let r = s.client(SERVICE)?.send_json(Method::PUT, &format!("/api/codes/{}", enc(id)), Some(fields), if_match).await;
        s.invalidate(SERVICE, "/api/admin/codes");
        r
    }

    pub async fn delete(s: &Session, id: &str, if_match: Option<&str>) -> Result<(), ApiError> {
        let r = s.client(SERVICE)?.send_json::<Value>(Method::DELETE, &format!("/api/codes/{}", enc(id)), None, if_match).await;
        s.invalidate(SERVICE, "/api/admin/codes");
        r.map(|_| ())
    }

    /// The code's image, whatever its visibility (admin preview).
    pub async fn preview(s: &Session, id: &str, png_size: Option<u32>) -> Result<bytes::Bytes, ApiError> {
        let c = s.client(SERVICE)?;
        let path = match png_size {
            Some(n) => format!("/api/admin/codes/{}/preview.png?size={n}", enc(id)),
            None => format!("/api/admin/codes/{}/preview.svg", enc(id)),
        };
        let r = c.send(c.request(Method::GET, &path)?, false).await?;
        ServiceClient::body(r, 8 << 20).await
    }

    /// What a scan of a proxy code opens, for sharing.
    pub fn redirect_url(s: &Session, id: &str) -> Option<String> {
        s.public_base(SERVICE).map(|b| format!("{}/r/{id}", b.trim_end_matches('/')))
    }
}

// ── scrap ────────────────────────────────────────────────────────────────

pub mod scrap {
    use super::*;
    pub const SERVICE: &str = "scrap";

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(rename_all = "camelCase")]
    pub struct Paste {
        pub id: String,
        #[serde(default)]
        pub cid: String,
        #[serde(default)]
        pub title: String,
        #[serde(default)]
        pub lang: String,
        #[serde(default)]
        pub body: String,
        #[serde(default)]
        pub visibility: String,
        #[serde(default)]
        pub expires_at: String,
        #[serde(default)]
        pub created_at: String,
        #[serde(default)]
        pub views: i64,
        #[serde(default)]
        pub alias: String,
        #[serde(default)]
        pub has_token: bool,
    }

    #[derive(Deserialize, Clone, Debug)]
    pub struct PastePage {
        pub pastes: Vec<Paste>,
        #[serde(default)]
        pub total: i64,
    }

    pub const EXPIRIES: [&str; 5] = ["never", "1h", "1d", "1w", "1m"];
    pub const VISIBILITIES: [&str; 3] = ["public", "unlisted", "private"];

    pub async fn list(s: &Session, page: u32, limit: u32) -> Result<Loaded<PastePage>, ApiError> {
        s.load(SERVICE, &format!("/api/admin/pastes?page={}&limit={limit}", page.max(1))).await
    }

    pub async fn get(s: &Session, id: &str) -> Result<Loaded<Paste>, ApiError> {
        s.load(SERVICE, &format!("/api/admin/pastes/{}", enc(id))).await
    }

    /// What creating a paste returns: its link and, when asked for, the
    /// magic-link token (shown once).
    #[derive(Clone, Debug, PartialEq)]
    pub struct Created {
        pub url: String,
        pub id: String,
        pub token: Option<String>,
    }

    pub fn parse_created(text: &str) -> Result<Created, ApiError> {
        let mut lines = text.lines();
        let url = lines.next().unwrap_or("").trim().to_string();
        if !url.starts_with("http") {
            return Err(ApiError::Decode(format!("scrap: unexpected reply {text:?}")));
        }
        let id = url.rsplit('/').next().unwrap_or("").to_string();
        let token = lines.find_map(|l| l.strip_prefix("token: ").map(|t| t.trim().to_string()));
        Ok(Created { url, id, token })
    }

    /// Create (or, for an identical body, update) a paste. Idempotent per
    /// body on the server, so safe to retry.
    pub async fn create(
        s: &Session,
        body: &str,
        title: &str,
        lang: &str,
        visibility: &str,
        expires: &str,
        magic_link: bool,
    ) -> Result<Created, ApiError> {
        let c = s.client(SERVICE)?;
        let mut path = format!(
            "/api/pastes?title={}&lang={}&visibility={}&expires={}",
            enc(title),
            enc(lang),
            enc(visibility),
            enc(expires)
        );
        if magic_link {
            path.push_str("&token=generate");
        }
        let rb = c.request(Method::POST, &path)?.header(header::CONTENT_TYPE, "text/plain; charset=utf-8").body(body.to_string());
        let r = c.send(rb, true).await?;
        let text = String::from_utf8_lossy(&ServiceClient::body(r, 1 << 20).await?).to_string();
        s.invalidate(SERVICE, "/api/admin/pastes?page=1&limit=50");
        parse_created(&text)
    }

    /// Change title, language, visibility or expiry.
    pub async fn update(s: &Session, id: &str, fields: &Value) -> Result<Versioned<Paste>, ApiError> {
        let r = s.client(SERVICE)?.send_json(Method::PUT, &format!("/api/admin/pastes/{}", enc(id)), Some(fields), None).await;
        s.invalidate(SERVICE, &format!("/api/admin/pastes/{}", enc(id)));
        r
    }

    pub async fn delete(s: &Session, id: &str) -> Result<(), ApiError> {
        s.client(SERVICE)?.send_json::<Value>(Method::DELETE, &format!("/api/pastes/{}", enc(id)), None, None).await.map(|_| ())
    }

    /// Issue a new magic-link token, invalidating the old one. Returns it
    /// (shown once).
    pub async fn rotate_token(s: &Session, id: &str) -> Result<String, ApiError> {
        let c = s.client(SERVICE)?;
        let r = c.send(c.request(Method::POST, &format!("/api/pastes/{}/token/roll", enc(id)))?, true).await?;
        let text = String::from_utf8_lossy(&ServiceClient::body(r, 64 << 10).await?).to_string();
        text.lines()
            .find_map(|l| l.strip_prefix("token: ").map(|t| t.trim().to_string()))
            .ok_or_else(|| ApiError::Decode("scrap: no token in reply".into()))
    }

    /// Revoke the magic link entirely.
    pub async fn revoke_token(s: &Session, id: &str) -> Result<(), ApiError> {
        s.client(SERVICE)?
            .send_json::<Value>(Method::DELETE, &format!("/api/pastes/{}/token", enc(id)), None, None)
            .await
            .map(|_| ())
    }

    /// The share link (with the token, when there is one to hand out).
    pub fn share_url(s: &Session, id: &str, token: Option<&str>) -> Option<String> {
        let base = s.public_base(SERVICE)?;
        Some(match token {
            Some(t) => format!("{}/{id}?t={}", base.trim_end_matches('/'), enc(t)),
            None => format!("{}/{id}", base.trim_end_matches('/')),
        })
    }
}

// ── library ──────────────────────────────────────────────────────────────

pub mod library {
    use super::*;
    pub const SERVICE: &str = "library";

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(rename_all = "camelCase")]
    pub struct Book {
        pub cid: String,
        #[serde(default)]
        pub title: String,
        #[serde(default)]
        pub author: String,
        #[serde(default)]
        pub language: String,
        #[serde(default)]
        pub identifier: String,
        #[serde(default)]
        pub description: String,
        #[serde(default)]
        pub collection: String,
        #[serde(default)]
        pub filename: String,
        #[serde(default)]
        pub size: i64,
        #[serde(default)]
        pub cover_cid: String,
        #[serde(default)]
        pub cover_mime: String,
        #[serde(default)]
        pub thumb_cid: String,
        #[serde(default)]
        pub created_at: String,
    }

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
    pub struct CollectionCount {
        pub name: String,
        pub count: i64,
    }

    #[derive(Clone, Debug, Deserialize)]
    pub struct Catalog {
        pub books: Vec<Book>,
        #[serde(default)]
        pub collections: Vec<CollectionCount>,
    }

    pub async fn catalog(s: &Session) -> Result<Loaded<Catalog>, ApiError> {
        s.load(SERVICE, "/api/admin/books").await
    }

    /// Move a book to a collection ("" = uncategorized).
    pub async fn set_collection(s: &Session, cid: &str, collection: &str) -> Result<Versioned<Book>, ApiError> {
        let c = s.client(SERVICE)?;
        let r = c
            .send(c.request(Method::PUT, &format!("/api/books/{cid}/collection?collection={}", enc(collection)))?, true)
            .await?;
        s.invalidate(SERVICE, "/api/admin/books");
        ServiceClient::json(r).await
    }

    pub async fn delete(s: &Session, cid: &str) -> Result<(), ApiError> {
        let r = s.client(SERVICE)?.send_json::<Value>(Method::DELETE, &format!("/api/books/{cid}"), None, None).await;
        s.invalidate(SERVICE, "/api/admin/books");
        r.map(|_| ())
    }

    pub async fn cover(s: &Session, cid: &str) -> Result<bytes::Bytes, ApiError> {
        let c = s.client(SERVICE)?;
        let r = c.send(c.request(Method::GET, &format!("/opds/cover/{cid}"))?, false).await?;
        ServiceClient::body(r, 8 << 20).await
    }

    pub fn invalidate(s: &Session) {
        s.invalidate(SERVICE, "/api/admin/books");
    }
}

// ── sideload ─────────────────────────────────────────────────────────────

pub mod sideload {
    use super::*;
    pub const SERVICE: &str = "sideload";

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(rename_all = "camelCase")]
    pub struct Build {
        pub id: String,
        #[serde(default)]
        pub cid: String,
        #[serde(default)]
        pub bundle_id: String,
        #[serde(default)]
        pub app_name: String,
        #[serde(default)]
        pub version: String,
        #[serde(default)]
        pub build_number: String,
        #[serde(default)]
        pub team: String,
        #[serde(default)]
        pub profile_expiry: String,
        #[serde(default)]
        pub device_count: i64,
        #[serde(default)]
        pub size_bytes: i64,
        #[serde(default)]
        pub filename: String,
        #[serde(default)]
        pub git_commit: String,
        #[serde(default)]
        pub notes: String,
        #[serde(default)]
        pub created_at: String,
        #[serde(default, rename = "installURL")]
        pub install_url: String,
    }

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(rename_all = "camelCase")]
    pub struct Share {
        pub token: String,
        #[serde(default, rename = "shareURL", alias = "shareUrl")]
        pub share_url: String,
        #[serde(default)]
        pub expires_at: String,
        #[serde(default)]
        pub max_installs: i64,
        #[serde(flatten)]
        pub extra: Map<String, Value>,
    }

    #[derive(Deserialize)]
    struct Builds {
        builds: Vec<Build>,
    }
    #[derive(Deserialize)]
    struct Shares {
        shares: Vec<Share>,
    }

    pub async fn builds(s: &Session) -> Result<Loaded<Vec<Build>>, ApiError> {
        let l = s.load::<Builds>(SERVICE, "/api/builds").await?;
        Ok(Loaded { value: l.value.builds, etag: l.etag, freshness: l.freshness })
    }

    /// Group builds by app (bundle id), newest build first in each.
    pub fn apps(builds: &[Build]) -> Vec<(String, Vec<Build>)> {
        let mut m: std::collections::BTreeMap<String, Vec<Build>> = Default::default();
        for b in builds {
            m.entry(b.bundle_id.clone()).or_default().push(b.clone());
        }
        let mut out: Vec<(String, Vec<Build>)> = m.into_iter().collect();
        for (_, v) in out.iter_mut() {
            v.sort_by(|a, b| b.created_at.cmp(&a.created_at));
        }
        out.sort_by(|a, b| b.1[0].created_at.cmp(&a.1[0].created_at));
        out
    }

    /// Upload an IPA, streamed. Idempotent by content (same IPA = same build).
    pub async fn upload(s: &Session, path: &Path, notes: &str, progress: &Progress) -> Result<Versioned<Build>, ApiError> {
        let c = s.client(SERVICE)?;
        let len = std::fs::metadata(path).map_err(|e| ApiError::BadRequest(e.to_string()))?.len();
        let name = path.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default();
        let rb = c
            .request(Method::POST, &format!("/api/builds?filename={}&notes={}", enc(&name), enc(notes)))?
            .timeout(std::time::Duration::from_secs(120 + len / (128 << 10)))
            .header(header::CONTENT_TYPE, "application/octet-stream")
            .header(header::CONTENT_LENGTH, len.to_string())
            .body(file_body(path, 0, len, progress.clone()).await?);
        let r = c.send(rb, true).await.map_err(|e| cancelled_or(progress, e))?;
        s.invalidate(SERVICE, "/api/builds");
        ServiceClient::json(r).await
    }

    pub async fn delete_build(s: &Session, id: &str) -> Result<(), ApiError> {
        let r = s.client(SERVICE)?.send_json::<Value>(Method::DELETE, &format!("/api/builds/{}", enc(id)), None, None).await;
        s.invalidate(SERVICE, "/api/builds");
        r.map(|_| ())
    }

    pub async fn delete_app(s: &Session, bundle: &str) -> Result<(), ApiError> {
        let r = s.client(SERVICE)?.send_json::<Value>(Method::DELETE, &format!("/api/apps/{}", enc(bundle)), None, None).await;
        s.invalidate(SERVICE, "/api/builds");
        r.map(|_| ())
    }

    /// Mint an expiring install link. ttl: 30m|2h|24h; max: 1|3|unlimited.
    pub async fn share(s: &Session, id: &str, ttl: &str, max: &str, label: &str) -> Result<Versioned<Share>, ApiError> {
        let path = format!("/api/builds/{}/share?ttl={}&max={}&label={}", enc(id), enc(ttl), enc(max), enc(label));
        let r = s.client(SERVICE)?.send_json(Method::POST, &path, None, None).await;
        s.invalidate(SERVICE, "/api/admin/shares");
        r
    }

    pub async fn shares(s: &Session) -> Result<Loaded<Vec<Share>>, ApiError> {
        let l = s.load::<Shares>(SERVICE, "/api/admin/shares").await?;
        Ok(Loaded { value: l.value.shares, etag: l.etag, freshness: l.freshness })
    }

    pub async fn revoke_share(s: &Session, token: &str) -> Result<(), ApiError> {
        let r = s
            .client(SERVICE)?
            .send_json::<Value>(Method::POST, &format!("/api/admin/shares/{}/revoke", enc(token)), None, None)
            .await;
        s.invalidate(SERVICE, "/api/admin/shares");
        r.map(|_| ())
    }

    /// The canonical install page for a build (public host).
    pub fn install_url(s: &Session, b: &Build) -> Option<String> {
        let base = s.public_base(SERVICE)?;
        let path = if b.install_url.is_empty() { format!("/b/{}", b.id) } else { b.install_url.clone() };
        Some(format!("{}{}", base.trim_end_matches('/'), path))
    }
}

// ── daily ────────────────────────────────────────────────────────────────

pub mod daily {
    use super::*;
    pub const SERVICE: &str = "daily";

    #[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
    #[serde(rename_all = "camelCase")]
    pub struct Photo {
        #[serde(default)]
        pub source: String,
        pub date: String,
        #[serde(default)]
        pub cid: String,
        #[serde(default)]
        pub title: String,
        #[serde(default)]
        pub explanation: String,
        #[serde(default)]
        pub image_url: String,
        #[serde(default)]
        pub thumb_url: String,
        #[serde(default)]
        pub media_type: String,
        #[serde(default)]
        pub credit: String,
        #[serde(default)]
        pub source_url: String,
        #[serde(default)]
        pub placeholder: bool,
    }

    #[derive(Clone, Debug, Deserialize)]
    pub struct Day {
        pub photo: Photo,
        #[serde(default)]
        pub prev: String,
        #[serde(default)]
        pub next: String,
    }

    #[derive(Clone, Debug, Deserialize)]
    pub struct Archive {
        pub page: i64,
        pub pages: i64,
        pub total: i64,
        pub photos: Vec<Photo>,
    }

    pub async fn today(s: &Session) -> Result<Loaded<Day>, ApiError> {
        s.load(SERVICE, "/api/photo").await
    }
    pub async fn day(s: &Session, date: &str) -> Result<Loaded<Day>, ApiError> {
        s.load(SERVICE, &format!("/api/photo/{}", enc(date))).await
    }
    pub async fn archive(s: &Session, page: u32) -> Result<Loaded<Archive>, ApiError> {
        s.load(SERVICE, &format!("/api/photos?page={}", page.max(1))).await
    }
    /// The day's generative artifact descriptor (`/api/art[/date]`).
    pub async fn art(s: &Session, date: Option<&str>) -> Result<Loaded<Value>, ApiError> {
        match date {
            Some(d) => s.load(SERVICE, &format!("/api/art/{}", enc(d))).await,
            None => s.load(SERVICE, "/api/art").await,
        }
    }
    /// The art plate as SVG.
    pub async fn art_svg(s: &Session, date: Option<&str>) -> Result<bytes::Bytes, ApiError> {
        let c = s.client(SERVICE)?;
        let path = match date {
            Some(d) => format!("/art/{}", enc(d)),
            None => "/art.svg".into(),
        };
        let r = c.send(c.request(Method::GET, &path)?, false).await?;
        ServiceClient::body(r, 8 << 20).await
    }
}

// ── pulse ────────────────────────────────────────────────────────────────

pub mod pulse {
    use super::*;
    pub const SERVICE: &str = "pulse";

    #[derive(Clone, Debug, Deserialize)]
    pub struct Overview {
        pub targets: Vec<Value>,
        #[serde(default)]
        pub incidents: Vec<Value>,
    }

    pub async fn overview(s: &Session) -> Result<Loaded<Overview>, ApiError> {
        s.load(SERVICE, "/api/overview").await
    }

    pub async fn traffic(s: &Session, app: &str, from: &str, to: &str) -> Result<Loaded<Value>, ApiError> {
        s.load(SERVICE, &format!("/api/traffic?app={}&from={}&to={}", enc(app), enc(from), enc(to))).await
    }
}

// ── switchboard, backup, apex ────────────────────────────────────────────

pub mod switchboard {
    use super::*;
    pub const SERVICE: &str = "switchboard";
    pub async fn messages(s: &Session, limit: u32) -> Result<Loaded<Value>, ApiError> {
        s.load(SERVICE, &format!("/api/admin/messages?limit={limit}")).await
    }
    pub async fn jobs(s: &Session) -> Result<Loaded<Value>, ApiError> {
        s.load(SERVICE, "/api/admin/jobs").await
    }
}

pub mod backup {
    use super::*;
    pub const SERVICE: &str = "backup";
    pub async fn snapshots(s: &Session) -> Result<Loaded<Value>, ApiError> {
        s.load(SERVICE, "/api/admin/snapshots").await
    }
}

pub mod apex {
    use super::*;
    pub const SERVICE: &str = "apex";
    pub async fn profile(s: &Session) -> Result<Loaded<Value>, ApiError> {
        s.load(SERVICE, "/api/profile").await
    }
}

/// `GET /status` for one service, unauthenticated — health only.
pub async fn status(s: &Session, service: &str) -> Result<Value, ApiError> {
    let c = s.client(service)?;
    let r = c.send(c.request(Method::GET, "/status")?.header(header::ACCEPT, "application/json"), false).await?;
    let v: Versioned<Value> = ServiceClient::json(r).await?;
    if v.value["ok"] != json!(true) {
        return Err(ApiError::Server { status: 200, message: "status not ok".into() });
    }
    Ok(v.value)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn scrap_created_reply() {
        let c = scrap::parse_created("https://scrap.farfield.systems/abc123\ntoken: s3cr3t\n").unwrap();
        assert_eq!(c.id, "abc123");
        assert_eq!(c.token.as_deref(), Some("s3cr3t"));
        assert!(scrap::parse_created("oops").is_err());
    }
}
