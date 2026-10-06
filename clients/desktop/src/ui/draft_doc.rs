//! One open document backed by a local draft: the fields, the editor, and
//! the two kinds of save.
//!
//! Every edit is written to the draft on this Mac (atomically, off the UI
//! thread, a moment after typing stops) — "saved on this Mac". Saving to the
//! server is explicit (⌘S), conditional on the version the draft was edited
//! from, and reported separately. A conflict keeps both versions and asks.

use crate::app::{self, describe, log};
use crate::shell::{confirm, toast};
use crate::theme::{theme, FONT_DOC, MEASURE, S2, S3, S4};
use crate::ui::doc_editor::{DocEditor, DocEvent};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, Kind as BtnKind};
use farfield_core::api::blobs;
use farfield_core::store::{Draft, SaveState};
use farfield_core::sync::{self, Kind, Resolution, SaveOutcome};
use farfield_core::upload::Progress;
use gpui::{div, prelude::*, px, AnyElement, Context, Entity, EventEmitter, SharedString, Task, Window};
use serde_json::Value;
use std::marker::PhantomData;
use std::path::PathBuf;
use std::time::Duration;

#[derive(Clone, Copy, PartialEq)]
pub enum FieldKind {
    Text,
    /// Comma-separated in the field, an array in the record.
    Tags,
    Mono,
}

pub struct FieldSpec {
    pub key: &'static str,
    pub label: &'static str,
    pub placeholder: &'static str,
    pub kind: FieldKind,
}

#[derive(Clone, Debug)]
pub enum DraftEvent {
    /// The draft moved key (a new record got its server name).
    Renamed { from: String, to: String },
    /// Saved to the server.
    Saved,
    /// Local state changed (for list markers).
    Touched,
}

struct Upload {
    name: String,
    progress: Progress,
}

pub struct DraftDoc<K: Kind + 'static> {
    pub draft: Draft,
    pub editor: Entity<DocEditor>,
    /// The title line, set in Newsreader above the body.
    pub title: Option<Entity<TextField>>,
    fields: Vec<(FieldSpec, Entity<TextField>)>,
    persist: Option<Task<()>>,
    _on_quit: gpui::Subscription,
    saving: bool,
    pub error: Option<String>,
    uploads: Vec<Upload>,
    _k: PhantomData<K>,
}

impl<K: Kind + 'static> EventEmitter<DraftEvent> for DraftDoc<K> {}

fn tags_to_text(v: &Value) -> String {
    v.as_array().map(|a| a.iter().filter_map(|s| s.as_str()).collect::<Vec<_>>().join(", ")).unwrap_or_default()
}

fn text_to_tags(s: &str) -> Value {
    Value::Array(
        s.split(',')
            .map(|t| t.trim().trim_start_matches('#').to_string())
            .filter(|t| !t.is_empty())
            .map(Value::String)
            .collect(),
    )
}

impl<K: Kind + 'static> DraftDoc<K> {
    pub fn new(
        draft: Draft,
        title_key: Option<&'static str>,
        specs: Vec<FieldSpec>,
        placeholder: &str,
        w: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let body = draft.local[K::TEXT].as_str().unwrap_or("").to_string();
        let session = app::session(cx);
        let editor = cx.new(|cx| DocEditor::new(w, cx, &body, placeholder, Some(session)));
        cx.subscribe_in(&editor, w, |this, _e, ev: &DocEvent, w, cx| match ev {
            DocEvent::Changed => this.touched(cx),
            DocEvent::FilesDropped(paths) => this.upload_and_insert(paths.clone(), w, cx),
            DocEvent::Blur => this.flush(cx),
        })
        .detach();
        let title = title_key.map(|k| {
            let v = draft.local[k].as_str().unwrap_or("").to_string();
            let f = cx.new(|cx| {
                let mut f = TextField::new(w, cx, "", "Untitled").doc();
                f.set_text(v, cx);
                f
            });
            cx.subscribe_in(&f, w, |this, _f, e: &FieldEvent, w, cx| match e {
                FieldEvent::Changed => this.touched(cx),
                FieldEvent::Submit => this.editor.read(cx).focus_editor(w),
                FieldEvent::Blur => this.flush(cx),
                _ => {}
            })
            .detach();
            f
        });
        let fields = specs
            .into_iter()
            .map(|spec| {
                let v = &draft.local[spec.key];
                let text =
                    if spec.kind == FieldKind::Tags { tags_to_text(v) } else { v.as_str().unwrap_or("").to_string() };
                let mono = spec.kind == FieldKind::Mono;
                let (label, ph) = (spec.label, spec.placeholder);
                let f = cx.new(|cx| {
                    let mut f = TextField::new(w, cx, label, ph);
                    if mono {
                        f = f.mono();
                    }
                    f.set_text(text, cx);
                    f
                });
                cx.subscribe_in(&f, w, |this, _f, e: &FieldEvent, _w, cx| match e {
                    FieldEvent::Changed => this.touched(cx),
                    FieldEvent::Blur | FieldEvent::Submit => this.flush(cx),
                    _ => {}
                })
                .detach();
                (spec, f)
            })
            .collect();
        let _ = title_key;
        // quitting mid-pause still keeps the last keystrokes
        let _on_quit = cx.on_app_quit(|this, cx| {
            this.flush_with(true, cx);
            async {}
        });
        DraftDoc {
            draft,
            editor,
            title,
            fields,
            persist: None,
            _on_quit,
            saving: false,
            error: None,
            uploads: Vec::new(),
            _k: PhantomData,
        }
    }

    pub fn is_dirty(&self) -> bool {
        matches!(self.draft.state, SaveState::Local | SaveState::Pending | SaveState::Conflict)
    }

    /// The record as the person has it now: the draft's fields (including
    /// ones this client doesn't show) with the visible fields over them.
    pub fn current(&mut self, cx: &mut Context<Self>) -> Value {
        let mut v = self.draft.local.clone();
        let text = self.editor.update(cx, |e, _| e.text());
        let o = v.as_object_mut().expect("draft is an object");
        o.insert(K::TEXT.into(), Value::String(text));
        if let Some(t) = &self.title {
            o.insert("title".into(), Value::String(t.read(cx).text()));
        }
        for (spec, f) in &self.fields {
            let s = f.read(cx).text();
            let val = if spec.kind == FieldKind::Tags { text_to_tags(&s) } else { Value::String(s) };
            o.insert(spec.key.into(), val);
        }
        v
    }

    fn touched(&mut self, cx: &mut Context<Self>) {
        // write to disk a moment after typing stops
        self.persist = Some(cx.spawn(async move |this, cx| {
            cx.background_executor().timer(Duration::from_millis(350)).await;
            let _ = this.update(cx, |this, cx| this.flush(cx));
        }));
        cx.emit(DraftEvent::Touched);
        cx.notify();
    }

    /// Write the current state to the draft file now (atomic, background).
    pub fn flush(&mut self, cx: &mut Context<Self>) {
        self.flush_with(false, cx)
    }

    /// `sync` writes before returning — for quitting, when a background
    /// write might not finish.
    fn flush_with(&mut self, sync: bool, cx: &mut Context<Self>) {
        self.persist = None;
        let local = self.current(cx);
        if local == self.draft.local && self.draft.state != SaveState::Local {
            return;
        }
        if local == self.draft.local {
            return;
        }
        self.draft.local = local;
        if self.draft.state == SaveState::Saved {
            self.draft.state = SaveState::Local;
        }
        self.draft.updated_ms = farfield_core::store::now_ms();
        let d = self.draft.clone();
        let session = app::session(cx);
        let area = K::DRAFTS;
        let write = move || {
            if let Ok(drafts) = session.drafts(area) {
                let _ = drafts.save(&d);
            }
        };
        if sync {
            write()
        } else {
            farfield_core::spawn(async move { write() });
        }
        cx.emit(DraftEvent::Touched);
        cx.notify();
    }

    /// Save to the server (⌘S).
    pub fn save_server(&mut self, cx: &mut Context<Self>) {
        if self.saving {
            return;
        }
        self.flush(cx);
        if self.draft.state == SaveState::Saved {
            toast(cx, "Already saved.", false);
            return;
        }
        self.saving = true;
        self.error = None;
        let snapshot = self.draft.local.clone();
        let mut d = self.draft.clone();
        let session = app::session(cx);
        let old_key = d.key.clone();
        log("save-start", &[("service", K::DRAFTS), ("key", &old_key)]);
        crate::perf::mark("save");
        let task = farfield_core::spawn(async move {
            let r = sync::save::<K>(&session, &mut d).await;
            (r, d)
        });
        cx.spawn(async move |this, cx| {
            let Ok((r, d)) = task.await else { return };
            let _ = this.update(cx, |this, cx| this.saved(r, d, snapshot, old_key, cx));
        })
        .detach();
        cx.notify();
    }

    fn saved(
        &mut self,
        r: Result<SaveOutcome, farfield_core::ApiError>,
        mut d: Draft,
        snapshot: Value,
        old_key: String,
        cx: &mut Context<Self>,
    ) {
        self.saving = false;
        // edits made while the save was in flight stay local
        let now = self.current(cx);
        let typed_meanwhile = now != snapshot;
        match r {
            Ok(SaveOutcome::Saved) => {
                if typed_meanwhile {
                    d.local = merge_keep_bookkeeping(&d.local, &now);
                    d.state = SaveState::Local;
                } else {
                    // take server bookkeeping (slug, cid, publishedAt) into
                    // the fields that show it
                    self.reload_fields(&d.local, cx);
                }
                log("saved", &[("service", K::DRAFTS), ("key", &d.key)]);
                crate::perf::end("save");
                toast(cx, "Saved.", false);
                cx.emit(DraftEvent::Saved);
            }
            Ok(SaveOutcome::Conflict) => {
                log("conflict", &[("service", K::DRAFTS), ("key", &d.key)]);
                if typed_meanwhile {
                    d.local = now;
                }
                toast(cx, "Conflict — changed on the server.", true);
            }
            Ok(SaveOutcome::NotSaved(e)) => {
                log("save-failed", &[("service", K::DRAFTS), ("key", &d.key), ("error", &e.to_string())]);
                if typed_meanwhile {
                    d.local = now;
                }
                self.error = Some(describe(&e));
                if e.is_auth() {
                    crate::shell::set_health(cx, K::SERVICE, crate::app::Health::NoAuth);
                } else if e.is_offline() {
                    crate::shell::set_health(cx, K::SERVICE, crate::app::Health::Down(e.to_string()));
                }
            }
            Err(e) => {
                self.error = Some(describe(&e));
            }
        }
        if d.key != old_key {
            cx.emit(DraftEvent::Renamed { from: old_key, to: d.key.clone() });
        }
        self.draft = d;
        // persist the post-save state (and any edits typed meanwhile)
        let dd = self.draft.clone();
        let session = app::session(cx);
        farfield_core::spawn(async move {
            if let Ok(drafts) = session.drafts(K::DRAFTS) {
                let _ = drafts.save(&dd);
            }
        });
        cx.notify();
    }

    fn reload_fields(&mut self, v: &Value, cx: &mut Context<Self>) {
        for (spec, f) in &self.fields {
            let val = &v[spec.key];
            let text =
                if spec.kind == FieldKind::Tags { tags_to_text(val) } else { val.as_str().unwrap_or("").to_string() };
            f.update(cx, |f, cx| f.set_text(text, cx));
        }
    }

    /// Replace everything shown with `v` (after a resolution).
    fn reload_all(&mut self, cx: &mut Context<Self>) {
        let v = self.draft.local.clone();
        self.reload_fields(&v, cx);
        if let Some(t) = &self.title {
            let s = v["title"].as_str().unwrap_or("").to_string();
            t.update(cx, |f, cx| f.set_text(s, cx));
        }
        let body = v[K::TEXT].as_str().unwrap_or("").to_string();
        self.editor.update(cx, |e, cx| e.set_text(&body, cx));
    }

    /// Change one record field and save to the server (publish/unpublish).
    pub fn set_and_save(&mut self, key: &str, val: Value, cx: &mut Context<Self>) {
        self.flush(cx);
        if let Some(o) = self.draft.local.as_object_mut() {
            o.insert(key.into(), val);
        }
        if self.draft.state == SaveState::Saved {
            self.draft.state = SaveState::Local;
        }
        self.save_server(cx);
    }

    pub fn resolve(&mut self, how: Resolution, cx: &mut Context<Self>) {
        let mut d = self.draft.clone();
        let session = app::session(cx);
        self.saving = how == Resolution::KeepMine;
        let task = farfield_core::spawn(async move {
            let r = sync::resolve::<K>(&session, &mut d, how).await;
            (r, d)
        });
        log("resolve", &[("service", K::DRAFTS), ("how", &format!("{how:?}"))]);
        cx.spawn(async move |this, cx| {
            let Ok((r, d)) = task.await else { return };
            let _ = this.update(cx, |this, cx| {
                this.saving = false;
                this.draft = d;
                this.reload_all(cx);
                match r {
                    Ok(SaveOutcome::Saved) => toast(cx, "Resolved and saved.", false),
                    Ok(SaveOutcome::Conflict) => toast(cx, "Still conflicting.", true),
                    Ok(SaveOutcome::NotSaved(farfield_core::ApiError::Cancelled)) => {
                        toast(cx, "Merged — review, then save.", false)
                    }
                    Ok(SaveOutcome::NotSaved(e)) | Err(e) => this.error = Some(describe(&e)),
                }
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    /// Discard local changes and go back to the server's version.
    pub fn revert(&mut self, cx: &mut Context<Self>) {
        let Some(base) = self.draft.base.clone() else { return };
        self.draft.local = base;
        self.draft.state = SaveState::Saved;
        self.draft.remote = None;
        let session = app::session(cx);
        let d = self.draft.clone();
        farfield_core::spawn(async move {
            if let Ok(dr) = session.drafts(K::DRAFTS) {
                let _ = dr.discard(&d.service, &d.key);
            }
        });
        self.reload_all(cx);
        log("revert", &[("service", K::DRAFTS), ("key", &self.draft.key)]);
        cx.notify();
    }

    /// Upload files to blobs and insert their references at the caret.
    pub fn upload_and_insert(&mut self, paths: Vec<PathBuf>, _w: &mut Window, cx: &mut Context<Self>) {
        for p in paths {
            let name = p.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default();
            let progress = Progress::new(std::fs::metadata(&p).map(|m| m.len()).unwrap_or(0));
            self.uploads.push(Upload { name: name.clone(), progress: progress.clone() });
            let session = app::session(cx);
            let pr = progress.clone();
            let task = farfield_core::spawn(async move { blobs::upload(&session, &p, &pr).await });
            log("upload-start", &[("file", &name)]);
            // repaint progress while it runs
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
                    this.uploads.retain(|u| u.name != name);
                    match r {
                        Ok(Ok(meta)) => {
                            let alt = name.rsplit_once('.').map(|(a, _)| a).unwrap_or(&name).to_string();
                            let md = format!("\n{}\n", meta.value.markdown(&alt));
                            this.editor.update(cx, |e, cx| e.insert(&md, cx));
                            log("upload-done", &[("file", &name), ("cid", &meta.value.cid)]);
                        }
                        Ok(Err(farfield_core::ApiError::Cancelled)) => {
                            toast(cx, format!("Upload of {name} cancelled."), false)
                        }
                        Ok(Err(e)) => toast(cx, format!("{name}: {}", describe(&e)), true),
                        Err(e) => toast(cx, e.to_string(), true),
                    }
                    cx.notify();
                });
            })
            .detach();
        }
        cx.notify();
    }

    pub fn pick_and_upload(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        let rx = cx.prompt_for_paths(gpui::PathPromptOptions {
            files: true,
            directories: false,
            multiple: true,
            prompt: Some("Insert".into()),
        });
        cx.spawn_in(w, async move |this, cx| {
            if let Ok(Ok(Some(paths))) = rx.await {
                let _ = this.update_in(cx, |this, w, cx| this.upload_and_insert(paths, w, cx));
            }
        })
        .detach();
    }

    /// The writing surface: title in Newsreader, then the body, at the
    /// reading measure.
    pub fn render_body(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        div()
            .size_full()
            .flex()
            .flex_col()
            .items_center()
            .child(
                div()
                    .w_full()
                    .max_w(MEASURE + px(64.))
                    .flex_1()
                    .flex()
                    .flex_col()
                    .px(px(32.))
                    .pt(px(28.))
                    .when_some(self.title.clone(), |d, title| {
                        d.child(
                            div()
                                .font_family(FONT_DOC)
                                .text_size(px(26.))
                                .line_height(px(34.))
                                .text_color(t.ink)
                                .child(title),
                        )
                    })
                    .child(div().flex_1().min_h_0().pt(S3).child(self.editor.clone())),
            )
            .into_any_element()
    }

    /// Save state, fields, conflict choices, uploads — the inspector part
    /// every document shares.
    pub fn render_inspector(&mut self, cx: &mut Context<Self>) -> Vec<AnyElement> {
        let t = theme(cx).clone();
        let mut out: Vec<AnyElement> = Vec::new();
        let (label, color) = match (self.saving, self.draft.state) {
            (true, _) => ("Saving…", t.accent),
            (_, SaveState::Saved) => ("Saved", t.good),
            (_, SaveState::Local) => ("Unsaved · on this Mac only", t.warn),
            (_, SaveState::Pending) => ("Pending · outcome unknown", t.warn),
            (_, SaveState::Conflict) => ("Conflict · changed on the server", t.bad),
        };
        out.push(ui::chip(label, color, cx).into_any_element());
        if let Some(e) = &self.error {
            out.push(ui::notice(e.clone(), t.bad, cx).into_any_element());
        }
        let ent = cx.entity();
        let mut row = div().flex().flex_wrap().gap(S2).pt(S2);
        let save_label = if self.draft.base.is_none() { "Create on server  ⌘S" } else { "Save to server  ⌘S" };
        if self.draft.state != SaveState::Conflict {
            let e = ent.clone();
            row = row.child(ui::button("save", save_label, BtnKind::Primary, cx, move |_, _, cx| {
                e.update(cx, |d, cx| d.save_server(cx))
            }));
        }
        if self.draft.base.is_some() && self.draft.state == SaveState::Local {
            let e = ent.clone();
            row = row.child(ui::button("revert", "Discard local changes", BtnKind::Quiet, cx, move |_, _, cx| {
                let e = e.clone();
                confirm(cx, "Discard local changes?", "", "Discard", true, move |_, cx| {
                    e.update(cx, |d, cx| d.revert(cx))
                })
            }));
        }
        out.push(row.into_any_element());

        if self.draft.state == SaveState::Conflict {
            let remote = self.draft.remote.clone().unwrap_or(Value::Null);
            let local = self.draft.local.clone();
            let deleted = remote.is_null();
            let differing: Vec<&str> = K::EDITABLE
                .iter()
                .copied()
                .filter(|f| sync::norm(remote.get(*f)) != sync::norm(local.get(*f)))
                .collect();
            out.push(ui::rule(cx).into_any_element());
            out.push(ui::eyebrow("Resolve", cx).into_any_element());
            out.push(
                div()
                    .text_sm()
                    .text_color(t.ink_2)
                    .child(if deleted {
                        "Deleted on the server.".to_string()
                    } else {
                        format!("Both changed: {}.", differing.join(", "))
                    })
                    .into_any_element(),
            );
            let (a, b, c) = (ent.clone(), ent.clone(), ent.clone());
            out.push(
                div()
                    .flex()
                    .flex_col()
                    .gap(S2)
                    .pt(S2)
                    .child(ui::button("merge", "Merge for review", BtnKind::Primary, cx, move |_, _, cx| {
                        a.update(cx, |d, cx| d.resolve(Resolution::Merge, cx))
                    }))
                    .child(ui::button("keep", "Keep mine (overwrite server)", BtnKind::Quiet, cx, move |_, _, cx| {
                        let b = b.clone();
                        confirm(cx, "Overwrite the server's version?", "", "Overwrite", true, move |_, cx| {
                            b.update(cx, |d, cx| d.resolve(Resolution::KeepMine, cx))
                        })
                    }))
                    .when(!deleted, |d| {
                        d.child(ui::button("theirs", "Take theirs", BtnKind::Quiet, cx, move |_, _, cx| {
                            c.update(cx, |d, cx| d.resolve(Resolution::TakeTheirs, cx))
                        }))
                    })
                    .into_any_element(),
            );
            if !deleted {
                let theirs = remote.get(K::TEXT).and_then(|v| v.as_str()).unwrap_or("").to_string();
                out.push(
                    ui::field_row(
                        "Server's text",
                        div().text_xs().text_color(t.ink_2).max_h(px(160.)).overflow_hidden().child(theirs),
                        cx,
                    )
                    .into_any_element(),
                );
            }
        }

        if !self.fields.is_empty() {
            out.push(ui::rule(cx).into_any_element());
            for (_, f) in &self.fields {
                out.push(div().py(px(4.)).child(f.clone()).into_any_element());
            }
        }

        out.push(ui::rule(cx).into_any_element());
        let e = ent.clone();
        out.push(
            div()
                .flex()
                .items_center()
                .justify_between()
                .child(ui::eyebrow("Media", cx))
                .child(ui::button("insert", "Insert file…", BtnKind::Quiet, cx, move |_, w, cx| {
                    e.update(cx, |d, cx| d.pick_and_upload(w, cx))
                }))
                .into_any_element(),
        );

        for (i, u) in self.uploads.iter().enumerate() {
            let p = u.progress.clone();
            let frac = p.fraction();
            out.push(
                div()
                    .flex()
                    .flex_col()
                    .gap(px(3.))
                    .py(px(4.))
                    .child(
                        div()
                            .flex()
                            .justify_between()
                            .text_xs()
                            .child(u.name.clone())
                            .child(format!("{}%", (frac * 100.0) as u32)),
                    )
                    .child(
                        div().w_full().h(px(2.)).bg(t.rule).child(div().h_full().w(gpui::relative(frac)).bg(t.accent)),
                    )
                    .child(ui::button(
                        SharedString::from(format!("cancel-{i}")),
                        "Cancel",
                        BtnKind::Quiet,
                        cx,
                        move |_, _, _| p.cancel(),
                    ))
                    .into_any_element(),
            );
        }
        let refs = farfield_core::merge::refs(self.draft.local[K::TEXT].as_str().unwrap_or(""));
        if !refs.is_empty() {
            out.push(
                ui::field_row(
                    "References",
                    div().flex().flex_col().children(refs.into_iter().map(|r| ui::mono(r, cx))),
                    cx,
                )
                .into_any_element(),
            );
        }
        let words = self.editor.update(cx, |e, _| e.words());
        out.push(ui::mono(format!("{words} words · key {}", self.draft.key), cx).pt(S4).into_any_element());
        out
    }
}

/// Keep the person's edits but take the server's bookkeeping fields.
fn merge_keep_bookkeeping(server: &Value, mine: &Value) -> Value {
    let mut out = mine.clone();
    if let (Some(o), Some(s)) = (out.as_object_mut(), server.as_object()) {
        for k in ["slug", "cid", "createdAt", "updatedAt", "publishedAt", "id"] {
            if let Some(v) = s.get(k) {
                o.insert(k.into(), v.clone());
            }
        }
    }
    out
}

impl<K: Kind + 'static> Render for DraftDoc<K> {
    fn render(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        crate::perf::lap("doc-open", "draft-render");
        let _ = S4;
        self.render_body(cx)
    }
}
