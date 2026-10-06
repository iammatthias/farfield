//! Connection profiles: where each service is reached, privately for API and
//! media traffic and publicly for share links.
//!
//! API traffic goes to a private endpoint — a per-service HTTPS address on the
//! tailnet (`tailscale serve --https=<port>` on the homelab) — so the client
//! keeps working when the public edge does not. Public URLs are only ever
//! used to build links meant for other people.

use crate::registry;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use url::Url;

/// One service's addresses within a profile.
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub struct Endpoint {
    /// Private API base, e.g. `https://homelab.tail0000.ts.net:8787`.
    pub api: String,
    /// Public base for share links, e.g. `https://content.farfield.systems`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub public: Option<String>,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub struct Profile {
    pub id: String,
    pub name: String,
    pub endpoints: BTreeMap<String, Endpoint>,
}

#[derive(Debug, Clone, thiserror::Error, PartialEq, Eq)]
pub enum EndpointError {
    #[error("not a URL: {0}")]
    Invalid(String),
    #[error("{0} must use https — plain http is only allowed to this machine (loopback)")]
    Insecure(String),
    #[error("{0} must be a bare origin (no path, query, or credentials)")]
    NotOrigin(String),
}

/// Is this host this machine? Plain HTTP is allowed only there, for the dev
/// fleet; anything that crosses a network must be TLS.
pub fn is_loopback(host: &str) -> bool {
    let h = host.trim_start_matches('[').trim_end_matches(']');
    h == "localhost" || h == "::1" || h.starts_with("127.")
}

/// Validate an endpoint base and return it normalised (no trailing slash).
pub fn validate_base(raw: &str) -> Result<Url, EndpointError> {
    let u = Url::parse(raw.trim()).map_err(|_| EndpointError::Invalid(raw.into()))?;
    let host = u.host_str().ok_or_else(|| EndpointError::Invalid(raw.into()))?;
    match u.scheme() {
        "https" => {}
        "http" if is_loopback(host) => {}
        "http" => return Err(EndpointError::Insecure(raw.into())),
        _ => return Err(EndpointError::Invalid(raw.into())),
    }
    if !u.username().is_empty()
        || u.password().is_some()
        || u.query().is_some()
        || (u.path() != "/" && !u.path().is_empty())
    {
        return Err(EndpointError::NotOrigin(raw.into()));
    }
    Ok(u)
}

/// The origin string of a URL (`scheme://host:port`), the unit credentials are
/// bound to.
pub fn origin(u: &Url) -> String {
    u.origin().ascii_serialization()
}

impl Profile {
    /// Every service on a tailnet host, each at its own HTTPS port — the shape
    /// `tailscale serve --https=<port>` gives the homelab.
    pub fn tailnet(id: &str, name: &str, host: &str) -> Self {
        let host = host.trim_end_matches('.');
        let endpoints = registry::services()
            .iter()
            .map(|s| (s.name.clone(), Endpoint { api: format!("https://{host}:{}", s.port), public: s.public_url() }))
            .collect();
        Profile { id: id.into(), name: name.into(), endpoints }
    }

    /// The local dev fleet (`make dev`): loopback HTTP on the registry ports.
    pub fn local() -> Self {
        let endpoints = registry::services()
            .iter()
            .map(|s| (s.name.clone(), Endpoint { api: format!("http://127.0.0.1:{}", s.port), public: None }))
            .collect();
        Profile { id: "local".into(), name: "This Mac (dev fleet)".into(), endpoints }
    }

    pub fn endpoint(&self, service: &str) -> Option<&Endpoint> {
        self.endpoints.get(service)
    }

    /// Every endpoint must validate before a profile is saved or used.
    pub fn validate(&self) -> Result<(), EndpointError> {
        for e in self.endpoints.values() {
            validate_base(&e.api)?;
            if let Some(p) = &e.public {
                let u = Url::parse(p).map_err(|_| EndpointError::Invalid(p.clone()))?;
                if u.scheme() != "https" && !u.host_str().is_some_and(is_loopback) {
                    return Err(EndpointError::Insecure(p.clone()));
                }
            }
        }
        Ok(())
    }
}

/// A device on the tailnet, read from `tailscale status --json` (OS
/// Tailscale; the client never runs its own) — only when the person asks.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TailnetPeer {
    pub host_name: String,
    pub dns_name: String,
    pub online: bool,
}

/// Parse `tailscale status --json` into the devices to offer: online first,
/// then by name. No device is assumed to be the fleet.
pub fn parse_tailscale_status(json: &str) -> Result<(bool, Vec<TailnetPeer>), String> {
    let v: serde_json::Value = serde_json::from_str(json).map_err(|e| e.to_string())?;
    let running = v["BackendState"].as_str() == Some("Running");
    let mut peers: Vec<TailnetPeer> = v["Peer"]
        .as_object()
        .map(|m| {
            m.values()
                .filter_map(|p| {
                    Some(TailnetPeer {
                        host_name: p["HostName"].as_str()?.to_string(),
                        dns_name: p["DNSName"].as_str()?.trim_end_matches('.').to_string(),
                        online: p["Online"].as_bool().unwrap_or(false),
                    })
                })
                .collect()
        })
        .unwrap_or_default();
    peers.sort_by_key(|p| (!p.online, p.host_name.to_lowercase()));
    Ok((running, peers))
}

/// Run the OS Tailscale CLI. Returns None when Tailscale is not installed.
pub fn tailscale_status() -> Option<String> {
    for bin in ["tailscale", "/usr/local/bin/tailscale", "/Applications/Tailscale.app/Contents/MacOS/Tailscale"] {
        if let Ok(o) = std::process::Command::new(bin).args(["status", "--json"]).output() {
            if o.status.success() {
                return Some(String::from_utf8_lossy(&o.stdout).into_owned());
            }
        }
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn tls_is_required_off_loopback() {
        assert!(validate_base("https://homelab.tail.ts.net:8787").is_ok());
        assert!(validate_base("http://127.0.0.1:8787").is_ok());
        assert!(validate_base("http://localhost:8787").is_ok());
        assert!(validate_base("http://[::1]:8787").is_ok());
        assert_eq!(validate_base("http://homelab:8787"), Err(EndpointError::Insecure("http://homelab:8787".into())));
        assert!(matches!(validate_base("https://x/api"), Err(EndpointError::NotOrigin(_))));
        assert!(matches!(validate_base("https://u:p@x"), Err(EndpointError::NotOrigin(_))));
        assert!(matches!(validate_base("ftp://x"), Err(EndpointError::Invalid(_))));
    }

    #[test]
    fn tailnet_profile_uses_registry_ports_and_keeps_public_links_separate() {
        let p = Profile::tailnet("home", "Homelab", "homelab.tail0000.ts.net.");
        let c = p.endpoint("content").unwrap();
        assert_eq!(c.api, "https://homelab.tail0000.ts.net:8787");
        assert_eq!(c.public.as_deref(), Some("https://content.farfield.systems"));
        assert_eq!(p.endpoint("backup").unwrap().public, None);
        p.validate().unwrap();
        Profile::local().validate().unwrap();
    }

    #[test]
    fn tailscale_status_orders_online_then_name() {
        let j = r#"{"BackendState":"Running","Peer":{
            "a":{"HostName":"zeta","DNSName":"zeta.t.ts.net.","Online":true},
            "b":{"HostName":"Alpha","DNSName":"alpha.t.ts.net.","Online":false},
            "c":{"HostName":"beta","DNSName":"beta.t.ts.net.","Online":true}}}"#;
        let (running, peers) = parse_tailscale_status(j).unwrap();
        assert!(running);
        let names: Vec<_> = peers.iter().map(|p| p.host_name.as_str()).collect();
        assert_eq!(names, ["beta", "zeta", "Alpha"]);
    }
}

/// What a person types for "where is my fleet": a tailnet name
/// (`homelab.tail1234.ts.net`), the same as a URL, or a loopback address for
/// the dev fleet. Scheme defaults to https; any port or path is dropped
/// because each service has its own port.
pub fn parse_fleet_address(input: &str) -> Result<(String, String), EndpointError> {
    let raw = input.trim().trim_end_matches('/');
    if raw.is_empty() {
        return Err(EndpointError::Invalid(input.into()));
    }
    let with_scheme = if raw.contains("://") {
        raw.to_string()
    } else if is_loopback(raw.split(':').next().unwrap_or("")) {
        format!("http://{raw}")
    } else {
        format!("https://{raw}")
    };
    let u = Url::parse(&with_scheme).map_err(|_| EndpointError::Invalid(input.into()))?;
    let host = u.host_str().ok_or_else(|| EndpointError::Invalid(input.into()))?.trim_end_matches('.').to_string();
    match u.scheme() {
        "https" => {}
        "http" if is_loopback(&host) => {}
        "http" => return Err(EndpointError::Insecure(input.into())),
        _ => return Err(EndpointError::Invalid(input.into())),
    }
    Ok((u.scheme().to_string(), host))
}

impl Profile {
    /// A profile from a typed fleet address: every service at
    /// `<scheme>://<host>:<port>`, public share links from the registry.
    pub fn from_address(id: &str, name: &str, address: &str) -> Result<Self, EndpointError> {
        let (scheme, host) = parse_fleet_address(address)?;
        let host_url = if host.contains(':') { format!("[{host}]") } else { host.clone() };
        let endpoints = registry::services()
            .iter()
            .map(|s| {
                (s.name.clone(), Endpoint { api: format!("{scheme}://{host_url}:{}", s.port), public: s.public_url() })
            })
            .collect();
        let p = Profile { id: id.into(), name: name.into(), endpoints };
        p.validate()?;
        Ok(p)
    }

    /// Replace one service's private address (validated).
    pub fn set_api(&mut self, service: &str, api: &str) -> Result<(), EndpointError> {
        let u = validate_base(api)?;
        let api = u.as_str().trim_end_matches('/').to_string();
        let public = registry::lookup(service).and_then(|s| s.public_url());
        self.endpoints.entry(service.into()).and_modify(|e| e.api = api.clone()).or_insert(Endpoint { api, public });
        Ok(())
    }

    /// The host most endpoints share — what Settings shows as "the fleet's
    /// address". None when they disagree.
    pub fn common_host(&self) -> Option<String> {
        let mut hosts =
            self.endpoints.values().filter_map(|e| Url::parse(&e.api).ok()?.host_str().map(|h| h.to_string()));
        let first = hosts.next()?;
        hosts.all(|h| h == first).then_some(first)
    }
}

#[cfg(test)]
mod address_tests {
    use super::*;

    #[test]
    fn typed_addresses() {
        assert_eq!(
            parse_fleet_address("homelab.tail1234.ts.net").unwrap(),
            ("https".into(), "homelab.tail1234.ts.net".into())
        );
        assert_eq!(parse_fleet_address("https://homelab.tail1234.ts.net:8787/x").unwrap().1, "homelab.tail1234.ts.net");
        assert_eq!(parse_fleet_address("127.0.0.1").unwrap(), ("http".into(), "127.0.0.1".into()));
        assert!(matches!(parse_fleet_address("http://homelab"), Err(EndpointError::Insecure(_))));
        assert!(parse_fleet_address("  ").is_err());
        let p = Profile::from_address("home", "Home", "homelab.t.ts.net").unwrap();
        assert_eq!(p.endpoint("feed").unwrap().api, "https://homelab.t.ts.net:8788");
        assert_eq!(p.common_host().as_deref(), Some("homelab.t.ts.net"));
        let mut p = p;
        p.set_api("feed", "https://other.t.ts.net:9000").unwrap();
        assert_eq!(p.common_host(), None);
        assert!(p.set_api("feed", "http://other:1").is_err());
    }
}
