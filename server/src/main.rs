//! `farfield-server` — the one Farfield backend binary.
//!
//! Axum records API. Two listeners: a read socket on `127.0.0.1` (GET only,
//! fronted by `cloudflared`) and a write socket on the tailnet interface
//! (full API). Runs the snapshot loop.
//!
//! Not yet implemented — this is the scaffolded entry point.

fn main() {
    eprintln!("farfield-server: not yet implemented (scaffold)");
}
