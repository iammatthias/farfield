//! Uploads: streamed from disk with byte-accurate progress and cancellation,
//! never buffered whole, plus a resumable tus client for the library.

use crate::transport::{ApiError, ServiceClient};
use futures_util::StreamExt;
use reqwest::{header, Method};
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use tokio::io::AsyncReadExt;

/// Shared progress for one upload, read by the UI while I/O runs.
#[derive(Clone, Default, Debug)]
pub struct Progress {
    inner: Arc<ProgressInner>,
}

#[derive(Default, Debug)]
struct ProgressInner {
    sent: AtomicU64,
    total: AtomicU64,
    cancelled: AtomicBool,
}

impl Progress {
    pub fn new(total: u64) -> Self {
        let p = Progress::default();
        p.inner.total.store(total, Ordering::Relaxed);
        p
    }
    pub fn sent(&self) -> u64 {
        self.inner.sent.load(Ordering::Relaxed)
    }
    pub fn total(&self) -> u64 {
        self.inner.total.load(Ordering::Relaxed)
    }
    pub fn fraction(&self) -> f32 {
        let t = self.total();
        if t == 0 {
            0.0
        } else {
            (self.sent() as f32 / t as f32).min(1.0)
        }
    }
    pub fn cancel(&self) {
        self.inner.cancelled.store(true, Ordering::Relaxed);
    }
    pub fn is_cancelled(&self) -> bool {
        self.inner.cancelled.load(Ordering::Relaxed)
    }
    fn add(&self, n: u64) {
        self.inner.sent.fetch_add(n, Ordering::Relaxed);
    }
    fn set(&self, n: u64) {
        self.inner.sent.store(n, Ordering::Relaxed);
    }
}

const CHUNK: usize = 256 * 1024;

/// A request body streaming `len` bytes of `path` from `offset`, counting
/// into `progress` and stopping when it is cancelled.
pub async fn file_body(path: &Path, offset: u64, len: u64, progress: Progress) -> Result<reqwest::Body, ApiError> {
    let mut f =
        tokio::fs::File::open(path).await.map_err(|e| ApiError::BadRequest(format!("open {}: {e}", path.display())))?;
    if offset > 0 {
        use tokio::io::AsyncSeekExt;
        f.seek(std::io::SeekFrom::Start(offset)).await.map_err(|e| ApiError::BadRequest(e.to_string()))?;
    }
    let stream = futures_util::stream::unfold((f, len, progress), |(mut f, left, p)| async move {
        if left == 0 {
            return None;
        }
        if p.is_cancelled() {
            return Some((Err(std::io::Error::new(std::io::ErrorKind::Interrupted, "cancelled")), (f, 0, p)));
        }
        let mut buf = vec![0u8; CHUNK.min(left as usize)];
        match f.read(&mut buf).await {
            Ok(0) => Some((Err(std::io::Error::new(std::io::ErrorKind::UnexpectedEof, "file shrank")), (f, 0, p))),
            Ok(n) => {
                buf.truncate(n);
                p.add(n as u64);
                Some((Ok(bytes::Bytes::from(buf)), (f, left - n as u64, p)))
            }
            Err(e) => Some((Err(e), (f, 0, p))),
        }
    });
    Ok(reqwest::Body::wrap_stream(stream))
}

/// Map a failed upload to Cancelled when the user cancelled it.
pub fn cancelled_or(progress: &Progress, e: ApiError) -> ApiError {
    if progress.is_cancelled() {
        ApiError::Cancelled
    } else {
        e
    }
}

/// Guess a MIME type from a file name, for the parts that carry one.
pub fn mime_for(path: &Path) -> &'static str {
    match path.extension().and_then(|e| e.to_str()).map(|e| e.to_ascii_lowercase()).as_deref() {
        Some("jpg" | "jpeg") => "image/jpeg",
        Some("png") => "image/png",
        Some("gif") => "image/gif",
        Some("webp") => "image/webp",
        Some("heic") => "image/heic",
        Some("heif") => "image/heif",
        Some("avif") => "image/avif",
        Some("svg") => "image/svg+xml",
        Some("mp4" | "m4v") => "video/mp4",
        Some("mov") => "video/quicktime",
        Some("webm") => "video/webm",
        Some("mp3") => "audio/mpeg",
        Some("m4a") => "audio/mp4",
        Some("wav") => "audio/wav",
        Some("pdf") => "application/pdf",
        Some("epub") => "application/epub+zip",
        Some("ipa") => "application/octet-stream",
        Some("txt" | "md") => "text/plain",
        _ => "application/octet-stream",
    }
}

// ── tus (library) ────────────────────────────────────────────────────────

/// A resumable upload's state, persisted so a relaunch can continue it.
#[derive(Serialize, Deserialize, Clone, Debug, PartialEq)]
#[serde(rename_all = "camelCase")]
pub struct TusState {
    pub file: PathBuf,
    pub size: u64,
    /// mtime of the file when the upload began: a changed file restarts.
    pub modified_ms: u64,
    /// Upload path on the server, e.g. `/api/upload/tus/<id>`.
    pub location: Option<String>,
    pub collection: String,
}

#[derive(Debug, Clone, PartialEq)]
pub enum TusOutcome {
    Done { cid: String },
    Failed(String),
}

/// Chunks stay well under the Cloudflare edge's body cap.
pub const TUS_CHUNK: u64 = 8 << 20;

fn modified_ms(path: &Path) -> u64 {
    std::fs::metadata(path)
        .and_then(|m| m.modified())
        .ok()
        .and_then(|t| t.duration_since(std::time::UNIX_EPOCH).ok())
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

impl TusState {
    pub fn new(file: PathBuf, collection: &str) -> std::io::Result<Self> {
        let size = std::fs::metadata(&file)?.len();
        Ok(TusState { modified_ms: modified_ms(&file), file, size, location: None, collection: collection.into() })
    }
    pub fn file_unchanged(&self) -> bool {
        std::fs::metadata(&self.file).map(|m| m.len()).ok() == Some(self.size)
            && modified_ms(&self.file) == self.modified_ms
    }
}

fn b64(s: &str) -> String {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.encode(s)
}

/// Run (or resume) a tus upload. `persist` is called whenever the state
/// changes (the server location is assigned), so an interruption anywhere
/// can be resumed from the last acknowledged offset.
pub async fn tus_upload(
    c: &ServiceClient,
    state: &mut TusState,
    progress: &Progress,
    persist: impl FnMut(&TusState),
) -> Result<TusOutcome, ApiError> {
    tus_upload_with(c, state, progress, TUS_CHUNK, persist).await
}

/// [`tus_upload`] with an explicit chunk size (tests use small chunks to
/// interrupt an upload part-way through).
pub async fn tus_upload_with(
    c: &ServiceClient,
    state: &mut TusState,
    progress: &Progress,
    chunk: u64,
    mut persist: impl FnMut(&TusState),
) -> Result<TusOutcome, ApiError> {
    let chunk = chunk.max(1);
    if !state.file_unchanged() {
        state.location = None;
        state.size = std::fs::metadata(&state.file).map_err(|e| ApiError::BadRequest(e.to_string()))?.len();
        state.modified_ms = modified_ms(&state.file);
    }
    progress.inner.total.store(state.size, Ordering::Relaxed);

    // where to resume from
    let mut offset = 0u64;
    if let Some(loc) = state.location.clone() {
        match c.send(c.request(Method::HEAD, &loc)?.header("Tus-Resumable", "1.0.0"), false).await {
            Ok(r) => {
                if let Some(cid) = header_str(&r, "X-Library-Cid") {
                    progress.set(state.size);
                    return Ok(TusOutcome::Done { cid });
                }
                offset = header_str(&r, "Upload-Offset").and_then(|s| s.parse().ok()).unwrap_or(0);
            }
            Err(ApiError::NotFound) => state.location = None, // expired (24h): start over
            Err(e) => return Err(e),
        }
    }
    if state.location.is_none() {
        let name = state.file.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default();
        let meta = format!("filename {},collection {}", b64(&name), b64(&state.collection));
        let rb = c
            .request(Method::POST, "/api/upload/tus")?
            .header("Tus-Resumable", "1.0.0")
            .header("Upload-Length", state.size.to_string())
            .header("Upload-Metadata", meta);
        let r = c.send(rb, true).await?;
        let loc = header_str(&r, "Location").ok_or_else(|| ApiError::Decode("tus: no Location".into()))?;
        // the server answers a relative path; keep only a path on our origin
        let loc =
            if loc.starts_with('/') { loc } else { url::Url::parse(&loc).map(|u| u.path().to_string()).unwrap_or(loc) };
        state.location = Some(loc);
        persist(state);
        offset = 0;
    }
    let loc = state.location.clone().unwrap();
    progress.set(offset);

    while offset < state.size {
        if progress.is_cancelled() {
            return Err(ApiError::Cancelled);
        }
        let n = chunk.min(state.size - offset);
        let body = file_body(&state.file, offset, n, progress.clone()).await?;
        let rb = c
            .request(Method::PATCH, &loc)?
            .header("Tus-Resumable", "1.0.0")
            .header("Upload-Offset", offset.to_string())
            .header(header::CONTENT_TYPE, "application/offset+octet-stream")
            .header(header::CONTENT_LENGTH, n.to_string())
            .body(body);
        match c.send(rb, true).await {
            Ok(r) => {
                offset = header_str(&r, "Upload-Offset").and_then(|s| s.parse().ok()).unwrap_or(offset + n);
                progress.set(offset);
            }
            // A chunk is idempotent by offset: on a lost response or a 409
            // mismatch, ask the server where it is and continue from there.
            Err(e @ (ApiError::Uncertain(_) | ApiError::Conflict { .. } | ApiError::Offline(_))) => {
                if progress.is_cancelled() {
                    return Err(ApiError::Cancelled);
                }
                let r = c
                    .send(c.request(Method::HEAD, &loc)?.header("Tus-Resumable", "1.0.0"), false)
                    .await
                    .map_err(|_| e.clone())?;
                offset = header_str(&r, "Upload-Offset").and_then(|s| s.parse().ok()).ok_or(e)?;
                progress.set(offset);
            }
            Err(e) => return Err(cancelled_or(progress, e)),
        }
    }

    // the server ingests in the background: poll until done
    for _ in 0..240 {
        // stopping the wait does not stop the ingest; the persisted state
        // finds the result (HEAD) next time
        if progress.is_cancelled() {
            return Err(ApiError::Cancelled);
        }
        let r = c.send(c.request(Method::HEAD, &loc)?.header("Tus-Resumable", "1.0.0"), false).await?;
        match header_str(&r, "X-Library-Status").as_deref() {
            Some("done") => {
                let cid = header_str(&r, "X-Library-Cid")
                    .ok_or_else(|| ApiError::Decode("tus: done without a cid".into()))?;
                return Ok(TusOutcome::Done { cid });
            }
            Some("error") => return Ok(TusOutcome::Failed(header_str(&r, "X-Library-Error").unwrap_or_default())),
            _ => tokio::time::sleep(std::time::Duration::from_millis(500)).await,
        }
    }
    Err(ApiError::Uncertain("the library is still processing the upload".into()))
}

fn header_str(r: &reqwest::Response, name: &str) -> Option<String> {
    r.headers().get(name).and_then(|v| v.to_str().ok()).map(|s| s.to_string())
}

/// Stream a response to a file atomically (download/export), with progress.
pub async fn download_to(resp: reqwest::Response, dest: &Path, progress: &Progress) -> Result<(), ApiError> {
    if let Some(n) = resp.content_length() {
        progress.inner.total.store(n, Ordering::Relaxed);
    }
    let tmp = dest.with_extension("part");
    let mut f = tokio::fs::File::create(&tmp).await.map_err(|e| ApiError::BadRequest(e.to_string()))?;
    let mut s = resp.bytes_stream();
    use tokio::io::AsyncWriteExt;
    while let Some(chunk) = s.next().await {
        if progress.is_cancelled() {
            let _ = tokio::fs::remove_file(&tmp).await;
            return Err(ApiError::Cancelled);
        }
        let chunk = chunk.map_err(|e| ApiError::Offline(e.to_string()))?;
        f.write_all(&chunk).await.map_err(|e| ApiError::BadRequest(e.to_string()))?;
        progress.add(chunk.len() as u64);
    }
    f.sync_all().await.map_err(|e| ApiError::BadRequest(e.to_string()))?;
    tokio::fs::rename(&tmp, dest).await.map_err(|e| ApiError::BadRequest(e.to_string()))?;
    Ok(())
}
