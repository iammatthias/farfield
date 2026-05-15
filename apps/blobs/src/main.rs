//! `farfield-blobs` — the shared blob service (`blobs.farfield.systems`).
//!
//! A standalone content-addressed blob store. `content` and `feed` (and any
//! future app) upload images here and reference them by CID. Owns its own
//! blob-index DB; GC takes the union of referenced CIDs from each consuming
//! app, so a blob is kept until every app has dropped it.
//!
//! Not yet implemented — this is the scaffolded entry point.

fn main() {
    eprintln!("farfield-blobs: not yet implemented (scaffold)");
}
