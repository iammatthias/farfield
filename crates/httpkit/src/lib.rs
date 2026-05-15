//! `farfield-httpkit` — HTTP scaffolding shared by every app service.
//!
//! Bearer-token auth, the read-socket / write-socket split (GET-only on the
//! `cloudflared`-fronted read socket; full API on the tailnet write socket),
//! uniform JSON error bodies, rate limiting, and the common `/status` bits.
//! Used by the records service and the blob service alike.
//!
//! Not yet implemented — this is the scaffolded crate boundary.
