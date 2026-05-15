//! `farfield-feed` — the feed service (`feed.farfield.systems`).
//!
//! Identical in behaviour to the content service — the shared records engine
//! — serving the `feed` collection from its own SQLite DB. RSS and rendering
//! are the website's job, not the backend's.

use std::net::SocketAddr;
use std::path::PathBuf;

use farfield_records::{Config, serve};

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let addr: SocketAddr = env_or("FARFIELD_FEED_ADDR", "127.0.0.1:8788").parse()?;
    let db_path = PathBuf::from(env_or("FARFIELD_FEED_DB", "farfield-feed.db"));
    let schema_dir = PathBuf::from(env_or("FARFIELD_FEED_SCHEMAS", "schemas/feed"));

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

    serve(Config {
        addr,
        db_path,
        schema_dir,
        tokens,
        service_name: "feed".to_string(),
    })
    .await
}

fn env_or(var: &str, default: &str) -> String {
    std::env::var(var).unwrap_or_else(|_| default.to_string())
}
