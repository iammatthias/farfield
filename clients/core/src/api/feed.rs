//! feed — short posts with media. Media goes up through feed's own multipart
//! endpoint, which stores each file in blobs and appends `![](blob://cid)`.

use crate::session::{Loaded, Session};
use crate::transport::{ApiError, Versioned};
use crate::upload::{cancelled_or, file_body, mime_for, Progress};
use reqwest::Method;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};
use std::path::PathBuf;

pub const SERVICE: &str = "feed";

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
#[serde(rename_all = "camelCase")]
pub struct Post {
    pub slug: String,
    #[serde(default)]
    pub cid: String,
    pub body: String,
    #[serde(default)]
    pub tags: Vec<String>,
    #[serde(default)]
    pub created_at: String,
    #[serde(default)]
    pub updated_at: String,
    #[serde(flatten)]
    pub extra: Map<String, Value>,
}

impl Post {
    /// The keyset cursor for the page after this post.
    pub fn cursor(&self) -> String {
        format!("{}|{}", self.created_at, self.slug)
    }
}

#[derive(Deserialize)]
struct Posts {
    posts: Vec<Post>,
}

fn enc(s: &str) -> String {
    url::form_urlencoded::byte_serialize(s.as_bytes()).collect()
}

/// A page of posts, newest first. `before` is the previous page's last
/// `Post::cursor()`; the server sends no `next`, so a short page is the end.
pub async fn posts(s: &Session, before: Option<&str>, limit: u32) -> Result<Loaded<(Vec<Post>, bool)>, ApiError> {
    let mut path = format!("/api/posts?limit={limit}");
    if let Some(b) = before {
        path.push_str("&before=");
        path.push_str(&enc(b));
    }
    let l = s.load::<Posts>(SERVICE, &path).await?;
    let more = l.value.posts.len() as u32 >= limit;
    Ok(Loaded { value: (l.value.posts, more), etag: l.etag, freshness: l.freshness })
}

pub async fn post(s: &Session, slug: &str) -> Result<Loaded<Post>, ApiError> {
    s.load(SERVICE, &format!("/api/posts/{}", enc(slug))).await
}

/// Publish a text post. Not idempotent: never retried automatically.
pub async fn create(s: &Session, body: &str, tags: &[String]) -> Result<Versioned<Post>, ApiError> {
    let v = serde_json::json!({"body": body, "tags": tags});
    s.client(SERVICE)?.send_json(Method::POST, "/api/posts", Some(&v), None).await
}

/// Publish a post with media files, streamed from disk with progress.
pub async fn create_with_media(
    s: &Session,
    body: &str,
    tags: &[String],
    files: &[PathBuf],
    progress: &Progress,
) -> Result<Versioned<Post>, ApiError> {
    let c = s.client(SERVICE)?;
    let mut form = reqwest::multipart::Form::new().text("body", body.to_string()).text("tags", tags.join(","));
    for f in files {
        let len = std::fs::metadata(f).map_err(|e| ApiError::BadRequest(format!("{}: {e}", f.display())))?.len();
        let part = reqwest::multipart::Part::stream_with_length(file_body(f, 0, len, progress.clone()).await?, len)
            .file_name(f.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_else(|| "upload".into()))
            .mime_str(mime_for(f))
            .map_err(|e| ApiError::BadRequest(e.to_string()))?;
        form = form.part("file", part);
    }
    // a generous ceiling: 90s plus a second per 128 KiB, as lib/capability does
    let rb = c
        .request(Method::POST, "/api/posts/media")?
        .timeout(std::time::Duration::from_secs(90 + progress.total() / (128 << 10)))
        .multipart(form);
    let resp = c.send(rb, true).await.map_err(|e| cancelled_or(progress, e))?;
    crate::transport::ServiceClient::json(resp).await
}

/// Replace a post's body and tags (full replace; omitted tags clear them).
pub async fn update(s: &Session, slug: &str, body: &str, tags: &[String], if_match: Option<&str>) -> Result<Versioned<Post>, ApiError> {
    let v = serde_json::json!({"body": body, "tags": tags});
    let path = format!("/api/posts/{}", enc(slug));
    let r = s.client(SERVICE)?.send_json(Method::PUT, &path, Some(&v), if_match).await;
    s.invalidate(SERVICE, &path);
    r
}

/// Delete a post; with `release_media` its blobs go too, unless something
/// else still references them (the server checks).
pub async fn delete(s: &Session, slug: &str, release_media: bool, if_match: Option<&str>) -> Result<(), ApiError> {
    let mut path = format!("/api/posts/{}", enc(slug));
    s.invalidate(SERVICE, &path);
    if release_media {
        path.push_str("?media=release");
    }
    s.client(SERVICE)?.send_json::<Value>(Method::DELETE, &path, None, if_match).await.map(|_| ())
}

/// `#hashtags` at the end of a post become tags, the way the capability
/// `/feed` command reads them.
pub fn split_hashtags(text: &str) -> (String, Vec<String>) {
    let mut words: Vec<&str> = text.trim_end().split(' ').collect();
    let mut tags = Vec::new();
    while let Some(w) = words.last() {
        if let Some(t) = w.strip_prefix('#') {
            if !t.is_empty() && t.chars().all(|c| c.is_alphanumeric() || c == '-' || c == '_') {
                tags.insert(0, t.to_lowercase());
                words.pop();
                continue;
            }
        }
        break;
    }
    (words.join(" ").trim_end().to_string(), tags)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn hashtags() {
        assert_eq!(split_hashtags("hello world #a #B-c"), ("hello world".into(), vec!["a".into(), "b-c".into()]));
        assert_eq!(split_hashtags("#1 in line"), ("#1 in line".into(), vec![]));
    }
}
