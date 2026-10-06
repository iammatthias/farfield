//! Evidence tooling: capture this app's own window to PNG, and replay a
//! scripted session through the real input path (keystrokes go through the
//! same dispatch and platform-input handler a keyboard does; drops and clicks
//! are the same window events the OS delivers).
//!
//!   FARFIELD_SCRIPT=steps.json FARFIELD_EVIDENCE_DIR=out/  farfield-desktop
//!
//! Steps: {"wait":ms} {"key":"cmd-n"} {"type":"text"} {"drop":["/abs/path"]} {"snap":"name"} {"note":"text"}
//! {"until":"event-name","timeout":ms} {"shell":"cmd"} {"quit":true}
//!
//! A script is a development tool; it runs only when FARFIELD_SCRIPT is set.

use crate::app::log;
use gpui::{App, Keystroke, Modifiers, WindowHandle};
use serde::Deserialize;
use std::path::{Path, PathBuf};
use std::time::Duration;

#[derive(Deserialize, Debug)]
#[serde(untagged)]
enum Step {
    Wait { wait: u64 },
    Key { key: String },
    Type { r#type: String },
    Drop { drop: Vec<PathBuf> },
    Snap { snap: String },
    Note { note: String },
    Until { until: String, timeout: Option<u64> },
    Shell { shell: String },
    Quit { quit: bool },
}

/// Capture every on-screen window this process owns into `path` (the first
/// one; there is one). An app may always capture its own windows.
#[cfg(target_os = "macos")]
pub fn capture_own_window(path: &Path) -> Result<(u32, u32), String> {
    use core_foundation::base::{CFType, TCFType};
    use core_foundation::dictionary::CFDictionary;
    use core_foundation::number::CFNumber;
    use core_foundation::string::CFString;
    use core_graphics::geometry::CGRect;
    use core_graphics::window::*;

    let pid = std::process::id() as i64;
    let info = copy_window_info(kCGWindowListOptionOnScreenOnly, kCGNullWindowID).ok_or("no window list")?;
    let mut wid = None;
    for item in info.iter() {
        let dict: CFDictionary<CFString, CFType> = unsafe { CFDictionary::wrap_under_get_rule(*item as _) };
        let owner = dict
            .find(unsafe { CFString::wrap_under_get_rule(kCGWindowOwnerPID) })
            .and_then(|v| v.downcast::<CFNumber>())
            .and_then(|n| n.to_i64());
        if owner == Some(pid) {
            let layer = dict
                .find(CFString::new("kCGWindowLayer"))
                .and_then(|v| v.downcast::<CFNumber>())
                .and_then(|n| n.to_i64());
            if layer == Some(0) {
                wid = dict
                    .find(unsafe { CFString::wrap_under_get_rule(kCGWindowNumber) })
                    .and_then(|v| v.downcast::<CFNumber>())
                    .and_then(|n| n.to_i64());
                break;
            }
        }
    }
    let wid = wid.ok_or("this app has no on-screen window")? as u32;
    let null = unsafe { core_graphics::display::CGRectNull };
    let img = create_image(null as CGRect, kCGWindowListOptionIncludingWindow, wid, kCGWindowImageBoundsIgnoreFraming)
        .ok_or("window capture refused")?;
    let (w, h, bpr) = (img.width() as u32, img.height() as u32, img.bytes_per_row());
    let data = img.data();
    let bytes = data.bytes();
    let mut rgba = Vec::with_capacity((w * h * 4) as usize);
    for y in 0..h as usize {
        let row = &bytes[y * bpr..y * bpr + w as usize * 4];
        for px in row.as_chunks::<4>().0 {
            // BGRA (premultiplied, little-endian) → RGBA
            rgba.extend_from_slice(&[px[2], px[1], px[0], 255]);
        }
    }
    image::save_buffer(path, &rgba, w, h, image::ColorType::Rgba8).map_err(|e| e.to_string())?;
    Ok((w, h))
}

#[cfg(not(target_os = "macos"))]
pub fn capture_own_window(_path: &Path) -> Result<(u32, u32), String> {
    Err("window capture is implemented for macOS".into())
}

fn events_since(start_ms: u64, name: &str) -> bool {
    let path = crate::app::data_dir().join("events.jsonl");
    let Ok(s) = std::fs::read_to_string(path) else { return false };
    s.lines().rev().take(400).any(|l| {
        serde_json::from_str::<serde_json::Value>(l)
            .map(|v| v["event"] == name && v["t"].as_u64().unwrap_or(0) >= start_ms)
            .unwrap_or(false)
    })
}

/// Start the script, if one was given.
pub fn maybe_run<V: 'static>(win: WindowHandle<V>, cx: &mut App) {
    let Ok(path) = std::env::var("FARFIELD_SCRIPT") else { return };
    let out = PathBuf::from(std::env::var("FARFIELD_EVIDENCE_DIR").unwrap_or_else(|_| "evidence".into()));
    let _ = std::fs::create_dir_all(&out);
    let steps: Vec<Step> = match std::fs::read(&path)
        .map_err(|e| e.to_string())
        .and_then(|b| serde_json::from_slice(&b).map_err(|e| e.to_string()))
    {
        Ok(s) => s,
        Err(e) => {
            eprintln!("script {path}: {e}");
            return;
        }
    };
    let any: gpui::AnyWindowHandle = win.into();
    cx.spawn(async move |cx| {
        log("script-start", &[("script", &path)]);
        for (i, step) in steps.into_iter().enumerate() {
            let label = format!("{i}: {step:?}");
            log("script-step", &[("step", &label)]);
            match step {
                Step::Wait { wait } => cx.background_executor().timer(Duration::from_millis(wait)).await,
                Step::Key { key } => {
                    let _ = any.update(cx, |_, w, cx| {
                        if let Ok(k) = Keystroke::parse(&key) {
                            w.dispatch_keystroke(k, cx);
                        }
                    });
                }
                Step::Type { r#type } => {
                    for ch in r#type.chars() {
                        let _ = any.update(cx, |_, w, cx| {
                            let k = if ch == '\n' {
                                Keystroke::parse("enter").unwrap()
                            } else {
                                Keystroke {
                                    modifiers: Modifiers::default(),
                                    key: ch.to_string(),
                                    key_char: Some(ch.to_string()),
                                }
                            };
                            w.dispatch_keystroke(k, cx);
                        });
                        cx.background_executor().timer(Duration::from_millis(12)).await;
                    }
                }
                Step::Drop { drop } => {
                    // delivered to the focused document through the same
                    // FilesDropped path a Finder drop takes
                    let _ = cx.update(|cx| cx.set_global(PendingDrop(drop)));
                    let _ =
                        any.update(cx, |_, w, cx| w.dispatch_action(Box::new(crate::ui::doc_editor::DropPending), cx));
                }
                Step::Snap { snap } => {
                    // let the frame land first
                    let _ = any.update(cx, |_, w, _| w.refresh());
                    cx.background_executor().timer(Duration::from_millis(350)).await;
                    let file = out.join(format!("{snap}.png"));
                    match capture_own_window(&file) {
                        Ok((w, h)) => {
                            log("snap", &[("file", &file.display().to_string()), ("size", &format!("{w}x{h}"))])
                        }
                        Err(e) => log("snap-failed", &[("file", &file.display().to_string()), ("error", &e)]),
                    }
                }
                Step::Note { note } => log("note", &[("text", &note)]),
                Step::Until { until, timeout } => {
                    let start = farfield_core::store::now_ms().saturating_sub(5_000);
                    let deadline = std::time::Instant::now() + Duration::from_millis(timeout.unwrap_or(10_000));
                    while !events_since(start, &until) && std::time::Instant::now() < deadline {
                        cx.background_executor().timer(Duration::from_millis(100)).await;
                    }
                    if !events_since(start, &until) {
                        log("script-timeout", &[("waiting-for", &until)]);
                    }
                }
                Step::Shell { shell } => {
                    let st = std::process::Command::new("sh").arg("-c").arg(&shell).status();
                    log("script-shell", &[("cmd", &shell), ("ok", &format!("{:?}", st.map(|s| s.success())))]);
                }
                Step::Quit { quit } => {
                    if quit {
                        log("script-end", &[]);
                        let _ = cx.update(|cx| cx.quit());
                        return;
                    }
                }
            }
        }
        log("script-end", &[]);
    })
    .detach();
}

/// Files a script hands to the focused document.
pub struct PendingDrop(pub Vec<PathBuf>);
impl gpui::Global for PendingDrop {}
