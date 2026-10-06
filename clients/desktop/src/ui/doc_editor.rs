//! The document editor: farfield's own editor.wasm, hosted in a GPUI element.
//!
//! This is the native counterpart of lib/editor/host.js and apps/desk, and
//! like them it is only plumbing: the module owns the document, Markdown
//! styling, layout, selection, undo and every pixel; the host moves input in
//! and the framebuffer out. Platform text input (IME, dictation) goes through
//! GPUI's input handler. Composition ("marked") text is held here and drawn
//! over the caret until the input method commits it — exactly as the
//! browser's hidden textarea holds it — so the editor's undo history only
//! ever sees committed text.

use crate::theme::theme;
use farfield_core::Session;
use farfield_editor::{mods, pointer, Command, Editor, Key};
use gpui::{
    actions, div, point, prelude::*, px, size, App, Bounds, ClipboardItem, Context, Corners, ElementId,
    ElementInputHandler, Entity, EntityInputHandler, EventEmitter, ExternalPaths, FocusHandle, Focusable,
    GlobalElementId, KeyBinding, LayoutId, MouseButton, MouseDownEvent, MouseMoveEvent, MouseUpEvent, Pixels, Point,
    RenderImage, ScrollWheelEvent, Style, Task, UTF16Selection, Window,
};
use std::ops::Range;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::{Duration, Instant};

actions!(
    doc,
    [
        KLeft,
        KRight,
        KUp,
        KDown,
        KHome,
        KEnd,
        KPageUp,
        KPageDown,
        KBackspace,
        KDelete,
        KEnter,
        KTab,
        KEscape,
        SLeft,
        SRight,
        SUp,
        SDown,
        SHome,
        SEnd,
        SPageUp,
        SPageDown,
        STab,
        WLeft,
        WRight,
        WSLeft,
        WSRight,
        WBackspace,
        WDelete,
        CLeft,
        CRight,
        CUp,
        CDown,
        CSLeft,
        CSRight,
        CSUp,
        CSDown,
        CBackspace,
        Bold,
        Italic,
        Code,
        Link,
        Strike,
        H1,
        H2,
        H3,
        Quote,
        Bullets,
        Numbers,
        CodeBlock,
        Undo,
        Redo,
        SelectAll,
        Rule,
        Copy,
        Cut,
        Paste,
        CharPalette,
        DropPending,
    ]
);

pub fn bind_keys(cx: &mut App) {
    let c = Some("DocEditor");
    cx.bind_keys([
        KeyBinding::new("left", KLeft, c),
        KeyBinding::new("right", KRight, c),
        KeyBinding::new("up", KUp, c),
        KeyBinding::new("down", KDown, c),
        KeyBinding::new("home", KHome, c),
        KeyBinding::new("end", KEnd, c),
        KeyBinding::new("pageup", KPageUp, c),
        KeyBinding::new("pagedown", KPageDown, c),
        KeyBinding::new("backspace", KBackspace, c),
        KeyBinding::new("delete", KDelete, c),
        KeyBinding::new("enter", KEnter, c),
        KeyBinding::new("tab", KTab, c),
        KeyBinding::new("escape", KEscape, c),
        KeyBinding::new("shift-left", SLeft, c),
        KeyBinding::new("shift-right", SRight, c),
        KeyBinding::new("shift-up", SUp, c),
        KeyBinding::new("shift-down", SDown, c),
        KeyBinding::new("shift-home", SHome, c),
        KeyBinding::new("shift-end", SEnd, c),
        KeyBinding::new("shift-pageup", SPageUp, c),
        KeyBinding::new("shift-pagedown", SPageDown, c),
        KeyBinding::new("shift-tab", STab, c),
        KeyBinding::new("alt-left", WLeft, c),
        KeyBinding::new("alt-right", WRight, c),
        KeyBinding::new("alt-shift-left", WSLeft, c),
        KeyBinding::new("alt-shift-right", WSRight, c),
        KeyBinding::new("alt-backspace", WBackspace, c),
        KeyBinding::new("alt-delete", WDelete, c),
        KeyBinding::new("cmd-left", CLeft, c),
        KeyBinding::new("cmd-right", CRight, c),
        KeyBinding::new("cmd-up", CUp, c),
        KeyBinding::new("cmd-down", CDown, c),
        KeyBinding::new("cmd-shift-left", CSLeft, c),
        KeyBinding::new("cmd-shift-right", CSRight, c),
        KeyBinding::new("cmd-shift-up", CSUp, c),
        KeyBinding::new("cmd-shift-down", CSDown, c),
        KeyBinding::new("cmd-backspace", CBackspace, c),
        KeyBinding::new("cmd-b", Bold, c),
        KeyBinding::new("cmd-i", Italic, c),
        KeyBinding::new("cmd-e", Code, c),
        KeyBinding::new("cmd-k", Link, c),
        KeyBinding::new("cmd-shift-x", Strike, c),
        KeyBinding::new("cmd-alt-1", H1, c),
        KeyBinding::new("cmd-alt-2", H2, c),
        KeyBinding::new("cmd-alt-3", H3, c),
        KeyBinding::new("cmd-shift-9", Quote, c),
        KeyBinding::new("cmd-shift-8", Bullets, c),
        KeyBinding::new("cmd-shift-7", Numbers, c),
        KeyBinding::new("cmd-alt-c", CodeBlock, c),
        KeyBinding::new("cmd-z", Undo, c),
        KeyBinding::new("cmd-shift-z", Redo, c),
        KeyBinding::new("cmd-y", Redo, c),
        KeyBinding::new("cmd-a", SelectAll, c),
        KeyBinding::new("cmd-alt-r", Rule, c),
        KeyBinding::new("cmd-c", Copy, c),
        KeyBinding::new("cmd-x", Cut, c),
        KeyBinding::new("cmd-v", Paste, c),
        KeyBinding::new("ctrl-cmd-space", CharPalette, c),
    ]);
}

#[derive(Clone, Debug)]
pub enum DocEvent {
    /// The text changed (a new revision).
    Changed,
    /// Files were dropped on the document; the owner uploads them and calls
    /// `insert` with the Markdown references.
    FilesDropped(Vec<PathBuf>),
    Blur,
}

pub struct DocEditor {
    ed: Option<Editor>,
    pub error: Option<String>,
    focus: FocusHandle,
    image: Option<Arc<RenderImage>>,
    retired: Vec<Arc<RenderImage>>,
    buf: Vec<u8>,
    surface: (u32, u32, f32),
    bounds: Option<Bounds<Pixels>>,
    marked: Option<String>,
    /// Where the composition sits, in UTF-16 units of the committed text.
    marked16: usize,
    rev: u32,
    started: Instant,
    blink: Option<Task<()>>,
    session: Option<Arc<Session>>,
    requested: std::collections::HashSet<String>,
    down: bool,
    plain: bool,
    /// When the last unpainted input arrived (keystroke → frame timing).
    input_at: Option<Instant>,
    /// The document text at a revision: the platform input system asks for
    /// it several times per keystroke, and each read copies the whole
    /// document out of the module.
    text_cache: Option<(u32, String)>,
}

impl EventEmitter<DocEvent> for DocEditor {}

impl DocEditor {
    pub fn new(
        w: &mut Window,
        cx: &mut Context<Self>,
        text: &str,
        placeholder: &str,
        session: Option<Arc<Session>>,
    ) -> Self {
        let focus = cx.focus_handle();
        cx.on_focus(&focus, w, |this: &mut Self, _w, cx| {
            this.with(|e| e.focus(true));
            this.start_blink(cx);
            cx.notify();
        })
        .detach();
        cx.on_blur(&focus, w, |this: &mut Self, _w, cx| {
            this.commit_marked();
            this.with(|e| e.focus(false));
            this.blink = None;
            cx.emit(DocEvent::Blur);
            cx.notify();
        })
        .detach();
        let (ed, error) = match Editor::new() {
            Ok(mut e) => {
                let _ = e.load_dictionary();
                let _ = e.set_placeholder(placeholder);
                let _ = e.set_text(text);
                (Some(e), None)
            }
            Err(e) => (None, Some(format!("The editor could not start: {e}"))),
        };
        let mut this = DocEditor {
            ed,
            error,
            focus,
            image: None,
            retired: Vec::new(),
            buf: Vec::new(),
            surface: (0, 0, 0.0),
            bounds: None,
            marked: None,
            marked16: 0,
            rev: 0,
            started: Instant::now(),
            blink: None,
            session,
            requested: Default::default(),
            down: false,
            plain: false,
            input_at: None,
            text_cache: None,
        };
        this.rev = this.with(|e| e.revision()).unwrap_or(0);
        this.apply_palette(cx);
        this.load_images(text, cx);
        cx.observe_global::<crate::theme::Theme>(|this, cx| {
            this.apply_palette(cx);
            cx.notify();
        })
        .detach();
        this
    }

    /// Plain-text mode: no Markdown styling (code, pastes).
    pub fn set_plain(&mut self, on: bool, cx: &mut Context<Self>) {
        self.plain = on;
        self.with(|e| e.set_plain(on));
        cx.notify();
    }

    fn with<T>(&mut self, f: impl FnOnce(&mut Editor) -> anyhow::Result<T>) -> Option<T> {
        let e = self.ed.as_mut()?;
        match f(e) {
            Ok(v) => Some(v),
            Err(err) => {
                // a trap in the module is a bug in the editor; keep the
                // document text (the owner's draft is already on disk) and
                // stop driving a broken instance
                self.error = Some(format!("The editor stopped: {err}"));
                self.ed = None;
                None
            }
        }
    }

    fn apply_palette(&mut self, cx: &App) {
        let p = theme(cx).editor_palette();
        self.with(|e| e.set_palette(&p));
    }

    pub fn text(&mut self) -> String {
        let rev = self.with(|e| e.revision()).unwrap_or(u32::MAX);
        if let Some((r, t)) = &self.text_cache {
            if *r == rev {
                return t.clone();
            }
        }
        let t = self.with(|e| e.text()).unwrap_or_default();
        self.text_cache = Some((rev, t.clone()));
        t
    }

    pub fn words(&mut self) -> u32 {
        self.with(|e| e.words()).unwrap_or(0)
    }

    /// Replace the whole document (loading a record, taking a resolution).
    pub fn set_text(&mut self, s: &str, cx: &mut Context<Self>) {
        // a replaced document may restart the revision count: never trust the
        // cached text across it
        self.text_cache = None;
        self.marked = None;
        self.with(|e| e.set_text(s));
        self.rev = self.with(|e| e.revision()).unwrap_or(self.rev);
        self.load_images(s, cx);
        cx.notify();
    }

    /// Insert at the caret, as typing would (one undo step).
    pub fn insert(&mut self, s: &str, cx: &mut Context<Self>) {
        self.commit_marked();
        self.with(|e| e.insert(s));
        self.after(cx);
    }

    pub fn focus_editor(&self, w: &mut Window) {
        w.focus(&self.focus);
    }

    fn after(&mut self, cx: &mut Context<Self>) {
        self.input_at.get_or_insert_with(Instant::now);
        let r = self.with(|e| e.revision()).unwrap_or(self.rev);
        if r != self.rev {
            self.rev = r;
            let t = self.text();
            self.load_images(&t, cx);
            cx.emit(DocEvent::Changed);
        }
        cx.notify();
    }

    fn start_blink(&mut self, cx: &mut Context<Self>) {
        if theme(cx).reduced_motion {
            // no blinking caret: the editor's caret stays drawn
            return;
        }
        let started = self.started;
        self.blink = Some(cx.spawn(async move |this, cx| loop {
            cx.background_executor().timer(Duration::from_millis(530)).await;
            let ok = this.update(cx, |this, cx| {
                this.with(|e| e.tick(started.elapsed().as_millis() as u32));
                cx.notify();
            });
            if ok.is_err() {
                break;
            }
        }));
    }

    fn key(&mut self, k: Key, m: i32, cx: &mut Context<Self>) {
        self.commit_marked();
        self.with(|e| e.key(k, m));
        self.after(cx);
    }

    fn cmd(&mut self, c: Command, cx: &mut Context<Self>) {
        self.commit_marked();
        self.with(|e| e.command(c));
        self.after(cx);
    }

    fn commit_marked(&mut self) {
        if let Some(m) = self.marked.take() {
            if !m.is_empty() {
                self.with(|e| e.insert(&m));
            }
        }
    }

    /// Fetch and hand over images for `![](blob://…)` and https lines —
    /// display only; the Markdown is never rewritten.
    fn load_images(&mut self, text: &str, cx: &mut Context<Self>) {
        let Some(session) = self.session.clone() else { return };
        for url in farfield_editor::image_refs(text) {
            if !self.requested.insert(url.clone()) {
                continue;
            }
            let session = session.clone();
            let u = url.clone();
            let col = if self.surface.0 > 100 { self.surface.0 } else { 1520 };
            let fetch = farfield_core::spawn(async move {
                // at most four image fetches+decodes at once, app-wide
                static LIMIT: std::sync::OnceLock<tokio::sync::Semaphore> = std::sync::OnceLock::new();
                let _permit = LIMIT.get_or_init(|| tokio::sync::Semaphore::new(4)).acquire().await.ok()?;
                // remote http(s) images are not fetched by the native host
                let cid = u.strip_prefix("blob://")?;
                let bytes = farfield_core::api::blobs::bytes(&session, cid, 64 << 20).await.ok()?;
                let img = image::load_from_memory(&bytes).ok()?;
                let w = img.width().min(col);
                let h = ((img.height() as u64 * w as u64) / img.width().max(1) as u64) as u32;
                let h = h.min(1400);
                let img = img.resize(w, h.max(1), image::imageops::FilterType::Triangle).to_rgba8();
                Some((img.width(), img.height(), img.into_raw()))
            });
            cx.spawn(async move |this, cx| {
                if let Ok(Some((w, h, px))) = fetch.await {
                    let _ = this.update(cx, |this, cx| {
                        this.with(|e| e.put_image(&url, w, h, &px));
                        cx.notify();
                    });
                }
            })
            .detach();
        }
    }

    fn device(&self, p: Point<Pixels>) -> (i32, i32) {
        let Some(b) = self.bounds else { return (0, 0) };
        let s = self.surface.2.max(1.0);
        (f32::from((p.x - b.left()) * s) as i32, f32::from((p.y - b.top()) * s) as i32)
    }

    fn mods_of(m: &gpui::Modifiers) -> i32 {
        let mut out = 0;
        if m.shift {
            out |= mods::SHIFT;
        }
        if m.alt {
            out |= mods::WORD;
        }
        if m.platform {
            out |= mods::CMD;
        }
        out
    }

    fn on_down(&mut self, e: &MouseDownEvent, w: &mut Window, cx: &mut Context<Self>) {
        w.focus(&self.focus);
        self.commit_marked();
        let (x, y) = self.device(e.position);
        if e.modifiers.platform {
            if let Some(link) = self.with(|ed| ed.link_at(x, y)).filter(|l| !l.is_empty()) {
                if link.starts_with("https://") || link.starts_with("http://") {
                    cx.open_url(&link);
                }
                return;
            }
        }
        self.down = true;
        self.with(|ed| ed.pointer(pointer::DOWN, x, y, Self::mods_of(&e.modifiers), e.click_count as i32));
        self.after(cx);
    }
    fn on_move(&mut self, e: &MouseMoveEvent, _: &mut Window, cx: &mut Context<Self>) {
        if self.down {
            let (x, y) = self.device(e.position);
            self.with(|ed| ed.pointer(pointer::MOVE, x, y, Self::mods_of(&e.modifiers), 0));
            cx.notify();
        }
    }
    fn on_up(&mut self, _: &MouseUpEvent, _: &mut Window, cx: &mut Context<Self>) {
        if self.down {
            self.down = false;
            self.with(|ed| ed.pointer(pointer::UP, 0, 0, 0, 0));
            cx.notify();
        }
    }
    fn on_wheel(&mut self, e: &ScrollWheelEvent, _: &mut Window, cx: &mut Context<Self>) {
        let d = e.delta.pixel_delta(px(20.));
        let dy = (-f32::from(d.y) * self.surface.2.max(1.0)) as i32;
        if dy != 0 {
            self.with(|ed| ed.wheel(dy));
            cx.notify();
        }
    }

    fn copy(&mut self, _: &Copy, _: &mut Window, cx: &mut Context<Self>) {
        if let Some(s) = self.with(|e| e.selection()).filter(|s| !s.is_empty()) {
            cx.write_to_clipboard(ClipboardItem::new_string(s));
        }
    }
    fn cut(&mut self, _: &Cut, _: &mut Window, cx: &mut Context<Self>) {
        if let Some(s) = self.with(|e| e.selection()).filter(|s| !s.is_empty()) {
            cx.write_to_clipboard(ClipboardItem::new_string(s));
            self.key(Key::Backspace, 0, cx);
        }
    }
    fn paste(&mut self, _: &Paste, _: &mut Window, cx: &mut Context<Self>) {
        if let Some(t) = cx.read_from_clipboard().and_then(|i| i.text()) {
            self.insert(&t.replace("\r\n", "\n"), cx);
        }
    }

    // ── the text the platform input system sees (UTF-16 offsets) ──

    /// The document with any composition spliced in at the caret, and the
    /// caret's byte offset.
    fn virtual_text(&mut self) -> (String, usize) {
        let t = self.text();
        let (a, _) = self.with(|e| e.selection_range()).unwrap_or((0, 0));
        let a = (a as usize).min(t.len());
        match &self.marked {
            Some(m) => (format!("{}{}{}", &t[..a], m, &t[a..]), a),
            None => (t, a),
        }
    }
}

impl EntityInputHandler for DocEditor {
    fn text_for_range(
        &mut self,
        r16: Range<usize>,
        actual: &mut Option<Range<usize>>,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<String> {
        let (t, _) = self.virtual_text();
        let r = farfield_editor::utf16_to_utf8(&t, r16.start)..farfield_editor::utf16_to_utf8(&t, r16.end);
        actual.replace(farfield_editor::utf8_to_utf16(&t, r.start)..farfield_editor::utf8_to_utf16(&t, r.end));
        Some(t[r].to_string())
    }
    fn selected_text_range(&mut self, _: bool, _: &mut Window, _: &mut Context<Self>) -> Option<UTF16Selection> {
        let t = self.text();
        let (a, b) = self.with(|e| e.selection_range())?;
        let (a, b) = (a as usize, b as usize);
        let base = farfield_editor::utf8_to_utf16(&t, a);
        if let Some(m) = &self.marked {
            let n = m.encode_utf16().count();
            return Some(UTF16Selection { range: base + n..base + n, reversed: false });
        }
        Some(UTF16Selection { range: base..farfield_editor::utf8_to_utf16(&t, b), reversed: false })
    }
    fn marked_text_range(&self, _: &mut Window, _: &mut Context<Self>) -> Option<Range<usize>> {
        let m = self.marked.as_ref()?;
        Some(self.marked16..self.marked16 + m.encode_utf16().count())
    }
    fn unmark_text(&mut self, _: &mut Window, cx: &mut Context<Self>) {
        self.commit_marked();
        self.after(cx);
    }
    fn replace_text_in_range(&mut self, r16: Option<Range<usize>>, new: &str, _: &mut Window, cx: &mut Context<Self>) {
        // committing a composition replaces it; otherwise a range from the
        // platform (autocorrect, the character palette) selects first
        let had_marked = self.marked.take().is_some();
        if let (Some(r16), false) = (r16, had_marked) {
            let t = self.text();
            let a = farfield_editor::utf16_to_utf8(&t, r16.start) as u32;
            let b = farfield_editor::utf16_to_utf8(&t, r16.end) as u32;
            self.with(|e| e.set_selection(a, b));
        }
        if new == "\n" {
            self.with(|e| e.key(Key::Enter, 0));
        } else {
            self.with(|e| e.insert(new));
        }
        self.after(cx);
    }
    fn replace_and_mark_text_in_range(
        &mut self,
        _r16: Option<Range<usize>>,
        new: &str,
        _sel: Option<Range<usize>>,
        _: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if self.marked.is_none() {
            let t = self.text();
            let (a, _) = self.with(|e| e.selection_range()).unwrap_or((0, 0));
            self.marked16 = farfield_editor::utf8_to_utf16(&t, a as usize);
        }
        self.marked = (!new.is_empty()).then(|| new.to_string());
        cx.notify();
    }
    fn bounds_for_range(
        &mut self,
        _r16: Range<usize>,
        el: Bounds<Pixels>,
        _: &mut Window,
        _: &mut Context<Self>,
    ) -> Option<Bounds<Pixels>> {
        // the IME candidate window sits under the caret
        let r = self.with(|e| e.caret_rect())?;
        let s = self.surface.2.max(1.0);
        Some(Bounds::new(
            point(el.left() + px(r.x as f32 / s), el.top() + px(r.y as f32 / s)),
            size(px(2.), px(r.h as f32 / s)),
        ))
    }
    fn character_index_for_point(&mut self, _p: Point<Pixels>, _: &mut Window, _: &mut Context<Self>) -> Option<usize> {
        None
    }
}

struct Surface {
    ed: Entity<DocEditor>,
}

impl IntoElement for Surface {
    type Element = Self;
    fn into_element(self) -> Self {
        self
    }
}

impl Element for Surface {
    type RequestLayoutState = ();
    type PrepaintState = ();
    fn id(&self) -> Option<ElementId> {
        None
    }
    fn source_location(&self) -> Option<&'static core::panic::Location<'static>> {
        None
    }
    fn request_layout(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        w: &mut Window,
        cx: &mut App,
    ) -> (LayoutId, ()) {
        let mut style = Style::default();
        style.size.width = gpui::relative(1.).into();
        style.size.height = gpui::relative(1.).into();
        (w.request_layout(style, [], cx), ())
    }
    fn prepaint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        _: Bounds<Pixels>,
        _: &mut (),
        _: &mut Window,
        _: &mut App,
    ) {
    }
    fn paint(
        &mut self,
        _: Option<&GlobalElementId>,
        _: Option<&gpui::InspectorElementId>,
        bounds: Bounds<Pixels>,
        _: &mut (),
        _: &mut (),
        w: &mut Window,
        cx: &mut App,
    ) {
        let scale = w.scale_factor();
        let focus = self.ed.read(cx).focus.clone();
        w.handle_input(&focus, ElementInputHandler::new(bounds, self.ed.clone()), cx);
        crate::perf::lap("doc-open", "surface-paint");
        let t_paint = std::time::Instant::now();
        let (img, retired, marked, input_at) = self.ed.update(cx, |this, _| {
            this.bounds = Some(bounds);
            let dw = (f32::from(bounds.size.width) * scale).round().max(1.0) as u32;
            let dh = (f32::from(bounds.size.height) * scale).round().max(1.0) as u32;
            if (dw, dh, scale) != this.surface {
                this.surface = (dw, dh, scale);
                this.with(|e| e.resize(dw, dh, scale as f64));
            }
            let drew = this.with(|e| e.render()).unwrap_or(false);
            if drew || this.image.is_none() {
                let mut buf = std::mem::take(&mut this.buf);
                if this.with(|e| e.framebuffer_bgra(&mut buf)).is_some() {
                    if let Some(frame) = image::ImageBuffer::from_raw(dw, dh, buf.clone()) {
                        let new = Arc::new(RenderImage::new(smallvec::smallvec![image::Frame::new(frame)]));
                        if let Some(old) = this.image.replace(new) {
                            this.retired.push(old);
                        }
                    }
                }
                this.buf = buf;
            }
            let caret = match this.marked.clone() {
                Some(m) => this.with(|e| e.caret_rect()).map(|r| (m, r)),
                None => None,
            };
            (this.image.clone(), std::mem::take(&mut this.retired), caret, this.input_at.take())
        });
        // every replaced frame leaves the sprite atlas, or GPU memory grows
        // with each keystroke and caret blink
        for old in retired {
            let _ = w.drop_image(old);
        }
        let t_upload = std::time::Instant::now();
        if input_at.is_some() {
            crate::perf::record("frame-host-prep", (t_upload - t_paint).as_secs_f64() * 1000.0);
        }
        if let Some(img) = img {
            let _ = w.paint_image(bounds, Corners::default(), img, 0, false);
            if input_at.is_some() {
                crate::perf::record("frame-paint-image", t_upload.elapsed().as_secs_f64() * 1000.0);
            }
            crate::perf::end("doc-open");
        }
        if let Some(t) = input_at {
            crate::perf::record("key-to-frame", t.elapsed().as_secs_f64() * 1000.0);
        }
        // composition text, drawn over the caret until the IME commits it
        if let Some((text, r)) = marked {
            let t = theme(cx).clone();
            let style = w.text_style();
            let font_size = px(r.h as f32 / scale * 0.72);
            let run = gpui::TextRun {
                len: text.len(),
                font: gpui::font(crate::theme::FONT_DOC),
                color: t.ink,
                background_color: Some(t.paper),
                underline: Some(gpui::UnderlineStyle { color: Some(t.accent), thickness: px(1.), wavy: false }),
                strikethrough: None,
            };
            let _ = style;
            let line = w.text_system().shape_line(text.into(), font_size, &[run], None);
            let origin = point(bounds.left() + px(r.x as f32 / scale), bounds.top() + px(r.y as f32 / scale));
            let _ = line.paint(origin, px(r.h as f32 / scale), w, cx);
        }
    }
}

macro_rules! keys {
    ($($act:ident => $key:ident, $m:expr;)*) => {
        impl DocEditor {
            $(
                #[allow(non_snake_case)]
                fn $act(&mut self, _: &$act, _: &mut Window, cx: &mut Context<Self>) { self.key(Key::$key, $m, cx) }
            )*
        }
        fn key_listeners(d: gpui::Stateful<gpui::Div>, cx: &mut Context<DocEditor>) -> gpui::Stateful<gpui::Div> {
            d $(.on_action(cx.listener(DocEditor::$act)))*
        }
    };
}

keys! {
    KLeft => Left, 0; KRight => Right, 0; KUp => Up, 0; KDown => Down, 0;
    KHome => Home, 0; KEnd => End, 0; KPageUp => PageUp, 0; KPageDown => PageDown, 0;
    KBackspace => Backspace, 0; KDelete => Delete, 0; KEnter => Enter, 0; KTab => Tab, 0; KEscape => Escape, 0;
    SLeft => Left, mods::SHIFT; SRight => Right, mods::SHIFT; SUp => Up, mods::SHIFT; SDown => Down, mods::SHIFT;
    SHome => Home, mods::SHIFT; SEnd => End, mods::SHIFT; SPageUp => PageUp, mods::SHIFT; SPageDown => PageDown, mods::SHIFT;
    STab => Tab, mods::SHIFT;
    WLeft => Left, mods::WORD; WRight => Right, mods::WORD;
    WSLeft => Left, mods::WORD | mods::SHIFT; WSRight => Right, mods::WORD | mods::SHIFT;
    WBackspace => Backspace, mods::WORD; WDelete => Delete, mods::WORD;
    CLeft => Left, mods::CMD; CRight => Right, mods::CMD; CUp => Up, mods::CMD; CDown => Down, mods::CMD;
    CSLeft => Left, mods::CMD | mods::SHIFT; CSRight => Right, mods::CMD | mods::SHIFT;
    CSUp => Up, mods::CMD | mods::SHIFT; CSDown => Down, mods::CMD | mods::SHIFT;
    CBackspace => Backspace, mods::CMD;
}

macro_rules! cmds {
    ($($act:ident => $c:ident;)*) => {
        impl DocEditor {
            $(
                #[allow(non_snake_case)]
                fn $act(&mut self, _: &$act, _: &mut Window, cx: &mut Context<Self>) { self.cmd(Command::$c, cx) }
            )*
        }
        fn cmd_listeners(d: gpui::Stateful<gpui::Div>, cx: &mut Context<DocEditor>) -> gpui::Stateful<gpui::Div> {
            d $(.on_action(cx.listener(DocEditor::$act)))*
        }
    };
}

cmds! {
    Bold => Bold; Italic => Italic; Code => Code; Link => Link; Strike => Strike;
    H1 => H1; H2 => H2; H3 => H3; Quote => Quote; Bullets => Bullets; Numbers => Numbers;
    CodeBlock => CodeBlock; Undo => Undo; Redo => Redo; SelectAll => SelectAll; Rule => Rule;
}

impl Render for DocEditor {
    fn render(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        crate::perf::lap("doc-open", "editor-render");
        let t = theme(cx).clone();
        if let Some(err) = &self.error {
            return div().p_4().text_color(t.bad).child(err.clone()).into_any_element();
        }
        let d = div()
            .id("doc-editor")
            .key_context("DocEditor")
            .track_focus(&self.focus)
            .cursor_text()
            .size_full()
            .bg(t.paper)
            .on_mouse_down(MouseButton::Left, cx.listener(Self::on_down))
            .on_mouse_move(cx.listener(Self::on_move))
            .on_mouse_up(MouseButton::Left, cx.listener(Self::on_up))
            .on_mouse_up_out(MouseButton::Left, cx.listener(Self::on_up))
            .on_scroll_wheel(cx.listener(Self::on_wheel))
            .on_action(cx.listener(Self::copy))
            .on_action(cx.listener(Self::cut))
            .on_action(cx.listener(Self::paste))
            // the Edit menu speaks the text field's actions
            .on_action(cx.listener(|this, _: &crate::ui::input::Copy, w, cx| this.copy(&Copy, w, cx)))
            .on_action(cx.listener(|this, _: &crate::ui::input::Cut, w, cx| this.cut(&Cut, w, cx)))
            .on_action(cx.listener(|this, _: &crate::ui::input::Paste, w, cx| this.paste(&Paste, w, cx)))
            .on_action(cx.listener(|this, _: &crate::ui::input::SelectAll, w, cx| this.SelectAll(&SelectAll, w, cx)))
            .on_action(cx.listener(|_, _: &CharPalette, w, _| w.show_character_palette()))
            .on_action(cx.listener(|_, _: &DropPending, _, cx| {
                if let Some(p) = cx.try_global::<crate::evidence::PendingDrop>().map(|p| p.0.clone()) {
                    cx.remove_global::<crate::evidence::PendingDrop>();
                    cx.emit(DocEvent::FilesDropped(p));
                }
            }))
            .drag_over::<ExternalPaths>(move |s, _, _, _| s.bg(t.wash))
            .on_drop(cx.listener(|_this, paths: &ExternalPaths, _w, cx| {
                cx.emit(DocEvent::FilesDropped(paths.paths().to_vec()));
            }));
        let d = key_listeners(d, cx);
        let d = cmd_listeners(d, cx);
        d.child(Surface { ed: cx.entity() }).into_any_element()
    }
}

impl Focusable for DocEditor {
    fn focus_handle(&self, _: &App) -> FocusHandle {
        self.focus.clone()
    }
}
