//! `farfield-content` — the content service (`content.farfield.systems`).
//!
//! A thin binary over the shared records engine: serves the `posts`,
//! `open-source`, `recipes`, `art`, `melange`, `media`, and `series`
//! collections from its own SQLite DB.
//!
//! Config is environment-driven for local dev. On the server this is wrapped
//! by `deploy/farfield.toml`; for now, sensible localhost defaults.

use std::net::SocketAddr;
use std::path::PathBuf;

use farfield_records::{Config, serve};

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let addr: SocketAddr = env_or("FARFIELD_CONTENT_ADDR", "127.0.0.1:8787").parse()?;
    let db_path = PathBuf::from(env_or("FARFIELD_CONTENT_DB", "farfield-content.db"));
    let schema_dir = PathBuf::from(env_or("FARFIELD_CONTENT_SCHEMAS", "schemas/content"));

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
        service_name: "content".to_string(),
    })
    .await
}

fn env_or(var: &str, default: &str) -> String {
    std::env::var(var).unwrap_or_else(|_| default.to_string())
}
