//! Extra client functions for the media workspaces (feed and blobs): reading
//! the media a post embeds, bucketing blobs by kind, and the guarded delete's
//! answer as a value rather than an error.

use crate::api::blobs::{self, Meta};
use crate::session::Session;
use crate::transport::ApiError;

/// A blob's broad kind, the way the blobs console buckets it.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum MediaKind {
    Image,
    Video,
    Audio,
    Other,
}

impl MediaKind {
    pub fn of(mime: &str) -> Self {
        if mime.starts_with("image/") {
            MediaKind::Image
        } else if mime.starts_with("video/") {
            MediaKind::Video
        } else if mime.starts_with("audio/") {
            MediaKind::Audio
        } else {
            MediaKind::Other
        }
    }
    pub fn word(self) -> &'static str {
        match self {
            MediaKind::Image => "image",
            MediaKind::Video => "video",
            MediaKind::Audio => "audio",
            MediaKind::Other => "file",
        }
    }
}

/// True for the shape of a blobs CID (base32 CIDv1, as feed and blobs match it).
fn is_cid(s: &str) -> bool {
    s.len() > 20 && s.starts_with('b') && s.bytes().all(|c| c.is_ascii_lowercase() || (b'2'..=b'7').contains(&c))
}

/// The blob CIDs a body embeds, in the order they appear, each once.
pub fn blob_refs(body: &str) -> Vec<String> {
    let mut out: Vec<String> = Vec::new();
    let mut rest = body;
    while let Some(i) = rest.find("blob://") {
        let after = &rest[i + 7..];
        let end = after.find(|c: char| !(c.is_ascii_lowercase() || c.is_ascii_digit())).unwrap_or(after.len());
        let cid = &after[..end];
        if is_cid(cid) && !out.iter().any(|c| c == cid) {
            out.push(cid.to_string());
        }
        rest = &after[end..];
    }
    out
}

/// A post's text without its image embed lines (`![…](blob://…)` on a line
/// of its own) — what a timeline shows above the thumbnails.
pub fn strip_embeds(body: &str) -> String {
    let kept: Vec<&str> = body
        .lines()
        .filter(|l| {
            let t = l.trim();
            !(t.starts_with("![") && t.ends_with(')') && t.contains("](blob://"))
        })
        .collect();
    let mut s = kept.join("\n");
    // collapse the blank lines the embeds left behind
    while s.contains("\n\n\n") {
        s = s.replace("\n\n\n", "\n\n");
    }
    s.trim().to_string()
}

/// The CID to fetch for a small preview of `m`: its generated thumbnail, or
/// the blob itself when it is an image small enough to need none.
pub fn thumb_source(m: &Meta) -> Option<String> {
    if !m.thumb_cid.is_empty() {
        return Some(m.thumb_cid.clone());
    }
    if m.is_image() && m.mime != "image/svg+xml" && (m.size == 0 || m.size <= 4 << 20) {
        return Some(m.cid.clone());
    }
    None
}

/// The reference count in a guarded delete's 409, when the server sent one
/// (it does not when it could not see every source).
pub fn references_in(e: &ApiError) -> Option<u64> {
    match e {
        ApiError::Conflict { body, .. } => body.get("references").and_then(|v| v.as_u64()),
        _ => None,
    }
}

/// What a guarded delete did.
#[derive(Clone, Debug, PartialEq)]
pub enum Release {
    Deleted,
    /// Kept by the server. `references` is how many documents embed it, when
    /// known; `message` is the server's reason, worth showing verbatim.
    Kept {
        references: Option<u64>,
        message: String,
    },
}

/// Delete a blob unless something still embeds it. A refusal is an answer,
/// not a failure: the server keeps what it cannot prove is an orphan.
pub async fn delete_unless_referenced(s: &Session, cid: &str) -> Result<Release, ApiError> {
    match blobs::delete(s, cid, true).await {
        Ok(()) => Ok(Release::Deleted),
        Err(e @ ApiError::Conflict { .. }) => {
            let references = references_in(&e);
            let ApiError::Conflict { message, .. } = e else { unreachable!() };
            Ok(Release::Kept { references, message })
        }
        Err(e) => Err(e),
    }
}

/// The blobs list's fixed page size.
pub const BLOBS_PAGE: i64 = 48;

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    const A: &str = "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy";
    const B: &str = "bafkreidz3xnm7lfyxqsefb5kdeymtfmzbg4qwfgvx5qrnfaokl6xmlwhfm";

    #[test]
    fn refs_in_order_once() {
        let body = format!("hi\n\n![](blob://{B})\n\n![a](blob://{A})\n[x](blob://{B})");
        assert_eq!(blob_refs(&body), vec![B.to_string(), A.to_string()]);
        assert!(blob_refs("blob://nope").is_empty());
    }

    #[test]
    fn embeds_stripped() {
        let body = format!("a photo\nsecond line\n\n![](blob://{A})\n\n![](blob://{B})");
        assert_eq!(strip_embeds(&body), "a photo\nsecond line");
        assert_eq!(strip_embeds(&format!("![](blob://{A})")), "");
        // an inline link is text, not an embed
        assert_eq!(strip_embeds(&format!("see [this](blob://{A}) ok")), format!("see [this](blob://{A}) ok"));
    }

    #[test]
    fn kinds_and_thumbs() {
        assert_eq!(MediaKind::of("image/png"), MediaKind::Image);
        assert_eq!(MediaKind::of("video/mp4"), MediaKind::Video);
        assert_eq!(MediaKind::of("audio/mpeg"), MediaKind::Audio);
        assert_eq!(MediaKind::of("application/pdf"), MediaKind::Other);
        let mut m = Meta { cid: A.into(), mime: "image/jpeg".into(), size: 1000, ..Default::default() };
        assert_eq!(thumb_source(&m).as_deref(), Some(A));
        m.thumb_cid = B.into();
        assert_eq!(thumb_source(&m).as_deref(), Some(B));
        let v = Meta { cid: A.into(), mime: "video/mp4".into(), ..Default::default() };
        assert_eq!(thumb_source(&v), None);
    }

    #[test]
    fn reference_count_from_409() {
        let e = ApiError::Conflict {
            message: "blob is still referenced; kept".into(),
            body: json!({"error": "x", "references": 2}),
        };
        assert_eq!(references_in(&e), Some(2));
        let e = ApiError::Conflict { message: "cannot confirm".into(), body: json!({"error": "cannot confirm"}) };
        assert_eq!(references_in(&e), None);
        assert_eq!(references_in(&ApiError::NotFound), None);
    }
}
