//! `farfield-records` — the typed-records service engine.
//!
//! Assembles `store` + `schema` + `httpkit` into the complete records HTTP
//! service: generic CRUD, list with the `(rkey, cid)` ETag, the `?since=<seq>`
//! cursor, schema validation on write. Both the `content` and `feed` apps are
//! thin binaries over `serve(config)` — same engine, different DB, schema
//! directory, and domain.
//!
//! Not yet implemented — this is the scaffolded crate boundary.
