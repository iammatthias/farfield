//! `farfield-snapshot` — database backup to R2.
//!
//! `VACUUM INTO` a timestamped file (after a free-disk check), upload to R2,
//! prune by retention policy. Restore downloads and swaps in a snapshot.
//! Snapshot failures are loud — surfaced by `farfield status`.
//!
//! Not yet implemented — this is the scaffolded crate boundary.
