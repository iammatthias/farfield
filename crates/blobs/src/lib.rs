//! `farfield-blobs` — the content-addressed blob store.
//!
//! Image bytes live in R2, keyed by CID. Two backends behind one trait: an
//! R2 backend for production and a local-directory backend for `--local`
//! development. Derives blob metadata (dimensions, blurhash, dominant color)
//! on upload, and reclaims unreferenced blobs via dry-run-first GC.
//!
//! Not yet implemented — this is the scaffolded crate boundary.
