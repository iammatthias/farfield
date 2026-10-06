//! Editor timing: the host-side costs behind "open a document" and
//! "keystroke → frame". Opt-in (timing depends on the machine):
//!
//!   cargo test --release -p farfield-editor --test perf -- --ignored --nocapture
//!
//! Prints the median of several runs per operation and fails if any is more
//! than 1.5× its entry in perf-baseline.json (the ratchet: lower the baseline
//! when a change makes something faster, never raise it to make a test pass).

use farfield_editor::{Command, Editor};
use std::time::Instant;

fn median(mut v: Vec<f64>) -> f64 {
    v.sort_by(|a, b| a.partial_cmp(b).unwrap());
    v[v.len() / 2]
}

fn time<F: FnMut()>(runs: usize, mut f: F) -> f64 {
    median(
        (0..runs)
            .map(|_| {
                let t = Instant::now();
                f();
                t.elapsed().as_secs_f64() * 1000.0
            })
            .collect(),
    )
}

fn document() -> String {
    let para = "The receiver held all night, and the carrier never drifted more than a hertz. \
                We logged *every* hour, wrote it up in the morning, and swapped the bulkhead run for LMR-400. ";
    let mut s = String::from("# A long field note\n\n");
    for i in 0..300 {
        s.push_str(para);
        s.push_str("\n\n");
        if i % 25 == 0 {
            s.push_str("## A section heading\n\n- a list item\n- another item\n\n```\ncode block\n```\n\n");
        }
    }
    s
}

#[test]
#[ignore]
fn editor_timings() {
    let (w, h, scale) = (1520u32, 1800u32, 2.0);
    let doc = document();
    let mut results: Vec<(&str, f64)> = Vec::new();

    // first instance pays module compilation; measure it once
    let t = Instant::now();
    let mut e = Editor::new().unwrap();
    results.push(("first-instance-ms", t.elapsed().as_secs_f64() * 1000.0));

    results.push(("new-instance-ms", time(5, || drop(Editor::new().unwrap()))));
    results.push(("load-dictionary-ms", time(5, || e.load_dictionary().unwrap())));

    e.resize(w, h, scale).unwrap();
    e.focus(true).unwrap();
    results.push((
        "set-text-and-render-ms",
        time(5, || {
            e.set_text(&doc).unwrap();
            e.render().unwrap();
        }),
    ));
    e.command(Command::SelectAll).unwrap();
    e.key(farfield_editor::Key::Right, 0).unwrap();
    e.render().unwrap();
    results.push((
        "insert-and-render-ms",
        time(50, || {
            e.insert("x").unwrap();
            e.render().unwrap();
        }),
    ));
    let mut buf = Vec::new();
    results.push(("framebuffer-bgra-full-ms", time(20, || e.framebuffer_bgra(&mut buf).unwrap())));

    let baseline: serde_json::Value =
        serde_json::from_str(include_str!("perf-baseline.json")).expect("perf-baseline.json");
    let mut failed = Vec::new();
    for (k, v) in &results {
        let base = baseline[*k].as_f64();
        println!("{k:28} {v:9.2}   baseline {}", base.map(|b| format!("{b:.2}")).unwrap_or("—".into()));
        if let Some(b) = base {
            if *v > b * 1.5 {
                failed.push(format!("{k}: {v:.2} > 1.5 × {b:.2}"));
            }
        }
    }
    assert!(failed.is_empty(), "slower than baseline: {failed:?}");
}
