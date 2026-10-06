//! Three-way merge for a record that changed on the server while it was being
//! edited here.
//!
//! Field by field: a side that did not change a field yields to the side that
//! did. When both changed the same text field, its lines are merged (diff3);
//! overlapping hunks stay as conflict markers for the person to settle. Any
//! `blob://` or `series://` reference the local edit added is guaranteed to
//! survive — an uploaded image must never silently fall out of a document
//! because someone else saved first.

use serde_json::{Map, Value};
use std::collections::BTreeSet;

#[derive(Debug, Clone, PartialEq)]
pub struct Merged {
    pub value: Value,
    /// Fields both sides changed differently. Text fields merged cleanly are
    /// not listed; text with overlapping edits is listed and carries markers.
    pub conflicts: Vec<String>,
    /// Local references that would otherwise have been lost and were
    /// appended to the body.
    pub restored_refs: Vec<String>,
}

/// References a body makes: `blob://<cid>` and `series://<slug>`.
pub fn refs(body: &str) -> BTreeSet<String> {
    let mut out = BTreeSet::new();
    for scheme in ["blob://", "series://"] {
        let mut rest = body;
        while let Some(i) = rest.find(scheme) {
            let tail = &rest[i + scheme.len()..];
            let n = tail.find(|c: char| !(c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-')).unwrap_or(tail.len());
            if n > 0 {
                out.insert(format!("{scheme}{}", &tail[..n]));
            }
            rest = &tail[n..];
        }
    }
    out
}

/// Merge `local` and `remote`, both descended from `base`. `text_field`
/// names the field holding the document (merged line by line).
pub fn merge3(base: &Value, local: &Value, remote: &Value, text_field: &str) -> Merged {
    let empty = Map::new();
    let b = base.as_object().unwrap_or(&empty);
    let l = local.as_object().unwrap_or(&empty);
    let r = remote.as_object().unwrap_or(&empty);
    let mut keys: BTreeSet<&String> = BTreeSet::new();
    keys.extend(l.keys());
    keys.extend(r.keys());
    let mut out = Map::new();
    let mut conflicts = Vec::new();
    for k in keys {
        let (bv, lv, rv) = (b.get(k), l.get(k), r.get(k));
        let pick = if lv == rv || lv == bv {
            rv.or(lv).cloned()
        } else if rv == bv {
            lv.cloned()
        } else if let (Some(Value::String(bs)), Some(Value::String(ls)), Some(Value::String(rs))) =
            (bv.or(Some(&Value::String(String::new()))), lv, rv)
        {
            match diffy::merge(bs, ls, rs) {
                Ok(m) => Some(Value::String(m)),
                Err(m) => {
                    conflicts.push(k.clone());
                    Some(Value::String(m))
                }
            }
        } else {
            // both changed a non-text field: the local edit wins, flagged
            conflicts.push(k.clone());
            lv.cloned()
        };
        if let Some(v) = pick {
            out.insert(k.clone(), v);
        }
    }
    let mut value = Value::Object(out);
    let restored_refs = keep_refs(base, local, &mut value, text_field);
    Merged { value, conflicts, restored_refs }
}

/// Ensure every reference `local` added (relative to `base`) is still in
/// `into`'s text field; append any that are missing as their own lines.
pub fn keep_refs(base: &Value, local: &Value, into: &mut Value, text_field: &str) -> Vec<String> {
    let text = |v: &Value| v.get(text_field).and_then(|s| s.as_str()).unwrap_or("").to_string();
    let added: BTreeSet<String> = refs(&text(local)).difference(&refs(&text(base))).cloned().collect();
    let have = refs(&text(into));
    let missing: Vec<String> = added.into_iter().filter(|r| !have.contains(r)).collect();
    if !missing.is_empty() {
        let mut body = text(into);
        for r in &missing {
            if !body.is_empty() && !body.ends_with('\n') {
                body.push('\n');
            }
            body.push('\n');
            body.push_str(&format!("![]({r})"));
        }
        if let Some(o) = into.as_object_mut() {
            o.insert(text_field.into(), Value::String(body));
        }
    }
    missing
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn disjoint_edits_merge_cleanly() {
        let base = json!({"title": "A", "body": "one\ntwo\nthree\n", "tags": ["x"]});
        let local = json!({"title": "A", "body": "ONE\ntwo\nthree\n", "tags": ["x"]});
        let remote = json!({"title": "B", "body": "one\ntwo\nTHREE\n", "tags": ["x"]});
        let m = merge3(&base, &local, &remote, "body");
        assert_eq!(m.value["title"], "B");
        assert_eq!(m.value["body"], "ONE\ntwo\nTHREE\n");
        assert!(m.conflicts.is_empty());
    }

    #[test]
    fn overlapping_text_keeps_markers_and_is_reported() {
        let base = json!({"body": "line\n"});
        let m = merge3(&base, &json!({"body": "mine\n"}), &json!({"body": "theirs\n"}), "body");
        assert_eq!(m.conflicts, vec!["body"]);
        let b = m.value["body"].as_str().unwrap();
        assert!(b.contains("mine") && b.contains("theirs") && b.contains("<<<<<<<"));
    }

    #[test]
    fn uploaded_refs_survive_taking_theirs() {
        let base = json!({"body": "intro\n"});
        let local = json!({"body": "intro\n\n![](blob://bafkreinew)\n"});
        let mut theirs = json!({"body": "rewritten intro\n"});
        let restored = keep_refs(&base, &local, &mut theirs, "body");
        assert_eq!(restored, vec!["blob://bafkreinew"]);
        assert!(theirs["body"].as_str().unwrap().contains("![](blob://bafkreinew)"));
    }

    #[test]
    fn refs_are_found() {
        let r = refs("a ![x](blob://bafk2) [s](series://spring-walk) blob://");
        assert_eq!(r.into_iter().collect::<Vec<_>>(), vec!["blob://bafk2", "series://spring-walk"]);
    }
}
