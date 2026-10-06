//! Library: the EPUB catalog as a wall of covers, grouped by collection.
//!
//! Covers come from the OPDS cover route (thumbnails where the server made
//! them), decoded off the UI thread into a bounded LRU with at most a few
//! fetches in flight. Uploads are resumable (tus): every in-flight upload is
//! remembered on disk, so one interrupted by a quit, a crash or an outage is
//! listed again on the next launch and continues from the server's offset.

use crate::app::{self, describe, log, Health};
use crate::shell::{confirm, set_health, toast};
use crate::theme::{theme, Theme, FONT_DOC, FONT_MONO, S1, S2, S3, S4, S5};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, Kind as BtnKind};
use crate::workspace::{PaletteItem, Workspace};
use farfield_core::api::ext_uploads::library_uploads::{self as lu, ServerState};
use farfield_core::api::library::{self, Book, CollectionCount};
use farfield_core::upload::{Progress, TusOutcome, TusState, TUS_CHUNK};
use farfield_core::{ApiError, Freshness, Latest};
use gpui::{
    div, img, prelude::*, px, uniform_list, AnyElement, App, Context, Entity, ExternalPaths, Hsla, ObjectFit,
    RenderImage, SharedString, UniformListScrollHandle, Window,
};
use std::collections::{HashMap, VecDeque};
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

const COVER_CAP: usize = 200;
const MAX_FETCHES: usize = 6;
const TILE_W: f32 = 132.;
const TILE_H: f32 = 198.;
const GUTTER: f32 = 28.;
const ROW_H: f32 = TILE_H + 70.;
const SIDE_W: f32 = 232.;

#[derive(Clone, PartialEq)]
enum Filter {
    All,
    Uncategorized,
    Named(String),
}

// ── covers ───────────────────────────────────────────────────────────────

enum Slot {
    Pending,
    Ready(Arc<RenderImage>),
    Missing,
}

/// Decoded covers by CID: a bounded LRU, fetched at most MAX_FETCHES at a
/// time. Evicted images go to `retired` so render can free their atlas space.
#[derive(Default)]
struct Covers {
    slots: HashMap<String, Slot>,
    order: VecDeque<String>,
    queue: VecDeque<String>,
    inflight: usize,
    retired: Vec<Arc<RenderImage>>,
}

impl Covers {
    fn get(&self, cid: &str) -> Option<&Slot> {
        self.slots.get(cid)
    }
    fn ready(&self) -> HashMap<String, Option<Arc<RenderImage>>> {
        self.slots
            .iter()
            .filter_map(|(k, s)| match s {
                Slot::Ready(i) => Some((k.clone(), Some(i.clone()))),
                Slot::Missing => Some((k.clone(), None)),
                Slot::Pending => None,
            })
            .collect()
    }
    fn touch(&mut self, cid: &str) {
        if let Some(i) = self.order.iter().position(|c| c == cid) {
            let c = self.order.remove(i).unwrap();
            self.order.push_back(c);
        }
    }
    fn settle(&mut self, cid: String, slot: Slot) {
        self.slots.insert(cid.clone(), slot);
        self.order.retain(|c| c != &cid);
        self.order.push_back(cid);
        while self.order.len() > COVER_CAP {
            if let Some(old) = self.order.pop_front() {
                if let Some(Slot::Ready(i)) = self.slots.remove(&old) {
                    self.retired.push(i);
                }
            }
        }
    }
}

fn decode(bytes: &[u8], max: u32) -> Option<Arc<RenderImage>> {
    let im = image::load_from_memory(bytes).ok()?;
    let im = if im.width() > max || im.height() > max { im.thumbnail(max, max) } else { im };
    let mut buf = im.to_rgba8();
    // RenderImage wants BGRA
    for p in buf.pixels_mut() {
        p.0.swap(0, 2);
    }
    Some(Arc::new(RenderImage::new(smallvec::smallvec![image::Frame::new(buf)])))
}

fn cover_cid(b: &Book) -> &str {
    if b.thumb_cid.is_empty() {
        &b.cover_cid
    } else {
        &b.thumb_cid
    }
}

// ── uploads ──────────────────────────────────────────────────────────────

#[derive(Clone, PartialEq)]
enum Phase {
    Running,
    /// Stopped (paused, outage, a previous session); resumable.
    Interrupted(String),
    /// The server settled it as not a book.
    Failed(String),
}

struct Up {
    key: String,
    name: String,
    state: TusState,
    progress: Progress,
    phase: Phase,
    /// Set when Cancel (not Pause) was pressed: discard once it stops.
    discard: bool,
    server: Option<ServerState>,
}

pub struct LibraryWs {
    books: Vec<Book>,
    collections: Vec<CollectionCount>,
    uncategorized: i64,
    filter: Filter,
    selected: Option<String>,
    loading: bool,
    loaded: bool,
    freshness: Option<Freshness>,
    error: Option<String>,
    latest: Arc<Latest>,
    search: Entity<TextField>,
    move_to: Entity<TextField>,
    scroll: UniformListScrollHandle,
    covers: Covers,
    uploads: Vec<Up>,
    ticking: bool,
    cols: usize,
}

impl LibraryWs {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        let search = cx.new(|cx| TextField::new(w, cx, "", "Filter  ⌘F"));
        cx.subscribe_in(&search, w, |this: &mut Self, _, e: &FieldEvent, w, cx| match e {
            FieldEvent::Changed => cx.notify(),
            FieldEvent::Down => this.step(1, cx),
            FieldEvent::Up => this.step(-1, cx),
            FieldEvent::Submit => this.move_to.read(cx).focus(w),
            _ => {}
        })
        .detach();
        let move_to = cx.new(|cx| TextField::new(w, cx, "", "Move to collection  ↵"));
        cx.subscribe_in(&move_to, w, |this: &mut Self, f, e: &FieldEvent, _w, cx| {
            if let FieldEvent::Submit = e {
                let name = f.read(cx).text().trim().to_string();
                if !name.is_empty() {
                    this.set_collection(name, cx);
                }
            }
        })
        .detach();
        let mut this = LibraryWs {
            books: Vec::new(),
            collections: Vec::new(),
            uncategorized: 0,
            filter: Filter::All,
            selected: None,
            loading: false,
            loaded: false,
            freshness: None,
            error: None,
            latest: Arc::new(Latest::default()),
            search,
            move_to,
            scroll: UniformListScrollHandle::new(),
            covers: Covers::default(),
            uploads: Vec::new(),
            ticking: false,
            cols: 4,
        };
        this.reload(cx);
        this.load_interrupted(cx);
        this
    }

    fn reload(&mut self, cx: &mut Context<Self>) {
        let ticket = self.latest.ticket();
        let latest = self.latest.clone();
        let s = app::session(cx);
        self.loading = true;
        cx.notify();
        let task = farfield_core::spawn(async move { library::catalog(&s).await });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                if !latest.is_current(ticket) {
                    return;
                }
                this.loading = false;
                match r {
                    Ok(Ok(l)) => {
                        this.books = l.value.books;
                        this.collections = l.value.collections;
                        this.uncategorized = l.value.uncategorized;
                        this.error = None;
                        this.loaded = true;
                        if let Filter::Named(n) = &this.filter {
                            if !this.collections.iter().any(|c| &c.name == n) {
                                this.filter = Filter::All;
                            }
                        }
                        if this.selected.as_ref().is_some_and(|c| !this.books.iter().any(|b| &b.cid == c)) {
                            this.selected = None;
                        }
                        set_health(
                            cx,
                            "library",
                            match &l.freshness {
                                Freshness::Live => Health::Up,
                                Freshness::Stale { error, .. } => Health::Down(error.to_string()),
                            },
                        );
                        this.freshness = Some(l.freshness);
                        log("library-loaded", &[("books", &this.books.len().to_string())]);
                    }
                    Ok(Err(e)) => {
                        if e.is_auth() {
                            set_health(cx, "library", Health::NoAuth);
                        } else if e.is_offline() {
                            set_health(cx, "library", Health::Down(e.to_string()));
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

    // ── covers ──

    fn want(&mut self, cid: String, cx: &mut Context<Self>) {
        if cid.is_empty() {
            return;
        }
        if self.covers.slots.contains_key(&cid) {
            self.covers.touch(&cid);
            return;
        }
        self.covers.slots.insert(cid.clone(), Slot::Pending);
        self.covers.queue.push_back(cid);
        self.pump(cx);
    }

    fn pump(&mut self, cx: &mut Context<Self>) {
        while self.covers.inflight < MAX_FETCHES {
            let Some(cid) = self.covers.queue.pop_front() else { break };
            self.covers.inflight += 1;
            let s = app::session(cx);
            let c = cid.clone();
            let task = farfield_core::spawn(async move { library::cover(&s, &c).await.map(|b| decode(&b, 600)) });
            cx.spawn(async move |this, cx| {
                let r = task.await;
                let _ = this.update(cx, |this, cx| {
                    this.covers.inflight -= 1;
                    let slot = match r {
                        Ok(Ok(Some(i))) => Slot::Ready(i),
                        _ => Slot::Missing,
                    };
                    this.covers.settle(cid, slot);
                    this.pump(cx);
                    cx.notify();
                });
            })
            .detach();
        }
    }

    // ── catalog view ──

    fn visible(&self, cx: &App) -> Vec<Book> {
        let q = self.search.read(cx).text().to_lowercase();
        self.books
            .iter()
            .filter(|b| match &self.filter {
                Filter::All => true,
                Filter::Uncategorized => b.collection.is_empty(),
                Filter::Named(n) => &b.collection == n,
            })
            .filter(|b| {
                q.is_empty()
                    || b.title.to_lowercase().contains(&q)
                    || b.author.to_lowercase().contains(&q)
                    || b.filename.to_lowercase().contains(&q)
            })
            .cloned()
            .collect()
    }

    fn selected_book(&self) -> Option<&Book> {
        let c = self.selected.as_ref()?;
        self.books.iter().find(|b| &b.cid == c)
    }

    fn select(&mut self, cid: String, cx: &mut Context<Self>) {
        if self.selected.as_deref() != Some(&cid) {
            self.move_to.update(cx, |f, cx| f.set_text("", cx));
        }
        self.selected = Some(cid);
        cx.notify();
    }

    fn step(&mut self, by: i32, cx: &mut Context<Self>) {
        let books = self.visible(cx);
        if books.is_empty() {
            return;
        }
        let cur = self.selected.as_ref().and_then(|c| books.iter().position(|b| &b.cid == c));
        let next = match cur {
            None => 0,
            Some(i) => (i as i32 + by).clamp(0, books.len() as i32 - 1) as usize,
        };
        self.scroll.scroll_to_item(next / self.cols.max(1), gpui::ScrollStrategy::Center);
        self.select(books[next].cid.clone(), cx);
    }

    fn set_filter(&mut self, f: Filter, cx: &mut Context<Self>) {
        self.filter = f;
        self.scroll.scroll_to_item(0, gpui::ScrollStrategy::Top);
        cx.notify();
    }

    // ── mutations ──

    fn set_collection(&mut self, name: String, cx: &mut Context<Self>) {
        let Some(b) = self.selected_book().cloned() else { return };
        if b.collection == name {
            return;
        }
        let s = app::session(cx);
        let (cid, n) = (b.cid.clone(), name.clone());
        let task = farfield_core::spawn(async move { library::set_collection(&s, &cid, &n).await });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| match r {
                Ok(Ok(_)) => {
                    log("library-move", &[("cid", &b.cid), ("collection", &name)]);
                    this.move_to.update(cx, |f, cx| f.set_text("", cx));
                    let label = if name.is_empty() { "Uncategorized".to_string() } else { format!("“{name}”") };
                    toast(cx, format!("Moved “{}” to {label}.", b.title), false);
                    this.reload(cx);
                }
                Ok(Err(e)) => toast(cx, describe(&e), true),
                Err(e) => toast(cx, e.to_string(), true),
            });
        })
        .detach();
    }

    fn delete(&mut self, cx: &mut Context<Self>) {
        let Some(b) = self.selected_book().cloned() else { return };
        let ent = cx.entity();
        let title = if b.title.is_empty() { b.filename.clone() } else { b.title.clone() };
        confirm(
            cx,
            "Delete this book?",
            format!("“{title}” — can't be undone."),
            "Delete",
            true,
            move |_, cx| {
                let s = app::session(cx);
                let cid = b.cid.clone();
                let task = farfield_core::spawn(async move { library::delete(&s, &cid).await });
                ent.update(cx, |_, cx| {
                    cx.spawn(async move |this, cx| {
                        let r = task.await;
                        let _ = this.update(cx, |this, cx| match r {
                            Ok(Ok(())) | Ok(Err(ApiError::NotFound)) => {
                                log("library-delete", &[("cid", &b.cid)]);
                                this.selected = None;
                                toast(cx, format!("Deleted “{title}”."), false);
                                this.reload(cx);
                            }
                            Ok(Err(e)) => toast(cx, describe(&e), true),
                            Err(e) => toast(cx, e.to_string(), true),
                        });
                    })
                    .detach();
                });
            },
        );
    }

    // ── uploads ──

    fn load_interrupted(&mut self, cx: &mut Context<Self>) {
        let s = app::session(cx);
        let task = farfield_core::spawn(async move {
            let mut out = Vec::new();
            for p in lu::interrupted(&s) {
                let server = match &p.state.location {
                    Some(loc) => lu::server_state(&s, loc).await.ok().flatten(),
                    None => None,
                };
                out.push((p, server));
            }
            out
        });
        cx.spawn(async move |this, cx| {
            let Ok(list) = task.await else { return };
            let _ = this.update(cx, |this, cx| {
                let mut finished = false;
                for (p, server) in list {
                    if this.uploads.iter().any(|u| u.key == p.key) {
                        continue;
                    }
                    // settled while we were away: nothing left to resume
                    if server.as_ref().is_some_and(|s| s.status == "done") {
                        lu::forget(&app::session(cx), &p.key);
                        finished = true;
                        continue;
                    }
                    let name = p.state.file.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default();
                    let progress = Progress::new(p.state.size);
                    let why = if !p.state.file.exists() {
                        "File missing from this Mac.".to_string()
                    } else {
                        "Interrupted.".to_string()
                    };
                    log("library-interrupted", &[("file", &name)]);
                    this.uploads.push(Up {
                        key: p.key,
                        name,
                        state: p.state,
                        progress,
                        phase: Phase::Interrupted(why),
                        discard: false,
                        server,
                    });
                }
                if finished {
                    this.reload(cx);
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn pick(&mut self, w: &mut Window, cx: &mut Context<Self>) {
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
        let collection = match &self.filter {
            Filter::Named(n) => n.clone(),
            _ => String::new(),
        };
        let mut skipped = Vec::new();
        for p in paths {
            let name = p.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default();
            if !p.extension().and_then(|e| e.to_str()).is_some_and(|e| e.eq_ignore_ascii_case("epub")) {
                skipped.push(name);
                continue;
            }
            let key = lu::key_for(&p);
            if let Some(u) = self.uploads.iter().position(|u| u.key == key) {
                match &self.uploads[u].phase {
                    Phase::Running => continue,
                    // the same file again: continue what the server has
                    Phase::Interrupted(_) if self.uploads[u].state.file_unchanged() => {
                        self.start(u, cx);
                        continue;
                    }
                    _ => {}
                }
                // changed or failed: drop the old partial before starting over
                let old = self.uploads.remove(u);
                let s = app::session(cx);
                farfield_core::spawn(async move { lu::drop_partial(&s, &old.state).await });
            }
            let state = match TusState::new(p.clone(), &collection) {
                Ok(s) => s,
                Err(e) => {
                    toast(cx, format!("{name}: {e}"), true);
                    continue;
                }
            };
            let progress = Progress::new(state.size);
            self.uploads.push(Up {
                key,
                name,
                state,
                progress,
                phase: Phase::Interrupted(String::new()),
                discard: false,
                server: None,
            });
            let i = self.uploads.len() - 1;
            self.start(i, cx);
        }
        if !skipped.is_empty() {
            toast(cx, format!("Skipped (not EPUB): {}.", skipped.join(", ")), true);
        }
        cx.notify();
    }

    fn start(&mut self, i: usize, cx: &mut Context<Self>) {
        let Some(u) = self.uploads.get_mut(i) else { return };
        if u.phase == Phase::Running {
            return;
        }
        u.progress = Progress::new(u.state.size);
        u.phase = Phase::Running;
        u.discard = false;
        let (s, mut st, pr, key, name) =
            (app::session(cx), u.state.clone(), u.progress.clone(), u.key.clone(), u.name.clone());
        log("library-upload-start", &[("file", &name)]);
        let task = farfield_core::spawn(async move {
            let r = lu::run_chunked(&s, &mut st, &pr, TUS_CHUNK).await;
            (st, r)
        });
        cx.spawn(async move |this, cx| {
            let Ok((st, r)) = task.await else { return };
            let _ = this.update(cx, |this, cx| this.finished(key, st, r, cx));
        })
        .detach();
        self.tick(cx);
    }

    fn finished(&mut self, key: String, st: TusState, r: Result<TusOutcome, ApiError>, cx: &mut Context<Self>) {
        let Some(i) = self.uploads.iter().position(|u| u.key == key) else { return };
        self.uploads[i].state = st.clone();
        let name = self.uploads[i].name.clone();
        match r {
            Ok(TusOutcome::Done { cid }) => {
                self.uploads.remove(i);
                log("library-upload-done", &[("file", &name), ("cid", &cid)]);
                toast(cx, format!("“{name}” is in the library."), false);
                self.selected = Some(cid);
                self.reload(cx);
            }
            Ok(TusOutcome::Failed(msg)) => {
                log("library-upload-failed", &[("file", &name), ("error", &msg)]);
                self.uploads[i].phase = Phase::Failed(if msg.is_empty() { "Not a readable EPUB.".into() } else { msg });
            }
            Err(ApiError::Cancelled) if self.uploads[i].discard => {
                self.uploads.remove(i);
                self.discard_state(st, cx);
            }
            Err(ApiError::Cancelled) => {
                log("library-upload-paused", &[("file", &name)]);
                self.uploads[i].phase = Phase::Interrupted("Paused.".into());
            }
            Err(e) => {
                if e.is_auth() {
                    set_health(cx, "library", Health::NoAuth);
                } else if e.is_offline() || matches!(e, ApiError::Uncertain(_)) {
                    set_health(cx, "library", Health::Down(e.to_string()));
                }
                log("library-upload-interrupted", &[("file", &name), ("error", &e.to_string())]);
                let why = if matches!(e, ApiError::Offline(_) | ApiError::Uncertain(_)) {
                    "Connection dropped.".to_string()
                } else {
                    describe(&e)
                };
                self.uploads[i].phase = Phase::Interrupted(why);
            }
        }
        cx.notify();
    }

    fn discard_state(&mut self, st: TusState, cx: &mut Context<Self>) {
        let s = app::session(cx);
        let name = st.file.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default();
        let task = farfield_core::spawn(async move { lu::discard(&s, &st).await });
        cx.spawn(async move |_, cx| {
            let r = task.await;
            let _ = cx.update(|cx| match r {
                Ok(Ok(())) => {
                    log("library-upload-discarded", &[("file", &name)]);
                    toast(cx, format!("Upload of {name} cancelled."), false)
                }
                Ok(Err(e)) => toast(cx, format!("Cancelled; server kept the partial upload. {}", describe(&e)), true),
                Err(e) => toast(cx, e.to_string(), true),
            });
        })
        .detach();
    }

    fn cancel(&mut self, key: &str, cx: &mut Context<Self>) {
        let Some(i) = self.uploads.iter().position(|u| u.key == key) else { return };
        if self.uploads[i].phase == Phase::Running {
            self.uploads[i].discard = true;
            self.uploads[i].progress.cancel();
        } else {
            let u = self.uploads.remove(i);
            if matches!(u.phase, Phase::Failed(_)) {
                lu::forget(&app::session(cx), &u.key);
            } else {
                self.discard_state(u.state, cx);
            }
        }
        cx.notify();
    }

    fn resume_all(&mut self, cx: &mut Context<Self>) {
        for i in 0..self.uploads.len() {
            if matches!(self.uploads[i].phase, Phase::Interrupted(_)) && self.uploads[i].state.file.exists() {
                self.start(i, cx);
            }
        }
    }

    /// Repaint progress while anything is running.
    fn tick(&mut self, cx: &mut Context<Self>) {
        if self.ticking {
            return;
        }
        self.ticking = true;
        cx.spawn(async move |this, cx| loop {
            cx.background_executor().timer(Duration::from_millis(150)).await;
            let go = this.update(cx, |this, cx| {
                cx.notify();
                let any = this.uploads.iter().any(|u| u.phase == Phase::Running);
                if !any {
                    this.ticking = false;
                }
                any
            });
            if !matches!(go, Ok(true)) {
                break;
            }
        })
        .detach();
    }

    // ── rendering ──

    fn render_side(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let total = self.books.len() as i64;
        let row = |id: SharedString, label: String, n: i64, on: bool, f: Filter, cx: &mut Context<Self>| {
            div()
                .id(id)
                .flex()
                .items_center()
                .justify_between()
                .gap(S2)
                .px(S4)
                .py(px(6.))
                .cursor_pointer()
                .text_sm()
                .when(on, |d| d.bg(t.accent_soft).border_l_2().border_color(t.accent).text_color(t.ink))
                .when(!on, |d| d.text_color(t.ink_2).hover(|s| s.bg(t.wash)))
                .child(div().truncate().child(label))
                .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(n.to_string()))
                .on_click(cx.listener(move |this, _, _, cx| this.set_filter(f.clone(), cx)))
        };
        let mut col = div()
            .flex()
            .flex_col()
            .size_full()
            .child(div().h(S4))
            .child(div().px(S4).pb(S1).child(ui::eyebrow("Collections", cx)))
            .child(row("f-all".into(), "All books".into(), total, self.filter == Filter::All, Filter::All, cx));
        for c in &self.collections {
            let on = self.filter == Filter::Named(c.name.clone());
            col = col.child(row(
                SharedString::from(format!("f-{}", c.name)),
                c.name.clone(),
                c.count,
                on,
                Filter::Named(c.name.clone()),
                cx,
            ));
        }
        col = col.child(row(
            "f-unc".into(),
            "Uncategorized".into(),
            self.uncategorized,
            self.filter == Filter::Uncategorized,
            Filter::Uncategorized,
            cx,
        ));
        col.into_any_element()
    }

    fn render_uploads(&self, cx: &mut Context<Self>) -> Option<AnyElement> {
        if self.uploads.is_empty() {
            return None;
        }
        let t = theme(cx).clone();
        let resumable =
            self.uploads.iter().filter(|u| matches!(u.phase, Phase::Interrupted(_)) && u.state.file.exists()).count();
        let running = self.uploads.iter().filter(|u| u.phase == Phase::Running).count();
        let head = if running > 0 {
            format!("Uploading {running} {}", if running == 1 { "book" } else { "books" })
        } else if resumable > 0 {
            format!("{resumable} interrupted {}", if resumable == 1 { "upload" } else { "uploads" })
        } else {
            "Uploads".into()
        };
        let mut d = div().flex().flex_col().px(S5).pt(S4).pb(S3).gap(S2).border_b_1().border_color(t.rule).child(
            div().flex().items_center().justify_between().child(ui::eyebrow(head, cx)).when(resumable > 1, |d| {
                d.child(ui::button(
                    "resume-all",
                    "Resume all",
                    BtnKind::Quiet,
                    cx,
                    cx.listener(|this, _, _, cx| this.resume_all(cx)),
                ))
            }),
        );
        for u in &self.uploads {
            let total = u.state.size.max(1);
            let (frac, status, color): (f32, String, Hsla) = match &u.phase {
                Phase::Running => {
                    let f = u.progress.fraction();
                    if u.progress.total() > 0 && u.progress.sent() >= u.progress.total() {
                        (1.0, "Finalizing…".into(), t.accent)
                    } else {
                        (f, format!("{} of {}", ui::bytes(u.progress.sent() as i64), ui::bytes(total as i64)), t.accent)
                    }
                }
                Phase::Interrupted(why) => {
                    let have = u.server.as_ref().map(|s| s.offset).unwrap_or_else(|| u.progress.sent());
                    let f = have as f32 / total as f32;
                    let mut s = why.clone();
                    if have > 0 {
                        s = format!("{} {} of {} on the server.", s, ui::bytes(have as i64), ui::bytes(total as i64))
                            .trim()
                            .to_string();
                    }
                    (f, s, t.signal)
                }
                Phase::Failed(msg) => (1.0, msg.clone(), t.bad),
            };
            let key = u.key.clone();
            let key2 = u.key.clone();
            let key3 = u.key.clone();
            let mut actions = div().flex().gap(S1);
            match &u.phase {
                Phase::Running => {
                    let p = u.progress.clone();
                    actions = actions
                        .child(ui::button(
                            SharedString::from(format!("pause-{key}")),
                            "Pause",
                            BtnKind::Quiet,
                            cx,
                            move |_, _, _| p.cancel(),
                        ))
                        .child(ui::button(
                            SharedString::from(format!("cancel-{key}")),
                            "Cancel",
                            BtnKind::Quiet,
                            cx,
                            cx.listener(move |this, _, _, cx| this.cancel(&key, cx)),
                        ));
                }
                Phase::Interrupted(_) => {
                    if u.state.file.exists() {
                        actions = actions.child(ui::button(
                            SharedString::from(format!("resume-{key}")),
                            "Resume",
                            BtnKind::Quiet,
                            cx,
                            cx.listener(move |this, _, _, cx| {
                                if let Some(i) = this.uploads.iter().position(|u| u.key == key2) {
                                    this.start(i, cx);
                                }
                            }),
                        ));
                    }
                    actions = actions.child(ui::button(
                        SharedString::from(format!("discard-{key}")),
                        "Discard",
                        BtnKind::Quiet,
                        cx,
                        cx.listener(move |this, _, _, cx| this.cancel(&key3, cx)),
                    ));
                }
                Phase::Failed(_) => {
                    actions = actions.child(ui::button(
                        SharedString::from(format!("dismiss-{key}")),
                        "Dismiss",
                        BtnKind::Quiet,
                        cx,
                        cx.listener(move |this, _, _, cx| this.cancel(&key, cx)),
                    ));
                }
            }
            d = d.child(
                div()
                    .flex()
                    .items_center()
                    .gap(S4)
                    .child(
                        div()
                            .flex_1()
                            .min_w_0()
                            .flex()
                            .flex_col()
                            .gap(px(5.))
                            .child(
                                div()
                                    .flex()
                                    .justify_between()
                                    .gap(S3)
                                    .child(div().text_sm().text_color(t.ink).truncate().child(u.name.clone()))
                                    .child(
                                        div()
                                            .font_family(FONT_MONO)
                                            .text_xs()
                                            .text_color(t.ink_3)
                                            .child(format!("{}%", (frac * 100.0) as u32)),
                                    ),
                            )
                            .child(
                                div()
                                    .w_full()
                                    .h(px(2.))
                                    .bg(t.rule)
                                    .child(div().h_full().w(gpui::relative(frac.clamp(0.0, 1.0))).bg(color)),
                            )
                            .child(div().text_xs().text_color(t.ink_2).truncate().child(status)),
                    )
                    .child(actions),
            );
        }
        Some(d.into_any_element())
    }

    fn render_grid(&mut self, w: &mut Window, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        // columns from the space this pane gets
        let prefs = app::state(cx).prefs.clone();
        let mut avail = f32::from(w.viewport_size().width) - SIDE_W - 2. * f32::from(S5);
        if prefs.nav_open {
            avail -= prefs.nav_width;
        }
        if prefs.inspector_open {
            avail -= prefs.inspector_width;
        }
        let cols = (((avail + GUTTER) / (TILE_W + GUTTER)).floor() as usize).max(1);
        self.cols = cols;
        let books = Arc::new(self.visible(cx));
        let n = books.len();
        let rows = n.div_ceil(cols);
        let ready = Arc::new(self.covers.ready());
        let selected = self.selected.clone();
        let ent = cx.entity();
        let list = uniform_list("library-grid", rows, move |range, _w, cx| {
            let t = theme(cx).clone();
            let mut missing = Vec::new();
            let out = range
                .map(|r| {
                    let mut row = div().h(px(ROW_H)).flex().gap(px(GUTTER)).px(S5).pt(S4);
                    for b in books.iter().skip(r * cols).take(cols) {
                        let cc = cover_cid(b).to_string();
                        let image = ready.get(&cc).cloned();
                        if image.is_none() && !cc.is_empty() {
                            missing.push(cc);
                        }
                        let on = selected.as_deref() == Some(b.cid.as_str());
                        let e = ent.clone();
                        let cid = b.cid.clone();
                        row = row.child(
                            tile(b, image.flatten(), on, &t)
                                .on_click(move |_, _, cx| e.update(cx, |this, cx| this.select(cid.clone(), cx))),
                        );
                    }
                    row.into_any_element()
                })
                .collect();
            if !missing.is_empty() {
                let e = ent.clone();
                cx.defer(move |cx| {
                    e.update(cx, |this, cx| {
                        for c in missing {
                            this.want(c, cx);
                        }
                    })
                });
            }
            out
        })
        .track_scroll(self.scroll.clone())
        .flex_1();
        if n == 0 {
            let msg = if self.loading && !self.loaded {
                "Loading…".to_string()
            } else if self.error.is_some() {
                String::new()
            } else if !self.search.read(cx).text().is_empty() {
                "No matches.".into()
            } else if self.books.is_empty() {
                "No books yet.".into()
            } else {
                "No books in this collection.".into()
            };
            return div()
                .flex_1()
                .child(div().max_w(px(520.)).child(ui::quiet_state(msg, cx).text_color(t.ink_2)))
                .into_any_element();
        }
        list.into_any_element()
    }
}

/// One book on the wall: the cover (or a typeset stand-in), title, author.
fn tile(b: &Book, image: Option<Arc<RenderImage>>, on: bool, t: &Theme) -> gpui::Stateful<gpui::Div> {
    let title = if b.title.is_empty() { b.filename.clone() } else { b.title.clone() };
    let cover: AnyElement = match image {
        Some(i) => img(i).w(px(TILE_W)).h(px(TILE_H)).object_fit(ObjectFit::Cover).rounded(px(2.)).into_any_element(),
        None => stand_in(&title, &b.author, TILE_W, TILE_H, t).into_any_element(),
    };
    let ink = t.ink;
    div()
        .id(SharedString::from(format!("book-{}", b.cid)))
        .w(px(TILE_W))
        .flex()
        .flex_col()
        .gap(px(6.))
        .cursor_pointer()
        .child(div().rounded(px(3.)).p(px(2.)).when(on, |d| d.bg(t.accent)).child(cover))
        .child(
            div()
                .flex()
                .flex_col()
                .child(
                    div()
                        .text_sm()
                        .line_height(px(17.))
                        .text_color(if on { t.accent } else { t.ink })
                        .truncate()
                        .child(title),
                )
                .child(div().text_xs().text_color(t.ink_3).truncate().child(if b.author.is_empty() {
                    "—".to_string()
                } else {
                    b.author.clone()
                })),
        )
        .hover(move |s| s.text_color(ink))
}

/// A cover for a book without one: a calm, title-tinted field with the title
/// set in the document face.
fn stand_in(title: &str, author: &str, w: f32, h: f32, t: &Theme) -> gpui::Div {
    let hue = title.bytes().fold(7u32, |a, b| a.wrapping_mul(31).wrapping_add(b as u32)) % 360;
    let bg = gpui::hsla(hue as f32 / 360., 0.16, if t.dark { 0.20 } else { 0.88 }, 1.);
    let fg = gpui::hsla(hue as f32 / 360., 0.25, if t.dark { 0.82 } else { 0.24 }, 1.);
    div()
        .w(px(w))
        .h(px(h))
        .rounded(px(2.))
        .bg(bg)
        .flex()
        .flex_col()
        .justify_between()
        .p(px(12.))
        .child(
            div()
                .font_family(FONT_DOC)
                .text_size(px(if w > 200. { 22. } else { 15. }))
                .line_height(px(if w > 200. { 26. } else { 18. }))
                .text_color(fg)
                .child(title.to_string()),
        )
        .child(div().h(px(1.)).w(px(24.)).bg(fg))
        .child(div().text_xs().text_color(fg).opacity(0.8).child(author.to_string()))
}

impl Workspace for LibraryWs {
    fn inspector(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> Option<AnyElement> {
        let t = theme(cx).clone();
        let b = self.selected_book()?.clone();
        // the full cover for the hero
        let hero_cid = if b.cover_cid.is_empty() { cover_cid(&b).to_string() } else { b.cover_cid.clone() };
        let hero = match self.covers.get(&hero_cid) {
            Some(Slot::Ready(i)) => Some(i.clone()),
            Some(_) => None,
            None => {
                if !hero_cid.is_empty() {
                    let ent = cx.entity();
                    let c = hero_cid.clone();
                    cx.defer(move |cx| ent.update(cx, |this, cx| this.want(c, cx)));
                }
                None
            }
        };
        let title = if b.title.is_empty() { b.filename.clone() } else { b.title.clone() };
        let mut col = div().flex().flex_col().gap(S2);
        col = col.child(div().flex().justify_center().py(S2).child(match hero {
            Some(i) => img(i).w(px(220.)).h(px(320.)).object_fit(ObjectFit::Contain).into_any_element(),
            None => stand_in(&title, &b.author, 220., 320., &t).into_any_element(),
        }));
        col = col.child(ui::doc_title(title.clone(), cx).text_size(px(20.)));
        if !b.author.is_empty() {
            col = col.child(div().text_sm().text_color(t.ink_2).child(b.author.clone()));
        }
        if !b.description.is_empty() {
            col = col.child(
                div()
                    .font_family(FONT_DOC)
                    .text_sm()
                    .text_color(t.ink_2)
                    .max_h(px(140.))
                    .overflow_hidden()
                    .child(b.description.clone()),
            );
        }
        col = col.child(ui::rule(cx)).child(ui::eyebrow("Collection", cx));
        col = col.child(div().text_sm().text_color(t.ink).child(if b.collection.is_empty() {
            "Uncategorized".to_string()
        } else {
            b.collection.clone()
        }));
        let mut chips = div().flex().flex_wrap().gap(px(6.));
        for c in self.collections.iter().filter(|c| c.name != b.collection) {
            let name = c.name.clone();
            chips = chips.child(
                div()
                    .id(SharedString::from(format!("mv-{}", c.name)))
                    .px(px(8.))
                    .py(px(2.))
                    .rounded(px(10.))
                    .border_1()
                    .border_color(t.rule_strong)
                    .text_xs()
                    .text_color(t.ink_2)
                    .cursor_pointer()
                    .hover(|s| s.text_color(t.ink).bg(t.wash))
                    .child(format!("→ {}", c.name))
                    .on_click(cx.listener(move |this, _, _, cx| this.set_collection(name.clone(), cx))),
            );
        }
        if !b.collection.is_empty() {
            chips = chips.child(
                div()
                    .id("mv-unc")
                    .px(px(8.))
                    .py(px(2.))
                    .text_xs()
                    .text_color(t.ink_3)
                    .cursor_pointer()
                    .hover(|s| s.text_color(t.ink))
                    .child("→ Uncategorized")
                    .on_click(cx.listener(|this, _, _, cx| this.set_collection(String::new(), cx))),
            );
        }
        col = col.child(chips).child(self.move_to.clone());
        col = col.child(ui::rule(cx));
        let fields: Vec<(&str, String)> = vec![
            ("Language", b.language.clone()),
            ("Identifier", b.identifier.clone()),
            ("Size", ui::bytes(b.size)),
            ("Filename", b.filename.clone()),
            ("Added", ui::when(&b.created_at)),
            ("CID", b.cid.clone()),
        ];
        for (k, v) in fields.into_iter().filter(|(_, v)| !v.is_empty()) {
            col = col.child(ui::field_row(k, ui::mono(v, cx), cx));
        }
        let cid = b.cid.clone();
        col = col.child(
            div()
                .flex()
                .gap(S2)
                .pt(S2)
                .child(ui::button("copy-cid", "Copy CID", BtnKind::Quiet, cx, move |_, _, cx| {
                    cx.write_to_clipboard(gpui::ClipboardItem::new_string(cid.clone()));
                    toast(cx, "CID copied.", false);
                }))
                .child(div().flex_1())
                .child(ui::button(
                    "delete-book",
                    "Delete…",
                    BtnKind::Danger,
                    cx,
                    cx.listener(|this, _, _, cx| this.delete(cx)),
                )),
        );
        Some(col.into_any_element())
    }

    fn palette(&self, _cx: &App) -> Vec<PaletteItem> {
        vec![PaletteItem::new("Library: upload EPUBs…", "⌘N", |_, cx| crate::shell::goto(cx, "library"))]
    }

    fn commands(&self, _cx: &App) -> Vec<(&'static str, String, &'static str)> {
        let mut v = vec![("upload", "Library: upload EPUBs…".to_string(), "⌘N")];
        if self.uploads.iter().any(|u| matches!(u.phase, Phase::Interrupted(_))) {
            v.push(("resume", "Library: resume interrupted uploads".into(), ""));
        }
        if let Some(b) = self.selected_book() {
            v.push(("move", "Library: move to collection…".into(), "↵"));
            if !b.collection.is_empty() {
                v.push(("uncategorize", "Library: move to Uncategorized".into(), ""));
            }
            v.push(("copy-cid", "Library: copy CID".into(), ""));
            v.push(("delete", "Library: delete book…".into(), ""));
        }
        v
    }

    fn run_command(&mut self, id: &str, w: &mut Window, cx: &mut Context<Self>) {
        match id {
            "upload" => self.pick(w, cx),
            "resume" => self.resume_all(cx),
            "move" => self.move_to.read(cx).focus(w),
            "uncategorize" => self.set_collection(String::new(), cx),
            "copy-cid" => {
                if let Some(b) = self.selected_book() {
                    cx.write_to_clipboard(gpui::ClipboardItem::new_string(b.cid.clone()));
                    toast(cx, "CID copied.", false);
                }
            }
            "delete" => self.delete(cx),
            _ => {}
        }
    }

    fn focus_search(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        self.search.read(cx).focus(w);
    }
    fn new_item(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        self.pick(w, cx)
    }
    fn refresh(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        self.reload(cx);
        self.load_interrupted(cx);
    }
    fn dirty(&self, _cx: &App) -> bool {
        self.uploads.iter().any(|u| u.phase == Phase::Running)
    }
}

impl Render for LibraryWs {
    fn render(&mut self, w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = theme(cx).clone();
        for old in self.covers.retired.drain(..) {
            let _ = w.drop_image(old);
        }
        let heading = match &self.filter {
            Filter::All => "All books".to_string(),
            Filter::Uncategorized => "Uncategorized".into(),
            Filter::Named(n) => n.clone(),
        };
        let count = self.visible(cx).len();
        let status_line = match (&self.error, &self.freshness) {
            (Some(e), _) => Some(ui::notice(e.clone(), t.bad, cx)),
            (None, Some(Freshness::Stale { age_ms, .. })) => Some(ui::notice(
                format!("Offline — showing what was loaded {} ago.", crate::ws::content::ago(*age_ms)),
                t.warn,
                cx,
            )),
            _ => None,
        };
        let uploads = self.render_uploads(cx);
        let grid = self.render_grid(w, cx);
        let wash = t.wash;
        div()
            .id("library")
            .size_full()
            .flex()
            .on_action(cx.listener(|this, _: &crate::ui::doc_editor::DropPending, _, cx| {
                if let Some(p) = cx.try_global::<crate::evidence::PendingDrop>().map(|p| p.0.clone()) {
                    cx.remove_global::<crate::evidence::PendingDrop>();
                    this.upload(p, cx);
                }
            }))
            .drag_over::<ExternalPaths>(move |s, _, _, _| s.bg(wash))
            .on_drop(cx.listener(|this, paths: &ExternalPaths, _, cx| this.upload(paths.paths().to_vec(), cx)))
            .child(
                div().w(px(SIDE_W)).flex_none().h_full().border_r_1().border_color(t.rule).child(self.render_side(cx)),
            )
            .child(
                div()
                    .flex_1()
                    .min_w_0()
                    .h_full()
                    .flex()
                    .flex_col()
                    .child(
                        div()
                            .flex()
                            .items_end()
                            .justify_between()
                            .gap(S4)
                            .px(S5)
                            .pt(S4)
                            .pb(S3)
                            .border_b_1()
                            .border_color(t.rule)
                            .child(
                                div()
                                    .flex()
                                    .items_baseline()
                                    .gap(S3)
                                    .child(
                                        div()
                                            .text_size(px(20.))
                                            .font_weight(gpui::FontWeight::MEDIUM)
                                            .text_color(t.ink)
                                            .child(heading),
                                    )
                                    .child(
                                        div()
                                            .font_family(FONT_MONO)
                                            .text_xs()
                                            .text_color(t.ink_3)
                                            .child(format!("{count} {}", if count == 1 { "book" } else { "books" })),
                                    ),
                            )
                            .child(div().w(px(240.)).flex().flex_col().child(self.search.clone()))
                            .child(ui::button(
                                "upload",
                                "Upload EPUBs…  ⌘N",
                                BtnKind::Primary,
                                cx,
                                cx.listener(|this, _, w, cx| this.pick(w, cx)),
                            )),
                    )
                    .when_some(status_line, |d, s| d.child(div().px(S5).py(S2).child(s)))
                    .when_some(uploads, |d, u| d.child(u))
                    .child(grid),
            )
    }
}
