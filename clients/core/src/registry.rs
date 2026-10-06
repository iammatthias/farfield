//! The fleet as lib/fleet declares it, and the quick actions lib/capability
//! declares, embedded at build time from lib/capability/cmd/manifest.

use serde::Deserialize;
use std::sync::OnceLock;

const MANIFEST: &str = include_str!(concat!(env!("OUT_DIR"), "/fleet.json"));

/// One farfield service.
#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct Service {
    pub name: String,
    pub port: u16,
    /// The public hostname, "" for a tailnet-only service.
    #[serde(default)]
    pub public: String,
    /// Runs on the host as a systemd unit rather than in compose.
    #[serde(default)]
    pub host: bool,
}

impl Service {
    /// The public root, for share links — never for API traffic.
    pub fn public_url(&self) -> Option<String> {
        (!self.public.is_empty()).then(|| format!("https://{}", self.public))
    }
}

/// One capability command (the `/feed`, `/bm`, `/qr` table).
#[derive(Clone, Debug, Deserialize)]
pub struct Command {
    pub name: String,
    #[serde(default)]
    pub aliases: Vec<String>,
    pub summary: String,
    pub usage: String,
    #[serde(default, rename = "takesFiles")]
    pub takes_files: bool,
}

#[derive(Deserialize)]
struct Manifest {
    services: Vec<Service>,
    commands: Vec<Command>,
}

fn manifest() -> &'static Manifest {
    static M: OnceLock<Manifest> = OnceLock::new();
    M.get_or_init(|| serde_json::from_str(MANIFEST).expect("embedded fleet manifest"))
}

/// Every service, in port order.
pub fn services() -> &'static [Service] {
    &manifest().services
}

pub fn lookup(name: &str) -> Option<&'static Service> {
    services().iter().find(|s| s.name == name)
}

pub fn commands() -> &'static [Command] {
    &manifest().commands
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn registry_matches_lib_fleet_shape() {
        assert!(services().len() >= 14);
        let content = lookup("content").unwrap();
        assert_eq!(content.port, 8787);
        assert_eq!(content.public_url().as_deref(), Some("https://content.farfield.systems"));
        // backup is tailnet-only by design
        assert_eq!(lookup("backup").unwrap().public_url(), None);
        assert!(commands().iter().any(|c| c.name == "feed"));
    }
}
