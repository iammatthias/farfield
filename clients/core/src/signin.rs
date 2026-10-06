//! Signing the app in with a passkey, the way native apps sign in (RFC 8252):
//! the browser does the passkey ceremony on the keys app, then hands a
//! one-time code back to a listener on this machine's loopback address; the
//! app redeems it — with the PKCE verifier only it knows — for a minted key,
//! scoped and revocable per device. The person never sees or pastes a key.
//!
//! The code is single-use and short-lived, the key is minted only at
//! redemption, and redemption goes to the keys app's private address.

use crate::profile::Profile;
use crate::secret::Credential;
use crate::transport::{ApiError, ServiceClient};
use base64::Engine;
use reqwest::Method;
use sha2::{Digest, Sha256};
use std::io::{Read, Write};
use std::net::TcpListener;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};
use url::Url;

pub const CLIENT_ID: &str = "farfield-desktop";

fn b64(b: &[u8]) -> String {
    base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(b)
}

fn random(n: usize) -> String {
    let mut b = vec![0u8; n];
    getrandom::fill(&mut b).expect("system randomness");
    b64(&b)
}

/// PKCE: the verifier stays here; only its S256 challenge goes to the browser.
pub struct Pkce {
    pub verifier: String,
    pub challenge: String,
}

impl Pkce {
    pub fn new() -> Self {
        let verifier = random(32);
        let challenge = b64(&Sha256::digest(verifier.as_bytes()));
        Pkce { verifier, challenge }
    }
}

impl Default for Pkce {
    fn default() -> Self {
        Self::new()
    }
}

/// One sign-in in progress.
pub struct Pending {
    listener: TcpListener,
    pub redirect_uri: String,
    pub state: String,
    pub pkce: Pkce,
    /// The page to open in the browser.
    pub authorize_url: String,
}

/// Where the browser goes: the keys app's public address when the profile
/// has one (passkeys belong to that domain), else its private address with
/// loopback spelled `localhost` (WebAuthn needs a domain, not an IP).
pub fn browser_base(profile: &Profile) -> Option<String> {
    let ep = profile.endpoint("keys")?;
    if let Some(p) = &ep.public {
        return Some(p.trim_end_matches('/').to_string());
    }
    let mut u = Url::parse(&ep.api).ok()?;
    if matches!(u.host_str(), Some("127.0.0.1") | Some("[::1]") | Some("::1")) {
        u.set_host(Some("localhost")).ok()?;
    }
    Some(u.as_str().trim_end_matches('/').to_string())
}

/// Start a sign-in: bind a one-shot listener on 127.0.0.1 and build the
/// authorize URL for `browser_base`.
pub fn start(browser_base: &str, device: &str) -> std::io::Result<Pending> {
    let listener = TcpListener::bind("127.0.0.1:0")?;
    let port = listener.local_addr()?.port();
    let redirect_uri = format!("http://127.0.0.1:{port}/callback");
    let state = random(16);
    let pkce = Pkce::new();
    let mut u = Url::parse(&format!("{browser_base}/device/authorize")).map_err(std::io::Error::other)?;
    u.query_pairs_mut()
        .append_pair("client_id", CLIENT_ID)
        .append_pair("redirect_uri", &redirect_uri)
        .append_pair("code_challenge", &pkce.challenge)
        .append_pair("code_challenge_method", "S256")
        .append_pair("state", &state)
        .append_pair("device", device);
    Ok(Pending { listener, redirect_uri, state, pkce, authorize_url: u.to_string() })
}

#[derive(Debug, thiserror::Error, PartialEq)]
pub enum SignInError {
    #[error("sign-in timed out")]
    TimedOut,
    #[error("sign-in was declined")]
    Denied,
    #[error("sign-in was cancelled")]
    Cancelled,
    #[error("sign-in reply didn't match this request")]
    Mismatch,
    #[error("{0}")]
    Io(String),
    #[error(transparent)]
    Api(#[from] ApiError),
}

const PAGE_OK: &str = "<!doctype html><meta charset=utf-8><title>Farfield</title>\
<body style=\"font:15px -apple-system,sans-serif;background:#f3e5d1;color:#0e222d;display:grid;place-items:center;height:90vh\">\
<p>Signed in. You can close this tab.</p>";
const PAGE_NO: &str = "<!doctype html><meta charset=utf-8><title>Farfield</title>\
<body style=\"font:15px -apple-system,sans-serif;background:#f3e5d1;color:#0e222d;display:grid;place-items:center;height:90vh\">\
<p>Not signed in. You can close this tab.</p>";

impl Pending {
    /// Wait (blocking — run it off the UI thread) for the browser to come
    /// back with the code. Accepts connections until one carries the
    /// callback (a browser may probe for /favicon.ico first), then closes.
    pub fn wait_for_code(&self, timeout: Duration) -> Result<String, SignInError> {
        self.wait_for_code_or(timeout, &AtomicBool::new(false))
    }

    /// `wait_for_code`, ending early with `Cancelled` once `cancel` is set.
    pub fn wait_for_code_or(&self, timeout: Duration, cancel: &AtomicBool) -> Result<String, SignInError> {
        let deadline = Instant::now() + timeout;
        self.listener.set_nonblocking(true).map_err(|e| SignInError::Io(e.to_string()))?;
        loop {
            if cancel.load(Ordering::Relaxed) {
                return Err(SignInError::Cancelled);
            }
            if Instant::now() >= deadline {
                return Err(SignInError::TimedOut);
            }
            let (mut stream, _) = match self.listener.accept() {
                Ok(s) => s,
                Err(e) if e.kind() == std::io::ErrorKind::WouldBlock => {
                    std::thread::sleep(Duration::from_millis(50));
                    continue;
                }
                Err(e) => return Err(SignInError::Io(e.to_string())),
            };
            let _ = stream.set_nonblocking(false);
            let _ = stream.set_read_timeout(Some(Duration::from_secs(5)));
            let mut buf = [0u8; 8192];
            let n = stream.read(&mut buf).unwrap_or(0);
            let head = String::from_utf8_lossy(&buf[..n]);
            let target = head.lines().next().and_then(|l| l.split_whitespace().nth(1)).unwrap_or("");
            let Ok(u) = Url::parse(&format!("http://127.0.0.1{target}")) else { continue };
            if u.path() != "/callback" {
                let _ = stream.write_all(b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n");
                continue;
            }
            let q: std::collections::HashMap<String, String> = u.query_pairs().into_owned().collect();
            let ok = q.get("state") == Some(&self.state) && q.contains_key("code");
            let body = if ok { PAGE_OK } else { PAGE_NO };
            let _ = write!(
                stream,
                "HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nCache-Control: no-store\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                body.len()
            );
            if q.get("state") != Some(&self.state) {
                return Err(SignInError::Mismatch);
            }
            if q.contains_key("error") {
                return Err(SignInError::Denied);
            }
            return q.get("code").cloned().ok_or(SignInError::Mismatch);
        }
    }

    /// Redeem the code at the keys app's private address for a minted key.
    pub async fn redeem(&self, keys: &ServiceClient, code: &str) -> Result<Credential, SignInError> {
        let form = [
            ("grant_type", "authorization_code"),
            ("code", code),
            ("code_verifier", self.pkce.verifier.as_str()),
            ("redirect_uri", self.redirect_uri.as_str()),
            ("client_id", CLIENT_ID),
        ];
        let body: String = url::form_urlencoded::Serializer::new(String::new()).extend_pairs(form).finish();
        let rb = keys
            .request(Method::POST, "/device/token")?
            .header(reqwest::header::CONTENT_TYPE, "application/x-www-form-urlencoded")
            .body(body);
        let resp = keys.send(rb, true).await?;
        let v: crate::transport::Versioned<serde_json::Value> = ServiceClient::json(resp).await?;
        v.value["access_token"]
            .as_str()
            .and_then(Credential::new)
            .ok_or_else(|| SignInError::Api(ApiError::Decode("no access_token".into())))
    }
}

/// This machine's name, for the key's label in the keys console.
pub fn device_name() -> String {
    #[cfg(target_os = "macos")]
    if let Ok(o) = std::process::Command::new("scutil").args(["--get", "ComputerName"]).output() {
        let s = String::from_utf8_lossy(&o.stdout).trim().to_string();
        if !s.is_empty() {
            return s;
        }
    }
    std::env::var("HOSTNAME").ok().filter(|s| !s.is_empty()).unwrap_or_else(|| "this computer".into())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pkce_challenge_is_s256_of_verifier() {
        let p = Pkce::new();
        assert_eq!(p.challenge, b64(&Sha256::digest(p.verifier.as_bytes())));
        assert!(p.verifier.len() >= 43, "RFC 7636 minimum length");
        assert_ne!(Pkce::new().verifier, p.verifier);
    }

    #[test]
    fn dev_browser_flow_uses_localhost() {
        let p = Profile::local();
        assert_eq!(browser_base(&p).unwrap(), "http://localhost:8801");
        let t = Profile::from_address("fleet", "box", "box.t.ts.net").unwrap();
        assert_eq!(browser_base(&t).unwrap(), "https://keys.farfield.systems");
    }

    #[test]
    fn callback_checks_state_and_ignores_strays() {
        let p = start("http://localhost:8801", "Test Mac").unwrap();
        assert!(p.authorize_url.contains("code_challenge_method=S256"));
        assert!(!p.authorize_url.contains(&p.pkce.verifier), "verifier never leaves");
        let uri = p.redirect_uri.clone();
        let state = p.state.clone();
        let h = std::thread::spawn(move || {
            let get = |path: &str| {
                let u = Url::parse(&uri).unwrap();
                let mut s = std::net::TcpStream::connect((u.host_str().unwrap(), u.port().unwrap())).unwrap();
                write!(s, "GET {path} HTTP/1.1\r\nHost: x\r\n\r\n").unwrap();
                let mut out = String::new();
                let _ = s.read_to_string(&mut out);
                out
            };
            get("/favicon.ico");
            get(&format!("/callback?code=abc&state={state}"))
        });
        assert_eq!(p.wait_for_code(Duration::from_secs(5)).unwrap(), "abc");
        assert!(h.join().unwrap().contains("Signed in"));
    }

    #[test]
    fn wrong_state_and_denial_fail() {
        let p = start("http://localhost:8801", "x").unwrap();
        let uri = p.redirect_uri.clone();
        std::thread::spawn(move || {
            let u = Url::parse(&uri).unwrap();
            let mut s = std::net::TcpStream::connect((u.host_str().unwrap(), u.port().unwrap())).unwrap();
            write!(s, "GET /callback?code=abc&state=forged HTTP/1.1\r\n\r\n").unwrap();
        });
        assert_eq!(p.wait_for_code(Duration::from_secs(5)), Err(SignInError::Mismatch));
        let p = start("http://localhost:8801", "x").unwrap();
        assert_eq!(p.wait_for_code(Duration::from_millis(200)), Err(SignInError::TimedOut));
    }
}
