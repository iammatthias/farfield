//! Typed clients, one module per service.
pub mod blobs;
pub mod content;
pub mod feed;
mod misc;
pub use misc::{apex, backup, bookmarks, daily, library, pulse, qr, scrap, sideload, status, switchboard};
