//! `farfield-records` — the typed-records service engine.
//!
//! Assembles `store` + `schema` + `httpkit` into the records HTTP API. The
//! `content` and `feed` apps are thin binaries over [`serve`] — same engine,
//! different DB, schema directory, and address.
//!
//! Endpoints:
//! - `GET  /status`                      service health + cursor
//! - `GET  /collections`                 collections + display metadata
//! - `GET  /schemas` `/schemas/{c}`       published schemas
//! - `GET  /records/{c}`                  list (ETag; `?since=<seq>` cursor)
//! - `GET  /records/{c}/{rkey}`           one record (ETag = CID)
//! - `POST /records/{c}`                  create, server-assigned rkey   (authed)
//! - `PUT  /records/{c}/{rkey}`           create or replace               (authed)
//! - `DELETE /records/{c}/{rkey}`         delete                          (authed)
//!
//! Concurrency note: the store is held behind a `Mutex` and every handler is
//! await-free while it holds the lock. A dedicated writer thread + read pool
//! is a later refinement; this is correct and simple for local + single-user.

use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::{SystemTime, UNIX_EPOCH};

use axum::extract::{Path, Query, State};
use axum::http::{HeaderMap, StatusCode, header};
use axum::response::{IntoResponse, Response};
use axum::routing::get;
use axum::{Json, Router};
use farfield_core::Record;
use farfield_httpkit::{ApiError, verify_bearer};
use farfield_schema::SchemaSet;
use farfield_store::{Store, Stored, Write};
use serde_json::{Value, json};

/// Configuration for one records service instance.
pub struct Config {
    /// Address to bind.
    pub addr: SocketAddr,
    /// SQLite database path.
    pub db_path: PathBuf,
    /// Directory of lexicon-lite schema files + `collections.json`.
    pub schema_dir: PathBuf,
    /// Accepted bearer tokens (write auth).
    pub tokens: Vec<String>,
    /// Service name, reported by `/status`.
    pub service_name: String,
}

struct AppState {
    store: Mutex<Store>,
    schemas: SchemaSet,
    tokens: Vec<String>,
    service_name: String,
}

/// Load the store + schemas and serve until the process is killed.
pub async fn serve(config: Config) -> anyhow::Result<()> {
    let store = Store::open(&config.db_path)?;
    let schemas = SchemaSet::load(&config.schema_dir)?;
    let collections: Vec<_> = schemas.collections().iter().map(|c| c.name.clone()).collect();
    let state = Arc::new(AppState {
        store: Mutex::new(store),
        schemas,
        tokens: config.tokens,
        service_name: config.service_name.clone(),
    });

    let listener = tokio::net::TcpListener::bind(config.addr).await?;
    println!(
        "farfield-{} listening on http://{} — collections: {}",
        config.service_name,
        listener.local_addr()?,
        collections.join(", ")
    );
    axum::serve(listener, router(state)).await?;
    Ok(())
}

fn router(state: Arc<AppState>) -> Router {
    Router::new()
        .route("/", get(root))
        .route("/status", get(status))
        .route("/collections", get(collections))
        .route("/schemas", get(schemas_all))
        .route("/schemas/{collection}", get(schema_one))
        .route("/records/{collection}", get(list).post(create))
        .route(
            "/records/{collection}/{rkey}",
            get(get_one).put(put_one).delete(delete_one),
        )
        .with_state(state)
}

// ---------- read handlers --------------------------------------------------

async fn root(State(s): State<Arc<AppState>>) -> Json<Value> {
    Json(json!({
        "service": format!("farfield-{}", s.service_name),
        "ok": true,
        "endpoints": [
            "GET    /status",
            "GET    /collections",
            "GET    /schemas",
            "GET    /schemas/{collection}",
            "GET    /records/{collection}",
            "GET    /records/{collection}?since={seq}",
            "GET    /records/{collection}/{rkey}",
            "POST   /records/{collection}            (auth)",
            "PUT    /records/{collection}/{rkey}     (auth)",
            "DELETE /records/{collection}/{rkey}     (auth)",
        ],
        "collections": s.schemas.collections().iter().map(|c| &c.name).collect::<Vec<_>>(),
    }))
}

async fn status(State(s): State<Arc<AppState>>) -> Json<Value> {
    let seq = s.store.lock().unwrap().current_seq().unwrap_or(0);
    Json(json!({
        "service": s.service_name,
        "ok": true,
        "cursor": seq,
        "collections": s.schemas.collections().iter().map(|c| &c.name).collect::<Vec<_>>(),
    }))
}

async fn collections(State(s): State<Arc<AppState>>) -> Json<Value> {
    Json(json!({ "collections": s.schemas.collections() }))
}

async fn schemas_all(State(s): State<Arc<AppState>>) -> Json<Value> {
    let all: Vec<_> = s
        .schemas
        .collections()
        .iter()
        .filter_map(|c| s.schemas.schema_for(&c.name))
        .collect();
    Json(json!({ "schemas": all }))
}

async fn schema_one(
    State(s): State<Arc<AppState>>,
    Path(collection): Path<String>,
) -> Result<Json<Value>, ApiError> {
    let schema = s
        .schemas
        .schema_for(&collection)
        .ok_or_else(|| ApiError::not_found(format!("unknown collection `{collection}`")))?;
    Ok(Json(json!(schema)))
}

#[derive(serde::Deserialize)]
struct ListQuery {
    since: Option<i64>,
}

async fn list(
    State(s): State<Arc<AppState>>,
    Path(collection): Path<String>,
    Query(q): Query<ListQuery>,
    headers: HeaderMap,
) -> Result<Response, ApiError> {
    require_known_collection(&s, &collection)?;
    let store = s.store.lock().unwrap();

    if let Some(since) = q.since {
        let (records, tombstones) =
            store.changed_since(&collection, since).map_err(internal)?;
        let cursor = store.current_seq().map_err(internal)?;
        return Ok(Json(json!({
            "records": records.iter().map(stored_json).collect::<Vec<_>>(),
            "deletions": tombstones.iter()
                .map(|t| json!({ "rkey": t.rkey, "seq": t.seq, "deletedAt": t.deleted_at }))
                .collect::<Vec<_>>(),
            "cursor": cursor,
        }))
        .into_response());
    }

    let etag = store.list_etag(&collection).map_err(internal)?;
    if let Some(etag) = &etag
        && if_none_match(&headers, etag)
    {
        return Ok(StatusCode::NOT_MODIFIED.into_response());
    }
    let records = store.list(&collection).map_err(internal)?;
    let cursor = store.current_seq().map_err(internal)?;
    let mut resp = Json(json!({
        "records": records.iter().map(stored_json).collect::<Vec<_>>(),
        "cursor": cursor,
    }))
    .into_response();
    if let Some(etag) = etag {
        resp.headers_mut()
            .insert(header::ETAG, quote(&etag));
    }
    Ok(resp)
}

async fn get_one(
    State(s): State<Arc<AppState>>,
    Path((collection, rkey)): Path<(String, String)>,
    headers: HeaderMap,
) -> Result<Response, ApiError> {
    let store = s.store.lock().unwrap();
    let record = store
        .get(&collection, &rkey)
        .map_err(internal)?
        .ok_or_else(|| ApiError::not_found(format!("{collection}/{rkey}")))?;
    if if_none_match(&headers, &record.cid) {
        return Ok(StatusCode::NOT_MODIFIED.into_response());
    }
    let mut resp = Json(stored_json(&record)).into_response();
    resp.headers_mut().insert(header::ETAG, quote(&record.cid));
    Ok(resp)
}

// ---------- write handlers -------------------------------------------------

async fn create(
    State(s): State<Arc<AppState>>,
    Path(collection): Path<String>,
    headers: HeaderMap,
    Json(value): Json<Value>,
) -> Result<Response, ApiError> {
    verify_bearer(&headers, &s.tokens)?;
    // Server-assigned rkey: unix-millis, sortable and unique for one writer.
    let rkey = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_err(|e| ApiError::internal(e.to_string()))?
        .as_millis()
        .to_string();
    write_record(&s, &collection, &rkey, value)
}

async fn put_one(
    State(s): State<Arc<AppState>>,
    Path((collection, rkey)): Path<(String, String)>,
    headers: HeaderMap,
    Json(value): Json<Value>,
) -> Result<Response, ApiError> {
    verify_bearer(&headers, &s.tokens)?;
    write_record(&s, &collection, &rkey, value)
}

async fn delete_one(
    State(s): State<Arc<AppState>>,
    Path((collection, rkey)): Path<(String, String)>,
    headers: HeaderMap,
) -> Result<Response, ApiError> {
    verify_bearer(&headers, &s.tokens)?;
    require_known_collection(&s, &collection)?;
    let removed = s
        .store
        .lock()
        .unwrap()
        .delete(&collection, &rkey)
        .map_err(internal)?;
    if removed {
        Ok(StatusCode::NO_CONTENT.into_response())
    } else {
        Err(ApiError::not_found(format!("{collection}/{rkey}")))
    }
}

fn write_record(
    s: &Arc<AppState>,
    collection: &str,
    rkey: &str,
    value: Value,
) -> Result<Response, ApiError> {
    require_known_collection(s, collection)?;
    validate_rkey(rkey)?;
    s.schemas
        .validate(collection, &value)
        .map_err(|e| ApiError::bad_request("invalid_record", e.to_string()))?;
    let obj = value
        .as_object()
        .ok_or_else(|| ApiError::bad_request("invalid_record", "record must be a JSON object"))?
        .clone();
    let record = Record::new(obj);

    let write = s
        .store
        .lock()
        .unwrap()
        .put(collection, rkey, &record)
        .map_err(internal)?;

    let (status, cid, seq) = match write {
        Write::Created { cid, seq } => (StatusCode::CREATED, cid, Some(seq)),
        Write::Updated { cid, seq } => (StatusCode::OK, cid, Some(seq)),
        Write::Unchanged { cid } => (StatusCode::OK, cid, None),
    };
    Ok((
        status,
        Json(json!({ "collection": collection, "rkey": rkey, "cid": cid, "seq": seq })),
    )
        .into_response())
}

// ---------- helpers --------------------------------------------------------

fn stored_json(r: &Stored) -> Value {
    json!({
        "collection": r.collection,
        "rkey": r.rkey,
        "cid": r.cid,
        "seq": r.seq,
        "createdAt": r.created_at,
        "updatedAt": r.updated_at,
        "value": r.json,
    })
}

fn require_known_collection(s: &Arc<AppState>, collection: &str) -> Result<(), ApiError> {
    if s.schemas.collection(collection).is_some() {
        Ok(())
    } else {
        Err(ApiError::new(
            StatusCode::NOT_FOUND,
            "unknown_collection",
            format!("unknown collection `{collection}`"),
        ))
    }
}

/// rkey grammar: `[a-z0-9-]{1,128}`.
fn validate_rkey(rkey: &str) -> Result<(), ApiError> {
    let ok = (1..=128).contains(&rkey.len())
        && rkey
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-');
    if ok {
        Ok(())
    } else {
        Err(ApiError::bad_request(
            "invalid_rkey",
            format!("rkey `{rkey}` must match [a-z0-9-]{{1,128}}"),
        ))
    }
}

fn if_none_match(headers: &HeaderMap, etag: &str) -> bool {
    headers
        .get(header::IF_NONE_MATCH)
        .and_then(|v| v.to_str().ok())
        .map(|v| {
            let v = v.trim();
            v == "*" || v.trim_matches('"') == etag
        })
        .unwrap_or(false)
}

fn quote(etag: &str) -> header::HeaderValue {
    header::HeaderValue::from_str(&format!("\"{etag}\""))
        .unwrap_or_else(|_| header::HeaderValue::from_static("\"\""))
}

fn internal<E: std::fmt::Display>(e: E) -> ApiError {
    ApiError::internal(e.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_state() -> Arc<AppState> {
        let dir = concat!(env!("CARGO_MANIFEST_DIR"), "/../../schemas/content");
        Arc::new(AppState {
            store: Mutex::new(Store::open_in_memory().unwrap()),
            schemas: SchemaSet::load(dir).unwrap(),
            tokens: vec!["test-token".to_string()],
            service_name: "content".to_string(),
        })
    }

    #[test]
    fn rkey_grammar_accepts_real_slugs_and_rejects_bad_ones() {
        assert!(validate_rkey("1587970800000-sourdough").is_ok());
        assert!(validate_rkey("Uppercase").is_err());
        assert!(validate_rkey("has space").is_err());
        assert!(validate_rkey("").is_err());
        assert!(validate_rkey(&"x".repeat(70)).is_ok(), "long real slugs are fine");
        assert!(validate_rkey(&"x".repeat(129)).is_err());
    }

    #[test]
    fn write_then_read_through_the_store() {
        let s = test_state();
        let rec = json!({
            "title": "Sourdough", "slug": "1587970800000-sourdough",
            "published": true, "created": "2023-11-02T12:14:00Z",
            "updated": "2025-05-24T16:58:00Z", "tags": ["bread"],
            "body": "# Sourdough"
        });
        let resp = write_record(&s, "recipes", "1587970800000-sourdough", rec)
            .expect("valid record writes");
        assert_eq!(resp.status(), StatusCode::CREATED);

        let got = s.store.lock().unwrap().get("recipes", "1587970800000-sourdough").unwrap();
        assert!(got.is_some());
    }

    #[test]
    fn an_invalid_record_is_rejected_as_bad_request() {
        let s = test_state();
        let err = write_record(&s, "recipes", "x", json!({ "body": "no title" }))
            .unwrap_err();
        assert_eq!(err.status, StatusCode::BAD_REQUEST);
        assert_eq!(err.code, "invalid_record");
    }

    #[test]
    fn an_unknown_collection_is_rejected() {
        let s = test_state();
        let err = write_record(&s, "nonsense", "x", json!({})).unwrap_err();
        assert_eq!(err.code, "unknown_collection");
    }
}
