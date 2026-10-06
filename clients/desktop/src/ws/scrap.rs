//! Scrap: every paste, paged, from the admin API — reading one counts no
//! view and needs no token. A paste's body is fixed once made (its id is
//! derived from it), so an open paste is read-only: title, language,
//! visibility and expiry change from the inspector. Magic-link tokens are
//! stored hashed on the server, so a new one is shown exactly once, here.

use super::bookmarks::kit;
use crate::app::{self, describe, log, Health};
use crate::shell::{confirm, set_health, toast};
use crate::theme::{theme, FONT_DOC, FONT_MONO, S1, S2, S3, S4, S5, S6};
use crate::ui::doc_editor::{DocEditor, DocEvent};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, Kind as BtnKind};
use crate::workspace::{PaletteItem, Workspace};
use farfield_core::api::ext_lists::scrap as xscrap;
use farfield_core::api::scrap::{self, Paste, EXPIRIES, VISIBILITIES};
use farfield_core::{ApiError, Freshness, Latest};
use gpui::{
    div, prelude::*, px, uniform_list, AnyElement, App, Context, Entity, ScrollHandle, SharedString,
    UniformListScrollHandle, WeakEntity, Window,
};
use serde_json::{Map, Value};
use std::sync::Arc;

const SVC: &str = "scrap";
const PAGE: u32 = 50;
const EXPIRY_LABELS: [&str; 5] = ["Never", "1 hour", "1 day", "1 week", "1 month"];

// one selection per workspace; size is irrelevant
#[allow(clippy::large_enum_variant)]
enum Sel {
    None,
    /// An existing paste; `full` arrives with its body.
    Paste {
        id: String,
        full: Option<Paste>,
        error: Option<String>,
    },
    New,
}

/// The new-paste form.
struct Compose {
    editor: Entity<DocEditor>,
    title: Entity<TextField>,
    lang: Entity<TextField>,
    vis: usize,
    exp: usize,
    magic: bool,
    saving: bool,
    error: Option<String>,
    /// Lines of body written so far (0 = empty), kept from change events.
    lines: usize,
}

/// Metadata edits for the open paste (inspector).
struct Meta {
    title: Entity<TextField>,
    lang: Entity<TextField>,
    vis: usize,
    /// None = keep the current expiry.
    exp: Option<usize>,
    saving: bool,
}

pub struct ScrapWs {
    me: WeakEntity<Self>,
    pastes: Vec<Paste>,
    total: i64,
    page: u32,
    loading: bool,
    loaded: bool,
    freshness: Option<Freshness>,
    error: Option<String>,
    latest: Arc<Latest>,
    search: Entity<TextField>,
    scroll: ScrollHandle,
    body_scroll: UniformListScrollHandle,
    sel: Sel,
    compose: Option<Compose>,
    meta: Meta,
    /// (paste id, token) right after a create or rotate — shown once,
    /// kept in memory only, gone when another paste is opened.
    fresh: Option<(String, String)>,
    busy: bool,
}

fn vis_index(v: &str) -> usize {
    VISIBILITIES.iter().position(|x| *x == v).unwrap_or(1)
}

impl ScrapWs {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        let search = cx.new(|cx| TextField::new(w, cx, "", "Filter  ⌘F"));
        cx.subscribe_in(&search, w, |this: &mut Self, _, e: &FieldEvent, _w, cx| match e {
            FieldEvent::Changed => cx.notify(),
            FieldEvent::Down => this.step(1, cx),
            FieldEvent::Up => this.step(-1, cx),
            _ => {}
        })
        .detach();
        let field = |label: &str, ph: &str, mono: bool, w: &mut Window, cx: &mut Context<Self>| {
            let (label, ph) = (label.to_string(), ph.to_string());
            let f = cx.new(|cx| {
                let f = TextField::new(w, cx, label, ph);
                if mono {
                    f.mono()
                } else {
                    f
                }
            });
            cx.subscribe(&f, |_this: &mut Self, _, e: &FieldEvent, cx| {
                if *e == FieldEvent::Changed {
                    cx.notify()
                }
            })
            .detach();
            f
        };
        let meta = Meta {
            title: field("Title", "untitled", false, w, cx),
            lang: field("Language", "plain text", true, w, cx),
            vis: 1,
            exp: None,
            saving: false,
        };
        let mut this = ScrapWs {
            me: cx.entity().downgrade(),
            pastes: Vec::new(),
            total: 0,
            page: 1,
            loading: false,
            loaded: false,
            freshness: None,
            error: None,
            latest: Arc::new(Latest::default()),
            search,
            scroll: ScrollHandle::new(),
            body_scroll: UniformListScrollHandle::new(),
            sel: Sel::None,
            compose: None,
            meta,
            fresh: None,
            busy: false,
        };
        this.load(false, cx);
        this
    }

    fn reload(&mut self, cx: &mut Context<Self>) {
        self.page = 1;
        self.load(false, cx);
    }

    fn load(&mut self, append: bool, cx: &mut Context<Self>) {
        let ticket = self.latest.ticket();
        let latest = self.latest.clone();
        let s = app::session(cx);
        let page = self.page;
        self.loading = true;
        cx.notify();
        let task = farfield_core::spawn(async move { scrap::list(&s, page, PAGE).await });
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
                            this.pastes.clear();
                        }
                        for p in l.value.pastes {
                            if !this.pastes.iter().any(|x| x.id == p.id) {
                                this.pastes.push(p);
                            }
                        }
                        this.total = l.value.total;
                        this.loaded = true;
                        this.error = None;
                        set_health(
                            cx,
                            SVC,
                            match &l.freshness {
                                Freshness::Live => Health::Up,
                                Freshness::Stale { error, .. } => Health::Down(error.to_string()),
                            },
                        );
                        this.freshness = Some(l.freshness);
                    }
                    Ok(Err(e)) => {
                        if e.is_auth() {
                            set_health(cx, SVC, Health::NoAuth);
                        } else if e.is_offline() {
                            set_health(cx, SVC, Health::Down(e.to_string()));
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
        (self.pastes.len() as i64) < self.total
    }

    fn load_more(&mut self, cx: &mut Context<Self>) {
        if self.loading || !self.has_more() {
            return;
        }
        self.page += 1;
        self.load(true, cx);
    }

    fn rows(&self, cx: &App) -> Vec<Paste> {
        let q = self.search.read(cx).text().to_lowercase();
        self.pastes
            .iter()
            .filter(|p| {
                q.is_empty() || [&p.title, &p.id, &p.lang, &p.visibility].iter().any(|s| s.to_lowercase().contains(&q))
            })
            .cloned()
            .collect()
    }

    fn selected_id(&self) -> Option<&str> {
        match &self.sel {
            Sel::Paste { id, .. } => Some(id),
            _ => None,
        }
    }

    fn step(&mut self, by: i32, cx: &mut Context<Self>) {
        let rows = self.rows(cx);
        if rows.is_empty() {
            return;
        }
        let cur = self.selected_id().and_then(|id| rows.iter().position(|p| p.id == id));
        let next = match cur {
            None => 0,
            Some(i) => (i as i32 + by).clamp(0, rows.len() as i32 - 1) as usize,
        };
        self.scroll.scroll_to_item(next);
        let id = rows[next].id.clone();
        self.open(id, cx);
    }

    fn compose_dirty(&self, cx: &mut App) -> bool {
        matches!(self.sel, Sel::New)
            && self.compose.as_ref().is_some_and(|c| !c.editor.update(cx, |e, _| e.text()).trim().is_empty())
    }

    /// Open a paste (with its body), asking first if a new one is half-written.
    fn open(&mut self, id: String, cx: &mut Context<Self>) {
        if self.selected_id() == Some(id.as_str()) {
            return;
        }
        if self.compose_dirty(cx) {
            let me = self.me.clone();
            confirm(cx, "Discard this new paste?", "", "Discard", true, move |_, cx| {
                let _ = me.update(cx, |this, cx| {
                    if let Some(c) = &this.compose {
                        c.editor.update(cx, |e, cx| e.set_text("", cx));
                    }
                    this.open_now(id, cx)
                });
            });
            return;
        }
        self.open_now(id, cx);
    }

    fn open_now(&mut self, id: String, cx: &mut Context<Self>) {
        if self.fresh.as_ref().is_some_and(|(f, _)| *f != id) {
            self.fresh = None;
        }
        // the list row stands in until the body arrives
        let row = self.pastes.iter().find(|p| p.id == id).cloned();
        if let Some(p) = &row {
            self.fill_meta(p, cx);
        }
        self.sel = Sel::Paste { id: id.clone(), full: None, error: None };
        self.fetch(id, cx);
        cx.notify();
    }

    fn fetch(&mut self, id: String, cx: &mut Context<Self>) {
        let s = app::session(cx);
        let k = id.clone();
        let task = farfield_core::spawn(async move { scrap::get(&s, &k).await });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                let Sel::Paste { id: cur, .. } = &this.sel else { return };
                if *cur != id {
                    return;
                }
                match r {
                    Ok(Ok(l)) => {
                        this.fill_meta(&l.value, cx);
                        // keep the list row in step (views, token, title)
                        if let Some(row) = this.pastes.iter_mut().find(|p| p.id == id) {
                            *row = Paste { body: String::new(), ..l.value.clone() };
                        }
                        this.sel = Sel::Paste { id, full: Some(l.value), error: None };
                    }
                    Ok(Err(ApiError::NotFound)) => {
                        this.pastes.retain(|p| p.id != id);
                        this.sel = Sel::Paste { id, full: None, error: Some("Paste is gone.".into()) };
                    }
                    Ok(Err(e)) => {
                        if e.is_auth() {
                            set_health(cx, SVC, Health::NoAuth);
                        }
                        this.sel = Sel::Paste { id, full: None, error: Some(describe(&e)) };
                    }
                    Err(e) => this.sel = Sel::Paste { id, full: None, error: Some(e.to_string()) },
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn fill_meta(&mut self, p: &Paste, cx: &mut Context<Self>) {
        self.meta.title.update(cx, |f, cx| f.set_text(p.title.clone(), cx));
        self.meta.lang.update(cx, |f, cx| f.set_text(p.lang.clone(), cx));
        self.meta.vis = vis_index(&p.visibility);
        self.meta.exp = None;
    }

    fn current(&self) -> Option<&Paste> {
        match &self.sel {
            Sel::Paste { id, full, .. } => full.as_ref().or_else(|| self.pastes.iter().find(|p| p.id == *id)),
            _ => None,
        }
    }

    fn meta_changes(&self, cx: &App) -> Map<String, Value> {
        let Some(p) = self.current() else { return Map::new() };
        let mut m = Map::new();
        let title = self.meta.title.read(cx).text().trim().to_string();
        let lang = self.meta.lang.read(cx).text().trim().to_lowercase();
        if title != p.title {
            m.insert("title".into(), title.into());
        }
        if lang != p.lang {
            m.insert("lang".into(), lang.into());
        }
        if VISIBILITIES[self.meta.vis] != p.visibility {
            m.insert("visibility".into(), VISIBILITIES[self.meta.vis].into());
        }
        if let Some(i) = self.meta.exp {
            m.insert("expires".into(), EXPIRIES[i].into());
        }
        m
    }

    fn save_meta(&mut self, cx: &mut Context<Self>) {
        let Some(id) = self.selected_id().map(|s| s.to_string()) else { return };
        let ch = self.meta_changes(cx);
        if ch.is_empty() {
            toast(cx, "No changes.", false);
            return;
        }
        if self.meta.saving {
            return;
        }
        self.meta.saving = true;
        cx.notify();
        let s = app::session(cx);
        let task = farfield_core::spawn(async move { scrap::update(&s, &id, &Value::Object(ch)).await });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                this.meta.saving = false;
                match r {
                    Ok(Ok(v)) => {
                        let p = v.value;
                        log("scrap-update", &[("id", &p.id)]);
                        let forced = this.meta.vis == 0 && p.visibility != "public";
                        toast(
                            cx,
                            if forced { "Saved as unlisted — magic-link pastes can't be public." } else { "Saved." },
                            forced,
                        );
                        this.fill_meta(&p, cx);
                        if let Some(row) = this.pastes.iter_mut().find(|x| x.id == p.id) {
                            *row = Paste { body: String::new(), ..p.clone() };
                        }
                        if let Sel::Paste { id, full, .. } = &mut this.sel {
                            if *id == p.id {
                                *full = Some(p);
                            }
                        }
                    }
                    Ok(Err(e)) => toast(cx, describe(&e), true),
                    Err(e) => toast(cx, e.to_string(), true),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn new_paste(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        if self.compose.is_none() {
            let editor = cx.new(|cx| {
                let mut e = DocEditor::new(w, cx, "", "Paste or type — fixed once created.", None);
                e.set_plain(true, cx);
                e
            });
            cx.subscribe(&editor, |this: &mut Self, ed, e: &DocEvent, cx| {
                if matches!(e, DocEvent::Changed) {
                    let text = ed.update(cx, |e, _| e.text());
                    if let Some(c) = this.compose.as_mut() {
                        c.lines = if text.trim().is_empty() { 0 } else { text.lines().count() };
                    }
                    cx.notify()
                }
            })
            .detach();
            let mk = |label: &str, ph: &str, mono: bool, w: &mut Window, cx: &mut Context<Self>| {
                let (label, ph) = (label.to_string(), ph.to_string());
                let f = cx.new(|cx| {
                    let f = TextField::new(w, cx, label, ph);
                    if mono {
                        f.mono()
                    } else {
                        f
                    }
                });
                cx.subscribe(&f, |_this: &mut Self, _, e: &FieldEvent, cx| {
                    if *e == FieldEvent::Changed {
                        cx.notify()
                    }
                })
                .detach();
                f
            };
            let title = mk("Title", "optional", false, w, cx);
            let lang = mk("Language", "plain text", true, w, cx);
            self.compose = Some(Compose {
                editor,
                title,
                lang,
                vis: 1,
                exp: 0,
                magic: false,
                saving: false,
                error: None,
                lines: 0,
            });
        }
        self.sel = Sel::New;
        self.fresh = None;
        log("new", &[("area", SVC)]);
        if let Some(c) = &self.compose {
            c.editor.read(cx).focus_editor(w);
        }
        cx.notify();
    }

    fn create(&mut self, cx: &mut Context<Self>) {
        let Some(c) = self.compose.as_mut() else { return };
        if c.saving {
            return;
        }
        let body = c.editor.update(cx, |e, _| e.text());
        if body.trim().is_empty() {
            c.error = Some("Nothing to paste.".into());
            cx.notify();
            return;
        }
        let title = c.title.read(cx).text().trim().to_string();
        let lang = c.lang.read(cx).text().trim().to_lowercase();
        let (vis, exp, magic) = (VISIBILITIES[c.vis], EXPIRIES[c.exp], c.magic);
        c.saving = true;
        c.error = None;
        cx.notify();
        let s = app::session(cx);
        let task = farfield_core::spawn(async move { scrap::create(&s, &body, &title, &lang, vis, exp, magic).await });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                if let Some(c) = this.compose.as_mut() {
                    c.saving = false;
                }
                match r {
                    Ok(Ok(made)) => {
                        // never log the token
                        log(
                            "scrap-create",
                            &[("id", &made.id), ("magic", if made.token.is_some() { "1" } else { "0" })],
                        );
                        let existed = this.pastes.iter().any(|p| p.id == made.id);
                        if let Some(c) = &this.compose {
                            c.editor.update(cx, |e, cx| e.set_text("", cx));
                            c.title.update(cx, |f, cx| f.set_text("", cx));
                            c.lang.update(cx, |f, cx| f.set_text("", cx));
                        }
                        if let Some(c) = this.compose.as_mut() {
                            c.magic = false;
                            c.lines = 0;
                        }
                        toast(
                            cx,
                            if existed { "Already a paste — settings updated." } else { "Paste created." },
                            false,
                        );
                        let id = made.id.clone();
                        this.sel = Sel::None;
                        this.open_now(id.clone(), cx);
                        this.fresh = made.token.map(|t| (id, t));
                        this.reload(cx);
                    }
                    Ok(Err(e)) => {
                        if e.is_auth() {
                            set_health(cx, SVC, Health::NoAuth);
                        }
                        if let Some(c) = this.compose.as_mut() {
                            c.error = Some(describe(&e));
                        }
                    }
                    Err(e) => toast(cx, e.to_string(), true),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn rotate(&mut self, cx: &mut Context<Self>) {
        let Some(p) = self.current().cloned() else { return };
        if !p.has_token {
            toast(cx, "No magic link.", true);
            return;
        }
        let me = self.me.clone();
        confirm(cx, "Rotate the magic link?", "The current link stops working.", "Rotate", true, move |_, cx| {
            let _ = me.update(cx, |this, cx| this.token_op(p.id.clone(), true, cx));
        });
    }

    fn revoke(&mut self, cx: &mut Context<Self>) {
        let Some(p) = self.current().cloned() else { return };
        if !p.has_token {
            toast(cx, "No magic link.", true);
            return;
        }
        let after = match p.visibility.as_str() {
            "private" => "Only you can open it after.",
            _ => "Anyone with the plain link can read it after.",
        };
        let me = self.me.clone();
        confirm(cx, "Revoke the magic link?", after, "Revoke", true, move |_, cx| {
            let _ = me.update(cx, |this, cx| this.token_op(p.id.clone(), false, cx));
        });
    }

    fn token_op(&mut self, id: String, rotate: bool, cx: &mut Context<Self>) {
        self.busy = true;
        cx.notify();
        let s = app::session(cx);
        let k = id.clone();
        let task = farfield_core::spawn(async move {
            if rotate {
                scrap::rotate_token(&s, &k).await.map(Some)
            } else {
                scrap::revoke_token(&s, &k).await.map(|_| None)
            }
        });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                this.busy = false;
                match r {
                    Ok(Ok(tok)) => {
                        log(if rotate { "scrap-rotate" } else { "scrap-revoke" }, &[("id", &id)]);
                        toast(cx, if rotate { "New magic link — copy it now." } else { "Magic link revoked." }, false);
                        this.fresh = tok.map(|t| (id.clone(), t));
                        this.fetch(id, cx);
                    }
                    Ok(Err(ApiError::NotFound)) => toast(cx, "Already gone.", true),
                    Ok(Err(e)) => toast(cx, describe(&e), true),
                    Err(e) => toast(cx, e.to_string(), true),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn delete(&mut self, cx: &mut Context<Self>) {
        if matches!(self.sel, Sel::New) {
            let me = self.me.clone();
            confirm(cx, "Discard this new paste?", "", "Discard", true, move |_, cx| {
                let _ = me.update(cx, |this, cx| {
                    if let Some(c) = &this.compose {
                        c.editor.update(cx, |e, cx| e.set_text("", cx));
                    }
                    if let Some(c) = this.compose.as_mut() {
                        c.lines = 0;
                    }
                    this.sel = Sel::None;
                    cx.notify();
                });
            });
            return;
        }
        let Some(p) = self.current().cloned() else { return };
        let name = if p.title.is_empty() { p.id.clone() } else { p.title.clone() };
        let me = self.me.clone();
        confirm(cx, "Delete this paste?", format!("“{name}”"), "Delete", true, move |_, cx| {
            let s = app::session(cx);
            let id = p.id.clone();
            let task = farfield_core::spawn(async move { scrap::delete(&s, &id).await });
            let _ = me.update(cx, |_, cx| {
                cx.spawn(async move |this, cx| {
                    let r = task.await;
                    let _ = this.update(cx, |this, cx| match r {
                        Ok(Ok(())) | Ok(Err(ApiError::NotFound)) => {
                            log("scrap-delete", &[("id", &p.id)]);
                            this.pastes.retain(|x| x.id != p.id);
                            this.total = (this.total - 1).max(0);
                            this.sel = Sel::None;
                            this.fresh = None;
                            toast(cx, "Deleted.", false);
                            this.reload(cx);
                        }
                        Ok(Err(e)) => toast(cx, describe(&e), true),
                        Err(e) => toast(cx, e.to_string(), true),
                    });
                })
                .detach();
            });
        });
    }

    /// The share link — with the token only while it's freshly shown.
    fn share_link(&self, cx: &App) -> Option<String> {
        let id = self.selected_id()?;
        let tok = self.fresh.as_ref().filter(|(f, _)| f == id).map(|(_, t)| t.as_str());
        scrap::share_url(&app::session(cx), id, tok)
    }

    fn copy_link(&mut self, cx: &mut Context<Self>) {
        match self.share_link(cx) {
            Some(u) => {
                let with = self.fresh.as_ref().is_some_and(|(f, _)| Some(f.as_str()) == self.selected_id());
                kit::copy(cx, &u, if with { "the link, with its token" } else { "the link" })
            }
            None => toast(cx, "No public address for scrap.", true),
        }
    }

    // ── rendering ──────────────────────────────────────────────────────

    fn render_list(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let rows = self.rows(cx);
        let sel = self.selected_id().map(|s| s.to_string());
        let me = self.me.clone();
        let now = xscrap::now();
        let head = div()
            .flex()
            .flex_col()
            .gap(S2)
            .px(S4)
            .pt(S3)
            .pb(S3)
            .border_b_1()
            .border_color(t.rule)
            .child(
                div()
                    .flex()
                    .items_baseline()
                    .gap(S2)
                    .child(div().text_lg().font_weight(gpui::FontWeight::SEMIBOLD).text_color(t.ink).child("Scrap"))
                    .child(ui::mono(
                        if self.has_more() {
                            format!("{} of {} pastes", self.pastes.len(), self.total)
                        } else {
                            format!("{} pastes", self.total)
                        },
                        cx,
                    ))
                    .child(div().flex_1())
                    .child(ui::button("sc-new", "New  ⌘N", BtnKind::Quiet, cx, {
                        let me = me.clone();
                        move |_, w, cx| {
                            let _ = me.update(cx, |this, cx| this.new_paste(w, cx));
                        }
                    })),
            )
            .child(self.search.clone());
        let status = match (&self.error, &self.freshness) {
            (Some(e), _) => Some(ui::notice(e.clone(), t.bad, cx)),
            (None, Some(Freshness::Stale { age_ms, .. })) => Some(ui::notice(
                format!("Offline — showing what was loaded {} ago.", super::content::ago(*age_ms)),
                t.warn,
                cx,
            )),
            _ => None,
        };
        let more = self.has_more();
        let loading = self.loading;
        let body: AnyElement = if rows.is_empty() {
            let msg = if self.loading && !self.loaded {
                "Loading…"
            } else if self.error.is_some() && !self.loaded {
                "Couldn't load."
            } else if self.pastes.is_empty() {
                "No pastes yet."
            } else {
                "No matches."
            };
            ui::quiet_state(msg, cx).into_any_element()
        } else {
            div()
                .id("sc-list")
                .flex_1()
                .overflow_y_scroll()
                .track_scroll(&self.scroll)
                .children(rows.into_iter().enumerate().map(|(i, p)| {
                    let on = sel.as_deref() == Some(p.id.as_str());
                    let me = me.clone();
                    let expired = xscrap::is_expired(&p.expires_at, now);
                    let title = if p.title.is_empty() { "Untitled".to_string() } else { p.title.clone() };
                    let (vw, vc) = match p.visibility.as_str() {
                        "public" => ("public", t.good),
                        "private" => ("private", t.ink_3),
                        _ => ("unlisted", t.ink_2),
                    };
                    let exp = xscrap::expiry_label(&p.expires_at, now);
                    let views = format!("{} view{}", p.views, if p.views == 1 { "" } else { "s" });
                    let id = p.id.clone();
                    ui::list_row(("sc", i), on, &t)
                        .flex()
                        .flex_col()
                        .gap(px(2.))
                        .when(expired, |d| d.opacity(0.6))
                        .child(
                            div()
                                .flex()
                                .justify_between()
                                .gap(S2)
                                .child(
                                    div()
                                        .text_sm()
                                        .text_color(if p.title.is_empty() { t.ink_2 } else { t.ink })
                                        .truncate()
                                        .child(title),
                                )
                                .child(if expired { ui::chip("expired", t.bad, cx) } else { ui::chip(vw, vc, cx) }),
                        )
                        .child(
                            div()
                                .flex()
                                .gap(S2)
                                .text_xs()
                                .font_family(FONT_MONO)
                                .text_color(t.ink_3)
                                .child(p.id.clone())
                                .when(!p.lang.is_empty(), |d| d.child(p.lang.clone()))
                                .child(views)
                                .when(!exp.is_empty() && !expired, |d| d.child(exp))
                                .when(p.has_token, |d| d.child(div().text_color(t.accent).child("· link"))),
                        )
                        .on_click(move |_, _, cx| {
                            let id = id.clone();
                            let _ = me.update(cx, |this, cx| this.open(id, cx));
                        })
                        .into_any_element()
                }))
                .when(more, |d| {
                    let me = me.clone();
                    d.child(
                        div()
                            .id("sc-more")
                            .px(S4)
                            .py(px(10.))
                            .text_sm()
                            .text_color(t.accent)
                            .cursor_pointer()
                            .child(if loading { "Loading…" } else { "Load more" })
                            .on_click(move |_, _, cx| {
                                let _ = me.update(cx, |this, cx| this.load_more(cx));
                            }),
                    )
                })
                .into_any_element()
        };
        div()
            .flex()
            .flex_col()
            .size_full()
            .child(head)
            .when_some(status, |d, s| d.child(div().px(S4).py(S2).child(s)))
            .child(body)
            .into_any_element()
    }

    /// The new paste's settings, shown in the inspector beside the editor.
    fn compose_side(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let Some(c) = &self.compose else { return div().into_any_element() };
        let me = self.me.clone();
        let lang_now = c.lang.read(cx).text();
        div()
            .flex()
            .flex_col()
            .gap(S3)
            .child(c.title.clone())
            .child(c.lang.clone())
            .child(div().flex().flex_wrap().gap_x(S2).gap_y(px(2.)).mt(px(-4.)).children(
                xscrap::LANGS.iter().skip(1).map(|l| {
                    let me = me.clone();
                    let on = lang_now == *l;
                    div()
                        .id(SharedString::from(format!("lang-{l}")))
                        .text_xs()
                        .font_family(FONT_MONO)
                        .cursor_pointer()
                        .text_color(if on { t.accent } else { t.ink_3 })
                        .hover(|s| s.underline())
                        .child(l.to_string())
                        .on_click(move |_, _, cx| {
                            let _ = me.update(cx, |this, cx| {
                                if let Some(c) = &this.compose {
                                    c.lang.update(cx, |f, cx| f.set_text(*l, cx));
                                }
                                cx.notify();
                            });
                        })
                }),
            ))
            .child(
                div()
                    .flex()
                    .flex_col()
                    .gap(S1)
                    .child(div().text_xs().text_color(t.ink_2).child("Visibility"))
                    .child(kit::seg("sc-vis", &["Public", "Unlisted", "Private"], c.vis, cx, {
                        let me = me.clone();
                        move |i, _, cx| {
                            let _ = me.update(cx, |this, cx| {
                                if let Some(c) = this.compose.as_mut() {
                                    c.vis = i;
                                }
                                cx.notify();
                            });
                        }
                    }))
                    .when(c.vis == 0 && c.magic, |d| {
                        d.child(div().text_xs().text_color(t.warn).child("With a magic link it stays unlisted."))
                    }),
            )
            .child(div().flex().flex_col().gap(S1).child(div().text_xs().text_color(t.ink_2).child("Expires")).child(
                kit::seg("sc-exp", &EXPIRY_LABELS, c.exp, cx, {
                    let me = me.clone();
                    move |i, _, cx| {
                        let _ = me.update(cx, |this, cx| {
                            if let Some(c) = this.compose.as_mut() {
                                c.exp = i;
                            }
                            cx.notify();
                        });
                    }
                }),
            ))
            .child(kit::switch("sc-magic", "Magic link", "Shown once, after creating.", c.magic, cx, {
                let me = me.clone();
                move |_, cx| {
                    let _ = me.update(cx, |this, cx| {
                        if let Some(c) = this.compose.as_mut() {
                            c.magic = !c.magic;
                        }
                        cx.notify();
                    });
                }
            }))
            .when_some(c.error.clone(), |d, e| d.child(ui::notice(e, t.bad, cx)))
            .child(
                div()
                    .flex()
                    .gap(S2)
                    .pt(S2)
                    .child(if c.saving {
                        ui::button_disabled("Creating…", cx).into_any_element()
                    } else {
                        ui::button("sc-create", "Create paste  ⌘S", BtnKind::Primary, cx, {
                            let me = me.clone();
                            move |_, _, cx| {
                                let _ = me.update(cx, |this, cx| this.create(cx));
                            }
                        })
                        .into_any_element()
                    })
                    .child(ui::button("sc-discard", "Discard…", BtnKind::Quiet, cx, {
                        let me = me.clone();
                        move |_, _, cx| {
                            let _ = me.update(cx, |this, cx| this.delete(cx));
                        }
                    })),
            )
            .into_any_element()
    }

    fn render_compose(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let Some(c) = &self.compose else { return div().into_any_element() };
        let lines = c.lines;
        div()
            .size_full()
            .flex()
            .flex_col()
            .child(
                div()
                    .px(S6)
                    .pt(S5)
                    .pb(S3)
                    .flex()
                    .flex_col()
                    .gap(S1)
                    .child(ui::eyebrow("Paste · new", cx))
                    .child(
                        div()
                            .font_family(FONT_DOC)
                            .text_size(px(26.))
                            .line_height(px(32.))
                            .text_color(t.ink)
                            .child("New paste"),
                    )
                    .child(ui::mono(format!("{lines} line{}", if lines == 1 { "" } else { "s" }), cx)),
            )
            .child(ui::rule(cx))
            .child(div().flex_1().min_h_0().w_full().child(c.editor.clone()))
            .into_any_element()
    }

    fn render_paste(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let Sel::Paste { id, full, error } = &self.sel else { return div().into_any_element() };
        let row = self.current().cloned();
        let now = xscrap::now();
        let title = row
            .as_ref()
            .map(|p| if p.title.is_empty() { "Untitled".to_string() } else { p.title.clone() })
            .unwrap_or_default();
        let mut head =
            div().px(S6).pt(S5).pb(S3).flex().flex_col().gap(S1).child(ui::eyebrow("Paste · read-only", cx)).child(
                div().font_family(FONT_DOC).text_size(px(26.)).line_height(px(32.)).text_color(t.ink).child(title),
            );
        if let Some(p) = &row {
            let expired = xscrap::is_expired(&p.expires_at, now);
            let exp = xscrap::expiry_label(&p.expires_at, now);
            head = head.child(
                div()
                    .flex()
                    .flex_wrap()
                    .gap(S4)
                    .child(ui::mono(id.clone(), cx))
                    .child(ui::mono(if p.lang.is_empty() { "plain text".to_string() } else { p.lang.clone() }, cx))
                    .child(ui::chip(p.visibility.clone(), if p.visibility == "public" { t.good } else { t.ink_3 }, cx))
                    .child(if expired {
                        ui::chip("expired", t.bad, cx)
                    } else if exp.is_empty() {
                        ui::chip("never expires", t.ink_3, cx)
                    } else {
                        ui::chip(format!("expires {exp}"), t.ink_3, cx)
                    })
                    .child(ui::mono(format!("{} views", p.views), cx)),
            );
        }
        let body: AnyElement = match (full, error) {
            (_, Some(e)) => div().p(S6).child(ui::notice(e.clone(), t.bad, cx)).into_any_element(),
            (None, None) => ui::quiet_state("Loading…", cx).into_any_element(),
            (Some(p), None) => {
                let lines: Arc<Vec<String>> = Arc::new(p.body.lines().map(|l| l.replace('\t', "    ")).collect());
                let n = lines.len();
                let gutter = px(8. * (n.max(1).to_string().len() as f32) + 16.);
                uniform_list("sc-body", n, move |range, _w, cx| {
                    let t = theme(cx).clone();
                    range
                        .map(|i| {
                            div()
                                .flex()
                                .px(S4)
                                .font_family(FONT_MONO)
                                .text_size(px(12.5))
                                .line_height(px(20.))
                                .child(div().w(gutter).flex_none().text_color(t.ink_3).child((i + 1).to_string()))
                                .child(div().flex_1().text_color(t.ink).whitespace_nowrap().child(lines[i].clone()))
                        })
                        .collect()
                })
                .track_scroll(self.body_scroll.clone())
                .flex_1()
                .py(S3)
                .into_any_element()
            }
        };
        div().size_full().flex().flex_col().child(head).child(ui::rule(cx)).child(body).into_any_element()
    }
}

impl Workspace for ScrapWs {
    fn inspector(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> Option<AnyElement> {
        let t = theme(cx).clone();
        let me = self.me.clone();
        if matches!(self.sel, Sel::New) {
            return Some(
                div()
                    .flex()
                    .flex_col()
                    .gap(S2)
                    .child(ui::eyebrow("New paste", cx))
                    .child(self.compose_side(cx))
                    .into_any_element(),
            );
        }
        let p = self.current()?.clone();
        let mut col = div().flex().flex_col().gap(S2);
        // the shown-once token comes first: it's the one thing that can't wait
        if let Some((_, tok)) = self.fresh.as_ref().filter(|(f, _)| *f == p.id) {
            let tok = tok.clone();
            col = col
                .child(ui::eyebrow("Magic link · shown once", cx))
                .child(ui::notice("Copy it now — it can't be shown again.", t.warn, cx))
                .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink).child(tok.clone()))
                .child(
                    div()
                        .flex()
                        .gap(px(2.))
                        .child(ui::button("sc-copy-tok-link", "Copy link with token", BtnKind::Primary, cx, {
                            let me = me.clone();
                            move |_, _, cx| {
                                let _ = me.update(cx, |this, cx| this.copy_link(cx));
                            }
                        }))
                        .child(ui::button("sc-copy-tok", "Copy token", BtnKind::Quiet, cx, move |_, _, cx| {
                            kit::copy(cx, &tok, "the token")
                        })),
                )
                .child(ui::button("sc-dismiss", "Hide", BtnKind::Quiet, cx, {
                    let me = me.clone();
                    move |_, _, cx| {
                        let _ = me.update(cx, |this, cx| {
                            this.fresh = None;
                            cx.notify();
                        });
                    }
                }))
                .child(ui::rule(cx));
        }
        let changes = !self.meta_changes(cx).is_empty();
        let now = xscrap::now();
        let exp_now = if p.expires_at.is_empty() {
            "never".to_string()
        } else if xscrap::is_expired(&p.expires_at, now) {
            format!("expired {}", ui::when(&p.expires_at))
        } else {
            format!("{} · {}", xscrap::expiry_label(&p.expires_at, now), ui::when(&p.expires_at))
        };
        let mut exp_labels = vec!["Keep"];
        exp_labels.extend(EXPIRY_LABELS);
        col = col
            .child(ui::eyebrow("Details", cx))
            .child(self.meta.title.clone())
            .child(self.meta.lang.clone())
            .child(div().text_xs().text_color(t.ink_2).child("Visibility"))
            .child(kit::seg("sc-mvis", &["Public", "Unlisted", "Private"], self.meta.vis, cx, {
                let me = me.clone();
                move |i, _, cx| {
                    let _ = me.update(cx, |this, cx| {
                        this.meta.vis = i;
                        cx.notify();
                    });
                }
            }))
            .when(p.has_token && self.meta.vis == 0, |d| {
                d.child(div().text_xs().text_color(t.warn).child("With a magic link it stays unlisted."))
            })
            .child(div().text_xs().text_color(t.ink_2).child(format!("Expiry — now {exp_now}")))
            .child(kit::seg("sc-mexp", &exp_labels, self.meta.exp.map(|i| i + 1).unwrap_or(0), cx, {
                let me = me.clone();
                move |i, _, cx| {
                    let _ = me.update(cx, |this, cx| {
                        this.meta.exp = if i == 0 { None } else { Some(i - 1) };
                        cx.notify();
                    });
                }
            }))
            .when(self.meta.exp.is_some_and(|i| i > 0), |d| {
                d.child(div().text_xs().text_color(t.ink_3).child("Counted from now."))
            })
            .child(div().flex().gap(S2).child(if self.meta.saving {
                ui::button_disabled("Saving…", cx).into_any_element()
            } else if changes {
                ui::button("sc-msave", "Save details  ⌘S", BtnKind::Primary, cx, {
                    let me = me.clone();
                    move |_, _, cx| {
                        let _ = me.update(cx, |this, cx| this.save_meta(cx));
                    }
                })
                .into_any_element()
            } else {
                ui::button_disabled("Save details  ⌘S", cx).into_any_element()
            }))
            .child(ui::rule(cx))
            .child(ui::eyebrow("Sharing", cx))
            .when_some(self.share_link(cx), |d, u| {
                // never print a token here; the shown-once block has it
                let shown = u.split("?t=").next().unwrap_or("").to_string();
                d.child(ui::mono(shown, cx))
            })
            .child(ui::button("sc-copy", "Copy share link", BtnKind::Quiet, cx, {
                let me = me.clone();
                move |_, _, cx| {
                    let _ = me.update(cx, |this, cx| this.copy_link(cx));
                }
            }))
            .child(ui::field_row(
                "Magic link",
                ui::chip(if p.has_token { "on" } else { "off" }, if p.has_token { t.accent } else { t.ink_3 }, cx),
                cx,
            ));
        if p.has_token {
            col = col.child(div().flex().gap(px(2.)).child(if self.busy {
                ui::button_disabled("Working…", cx).into_any_element()
            } else {
                div()
                    .flex()
                    .gap(px(2.))
                    .child(ui::button("sc-rotate", "Rotate…", BtnKind::Quiet, cx, {
                        let me = me.clone();
                        move |_, _, cx| {
                            let _ = me.update(cx, |this, cx| this.rotate(cx));
                        }
                    }))
                    .child(ui::button("sc-revoke", "Revoke…", BtnKind::Danger, cx, {
                        let me = me.clone();
                        move |_, _, cx| {
                            let _ = me.update(cx, |this, cx| this.revoke(cx));
                        }
                    }))
                    .into_any_element()
            }));
        }
        col = col
            .child(ui::rule(cx))
            .child(ui::eyebrow("Record", cx))
            .child(ui::field_row("CID", ui::mono(p.cid.clone(), cx), cx))
            .child(ui::field_row("Created", ui::mono(ui::when(&p.created_at), cx), cx))
            .when(!p.alias.is_empty(), |d| d.child(ui::field_row("Alias", ui::mono(p.alias.clone(), cx), cx)))
            .child(ui::rule(cx))
            .child(ui::button("sc-delete", "Delete paste…", BtnKind::Danger, cx, move |_, _, cx| {
                let _ = me.update(cx, |this, cx| this.delete(cx));
            }));
        Some(col.into_any_element())
    }

    fn palette(&self, _cx: &App) -> Vec<PaletteItem> {
        let me = self.me.clone();
        vec![PaletteItem::new("Scrap: new paste", "⌘N", move |w, cx| {
            crate::shell::goto(cx, SVC);
            let _ = me.update(cx, |this, cx| this.new_paste(w, cx));
        })]
    }

    fn commands(&self, _cx: &App) -> Vec<(&'static str, String, &'static str)> {
        let mut v =
            vec![("new", "Scrap: new paste".to_string(), "⌘N"), ("reload", "Scrap: reload pastes".into(), "⌘R")];
        match &self.sel {
            Sel::New => v.push(("save", "Scrap: create paste".into(), "⌘S")),
            Sel::Paste { .. } => {
                v.push(("save", "Paste: save details".into(), "⌘S"));
                v.push(("copy-link", "Paste: copy share link".into(), ""));
                if self.current().is_some_and(|p| p.has_token) {
                    v.push(("rotate", "Paste: rotate magic link…".into(), ""));
                    v.push(("revoke", "Paste: revoke magic link…".into(), ""));
                }
                v.push(("delete", "Paste: delete…".into(), ""));
            }
            Sel::None => {}
        }
        if self.has_more() {
            v.push(("more", "Scrap: load more pastes".into(), ""));
        }
        v
    }

    fn run_command(&mut self, id: &str, w: &mut Window, cx: &mut Context<Self>) {
        match id {
            "new" => self.new_paste(w, cx),
            "reload" => self.reload(cx),
            "save" => Workspace::save(self, w, cx),
            "copy-link" => self.copy_link(cx),
            "rotate" => self.rotate(cx),
            "revoke" => self.revoke(cx),
            "delete" => self.delete(cx),
            "more" => self.load_more(cx),
            _ => {}
        }
    }

    fn focus_search(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        self.search.read(cx).focus(w);
    }
    fn new_item(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        self.new_paste(w, cx)
    }
    fn save(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        match self.sel {
            Sel::New => self.create(cx),
            Sel::Paste { .. } => self.save_meta(cx),
            Sel::None => {}
        }
    }
    fn refresh(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        self.reload(cx);
        if let Some(id) = self.selected_id().map(|s| s.to_string()) {
            self.fetch(id, cx);
        }
    }
    fn dirty(&self, cx: &App) -> bool {
        matches!(self.sel, Sel::New) && self.compose.as_ref().is_some_and(|c| c.lines > 0)
            || (matches!(self.sel, Sel::Paste { .. }) && !self.meta_changes(cx).is_empty())
    }
}

impl Render for ScrapWs {
    fn render(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = theme(cx).clone();
        let main: AnyElement = match &self.sel {
            Sel::New => self.render_compose(cx),
            Sel::Paste { .. } => self.render_paste(cx),
            Sel::None => div()
                .size_full()
                .flex()
                .flex_col()
                .justify_center()
                .items_center()
                .gap(S2)
                .child(div().text_sm().text_color(t.ink_2).child("No paste selected."))
                .into_any_element(),
        };
        div()
            .size_full()
            .flex()
            .child(div().w(px(340.)).flex_none().h_full().border_r_1().border_color(t.rule).child(self.render_list(cx)))
            .child(div().flex_1().min_w_0().h_full().child(main))
    }
}
