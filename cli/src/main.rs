//! `farfield` — the CLI.
//!
//! v1 commands: `import` (bulk-load a directory of markdown), `push`
//! (publish individual files), `status`. Each markdown file becomes a record:
//! YAML frontmatter is coerced to the collection's schema types, the markdown
//! body becomes the `body` field, and the record is `PUT` to the service.

use std::path::{Path, PathBuf};

use anyhow::{Context, Result, bail};
use clap::{Parser, Subcommand};
use farfield_schema::{Field, FieldType, Schema, SchemaSet};
use serde_json::{Map, Value};
use time::format_description::well_known::Rfc3339;
use time::{OffsetDateTime, PrimitiveDateTime, format_description};

const DEFAULT_SERVICE: &str = "http://127.0.0.1:8787";
const DEFAULT_SCHEMAS: &str = "schemas/content";

#[derive(Parser)]
#[command(name = "farfield", about = "Farfield content backend CLI", version)]
struct Cli {
    #[command(subcommand)]
    command: Command,
}

#[derive(Subcommand)]
enum Command {
    /// Bulk-import a directory whose subfolders are collections of *.md files.
    Import {
        /// The directory to import (e.g. an obsidian_cms content folder).
        dir: PathBuf,
        #[arg(long, default_value = DEFAULT_SERVICE)]
        service: String,
        #[arg(long, default_value = DEFAULT_SCHEMAS)]
        schemas: PathBuf,
        /// Parse, coerce, and validate every file — but write nothing.
        #[arg(long)]
        dry_run: bool,
    },
    /// Publish one or more markdown files. Collection = the parent folder name.
    Push {
        /// Markdown files to publish.
        files: Vec<PathBuf>,
        #[arg(long, default_value = DEFAULT_SERVICE)]
        service: String,
        #[arg(long, default_value = DEFAULT_SCHEMAS)]
        schemas: PathBuf,
        #[arg(long)]
        dry_run: bool,
    },
    /// Print a service's status.
    Status {
        #[arg(long, default_value = DEFAULT_SERVICE)]
        service: String,
    },
}

fn main() -> Result<()> {
    match Cli::parse().command {
        Command::Import {
            dir,
            service,
            schemas,
            dry_run,
        } => import(&dir, &service, &schemas, dry_run),
        Command::Push {
            files,
            service,
            schemas,
            dry_run,
        } => push(&files, &service, &schemas, dry_run),
        Command::Status { service } => status(&service),
    }
}

// ---------- commands -------------------------------------------------------

fn status(service: &str) -> Result<()> {
    let body: Value = reqwest::blocking::get(format!("{service}/status"))
        .context("reaching the service")?
        .json()
        .context("parsing the response")?;
    println!("{}", serde_json::to_string_pretty(&body)?);
    Ok(())
}

fn import(dir: &Path, service: &str, schema_dir: &Path, dry_run: bool) -> Result<()> {
    let schemas = SchemaSet::load(schema_dir)
        .with_context(|| format!("loading schemas from {}", schema_dir.display()))?;
    let client = reqwest::blocking::Client::new();
    let token = token();
    let mut tally = Tally::default();

    for entry in std::fs::read_dir(dir).with_context(|| format!("reading {}", dir.display()))? {
        let path = entry?.path();
        if !path.is_dir() {
            continue;
        }
        let Some(collection) = path.file_name().and_then(|n| n.to_str()) else {
            continue;
        };
        let Some(schema) = schemas.schema_for(collection) else {
            println!("skip  {collection}/ — not a known collection");
            continue;
        };
        let mut files: Vec<_> = std::fs::read_dir(&path)?
            .filter_map(|e| e.ok().map(|e| e.path()))
            .filter(|p| p.extension().and_then(|e| e.to_str()) == Some("md"))
            .collect();
        files.sort();
        println!("\n{}/  ({} files)", collection, files.len());
        for file in files {
            publish(
                &file, collection, schema, service, &client, &token, dry_run, &mut tally,
            );
        }
    }

    println!("\n{}", tally.summary(dry_run));
    if tally.failed > 0 {
        bail!("{} file(s) failed", tally.failed);
    }
    Ok(())
}

fn push(files: &[PathBuf], service: &str, schema_dir: &Path, dry_run: bool) -> Result<()> {
    if files.is_empty() {
        bail!("no files given");
    }
    let schemas = SchemaSet::load(schema_dir)
        .with_context(|| format!("loading schemas from {}", schema_dir.display()))?;
    let client = reqwest::blocking::Client::new();
    let token = token();
    let mut tally = Tally::default();

    for file in files {
        let collection = file
            .parent()
            .and_then(|p| p.file_name())
            .and_then(|n| n.to_str())
            .context("cannot infer collection from the file's parent folder")?;
        let Some(schema) = schemas.schema_for(collection) else {
            println!("fail  {} — unknown collection `{collection}`", file.display());
            tally.failed += 1;
            continue;
        };
        publish(
            file, collection, schema, service, &client, &token, dry_run, &mut tally,
        );
    }

    println!("\n{}", tally.summary(dry_run));
    if tally.failed > 0 {
        bail!("{} file(s) failed", tally.failed);
    }
    Ok(())
}

// ---------- one file -------------------------------------------------------

#[allow(clippy::too_many_arguments)]
fn publish(
    file: &Path,
    collection: &str,
    schema: &Schema,
    service: &str,
    client: &reqwest::blocking::Client,
    token: &str,
    dry_run: bool,
    tally: &mut Tally,
) {
    let name = file.display();
    match prepare(file, schema) {
        Err(e) => {
            println!("fail  {name} — {e}");
            tally.failed += 1;
        }
        Ok((rkey, record)) => {
            if dry_run {
                println!("ok    {collection}/{rkey} (dry-run)");
                tally.dry += 1;
                return;
            }
            match send(client, service, collection, &rkey, &record, token) {
                Ok(outcome) => {
                    println!("{:<5} {collection}/{rkey}", outcome.label());
                    tally.add(outcome);
                }
                Err(e) => {
                    println!("fail  {collection}/{rkey} — {e}");
                    tally.failed += 1;
                }
            }
        }
    }
}

/// Read a markdown file and build its record: `(rkey, record-json)`.
fn prepare(file: &Path, schema: &Schema) -> Result<(String, Value)> {
    let text = std::fs::read_to_string(file).context("reading the file")?;
    let (frontmatter, body) = split_frontmatter(&text)?;
    let parsed: Value = serde_yml::from_str(frontmatter).context("parsing YAML frontmatter")?;
    let fm = parsed
        .as_object()
        .context("frontmatter is not a YAML mapping")?;

    let record = build_record(schema, fm, body)?;

    // rkey is the `slug`, falling back to the filename stem.
    let rkey = fm
        .get("slug")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
        .or_else(|| {
            file.file_stem()
                .and_then(|s| s.to_str())
                .map(str::to_string)
        })
        .context("no slug and no filename")?;
    if !valid_rkey(&rkey) {
        bail!("slug `{rkey}` is not a valid rkey ([a-z0-9-], 1-64 chars)");
    }

    // Validate locally for an immediate, server-free error.
    farfield_schema::validate_against(schema, &record).map_err(|e| anyhow::anyhow!("{e}"))?;
    Ok((rkey, record))
}

/// Project the frontmatter onto the schema: every declared field, coerced to
/// its declared type; `body` taken from the markdown body. Fields not in the
/// schema are dropped.
fn build_record(schema: &Schema, frontmatter: &Map<String, Value>, body: &str) -> Result<Value> {
    let mut out = Map::new();
    for (name, field) in &schema.properties {
        if name == "body" {
            out.insert("body".to_string(), Value::String(body.to_string()));
            continue;
        }
        if let Some(value) = frontmatter.get(name) {
            out.insert(
                name.clone(),
                coerce(field.ty, field.items.as_deref(), value)
                    .with_context(|| format!("field `{name}`"))?,
            );
        }
    }
    Ok(Value::Object(out))
}

fn coerce(ty: FieldType, items: Option<&Field>, v: &Value) -> Result<Value> {
    Ok(match ty {
        FieldType::String => Value::String(as_string(v)),
        FieldType::Datetime => Value::String(to_rfc3339(&as_string(v))?),
        FieldType::Boolean => match v {
            Value::Bool(b) => Value::Bool(*b),
            Value::String(s) => Value::Bool(matches!(
                s.to_ascii_lowercase().as_str(),
                "true" | "yes" | "on" | "1"
            )),
            other => other.clone(),
        },
        FieldType::Integer | FieldType::Float | FieldType::Object => v.clone(),
        FieldType::Array => {
            let elems = v.as_array().cloned().unwrap_or_default();
            match items {
                Some(item) => Value::Array(
                    elems
                        .iter()
                        .map(|e| coerce(item.ty, item.items.as_deref(), e))
                        .collect::<Result<_>>()?,
                ),
                None => Value::Array(elems),
            }
        }
    })
}

fn as_string(v: &Value) -> String {
    match v {
        Value::String(s) => s.clone(),
        Value::Bool(b) => b.to_string(),
        Value::Number(n) => n.to_string(),
        Value::Null => String::new(),
        other => other.to_string(),
    }
}

/// Normalize a datetime to RFC3339 UTC. Accepts: RFC3339 already; a unix
/// epoch (10-digit seconds or 13-digit millis); or the `YYYY-MM-DD HH:MM[:SS]`
/// shape obsidian_cms frontmatter uses, with an optionally unpadded hour.
fn to_rfc3339(s: &str) -> Result<String> {
    let s = s.trim();
    if OffsetDateTime::parse(s, &Rfc3339).is_ok() {
        return Ok(s.to_string());
    }
    // A bare integer is a unix timestamp — 13 digits = millis, 10 = seconds.
    if (10..=14).contains(&s.len())
        && s.bytes().all(|b| b.is_ascii_digit())
        && let Ok(n) = s.parse::<i128>()
    {
        let nanos = if s.len() >= 13 {
            n * 1_000_000
        } else {
            n * 1_000_000_000
        };
        if let Ok(dt) = OffsetDateTime::from_unix_timestamp_nanos(nanos) {
            return Ok(dt.format(&Rfc3339)?);
        }
    }
    for pattern in [
        "[year]-[month]-[day] [hour]:[minute]:[second]",
        "[year]-[month]-[day] [hour]:[minute]",
        "[year]-[month]-[day] [hour padding:none]:[minute]",
        "[year]-[month]-[day]",
    ] {
        let fd = format_description::parse(pattern)?;
        if let Ok(dt) = PrimitiveDateTime::parse(s, &fd) {
            return Ok(dt.assume_utc().format(&Rfc3339)?);
        }
        // A bare date has no time-of-day — parse as a Date, assume midnight.
        if pattern == "[year]-[month]-[day]"
            && let Ok(date) = time::Date::parse(s, &fd)
        {
            return Ok(date.midnight().assume_utc().format(&Rfc3339)?);
        }
    }
    bail!("unparseable datetime `{s}`")
}

/// Split `---`-delimited YAML frontmatter from the markdown body.
fn split_frontmatter(text: &str) -> Result<(&str, &str)> {
    let text = text.strip_prefix('\u{feff}').unwrap_or(text);
    let after_open = text
        .strip_prefix("---\n")
        .or_else(|| text.strip_prefix("---\r\n"))
        .context("file has no `---` frontmatter block")?;
    let end = after_open
        .find("\n---")
        .context("frontmatter block is not terminated by `---`")?;
    let yaml = &after_open[..end];
    let body = after_open[end + 4..].trim_start_matches(['\r', '\n']);
    Ok((yaml, body))
}

fn valid_rkey(rkey: &str) -> bool {
    (1..=128).contains(&rkey.len())
        && rkey
            .bytes()
            .all(|b| b.is_ascii_lowercase() || b.is_ascii_digit() || b == b'-')
}

// ---------- HTTP -----------------------------------------------------------

enum Outcome {
    Created,
    Updated,
    Unchanged,
}

impl Outcome {
    fn label(&self) -> &'static str {
        match self {
            Outcome::Created => "new",
            Outcome::Updated => "upd",
            Outcome::Unchanged => "same",
        }
    }
}

fn send(
    client: &reqwest::blocking::Client,
    service: &str,
    collection: &str,
    rkey: &str,
    record: &Value,
    token: &str,
) -> Result<Outcome> {
    let url = format!("{service}/records/{collection}/{rkey}");
    let resp = client
        .put(&url)
        .bearer_auth(token)
        .json(record)
        .send()
        .context("the service is unreachable — is it running?")?;
    let status = resp.status();
    let body: Value = resp.json().unwrap_or(Value::Null);
    if !status.is_success() {
        let msg = body
            .get("message")
            .and_then(|m| m.as_str())
            .unwrap_or("unknown error");
        bail!("{} {}", status.as_u16(), msg);
    }
    // `seq` is null when the write was a no-op (unchanged content).
    if body.get("seq").map(Value::is_null).unwrap_or(true) {
        Ok(Outcome::Unchanged)
    } else if status.as_u16() == 201 {
        Ok(Outcome::Created)
    } else {
        Ok(Outcome::Updated)
    }
}

fn token() -> String {
    std::env::var("FARFIELD_TOKEN").unwrap_or_else(|_| {
        eprintln!("warning: FARFIELD_TOKEN unset — using 'dev-token' (local dev only)");
        "dev-token".to_string()
    })
}

#[derive(Default)]
struct Tally {
    created: usize,
    updated: usize,
    unchanged: usize,
    dry: usize,
    failed: usize,
}

impl Tally {
    fn add(&mut self, o: Outcome) {
        match o {
            Outcome::Created => self.created += 1,
            Outcome::Updated => self.updated += 1,
            Outcome::Unchanged => self.unchanged += 1,
        }
    }

    fn summary(&self, dry_run: bool) -> String {
        if dry_run {
            return format!("dry-run: {} ok, {} failed", self.dry, self.failed);
        }
        format!(
            "{} new, {} updated, {} unchanged, {} failed",
            self.created, self.updated, self.unchanged, self.failed
        )
    }
}
