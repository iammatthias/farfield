//! QR: every code — private and disabled ones included, from the admin API.
//! The code itself is the hero: a live preview rendered by the server (it
//! knows the public URL a proxy code encodes), refreshed after each save.
//! Edits are partial and conditional (If-Match on the code's CID); exports
//! go through a save dialog and are written atomically.

use super::bookmarks::kit::{self, Images, Slot};
use crate::app::{self, describe, log, Health};
use crate::shell::{confirm, set_health, toast};
use crate::theme::{theme, FONT_DOC, FONT_MONO, S1, S2, S3, S4, S5, S6};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, Kind as BtnKind};
use crate::workspace::{PaletteItem, Workspace};
use farfield_core::api::ext_lists;
use farfield_core::api::qr::{self, Code};
use farfield_core::{ApiError, Freshness, Latest};
use gpui::{div, img, prelude::*, px, AnyElement, App, Context, Entity, ObjectFit, ScrollHandle, WeakEntity, Window};
use serde_json::{json, Map, Value};
use std::sync::Arc;

const SVC: &str = "qr";
const EC: [&str; 4] = ["L", "M", "Q", "H"];
const EC_HINT: [&str; 4] = [
    "Low — recovers ~7% damage; the sparsest code.",
    "Medium — recovers ~15%; the usual choice.",
    "Quartile — recovers ~25%; for print that may scuff.",
    "High — recovers ~30%; the densest code.",
];
/// The preview is requested at twice its on-screen size, for Retina.
const PREVIEW_PX: u32 = 560;

#[derive(Clone, Copy, PartialEq)]
enum Show {
    All,
    Live,
    Off,
}

struct Form {
    id: Option<String>,
    base: Code,
    mode: String,
    ec: String,
    public: bool,
    enabled: bool,
    saving: bool,
    conflict: Option<Code>,
    error: Option<String>,
}

pub struct QrWs {
    me: WeakEntity<Self>,
    items: Vec<Code>,
    loading: bool,
    loaded: bool,
    freshness: Option<Freshness>,
    error: Option<String>,
    latest: Arc<Latest>,
    search: Entity<TextField>,
    show: Show,
    scroll: ScrollHandle,
    form: Option<Form>,
    f_label: Entity<TextField>,
    f_target: Entity<TextField>,
    f_notes: Entity<TextField>,
    previews: Images,
    export_size: usize,
    exporting: bool,
}

fn live(c: &Code) -> bool {
    c.public && c.enabled
}

fn preview_key(c: &Code) -> String {
    format!("{}@{}", c.id, c.cid)
}

impl QrWs {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        let search = cx.new(|cx| TextField::new(w, cx, "", "Filter codes  ⌘F"));
        cx.subscribe_in(&search, w, |this: &mut Self, _, e: &FieldEvent, w, cx| match e {
            FieldEvent::Changed => cx.notify(),
            FieldEvent::Down => this.step(1, cx),
            FieldEvent::Up => this.step(-1, cx),
            FieldEvent::Submit => this.f_label.read(cx).focus(w),
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
        let f_label = field("Label", "what it's for — e.g. “Menu, table cards”", false, w, cx);
        let f_target = field("Target", "a URL or any text", true, w, cx);
        let f_notes = field("Admin notes · private", "where it's printed, who has it", false, w, cx);
        let mut this = QrWs {
            me: cx.entity().downgrade(),
            items: Vec::new(),
            loading: false,
            loaded: false,
            freshness: None,
            error: None,
            latest: Arc::new(Latest::default()),
            search,
            show: Show::All,
            scroll: ScrollHandle::new(),
            form: None,
            f_label,
            f_target,
            f_notes,
            previews: Images::new(48),
            export_size: 1,
            exporting: false,
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
        let task = farfield_core::spawn(async move { qr::all(&s).await });
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
                        if let Some(id) = this.form.as_ref().and_then(|f| f.id.clone()) {
                            if !this.dirty_form(cx) {
                                if let Some(c) = this.items.iter().find(|c| c.id == id).cloned() {
                                    this.set_form(c, cx);
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

    fn rows(&self, cx: &App) -> Vec<Code> {
        let q = self.search.read(cx).text().to_lowercase();
        self.items
            .iter()
            .filter(|c| match self.show {
                Show::All => true,
                Show::Live => live(c),
                Show::Off => !live(c),
            })
            .filter(|c| {
                q.is_empty()
                    || [&c.label, &c.target, &c.id, &c.admin_notes].iter().any(|s| s.to_lowercase().contains(&q))
            })
            .cloned()
            .collect()
    }

    fn selected_id(&self) -> Option<&str> {
        self.form.as_ref().and_then(|f| f.id.as_deref())
    }

    fn step(&mut self, by: i32, cx: &mut Context<Self>) {
        let rows = self.rows(cx);
        if rows.is_empty() {
            return;
        }
        let cur = self.selected_id().and_then(|id| rows.iter().position(|c| c.id == id));
        let next = match cur {
            None => 0,
            Some(i) => (i as i32 + by).clamp(0, rows.len() as i32 - 1) as usize,
        };
        self.scroll.scroll_to_item(next);
        self.select(rows[next].clone(), cx);
    }

    fn select(&mut self, c: Code, cx: &mut Context<Self>) {
        if self.selected_id() == Some(c.id.as_str()) {
            return;
        }
        self.guard(cx, move |this, cx| {
            this.set_form(c, cx);
            this.want_preview(cx);
        });
    }

    fn guard(&mut self, cx: &mut Context<Self>, then: impl FnOnce(&mut Self, &mut Context<Self>) + 'static) {
        if !self.dirty_form(cx) {
            then(self, cx);
            cx.notify();
            return;
        }
        let me = self.me.clone();
        confirm(
            cx,
            "Discard your changes?",
            "This code has edits that aren't saved to the server.",
            "Discard",
            true,
            move |_, cx| {
                let _ = me.update(cx, |this, cx| {
                    then(this, cx);
                    cx.notify();
                });
            },
        );
    }

    fn set_form(&mut self, c: Code, cx: &mut Context<Self>) {
        self.f_label.update(cx, |f, cx| f.set_text(c.label.clone(), cx));
        self.f_target.update(cx, |f, cx| f.set_text(c.target.clone(), cx));
        self.f_notes.update(cx, |f, cx| f.set_text(c.admin_notes.clone(), cx));
        let id = if c.id.is_empty() { None } else { Some(c.id.clone()) };
        let mode = if c.mode.is_empty() { "direct".to_string() } else { c.mode.clone() };
        let ec = if c.ec.is_empty() { "M".to_string() } else { c.ec.clone() };
        self.form = Some(Form {
            id,
            mode,
            ec,
            public: c.public,
            enabled: c.enabled,
            base: c,
            saving: false,
            conflict: None,
            error: None,
        });
        cx.notify();
    }

    fn new_code(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        self.guard(cx, |this, cx| {
            // a new code is live unless you say otherwise
            this.set_form(
                Code { mode: "direct".into(), ec: "M".into(), public: true, enabled: true, ..Default::default() },
                cx,
            );
            log("new", &[("area", SVC)]);
        });
        self.f_label.read(cx).focus(w);
    }

    fn fields(&self, cx: &App) -> Map<String, Value> {
        let Some(f) = &self.form else { return Map::new() };
        let txt = |e: &Entity<TextField>| Value::String(e.read(cx).text().trim().to_string());
        let mut m = Map::new();
        m.insert("label".into(), txt(&self.f_label));
        m.insert("target".into(), txt(&self.f_target));
        m.insert("adminNotes".into(), txt(&self.f_notes));
        m.insert("mode".into(), Value::String(f.mode.clone()));
        m.insert("ec".into(), Value::String(f.ec.clone()));
        m.insert("public".into(), Value::Bool(f.public));
        m.insert("enabled".into(), Value::Bool(f.enabled));
        m
    }

    fn changes(&self, base: &Code, cx: &App) -> Map<String, Value> {
        let b = serde_json::to_value(base).unwrap_or(Value::Null);
        self.fields(cx).into_iter().filter(|(k, v)| b.get(k).unwrap_or(&json!("")) != v).collect()
    }

    fn dirty_form(&self, cx: &App) -> bool {
        match &self.form {
            None => false,
            Some(f) if f.id.is_none() => {
                !self.f_target.read(cx).text().trim().is_empty() || !self.f_label.read(cx).text().trim().is_empty()
            }
            Some(f) => !self.changes(&f.base, cx).is_empty(),
        }
    }

    fn save(&mut self, cx: &mut Context<Self>) {
        let Some(form) = &self.form else { return };
        if form.saving {
            return;
        }
        let fields = self.fields(cx);
        if fields["target"].as_str().unwrap_or("").is_empty() {
            self.form.as_mut().unwrap().error = Some(
                if form.mode == "proxy" {
                    "A proxy code needs a destination."
                } else {
                    "A code needs something to encode."
                }
                .into(),
            );
            cx.notify();
            return;
        }
        let s = app::session(cx);
        let (id, cid) = (form.id.clone(), form.base.cid.clone());
        let task = match &id {
            None => {
                let c: Code = match serde_json::from_value(Value::Object(fields)) {
                    Ok(c) => c,
                    Err(e) => return toast(cx, e.to_string(), true),
                };
                farfield_core::spawn(async move { qr::create(&s, &c).await })
            }
            Some(id) => {
                let ch = self.changes(&form.base, cx);
                if ch.is_empty() {
                    toast(cx, "Nothing to save — no changes.", false);
                    return;
                }
                let id = id.clone();
                farfield_core::spawn(async move { qr::update(&s, &id, &Value::Object(ch), Some(&cid)).await })
            }
        };
        self.put(id.is_none(), task, cx);
    }

    fn put<E: std::fmt::Display + 'static>(
        &mut self,
        created: bool,
        task: impl std::future::Future<Output = Result<Result<farfield_core::transport::Versioned<Code>, ApiError>, E>>
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
                        let c = v.value;
                        log(if created { "qr-create" } else { "qr-save" }, &[("id", &c.id)]);
                        toast(cx, if created { "Code created." } else { "Saved." }, false);
                        if let Some(old) = this.form.as_ref().filter(|f| f.id.is_some()).map(|f| preview_key(&f.base)) {
                            this.previews.forget(&old);
                        }
                        this.set_form(c, cx);
                        this.want_preview(cx);
                        this.reload(cx);
                    }
                    Ok(Err(ApiError::Precondition { current, .. })) => {
                        log("conflict", &[("service", SVC)]);
                        let cur = ext_lists::qr::from_conflict(&current);
                        if let Some(f) = this.form.as_mut() {
                            f.conflict = cur;
                            f.error = Some("Someone saved this code since you opened it. Compare below, then reapply yours or take theirs.".into());
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
        let task = farfield_core::spawn(async move { qr::update(&s, &id, &Value::Object(mine), Some(&cid)).await });
        self.put(false, task, cx);
    }

    fn take_theirs(&mut self, cx: &mut Context<Self>) {
        if let Some(cur) = self.form.as_ref().and_then(|f| f.conflict.clone()) {
            log("conflict-theirs", &[("service", SVC), ("id", &cur.id)]);
            self.set_form(cur, cx);
            self.want_preview(cx);
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
        let name = if f.base.label.is_empty() { id.clone() } else { f.base.label.clone() };
        let printed = if f.base.mode == "proxy" { " Printed copies stop redirecting." } else { "" };
        let me = self.me.clone();
        confirm(
            cx,
            "Delete this code?",
            format!("“{name}” is removed for good.{printed}"),
            "Delete",
            true,
            move |_, cx| {
                let s = app::session(cx);
                let k = id.clone();
                let task = farfield_core::spawn(async move { qr::delete(&s, &k, Some(&cid)).await });
                let _ = me.update(cx, |_, cx| {
                cx.spawn(async move |this, cx| {
                    let r = task.await;
                    let _ = this.update(cx, |this, cx| match r {
                        Ok(Ok(())) => {
                            log("qr-delete", &[("id", &id)]);
                            this.form = None;
                            toast(cx, "Deleted.", false);
                            this.reload(cx);
                        }
                        Ok(Err(ApiError::Precondition { current, .. })) => {
                            if let Some(f) = this.form.as_mut() {
                                f.conflict = ext_lists::qr::from_conflict(&current);
                                f.error = Some("Not deleted: it changed on the server since you opened it. Look at the change first.".into());
                            }
                            cx.notify();
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

    /// Fetch the server's rendering of the selected code at its saved version.
    fn want_preview(&mut self, cx: &mut Context<Self>) {
        let Some(f) = &self.form else { return };
        let Some(id) = f.id.clone() else { return };
        let key = preview_key(&f.base);
        if !self.previews.begin(&key) {
            return;
        }
        let s = app::session(cx);
        let task = farfield_core::spawn(async move {
            let b = qr::preview(&s, &id, Some(PREVIEW_PX)).await?;
            Ok::<_, ApiError>(kit::decode(&b, 2048, true))
        });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                match r {
                    Ok(Ok(img)) => this.previews.finish(&key, img),
                    Ok(Err(e)) => {
                        if e.is_auth() {
                            set_health(cx, SVC, Health::NoAuth);
                        }
                        this.previews.finish(&key, None)
                    }
                    Err(_) => this.previews.finish(&key, None),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn export(&mut self, svg: bool, cx: &mut Context<Self>) {
        let Some(f) = &self.form else { return };
        let Some(id) = f.id.clone() else {
            toast(cx, "Save the code first — exports come from the server's rendering.", true);
            return;
        };
        if self.exporting {
            return;
        }
        let size = ext_lists::qr::PNG_SIZES[self.export_size];
        let stem: String = if f.base.label.is_empty() { format!("qr-{id}") } else { f.base.label.clone() }
            .chars()
            .map(|c| if c.is_alphanumeric() || c == '-' || c == '_' { c } else { '-' })
            .collect::<String>()
            .trim_matches('-')
            .to_string();
        let name = if svg { format!("{stem}.svg") } else { format!("{stem}-{size}.png") };
        let dir = directories::UserDirs::new()
            .and_then(|u| u.download_dir().map(|p| p.to_path_buf()))
            .unwrap_or_else(std::env::temp_dir);
        let rx = cx.prompt_for_new_path(&dir, Some(&name));
        let s = app::session(cx);
        cx.spawn(async move |this, cx| {
            let Ok(Ok(Some(path))) = rx.await else { return };
            let _ = this.update(cx, |this, cx| {
                this.exporting = true;
                cx.notify();
            });
            let p = path.clone();
            let r = farfield_core::spawn(async move {
                let bytes =
                    qr::preview(&s, &id, if svg { None } else { Some(size) }).await.map_err(|e| describe(&e))?;
                ext_lists::export_file(&p, &bytes).map_err(|e| format!("Couldn't write the file: {e}"))
            })
            .await;
            let _ = this.update(cx, |this, cx| {
                this.exporting = false;
                match r {
                    Ok(Ok(())) => {
                        log("qr-export", &[("kind", if svg { "svg" } else { "png" })]);
                        toast(
                            cx,
                            format!(
                                "Exported {}.",
                                path.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default()
                            ),
                            false,
                        );
                    }
                    Ok(Err(e)) => toast(cx, e, true),
                    Err(e) => toast(cx, e.to_string(), true),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn copy_link(&mut self, cx: &mut Context<Self>) {
        let Some(f) = &self.form else { return };
        let Some(id) = &f.id else { return };
        if f.base.mode != "proxy" {
            kit::copy(cx, &f.base.target, "the encoded text");
            return;
        }
        match qr::redirect_url(&app::session(cx), id) {
            Some(u) => kit::copy(cx, &u, "the scan link"),
            None => toast(cx, "This profile has no public address for qr, so there's no link to share.", true),
        }
    }

    // ── rendering ──────────────────────────────────────────────────────

    fn render_list(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let rows = self.rows(cx);
        let sel = self.selected_id().map(|s| s.to_string());
        let me = self.me.clone();
        let total = self.items.len();
        let off = self.items.iter().filter(|c| !live(c)).count();
        let show = match self.show {
            Show::All => 0,
            Show::Live => 1,
            Show::Off => 2,
        };
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
                    .child(div().text_lg().font_weight(gpui::FontWeight::SEMIBOLD).text_color(t.ink).child("QR codes"))
                    .child(ui::mono(format!("{total} · {off} not scanning"), cx))
                    .child(div().flex_1())
                    .child(ui::button("qr-new", "New  ⌘N", BtnKind::Quiet, cx, {
                        let me = me.clone();
                        move |_, w, cx| {
                            let _ = me.update(cx, |this, cx| this.new_code(w, cx));
                        }
                    })),
            )
            .child(kit::seg("qr-show", &["All", "Scanning", "Off"], show, cx, {
                let me = me.clone();
                move |i, _, cx| {
                    let _ = me.update(cx, |this, cx| {
                        this.show = [Show::All, Show::Live, Show::Off][i];
                        cx.notify();
                    });
                }
            }))
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
        let body: AnyElement = if rows.is_empty() {
            let msg = if self.loading && !self.loaded {
                "Loading codes…"
            } else if self.error.is_some() && !self.loaded {
                "Codes can't be shown until the service answers. ⌘R tries again."
            } else if self.items.is_empty() {
                "No codes yet. ⌘N makes one."
            } else {
                "Nothing matches this filter."
            };
            ui::quiet_state(msg, cx).into_any_element()
        } else {
            div()
                .id("qr-list")
                .flex_1()
                .overflow_y_scroll()
                .track_scroll(&self.scroll)
                .children(rows.into_iter().enumerate().map(|(i, c)| {
                    let on = sel.as_deref() == Some(c.id.as_str());
                    let me = me.clone();
                    let title = if c.label.is_empty() { c.id.clone() } else { c.label.clone() };
                    let (word, color) = match (c.public, c.enabled) {
                        (true, true) => ("scanning", t.good),
                        (_, false) => ("disabled", t.ink_3),
                        (false, true) => ("private", t.ink_3),
                    };
                    ui::list_row(("qr", i), on, &t)
                        .flex()
                        .flex_col()
                        .gap(px(2.))
                        .child(
                            div()
                                .flex()
                                .justify_between()
                                .gap(S2)
                                .child(div().text_sm().text_color(t.ink).truncate().child(title))
                                .child(ui::chip(word, color, cx)),
                        )
                        .child(
                            div()
                                .flex()
                                .gap(S2)
                                .text_xs()
                                .font_family(FONT_MONO)
                                .text_color(t.ink_3)
                                .child(
                                    div()
                                        .flex_none()
                                        .text_color(if c.mode == "proxy" { t.accent } else { t.ink_3 })
                                        .child(if c.mode == "proxy" { "proxy →" } else { "direct" }),
                                )
                                .child(div().truncate().child(c.target.clone())),
                        )
                        .on_click(move |_, _, cx| {
                            let c = c.clone();
                            let _ = me.update(cx, |this, cx| this.select(c, cx));
                        })
                        .into_any_element()
                }))
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

    fn render_preview(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let Some(f) = &self.form else { return div().into_any_element() };
        let side = px((PREVIEW_PX / 2) as f32);
        // the code sits on white whatever the theme: that's how it prints
        let frame =
            div().w(side).h(side).flex_none().flex().items_center().justify_center().rounded(px(6.)).bg(gpui::white());
        let inner: AnyElement = if f.id.is_none() {
            div()
                .p(S4)
                .text_sm()
                .text_color(gpui::black().opacity(0.55))
                .child("The preview appears after the first save.")
                .into_any_element()
        } else {
            match self.previews.get(&preview_key(&f.base)) {
                Some(Slot::Ready(..)) => {
                    let (im, _, _) = self.previews.ready(&preview_key(&f.base)).unwrap();
                    img(im).size(side).object_fit(ObjectFit::Contain).into_any_element()
                }
                Some(Slot::Failed) => div()
                    .p(S4)
                    .text_sm()
                    .text_color(t.bad)
                    .child("The preview couldn't be rendered. ⌘R to retry.")
                    .into_any_element(),
                _ => div().text_sm().text_color(gpui::black().opacity(0.4)).child("Rendering…").into_any_element(),
            }
        };
        let dirty = f.id.is_some() && self.dirty_form(cx);
        div()
            .flex()
            .flex_col()
            .items_center()
            .gap(S2)
            .child(frame.child(inner))
            .child(div().text_xs().text_color(if dirty { t.warn } else { t.ink_3 }).child(if dirty {
                "Showing the saved version — save to see your changes."
            } else if f.mode == "proxy" {
                "Encodes the scan link, not the destination."
            } else {
                "Encodes the target exactly."
            }))
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
                .child(
                    div()
                        .font_family(FONT_DOC)
                        .text_size(px(22.))
                        .text_color(t.ink)
                        .child("Codes for things in the world."),
                )
                .child(
                    div()
                        .text_sm()
                        .text_color(t.ink_2)
                        .child("Choose a code to see and edit it, or press ⌘N to make one."),
                )
                .into_any_element();
        };
        let me = self.me.clone();
        let is_new = f.id.is_none();
        let dirty = self.dirty_form(cx);
        let proxy = f.mode == "proxy";
        let heading = if is_new {
            "New code".to_string()
        } else if f.base.label.is_empty() {
            f.base.id.clone()
        } else {
            f.base.label.clone()
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
        let scanning = f.public && f.enabled;
        let ec_i = EC.iter().position(|e| *e == f.ec).unwrap_or(1);
        let conflict = f.conflict.as_ref().map(|c| self.render_conflict(c, cx));
        let mut fields = div()
            .flex_1()
            .min_w(px(300.))
            .flex()
            .flex_col()
            .gap(S3)
            .when_some(f.error.clone(), |d, e| d.child(ui::notice(e, t.bad, cx)))
            .when_some(conflict, |d, c| d.child(c))
            .child(self.f_label.clone())
            .child(
                div()
                    .flex()
                    .flex_col()
                    .gap(S1)
                    .child(div().text_xs().text_color(t.ink_2).child("Mode"))
                    .child(kit::seg("qr-mode", &["Direct", "Proxy"], usize::from(proxy), cx, {
                        let me = me.clone();
                        move |i, _, cx| {
                            let _ = me.update(cx, |this, cx| {
                                if let Some(f) = this.form.as_mut() {
                                    f.mode = if i == 1 { "proxy" } else { "direct" }.into();
                                }
                                cx.notify();
                            });
                        }
                    }))
                    .child(div().text_xs().text_color(t.ink_2).child(if proxy {
                        "Proxy: the code holds a short scan link (/r/…) that redirects to the destination — change the destination any time without reprinting."
                    } else {
                        "Direct: the code holds the target itself. Simple and offline-proof, but changing it means reprinting."
                    })),
            )
            .child(self.f_target.clone())
            .child(div().mt(px(-6.)).text_xs().text_color(t.ink_3).child(if proxy { "Destination — where a scan lands." } else { "Exactly what a scan reads." }))
            .child(
                div()
                    .flex()
                    .flex_col()
                    .gap(S1)
                    .child(div().text_xs().text_color(t.ink_2).child("Error correction"))
                    .child(kit::seg("qr-ec", &EC, ec_i, cx, {
                        let me = me.clone();
                        move |i, _, cx| {
                            let _ = me.update(cx, |this, cx| {
                                if let Some(f) = this.form.as_mut() {
                                    f.ec = EC[i].into();
                                }
                                cx.notify();
                            });
                        }
                    }))
                    .child(div().text_xs().text_color(t.ink_2).child(EC_HINT[ec_i])),
            )
            .child(kit::switch("qr-public", "Public", "Listed in the public API; required for scans to work.", f.public, cx, {
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
            .child(kit::switch("qr-enabled", "Enabled", "Off stops scans and the redirect without deleting anything.", f.enabled, cx, {
                let me = me.clone();
                move |_, cx| {
                    let _ = me.update(cx, |this, cx| {
                        if let Some(f) = this.form.as_mut() {
                            f.enabled = !f.enabled;
                        }
                        cx.notify();
                    });
                }
            }))
            .child(self.f_notes.clone());
        let mut actions = div().flex().gap(S2).pt(S2);
        if f.conflict.is_none() {
            actions = actions.child(if f.saving {
                ui::button_disabled("Saving…", cx).into_any_element()
            } else {
                ui::button(
                    "qr-save",
                    if is_new { "Create code  ⌘S" } else { "Save changes  ⌘S" },
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
            actions = actions.child(ui::button("qr-revert", "Revert", BtnKind::Quiet, cx, {
                let me = me.clone();
                move |_, _, cx| {
                    let b = base.clone();
                    let _ = me.update(cx, |this, cx| this.set_form(b, cx));
                }
            }));
        }
        if is_new {
            actions = actions.child(ui::button("qr-cancel", "Cancel", BtnKind::Quiet, cx, {
                let me = me.clone();
                move |_, _, cx| {
                    let _ = me.update(cx, |this, cx| {
                        this.form = None;
                        cx.notify();
                    });
                }
            }));
        }
        fields = fields.child(actions);
        let col = div()
            .w_full()
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
                    .child(ui::eyebrow(if is_new { "QR code · new" } else { "QR code" }, cx))
                    .child(
                        div()
                            .font_family(FONT_DOC)
                            .text_size(px(26.))
                            .line_height(px(32.))
                            .text_color(t.ink)
                            .child(heading),
                    )
                    .child(div().flex().gap(S4).child(ui::chip(state.0, state.1, cx)).child(ui::chip(
                        if scanning { "scanning" } else { "not scanning" },
                        if scanning { t.good } else { t.ink_3 },
                        cx,
                    ))),
            )
            .child(ui::rule(cx))
            .child(div().flex().flex_wrap().gap(S6).items_start().child(self.render_preview(cx)).child(fields));
        div().id("qr-form").size_full().overflow_y_scroll().child(col).into_any_element()
    }

    fn render_conflict(&self, cur: &Code, cx: &mut Context<Self>) -> AnyElement {
        let me = self.me.clone();
        let mine = self.fields(cx);
        let theirs = serde_json::to_value(cur).unwrap_or(Value::Null);
        let base =
            self.form.as_ref().map(|f| serde_json::to_value(&f.base).unwrap_or(Value::Null)).unwrap_or(Value::Null);
        let show = |k: &str, v: &Value| match (k, v) {
            (_, Value::String(s)) if s.is_empty() => "—".to_string(),
            (_, Value::String(s)) => s.clone(),
            ("public", Value::Bool(b)) => if *b { "public" } else { "private" }.into(),
            ("enabled", Value::Bool(b)) => if *b { "enabled" } else { "disabled" }.into(),
            _ => "—".into(),
        };
        let labels = [
            ("label", "Label"),
            ("mode", "Mode"),
            ("target", "Target"),
            ("ec", "Error corr."),
            ("public", "Public"),
            ("enabled", "Enabled"),
            ("adminNotes", "Notes"),
        ];
        let mut diffs = Vec::new();
        for (k, label) in labels {
            let a = show(k, mine.get(k).unwrap_or(&Value::Null));
            let b = show(k, theirs.get(k).unwrap_or(&Value::Null));
            let was = show(k, base.get(k).unwrap_or(&Value::Null));
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
                    .child(ui::button("qr-reapply", "Reapply my changes", BtnKind::Primary, cx, {
                        let me = me.clone();
                        move |_, _, cx| {
                            let _ = me.update(cx, |this, cx| this.reapply(cx));
                        }
                    }))
                    .child(ui::button("qr-theirs", "Use the server's version", BtnKind::Quiet, cx, {
                        let me = me.clone();
                        move |_, _, cx| {
                            let me = me.clone();
                            confirm(
                                cx,
                                "Use the server's version?",
                                "Your unsaved edits to this code are dropped.",
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

impl Workspace for QrWs {
    fn inspector(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> Option<AnyElement> {
        let t = theme(cx).clone();
        let f = self.form.as_ref()?;
        let me = self.me.clone();
        let col = div().flex().flex_col().gap(S2);
        if f.id.is_none() {
            return Some(
                col.child(ui::eyebrow("New code", cx))
                    .child(div().text_sm().text_color(t.ink_2).child(
                        "Pick direct for text that will never change (Wi-Fi details, a phone number). Pick proxy for anything printed that points at the web — you can repoint it later.",
                    ))
                    .into_any_element(),
            );
        }
        let c = &f.base;
        let s = app::session(cx);
        let scan = qr::redirect_url(&s, &c.id);
        let public_png = ext_lists::qr::public_png_url(&s, &c.id);
        let exporting = self.exporting;
        let size_labels: Vec<String> = ext_lists::qr::PNG_SIZES.iter().map(|n| n.to_string()).collect();
        let size_refs: Vec<&str> = size_labels.iter().map(|s| s.as_str()).collect();
        let mut col = col
            .child(ui::eyebrow("Export", cx))
            .child(div().text_xs().text_color(t.ink_2).child("PNG size, in pixels"))
            .child(kit::seg("qr-size", &size_refs, self.export_size, cx, {
                let me = me.clone();
                move |i, _, cx| {
                    let _ = me.update(cx, |this, cx| {
                        this.export_size = i;
                        cx.notify();
                    });
                }
            }))
            .child(
                div()
                    .flex()
                    .gap(px(2.))
                    .child(if exporting {
                        ui::button_disabled("Exporting…", cx).into_any_element()
                    } else {
                        ui::button("qr-png", "Export PNG…", BtnKind::Quiet, cx, {
                            let me = me.clone();
                            move |_, _, cx| {
                                let _ = me.update(cx, |this, cx| this.export(false, cx));
                            }
                        })
                        .into_any_element()
                    })
                    .child(ui::button("qr-svg", "Export SVG…", BtnKind::Quiet, cx, {
                        let me = me.clone();
                        move |_, _, cx| {
                            let _ = me.update(cx, |this, cx| this.export(true, cx));
                        }
                    })),
            )
            .child(div().text_xs().text_color(t.ink_3).child("SVG scales to any print size."))
            .child(ui::rule(cx))
            .child(ui::eyebrow("Scanning", cx));
        if c.mode == "proxy" {
            col = col
                .when_some(scan.clone(), |d, u| d.child(ui::field_row("Scan link", ui::mono(u, cx), cx)))
                .child(ui::field_row("Redirects to", ui::mono(c.target.clone(), cx), cx))
                .child(ui::button("qr-copy", "Copy scan link", BtnKind::Quiet, cx, {
                    let me = me.clone();
                    move |_, _, cx| {
                        let _ = me.update(cx, |this, cx| this.copy_link(cx));
                    }
                }));
        } else {
            col = col.child(ui::field_row("Encodes", ui::mono(c.target.clone(), cx), cx));
        }
        col = col.child(div().text_sm().text_color(t.ink_2).child(match (c.public, c.enabled) {
            (true, true) => "Live: scans work and the public image is served.",
            (_, false) => "Disabled: scans and the public image return not-found. Nothing is deleted.",
            (false, true) => "Private: scans and the public image return not-found until it's public.",
        }));
        if live(c) {
            if let Some(u) = public_png {
                col = col.child(ui::field_row("Public image", ui::mono(u, cx), cx));
            }
        }
        col = col
            .child(ui::rule(cx))
            .child(ui::eyebrow("Record", cx))
            .child(ui::field_row("ID", ui::mono(c.id.clone(), cx), cx))
            .child(ui::field_row("CID · the version", ui::mono(c.cid.clone(), cx), cx))
            .child(ui::field_row(
                "Saved",
                ui::mono(format!("{} · created {}", ui::when(&c.updated_at), ui::when(&c.created_at)), cx),
                cx,
            ))
            .child(ui::rule(cx))
            .child(ui::button("qr-delete", "Delete code…", BtnKind::Danger, cx, move |_, _, cx| {
                let _ = me.update(cx, |this, cx| this.delete(cx));
            }));
        Some(col.into_any_element())
    }

    fn palette(&self, _cx: &App) -> Vec<PaletteItem> {
        let me = self.me.clone();
        vec![PaletteItem::new("QR: new code", "⌘N", move |w, cx| {
            crate::shell::goto(cx, SVC);
            let _ = me.update(cx, |this, cx| this.new_code(w, cx));
        })]
    }

    fn commands(&self, _cx: &App) -> Vec<(&'static str, String, &'static str)> {
        let mut v = vec![("new", "QR: new code".to_string(), "⌘N"), ("reload", "QR: reload codes".into(), "⌘R")];
        if let Some(f) = &self.form {
            v.push(("save", "QR: save code".into(), "⌘S"));
            if f.id.is_some() {
                v.push((
                    "export-png",
                    format!("QR: export PNG ({} px)…", ext_lists::qr::PNG_SIZES[self.export_size]),
                    "",
                ));
                v.push(("export-svg", "QR: export SVG…".into(), ""));
                if f.base.mode == "proxy" {
                    v.push(("copy-link", "QR: copy scan link".into(), ""));
                }
                v.push(("delete", "QR: delete code…".into(), ""));
            }
            if f.conflict.is_some() {
                v.push(("reapply", "QR: reapply my changes on the server's version".into(), ""));
                v.push(("theirs", "QR: use the server's version".into(), ""));
            }
        }
        v
    }

    fn run_command(&mut self, id: &str, w: &mut Window, cx: &mut Context<Self>) {
        match id {
            "new" => self.new_code(w, cx),
            "reload" => self.reload(cx),
            "save" => self.save(cx),
            "export-png" => self.export(false, cx),
            "export-svg" => self.export(true, cx),
            "copy-link" => self.copy_link(cx),
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
        self.new_code(w, cx)
    }
    fn save(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        QrWs::save(self, cx)
    }
    fn refresh(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        // a failed preview gets another try
        if let Some(k) = self.form.as_ref().map(|f| preview_key(&f.base)) {
            if matches!(self.previews.get(&k), Some(Slot::Failed)) {
                self.previews.forget(&k);
            }
        }
        self.want_preview(cx);
        self.reload(cx)
    }
    fn dirty(&self, cx: &App) -> bool {
        self.dirty_form(cx)
    }
}

impl Render for QrWs {
    fn render(&mut self, w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        self.previews.drop_retired(w);
        let t = theme(cx).clone();
        div()
            .size_full()
            .flex()
            .child(div().w(px(340.)).flex_none().h_full().border_r_1().border_color(t.rule).child(self.render_list(cx)))
            .child(div().flex_1().min_w_0().h_full().child(self.render_form(cx)))
    }
}
