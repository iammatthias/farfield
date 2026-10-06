//! farfield-core: everything the native clients share that is not UI —
//! the fleet registry, connection profiles, credentials, the transport, typed
//! service clients, the response cache, local drafts and conflict-safe sync.
//!
//! Portable by design (macOS, Linux, iOS): no UI types, no platform calls
//! outside the secret store and the Tailscale CLI probe.

pub mod api;
pub mod merge;
pub mod profile;
pub mod registry;
pub mod secret;
pub mod session;
pub mod signin;
pub mod store;
pub mod sync;
pub mod transport;
pub mod upload;

pub use session::{Freshness, Latest, Loaded, Session};
pub use transport::{runtime, spawn, ApiError};
