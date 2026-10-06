//! Typed clients, one module per service.
pub mod blobs;
pub mod content;
pub mod feed;
mod misc;
pub use misc::{apex, backup, bookmarks, daily, library, probe, probe_path, pulse, qr, scrap, sideload, status, switchboard};
