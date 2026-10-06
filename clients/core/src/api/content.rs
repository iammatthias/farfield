//! content — collections, entries (drafts and published) and series.
//!
//! The entry PUT is a full replace on the server: an omitted `published`
//! unpublishes, an omitted `collection` is refused. So every write here sends
//! the whole record, read-modify-write, made safe by If-Match. Fields this
//! client does not know are carried through untouched (`extra`).

use crate::session::{Loaded, Session};
use crate::transport::{ApiError, Versioned};
use reqwest::Method;
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};

pub const SERVICE: &str = "content";

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct Collection {
    pub slug: String,
    pub name: String,
    #[serde(default)]
    pub description: String,
    #[serde(default)]
    pub created_at: String,
    #[serde(default)]
    pub entry_count: i64,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
#[serde(rename_all = "camelCase")]
pub struct Entry {
    pub collection: String,
    #[serde(default)]
    pub slug: String,
    #[serde(default)]
    pub cid: String,
    pub title: String,
    #[serde(default)]
    pub excerpt: String,
    #[serde(default)]
    pub body: String,
    #[serde(default)]
    pub tags: Vec<String>,
    #[serde(default)]
    pub published: bool,
    #[serde(default)]
    pub created_at: String,
    #[serde(default)]
    pub updated_at: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub published_at: String,
    /// Fields a newer server sends that this client does not model; sent
    /// back verbatim so a save never drops them.
    #[serde(flatten)]
    pub extra: Map<String, Value>,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Default)]
#[serde(rename_all = "camelCase")]
pub struct Series {
    pub slug: String,
    #[serde(default)]
    pub cid: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub body: String,
    #[serde(default)]
    pub created_at: String,
    #[serde(default)]
    pub updated_at: String,
    #[serde(flatten)]
    pub extra: Map<String, Value>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Status {
    Published,
    Drafts,
    All,
}

impl Status {
    fn as_str(self) -> &'static str {
        match self {
            Status::Published => "published",
            Status::Drafts => "draft",
            Status::All => "all",
        }
    }
}

#[derive(Deserialize)]
struct Collections {
    collections: Vec<Collection>,
}
#[derive(Deserialize)]
struct Entries {
    entries: Vec<Entry>,
}
#[derive(Deserialize)]
struct SeriesList {
    series: Vec<Series>,
}

/// One page of entries. The server sends no total: a short page is the end.
#[derive(Clone, Debug)]
pub struct Page<T> {
    pub items: Vec<T>,
    pub page: u32,
    pub has_more: bool,
}

pub fn entries_path(collection: Option<&str>, status: Status, page: u32, limit: u32, bodies: bool) -> String {
    let mut q = format!("/api/entries?status={}&page={}&limit={}", status.as_str(), page.max(1), limit);
    if let Some(c) = collection {
        q.push_str("&collection=");
        q.push_str(&url::form_urlencoded::byte_serialize(c.as_bytes()).collect::<String>());
    }
    if !bodies {
        q.push_str("&bodies=0");
    }
    q
}

fn enc(s: &str) -> String {
    url::form_urlencoded::byte_serialize(s.as_bytes()).collect()
}

pub async fn collections(s: &Session) -> Result<Loaded<Vec<Collection>>, ApiError> {
    let l = s.load::<Collections>(SERVICE, "/api/collections").await?;
    Ok(Loaded { value: l.value.collections, etag: l.etag, freshness: l.freshness })
}

/// A page of entries (slim: bodies omitted) for browsing.
pub async fn entries(
    s: &Session,
    collection: Option<&str>,
    status: Status,
    page: u32,
    limit: u32,
) -> Result<Loaded<Page<Entry>>, ApiError> {
    let l = s.load::<Entries>(SERVICE, &entries_path(collection, status, page, limit, false)).await?;
    let has_more = l.value.entries.len() as u32 >= limit;
    Ok(Loaded { value: Page { items: l.value.entries, page, has_more }, etag: l.etag, freshness: l.freshness })
}

/// One entry, with the version to send back as If-Match.
pub async fn entry(s: &Session, slug: &str) -> Result<Loaded<Entry>, ApiError> {
    s.load(SERVICE, &format!("/api/entries/{}", enc(slug))).await
}

pub async fn create(s: &Session, e: &Entry) -> Result<Versioned<Entry>, ApiError> {
    let mut v = serde_json::to_value(e).map_err(|e| ApiError::Decode(e.to_string()))?;
    // the server stamps the slug; send the requested one only if set
    if e.slug.is_empty() {
        v.as_object_mut().unwrap().remove("slug");
    }
    for k in ["cid", "updatedAt"] {
        v.as_object_mut().unwrap().remove(k);
    }
    if e.created_at.is_empty() {
        v.as_object_mut().unwrap().remove("createdAt");
    }
    let r = s.client(SERVICE)?.send_json(Method::POST, "/api/entries", Some(&v), None).await;
    invalidate_lists(s);
    r
}

/// Save the whole record. `if_match` is the version it was edited from; a
/// 412 comes back as `ApiError::Precondition` with the server's copy.
pub async fn update(s: &Session, slug: &str, e: &Entry, if_match: Option<&str>) -> Result<Versioned<Entry>, ApiError> {
    let v = serde_json::to_value(e).map_err(|e| ApiError::Decode(e.to_string()))?;
    let path = format!("/api/entries/{}", enc(slug));
    let r = s.client(SERVICE)?.send_json(Method::PUT, &path, Some(&v), if_match).await;
    s.invalidate(SERVICE, &path);
    invalidate_lists(s);
    r
}

/// Move to trash (30-day retention on the server). Explicit only.
pub async fn delete(s: &Session, slug: &str, if_match: Option<&str>) -> Result<(), ApiError> {
    let path = format!("/api/entries/{}", enc(slug));
    let r = s.client(SERVICE)?.send_json::<Value>(Method::DELETE, &path, None, if_match).await;
    s.invalidate(SERVICE, &path);
    invalidate_lists(s);
    r.map(|_| ())
}

pub async fn series_list(s: &Session) -> Result<Loaded<Vec<Series>>, ApiError> {
    let l = s.load::<SeriesList>(SERVICE, "/api/series").await?;
    Ok(Loaded { value: l.value.series, etag: l.etag, freshness: l.freshness })
}

pub async fn series(s: &Session, slug: &str) -> Result<Loaded<Series>, ApiError> {
    s.load(SERVICE, &format!("/api/series/{}", enc(slug))).await
}

pub async fn create_series(s: &Session, title: &str, slug: &str, body: &str) -> Result<Versioned<Series>, ApiError> {
    let v = serde_json::json!({"title": title, "slug": slug, "body": body});
    let r = s.client(SERVICE)?.send_json(Method::POST, "/api/series", Some(&v), None).await;
    s.invalidate(SERVICE, "/api/series");
    r
}

pub async fn update_series(
    s: &Session,
    slug: &str,
    title: &str,
    body: &str,
    if_match: Option<&str>,
) -> Result<Versioned<Series>, ApiError> {
    let v = serde_json::json!({"title": title, "body": body});
    let path = format!("/api/series/{}", enc(slug));
    let r = s.client(SERVICE)?.send_json(Method::PUT, &path, Some(&v), if_match).await;
    s.invalidate(SERVICE, &path);
    s.invalidate(SERVICE, "/api/series");
    r
}

/// Delete a series. The server refuses (409) while an entry still embeds it.
pub async fn delete_series(s: &Session, slug: &str, if_match: Option<&str>) -> Result<(), ApiError> {
    let path = format!("/api/series/{}", enc(slug));
    let r = s.client(SERVICE)?.send_json::<Value>(Method::DELETE, &path, None, if_match).await;
    s.invalidate(SERVICE, &path);
    s.invalidate(SERVICE, "/api/series");
    r.map(|_| ())
}

fn invalidate_lists(s: &Session) {
    // list ETags change with any write; the next load revalidates anyway,
    // but dropping the collections count avoids showing a stale number
    s.invalidate(SERVICE, "/api/collections");
}

/// Resolve `series://` embeds for display only — the stored body is never
/// rewritten. Nested series are followed with cycle detection; a series that
/// refers back to one already being expanded is left as its literal line.
pub async fn resolve_for_display(s: &Session, body: &str) -> String {
    let mut seen = std::collections::HashSet::new();
    Box::pin(expand(s, body, &mut seen, 0)).await
}

async fn expand(s: &Session, body: &str, seen: &mut std::collections::HashSet<String>, depth: usize) -> String {
    if depth > 8 {
        return body.to_string();
    }
    let mut out = String::with_capacity(body.len());
    for line in body.split_inclusive('\n') {
        let t = line.trim();
        let slug = series_ref(t);
        match slug {
            Some(slug) if !seen.contains(&slug) => match series(s, &slug).await {
                Ok(l) => {
                    seen.insert(slug.clone());
                    let inner = Box::pin(expand(s, &l.value.body, seen, depth + 1)).await;
                    seen.remove(&slug);
                    out.push_str(&inner);
                    if !inner.ends_with('\n') {
                        out.push('\n');
                    }
                }
                Err(_) => out.push_str(line),
            },
            _ => out.push_str(line),
        }
    }
    out
}

/// A line that is only a series reference: `![](series://slug)` or a bare
/// `series://slug`.
pub fn series_ref(line: &str) -> Option<String> {
    let inner = line
        .strip_prefix("![")
        .and_then(|r| r.split_once("](").map(|(_, u)| u))
        .and_then(|u| u.strip_suffix(')'))
        .unwrap_or(line);
    let slug = inner.strip_prefix("series://")?;
    (!slug.is_empty() && slug.chars().all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-'))
        .then(|| slug.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn unknown_fields_round_trip() {
        let j = r#"{"collection":"notes","slug":"1-a","cid":"b","title":"A","body":"x","tags":[],"published":true,
                    "createdAt":"t","updatedAt":"u","publishedAt":"p","futureField":{"k":1}}"#;
        let e: Entry = serde_json::from_str(j).unwrap();
        let back = serde_json::to_value(&e).unwrap();
        assert_eq!(back["futureField"]["k"], 1);
        assert_eq!(back["publishedAt"], "p");
    }

    #[test]
    fn series_refs() {
        assert_eq!(series_ref("![](series://spring-walk)").as_deref(), Some("spring-walk"));
        assert_eq!(series_ref("series://a1"), Some("a1".into()));
        assert_eq!(series_ref("text series://a1"), None);
        assert_eq!(series_ref("![](series://Bad)"), None);
    }

    #[test]
    fn entries_path_shape() {
        assert_eq!(
            entries_path(Some("notes & more"), Status::Drafts, 2, 50, false),
            "/api/entries?status=draft&page=2&limit=50&collection=notes+%26+more&bodies=0"
        );
    }
}
