//! `farfield-store` — SQLite-backed storage (rusqlite).
//!
//! Holds `records`, `blobs`, `deletions`, `collections_meta`, and the
//! `change_seq` monotonic counter. All writes go through one dedicated
//! writer thread; reads use a short-lived connection pool. WAL mode.
//!
//! Not yet implemented — this is the scaffolded crate boundary.
