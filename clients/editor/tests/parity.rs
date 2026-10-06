//! Replays lib/editor/testdata/parity against this host and requires the Go
//! host's golden observations exactly: same text, selection, revision, word
//! count and a byte-identical framebuffer.

use farfield_editor::{Command, Editor, Key};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::path::PathBuf;

#[derive(Deserialize)]
struct Script {
    name: String,
    width: u32,
    height: u32,
    scale: f64,
    ops: Vec<Op>,
}

#[derive(Deserialize)]
struct Op {
    op: String,
    #[serde(default)]
    s: String,
    #[serde(default)]
    k: i32,
    #[serde(default)]
    m: i32,
    #[serde(default)]
    c: i32,
    #[serde(default)]
    kind: i32,
    #[serde(default)]
    x: i32,
    #[serde(default)]
    y: i32,
    #[serde(default)]
    clicks: i32,
}

#[derive(Deserialize, Debug, PartialEq)]
#[serde(rename_all = "camelCase")]
struct Check {
    text: String,
    sel_start: u32,
    sel_end: u32,
    revision: u32,
    words: u32,
    frame: String,
}

fn key(k: i32) -> Key {
    use Key::*;
    [Left, Right, Up, Down, Home, End, PageUp, PageDown, Backspace, Delete, Enter, Tab, Escape][(k - 1) as usize]
}

fn command(c: i32) -> Command {
    use Command::*;
    [
        Bold, Italic, Code, Link, Strike, H1, H2, H3, Quote, Bullets, Numbers, CodeBlock, Undo, Redo, SelectAll, Rule,
        SelectWord, SelectLine,
    ][(c - 1) as usize]
}

#[test]
fn parity_with_go_host() {
    let dir = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../lib/editor/testdata/parity");
    let mut n = 0;
    for entry in std::fs::read_dir(&dir).unwrap() {
        let path = entry.unwrap().path();
        let name = path.file_name().unwrap().to_string_lossy().to_string();
        if !name.ends_with(".json") || name.ends_with(".golden.json") {
            continue;
        }
        n += 1;
        let script: Script = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        let golden: Vec<Check> =
            serde_json::from_slice(&std::fs::read(path.with_extension("golden.json")).unwrap()).unwrap();

        let mut e = Editor::new().unwrap();
        e.resize(script.width, script.height, script.scale).unwrap();
        e.focus(true).unwrap();
        let mut got = Vec::new();
        for op in &script.ops {
            match op.op.as_str() {
                "set_text" => e.set_text(&op.s).unwrap(),
                "insert" => e.insert(&op.s).unwrap(),
                "key" => {
                    e.key(key(op.k), op.m).unwrap();
                }
                "command" => e.command(command(op.c)).unwrap(),
                "pointer" => e.pointer(op.kind, op.x, op.y, op.m, op.clicks).unwrap(),
                "check" => {
                    e.render().unwrap();
                    let fb = e.framebuffer().unwrap();
                    let (sel_start, sel_end) = e.selection_range().unwrap();
                    got.push(Check {
                        text: e.text().unwrap(),
                        sel_start,
                        sel_end,
                        revision: e.revision().unwrap(),
                        words: e.words().unwrap(),
                        frame: hex::encode(Sha256::digest(&fb)),
                    });
                }
                other => panic!("unknown op {other}"),
            }
        }
        assert_eq!(got.len(), golden.len(), "{}: check count", script.name);
        for (i, (g, w)) in got.iter().zip(&golden).enumerate() {
            assert_eq!(g, w, "{} check {i}", script.name);
        }
    }
    assert!(n >= 3, "expected the parity scripts, found {n}");
}
