//! blobs — content-addressed media. Uploads are raw bodies (not multipart),
//! sanitized server-side (GPS stripped, HEIC → JPEG) before hashing, so a
//! CID can only be known from the server's answer.

use crate::session::{Loaded, Session};
use crate::transport::{ApiError, ServiceClient, Versioned};
use crate::upload::{cancelled_or, file_body, mime_for, Progress};
use reqwest::{header, Method};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::path::Path;

pub const SERVICE: &str = "blobs";

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
#[serde(rename_all = "camelCase")]
pub struct Meta {
    pub cid: String,
    #[serde(default)]
    pub size: i64,
    #[serde(default)]
    pub mime: String,
    #[serde(default)]
    pub width: i64,
    #[serde(default)]
    pub height: i64,
    #[serde(default)]
    pub blurhash: String,
    #[serde(default)]
    pub dominant_color: String,
    #[serde(default)]
    pub thumb_cid: String,
    #[serde(default)]
    pub created_at: String,
}

impl Meta {
    pub fn is_image(&self) -> bool {
        self.mime.starts_with("image/")
    }
    /// The reference a document embeds.
    pub fn reference(&self) -> String {
        format!("blob://{}", self.cid)
    }
    /// Markdown to insert into a document for this blob.
    pub fn markdown(&self, alt: &str) -> String {
        if self.is_image() || self.mime.starts_with("video/") || self.mime.starts_with("audio/") {
            format!("![{alt}](blob://{})", self.cid)
        } else {
            format!("[{}](blob://{})", if alt.is_empty() { &self.cid } else { alt }, self.cid)
        }
    }
}

#[derive(Clone, Debug, Deserialize)]
pub struct BlobPage {
    pub blobs: Vec<Meta>,
    pub total: i64,
    pub page: i64,
    pub pages: i64,
}

/// A page of 48 blobs, newest first (the server's fixed page size).
pub async fn list(s: &Session, page: u32) -> Result<Loaded<BlobPage>, ApiError> {
    s.load(SERVICE, &format!("/blobs?page={}", page.max(1))).await
}

pub async fn meta(s: &Session, cid: &str) -> Result<Loaded<Meta>, ApiError> {
    s.load(SERVICE, &format!("/blobs/{cid}/meta")).await
}

/// Bytes of a blob (or its thumbnail), fetched over the private endpoint.
/// Immutable by CID, so callers may cache freely by CID.
pub async fn bytes(s: &Session, cid: &str, cap: usize) -> Result<bytes::Bytes, ApiError> {
    let c = s.client(SERVICE)?;
    let r = c.send(c.request(Method::GET, &format!("/blobs/{cid}"))?, false).await?;
    ServiceClient::body(r, cap).await
}

/// Upload one file, streamed with progress. Identical bytes return the
/// existing blob — uploads are idempotent by content, so a retry after a lost
/// response is safe.
pub async fn upload(s: &Session, path: &Path, progress: &Progress) -> Result<Versioned<Meta>, ApiError> {
    let c = s.client(SERVICE)?;
    let len = std::fs::metadata(path).map_err(|e| ApiError::BadRequest(format!("{}: {e}", path.display())))?.len();
    let rb = c
        .request(Method::POST, "/blobs")?
        .timeout(std::time::Duration::from_secs(90 + len / (128 << 10)))
        .header(header::CONTENT_TYPE, mime_for(path))
        .header(header::CONTENT_LENGTH, len.to_string())
        .body(file_body(path, 0, len, progress.clone()).await?);
    let resp = c.send(rb, true).await.map_err(|e| cancelled_or(progress, e))?;
    ServiceClient::json(resp).await
}

/// Delete a blob. With `unless_referenced` the server keeps it (409) if any
/// document still embeds it — the safe default from the UI.
pub async fn delete(s: &Session, cid: &str, unless_referenced: bool) -> Result<(), ApiError> {
    let mut path = format!("/blobs/{cid}");
    if unless_referenced {
        path.push_str("?unlessReferenced=1");
    }
    s.client(SERVICE)?.send_json::<Value>(Method::DELETE, &path, None, None).await.map(|_| ())
}

/// The public link to a blob, for sharing.
pub fn public_url(s: &Session, cid: &str) -> Option<String> {
    s.public_base(SERVICE).map(|b| format!("{}/blobs/{cid}", b.trim_end_matches('/')))
}
