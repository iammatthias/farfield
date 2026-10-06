//! Journey timing. A journey starts at a mark (a person's action, or process
//! start) and ends when its result is on screen; the span is logged as a
//! `perf` event in events.jsonl, which `clients/desktop/perf/journeys.sh`
//! turns into medians against the committed baseline.
//!
//! Marks are cheap (an Instant in a small map) and spans are only logged
//! the first time a mark is consumed, so a repaint loop can call `end` freely.

use parking_lot::Mutex;
use std::collections::HashMap;
use std::sync::OnceLock;
use std::time::Instant;

fn marks() -> &'static Mutex<HashMap<String, Instant>> {
    static M: OnceLock<Mutex<HashMap<String, Instant>>> = OnceLock::new();
    M.get_or_init(Default::default)
}

/// When the process started (set first thing in main).
pub fn process_start() -> Instant {
    static T: OnceLock<Instant> = OnceLock::new();
    *T.get_or_init(Instant::now)
}

/// Start (or restart) a journey.
pub fn mark(name: &str) {
    marks().lock().insert(name.to_string(), Instant::now());
}

/// End a journey if it is running: log its duration and forget the mark.
/// Returns the duration in ms.
pub fn end(name: &str) -> Option<f64> {
    let t = marks().lock().remove(name)?;
    let ms = t.elapsed().as_secs_f64() * 1000.0;
    crate::app::log("perf", &[("span", name), ("ms", &format!("{ms:.2}"))]);
    Some(ms)
}

/// Log a span measured from process start (cold-start journeys), once.
pub fn since_start(name: &str) {
    static DONE: OnceLock<Mutex<std::collections::HashSet<String>>> = OnceLock::new();
    if DONE.get_or_init(Default::default).lock().insert(name.to_string()) {
        let ms = process_start().elapsed().as_secs_f64() * 1000.0;
        crate::app::log("perf", &[("span", name), ("ms", &format!("{ms:.2}"))]);
    }
}

/// Log a measured duration directly (for spans timed in place).
pub fn record(name: &str, ms: f64) {
    crate::app::log("perf", &[("span", name), ("ms", &format!("{ms:.3}"))]);
}

/// Log the time since a running mark without ending it (`<mark>@<label>`).
pub fn lap(name: &str, label: &str) {
    if let Some(t) = marks().lock().get(name).copied() {
        let ms = t.elapsed().as_secs_f64() * 1000.0;
        crate::app::log("perf", &[("span", &format!("{name}@{label}")), ("ms", &format!("{ms:.2}"))]);
    }
}
