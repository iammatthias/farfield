//! `farfield-schema` — the lexicon-lite schema system.
//!
//! A schema is one JSON file: `{ id, version, type: "object", required,
//! properties }`. A `collections.json` manifest in the same directory maps
//! each collection name to a schema id plus display metadata. The server and
//! the CLI's codegen share this one loader, so frontend types and backend
//! validation cannot drift.
//!
//! Validation is a small hand-rolled check over the lexicon-lite subset —
//! deliberately not a full JSON Schema engine.

use std::collections::BTreeMap;
use std::path::Path;

use serde::{Deserialize, Serialize};
use serde_json::Value;

/// Errors from loading a schema set.
#[derive(Debug, thiserror::Error)]
pub enum SchemaError {
    /// A schema directory or file could not be read.
    #[error("reading {0}: {1}")]
    Io(String, std::io::Error),
    /// A schema or manifest file was not valid JSON.
    #[error("parsing {0}: {1}")]
    Parse(String, serde_json::Error),
    /// Two schema files declared the same `id`.
    #[error("duplicate schema id `{0}`")]
    DuplicateId(String),
    /// A collection references a schema id that no file defines.
    #[error("collection `{0}` references unknown schema `{1}`")]
    UnknownSchema(String, String),
}

/// A lexicon-lite field type.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum FieldType {
    String,
    Integer,
    Float,
    Boolean,
    /// An RFC3339 timestamp, carried as a string.
    Datetime,
    Array,
    Object,
}

/// One field in a schema's `properties`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Field {
    #[serde(rename = "type")]
    pub ty: FieldType,
    #[serde(default)]
    pub description: String,
    /// The element type, when `ty` is [`FieldType::Array`].
    #[serde(default)]
    pub items: Option<Box<Field>>,
}

/// A content-type schema.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Schema {
    pub id: String,
    #[serde(default = "one")]
    pub version: u32,
    #[serde(default)]
    pub description: String,
    #[serde(default)]
    pub required: Vec<String>,
    pub properties: BTreeMap<String, Field>,
}

fn one() -> u32 {
    1
}

/// A collection's definition: its name, the schema it uses, display metadata.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CollectionDef {
    pub name: String,
    pub schema: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub description: String,
}

#[derive(Deserialize)]
struct Manifest {
    collections: Vec<CollectionDef>,
}

/// A loaded set of schemas plus the collection manifest — everything one app
/// service needs to know which collections exist and how to validate them.
#[derive(Debug, Clone)]
pub struct SchemaSet {
    schemas: BTreeMap<String, Schema>,
    collections: Vec<CollectionDef>,
}

impl SchemaSet {
    /// Load every `*.json` in `dir`. `collections.json` is the manifest;
    /// every other file is a schema.
    pub fn load(dir: impl AsRef<Path>) -> Result<Self, SchemaError> {
        let dir = dir.as_ref();
        let mut schemas: BTreeMap<String, Schema> = BTreeMap::new();
        let mut manifest: Option<Manifest> = None;

        let entries = std::fs::read_dir(dir)
            .map_err(|e| SchemaError::Io(dir.display().to_string(), e))?;
        for entry in entries {
            let path = entry
                .map_err(|e| SchemaError::Io(dir.display().to_string(), e))?
                .path();
            if path.extension().and_then(|e| e.to_str()) != Some("json") {
                continue;
            }
            let name = path.display().to_string();
            let text =
                std::fs::read_to_string(&path).map_err(|e| SchemaError::Io(name.clone(), e))?;
            if path.file_name().and_then(|n| n.to_str()) == Some("collections.json") {
                manifest = Some(
                    serde_json::from_str(&text).map_err(|e| SchemaError::Parse(name, e))?,
                );
            } else {
                let schema: Schema =
                    serde_json::from_str(&text).map_err(|e| SchemaError::Parse(name, e))?;
                if schemas.insert(schema.id.clone(), schema.clone()).is_some() {
                    return Err(SchemaError::DuplicateId(schema.id));
                }
            }
        }

        let collections = manifest.map(|m| m.collections).unwrap_or_default();
        for c in &collections {
            if !schemas.contains_key(&c.schema) {
                return Err(SchemaError::UnknownSchema(c.name.clone(), c.schema.clone()));
            }
        }
        Ok(Self {
            schemas,
            collections,
        })
    }

    /// Every collection in the manifest.
    pub fn collections(&self) -> &[CollectionDef] {
        &self.collections
    }

    /// The definition of one collection, if it exists.
    pub fn collection(&self, name: &str) -> Option<&CollectionDef> {
        self.collections.iter().find(|c| c.name == name)
    }

    /// The schema backing a collection, if the collection exists.
    pub fn schema_for(&self, collection: &str) -> Option<&Schema> {
        self.collection(collection)
            .and_then(|c| self.schemas.get(&c.schema))
    }

    /// Validate a record against its collection's schema.
    pub fn validate(&self, collection: &str, record: &Value) -> Result<(), ValidationError> {
        let schema = self
            .schema_for(collection)
            .ok_or_else(|| ValidationError::unknown_collection(collection))?;
        validate_against(schema, record)
    }
}

/// A record that failed validation. Carries one issue per problem found.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ValidationError {
    pub issues: Vec<String>,
}

impl ValidationError {
    fn unknown_collection(name: &str) -> Self {
        Self {
            issues: vec![format!("unknown collection `{name}`")],
        }
    }
}

impl std::fmt::Display for ValidationError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "invalid record: {}", self.issues.join("; "))
    }
}

impl std::error::Error for ValidationError {}

/// Validate `record` against `schema`: required fields present, no unknown
/// fields, every field the declared type.
pub fn validate_against(schema: &Schema, record: &Value) -> Result<(), ValidationError> {
    let mut issues = Vec::new();
    let Some(obj) = record.as_object() else {
        return Err(ValidationError {
            issues: vec!["record is not a JSON object".into()],
        });
    };

    for req in &schema.required {
        if !obj.contains_key(req) {
            issues.push(format!("missing required field `{req}`"));
        }
    }
    for (key, value) in obj {
        match schema.properties.get(key) {
            None => issues.push(format!("unknown field `{key}` (not in schema)")),
            Some(field) => check_field(key, field, value, &mut issues),
        }
    }

    if issues.is_empty() {
        Ok(())
    } else {
        Err(ValidationError { issues })
    }
}

fn check_field(path: &str, field: &Field, value: &Value, issues: &mut Vec<String>) {
    let ok = match field.ty {
        FieldType::String | FieldType::Datetime => value.is_string(),
        FieldType::Boolean => value.is_boolean(),
        FieldType::Integer => value.is_i64() || value.is_u64(),
        FieldType::Float => value.is_number(),
        FieldType::Object => value.is_object(),
        FieldType::Array => value.is_array(),
    };
    if !ok {
        issues.push(format!(
            "field `{path}` should be {:?}, got {}",
            field.ty,
            kind_of(value)
        ));
        return;
    }
    if field.ty == FieldType::Array {
        let items = field.items.as_deref();
        if let (Some(item_schema), Some(arr)) = (items, value.as_array()) {
            for (i, elem) in arr.iter().enumerate() {
                check_field(&format!("{path}[{i}]"), item_schema, elem, issues);
            }
        }
    }
}

fn kind_of(v: &Value) -> &'static str {
    match v {
        Value::Null => "null",
        Value::Bool(_) => "boolean",
        Value::Number(_) => "number",
        Value::String(_) => "string",
        Value::Array(_) => "array",
        Value::Object(_) => "object",
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn entry_schema() -> Schema {
        serde_json::from_value(json!({
            "id": "entry",
            "required": ["title", "tags", "body"],
            "properties": {
                "title": { "type": "string" },
                "tags":  { "type": "array", "items": { "type": "string" } },
                "excerpt": { "type": "string" },
                "body":  { "type": "string" }
            }
        }))
        .unwrap()
    }

    #[test]
    fn a_valid_record_passes() {
        let s = entry_schema();
        let r = json!({ "title": "x", "tags": ["a", "b"], "body": "hi" });
        assert!(validate_against(&s, &r).is_ok());
    }

    #[test]
    fn an_optional_field_may_be_absent() {
        let s = entry_schema();
        let r = json!({ "title": "x", "tags": [], "body": "hi" });
        assert!(validate_against(&s, &r).is_ok(), "excerpt is optional");
    }

    #[test]
    fn a_missing_required_field_is_caught_by_name() {
        let s = entry_schema();
        let err = validate_against(&s, &json!({ "tags": [], "body": "hi" })).unwrap_err();
        assert!(err.issues.iter().any(|i| i.contains("`title`")));
    }

    #[test]
    fn a_wrong_type_is_caught() {
        let s = entry_schema();
        let err = validate_against(&s, &json!({ "title": 7, "tags": [], "body": "hi" }))
            .unwrap_err();
        assert!(err.issues.iter().any(|i| i.contains("`title`")));
    }

    #[test]
    fn a_wrong_array_element_type_is_caught() {
        let s = entry_schema();
        let err = validate_against(&s, &json!({ "title": "x", "tags": ["ok", 9], "body": "h" }))
            .unwrap_err();
        assert!(err.issues.iter().any(|i| i.contains("tags[1]")));
    }

    #[test]
    fn an_unknown_field_is_rejected() {
        let s = entry_schema();
        let err = validate_against(
            &s,
            &json!({ "title": "x", "tags": [], "body": "h", "surprise": 1 }),
        )
        .unwrap_err();
        assert!(err.issues.iter().any(|i| i.contains("`surprise`")));
    }

    #[test]
    fn the_real_content_schemas_load_and_validate() {
        // Integration check against the repo's actual schema files.
        let dir = concat!(env!("CARGO_MANIFEST_DIR"), "/../../schemas/content");
        let set = SchemaSet::load(dir).expect("real content schemas load");
        assert_eq!(set.collections().len(), 7);
        assert!(set.schema_for("posts").is_some());
        assert!(set.schema_for("melange").is_some());
        assert!(set.schema_for("media").is_some());

        // A record shaped like real obsidian_cms frontmatter + body.
        let record = json!({
            "title": "Sourdough",
            "slug": "1587970800000-sourdough",
            "published": true,
            "created": "2023-11-02T12:14:00Z",
            "updated": "2025-05-24T16:58:00Z",
            "tags": ["sourdough", "bread"],
            "excerpt": "Growing up near San Francisco...",
            "body": "# Sourdough\n\n![](ipfs://bafy...)"
        });
        set.validate("recipes", &record).expect("real-shaped record validates");
    }
}
