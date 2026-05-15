//! `farfield-store` — the SQLite record store.
//!
//! Holds the current published state for one app service: `records`,
//! `collections_meta`, `deletions`, and the `change_seq` monotonic counter.
//! No history — git is the history layer; `change_seq` exists only to give
//! incremental consumers a safe cursor (a wall-clock timestamp is not safe).
//!
//! This crate is synchronous and owns one [`Connection`]. The threading model
//! — a dedicated writer thread, a read pool — belongs to the service that
//! wraps it (`farfield-records`). Here it is plain, transactional SQL.

use std::path::Path;

use farfield_core::{EncodeError, Record};
use rusqlite::{Connection, OptionalExtension, params};
use serde_json::Value;
use sha2::{Digest, Sha256};
use time::OffsetDateTime;
use time::format_description::well_known::Rfc3339;

const MIGRATION: &str = r#"
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS change_seq (
    id   INTEGER PRIMARY KEY CHECK (id = 1),
    next INTEGER NOT NULL
);
INSERT OR IGNORE INTO change_seq (id, next) VALUES (1, 1);

CREATE TABLE IF NOT EXISTS records (
    collection TEXT NOT NULL,
    rkey       TEXT NOT NULL,
    cid        TEXT NOT NULL,
    bytes      BLOB NOT NULL,   -- canonical DAG-CBOR (hashes to cid)
    json       TEXT NOT NULL,   -- serving representation
    seq        INTEGER NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (collection, rkey)
);
CREATE INDEX IF NOT EXISTS records_by_seq ON records (collection, seq);

CREATE TABLE IF NOT EXISTS collections_meta (
    collection TEXT PRIMARY KEY,
    list_etag  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS deletions (
    collection TEXT NOT NULL,
    rkey       TEXT NOT NULL,
    seq        INTEGER NOT NULL,
    deleted_at TEXT NOT NULL,
    PRIMARY KEY (collection, rkey)
);
CREATE INDEX IF NOT EXISTS deletions_by_seq ON deletions (collection, seq);
"#;

/// Errors from the store.
#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    /// A SQLite-level failure.
    #[error("sqlite: {0}")]
    Sqlite(#[from] rusqlite::Error),
    /// A record could not be canonically encoded.
    #[error(transparent)]
    Encode(#[from] EncodeError),
    /// A stored `json` column was not valid JSON (database corruption).
    #[error("stored json is corrupt for {0}/{1}")]
    CorruptJson(String, String),
    /// The system clock could not be formatted.
    #[error("time formatting: {0}")]
    Time(#[from] time::error::Format),
}

/// Store result alias.
pub type Result<T> = std::result::Result<T, StoreError>;

/// A record as stored — the serving representation the API returns.
#[derive(Debug, Clone, PartialEq)]
pub struct Stored {
    pub collection: String,
    pub rkey: String,
    /// Content identifier — the record's HTTP ETag.
    pub cid: String,
    /// The record fields as JSON.
    pub json: Value,
    /// The `change_seq` value at this record's last write — the sync cursor.
    pub seq: i64,
    pub created_at: String,
    pub updated_at: String,
}

/// A tombstone — a record that was deleted, kept so incremental consumers can
/// evict it. Cleared if the rkey is later recreated.
#[derive(Debug, Clone, PartialEq)]
pub struct Tombstone {
    pub collection: String,
    pub rkey: String,
    pub seq: i64,
    pub deleted_at: String,
}

/// The outcome of a [`Store::put`].
#[derive(Debug, Clone, PartialEq)]
pub enum Write {
    /// A new record was created.
    Created { cid: String, seq: i64 },
    /// An existing record was replaced.
    Updated { cid: String, seq: i64 },
    /// The submitted content hashed to the record's current CID — nothing
    /// was written, and no cursor moved.
    Unchanged { cid: String },
}

/// A SQLite-backed record store. Owns one connection; not `Sync`.
pub struct Store {
    conn: Connection,
}

impl Store {
    /// Open (creating if absent) a store at `path`.
    pub fn open(path: impl AsRef<Path>) -> Result<Self> {
        Self::init(Connection::open(path)?)
    }

    /// Open an in-memory store — for tests.
    pub fn open_in_memory() -> Result<Self> {
        Self::init(Connection::open_in_memory()?)
    }

    fn init(conn: Connection) -> Result<Self> {
        conn.execute_batch(MIGRATION)?;
        Ok(Self { conn })
    }

    /// Create or replace a record. A submission whose canonical content
    /// hashes to the record's current CID is a no-op ([`Write::Unchanged`]).
    pub fn put(&mut self, collection: &str, rkey: &str, record: &Record) -> Result<Write> {
        let bytes = record.canonical_bytes()?;
        let cid = farfield_core::cid_for(farfield_core::DAG_CBOR, &bytes).to_string();
        let json = serde_json::to_string(&record.to_json()).expect("a JSON object serializes");
        let now = now_rfc3339()?;

        let tx = self.conn.transaction()?;
        let existing: Option<(String, String)> = tx
            .query_row(
                "SELECT cid, created_at FROM records WHERE collection = ?1 AND rkey = ?2",
                params![collection, rkey],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .optional()?;

        if let Some((existing_cid, _)) = &existing
            && *existing_cid == cid
        {
            return Ok(Write::Unchanged { cid });
        }

        let seq = next_seq(&tx)?;
        let created_at = existing
            .as_ref()
            .map(|(_, c)| c.clone())
            .unwrap_or_else(|| now.clone());

        tx.execute(
            "INSERT INTO records
                 (collection, rkey, cid, bytes, json, seq, created_at, updated_at)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)
             ON CONFLICT(collection, rkey) DO UPDATE SET
                 cid = excluded.cid, bytes = excluded.bytes, json = excluded.json,
                 seq = excluded.seq, updated_at = excluded.updated_at",
            params![collection, rkey, cid, bytes, json, seq, created_at, now],
        )?;
        // A recreate clears any prior tombstone — the rkey exists again.
        tx.execute(
            "DELETE FROM deletions WHERE collection = ?1 AND rkey = ?2",
            params![collection, rkey],
        )?;
        update_list_etag(&tx, collection)?;
        tx.commit()?;

        Ok(if existing.is_some() {
            Write::Updated { cid, seq }
        } else {
            Write::Created { cid, seq }
        })
    }

    /// Delete a record, writing a tombstone. Returns whether a record existed.
    pub fn delete(&mut self, collection: &str, rkey: &str) -> Result<bool> {
        let now = now_rfc3339()?;
        let tx = self.conn.transaction()?;
        let removed = tx.execute(
            "DELETE FROM records WHERE collection = ?1 AND rkey = ?2",
            params![collection, rkey],
        )?;
        if removed == 0 {
            return Ok(false);
        }
        let seq = next_seq(&tx)?;
        tx.execute(
            "INSERT INTO deletions (collection, rkey, seq, deleted_at)
             VALUES (?1, ?2, ?3, ?4)
             ON CONFLICT(collection, rkey) DO UPDATE SET
                 seq = excluded.seq, deleted_at = excluded.deleted_at",
            params![collection, rkey, seq, now],
        )?;
        update_list_etag(&tx, collection)?;
        tx.commit()?;
        Ok(true)
    }

    /// Fetch one record.
    pub fn get(&self, collection: &str, rkey: &str) -> Result<Option<Stored>> {
        let raw = self
            .conn
            .query_row(
                "SELECT collection, rkey, cid, json, seq, created_at, updated_at
                   FROM records WHERE collection = ?1 AND rkey = ?2",
                params![collection, rkey],
                row_to_raw,
            )
            .optional()?;
        raw.map(StoredRaw::into_stored).transpose()
    }

    /// List a collection, ordered by rkey.
    pub fn list(&self, collection: &str) -> Result<Vec<Stored>> {
        let mut stmt = self.conn.prepare(
            "SELECT collection, rkey, cid, json, seq, created_at, updated_at
               FROM records WHERE collection = ?1 ORDER BY rkey",
        )?;
        let rows = stmt.query_map(params![collection], row_to_raw)?;
        let mut out = Vec::new();
        for r in rows {
            out.push(r?.into_stored()?);
        }
        Ok(out)
    }

    /// The collection's list ETag — a hash of its sorted `(rkey, cid)` pairs.
    /// `None` for an empty or unknown collection.
    pub fn list_etag(&self, collection: &str) -> Result<Option<String>> {
        Ok(self
            .conn
            .query_row(
                "SELECT list_etag FROM collections_meta WHERE collection = ?1",
                params![collection],
                |r| r.get(0),
            )
            .optional()?)
    }

    /// Records and tombstones in `collection` with `seq` greater than `since`
    /// — the incremental-sync payload for `?since=<seq>`.
    pub fn changed_since(
        &self,
        collection: &str,
        since: i64,
    ) -> Result<(Vec<Stored>, Vec<Tombstone>)> {
        let mut rstmt = self.conn.prepare(
            "SELECT collection, rkey, cid, json, seq, created_at, updated_at
               FROM records WHERE collection = ?1 AND seq > ?2 ORDER BY seq",
        )?;
        let mut records = Vec::new();
        for r in rstmt.query_map(params![collection, since], row_to_raw)? {
            records.push(r?.into_stored()?);
        }
        let mut dstmt = self.conn.prepare(
            "SELECT collection, rkey, seq, deleted_at
               FROM deletions WHERE collection = ?1 AND seq > ?2 ORDER BY seq",
        )?;
        let tombstones = dstmt
            .query_map(params![collection, since], |r| {
                Ok(Tombstone {
                    collection: r.get(0)?,
                    rkey: r.get(1)?,
                    seq: r.get(2)?,
                    deleted_at: r.get(3)?,
                })
            })?
            .collect::<rusqlite::Result<Vec<_>>>()?;
        Ok((records, tombstones))
    }

    /// The highest `seq` assigned so far — the cursor a `?since` response
    /// hands back. `0` before any write.
    pub fn current_seq(&self) -> Result<i64> {
        let next: i64 =
            self.conn
                .query_row("SELECT next FROM change_seq WHERE id = 1", [], |r| r.get(0))?;
        Ok(next - 1)
    }
}

struct StoredRaw {
    collection: String,
    rkey: String,
    cid: String,
    json: String,
    seq: i64,
    created_at: String,
    updated_at: String,
}

impl StoredRaw {
    fn into_stored(self) -> Result<Stored> {
        let json = serde_json::from_str(&self.json)
            .map_err(|_| StoreError::CorruptJson(self.collection.clone(), self.rkey.clone()))?;
        Ok(Stored {
            collection: self.collection,
            rkey: self.rkey,
            cid: self.cid,
            json,
            seq: self.seq,
            created_at: self.created_at,
            updated_at: self.updated_at,
        })
    }
}

fn row_to_raw(r: &rusqlite::Row) -> rusqlite::Result<StoredRaw> {
    Ok(StoredRaw {
        collection: r.get(0)?,
        rkey: r.get(1)?,
        cid: r.get(2)?,
        json: r.get(3)?,
        seq: r.get(4)?,
        created_at: r.get(5)?,
        updated_at: r.get(6)?,
    })
}

/// Read the next `change_seq` value and advance the counter. Call inside a
/// write transaction so the read and the increment are atomic.
fn next_seq(tx: &rusqlite::Transaction) -> Result<i64> {
    let seq: i64 = tx.query_row("SELECT next FROM change_seq WHERE id = 1", [], |r| r.get(0))?;
    tx.execute("UPDATE change_seq SET next = next + 1 WHERE id = 1", [])?;
    Ok(seq)
}

/// Recompute and store a collection's list ETag inside the write transaction.
/// The ETag covers `(rkey, cid)` pairs — not bare CIDs — so an rkey moving is
/// detected even if the set of CIDs is unchanged.
fn update_list_etag(tx: &rusqlite::Transaction, collection: &str) -> Result<()> {
    let mut stmt =
        tx.prepare("SELECT rkey, cid FROM records WHERE collection = ?1 ORDER BY rkey")?;
    let mut hasher = Sha256::new();
    let mut any = false;
    for row in stmt.query_map(params![collection], |r| {
        Ok((r.get::<_, String>(0)?, r.get::<_, String>(1)?))
    })? {
        let (rkey, cid) = row?;
        any = true;
        hasher.update(rkey.as_bytes());
        hasher.update([0u8]);
        hasher.update(cid.as_bytes());
        hasher.update([b'\n']);
    }
    drop(stmt);
    if any {
        let etag = hex(hasher.finalize().as_slice());
        tx.execute(
            "INSERT INTO collections_meta (collection, list_etag) VALUES (?1, ?2)
             ON CONFLICT(collection) DO UPDATE SET list_etag = excluded.list_etag",
            params![collection, etag],
        )?;
    } else {
        tx.execute(
            "DELETE FROM collections_meta WHERE collection = ?1",
            params![collection],
        )?;
    }
    Ok(())
}

fn hex(bytes: &[u8]) -> String {
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        s.push_str(&format!("{b:02x}"));
    }
    s
}

fn now_rfc3339() -> Result<String> {
    Ok(OffsetDateTime::now_utc().format(&Rfc3339)?)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn rec(v: serde_json::Value) -> Record {
        Record::new(v.as_object().expect("object").clone())
    }

    #[test]
    fn put_then_get_round_trips() {
        let mut s = Store::open_in_memory().unwrap();
        let w = s.put("posts", "hello", &rec(json!({"body": "hi"}))).unwrap();
        assert!(matches!(w, Write::Created { .. }));
        let got = s.get("posts", "hello").unwrap().unwrap();
        assert_eq!(got.json, json!({"body": "hi"}));
        assert_eq!(got.seq, 1);
    }

    #[test]
    fn identical_content_is_a_noop_and_moves_no_cursor() {
        let mut s = Store::open_in_memory().unwrap();
        s.put("posts", "a", &rec(json!({"body": "x"}))).unwrap();
        let again = s.put("posts", "a", &rec(json!({"body": "x"}))).unwrap();
        assert!(matches!(again, Write::Unchanged { .. }));
        assert_eq!(s.current_seq().unwrap(), 1);
    }

    #[test]
    fn update_bumps_the_seq() {
        let mut s = Store::open_in_memory().unwrap();
        s.put("posts", "a", &rec(json!({"body": "one"}))).unwrap();
        let w = s.put("posts", "a", &rec(json!({"body": "two"}))).unwrap();
        match w {
            Write::Updated { seq, .. } => assert_eq!(seq, 2),
            other => panic!("expected Updated, got {other:?}"),
        }
    }

    #[test]
    fn delete_removes_the_record_and_writes_a_tombstone() {
        let mut s = Store::open_in_memory().unwrap();
        s.put("posts", "a", &rec(json!({"body": "x"}))).unwrap();
        assert!(s.delete("posts", "a").unwrap());
        assert!(s.get("posts", "a").unwrap().is_none());
        let (_, tombs) = s.changed_since("posts", 0).unwrap();
        assert_eq!(tombs.len(), 1);
        assert_eq!(tombs[0].rkey, "a");
        assert!(!s.delete("posts", "a").unwrap(), "second delete is a no-op");
    }

    #[test]
    fn recreate_clears_the_tombstone() {
        let mut s = Store::open_in_memory().unwrap();
        s.put("posts", "a", &rec(json!({"body": "x"}))).unwrap();
        s.delete("posts", "a").unwrap();
        s.put("posts", "a", &rec(json!({"body": "back"}))).unwrap();
        let (recs, tombs) = s.changed_since("posts", 0).unwrap();
        assert_eq!(recs.len(), 1);
        assert!(tombs.is_empty(), "a recreated rkey is no longer deleted");
    }

    #[test]
    fn changed_since_returns_only_newer_writes() {
        let mut s = Store::open_in_memory().unwrap();
        s.put("posts", "a", &rec(json!({"body": "1"}))).unwrap(); // seq 1
        s.put("posts", "b", &rec(json!({"body": "2"}))).unwrap(); // seq 2
        let (recs, _) = s.changed_since("posts", 1).unwrap();
        assert_eq!(recs.len(), 1);
        assert_eq!(recs[0].rkey, "b");
    }

    #[test]
    fn list_is_rkey_ordered_and_etag_tracks_content() {
        let mut s = Store::open_in_memory().unwrap();
        s.put("posts", "b", &rec(json!({"body": "2"}))).unwrap();
        s.put("posts", "a", &rec(json!({"body": "1"}))).unwrap();
        let list = s.list("posts").unwrap();
        assert_eq!(
            list.iter().map(|r| r.rkey.as_str()).collect::<Vec<_>>(),
            ["a", "b"]
        );
        let etag1 = s.list_etag("posts").unwrap().unwrap();
        s.put("posts", "a", &rec(json!({"body": "changed"}))).unwrap();
        let etag2 = s.list_etag("posts").unwrap().unwrap();
        assert_ne!(etag1, etag2, "the list ETag moves when a member CID moves");
    }

    #[test]
    fn collections_are_isolated() {
        let mut s = Store::open_in_memory().unwrap();
        s.put("posts", "a", &rec(json!({"body": "p"}))).unwrap();
        s.put("recipes", "a", &rec(json!({"body": "r"}))).unwrap();
        assert_eq!(s.list("posts").unwrap().len(), 1);
        assert_eq!(s.list("recipes").unwrap().len(), 1);
        assert!(s.list("art").unwrap().is_empty());
    }
}
