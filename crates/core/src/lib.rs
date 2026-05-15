//! `farfield-core` — record types, canonical encoding, and content identifiers.
//!
//! A [`Record`] is a typed object: a `body` markdown field plus schema-defined
//! frontmatter fields, modelled here as a JSON object. It is canonically
//! encoded to DAG-CBOR and content-hashed to a [`Cid`]. The CID's one job is
//! to be a stable HTTP ETag — the same record content always yields the same
//! CID, so a consumer can cache on it.
//!
//! This crate is deliberately small and pure: no SQLite, no HTTP, no I/O. It
//! is the seam every other crate hashes through.

use serde_json::{Map, Value};
use sha2::{Digest, Sha256};

pub use cid::Cid;

/// DAG-CBOR multicodec — the codec for an encoded record.
pub const DAG_CBOR: u64 = 0x71;
/// Raw multicodec — the codec for a binary blob (an image).
pub const RAW: u64 = 0x55;
/// SHA2-256 multihash code.
const SHA2_256: u64 = 0x12;

/// Errors from canonical encoding.
#[derive(Debug, thiserror::Error)]
pub enum EncodeError {
    /// The record could not be encoded as canonical DAG-CBOR.
    #[error("canonical DAG-CBOR encoding failed: {0}")]
    Cbor(#[from] serde_ipld_dagcbor::EncodeError<std::collections::TryReserveError>),
}

/// A content record: an object of fields, one of which is conventionally
/// `body` (markdown). The field set and types are governed by a schema in the
/// `farfield-schema` crate; `core` treats a record as already-canonical input
/// and only encodes and hashes it.
#[derive(Debug, Clone, PartialEq)]
pub struct Record {
    fields: Map<String, Value>,
}

impl Record {
    /// Wrap an object of fields as a record.
    pub fn new(fields: Map<String, Value>) -> Self {
        Self { fields }
    }

    /// The record's fields.
    pub fn fields(&self) -> &Map<String, Value> {
        &self.fields
    }

    /// The record as a JSON value — the serving representation the API returns.
    pub fn to_json(&self) -> Value {
        Value::Object(self.fields.clone())
    }

    /// Canonical DAG-CBOR bytes. Deterministic: equal records encode to equal
    /// bytes (DAG-CBOR mandates sorted map keys and a single encoding per
    /// value), so the CID is stable.
    pub fn canonical_bytes(&self) -> Result<Vec<u8>, EncodeError> {
        Ok(serde_ipld_dagcbor::to_vec(&self.fields)?)
    }

    /// The record's content identifier — a CID over its canonical bytes.
    pub fn cid(&self) -> Result<Cid, EncodeError> {
        Ok(cid_for(DAG_CBOR, &self.canonical_bytes()?))
    }
}

/// Build a CIDv1 over `bytes` with the given multicodec, using a SHA2-256
/// multihash.
pub fn cid_for(codec: u64, bytes: &[u8]) -> Cid {
    let digest = Sha256::digest(bytes);
    let mh = multihash::Multihash::<64>::wrap(SHA2_256, &digest)
        .expect("a 32-byte SHA2-256 digest always fits a 64-byte multihash");
    Cid::new_v1(codec, mh)
}

/// The CID of a binary blob (raw codec) — used for image bytes in the blob
/// store. A blob is self-verifying: re-hashing its bytes reproduces this CID.
pub fn blob_cid(bytes: &[u8]) -> Cid {
    cid_for(RAW, bytes)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn record(v: Value) -> Record {
        Record::new(v.as_object().expect("test value must be an object").clone())
    }

    #[test]
    fn encoding_is_deterministic() {
        let r = record(json!({ "title": "Pale Blue Dot", "body": "# hi" }));
        assert_eq!(r.canonical_bytes().unwrap(), r.canonical_bytes().unwrap());
    }

    #[test]
    fn field_order_does_not_change_the_cid() {
        // DAG-CBOR sorts map keys, so insertion order is irrelevant.
        let a = record(json!({ "title": "x", "body": "y" }));
        let b = record(json!({ "body": "y", "title": "x" }));
        assert_eq!(a.cid().unwrap(), b.cid().unwrap());
    }

    #[test]
    fn different_content_yields_a_different_cid() {
        let a = record(json!({ "body": "one" }));
        let b = record(json!({ "body": "two" }));
        assert_ne!(a.cid().unwrap(), b.cid().unwrap());
    }

    #[test]
    fn cid_is_v1_dag_cbor_and_round_trips_as_a_string() {
        let r = record(json!({ "body": "hello" }));
        let cid = r.cid().unwrap();
        assert_eq!(cid.codec(), DAG_CBOR);
        assert_eq!(cid.version(), cid::Version::V1);
        let parsed: Cid = cid.to_string().parse().unwrap();
        assert_eq!(parsed, cid);
    }

    #[test]
    fn blob_cid_uses_the_raw_codec() {
        let cid = blob_cid(b"\x89PNG\r\n\x1a\n");
        assert_eq!(cid.codec(), RAW);
    }
}
