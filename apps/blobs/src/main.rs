//! `farfield-blobs` — the shared blob service (`blobs.farfield.systems`).
//!
//! A standalone content-addressed image store. `content` (and any future
//! app) upload images here and reference them by CID. v1 uses the local-
//! directory backend so the whole stack runs on one machine; an R2 backend
//! slots in behind the same `BlobStore` trait for the server.
//!
//! Endpoints:
//! - `POST /blobs`              upload image bytes -> CID + metadata  (auth)
//! - `GET  /blobs`              list stored CIDs
//! - `GET  /blobs/{cid}`        the image bytes (immutable, cacheable)
//! - `GET  /blobs/{cid}/meta`   the image metadata
//! - `GET  /status`, `GET /`

use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;

use axum::body::Bytes;
use axum::extract::{DefaultBodyLimit, Path, State};
use axum::http::{StatusCode, header};
use axum::response::{IntoResponse, Response};
use axum::routing::get;
use axum::{Json, Router};
use farfield_blobs::{BlobStore, LocalDir, derive_metadata};
use farfield_httpkit::{ApiError, verify_bearer};
use serde_json::{Value, json};

/// 50 MB — the upload cap, enforced on the received body.
const MAX_UPLOAD: usize = 50 * 1024 * 1024;

struct BlobState {
    store: Box<dyn BlobStore>,
    tokens: Vec<String>,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let addr: SocketAddr = env_or("FARFIELD_BLOBS_ADDR", "127.0.0.1:8789").parse()?;
    let data_dir = PathBuf::from(env_or("FARFIELD_BLOBS_DIR", "farfield-blobs-data"));

    let mut tokens = Vec::new();
    for var in ["FARFIELD_TOKEN", "FARFIELD_TOKEN_PREVIOUS"] {
        if let Ok(t) = std::env::var(var)
            && !t.is_empty()
        {
            tokens.push(t);
        }
    }
    if tokens.is_empty() {
        eprintln!("warning: FARFIELD_TOKEN unset — using 'dev-token' (local dev only)");
        tokens.push("dev-token".to_string());
    }

    let store = LocalDir::open(&data_dir)?;
    let state = Arc::new(BlobState {
        store: Box::new(store),
        tokens,
    });

    let app = Router::new()
        .route("/", get(root))
        .route("/status", get(status))
        .route("/blobs", get(list).post(upload))
        .route("/blobs/{cid}", get(get_blob))
        .route("/blobs/{cid}/meta", get(get_meta))
        .layer(DefaultBodyLimit::max(MAX_UPLOAD))
        .with_state(state);

    let listener = tokio::net::TcpListener::bind(addr).await?;
    println!(
        "farfield-blobs listening on http://{} — store: {}",
        listener.local_addr()?,
        data_dir.display()
    );
    axum::serve(listener, app).await?;
    Ok(())
}

async fn root() -> Json<Value> {
    Json(json!({
        "service": "farfield-blobs",
        "ok": true,
        "endpoints": [
            "GET    /status",
            "GET    /blobs",
            "GET    /blobs/{cid}",
            "GET    /blobs/{cid}/meta",
            "POST   /blobs            (auth)",
        ],
    }))
}

async fn status(State(s): State<Arc<BlobState>>) -> Result<Json<Value>, ApiError> {
    let count = s.store.list().map_err(internal)?.len();
    Ok(Json(json!({ "service": "farfield-blobs", "ok": true, "blobs": count })))
}

async fn list(State(s): State<Arc<BlobState>>) -> Result<Json<Value>, ApiError> {
    let cids = s.store.list().map_err(internal)?;
    Ok(Json(json!({ "blobs": cids })))
}

async fn upload(
    State(s): State<Arc<BlobState>>,
    headers: axum::http::HeaderMap,
    body: Bytes,
) -> Result<Json<Value>, ApiError> {
    verify_bearer(&headers, &s.tokens)?;
    if body.is_empty() {
        return Err(ApiError::bad_request("empty_blob", "no bytes uploaded"));
    }
    let meta = derive_metadata(&body)
        .map_err(|e| ApiError::bad_request("invalid_image", e.to_string()))?;
    s.store.put(&meta, &body).map_err(internal)?;
    Ok(Json(json!(meta)))
}

async fn get_blob(
    State(s): State<Arc<BlobState>>,
    Path(cid): Path<String>,
) -> Result<Response, ApiError> {
    let cid = checked_cid(&cid)?;
    let bytes = s
        .store
        .get_bytes(cid)
        .map_err(internal)?
        .ok_or_else(|| ApiError::not_found(format!("blob {cid}")))?;
    let mime = s
        .store
        .get_meta(cid)
        .map_err(internal)?
        .map(|m| m.mime)
        .unwrap_or_else(|| "application/octet-stream".to_string());
    Ok((
        [
            (header::CONTENT_TYPE, mime),
            // Content-addressed: the bytes for a CID never change.
            (
                header::CACHE_CONTROL,
                "public, max-age=31536000, immutable".to_string(),
            ),
        ],
        bytes,
    )
        .into_response())
}

async fn get_meta(
    State(s): State<Arc<BlobState>>,
    Path(cid): Path<String>,
) -> Result<Json<Value>, ApiError> {
    let cid = checked_cid(&cid)?;
    let meta = s
        .store
        .get_meta(cid)
        .map_err(internal)?
        .ok_or_else(|| ApiError::not_found(format!("blob {cid}")))?;
    Ok(Json(json!(meta)))
}

/// Validate a CID path segment — base32 CIDv1 alphabet only, so it can never
/// be a path-traversal payload.
fn checked_cid(cid: &str) -> Result<&str, ApiError> {
    let ok = (1..=80).contains(&cid.len())
        && cid
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit());
    if ok {
        Ok(cid)
    } else {
        Err(ApiError::bad_request("invalid_cid", "malformed CID"))
    }
}

fn internal<E: std::fmt::Display>(e: E) -> ApiError {
    ApiError::new(StatusCode::INTERNAL_SERVER_ERROR, "internal", e.to_string())
}

fn env_or(var: &str, default: &str) -> String {
    std::env::var(var).unwrap_or_else(|_| default.to_string())
}
