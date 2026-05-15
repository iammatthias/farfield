//! `farfield-core` — record types, canonical encoding, and content identifiers.
//!
//! A record is a typed object (one `body` markdown field plus schema-defined
//! frontmatter fields). It is canonically encoded to a stable byte form and
//! content-hashed to a CID. The CID's job is to be a stable HTTP ETag: same
//! content in, same CID out.
//!
//! Not yet implemented — this is the scaffolded crate boundary.
