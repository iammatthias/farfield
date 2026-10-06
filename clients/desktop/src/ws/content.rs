//! Content: collections and series, drafts and published entries.
//!
//! The list pages through the server (a short page is the end), revalidating
//! with ETags; every open document is kept, so moving between entries or
//! workspaces never loses an edit. Local drafts on this Mac — including ones
//! from a session that ended without saving — are listed first.

use crate::app::{self, describe, log, Health};
use crate::shell::{confirm, set_health, toast};
use crate::theme::{theme, FONT_MONO, S2, S3, S4};
use crate::ui::draft_doc::{DraftDoc, DraftEvent, FieldKind, FieldSpec};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, Kind as BtnKind};
use crate::workspace::{PaletteItem, Workspace};
use farfield_core::api::content::{self, Collection, Entry, Series, Status};
use farfield_core::store::{Draft, SaveState};
use farfield_core::sync::{self, ContentEntry, ContentSeries};
use farfield_core::{Freshness, Latest};
use gpui::{
    div, prelude::*, px, uniform_list, AnyElement, App, Context, Entity, SharedString, UniformListScrollHandle, Window,
};
use serde_json::{json, Value};
use std::collections::HashMap;
use std::sync::Arc;

const PAGE: u32 = 50;

#[derive(Clone, Copy, PartialEq)]
enum Mode {
    Entries,
    Series,
}

#[derive(Clone)]
enum Row {
    Draft(Draft),
    Entry(Entry),
    Series(Series),
}

enum Open {
    Entry(Entity<DraftDoc<ContentEntry>>),
    Series(Entity<DraftDoc<ContentSeries>>),
}

pub struct ContentWs {
    mode: Mode,
    status: Status,
    collection: Option<String>,
    collections: Vec<Collection>,
    entries: Vec<Entry>,
    series: Vec<Series>,
    drafts: Vec<Draft>,
    page: u32,
    has_more: bool,
    loading: bool,
    freshness: Option<Freshness>,
    error: Option<String>,
    latest: Arc<Latest>,
    search: Entity<TextField>,
    scroll: UniformListScrollHandle,
    open: HashMap<String, Open>,
    selected: Option<String>,
}

fn entry_specs() -> Vec<FieldSpec> {
    vec![
        FieldSpec { key: "collection", label: "Collection", placeholder: "collection slug", kind: FieldKind::Mono },
        FieldSpec { key: "slug", label: "Slug", placeholder: "from the title", kind: FieldKind::Mono },
        FieldSpec { key: "tags", label: "Tags", placeholder: "comma, separated", kind: FieldKind::Tags },
        FieldSpec {
            key: "excerpt",
            label: "Excerpt",
            placeholder: "a line for lists and feeds",
            kind: FieldKind::Text,
        },
    ]
}

impl ContentWs {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        let search = cx.new(|cx| TextField::new(w, cx, "", "Filter this list  ⌘F"));
        cx.subscribe_in(&search, w, |this: &mut Self, _, e: &FieldEvent, w, cx| match e {
            FieldEvent::Changed => cx.notify(),
            FieldEvent::Down => this.step(1, w, cx),
            FieldEvent::Up => this.step(-1, w, cx),
            FieldEvent::Submit => this.focus_doc(w, cx),
            _ => {}
        })
        .detach();
        let mut this = ContentWs {
            mode: Mode::Entries,
            status: Status::All,
            collection: None,
            collections: Vec::new(),
            entries: Vec::new(),
            series: Vec::new(),
            drafts: Vec::new(),
            page: 1,
            has_more: false,
            loading: false,
            freshness: None,
            error: None,
            latest: Arc::new(Latest::default()),
            search,
            scroll: UniformListScrollHandle::new(),
            open: HashMap::new(),
            selected: None,
        };
        this.reload(cx);
        this
    }

    fn draft_area(&self) -> &'static str {
        if self.mode == Mode::Entries {
            "content"
        } else {
            "content-series"
        }
    }

    fn load_drafts(&mut self, cx: &mut Context<Self>) {
        let s = app::session(cx);
        let area = self.draft_area();
        let task = farfield_core::spawn(async move { s.drafts(area).map(|d| d.list(area)).unwrap_or_default() });
        cx.spawn(async move |this, cx| {
            if let Ok(list) = task.await {
                let _ = this.update(cx, |this, cx| {
                    this.drafts = list.into_iter().filter(|d| d.state != SaveState::Saved).collect();
                    cx.notify();
                });
            }
        })
        .detach();
    }

    /// Load (or revalidate) page 1 of the current view.
    fn reload(&mut self, cx: &mut Context<Self>) {
        self.page = 1;
        self.load(false, cx);
        self.load_drafts(cx);
        let s = app::session(cx);
        let t = farfield_core::spawn(async move { content::collections(&s).await });
        cx.spawn(async move |this, cx| {
            if let Ok(Ok(c)) = t.await {
                let _ = this.update(cx, |this, cx| {
                    this.collections = c.value;
                    cx.notify();
                });
            }
        })
        .detach();
    }

    fn load(&mut self, append: bool, cx: &mut Context<Self>) {
        let ticket = self.latest.ticket();
        let latest = self.latest.clone();
        let s = app::session(cx);
        let (mode, status, col, page) = (self.mode, self.status, self.collection.clone(), self.page);
        self.loading = true;
        cx.notify();
        let task = farfield_core::spawn(async move {
            match mode {
                Mode::Entries => content::entries(&s, col.as_deref(), status, page, PAGE).await.map(|l| {
                    (l.value.items.into_iter().map(Row::Entry).collect::<Vec<_>>(), l.value.has_more, l.freshness)
                }),
                Mode::Series => content::series_list(&s)
                    .await
                    .map(|l| (l.value.into_iter().map(Row::Series).collect::<Vec<_>>(), false, l.freshness)),
            }
        });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                // a newer request superseded this one: drop it
                if !latest.is_current(ticket) {
                    return;
                }
                this.loading = false;
                match r {
                    Ok(Ok((rows, more, fresh))) => {
                        if !append {
                            this.entries.clear();
                            this.series.clear();
                        }
                        for r in rows {
                            match r {
                                Row::Entry(e) => this.entries.push(e),
                                Row::Series(s) => this.series.push(s),
                                Row::Draft(_) => {}
                            }
                        }
                        this.has_more = more;
                        this.error = None;
                        let h = match &fresh {
                            Freshness::Live => Health::Up,
                            Freshness::Stale { error, .. } => Health::Down(error.to_string()),
                        };
                        set_health(cx, "content", h);
                        this.freshness = Some(fresh);
                    }
                    Ok(Err(e)) => {
                        if e.is_auth() {
                            set_health(cx, "content", Health::NoAuth);
                        } else if e.is_offline() {
                            set_health(cx, "content", Health::Down(e.to_string()));
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

    fn load_more(&mut self, cx: &mut Context<Self>) {
        if self.loading || !self.has_more {
            return;
        }
        self.page += 1;
        self.load(true, cx);
    }

    fn rows(&self, cx: &App) -> Vec<Row> {
        let q = self.search.read(cx).text().to_lowercase();
        let hit = |title: &str, slug: &str| {
            q.is_empty() || title.to_lowercase().contains(&q) || slug.to_lowercase().contains(&q)
        };
        let mut out: Vec<Row> = self
            .drafts
            .iter()
            .filter(|d| hit(d.local["title"].as_str().unwrap_or(""), &d.key))
            .cloned()
            .map(Row::Draft)
            .collect();
        let draft_keys: std::collections::HashSet<&str> = self.drafts.iter().map(|d| d.key.as_str()).collect();
        match self.mode {
            Mode::Entries => out.extend(
                self.entries
                    .iter()
                    .filter(|e| !draft_keys.contains(e.slug.as_str()) && hit(&e.title, &e.slug))
                    .cloned()
                    .map(Row::Entry),
            ),
            Mode::Series => out.extend(
                self.series
                    .iter()
                    .filter(|s| !draft_keys.contains(s.slug.as_str()) && hit(&s.title, &s.slug))
                    .cloned()
                    .map(Row::Series),
            ),
        }
        out
    }

    fn select_row(&mut self, row: Row, w: &mut Window, cx: &mut Context<Self>) {
        let key = match &row {
            Row::Draft(d) => d.key.clone(),
            Row::Entry(e) => e.slug.clone(),
            Row::Series(s) => s.slug.clone(),
        };
        self.selected = Some(key.clone());
        if self.open.contains_key(&key) {
            cx.notify();
            return;
        }
        match row {
            Row::Draft(d) => self.open_draft(d, w, cx),
            _ => {
                let s = app::session(cx);
                let k = key.clone();
                let mode = self.mode;
                let task = farfield_core::spawn(async move {
                    match mode {
                        Mode::Entries => sync::open::<ContentEntry>(&s, &k).await,
                        Mode::Series => sync::open::<ContentSeries>(&s, &k).await,
                    }
                });
                cx.spawn_in(w, async move |this, cx| {
                    let r = task.await;
                    let _ = this.update_in(cx, |this, w, cx| match r {
                        Ok(Ok(d)) => this.open_draft(d, w, cx),
                        Ok(Err(e)) => toast(cx, describe(&e), true),
                        Err(e) => toast(cx, e.to_string(), true),
                    });
                })
                .detach();
            }
        }
        cx.notify();
    }

    fn open_draft(&mut self, d: Draft, w: &mut Window, cx: &mut Context<Self>) {
        let key = d.key.clone();
        let open = if d.service == "content-series" {
            let doc = cx.new(|cx| {
                DraftDoc::<ContentSeries>::new(d, Some("title"), vec![], "Images, one per line: ![](blob://…)", w, cx)
            });
            self.watch(&doc, cx);
            Open::Series(doc)
        } else {
            let doc =
                cx.new(|cx| DraftDoc::<ContentEntry>::new(d, Some("title"), entry_specs(), "Write something…", w, cx));
            self.watch(&doc, cx);
            Open::Entry(doc)
        };
        self.open.insert(key.clone(), open);
        self.selected = Some(key);
        cx.notify();
    }

    fn watch<K: farfield_core::sync::Kind + 'static>(&mut self, doc: &Entity<DraftDoc<K>>, cx: &mut Context<Self>) {
        cx.subscribe(doc, |this, doc, e: &DraftEvent, cx| match e {
            DraftEvent::Renamed { from, to } => {
                if let Some(o) = this.open.remove(from) {
                    this.open.insert(to.clone(), o);
                }
                if this.selected.as_deref() == Some(from) {
                    this.selected = Some(to.clone());
                }
                let _ = doc;
                this.reload(cx);
            }
            DraftEvent::Saved => {
                this.load_drafts(cx);
                this.page = 1;
                this.load(false, cx);
            }
            DraftEvent::Touched => {
                this.load_drafts(cx);
            }
        })
        .detach();
    }

    fn new_entry(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        let local = match self.mode {
            Mode::Entries => {
                let col = self
                    .collection
                    .clone()
                    .or_else(|| self.collections.first().map(|c| c.slug.clone()))
                    .unwrap_or_default();
                if col.is_empty() {
                    toast(cx, "Create a collection in the content console first — there's none to write into.", true);
                    return;
                }
                serde_json::to_value(Entry { collection: col, ..Default::default() }).unwrap()
            }
            Mode::Series => json!({"slug": "", "title": "", "body": ""}),
        };
        let mut d = sync::new_draft(self.draft_area(), local);
        d.service = self.draft_area().into();
        log("new", &[("area", self.draft_area()), ("key", &d.key)]);
        let s = app::session(cx);
        let dd = d.clone();
        farfield_core::spawn(async move {
            if let Ok(dr) = s.drafts(&dd.service) {
                let _ = dr.save(&dd);
            }
        });
        self.open_draft(d, w, cx);
        self.focus_title(w, cx);
    }

    /// Move the selection through the (filtered) list from the keyboard.
    fn step(&mut self, by: i32, w: &mut Window, cx: &mut Context<Self>) {
        let rows = self.rows(cx);
        if rows.is_empty() {
            return;
        }
        let key_of = |r: &Row| match r {
            Row::Draft(d) => d.key.clone(),
            Row::Entry(e) => e.slug.clone(),
            Row::Series(s) => s.slug.clone(),
        };
        let cur = self.selected.as_ref().and_then(|k| rows.iter().position(|r| key_of(r) == *k));
        let next = match cur {
            None => 0,
            Some(i) => (i as i32 + by).clamp(0, rows.len() as i32 - 1) as usize,
        };
        self.scroll.scroll_to_item(next, gpui::ScrollStrategy::Center);
        self.select_row(rows[next].clone(), w, cx);
    }

    /// Enter from the list: put the caret in the open document.
    fn focus_doc(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        let Some(k) = &self.selected else { return };
        match self.open.get(k) {
            Some(Open::Entry(d)) => d.read(cx).editor.read(cx).focus_editor(w),
            Some(Open::Series(d)) => d.read(cx).editor.read(cx).focus_editor(w),
            None => {}
        }
    }

    fn focus_title(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        let Some(k) = &self.selected else { return };
        match self.open.get(k) {
            Some(Open::Entry(d)) => {
                if let Some(t) = d.read(cx).title.clone() {
                    t.read(cx).focus(w)
                }
            }
            Some(Open::Series(d)) => {
                if let Some(t) = d.read(cx).title.clone() {
                    t.read(cx).focus(w)
                }
            }
            None => {}
        }
    }

    fn publish(&mut self, on: bool, cx: &mut Context<Self>) {
        let Some(Open::Entry(doc)) = self.selected.as_ref().and_then(|k| self.open.get(k)) else { return };
        let doc = doc.clone();
        let title = doc.update(cx, |d, cx| d.current(cx)["title"].as_str().unwrap_or("").to_string());
        let (head, body, act) = if on {
            (
                "Publish this entry?",
                format!(
                    "“{title}” goes live on the site's next rebuild. Its slug, CID and first-published date are kept."
                ),
                "Publish",
            )
        } else {
            (
                "Unpublish this entry?",
                format!("“{title}” returns to drafts. Its publishedAt is kept for when it returns."),
                "Unpublish",
            )
        };
        confirm(cx, head, body, act, !on, move |_, cx| {
            log(if on { "publish" } else { "unpublish" }, &[("title", &title)]);
            doc.update(cx, |d, cx| d.set_and_save("published", Value::Bool(on), cx))
        });
    }

    fn delete(&mut self, cx: &mut Context<Self>) {
        let Some(key) = self.selected.clone() else { return };
        let Some(open) = self.open.get(&key) else { return };
        let (is_series, base_etag, has_base, title) = match open {
            Open::Entry(d) => {
                let d = d.read(cx);
                (
                    false,
                    d.draft.base_etag.clone(),
                    d.draft.base.is_some(),
                    d.draft.local["title"].as_str().unwrap_or("").to_string(),
                )
            }
            Open::Series(d) => {
                let d = d.read(cx);
                (
                    true,
                    d.draft.base_etag.clone(),
                    d.draft.base.is_some(),
                    d.draft.local["title"].as_str().unwrap_or("").to_string(),
                )
            }
        };
        let ent = cx.entity();
        let area = if is_series { "content-series" } else { "content" };
        if !has_base {
            confirm(
                cx,
                "Discard this draft?",
                "It was never saved to the server; this removes it from this Mac.",
                "Discard",
                true,
                move |_, cx| {
                    let s = app::session(cx);
                    if let Ok(d) = s.drafts(area) {
                        let _ = d.discard(area, &key);
                    }
                    ent.update(cx, |this, cx| {
                        this.open.remove(&key);
                        this.selected = None;
                        this.load_drafts(cx);
                        cx.notify();
                    });
                },
            );
            return;
        }
        let body = if is_series {
            "The series is deleted. The server refuses while an entry still embeds it.".to_string()
        } else {
            format!("“{title}” moves to the server's trash (kept 30 days, restorable from the content console).")
        };
        confirm(
            cx,
            if is_series { "Delete this series?" } else { "Delete this entry?" },
            body,
            "Delete",
            true,
            move |_, cx| {
                let s = app::session(cx);
                let k = key.clone();
                let task = farfield_core::spawn(async move {
                    if is_series {
                        content::delete_series(&s, &k, base_etag.as_deref()).await
                    } else {
                        content::delete(&s, &k, base_etag.as_deref()).await
                    }
                });
                ent.update(cx, |_, cx| {
                cx.spawn(async move |this, cx| {
                    let r = task.await;
                    let _ = this.update(cx, |this, cx| match r {
                        Ok(Ok(())) => {
                            log("delete", &[("area", area), ("key", &key)]);
                            let s = app::session(cx);
                            if let Ok(d) = s.drafts(area) {
                                let _ = d.discard(area, &key);
                            }
                            this.open.remove(&key);
                            this.selected = None;
                            toast(cx, "Deleted.", false);
                            this.reload(cx);
                        }
                        Ok(Err(farfield_core::ApiError::Precondition { .. })) => {
                            toast(cx, "Not deleted: it changed on the server since you opened it. Reopen it to see the change.", true)
                        }
                        Ok(Err(farfield_core::ApiError::Conflict { message, .. })) => toast(cx, format!("Not deleted: {message}"), true),
                        Ok(Err(e)) => toast(cx, describe(&e), true),
                        Err(e) => toast(cx, e.to_string(), true),
                    });
                })
                .detach();
            });
            },
        );
    }

    fn set_mode(&mut self, m: Mode, cx: &mut Context<Self>) {
        if self.mode != m {
            self.mode = m;
            self.reload(cx);
        }
    }

    fn render_filters(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let tab = |id: &'static str, label: &'static str, on: bool| {
            div()
                .id(id)
                .px(S2)
                .py(px(3.))
                .text_sm()
                .cursor_pointer()
                .border_b_2()
                .border_color(if on { t.accent } else { gpui::transparent_black() })
                .text_color(if on { t.ink } else { t.ink_2 })
                .child(label)
        };
        let mut d = div().flex().flex_col().gap(S2).px(S4).pt(S3).pb(S2).border_b_1().border_color(t.rule);
        d = d.child(
            div()
                .flex()
                .gap(S2)
                .child(
                    tab("m-entries", "Entries", self.mode == Mode::Entries)
                        .on_click(cx.listener(|this, _, _, cx| this.set_mode(Mode::Entries, cx))),
                )
                .child(
                    tab("m-series", "Series", self.mode == Mode::Series)
                        .on_click(cx.listener(|this, _, _, cx| this.set_mode(Mode::Series, cx))),
                )
                .child(div().flex_1())
                .child(ui::button("new", "New  ⌘N", BtnKind::Quiet, cx, {
                    let e = cx.entity();
                    move |_, w, cx| e.update(cx, |this, cx| this.new_entry(w, cx))
                })),
        );
        if self.mode == Mode::Entries {
            let st = self.status;
            d = d.child(
                div()
                    .flex()
                    .gap(S2)
                    .child(tab("s-all", "All", st == Status::All).on_click(cx.listener(|this, _, _, cx| {
                        this.status = Status::All;
                        this.reload(cx)
                    })))
                    .child(tab("s-drafts", "Drafts", st == Status::Drafts).on_click(cx.listener(|this, _, _, cx| {
                        this.status = Status::Drafts;
                        this.reload(cx)
                    })))
                    .child(tab("s-pub", "Published", st == Status::Published).on_click(cx.listener(
                        |this, _, _, cx| {
                            this.status = Status::Published;
                            this.reload(cx)
                        },
                    ))),
            );
            let cols = self.collections.clone();
            let cur = self.collection.clone();
            d = d.child(
                div()
                    .flex()
                    .flex_wrap()
                    .gap(px(6.))
                    .child(
                        div()
                            .id("col-all")
                            .text_xs()
                            .cursor_pointer()
                            .text_color(if cur.is_none() { t.ink } else { t.ink_3 })
                            .child("every collection")
                            .on_click(cx.listener(|this, _, _, cx| {
                                this.collection = None;
                                this.reload(cx)
                            })),
                    )
                    .children(cols.into_iter().map(|c| {
                        let slug = c.slug.clone();
                        let on = cur.as_deref() == Some(&c.slug);
                        div()
                            .id(SharedString::from(format!("col-{}", c.slug)))
                            .text_xs()
                            .cursor_pointer()
                            .text_color(if on { t.accent } else { t.ink_2 })
                            .child(format!("{} {}", c.name, c.entry_count))
                            .on_click(cx.listener(move |this, _, _, cx| {
                                this.collection = Some(slug.clone());
                                this.reload(cx)
                            }))
                    })),
            );
        }
        d.child(self.search.clone()).into_any_element()
    }

    fn render_list(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let rows = self.rows(cx);
        let n = rows.len();
        let selected = self.selected.clone();
        let more = self.has_more;
        let loading = self.loading;
        let ent = cx.entity();
        let rows = Arc::new(rows);
        let list = uniform_list("content-list", n + usize::from(more), {
            let rows = rows.clone();
            move |range, _w, cx| {
                let t = theme(cx).clone();
                range
                    .map(|i| {
                        if i >= rows.len() {
                            let e = ent.clone();
                            return div()
                                .id(("more", i))
                                .px(S4)
                                .py(px(10.))
                                .text_sm()
                                .text_color(t.accent)
                                .cursor_pointer()
                                .child(if loading { "Loading…" } else { "Load more" })
                                .on_click(move |_, _, cx| e.update(cx, |this, cx| this.load_more(cx)))
                                .into_any_element();
                        }
                        let row = rows[i].clone();
                        let (key, title, sub, chip) = match &row {
                            Row::Draft(d) => (
                                d.key.clone(),
                                d.local["title"].as_str().filter(|s| !s.is_empty()).unwrap_or("Untitled").to_string(),
                                if d.base.is_none() {
                                    "new · on this Mac".to_string()
                                } else {
                                    format!("{} · on this Mac", d.key)
                                },
                                Some((
                                    match d.state {
                                        SaveState::Conflict => "conflict",
                                        SaveState::Pending => "pending",
                                        _ => "unsaved",
                                    },
                                    if d.state == SaveState::Conflict { t.bad } else { t.warn },
                                )),
                            ),
                            Row::Entry(e) => (
                                e.slug.clone(),
                                if e.title.is_empty() { "Untitled".into() } else { e.title.clone() },
                                format!(
                                    "{} · {}",
                                    e.collection,
                                    ui::when(if e.published_at.is_empty() { &e.updated_at } else { &e.published_at })
                                ),
                                Some(if e.published { ("published", t.good) } else { ("draft", t.ink_3) }),
                            ),
                            Row::Series(s) => (
                                s.slug.clone(),
                                if s.title.is_empty() { s.slug.clone() } else { s.title.clone() },
                                format!("{} images", farfield_core::merge::refs(&s.body).len()),
                                None,
                            ),
                        };
                        let on = selected.as_deref() == Some(key.as_str());
                        let e = ent.clone();
                        ui::list_row(("row", i), on, &t)
                            .flex()
                            .flex_col()
                            .gap(px(2.))
                            .child(
                                div()
                                    .flex()
                                    .justify_between()
                                    .gap(S2)
                                    .child(div().text_sm().text_color(t.ink).truncate().child(title))
                                    .when_some(chip, |d, (w, c)| d.child(ui::chip(w, c, cx))),
                            )
                            .child(div().text_xs().font_family(FONT_MONO).text_color(t.ink_3).truncate().child(sub))
                            .on_click(move |_, w, cx| e.update(cx, |this, cx| this.select_row(row.clone(), w, cx)))
                            .into_any_element()
                    })
                    .collect()
            }
        })
        .track_scroll(self.scroll.clone())
        .flex_1();
        let status_line = match (&self.error, &self.freshness) {
            (Some(e), _) => Some(ui::notice(e.clone(), t.bad, cx)),
            (None, Some(Freshness::Stale { age_ms, .. })) => {
                Some(ui::notice(format!("Offline — showing what was loaded {} ago.", ago(*age_ms)), t.warn, cx))
            }
            _ => None,
        };
        div()
            .flex()
            .flex_col()
            .size_full()
            .child(self.render_filters(cx))
            .when_some(status_line, |d, s| d.child(div().px(S4).py(S2).child(s)))
            .child(if n == 0 && !self.loading && self.error.is_none() {
                ui::quiet_state(
                    if self.mode == Mode::Entries { "No entries here yet. ⌘N starts one." } else { "No series yet." },
                    cx,
                )
                .into_any_element()
            } else {
                list.into_any_element()
            })
            .into_any_element()
    }
}

pub fn ago(ms: u64) -> String {
    let s = ms / 1000;
    match s {
        0..=59 => format!("{s}s"),
        60..=3599 => format!("{}m", s / 60),
        3600..=86399 => format!("{}h", s / 3600),
        _ => format!("{}d", s / 86400),
    }
}

impl Workspace for ContentWs {
    fn inspector(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> Option<AnyElement> {
        let t = theme(cx).clone();
        let key = self.selected.clone()?;
        let open = self.open.get(&key)?;
        let mut col = div().flex().flex_col().gap(S2);
        match open {
            Open::Entry(doc) => {
                let doc = doc.clone();
                let (published, published_at, cid, created) = {
                    let d = &doc.read(cx).draft;
                    (
                        d.local["published"].as_bool().unwrap_or(false),
                        d.local["publishedAt"].as_str().unwrap_or("").to_string(),
                        d.local["cid"].as_str().unwrap_or("").to_string(),
                        d.local["createdAt"].as_str().unwrap_or("").to_string(),
                    )
                };
                col = col.child(ui::eyebrow("Entry", cx)).children(doc.update(cx, |d, cx| d.render_inspector(cx)));
                col = col.child(ui::rule(cx)).child(ui::eyebrow("Publishing", cx)).child(ui::chip(
                    if published { "published" } else { "draft — not on the site" },
                    if published { t.good } else { t.ink_3 },
                    cx,
                ));
                if !published_at.is_empty() {
                    col = col.child(ui::field_row("First published", ui::mono(published_at, cx), cx));
                }
                let e = cx.entity();
                let e2 = cx.entity();
                col = col.child(
                    div()
                        .flex()
                        .gap(S2)
                        .child(if published {
                            ui::button("unpublish", "Unpublish…", BtnKind::Quiet, cx, move |_, _, cx| {
                                e.update(cx, |this, cx| this.publish(false, cx))
                            })
                        } else {
                            ui::button("publish", "Publish…", BtnKind::Primary, cx, move |_, _, cx| {
                                e.update(cx, |this, cx| this.publish(true, cx))
                            })
                        })
                        .child(ui::button("delete", "Delete…", BtnKind::Danger, cx, move |_, _, cx| {
                            e2.update(cx, |this, cx| this.delete(cx))
                        })),
                );
                if !cid.is_empty() {
                    col = col.child(ui::field_row("CID", ui::mono(cid, cx), cx));
                }
                if !created.is_empty() {
                    col = col.child(ui::field_row("Created", ui::mono(created, cx), cx));
                }
                if let Some(p) = app::session(cx).public_base("content") {
                    let _ = p;
                }
            }
            Open::Series(doc) => {
                let doc = doc.clone();
                col = col.child(ui::eyebrow("Series", cx)).children(doc.update(cx, |d, cx| d.render_inspector(cx)));
                let slug = doc.read(cx).draft.key.clone();
                col = col.child(ui::field_row("Embed in an entry", ui::mono(format!("![](series://{slug})"), cx), cx));
                let e = cx.entity();
                col =
                    col.child(ui::button("delete-series", "Delete series…", BtnKind::Danger, cx, move |_, _, cx| {
                        e.update(cx, |this, cx| this.delete(cx))
                    }));
            }
        }
        Some(col.into_any_element())
    }

    fn palette(&self, _cx: &App) -> Vec<PaletteItem> {
        vec![PaletteItem::new("Content: new entry", "⌘N", |w, cx| {
            crate::shell::goto(cx, "content");
            let _ = w;
        })]
    }

    fn focus_search(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        self.search.read(cx).focus(w);
    }
    fn commands(&self, cx: &App) -> Vec<(&'static str, String, &'static str)> {
        let Some(k) = &self.selected else { return vec![] };
        match self.open.get(k) {
            Some(Open::Entry(d)) if d.read(cx).draft.state == SaveState::Conflict => vec![
                ("merge", "Conflict: merge both versions for review".into(), ""),
                ("keepmine", "Conflict: keep mine (overwrite server)".into(), ""),
                ("theirs", "Conflict: take the server's version".into(), ""),
            ],
            Some(Open::Entry(d)) => {
                let published = d.read(cx).draft.local["published"].as_bool().unwrap_or(false);
                vec![
                    ("save", "Content: save to server".into(), "⌘S"),
                    if published {
                        ("unpublish", "Content: unpublish this entry".into(), "")
                    } else {
                        ("publish", "Content: publish this entry".into(), "")
                    },
                    ("insert", "Content: insert file…".into(), ""),
                    ("delete", "Content: delete this entry".into(), ""),
                ]
            }
            Some(Open::Series(_)) => {
                vec![("save", "Series: save to server".into(), "⌘S"), ("delete", "Series: delete".into(), "")]
            }
            None => vec![],
        }
    }
    fn run_command(&mut self, id: &str, w: &mut Window, cx: &mut Context<Self>) {
        match id {
            "save" => Workspace::save(self, w, cx),
            "publish" => self.publish(true, cx),
            "unpublish" => self.publish(false, cx),
            "delete" => self.delete(cx),
            "merge" | "keepmine" | "theirs" => {
                use farfield_core::sync::Resolution;
                let how = match id {
                    "merge" => Resolution::Merge,
                    "keepmine" => Resolution::KeepMine,
                    _ => Resolution::TakeTheirs,
                };
                if let Some(Open::Entry(d)) = self.selected.as_ref().and_then(|k| self.open.get(k)) {
                    let d = d.clone();
                    if how == Resolution::KeepMine {
                        confirm(
                            cx,
                            "Overwrite the server's version?",
                            "The server's changes are replaced by yours.",
                            "Overwrite",
                            true,
                            move |_, cx| d.update(cx, |d, cx| d.resolve(how, cx)),
                        );
                    } else {
                        d.update(cx, |d, cx| d.resolve(how, cx));
                    }
                }
            }
            "insert" => {
                if let Some(Open::Entry(d)) = self.selected.as_ref().and_then(|k| self.open.get(k)) {
                    d.clone().update(cx, |d, cx| d.pick_and_upload(w, cx));
                }
            }
            _ => {}
        }
    }
    fn new_item(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        self.new_entry(w, cx)
    }
    fn save(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        let Some(k) = self.selected.clone() else { return };
        match self.open.get(&k) {
            Some(Open::Entry(d)) => d.update(cx, |d, cx| d.save_server(cx)),
            Some(Open::Series(d)) => d.update(cx, |d, cx| d.save_server(cx)),
            None => {}
        }
    }
    fn refresh(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        self.reload(cx)
    }
    fn dirty(&self, cx: &App) -> bool {
        !self.drafts.is_empty()
            || self.open.values().any(|o| match o {
                Open::Entry(d) => d.read(cx).is_dirty(),
                Open::Series(d) => d.read(cx).is_dirty(),
            })
    }
}

impl Render for ContentWs {
    fn render(&mut self, w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        // a reference handed over from Blobs lands at the open document's caret
        if let Some(md) = cx.global_mut::<crate::shell::Overlay>().insert.take() {
            let editor = self.selected.as_ref().and_then(|k| self.open.get(k)).map(|o| match o {
                Open::Entry(d) => d.read(cx).editor.clone(),
                Open::Series(d) => d.read(cx).editor.clone(),
            });
            match editor {
                Some(ed) => {
                    ed.update(cx, |e, cx| e.insert(&format!("\n{md}\n"), cx));
                    ed.read(cx).focus_editor(w);
                    log("blob-inserted", &[("md", &md)]);
                    toast(cx, "Inserted at the caret.", false);
                }
                None => {
                    toast(cx, format!("No document is open — {md} is on the clipboard; open one and paste."), false)
                }
            }
        }
        let t = theme(cx).clone();
        let doc: Option<AnyElement> = self.selected.as_ref().and_then(|k| self.open.get(k)).map(|o| match o {
            Open::Entry(d) => d.clone().into_any_element(),
            Open::Series(d) => d.clone().into_any_element(),
        });
        div()
            .size_full()
            .flex()
            .child(div().w(px(320.)).flex_none().h_full().border_r_1().border_color(t.rule).child(self.render_list(cx)))
            .child(div().flex_1().min_w_0().h_full().child(match doc {
                Some(d) => d,
                None => ui::quiet_state("Choose an entry, or press ⌘N to start one.", cx).into_any_element(),
            }))
    }
}
