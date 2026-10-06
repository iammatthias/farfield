//! Bookmarks: every bookmark — private ones and admin notes included, from
//! the admin API — grouped by category. The form saves partial changes
//! conditionally (If-Match on the bookmark's CID); when someone else saved
//! first, the server's version is shown beside yours and yours can be
//! reapplied on top of it. The inspector shows what the page says about
//! itself (og image, site name, favicon), fetched straight from the web
//! without any fleet credential.

use crate::app::{self, describe, log, Health};
use crate::shell::{confirm, set_health, toast};
use crate::theme::{theme, FONT_DOC, FONT_MONO, MEASURE, S1, S2, S3, S4, S5, S6};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, Kind as BtnKind};
use crate::workspace::{PaletteItem, Workspace};
use farfield_core::api::bookmarks::{self, Bookmark};
use farfield_core::api::ext_lists;
use farfield_core::{ApiError, Freshness, Latest};
use gpui::{
    div, img, prelude::*, px, AnyElement, App, Context, Entity, ObjectFit, ScrollHandle, SharedString, WeakEntity,
    Window,
};
use serde_json::{json, Map, Value};
use std::sync::Arc;

use kit::{Images, Slot};

/// Pieces the lists workspaces share: a segmented choice, a switch, a
/// bounded image cache, decoding to GPUI's BGRA images, small text helpers.
pub(crate) mod kit {
    use crate::theme::{theme, S2};
    use gpui::{div, prelude::*, px, App, ClickEvent, Div, RenderImage, SharedString, Window};
    use std::collections::{HashMap, VecDeque};
    use std::sync::Arc;

    /// A row of mutually exclusive choices (an underline marks the current
    /// one). `on_pick` gets the chosen index.
    pub fn seg(
        id: &str,
        labels: &[&str],
        current: usize,
        cx: &App,
        on_pick: impl Fn(usize, &mut Window, &mut App) + 'static,
    ) -> Div {
        let t = theme(cx).clone();
        let on_pick = std::rc::Rc::new(on_pick);
        div().flex().flex_wrap().gap(S2).children(labels.iter().enumerate().map(|(i, l)| {
            let on = i == current;
            let f = on_pick.clone();
            let wash = t.wash;
            div()
                .id(SharedString::from(format!("{id}-{i}")))
                .px(px(6.))
                .py(px(3.))
                .text_sm()
                .cursor_pointer()
                .border_b_2()
                .border_color(if on { t.accent } else { gpui::transparent_black() })
                .text_color(if on { t.ink } else { t.ink_2 })
                .hover(move |s| s.bg(wash))
                .child(l.to_string())
                .on_click(move |_: &ClickEvent, w, cx| f(i, w, cx))
        }))
    }

    /// An on/off switch with a label and a one-line explanation.
    pub fn switch(
        id: &str,
        label: &str,
        hint: &str,
        on: bool,
        cx: &App,
        on_toggle: impl Fn(&mut Window, &mut App) + 'static,
    ) -> impl IntoElement {
        let t = theme(cx).clone();
        let wash = t.wash;
        div()
            .id(SharedString::from(id.to_string()))
            .flex()
            .items_start()
            .gap(px(10.))
            .py(px(4.))
            .cursor_pointer()
            .rounded(px(3.))
            .hover(move |s| s.bg(wash))
            .on_click(move |_, w, cx| on_toggle(w, cx))
            .child(
                div()
                    .mt(px(2.))
                    .w(px(26.))
                    .h(px(15.))
                    .flex_none()
                    .rounded_full()
                    .bg(if on { t.accent } else { t.rule_strong })
                    .flex()
                    .items_center()
                    .when(on, |d| d.justify_end())
                    .px(px(2.))
                    .child(div().w(px(11.)).h(px(11.)).rounded_full().bg(t.paper)),
            )
            .child(
                div()
                    .flex_1()
                    .min_w_0()
                    .flex()
                    .flex_col()
                    .child(div().text_sm().text_color(t.ink).child(label.to_string()))
                    .when(!hint.is_empty(), |d| d.child(div().text_xs().text_color(t.ink_2).child(hint.to_string()))),
            )
    }

    pub enum Slot {
        Loading,
        Ready(Arc<RenderImage>, u32, u32),
        Failed,
    }

    /// Decoded images by key (a URL, or code id + version), bounded; at most
    /// `MAX_INFLIGHT` fetches at once. Evicted images are handed back to the
    /// window (`drop_retired`) so they leave the sprite atlas.
    pub struct Images {
        map: HashMap<String, Slot>,
        order: VecDeque<String>,
        cap: usize,
        inflight: usize,
        retired: Vec<Arc<RenderImage>>,
    }

    pub const MAX_INFLIGHT: usize = 6;

    impl Images {
        pub fn new(cap: usize) -> Self {
            Images { map: HashMap::new(), order: VecDeque::new(), cap, inflight: 0, retired: Vec::new() }
        }
        pub fn get(&self, k: &str) -> Option<&Slot> {
            self.map.get(k)
        }
        pub fn ready(&self, k: &str) -> Option<(Arc<RenderImage>, u32, u32)> {
            match self.map.get(k) {
                Some(Slot::Ready(i, w, h)) => Some((i.clone(), *w, *h)),
                _ => None,
            }
        }
        /// Claim a fetch for `k`: false if it is cached, loading, or the
        /// concurrency budget is spent (ask again when one finishes).
        pub fn begin(&mut self, k: &str) -> bool {
            if self.map.contains_key(k) || self.inflight >= MAX_INFLIGHT {
                return false;
            }
            self.inflight += 1;
            self.map.insert(k.to_string(), Slot::Loading);
            true
        }
        pub fn finish(&mut self, k: &str, r: Option<(Arc<RenderImage>, u32, u32)>) {
            self.inflight = self.inflight.saturating_sub(1);
            if !self.map.contains_key(k) {
                // forgotten while loading
                if let Some((i, _, _)) = r {
                    self.retired.push(i);
                }
                return;
            }
            self.map.insert(
                k.to_string(),
                match r {
                    Some((i, w, h)) => Slot::Ready(i, w, h),
                    None => Slot::Failed,
                },
            );
            self.order.retain(|x| x != k);
            self.order.push_back(k.to_string());
            while self.order.len() > self.cap {
                if let Some(old) = self.order.pop_front() {
                    self.forget(&old);
                }
            }
        }
        pub fn forget(&mut self, k: &str) {
            if let Some(Slot::Ready(i, _, _)) = self.map.remove(k) {
                self.retired.push(i);
            }
            self.order.retain(|x| x != k);
        }
        pub fn drop_retired(&mut self, w: &mut Window) {
            for i in self.retired.drain(..) {
                let _ = w.drop_image(i);
            }
        }
    }

    /// Decode bytes into a BGRA RenderImage no larger than `max` px on a
    /// side. `crisp` keeps hard pixel edges (QR modules). Run off the UI
    /// thread.
    pub fn decode(bytes: &[u8], max: u32, crisp: bool) -> Option<(Arc<RenderImage>, u32, u32)> {
        let im = image::load_from_memory(bytes).ok()?;
        let im = if im.width() > max || im.height() > max {
            let f = if crisp { image::imageops::FilterType::Nearest } else { image::imageops::FilterType::Triangle };
            im.resize(max, max, f)
        } else {
            im
        };
        let mut buf = im.to_rgba8();
        for p in buf.pixels_mut() {
            p.0.swap(0, 2);
        }
        let (w, h) = buf.dimensions();
        Some((Arc::new(RenderImage::new(smallvec::smallvec![image::Frame::new(buf)])), w, h))
    }

    /// The host of a URL, for list rows ("example.com").
    pub fn host(url: &str) -> String {
        let rest = url.split_once("://").map(|(_, r)| r).unwrap_or(url);
        let h = rest.split(['/', '?', '#']).next().unwrap_or("");
        h.strip_prefix("www.").unwrap_or(h).to_string()
    }

    /// A 412, field by field: only what differs, yours above the server's;
    /// `edited` = you changed it (otherwise only the server did).
    pub fn diff_rows(diffs: Vec<(&str, String, String, bool)>, server_at: &str, cx: &App) -> Div {
        let t = theme(cx).clone();
        if diffs.is_empty() {
            return div().text_sm().text_color(t.ink_2).child("No visible fields differ.");
        }
        let line = |who: &str, v: String, strong: bool| {
            div()
                .flex()
                .gap(S2)
                .child(
                    div()
                        .w(px(52.))
                        .flex_none()
                        .pt(px(2.))
                        .font_family(crate::theme::FONT_MONO)
                        .text_xs()
                        .text_color(t.ink_3)
                        .child(who.to_string()),
                )
                .child(div().flex_1().min_w_0().text_sm().text_color(if strong { t.ink } else { t.ink_2 }).child(v))
        };
        div()
            .flex()
            .flex_col()
            .child(
                div()
                    .text_xs()
                    .text_color(t.ink_2)
                    .pb(px(4.))
                    .child(format!("Server saved {}", crate::ui::when(server_at))),
            )
            .children(diffs.into_iter().map(|(label, mine, theirs, edited)| {
                div()
                    .flex()
                    .flex_col()
                    .gap(px(2.))
                    .py(px(6.))
                    .border_t_1()
                    .border_color(t.rule)
                    .child(div().text_xs().text_color(t.bad).child(label.to_string()))
                    .when(edited, |d| d.child(line("yours", mine, true)))
                    .child(line("server", theirs, !edited))
                    .when(!edited, |d| {
                        d.child(div().pl(px(60.)).text_xs().text_color(t.ink_3).child("unchanged by you"))
                    })
            }))
    }

    pub fn copy(cx: &mut App, s: &str, what: &str) {
        cx.write_to_clipboard(gpui::ClipboardItem::new_string(s.to_string()));
        crate::shell::toast(cx, format!("Copied {what}."), false);
    }
}

#[derive(Clone, Copy, PartialEq)]
enum Vis {
    All,
    Public,
    Private,
}

// rows are built per render and short-lived; boxing buys nothing
#[allow(clippy::large_enum_variant)]
#[derive(Clone)]
enum Row {
    Header(String, usize),
    Item(Bookmark),
}

/// The bookmark being edited (or created).
struct Form {
    /// None until the bookmark exists on the server.
    id: Option<String>,
    /// The server's version this form started from — the If-Match tag and
    /// the baseline for a partial PUT.
    base: Bookmark,
    public: bool,
    saving: bool,
    refreshing: bool,
    /// A 412: the server's version, shown beside yours.
    conflict: Option<Bookmark>,
    error: Option<String>,
}

pub struct BookmarksWs {
    me: WeakEntity<Self>,
    items: Vec<Bookmark>,
    loading: bool,
    loaded: bool,
    freshness: Option<Freshness>,
    error: Option<String>,
    latest: Arc<Latest>,
    search: Entity<TextField>,
    category: Option<String>,
    vis: Vis,
    scroll: ScrollHandle,
    form: Option<Form>,
    f_url: Entity<TextField>,
    f_title: Entity<TextField>,
    f_desc: Entity<TextField>,
    f_cat: Entity<TextField>,
    f_notes: Entity<TextField>,
    images: Images,
}

const SVC: &str = "bookmarks";

impl BookmarksWs {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        let search = cx.new(|cx| TextField::new(w, cx, "", "Filter  ⌘F"));
        cx.subscribe_in(&search, w, |this: &mut Self, _, e: &FieldEvent, w, cx| match e {
            FieldEvent::Changed => cx.notify(),
            FieldEvent::Down => this.step(1, w, cx),
            FieldEvent::Up => this.step(-1, w, cx),
            FieldEvent::Submit => this.f_title.read(cx).focus(w),
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
        let f_url = field("URL", "https://…", true, w, cx);
        let f_title = field("Title", "from the page", false, w, cx);
        let f_desc = field("Description", "", false, w, cx);
        let f_cat = field("Category", "", false, w, cx);
        let f_notes = field("Admin notes · private", "", false, w, cx);
        let mut this = BookmarksWs {
            me: cx.entity().downgrade(),
            items: Vec::new(),
            loading: false,
            loaded: false,
            freshness: None,
            error: None,
            latest: Arc::new(Latest::default()),
            search,
            category: None,
            vis: Vis::All,
            scroll: ScrollHandle::new(),
            form: None,
            f_url,
            f_title,
            f_desc,
            f_cat,
            f_notes,
            images: Images::new(64),
        };
        this.reload(cx);
        this
    }

    fn reload(&mut self, cx: &mut Context<Self>) {
        let ticket = self.latest.ticket();
        let latest = self.latest.clone();
        let s = app::session(cx);
        self.loading = true;
        cx.notify();
        let task = farfield_core::spawn(async move { bookmarks::all(&s).await });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                if !latest.is_current(ticket) {
                    return;
                }
                this.loading = false;
                match r {
                    Ok(Ok(l)) => {
                        this.items = l.value;
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
                        // keep an unedited form in step with the server
                        if let Some(id) = this.form.as_ref().and_then(|f| f.id.clone()) {
                            if !this.dirty_form(cx) {
                                if let Some(b) = this.items.iter().find(|b| b.id == id).cloned() {
                                    this.set_form(b, cx);
                                }
                            }
                        }
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

    fn categories(&self) -> Vec<(String, usize)> {
        let mut out: Vec<(String, usize)> = Vec::new();
        for c in bookmarks::categories(&self.items) {
            let n = self.items.iter().filter(|b| b.category == c).count();
            out.push((c, n));
        }
        out
    }

    fn filtered(&self, cx: &App) -> Vec<Bookmark> {
        let q = self.search.read(cx).text().to_lowercase();
        self.items
            .iter()
            .filter(|b| match self.vis {
                Vis::All => true,
                Vis::Public => b.public,
                Vis::Private => !b.public,
            })
            .filter(|b| self.category.as_ref().is_none_or(|c| &b.category == c))
            .filter(|b| {
                q.is_empty()
                    || [&b.title, &b.url, &b.category, &b.description, &b.og_title, &b.admin_notes]
                        .iter()
                        .any(|s| s.to_lowercase().contains(&q))
            })
            .cloned()
            .collect()
    }

    /// Filtered bookmarks, grouped by category (the server's order is
    /// category A–Z, then newest).
    fn rows(&self, cx: &App) -> Vec<Row> {
        let mut items = self.filtered(cx);
        items.sort_by(|a, b| {
            let (x, y) = (a.category.to_lowercase(), b.category.to_lowercase());
            // uncategorized last
            (x.is_empty(), x).cmp(&(y.is_empty(), y))
        });
        let mut out = Vec::new();
        let mut i = 0;
        while i < items.len() {
            let c = items[i].category.clone();
            let n = items[i..].iter().take_while(|b| b.category == c).count();
            out.push(Row::Header(c, n));
            out.extend(items[i..i + n].iter().cloned().map(Row::Item));
            i += n;
        }
        out
    }

    fn selected_id(&self) -> Option<&str> {
        self.form.as_ref().and_then(|f| f.id.as_deref())
    }

    fn step(&mut self, by: i32, w: &mut Window, cx: &mut Context<Self>) {
        let rows = self.rows(cx);
        let items: Vec<(usize, &Bookmark)> = rows
            .iter()
            .enumerate()
            .filter_map(|(i, r)| match r {
                Row::Item(b) => Some((i, b)),
                _ => None,
            })
            .collect();
        if items.is_empty() {
            return;
        }
        let cur = self.selected_id().and_then(|id| items.iter().position(|(_, b)| b.id == id));
        let next = match cur {
            None => 0,
            Some(i) => (i as i32 + by).clamp(0, items.len() as i32 - 1) as usize,
        };
        self.scroll.scroll_to_item(items[next].0);
        let b = items[next].1.clone();
        self.select(b, w, cx);
    }

    /// Switch the form to `b`, asking first if the current one has edits.
    fn select(&mut self, b: Bookmark, _w: &mut Window, cx: &mut Context<Self>) {
        if self.selected_id() == Some(b.id.as_str()) {
            return;
        }
        self.guard(cx, move |this, cx| {
            this.set_form(b, cx);
            this.want_images(cx);
        });
    }

    /// Run `then` now, or after the person agrees to drop unsaved edits.
    fn guard(&mut self, cx: &mut Context<Self>, then: impl FnOnce(&mut Self, &mut Context<Self>) + 'static) {
        if !self.dirty_form(cx) {
            then(self, cx);
            cx.notify();
            return;
        }
        let me = self.me.clone();
        confirm(cx, "Discard your changes?", "", "Discard", true, move |_, cx| {
            let _ = me.update(cx, |this, cx| {
                then(this, cx);
                cx.notify();
            });
        });
    }

    fn set_form(&mut self, b: Bookmark, cx: &mut Context<Self>) {
        self.f_url.update(cx, |f, cx| f.set_text(b.url.clone(), cx));
        self.f_title.update(cx, |f, cx| f.set_text(b.title.clone(), cx));
        self.f_desc.update(cx, |f, cx| f.set_text(b.description.clone(), cx));
        self.f_cat.update(cx, |f, cx| f.set_text(b.category.clone(), cx));
        self.f_notes.update(cx, |f, cx| f.set_text(b.admin_notes.clone(), cx));
        let id = if b.id.is_empty() { None } else { Some(b.id.clone()) };
        self.form =
            Some(Form { id, public: b.public, base: b, saving: false, refreshing: false, conflict: None, error: None });
        cx.notify();
    }

    fn new_bookmark(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        let cat = self.category.clone().unwrap_or_default();
        let me = self.me.clone();
        let focus = self.f_url.clone();
        self.guard(cx, move |this, cx| {
            this.set_form(Bookmark { category: cat, public: true, ..Default::default() }, cx);
            log("new", &[("area", SVC)]);
            let _ = me;
        });
        focus.read(cx).focus(w);
    }

    /// What the form says now, as the API's field names.
    fn fields(&self, cx: &App) -> Map<String, Value> {
        let Some(f) = &self.form else { return Map::new() };
        let txt = |e: &Entity<TextField>| Value::String(e.read(cx).text().trim().to_string());
        let mut m = Map::new();
        m.insert("url".into(), txt(&self.f_url));
        m.insert("title".into(), txt(&self.f_title));
        m.insert("description".into(), txt(&self.f_desc));
        m.insert("category".into(), txt(&self.f_cat));
        m.insert("adminNotes".into(), txt(&self.f_notes));
        m.insert("public".into(), Value::Bool(f.public));
        m
    }

    /// The fields that differ from `base` — a partial PUT.
    fn changes(&self, base: &Bookmark, cx: &App) -> Map<String, Value> {
        let b = serde_json::to_value(base).unwrap_or(Value::Null);
        self.fields(cx).into_iter().filter(|(k, v)| b.get(k).unwrap_or(&json!("")) != v).collect()
    }

    fn dirty_form(&self, cx: &App) -> bool {
        match &self.form {
            None => false,
            Some(f) if f.id.is_none() => !self.f_url.read(cx).text().trim().is_empty(),
            Some(f) => !self.changes(&f.base, cx).is_empty(),
        }
    }

    fn save(&mut self, cx: &mut Context<Self>) {
        let Some(form) = &self.form else { return };
        if form.saving {
            return;
        }
        let fields = self.fields(cx);
        if fields["url"].as_str().unwrap_or("").is_empty() {
            self.form.as_mut().unwrap().error = Some("A bookmark needs a URL.".into());
            cx.notify();
            return;
        }
        let s = app::session(cx);
        let (id, cid) = (form.id.clone(), form.base.cid.clone());
        let task = match &id {
            None => {
                let b: Bookmark = match serde_json::from_value(Value::Object(fields)) {
                    Ok(b) => b,
                    Err(e) => return toast(cx, e.to_string(), true),
                };
                farfield_core::spawn(async move { bookmarks::create(&s, &b).await })
            }
            Some(id) => {
                let ch = self.changes(&form.base, cx);
                if ch.is_empty() {
                    toast(cx, "No changes.", false);
                    return;
                }
                let id = id.clone();
                farfield_core::spawn(async move { bookmarks::update(&s, &id, &Value::Object(ch), Some(&cid)).await })
            }
        };
        self.put(id.is_none(), task, cx);
    }

    /// Send a write and take its outcome into the form.
    fn put<E: std::fmt::Display + 'static>(
        &mut self,
        created: bool,
        task: impl std::future::Future<Output = Result<Result<farfield_core::transport::Versioned<Bookmark>, ApiError>, E>>
            + 'static,
        cx: &mut Context<Self>,
    ) {
        if let Some(f) = self.form.as_mut() {
            f.saving = true;
            f.error = None;
        }
        cx.notify();
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                if let Some(f) = this.form.as_mut() {
                    f.saving = false;
                }
                match r {
                    Ok(Ok(v)) => {
                        let b = v.value;
                        log(if created { "bookmark-create" } else { "bookmark-save" }, &[("id", &b.id)]);
                        toast(cx, if created { "Saved. Fetching page details…" } else { "Saved." }, false);
                        let id = b.id.clone();
                        this.set_form(b, cx);
                        this.reload(cx);
                        this.want_images(cx);
                        if created {
                            // the server fetches page metadata after answering
                            this.refetch_later(id, cx);
                        }
                    }
                    Ok(Err(ApiError::Precondition { current, .. })) => {
                        log("conflict", &[("service", SVC)]);
                        let cur = ext_lists::bookmarks::from_conflict(&current);
                        if let Some(f) = this.form.as_mut() {
                            f.conflict = cur;
                            f.error = Some("Changed on the server.".into());
                        }
                    }
                    Ok(Err(e)) => {
                        if e.is_auth() {
                            set_health(cx, SVC, Health::NoAuth);
                        }
                        if let Some(f) = this.form.as_mut() {
                            f.error = Some(describe(&e));
                        }
                    }
                    Err(e) => toast(cx, e.to_string(), true),
                }
                cx.notify();
            });
        })
        .detach();
    }

    /// After a create: pick up the og details once the server has them.
    fn refetch_later(&mut self, id: String, cx: &mut Context<Self>) {
        let s = app::session(cx);
        cx.spawn(async move |this, cx| {
            cx.background_executor().timer(std::time::Duration::from_millis(2500)).await;
            let r = farfield_core::spawn(async move { bookmarks::get(&s, &id).await }).await;
            let _ = this.update(cx, |this, cx| {
                if let Ok(Ok(l)) = r {
                    if this.selected_id() == Some(l.value.id.as_str()) && !this.dirty_form(cx) {
                        this.set_form(l.value, cx);
                        this.want_images(cx);
                        this.reload(cx);
                    }
                }
            });
        })
        .detach();
    }

    /// Conflict: put my changes on top of the server's version.
    fn reapply(&mut self, cx: &mut Context<Self>) {
        let Some(form) = &self.form else { return };
        let (Some(cur), Some(id)) = (form.conflict.clone(), form.id.clone()) else { return };
        let mine = self.changes(&form.base, cx);
        let s = app::session(cx);
        let cid = cur.cid.clone();
        if let Some(f) = self.form.as_mut() {
            f.base = cur;
            f.conflict = None;
        }
        log("conflict-reapply", &[("service", SVC), ("id", &id)]);
        let task =
            farfield_core::spawn(async move { bookmarks::update(&s, &id, &Value::Object(mine), Some(&cid)).await });
        self.put(false, task, cx);
    }

    fn take_theirs(&mut self, cx: &mut Context<Self>) {
        if let Some(cur) = self.form.as_ref().and_then(|f| f.conflict.clone()) {
            log("conflict-theirs", &[("service", SVC), ("id", &cur.id)]);
            self.set_form(cur, cx);
            self.want_images(cx);
        }
    }

    fn delete(&mut self, cx: &mut Context<Self>) {
        let Some(f) = &self.form else { return };
        let Some(id) = f.id.clone() else {
            self.form = None;
            cx.notify();
            return;
        };
        let cid = f.base.cid.clone();
        let name = if f.base.title.is_empty() { f.base.url.clone() } else { f.base.title.clone() };
        let me = self.me.clone();
        confirm(cx, "Delete this bookmark?", format!("“{name}”"), "Delete", true, move |_, cx| {
            let s = app::session(cx);
            let k = id.clone();
            let task = farfield_core::spawn(async move { bookmarks::delete(&s, &k, Some(&cid)).await });
            let _ = me.update(cx, |_, cx| {
                cx.spawn(async move |this, cx| {
                    let r = task.await;
                    let _ = this.update(cx, |this, cx| match r {
                        Ok(Ok(())) => {
                            log("bookmark-delete", &[("id", &id)]);
                            this.form = None;
                            toast(cx, "Deleted.", false);
                            this.reload(cx);
                        }
                        Ok(Err(ApiError::Precondition { current, .. })) => {
                            if let Some(f) = this.form.as_mut() {
                                f.conflict = ext_lists::bookmarks::from_conflict(&current);
                                f.error = Some("Not deleted — changed on the server.".into());
                            }
                            cx.notify();
                        }
                        Ok(Err(e)) => toast(cx, describe(&e), true),
                        Err(e) => toast(cx, e.to_string(), true),
                    });
                })
                .detach();
            });
        });
    }

    fn refresh_meta(&mut self, cx: &mut Context<Self>) {
        let Some(f) = self.form.as_mut() else { return };
        let Some(id) = f.id.clone() else { return };
        if f.refreshing {
            return;
        }
        f.refreshing = true;
        cx.notify();
        let s = app::session(cx);
        let task = farfield_core::spawn(async move { bookmarks::refresh(&s, &id).await });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                if let Some(f) = this.form.as_mut() {
                    f.refreshing = false;
                }
                match r {
                    Ok(Ok(v)) => {
                        log("bookmark-refresh", &[("id", &v.value.id)]);
                        let dirty = this.dirty_form(cx);
                        if this.selected_id() == Some(v.value.id.as_str()) {
                            // forget the old pictures so a changed og:image shows
                            let (og, fav) = (v.value.og_image.clone(), v.value.favicon.clone());
                            this.images.forget(&og);
                            this.images.forget(&fav);
                            if dirty {
                                if let Some(f) = this.form.as_mut() {
                                    f.base = v.value;
                                }
                            } else {
                                this.set_form(v.value, cx);
                            }
                            this.want_images(cx);
                        }
                        toast(cx, "Page details refreshed.", false);
                        this.reload(cx);
                    }
                    Ok(Err(e @ ApiError::Server { status: 502, .. })) => {
                        toast(cx, format!("Couldn't fetch the page. {}", describe(&e)), true)
                    }
                    Ok(Err(e)) => toast(cx, describe(&e), true),
                    Err(e) => toast(cx, e.to_string(), true),
                }
                cx.notify();
            });
        })
        .detach();
    }

    /// Fetch the selected bookmark's og image and favicon (no credentials).
    fn want_images(&mut self, cx: &mut Context<Self>) {
        let Some(f) = &self.form else { return };
        for (url, max) in [(f.base.og_image.clone(), 640), (f.base.favicon.clone(), 64)] {
            if url.is_empty() || !self.images.begin(&url) {
                continue;
            }
            let u = url.clone();
            let task = farfield_core::spawn(async move {
                let b = ext_lists::remote_image(&u).await.ok()?;
                kit::decode(&b, max, false)
            });
            cx.spawn(async move |this, cx| {
                let r = task.await.ok().flatten();
                let _ = this.update(cx, |this, cx| {
                    this.images.finish(&url, r);
                    this.want_images(cx);
                    cx.notify();
                });
            })
            .detach();
        }
    }

    fn copy_url(&mut self, cx: &mut Context<Self>) {
        let u = self.f_url.read(cx).text();
        if !u.is_empty() {
            kit::copy(cx, &u, "the URL");
        }
    }

    fn open_url(&mut self, cx: &mut Context<Self>) {
        let u = self.f_url.read(cx).text();
        if u.starts_with("http://") || u.starts_with("https://") {
            cx.open_url(&u);
        }
    }

    // ── rendering ──────────────────────────────────────────────────────

    fn render_head(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let total = self.items.len();
        let private = self.items.iter().filter(|b| !b.public).count();
        let cur = self.category.clone();
        let cats = self.categories();
        let me = self.me.clone();
        let vis = match self.vis {
            Vis::All => 0,
            Vis::Public => 1,
            Vis::Private => 2,
        };
        let cat_chip = |id: String, label: String, n: Option<usize>, on: bool| {
            div()
                .id(SharedString::from(id))
                .flex()
                .gap(px(4.))
                .text_xs()
                .cursor_pointer()
                .text_color(if on { t.accent } else { t.ink_2 })
                .hover(|s| s.underline())
                .child(label)
                .when_some(n, |d, n| d.child(div().font_family(FONT_MONO).text_color(t.ink_3).child(n.to_string())))
        };
        div()
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
                    .child(div().text_lg().font_weight(gpui::FontWeight::SEMIBOLD).text_color(t.ink).child("Bookmarks"))
                    .child(ui::mono(format!("{total} · {private} private"), cx))
                    .child(div().flex_1())
                    .child(ui::button("bm-new", "New  ⌘N", BtnKind::Quiet, cx, {
                        let me = me.clone();
                        move |_, w, cx| {
                            let _ = me.update(cx, |this, cx| this.new_bookmark(w, cx));
                        }
                    })),
            )
            .child(kit::seg("bm-vis", &["All", "Public", "Private"], vis, cx, {
                let me = me.clone();
                move |i, _, cx| {
                    let _ = me.update(cx, |this, cx| {
                        this.vis = [Vis::All, Vis::Public, Vis::Private][i];
                        cx.notify();
                    });
                }
            }))
            .child(
                div()
                    .flex()
                    .flex_wrap()
                    .gap_x(S3)
                    .gap_y(px(4.))
                    .child(cat_chip("cat-all".into(), "every category".into(), None, cur.is_none()).on_click({
                        let me = me.clone();
                        move |_, _, cx| {
                            let _ = me.update(cx, |this, cx| {
                                this.category = None;
                                cx.notify();
                            });
                        }
                    }))
                    .children(cats.into_iter().map(|(c, n)| {
                        let on = cur.as_deref() == Some(c.as_str());
                        let me = me.clone();
                        let pick = c.clone();
                        cat_chip(format!("cat-{c}"), c, Some(n), on).on_click(move |_, _, cx| {
                            let _ = me.update(cx, |this, cx| {
                                this.category = if this.category.as_deref() == Some(pick.as_str()) {
                                    None
                                } else {
                                    Some(pick.clone())
                                };
                                cx.notify();
                            });
                        })
                    })),
            )
            .child(self.search.clone())
            .into_any_element()
    }

    fn render_list(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let rows = self.rows(cx);
        let sel = self.selected_id().map(|s| s.to_string());
        let me = self.me.clone();
        let status = match (&self.error, &self.freshness) {
            (Some(e), _) => Some(ui::notice(e.clone(), t.bad, cx)),
            (None, Some(Freshness::Stale { age_ms, .. })) => Some(ui::notice(
                format!("Offline — showing what was loaded {} ago.", super::content::ago(*age_ms)),
                t.warn,
                cx,
            )),
            _ => None,
        };
        let body: AnyElement = if rows.is_empty() {
            let msg = if self.loading && !self.loaded {
                "Loading…"
            } else if self.error.is_some() && !self.loaded {
                "Couldn't load."
            } else if self.items.is_empty() {
                "No bookmarks yet."
            } else {
                "No matches."
            };
            ui::quiet_state(msg, cx).into_any_element()
        } else {
            div()
                .id("bm-list")
                .flex_1()
                .overflow_y_scroll()
                .track_scroll(&self.scroll)
                .children(rows.into_iter().enumerate().map(|(i, r)| {
                    match r {
                        Row::Header(c, n) => div()
                            .flex()
                            .items_baseline()
                            .justify_between()
                            .px(S4)
                            .pt(S4)
                            .pb(px(6.))
                            .border_b_1()
                            .border_color(t.rule)
                            .child(ui::eyebrow(if c.is_empty() { "Uncategorized".to_string() } else { c }, cx))
                            .child(ui::mono(n.to_string(), cx))
                            .into_any_element(),
                        Row::Item(b) => {
                            let on = sel.as_deref() == Some(b.id.as_str());
                            let me = me.clone();
                            let title = if !b.title.is_empty() {
                                b.title.clone()
                            } else if !b.og_title.is_empty() {
                                b.og_title.clone()
                            } else {
                                kit::host(&b.url)
                            };
                            let host = kit::host(&b.url);
                            let public = b.public;
                            let notes = !b.admin_notes.is_empty();
                            ui::list_row(("bm", i), on, &t)
                                .flex()
                                .flex_col()
                                .gap(px(2.))
                                .child(
                                    div()
                                        .flex()
                                        .justify_between()
                                        .gap(S2)
                                        .child(div().text_sm().text_color(t.ink).truncate().child(title))
                                        .when(!public, |d| d.child(ui::chip("private", t.ink_3, cx))),
                                )
                                .child(
                                    div()
                                        .flex()
                                        .gap(S2)
                                        .text_xs()
                                        .font_family(FONT_MONO)
                                        .text_color(t.ink_3)
                                        .child(div().truncate().child(host))
                                        .when(notes, |d| d.child(div().flex_none().child("· notes"))),
                                )
                                .on_click(move |_, w, cx| {
                                    let b = b.clone();
                                    let _ = me.update(cx, |this, cx| this.select(b, w, cx));
                                })
                                .into_any_element()
                        }
                    }
                }))
                .into_any_element()
        };
        div()
            .flex()
            .flex_col()
            .size_full()
            .child(self.render_head(cx))
            .when_some(status, |d, s| d.child(div().px(S4).py(S2).child(s)))
            .child(body)
            .into_any_element()
    }

    fn render_form(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let Some(f) = &self.form else {
            return div()
                .size_full()
                .flex()
                .flex_col()
                .justify_center()
                .items_center()
                .gap(S2)
                .child(div().text_sm().text_color(t.ink_2).child("No bookmark selected."))
                .into_any_element();
        };
        let me = self.me.clone();
        let is_new = f.id.is_none();
        let dirty = self.dirty_form(cx);
        let cats = bookmarks::categories(&self.items);
        let cur_cat = self.f_cat.read(cx).text();
        let heading = if is_new {
            "New bookmark".to_string()
        } else if !f.base.title.is_empty() {
            f.base.title.clone()
        } else {
            kit::host(&f.base.url)
        };
        let state = if f.saving {
            ("Saving…", t.accent)
        } else if f.conflict.is_some() {
            ("Conflict · changed on the server", t.bad)
        } else if is_new {
            ("Not saved yet", t.warn)
        } else if dirty {
            ("Unsaved changes", t.warn)
        } else {
            ("Saved", t.good)
        };
        let conflict = f.conflict.as_ref().map(|c| self.render_conflict(c, cx));
        let mut col = div()
            .w_full()
            .max_w(MEASURE)
            .flex()
            .flex_col()
            .gap(S3)
            .px(S6)
            .py(S5)
            .child(
                div()
                    .flex()
                    .flex_col()
                    .gap(S1)
                    .child(ui::eyebrow(if is_new { "Bookmark · new" } else { "Bookmark" }, cx))
                    .child(
                        div()
                            .font_family(FONT_DOC)
                            .text_size(px(26.))
                            .line_height(px(32.))
                            .text_color(t.ink)
                            .child(heading),
                    )
                    .child(div().flex().gap(S4).child(ui::chip(state.0, state.1, cx)).child(ui::chip(
                        if f.public { "public" } else { "private" },
                        if f.public { t.good } else { t.ink_3 },
                        cx,
                    ))),
            )
            .child(ui::rule(cx))
            .when_some(f.error.clone(), |d, e| d.child(ui::notice(e, t.bad, cx)))
            .when_some(conflict, |d, c| d.child(c))
            .child(self.f_url.clone())
            .child(self.f_title.clone())
            .child(self.f_desc.clone())
            .child(self.f_cat.clone());
        if !cats.is_empty() {
            col = col.child(div().flex().flex_wrap().gap_x(S3).gap_y(px(4.)).mt(px(-4.)).children(
                cats.into_iter().map(|c| {
                    let on = c == cur_cat;
                    let me = me.clone();
                    let pick = c.clone();
                    div()
                        .id(SharedString::from(format!("pick-{c}")))
                        .text_xs()
                        .cursor_pointer()
                        .text_color(if on { t.accent } else { t.ink_2 })
                        .hover(|s| s.underline())
                        .child(c)
                        .on_click(move |_, _, cx| {
                            let _ = me.update(cx, |this, cx| {
                                let p = pick.clone();
                                this.f_cat.update(cx, |f, cx| f.set_text(p, cx));
                                cx.notify();
                            });
                        })
                }),
            ));
        }
        col = col
            .child(kit::switch("bm-public", "Public", "", f.public, cx, {
                let me = me.clone();
                move |_, cx| {
                    let _ = me.update(cx, |this, cx| {
                        if let Some(f) = this.form.as_mut() {
                            f.public = !f.public;
                        }
                        cx.notify();
                    });
                }
            }))
            .child(self.f_notes.clone());
        // actions
        let mut actions = div().flex().gap(S2).pt(S2);
        if f.conflict.is_none() {
            actions = actions.child(if f.saving {
                ui::button_disabled("Saving…", cx).into_any_element()
            } else {
                ui::button(
                    "bm-save",
                    if is_new { "Save bookmark  ⌘S" } else { "Save changes  ⌘S" },
                    BtnKind::Primary,
                    cx,
                    {
                        let me = me.clone();
                        move |_, _, cx| {
                            let _ = me.update(cx, |this, cx| this.save(cx));
                        }
                    },
                )
                .into_any_element()
            });
        }
        if dirty && !is_new && f.conflict.is_none() {
            let base = f.base.clone();
            actions = actions.child(ui::button("bm-revert", "Revert", BtnKind::Quiet, cx, {
                let me = me.clone();
                move |_, _, cx| {
                    let b = base.clone();
                    let _ = me.update(cx, |this, cx| this.set_form(b, cx));
                }
            }));
        }
        if is_new {
            actions = actions.child(ui::button("bm-cancel", "Cancel", BtnKind::Quiet, cx, {
                let me = me.clone();
                move |_, _, cx| {
                    let _ = me.update(cx, |this, cx| {
                        this.form = None;
                        cx.notify();
                    });
                }
            }));
        }
        col = col.child(actions);
        div().id("bm-form").size_full().overflow_y_scroll().child(col).into_any_element()
    }

    /// The server's version beside yours, field by field.
    fn render_conflict(&self, cur: &Bookmark, cx: &mut Context<Self>) -> AnyElement {
        let me = self.me.clone();
        let mine = self.fields(cx);
        let theirs = serde_json::to_value(cur).unwrap_or(Value::Null);
        let base =
            self.form.as_ref().map(|f| serde_json::to_value(&f.base).unwrap_or(Value::Null)).unwrap_or(Value::Null);
        let show = |v: &Value| match v {
            Value::String(s) if s.is_empty() => "—".to_string(),
            Value::String(s) => s.clone(),
            Value::Bool(b) => if *b { "public" } else { "private" }.to_string(),
            Value::Null => "—".into(),
            v => v.to_string(),
        };
        let labels = [
            ("url", "URL"),
            ("title", "Title"),
            ("description", "Description"),
            ("category", "Category"),
            ("public", "Visibility"),
            ("adminNotes", "Admin notes"),
        ];
        let mut diffs = Vec::new();
        for (k, label) in labels {
            let a = show(&mine.get(k).cloned().unwrap_or(Value::Null));
            let b = show(&theirs.get(k).cloned().unwrap_or(json!("")));
            let was = show(&base.get(k).cloned().unwrap_or(json!("")));
            if a != b {
                diffs.push((label, a.clone(), b, a != was));
            }
        }
        div()
            .flex()
            .flex_col()
            .gap(S2)
            .pb(S2)
            .child(ui::eyebrow("Resolve the conflict", cx))
            .child(kit::diff_rows(diffs, &cur.updated_at, cx))
            .child(
                div()
                    .flex()
                    .gap(S2)
                    .pt(S2)
                    .child(ui::button("bm-reapply", "Reapply my changes", BtnKind::Primary, cx, {
                        let me = me.clone();
                        move |_, _, cx| {
                            let _ = me.update(cx, |this, cx| this.reapply(cx));
                        }
                    }))
                    .child(ui::button("bm-theirs", "Use the server's version", BtnKind::Quiet, cx, {
                        let me = me.clone();
                        move |_, _, cx| {
                            let me = me.clone();
                            confirm(
                                cx,
                                "Use the server's version?",
                                "Your edits are dropped.",
                                "Use theirs",
                                true,
                                move |_, cx| {
                                    let _ = me.update(cx, |this, cx| this.take_theirs(cx));
                                },
                            );
                        }
                    })),
            )
            .into_any_element()
    }
}

impl Workspace for BookmarksWs {
    fn inspector(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> Option<AnyElement> {
        let t = theme(cx).clone();
        let f = self.form.as_ref()?;
        let me = self.me.clone();
        let mut col = div().flex().flex_col().gap(S2);
        if f.id.is_none() {
            return Some(col.child(ui::eyebrow("New bookmark", cx)).into_any_element());
        }
        let b = &f.base;
        // the page, as it describes itself
        col = col.child(ui::eyebrow("The page", cx));
        match (b.og_image.is_empty(), self.images.get(&b.og_image)) {
            (false, Some(Slot::Ready(..))) => {
                let (im, w, h) = self.images.ready(&b.og_image).unwrap();
                let width = 268.0f32;
                let height = (width * h as f32 / w.max(1) as f32).clamp(40.0, 220.0);
                col = col.child(img(im).w(px(width)).h(px(height)).object_fit(ObjectFit::Cover).rounded(px(4.)));
            }
            (false, Some(Slot::Loading)) => col = col.child(div().w(px(268.)).h(px(140.)).rounded(px(4.)).bg(t.wash)),
            (false, Some(Slot::Failed)) => {
                col = col.child(div().text_xs().text_color(t.ink_3).child("Image failed to load."))
            }
            _ => {}
        }
        let site = if !b.og_site_name.is_empty() { b.og_site_name.clone() } else { kit::host(&b.url) };
        let fav = self.images.ready(&b.favicon);
        col = col.child(
            div()
                .flex()
                .items_center()
                .gap(S2)
                .pt(S1)
                .when_some(fav, |d, (im, _, _)| d.child(img(im).w(px(16.)).h(px(16.)).object_fit(ObjectFit::Contain)))
                .child(div().text_sm().font_weight(gpui::FontWeight::MEDIUM).text_color(t.ink).child(site)),
        );
        if !b.og_title.is_empty() {
            col = col.child(div().font_family(FONT_DOC).text_size(px(16.)).text_color(t.ink).child(b.og_title.clone()));
        }
        if !b.og_description.is_empty() {
            col = col.child(div().text_sm().text_color(t.ink_2).child(b.og_description.clone()));
        }
        if b.og_title.is_empty() && b.og_image.is_empty() {
            col = col.child(div().text_xs().text_color(t.ink_3).child("No page details."));
        }
        let refreshing = f.refreshing;
        col = col
            .child(
                div()
                    .flex()
                    .flex_wrap()
                    .gap(px(2.))
                    .pt(S1)
                    .child(if refreshing {
                        ui::button_disabled("Refreshing…", cx).into_any_element()
                    } else {
                        ui::button("bm-refresh", "Refresh details", BtnKind::Quiet, cx, {
                            let me = me.clone();
                            move |_, _, cx| {
                                let _ = me.update(cx, |this, cx| this.refresh_meta(cx));
                            }
                        })
                        .into_any_element()
                    })
                    .child(ui::button("bm-copy", "Copy URL", BtnKind::Quiet, cx, {
                        let me = me.clone();
                        move |_, _, cx| {
                            let _ = me.update(cx, |this, cx| this.copy_url(cx));
                        }
                    }))
                    .child(ui::button("bm-open", "Open ↗", BtnKind::Quiet, cx, {
                        let me = me.clone();
                        move |_, _, cx| {
                            let _ = me.update(cx, |this, cx| this.open_url(cx));
                        }
                    })),
            )
            .child(ui::rule(cx))
            .child(ui::eyebrow("Record", cx))
            .child(ui::field_row(
                "Visibility",
                ui::chip(if b.public { "public" } else { "private" }, if b.public { t.good } else { t.ink_3 }, cx),
                cx,
            ))
            .child(ui::field_row("ID", ui::mono(b.id.clone(), cx), cx))
            .child(ui::field_row("CID", ui::mono(b.cid.clone(), cx), cx))
            .child(ui::field_row(
                "Saved",
                ui::mono(format!("{} · created {}", ui::when(&b.updated_at), ui::when(&b.created_at)), cx),
                cx,
            ))
            .child(ui::rule(cx))
            .child(ui::button("bm-delete", "Delete bookmark…", BtnKind::Danger, cx, move |_, _, cx| {
                let _ = me.update(cx, |this, cx| this.delete(cx));
            }));
        Some(col.into_any_element())
    }

    fn palette(&self, _cx: &App) -> Vec<PaletteItem> {
        let me = self.me.clone();
        vec![PaletteItem::new("Bookmarks: new bookmark", "⌘N", move |w, cx| {
            crate::shell::goto(cx, SVC);
            let _ = me.update(cx, |this, cx| this.new_bookmark(w, cx));
        })]
    }

    fn commands(&self, _cx: &App) -> Vec<(&'static str, String, &'static str)> {
        let mut v =
            vec![("new", "Bookmarks: new bookmark".to_string(), "⌘N"), ("reload", "Bookmarks: reload".into(), "⌘R")];
        if let Some(f) = &self.form {
            v.push(("save", "Bookmark: save".into(), "⌘S"));
            if f.id.is_some() {
                v.push(("refresh-meta", "Bookmark: refresh page details".into(), ""));
                v.push(("copy-url", "Bookmark: copy URL".into(), ""));
                v.push(("open", "Bookmark: open in browser".into(), ""));
                v.push(("delete", "Bookmark: delete…".into(), ""));
            }
            if f.conflict.is_some() {
                v.push(("reapply", "Bookmark: reapply my changes".into(), ""));
                v.push(("theirs", "Bookmark: use the server's version".into(), ""));
            }
        }
        v
    }

    fn run_command(&mut self, id: &str, w: &mut Window, cx: &mut Context<Self>) {
        match id {
            "new" => self.new_bookmark(w, cx),
            "reload" => self.reload(cx),
            "save" => self.save(cx),
            "refresh-meta" => self.refresh_meta(cx),
            "copy-url" => self.copy_url(cx),
            "open" => self.open_url(cx),
            "delete" => self.delete(cx),
            "reapply" => self.reapply(cx),
            "theirs" => self.take_theirs(cx),
            _ => {}
        }
    }

    fn focus_search(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        self.search.read(cx).focus(w);
    }
    fn new_item(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        self.new_bookmark(w, cx)
    }
    fn save(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        BookmarksWs::save(self, cx)
    }
    fn refresh(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        self.reload(cx)
    }
    fn dirty(&self, cx: &App) -> bool {
        self.dirty_form(cx)
    }
}

impl Render for BookmarksWs {
    fn render(&mut self, w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        self.images.drop_retired(w);
        let t = theme(cx).clone();
        div()
            .size_full()
            .flex()
            .child(div().w(px(340.)).flex_none().h_full().border_r_1().border_color(t.rule).child(self.render_list(cx)))
            .child(div().flex_1().min_w_0().h_full().child(self.render_form(cx)))
    }
}
