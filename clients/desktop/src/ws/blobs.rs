//! Blobs: every stored file as a calm thumbnail grid.
//!
//! The server pages 48 at a time; pages load as the grid scrolls to its end.
//! Thumbnails come from a blob's generated thumbnail (or the image itself
//! when it is small), fetched at most six at a time into a bounded cache by
//! CID; until one lands, the tile is the image's dominant colour. Filtering
//! by kind and CID is client-side over what is loaded. Uploads stream with
//! progress and can be cancelled; deletes are guarded by the server's
//! reference check.

use crate::app::{self, describe, log, Health};
use crate::shell::{confirm, set_health, toast};
use crate::theme::{parse_color, theme, Theme, FONT_MONO, S1, S2, S3, S4, S5};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, Kind as BtnKind};
use crate::workspace::Workspace;
use farfield_core::api::blobs::{self, Meta};
use farfield_core::api::ext_media::{self, MediaKind, Release};
use farfield_core::upload::Progress;
use farfield_core::{ApiError, Freshness, Latest};
use gpui::{
    canvas, div, img, prelude::*, px, AnyElement, App, ClipboardItem, Context, Entity, ExternalPaths, Hsla, ObjectFit,
    RenderImage, SharedString, StyledImage, UniformListScrollHandle, Window,
};
use std::cell::Cell;
use std::collections::{HashMap, HashSet, VecDeque};
use std::path::PathBuf;
use std::rc::Rc;
use std::sync::Arc;
use std::time::Duration;

// ── thumbnails (shared with the feed workspace) ─────────────────────────────

/// How many decoded thumbnails stay in memory (and in the sprite atlas).
const THUMB_CAP: usize = 200;
/// Concurrent thumbnail fetches.
const MAX_FETCHES: usize = 6;
/// Longest side a thumbnail is decoded to (tiles are ≤ ~260pt at 2×).
const THUMB_PX: u32 = 520;

/// What a tile can show right now.
#[derive(Clone)]
pub enum Thumb {
    /// On its way (or queued): show the placeholder.
    Loading,
    Ready(Arc<RenderImage>),
    /// Nothing to show (not an image, or it could not be fetched).
    None,
}

struct Want {
    key: String,
    /// The CID to fetch; None means "look up the blob's meta first".
    source: Option<String>,
}

/// Decoded thumbnails by blob CID: a bounded LRU with a fetch queue.
/// Replaced images are handed back through `take_retired` so the owner can
/// drop them from the sprite atlas (that needs a Window).
pub struct Thumbs {
    ready: HashMap<String, Arc<RenderImage>>,
    lru: VecDeque<String>,
    failed: HashSet<String>,
    pending: HashSet<String>,
    queue: VecDeque<Want>,
    in_flight: usize,
    retired: Vec<Arc<RenderImage>>,
    metas: HashMap<String, Meta>,
}

impl Thumbs {
    pub fn new() -> Self {
        Thumbs {
            ready: HashMap::new(),
            lru: VecDeque::new(),
            failed: HashSet::new(),
            pending: HashSet::new(),
            queue: VecDeque::new(),
            in_flight: 0,
            retired: Vec::new(),
            metas: HashMap::new(),
        }
    }

    fn touch(&mut self, key: &str) {
        if let Some(i) = self.lru.iter().position(|k| k == key) {
            if i + 1 != self.lru.len() {
                let k = self.lru.remove(i).unwrap();
                self.lru.push_back(k);
            }
        }
    }

    fn lookup(&mut self, key: &str) -> Option<Thumb> {
        if let Some(i) = self.ready.get(key).cloned() {
            self.touch(key);
            return Some(Thumb::Ready(i));
        }
        if self.failed.contains(key) {
            return Some(Thumb::None);
        }
        if self.pending.contains(key) {
            return Some(Thumb::Loading);
        }
        None
    }

    /// The thumbnail for a blob whose meta is known.
    pub fn for_meta(&mut self, m: &Meta, cx: &mut Context<Self>) -> Thumb {
        if let Some(t) = self.lookup(&m.cid) {
            return t;
        }
        match ext_media::thumb_source(m) {
            None => Thumb::None,
            Some(src) => {
                self.enqueue(Want { key: m.cid.clone(), source: Some(src) }, cx);
                Thumb::Loading
            }
        }
    }

    /// The thumbnail for a blob known only by CID (a post's embed); its meta
    /// is looked up first and kept (`meta`).
    pub fn for_cid(&mut self, cid: &str, cx: &mut Context<Self>) -> Thumb {
        if let Some(t) = self.lookup(cid) {
            return t;
        }
        let source = self.metas.get(cid).map(ext_media::thumb_source);
        match source {
            Some(None) => Thumb::None,
            Some(Some(src)) => {
                self.enqueue(Want { key: cid.into(), source: Some(src) }, cx);
                Thumb::Loading
            }
            None => {
                self.enqueue(Want { key: cid.into(), source: None }, cx);
                Thumb::Loading
            }
        }
    }

    /// A blob's meta, once a `for_cid` lookup has fetched it.
    pub fn meta(&self, cid: &str) -> Option<&Meta> {
        self.metas.get(cid)
    }

    /// Forget failures (after a refresh: the service may be back).
    pub fn retry_failed(&mut self) {
        self.failed.clear();
    }

    pub fn take_retired(&mut self) -> Vec<Arc<RenderImage>> {
        std::mem::take(&mut self.retired)
    }

    fn enqueue(&mut self, w: Want, cx: &mut Context<Self>) {
        self.pending.insert(w.key.clone());
        self.queue.push_back(w);
        self.pump(cx);
    }

    fn pump(&mut self, cx: &mut Context<Self>) {
        while self.in_flight < MAX_FETCHES {
            let Some(w) = self.queue.pop_back() else { break }; // newest first: what is on screen now
            self.in_flight += 1;
            let s = app::session(cx);
            let key = w.key.clone();
            let task = farfield_core::spawn(async move {
                let (meta, src) = match w.source {
                    Some(src) => (None, Some(src)),
                    None => {
                        let m = blobs::meta(&s, &w.key).await.ok().map(|l| l.value);
                        let src = m.as_ref().and_then(ext_media::thumb_source);
                        (m, src)
                    }
                };
                let Some(src) = src else { return (meta, None) };
                let Ok(bytes) = blobs::bytes(&s, &src, 16 << 20).await else { return (meta, None) };
                let img =
                    farfield_core::runtime().spawn_blocking(move || decode(&bytes, THUMB_PX)).await.ok().flatten();
                (meta, img)
            });
            cx.spawn(async move |this, cx| {
                let r = task.await;
                let _ = this.update(cx, |t, cx| {
                    t.in_flight -= 1;
                    t.pending.remove(&key);
                    match r {
                        Ok((meta, Some(img))) => {
                            if let Some(m) = meta {
                                t.metas.insert(key.clone(), m);
                            }
                            t.ready.insert(key.clone(), img);
                            t.lru.push_back(key);
                            while t.ready.len() > THUMB_CAP {
                                let Some(old) = t.lru.pop_front() else { break };
                                if let Some(i) = t.ready.remove(&old) {
                                    t.retired.push(i);
                                }
                            }
                        }
                        Ok((meta, None)) => {
                            if let Some(m) = meta {
                                t.metas.insert(key.clone(), m);
                            }
                            t.failed.insert(key);
                        }
                        Err(_) => {
                            t.failed.insert(key);
                        }
                    }
                    t.pump(cx);
                    cx.notify();
                });
            })
            .detach();
        }
    }
}

impl Default for Thumbs {
    fn default() -> Self {
        Self::new()
    }
}

/// Decode image bytes for display: scaled to fit `max`, BGRA as the sprite
/// atlas wants. Runs off the UI thread.
pub fn decode(bytes: &[u8], max: u32) -> Option<Arc<RenderImage>> {
    let im = image::load_from_memory(bytes).ok()?;
    let im = if im.width() > max || im.height() > max { im.thumbnail(max, max) } else { im };
    let mut buf = im.to_rgba8();
    for p in buf.pixels_mut() {
        p.0.swap(0, 2);
    }
    Some(Arc::new(RenderImage::new(smallvec::smallvec![image::Frame::new(buf)])))
}

/// A blob's dominant colour, for the placeholder behind its thumbnail.
pub fn dominant(m: Option<&Meta>, t: &Theme) -> Hsla {
    m.and_then(|m| parse_color(&m.dominant_color)).map(|c| gpui::rgba(c).into()).unwrap_or(t.paper_2)
}

/// The word a tile shows when there is no picture: the file's extension-ish
/// subtype ("MP4", "PDF") or its kind.
pub fn kind_label(m: &Meta) -> String {
    let sub = m.mime.split('/').nth(1).unwrap_or("").split(['+', ';', '.']).next().unwrap_or("");
    let sub = sub.trim_start_matches("x-");
    if sub.is_empty() || sub.len() > 8 {
        MediaKind::of(&m.mime).word().to_uppercase()
    } else {
        sub.to_uppercase()
    }
}

/// How long ago, briefly ("40s", "3m", "2h", "5d") — for the offline banners.
pub fn ago(ms: u64) -> String {
    let s = ms / 1000;
    match s {
        0..=59 => format!("{s}s"),
        60..=3599 => format!("{}m", s / 60),
        3600..=86399 => format!("{}h", s / 3600),
        _ => format!("{}d", s / 86400),
    }
}

fn short_cid(c: &str) -> String {
    if c.len() > 14 {
        format!("{}…{}", &c[..6], &c[c.len() - 6..])
    } else {
        c.to_string()
    }
}

// ── the workspace ───────────────────────────────────────────────────────────

const TILE_MIN: f32 = 156.;
const GAP: f32 = 14.;
const PAD: f32 = 24.;
const CAPTION: f32 = 34.;
const PREVIEW_PX: u32 = 1400;

struct Upload {
    id: u64,
    name: String,
    progress: Progress,
}

pub struct BlobsWs {
    blobs: Vec<Meta>,
    page: u32,
    pages: i64,
    total: i64,
    loading: bool,
    freshness: Option<Freshness>,
    error: Option<String>,
    latest: Arc<Latest>,
    search: Entity<TextField>,
    kind: Option<MediaKind>,
    scroll: UniformListScrollHandle,
    selected: Option<String>,
    thumbs: Entity<Thumbs>,
    /// The grid's width last frame, measured; columns follow it.
    width: Rc<Cell<f32>>,
    preview: Option<(String, Arc<RenderImage>)>,
    preview_loading: Option<String>,
    retired: Vec<Arc<RenderImage>>,
    uploads: Vec<Upload>,
    next_upload: u64,
}

impl BlobsWs {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        let search = cx.new(|cx| TextField::new(w, cx, "", "Filter by CID or type  ⌘F").mono());
        cx.subscribe_in(&search, w, |this: &mut Self, _, e: &FieldEvent, _w, cx| match e {
            FieldEvent::Changed => cx.notify(),
            FieldEvent::Down => this.step(1, cx),
            FieldEvent::Up => this.step(-1, cx),
            FieldEvent::Submit => this.insert_selected(cx),
            _ => {}
        })
        .detach();
        let thumbs = cx.new(|_| Thumbs::new());
        cx.observe(&thumbs, |_, _, cx| cx.notify()).detach();
        let mut this = BlobsWs {
            blobs: Vec::new(),
            page: 1,
            pages: 0,
            total: 0,
            loading: false,
            freshness: None,
            error: None,
            latest: Arc::new(Latest::default()),
            search,
            kind: None,
            scroll: UniformListScrollHandle::new(),
            selected: None,
            thumbs,
            width: Rc::new(Cell::new(0.)),
            preview: None,
            preview_loading: None,
            retired: Vec::new(),
            uploads: Vec::new(),
            next_upload: 0,
        };
        this.reload(cx);
        this
    }

    fn reload(&mut self, cx: &mut Context<Self>) {
        self.page = 1;
        self.thumbs.update(cx, |t, _| t.retry_failed());
        self.load(false, cx);
    }

    fn load(&mut self, append: bool, cx: &mut Context<Self>) {
        let ticket = self.latest.ticket();
        let latest = self.latest.clone();
        let s = app::session(cx);
        let page = self.page;
        self.loading = true;
        cx.notify();
        let task = farfield_core::spawn(async move { blobs::list(&s, page).await });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                if !latest.is_current(ticket) {
                    return;
                }
                this.loading = false;
                match r {
                    Ok(Ok(l)) => {
                        if !append {
                            this.blobs.clear();
                        }
                        // pages shift as uploads land: never show a CID twice
                        let have: HashSet<String> = this.blobs.iter().map(|b| b.cid.clone()).collect();
                        this.blobs.extend(l.value.blobs.into_iter().filter(|b| !have.contains(&b.cid)));
                        this.total = l.value.total;
                        this.pages = l.value.pages;
                        this.error = None;
                        let h = match &l.freshness {
                            Freshness::Live => Health::Up,
                            Freshness::Stale { error, .. } => Health::Down(error.to_string()),
                        };
                        set_health(cx, "blobs", h);
                        this.freshness = Some(l.freshness);
                        log("blobs-loaded", &[("page", &page.to_string()), ("count", &this.blobs.len().to_string())]);
                    }
                    Ok(Err(e)) => {
                        if e.is_auth() {
                            set_health(cx, "blobs", Health::NoAuth);
                        } else if e.is_offline() {
                            set_health(cx, "blobs", Health::Down(e.to_string()));
                        }
                        if append {
                            this.page = this.page.saturating_sub(1).max(1);
                        }
                        this.error = Some(describe(&e));
                    }
                    Err(e) => this.error = Some(e.to_string()),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn has_more(&self) -> bool {
        (self.page as i64) < self.pages
    }

    fn load_more(&mut self, cx: &mut Context<Self>) {
        if self.loading || !self.has_more() || self.error.is_some() {
            return;
        }
        self.page += 1;
        self.load(true, cx);
    }

    /// What the grid shows: loaded blobs through the kind and text filters.
    fn visible(&self, cx: &App) -> Vec<Meta> {
        let q = self.search.read(cx).text().trim().to_lowercase();
        self.blobs
            .iter()
            .filter(|b| self.kind.is_none_or(|k| MediaKind::of(&b.mime) == k))
            .filter(|b| q.is_empty() || b.cid.contains(&q) || b.mime.contains(&q))
            .cloned()
            .collect()
    }

    fn selected_meta(&self) -> Option<&Meta> {
        let c = self.selected.as_ref()?;
        self.blobs.iter().find(|b| &b.cid == c)
    }

    fn cols(&self) -> usize {
        let w = self.width.get();
        if w <= 0. {
            return 4;
        }
        (((w - 2. * PAD + GAP) / (TILE_MIN + GAP)).floor() as usize).max(1)
    }

    fn step(&mut self, by: i32, cx: &mut Context<Self>) {
        let v = self.visible(cx);
        if v.is_empty() {
            return;
        }
        let cur = self.selected.as_ref().and_then(|c| v.iter().position(|b| &b.cid == c));
        let next = match cur {
            None => 0,
            Some(i) => (i as i32 + by).clamp(0, v.len() as i32 - 1) as usize,
        };
        self.scroll.scroll_to_item(next / self.cols(), gpui::ScrollStrategy::Center);
        self.select(v[next].cid.clone(), cx);
    }

    fn select(&mut self, cid: String, cx: &mut Context<Self>) {
        if self.selected.as_deref() == Some(cid.as_str()) {
            return;
        }
        self.selected = Some(cid.clone());
        log("blob-select", &[("cid", &cid)]);
        self.load_preview(cx);
        cx.notify();
    }

    /// The inspector's hero: the image itself, larger than the grid's.
    fn load_preview(&mut self, cx: &mut Context<Self>) {
        let Some(m) = self.selected_meta().cloned() else { return };
        if self.preview.as_ref().is_some_and(|(c, _)| c == &m.cid) {
            return;
        }
        if let Some((_, old)) = self.preview.take() {
            self.retired.push(old);
        }
        if !m.is_image() || m.mime == "image/svg+xml" || m.size > 48 << 20 {
            self.preview_loading = None;
            return;
        }
        self.preview_loading = Some(m.cid.clone());
        let s = app::session(cx);
        let cid = m.cid.clone();
        let task = farfield_core::spawn(async move {
            let bytes = blobs::bytes(&s, &cid, 64 << 20).await.ok()?;
            farfield_core::runtime().spawn_blocking(move || decode(&bytes, PREVIEW_PX)).await.ok().flatten()
        });
        let cid = m.cid;
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                if this.preview_loading.as_deref() != Some(cid.as_str()) {
                    if let Ok(Some(i)) = r {
                        this.retired.push(i);
                    }
                    return;
                }
                this.preview_loading = None;
                if let Ok(Some(i)) = r {
                    this.preview = Some((cid, i));
                }
                cx.notify();
            });
        })
        .detach();
    }

    // ── actions ──

    fn copy(&self, text: String, what: &str, cx: &mut Context<Self>) {
        cx.write_to_clipboard(ClipboardItem::new_string(text.clone()));
        log("blob-copy", &[("what", what), ("text", &text)]);
        toast(cx, format!("Copied {what}."), false);
    }

    fn copy_ref(&mut self, cx: &mut Context<Self>) {
        if let Some(m) = self.selected_meta() {
            let r = m.reference();
            self.copy(r, "the blob:// reference", cx);
        }
    }

    fn copy_link(&mut self, cx: &mut Context<Self>) {
        let Some(m) = self.selected_meta() else { return };
        match blobs::public_url(&app::session(cx), &m.cid) {
            Some(u) => self.copy(u, "the public link", cx),
            None => toast(cx, "This profile has no public address for blobs — add one in Connections.", true),
        }
    }

    /// "Insert into the open document": the Markdown goes on the clipboard,
    /// ready for ⌘V in the document — the editor owns its caret, and a paste
    /// is the one insertion that cannot land in the wrong place.
    fn insert_selected(&mut self, cx: &mut Context<Self>) {
        let Some(m) = self.selected_meta().cloned() else { return };
        let md = m.markdown("");
        cx.write_to_clipboard(ClipboardItem::new_string(md.clone()));
        log("blob-insert", &[("cid", &m.cid)]);
        toast(cx, format!("Copied {md} — switch to the document (⌘1) and paste it where it belongs (⌘V)."), false);
    }

    fn pick_upload(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        let rx = cx.prompt_for_paths(gpui::PathPromptOptions {
            files: true,
            directories: false,
            multiple: true,
            prompt: Some("Upload".into()),
        });
        cx.spawn_in(w, async move |this, cx| {
            if let Ok(Ok(Some(paths))) = rx.await {
                let _ = this.update(cx, |this, cx| this.upload(paths, cx));
            }
        })
        .detach();
    }

    fn upload(&mut self, paths: Vec<PathBuf>, cx: &mut Context<Self>) {
        for p in paths {
            if p.is_dir() {
                toast(cx, format!("{} is a folder — drop the files inside it.", p.display()), true);
                continue;
            }
            let name = p.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default();
            let progress = Progress::new(std::fs::metadata(&p).map(|m| m.len()).unwrap_or(0));
            self.next_upload += 1;
            let id = self.next_upload;
            self.uploads.push(Upload { id, name: name.clone(), progress: progress.clone() });
            log("blob-upload-start", &[("file", &name)]);
            let s = app::session(cx);
            let pr = progress.clone();
            let task = farfield_core::spawn(async move { blobs::upload(&s, &p, &pr).await });
            let pr2 = progress.clone();
            cx.spawn(async move |this, cx| loop {
                cx.background_executor().timer(Duration::from_millis(120)).await;
                let done = pr2.sent() >= pr2.total() || pr2.is_cancelled();
                if this.update(cx, |_, cx| cx.notify()).is_err() || done {
                    break;
                }
            })
            .detach();
            cx.spawn(async move |this, cx| {
                let r = task.await;
                let _ = this.update(cx, |this, cx| {
                    this.uploads.retain(|u| u.id != id);
                    match r {
                        Ok(Ok(m)) => {
                            let known = this.blobs.iter().any(|b| b.cid == m.value.cid);
                            log("blob-upload-done", &[("file", &name), ("cid", &m.value.cid)]);
                            toast(cx, if known { format!("{name} was already stored — same bytes, same CID.") } else { format!("Uploaded {name}.") }, false);
                            this.selected = None;
                            this.select(m.value.cid.clone(), cx);
                            if !known {
                                this.blobs.insert(0, m.value);
                                this.total += 1;
                            }
                            this.load_preview(cx);
                            if this.uploads.is_empty() {
                                this.reload(cx);
                            }
                        }
                        Ok(Err(ApiError::Cancelled)) => {
                            log("blob-upload-cancelled", &[("file", &name)]);
                            toast(cx, format!("Upload of {name} cancelled."), false)
                        }
                        Ok(Err(e @ ApiError::Uncertain(_))) => {
                            toast(cx, format!("{name}: the connection dropped mid-upload. Uploading again is safe — the same bytes keep the same CID."), true);
                            log("blob-upload-failed", &[("file", &name), ("error", &e.to_string())]);
                        }
                        Ok(Err(e)) => {
                            if e.is_auth() {
                                set_health(cx, "blobs", Health::NoAuth);
                            }
                            log("blob-upload-failed", &[("file", &name), ("error", &e.to_string())]);
                            toast(cx, format!("{name}: {}", describe(&e)), true)
                        }
                        Err(e) => toast(cx, e.to_string(), true),
                    }
                    cx.notify();
                });
            })
            .detach();
        }
        cx.notify();
    }

    fn delete(&mut self, cx: &mut Context<Self>) {
        let Some(m) = self.selected_meta().cloned() else { return };
        let ent = cx.entity();
        let body = format!(
            "{} · {} is removed for good — blobs have no backup. If any entry, series or post still embeds it, the server keeps it and says how many do.",
            short_cid(&m.cid),
            ui::bytes(m.size)
        );
        confirm(cx, "Delete this blob?", body, "Delete", true, move |_, cx| {
            let s = app::session(cx);
            let cid = m.cid.clone();
            let task = farfield_core::spawn(async move { ext_media::delete_unless_referenced(&s, &cid).await });
            ent.update(cx, |_, cx| {
                cx.spawn(async move |this, cx| {
                    let r = task.await;
                    let _ = this.update(cx, |this, cx| {
                        match r {
                            Ok(Ok(Release::Deleted)) => {
                                log("blob-delete", &[("cid", &m.cid)]);
                                this.blobs.retain(|b| b.cid != m.cid);
                                this.total = (this.total - 1).max(0);
                                this.selected = None;
                                if let Some((_, old)) = this.preview.take() {
                                    this.retired.push(old);
                                }
                                toast(cx, "Deleted. Edge caches may serve it for a while; check with ?cb= before relying on it.", false);
                            }
                            Ok(Ok(Release::Kept { references, message })) => {
                                log("blob-delete-kept", &[("cid", &m.cid), ("references", &format!("{references:?}"))]);
                                let why = match references {
                                    Some(1) => "One document still embeds it".to_string(),
                                    Some(n) => format!("{n} documents still embed it"),
                                    None => format!("The server said: {message}"),
                                };
                                toast(cx, format!("Kept. {why} — remove those references first."), true);
                            }
                            Ok(Err(e)) => toast(cx, describe(&e), true),
                            Err(e) => toast(cx, e.to_string(), true),
                        }
                        cx.notify();
                    });
                })
                .detach();
            });
        });
    }

    fn set_kind(&mut self, k: Option<MediaKind>, cx: &mut Context<Self>) {
        self.kind = k;
        self.scroll.scroll_to_item(0, gpui::ScrollStrategy::Top);
        cx.notify();
    }

    // ── rendering ──

    fn render_header(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let counts = |k: MediaKind| self.blobs.iter().filter(|b| MediaKind::of(&b.mime) == k).count();
        let tab = |id: &'static str, label: String, on: bool| {
            div()
                .id(id)
                .px(S2)
                .py(px(3.))
                .text_sm()
                .cursor_pointer()
                .border_b_2()
                .border_color(if on { t.accent } else { gpui::transparent_black() })
                .text_color(if on { t.ink } else { t.ink_2 })
                .hover(|s| s.text_color(t.ink))
                .child(label)
        };
        let k = self.kind;
        let tabs: Vec<(&'static str, &'static str, Option<MediaKind>, usize)> = vec![
            ("k-all", "All", None, self.blobs.len()),
            ("k-img", "Images", Some(MediaKind::Image), counts(MediaKind::Image)),
            ("k-vid", "Video", Some(MediaKind::Video), counts(MediaKind::Video)),
            ("k-aud", "Audio", Some(MediaKind::Audio), counts(MediaKind::Audio)),
            ("k-oth", "Other", Some(MediaKind::Other), counts(MediaKind::Other)),
        ];
        let loaded = self.blobs.len() as i64;
        let readout = if self.total == 0 && self.blobs.is_empty() {
            String::new()
        } else if loaded >= self.total {
            format!("{} blobs", self.total)
        } else {
            format!("{} of {} loaded", loaded, self.total)
        };
        let e = cx.entity();
        div()
            .flex()
            .flex_col()
            .gap(S3)
            .px(px(PAD))
            .pt(S4)
            .pb(S3)
            .child(
                div()
                    .flex()
                    .items_end()
                    .gap(S3)
                    .child(
                        div().text_size(px(20.)).text_color(t.ink).font_weight(gpui::FontWeight::MEDIUM).child("Blobs"),
                    )
                    .child(div().pb(px(3.)).font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(readout))
                    .child(div().flex_1())
                    .child(ui::button("upload", "Upload…  ⌘N", BtnKind::Primary, cx, move |_, w, cx| {
                        e.update(cx, |this, cx| this.pick_upload(w, cx))
                    })),
            )
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(S4)
                    .child(div().flex().gap(S1).children(tabs.into_iter().map(|(id, label, kind, n)| {
                        let label = if n > 0 || kind.is_none() { format!("{label} {n}") } else { label.to_string() };
                        tab(id, label, k == kind).on_click(cx.listener(move |this, _, _, cx| this.set_kind(kind, cx)))
                    })))
                    .child(div().flex_1().min_w(px(160.)).max_w(px(360.)).child(self.search.clone())),
            )
            .into_any_element()
    }

    fn render_uploads(&self, cx: &mut Context<Self>) -> Option<AnyElement> {
        if self.uploads.is_empty() {
            return None;
        }
        let t = theme(cx).clone();
        let mut col = div().flex().flex_col().gap(S2).px(px(PAD)).pb(S3);
        for u in &self.uploads {
            let p = u.progress.clone();
            let frac = p.fraction();
            col = col.child(
                div()
                    .flex()
                    .items_center()
                    .gap(S3)
                    .child(div().w(px(220.)).truncate().text_sm().text_color(t.ink).child(u.name.clone()))
                    .child(
                        div().flex_1().h(px(2.)).bg(t.rule).child(div().h_full().w(gpui::relative(frac)).bg(t.accent)),
                    )
                    .child(div().w(px(120.)).font_family(FONT_MONO).text_xs().text_color(t.ink_2).child(format!(
                        "{} / {}",
                        ui::bytes(p.sent() as i64),
                        ui::bytes(p.total() as i64)
                    )))
                    .child(ui::button(
                        SharedString::from(format!("cancel-{}", u.id)),
                        "Cancel",
                        BtnKind::Quiet,
                        cx,
                        move |_, _, _| p.cancel(),
                    )),
            );
        }
        Some(col.into_any_element())
    }

    fn render_grid(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let items = Arc::new(self.visible(cx));
        let cols = self.cols();
        let width = self.width.get();
        let tile =
            if width > 0. { ((width - 2. * PAD - GAP * (cols as f32 - 1.)) / cols as f32).floor() } else { TILE_MIN };
        let more = self.has_more();
        let rows = items.len().div_ceil(cols) + usize::from(more || self.loading);
        let selected = self.selected.clone();
        let loading = self.loading;
        let ent = cx.entity();
        let thumbs = self.thumbs.clone();
        let cell = self.width.clone();
        let ent2 = cx.entity();
        let measure = canvas(
            move |bounds, _, cx| {
                let w = f32::from(bounds.size.width);
                if (cell.get() - w).abs() > 0.5 {
                    cell.set(w);
                    ent2.update(cx, |_, cx| cx.notify());
                }
            },
            |_, _, _, _| {},
        )
        .absolute()
        .top_0()
        .left_0()
        .size_full();

        let list = gpui::uniform_list("blob-grid", rows, move |range, _w, cx| {
            let t = theme(cx).clone();
            let last = range.end;
            let out: Vec<AnyElement> = range
                .map(|r| {
                    let start = r * cols;
                    if start >= items.len() {
                        let e = ent.clone();
                        return div()
                            .id(("more", r))
                            .h(px(tile + CAPTION + GAP))
                            .px(px(PAD))
                            .flex()
                            .items_center()
                            .text_sm()
                            .text_color(t.ink_2)
                            .child(if loading { "Loading more…" } else { "Load more" })
                            .cursor_pointer()
                            .on_click(move |_, _, cx| e.update(cx, |this, cx| this.load_more(cx)))
                            .into_any_element();
                    }
                    let end = (start + cols).min(items.len());
                    div()
                        .id(("row", r))
                        .h(px(tile + CAPTION + GAP))
                        .px(px(PAD))
                        .flex()
                        .gap(px(GAP))
                        .children((start..end).map(|i| {
                            let m = items[i].clone();
                            let th = thumbs.update(cx, |th, cx| th.for_meta(&m, cx));
                            let on = selected.as_deref() == Some(m.cid.as_str());
                            let e = ent.clone();
                            render_tile(&m, th, tile, on, &t, i).on_click(move |_, _, cx| {
                                let cid = m.cid.clone();
                                e.update(cx, |this, cx| this.select(cid, cx))
                            })
                        }))
                        .into_any_element()
                })
                .collect();
            // reaching the end of what is loaded fetches the next page
            if more && !loading && last >= rows.saturating_sub(1) {
                let e = ent.clone();
                cx.defer(move |cx| e.update(cx, |this, cx| this.load_more(cx)));
            }
            out
        })
        .track_scroll(self.scroll.clone())
        .size_full();

        let _ = t;
        div().relative().flex_1().min_h_0().child(measure).child(list).into_any_element()
    }

    fn render_empty(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let (head, sub) = if self.loading {
            ("Loading blobs…", "")
        } else if self.blobs.is_empty() {
            (
                "Nothing stored yet.",
                "Drop files anywhere here, or press ⌘N to choose some. Images get thumbnails; everything gets a CID.",
            )
        } else {
            (
                "Nothing matches.",
                "Clear the filter, or pick another kind. Only loaded pages are filtered — scroll to load more.",
            )
        };
        div()
            .flex_1()
            .flex()
            .flex_col()
            .items_center()
            .justify_center()
            .gap(S2)
            .p(S5)
            .child(div().text_size(px(17.)).text_color(t.ink).child(head))
            .child(div().max_w(px(420.)).text_sm().text_color(t.ink_2).text_center().child(sub))
            .into_any_element()
    }
}

/// One grid tile: the picture (or its placeholder) and a mono caption.
fn render_tile(m: &Meta, th: Thumb, size: f32, on: bool, t: &Theme, i: usize) -> gpui::Stateful<gpui::Div> {
    let bg = dominant(Some(m), t);
    let ring = if on { t.accent } else { gpui::transparent_black() };
    let hover = t.rule_strong;
    let face: AnyElement = match th {
        Thumb::Ready(im) => img(im).size_full().object_fit(ObjectFit::Cover).into_any_element(),
        Thumb::Loading => div().size_full().into_any_element(),
        Thumb::None => div()
            .size_full()
            .flex()
            .flex_col()
            .items_center()
            .justify_center()
            .gap(px(2.))
            .child(div().font_family(FONT_MONO).text_sm().text_color(t.ink_2).child(kind_label(m)))
            .child(div().text_xs().text_color(t.ink_3).child(MediaKind::of(&m.mime).word()))
            .into_any_element(),
    };
    let dims =
        if m.width > 0 && m.height > 0 { format!("{}×{}", m.width, m.height) } else { kind_label(m).to_lowercase() };
    div()
        .id(("tile", i))
        .w(px(size))
        .flex()
        .flex_col()
        .gap(px(6.))
        .cursor_pointer()
        .child(
            div()
                .w(px(size))
                .h(px(size))
                .rounded(px(4.))
                .overflow_hidden()
                .bg(if matches!(face_kind(m), MediaKind::Image) { bg } else { t.paper_2 })
                .border_2()
                .border_color(ring)
                .when(!on, move |d| d.hover(move |s| s.border_color(hover)))
                .child(face),
        )
        .child(
            div()
                .flex()
                .justify_between()
                .gap(S2)
                .font_family(FONT_MONO)
                .text_xs()
                .text_color(if on { t.ink } else { t.ink_3 })
                .child(div().truncate().child(dims))
                .child(div().flex_none().child(ui::bytes(m.size))),
        )
}

fn face_kind(m: &Meta) -> MediaKind {
    MediaKind::of(&m.mime)
}

impl Workspace for BlobsWs {
    fn inspector(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> Option<AnyElement> {
        let t = theme(cx).clone();
        let Some(m) = self.selected_meta().cloned() else {
            return Some(
                div()
                    .flex()
                    .flex_col()
                    .gap(S2)
                    .child(ui::eyebrow("Blob", cx))
                    .child(
                        div()
                            .text_sm()
                            .text_color(t.ink_2)
                            .child("Choose a tile to see it large, copy its reference, or put it in a document."),
                    )
                    .child(div().text_xs().text_color(t.ink_3).child(
                        "↑ ↓ from the filter move through the grid; Enter copies the selected blob for pasting.",
                    ))
                    .into_any_element(),
            );
        };
        let kind = MediaKind::of(&m.mime);
        // the hero: the full image, aspect kept; otherwise its colour field
        let aspect = if m.width > 0 && m.height > 0 { m.height as f32 / m.width as f32 } else { 0.75 };
        let w_avail = (app::state(cx).prefs.inspector_width - 32.).max(160.);
        let h = (w_avail * aspect).clamp(80., 420.);
        let hero: AnyElement = match (&self.preview, kind) {
            (Some((c, im)), _) if c == &m.cid => {
                img(im.clone()).w_full().h(px(h)).object_fit(ObjectFit::Contain).into_any_element()
            }
            (_, MediaKind::Image) => {
                let th = self.thumbs.update(cx, |th, cx| th.for_meta(&m, cx));
                match th {
                    Thumb::Ready(im) => img(im).w_full().h(px(h)).object_fit(ObjectFit::Contain).into_any_element(),
                    _ => div().w_full().h(px(h)).rounded(px(4.)).bg(dominant(Some(&m), &t)).into_any_element(),
                }
            }
            _ => div()
                .w_full()
                .h(px(140.))
                .rounded(px(4.))
                .bg(t.paper_2)
                .flex()
                .flex_col()
                .items_center()
                .justify_center()
                .gap(px(2.))
                .child(div().font_family(FONT_MONO).text_size(px(18.)).text_color(t.ink_2).child(kind_label(&m)))
                .child(div().text_xs().text_color(t.ink_3).child(format!("{} · no preview here", kind.word())))
                .into_any_element(),
        };
        let readout = |label: &'static str, v: String| {
            div()
                .flex()
                .justify_between()
                .gap(S3)
                .py(px(3.))
                .child(div().text_xs().text_color(t.ink_2).child(label))
                .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink).child(v))
        };
        let e = cx.entity();
        let (e1, e2, e3, e4) = (e.clone(), e.clone(), e.clone(), e.clone());
        let mut col = div()
            .flex()
            .flex_col()
            .gap(S2)
            .child(hero)
            .child(div().pt(S2).child(ui::eyebrow(format!("{} blob", kind.word()), cx)))
            .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink).child(m.cid.clone()))
            .child(
                div()
                    .flex()
                    .flex_col()
                    .pt(S1)
                    .child(readout("Type", m.mime.clone()))
                    .child(readout("Size", ui::bytes(m.size)))
                    .when(m.width > 0 && m.height > 0, |d| {
                        d.child(readout("Dimensions", format!("{} × {}", m.width, m.height)))
                    })
                    .child(readout("Created", ui::when(&m.created_at)))
                    .when(!m.dominant_color.is_empty(), |d| {
                        d.child(
                            div()
                                .flex()
                                .justify_between()
                                .py(px(3.))
                                .child(div().text_xs().text_color(t.ink_2).child("Colour"))
                                .child(
                                    div()
                                        .flex()
                                        .items_center()
                                        .gap(px(6.))
                                        .child(div().w(px(10.)).h(px(10.)).rounded_full().bg(dominant(Some(&m), &t)))
                                        .child(
                                            div()
                                                .font_family(FONT_MONO)
                                                .text_xs()
                                                .text_color(t.ink)
                                                .child(m.dominant_color.clone()),
                                        ),
                                ),
                        )
                    }),
            )
            .child(ui::rule(cx))
            .child(ui::eyebrow("Use it", cx))
            .child(ui::button("insert", "Insert into open document", BtnKind::Primary, cx, move |_, _, cx| {
                e1.update(cx, |this, cx| this.insert_selected(cx))
            }))
            .child(div().text_xs().text_color(t.ink_3).child(m.markdown("")))
            .child(
                div()
                    .flex()
                    .flex_wrap()
                    .gap(S1)
                    .child(ui::button("copy-ref", "Copy blob:// reference", BtnKind::Quiet, cx, move |_, _, cx| {
                        e2.update(cx, |this, cx| this.copy_ref(cx))
                    }))
                    .child(ui::button("copy-link", "Copy public link", BtnKind::Quiet, cx, move |_, _, cx| {
                        e3.update(cx, |this, cx| this.copy_link(cx))
                    })),
            );
        if !m.thumb_cid.is_empty() {
            col = col.child(ui::field_row("Thumbnail", ui::mono(m.thumb_cid.clone(), cx), cx));
        }
        col = col
            .child(ui::rule(cx))
            .child(ui::button("delete", "Delete…", BtnKind::Danger, cx, move |_, _, cx| {
                e4.update(cx, |this, cx| this.delete(cx))
            }))
            .child(div().text_xs().text_color(t.ink_3).child("Refused while any document embeds it."));
        Some(col.into_any_element())
    }

    fn commands(&self, _cx: &App) -> Vec<(&'static str, String, &'static str)> {
        let mut v = vec![
            ("upload", "Blobs: upload files…".to_string(), "⌘N"),
            ("all", "Blobs: show everything".into(), ""),
            ("images", "Blobs: show images".into(), ""),
            ("video", "Blobs: show video".into(), ""),
            ("audio", "Blobs: show audio".into(), ""),
            ("other", "Blobs: show other files".into(), ""),
        ];
        if self.selected.is_some() {
            v.extend([
                ("insert", "Blobs: insert into open document".to_string(), "↵"),
                ("copy-ref", "Blobs: copy blob:// reference".into(), ""),
                ("copy-link", "Blobs: copy public link".into(), ""),
                ("delete", "Blobs: delete this blob…".into(), ""),
            ]);
        }
        v
    }

    fn run_command(&mut self, id: &str, w: &mut Window, cx: &mut Context<Self>) {
        match id {
            "upload" => self.pick_upload(w, cx),
            "all" => self.set_kind(None, cx),
            "images" => self.set_kind(Some(MediaKind::Image), cx),
            "video" => self.set_kind(Some(MediaKind::Video), cx),
            "audio" => self.set_kind(Some(MediaKind::Audio), cx),
            "other" => self.set_kind(Some(MediaKind::Other), cx),
            "insert" => self.insert_selected(cx),
            "copy-ref" => self.copy_ref(cx),
            "copy-link" => self.copy_link(cx),
            "delete" => self.delete(cx),
            _ => {}
        }
    }

    fn focus_search(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        self.search.read(cx).focus(w);
    }
    fn new_item(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        self.pick_upload(w, cx)
    }
    fn refresh(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        self.reload(cx)
    }
    fn dirty(&self, _cx: &App) -> bool {
        !self.uploads.is_empty()
    }
}

impl Render for BlobsWs {
    fn render(&mut self, w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = theme(cx).clone();
        for old in self.thumbs.update(cx, |th, _| th.take_retired()).into_iter().chain(self.retired.drain(..)) {
            let _ = w.drop_image(old);
        }
        let status: Option<AnyElement> = match (&self.error, &self.freshness) {
            (Some(e), _) => Some(ui::notice(format!("{e}  ⌘R tries again."), t.bad, cx).into_any_element()),
            (None, Some(Freshness::Stale { age_ms, .. })) => Some(
                ui::notice(
                    format!(
                        "Offline — showing what was loaded {} ago. Uploads and deletes wait for the service.",
                        ago(*age_ms)
                    ),
                    t.warn,
                    cx,
                )
                .into_any_element(),
            ),
            _ => None,
        };
        let empty = self.visible(cx).is_empty();
        let wash = t.wash;
        div()
            .size_full()
            .flex()
            .flex_col()
            .on_action(cx.listener(|this, _: &crate::ui::doc_editor::DropPending, _, cx| {
                if let Some(p) = cx.try_global::<crate::evidence::PendingDrop>().map(|p| p.0.clone()) {
                    cx.remove_global::<crate::evidence::PendingDrop>();
                    this.upload(p, cx);
                }
            }))
            .drag_over::<ExternalPaths>(move |s, _, _, _| s.bg(wash))
            .on_drop(cx.listener(|this, paths: &ExternalPaths, _, cx| this.upload(paths.paths().to_vec(), cx)))
            .child(self.render_header(cx))
            .children(self.render_uploads(cx))
            .when_some(status, |d, s| d.child(div().px(px(PAD)).pb(S3).child(s)))
            .child(ui::rule(cx))
            .child(if empty && (self.error.is_none() || !self.blobs.is_empty()) {
                self.render_empty(cx)
            } else if empty {
                div().flex_1().into_any_element()
            } else {
                div().flex_1().min_h_0().pt(S4).flex().flex_col().child(self.render_grid(cx)).into_any_element()
            })
    }
}
