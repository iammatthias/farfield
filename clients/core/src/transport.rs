//! The HTTP transport every service client shares — the Rust twin of
//! lib/capability's `svc`.
//!
//! Rules it enforces so nothing above it has to remember them:
//! - TLS off loopback (profile validation refuses anything else);
//! - the credential is attached only to requests for the origin it was
//!   stored for, and a request URL is always built from that origin;
//! - redirects are never followed: an API answers one only to send a caller
//!   without a valid key to a login page, so a 3xx means "not signed in";
//! - a real User-Agent, since the Cloudflare edge 403s several default ones;
//! - every failure is classified, and a mutation whose request may have
//!   reached the server is reported as *uncertain*, never as "offline".

use crate::profile::{origin, validate_base, EndpointError};
use crate::secret::{redact, Credential};
use bytes::Bytes;
use reqwest::{header, Method, StatusCode};
use serde::de::DeserializeOwned;
use serde_json::Value;
use std::sync::OnceLock;
use std::time::Duration;
use url::Url;

pub const USER_AGENT: &str = concat!("farfield-desktop/", env!("CARGO_PKG_VERSION"));

/// The JSON responses the fleet sends are small; this bounds a misbehaving
/// one. Content lists with bodies are the largest (500 entries a page).
const MAX_JSON: usize = 32 << 20;

/// The shared async runtime. The UI awaits its JoinHandles from its own
/// executor; all I/O runs here, never on the UI thread.
pub fn runtime() -> &'static tokio::runtime::Runtime {
    static RT: OnceLock<tokio::runtime::Runtime> = OnceLock::new();
    RT.get_or_init(|| {
        tokio::runtime::Builder::new_multi_thread()
            .worker_threads(4)
            .thread_name("farfield-io")
            .enable_all()
            .build()
            .expect("tokio runtime")
    })
}

/// Spawn I/O on the shared runtime.
pub fn spawn<F>(f: F) -> tokio::task::JoinHandle<F::Output>
where
    F: std::future::Future + Send + 'static,
    F::Output: Send + 'static,
{
    runtime().spawn(f)
}

#[derive(Debug, Clone, thiserror::Error, PartialEq)]
pub enum ApiError {
    /// The request never reached the server (DNS, connect, TLS handshake).
    #[error("offline: {0}")]
    Offline(String),
    /// A mutation may or may not have been applied: the request was sent and
    /// the response was lost. Never retried automatically.
    #[error("the server may or may not have applied this: {0}")]
    Uncertain(String),
    #[error("not signed in to {0} — the key was missing or not accepted")]
    Unauthorized(String),
    #[error("forbidden: {0}")]
    Forbidden(String),
    #[error("not found")]
    NotFound,
    /// 412: the record changed since it was read. `current` is the server's
    /// copy, `etag` its version.
    #[error("changed on the server since it was opened")]
    Precondition { current: Value, etag: Option<String> },
    /// 409: refused because of state (e.g. a blob still referenced).
    #[error("conflict: {message}")]
    Conflict { message: String, body: Value },
    #[error("rate limited")]
    RateLimited { retry_after: Option<u64> },
    /// 503 from an admin route: the server has no key configured.
    #[error("unavailable: {0}")]
    Unavailable(String),
    #[error("{0}")]
    BadRequest(String),
    #[error("server error {status}: {message}")]
    Server { status: u16, message: String },
    #[error("unexpected response: {0}")]
    Decode(String),
    #[error(transparent)]
    Endpoint(#[from] EndpointError),
    #[error("cancelled")]
    Cancelled,
}

impl ApiError {
    /// Worth showing as "offline" rather than as a failure of this action.
    pub fn is_offline(&self) -> bool {
        matches!(self, ApiError::Offline(_))
    }
    pub fn is_auth(&self) -> bool {
        matches!(self, ApiError::Unauthorized(_))
    }
}

/// A response body plus the version the server stamped on it.
#[derive(Debug, Clone)]
pub struct Versioned<T> {
    pub value: T,
    pub etag: Option<String>,
}

/// A conditional GET's outcome.
#[derive(Debug, Clone)]
pub enum Fetched<T> {
    Fresh(Versioned<T>),
    NotModified,
}

/// One service on one endpoint, with (optionally) its credential.
#[derive(Clone)]
pub struct ServiceClient {
    pub service: String,
    base: Url,
    origin: String,
    cred: Option<Credential>,
    http: reqwest::Client,
}

fn http_client() -> reqwest::Client {
    static C: OnceLock<reqwest::Client> = OnceLock::new();
    C.get_or_init(|| {
        reqwest::Client::builder()
            .user_agent(USER_AGENT)
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(Duration::from_secs(8))
            .pool_idle_timeout(Duration::from_secs(60))
            .https_only(false) // per-endpoint TLS rule is enforced by validate_base
            .build()
            .expect("http client")
    })
    .clone()
}

impl std::fmt::Debug for ServiceClient {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ServiceClient")
            .field("service", &self.service)
            .field("origin", &self.origin)
            .field("cred", &self.cred)
            .finish()
    }
}

impl ServiceClient {
    pub fn new(service: &str, base: &str, cred: Option<Credential>) -> Result<Self, ApiError> {
        let base = validate_base(base)?;
        let origin = origin(&base);
        Ok(ServiceClient { service: service.into(), base, origin, cred, http: http_client() })
    }

    pub fn origin(&self) -> &str {
        &self.origin
    }
    pub fn has_credential(&self) -> bool {
        self.cred.is_some()
    }
    pub fn credential(&self) -> Option<&Credential> {
        self.cred.as_ref()
    }
    /// The partition key for anything cached from this client: origin plus
    /// which key (never the key itself).
    pub fn identity(&self) -> String {
        format!("{}#{}", self.origin, self.cred.as_ref().map(|c| c.fingerprint()).unwrap_or_else(|| "anon".into()))
    }

    /// Resolve an API path against the base. Absolute URLs are refused: a
    /// path from a server response cannot redirect the credential elsewhere.
    pub fn url(&self, path_and_query: &str) -> Result<Url, ApiError> {
        if !path_and_query.starts_with('/') || path_and_query.starts_with("//") {
            return Err(ApiError::Decode(format!("refusing non-path URL {path_and_query}")));
        }
        let u = self.base.join(path_and_query).map_err(|e| ApiError::Decode(e.to_string()))?;
        if origin(&u) != self.origin {
            return Err(ApiError::Decode("cross-origin request refused".into()));
        }
        Ok(u)
    }

    /// A request builder with the credential attached (same origin only).
    pub fn request(&self, method: Method, path: &str) -> Result<reqwest::RequestBuilder, ApiError> {
        let u = self.url(path)?;
        let mut rb = self.http.request(method, u).timeout(Duration::from_secs(60));
        if let Some(c) = &self.cred {
            rb = rb.header("X-API-Key", c.expose());
        }
        Ok(rb)
    }

    /// Send, classifying transport failures. `mutation` decides whether a
    /// failure after sending is "offline" or "uncertain".
    pub async fn send(&self, rb: reqwest::RequestBuilder, mutation: bool) -> Result<reqwest::Response, ApiError> {
        let resp = rb.send().await.map_err(|e| classify(&e, mutation))?;
        let status = resp.status();
        if status.is_success() || status == StatusCode::NOT_MODIFIED {
            return Ok(resp);
        }
        Err(self.error_from(resp).await)
    }

    async fn error_from(&self, resp: reqwest::Response) -> ApiError {
        let status = resp.status();
        let etag = resp.headers().get(header::ETAG).and_then(|v| v.to_str().ok()).map(|s| s.to_string());
        let retry = resp.headers().get(header::RETRY_AFTER).and_then(|v| v.to_str().ok()).and_then(|s| s.parse().ok());
        if status.is_redirection() {
            return ApiError::Unauthorized(self.service.clone());
        }
        let body = resp.bytes().await.unwrap_or_default();
        let json: Value = serde_json::from_slice(&body).unwrap_or(Value::Null);
        let message = json["error"]
            .as_str()
            .map(|s| s.to_string())
            .unwrap_or_else(|| String::from_utf8_lossy(&body).lines().next().unwrap_or("").to_string());
        let message = redact(&message);
        match status.as_u16() {
            401 => ApiError::Unauthorized(self.service.clone()),
            403 => ApiError::Forbidden(message),
            404 | 410 => ApiError::NotFound,
            409 => ApiError::Conflict { message, body: json },
            412 => ApiError::Precondition { current: json["current"].clone(), etag },
            429 => ApiError::RateLimited { retry_after: retry },
            503 => ApiError::Unavailable(message),
            400..=499 => ApiError::BadRequest(message),
            s => ApiError::Server { status: s, message },
        }
    }

    /// Read a body with a size cap.
    pub async fn body(resp: reqwest::Response, cap: usize) -> Result<Bytes, ApiError> {
        if resp.content_length().is_some_and(|n| n as usize > cap) {
            return Err(ApiError::Decode("response too large".into()));
        }
        let mut out = Vec::new();
        let mut resp = resp;
        while let Some(chunk) = resp.chunk().await.map_err(|e| ApiError::Offline(e.to_string()))? {
            if out.len() + chunk.len() > cap {
                return Err(ApiError::Decode("response too large".into()));
            }
            out.extend_from_slice(&chunk);
        }
        Ok(out.into())
    }

    pub async fn json<T: DeserializeOwned>(resp: reqwest::Response) -> Result<Versioned<T>, ApiError> {
        let etag = etag_of(&resp);
        let b = Self::body(resp, MAX_JSON).await?;
        let value = serde_json::from_slice(&b).map_err(|e| ApiError::Decode(e.to_string()))?;
        Ok(Versioned { value, etag })
    }

    /// GET JSON, revalidating with If-None-Match when a version is known.
    /// Idempotent, so transient failures are retried twice with backoff.
    pub async fn get_json<T: DeserializeOwned>(
        &self,
        path: &str,
        if_none_match: Option<&str>,
    ) -> Result<Fetched<T>, ApiError> {
        let mut delay = Duration::from_millis(250);
        let mut attempt = 0;
        loop {
            let mut rb = self.request(Method::GET, path)?;
            if let Some(e) = if_none_match {
                rb = rb.header(header::IF_NONE_MATCH, e);
            }
            match self.send(rb, false).await {
                Ok(resp) if resp.status() == StatusCode::NOT_MODIFIED => return Ok(Fetched::NotModified),
                Ok(resp) => return Ok(Fetched::Fresh(Self::json(resp).await?)),
                Err(e) if attempt < 2 && retryable(&e) => {
                    attempt += 1;
                    tokio::time::sleep(delay).await;
                    delay *= 3;
                }
                Err(e) => return Err(e),
            }
        }
    }

    /// GET JSON unconditionally.
    pub async fn get<T: DeserializeOwned>(&self, path: &str) -> Result<Versioned<T>, ApiError> {
        match self.get_json(path, None).await? {
            Fetched::Fresh(v) => Ok(v),
            Fetched::NotModified => Err(ApiError::Decode("304 to an unconditional GET".into())),
        }
    }

    /// A JSON mutation. `if_match` makes it conditional (412 → Precondition).
    /// Mutations are never retried here: the caller decides, knowing whether
    /// the operation is idempotent.
    pub async fn send_json<T: DeserializeOwned>(
        &self,
        method: Method,
        path: &str,
        body: Option<&Value>,
        if_match: Option<&str>,
    ) -> Result<Versioned<T>, ApiError> {
        let mut rb = self.request(method, path)?;
        if let Some(b) = body {
            rb = rb.json(b);
        }
        if let Some(e) = if_match {
            rb = rb.header(header::IF_MATCH, e);
        }
        let resp = self.send(rb, true).await?;
        Self::json(resp).await
    }
}

pub fn etag_of(resp: &reqwest::Response) -> Option<String> {
    resp.headers().get(header::ETAG).and_then(|v| v.to_str().ok()).map(|s| s.to_string())
}

fn retryable(e: &ApiError) -> bool {
    matches!(e, ApiError::Offline(_) | ApiError::Server { status: 502..=504, .. })
}

fn classify(e: &reqwest::Error, mutation: bool) -> ApiError {
    let msg = redact(&e.to_string());
    if e.is_connect() || is_builder_or_dns(e) {
        return ApiError::Offline(msg);
    }
    if mutation {
        ApiError::Uncertain(msg)
    } else {
        ApiError::Offline(msg)
    }
}

fn is_builder_or_dns(e: &reqwest::Error) -> bool {
    e.is_builder()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn paths_cannot_escape_the_origin() {
        let c = ServiceClient::new("feed", "https://h.ts.net:8788", Credential::new("k")).unwrap();
        assert!(c.url("/api/posts").is_ok());
        assert!(c.url("https://evil.example/x").is_err());
        assert!(c.url("//evil.example/x").is_err());
        assert!(c.url("api/posts").is_err());
    }

    #[test]
    fn plain_http_is_refused_off_loopback() {
        assert!(ServiceClient::new("feed", "http://homelab:8788", None).is_err());
        assert!(ServiceClient::new("feed", "http://127.0.0.1:8788", None).is_ok());
    }

    #[test]
    fn identity_partitions_by_key_without_revealing_it() {
        let a = ServiceClient::new("feed", "https://h:1", Credential::new("one")).unwrap();
        let b = ServiceClient::new("feed", "https://h:1", Credential::new("two")).unwrap();
        assert_ne!(a.identity(), b.identity());
        assert!(!a.identity().contains("one"));
        assert!(format!("{a:?}").contains("Credential(…"));
    }
}
