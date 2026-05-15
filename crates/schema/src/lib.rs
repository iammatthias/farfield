//! `farfield-schema` — the lexicon-lite schema system.
//!
//! Loads one JSON schema file per content type, validates records with a
//! small hand-rolled validator, enforces additive-only schema changes, and
//! powers `farfield codegen` (emits TypeScript types). The server and codegen
//! share this one loader so types and validation cannot drift.
//!
//! Not yet implemented — this is the scaffolded crate boundary.
