//! Feed: the timeline of short posts, newest first, each with its photos.
//!
//! The composer sits on top: write, attach (file dialog or drop), and post
//! explicitly (the button, ⌘S or ⌘↵ — never on typing). Media goes up in the
//! same request through feed's multipart endpoint, with progress and cancel;
//! trailing #hashtags become tags. Editing a post opens it as a local draft
//! (saved on this Mac as you type, to the server on ⌘S, conflicts kept and
//! resolved like any document). The timeline pages by keyset cursor and
//! revalidates with ETags; offline it shows what it last had and says so.

use crate::app::{self, describe, log, Health};
use crate::shell::{confirm, set_health, toast};
use crate::theme::{theme, FONT_DOC, FONT_MONO, MEASURE, S1, S2, S3, S4, S5};
use crate::ui::doc_editor::{DocEditor, DocEvent};
use crate::ui::draft_doc::{DraftDoc, DraftEvent, FieldKind, FieldSpec};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, Kind as BtnKind};
use crate::workspace::Workspace;
use crate::ws::blobs::{decode, dominant, Thumb, Thumbs};
use farfield_core::api::ext_media;
use farfield_core::api::feed::{self, Post};
use farfield_core::store::{Draft, SaveState};
use farfield_core::sync::{self, FeedPost};
use farfield_core::upload::Progress;
use farfield_core::{ApiError, Freshness, Latest};
use gpui::{
    actions, div, img, prelude::*, px, AnyElement, App, Context, Entity, KeyBinding, ObjectFit, RenderImage,
    ScrollHandle, SharedString, StyledImage, Window,
};
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

actions!(feed_ws, [Publish]);

const PAGE: u32 = 20;
/// The composer's local draft area (apart from edits to existing posts).
const COMPOSER: &str = "feed-composer";
/// Longest side of an attachment preview in the composer.
const ATTACH_PX: u32 = 240;

pub struct FeedWs {
    posts: Vec<Post>,
    has_more: bool,
    loading: bool,
    freshness: Option<Freshness>,
    error: Option<String>,
    latest: Arc<Latest>,
    scroll: ScrollHandle,
    search: Entity<TextField>,
    composer: Entity<DocEditor>,
    /// Debounced write of the composer to its local draft.
    composer_save: Option<gpui::Task<()>>,
    attachments: Vec<PathBuf>,
    attach_previews: HashMap<PathBuf, Arc<RenderImage>>,
    /// The post being sent, while it is.
    posting: Option<Progress>,
    selected: Option<String>,
    /// The post open in the editor (center pane), if any.
    editing: Option<String>,
    open: HashMap<String, Entity<DraftDoc<FeedPost>>>,
    /// Edits on this Mac that never reached the server.
    drafts: Vec<Draft>,
    release_media: bool,
    thumbs: Entity<Thumbs>,
    retired: Vec<Arc<RenderImage>>,
}

fn tag_specs() -> Vec<FieldSpec> {
    vec![FieldSpec { key: "tags", label: "Tags", placeholder: "comma, separated", kind: FieldKind::Tags }]
}

impl FeedWs {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        cx.bind_keys([KeyBinding::new("cmd-enter", Publish, Some("Feed"))]);
        let search = cx.new(|cx| TextField::new(w, cx, "", "Filter posts by text or #tag  ⌘F"));
        cx.subscribe_in(&search, w, |this: &mut Self, _, e: &FieldEvent, w, cx| match e {
            FieldEvent::Changed => cx.notify(),
            FieldEvent::Down => this.step(1, cx),
            FieldEvent::Up => this.step(-1, cx),
            FieldEvent::Submit => {
                if let Some(s) = this.selected.clone() {
                    this.edit(s, w, cx)
                }
            }
            _ => {}
        })
        .detach();
        let session = app::session(cx);
        let composer =
            cx.new(|cx| DocEditor::new(w, cx, "", "What's happening? Trailing #tags become tags.", Some(session)));
        cx.subscribe_in(&composer, w, |this: &mut Self, _, e: &DocEvent, _w, cx| match e {
            DocEvent::Changed => {
                this.persist_composer(cx);
                cx.notify()
            }
            DocEvent::FilesDropped(p) => this.attach(p.clone(), cx),
            DocEvent::Blur => {}
        })
        .detach();
        let thumbs = cx.new(|_| Thumbs::new());
        cx.observe(&thumbs, |_, _, cx| cx.notify()).detach();
        let mut this = FeedWs {
            posts: Vec::new(),
            has_more: false,
            loading: false,
            freshness: None,
            error: None,
            latest: Arc::new(Latest::default()),
            scroll: ScrollHandle::new(),
            search,
            composer,
            composer_save: None,
            attachments: Vec::new(),
            attach_previews: HashMap::new(),
            posting: None,
            selected: None,
            editing: None,
            open: HashMap::new(),
            drafts: Vec::new(),
            release_media: false,
            thumbs,
            retired: Vec::new(),
        };
        this.reload(cx);
        this.restore_composer(cx);
        this
    }

    // ── loading ──

    fn reload(&mut self, cx: &mut Context<Self>) {
        self.thumbs.update(cx, |t, _| t.retry_failed());
        self.load(None, cx);
        self.load_drafts(cx);
    }

    fn load_drafts(&mut self, cx: &mut Context<Self>) {
        let s = app::session(cx);
        let task = farfield_core::spawn(async move { s.drafts("feed").map(|d| d.list("feed")).unwrap_or_default() });
        cx.spawn(async move |this, cx| {
            if let Ok(list) = task.await {
                let _ = this.update(cx, |this, cx| {
                    this.drafts =
                        list.into_iter().filter(|d| d.state != SaveState::Saved && d.base.is_some()).collect();
                    cx.notify();
                });
            }
        })
        .detach();
    }

    /// Load page 1 (`before` None) or the page after `before`.
    fn load(&mut self, before: Option<String>, cx: &mut Context<Self>) {
        let ticket = self.latest.ticket();
        let latest = self.latest.clone();
        let s = app::session(cx);
        let append = before.is_some();
        self.loading = true;
        cx.notify();
        let task = farfield_core::spawn(async move { feed::posts(&s, before.as_deref(), PAGE).await });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                if !latest.is_current(ticket) {
                    return;
                }
                this.loading = false;
                match r {
                    Ok(Ok(l)) => {
                        let (posts, more) = l.value;
                        if !append {
                            this.posts.clear();
                        }
                        // a full last page looks like "more"; the empty page after it ends the list
                        this.has_more = more && !posts.is_empty();
                        let n = posts.len();
                        let fresh: Vec<Post> = posts.into_iter().filter(|p| !this_has(&this.posts, &p.slug)).collect();
                        this.posts.extend(fresh);
                        this.error = None;
                        let h = match &l.freshness {
                            Freshness::Live => Health::Up,
                            Freshness::Stale { error, .. } => Health::Down(error.to_string()),
                        };
                        set_health(cx, "feed", h);
                        this.freshness = Some(l.freshness);
                        log("feed-loaded", &[("count", &this.posts.len().to_string()), ("page", &n.to_string())]);
                    }
                    Ok(Err(e)) => {
                        if e.is_auth() {
                            set_health(cx, "feed", Health::NoAuth);
                        } else if e.is_offline() {
                            set_health(cx, "feed", Health::Down(e.to_string()));
                        }
                        this.error = Some(if append && e.is_offline() {
                            "Offline — the next page wasn't loaded before the connection went. ⌘R tries again.".into()
                        } else {
                            describe(&e)
                        });
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
        let Some(last) = self.posts.last() else { return };
        self.load(Some(last.cursor()), cx);
    }

    fn visible(&self, cx: &App) -> Vec<Post> {
        let q = self.search.read(cx).text().trim().to_lowercase();
        let q = q.as_str();
        self.posts
            .iter()
            .filter(|p| {
                q.is_empty()
                    || p.body.to_lowercase().contains(q)
                    || p.tags.iter().any(|t| t.contains(q.trim_start_matches('#')))
                    || p.slug.contains(q)
            })
            .cloned()
            .collect()
    }

    fn post(&self, slug: &str) -> Option<&Post> {
        self.posts.iter().find(|p| p.slug == slug)
    }

    fn step(&mut self, by: i32, cx: &mut Context<Self>) {
        let v = self.visible(cx);
        if v.is_empty() {
            return;
        }
        let cur = self.selected.as_ref().and_then(|s| v.iter().position(|p| &p.slug == s));
        let next = match cur {
            None => 0,
            Some(i) => (i as i32 + by).clamp(0, v.len() as i32 - 1) as usize,
        };
        // child 0 is the composer block; posts follow
        self.scroll.scroll_to_item(next + 1);
        self.selected = Some(v[next].slug.clone());
        cx.notify();
    }

    // ── composing ──

    /// The composer is local work like any draft: written to this Mac a moment
    /// after typing stops (text and attachment paths), restored at launch,
    /// and dropped once posted.
    fn persist_composer(&mut self, cx: &mut Context<Self>) {
        self.composer_save = Some(cx.spawn(async move |this, cx| {
            cx.background_executor().timer(std::time::Duration::from_millis(400)).await;
            let _ = this.update(cx, |this, cx| {
                let body = this.composer.update(cx, |e, _| e.text());
                let files: Vec<String> = this.attachments.iter().map(|p| p.display().to_string()).collect();
                let s = app::session(cx);
                farfield_core::spawn(async move {
                    let Ok(d) = s.drafts(COMPOSER) else { return };
                    if body.trim().is_empty() && files.is_empty() {
                        let _ = d.discard(COMPOSER, "composer");
                    } else {
                        let mut draft = farfield_core::sync::new_draft(
                            COMPOSER,
                            serde_json::json!({"body": body, "attachments": files}),
                        );
                        draft.key = "composer".into();
                        let _ = d.save(&draft);
                    }
                });
            });
        }));
    }

    fn restore_composer(&mut self, cx: &mut Context<Self>) {
        let s = app::session(cx);
        let task = farfield_core::spawn(async move { s.drafts(COMPOSER).ok()?.load(COMPOSER, "composer") });
        cx.spawn(async move |this, cx| {
            if let Ok(Some(d)) = task.await {
                let _ = this.update(cx, |this, cx| {
                    let body = d.local["body"].as_str().unwrap_or("").to_string();
                    if !body.is_empty() {
                        this.composer.update(cx, |e, cx| e.set_text(&body, cx));
                    }
                    let files: Vec<PathBuf> = d.local["attachments"]
                        .as_array()
                        .map(|a| {
                            a.iter().filter_map(|v| v.as_str()).map(PathBuf::from).filter(|p| p.exists()).collect()
                        })
                        .unwrap_or_default();
                    this.attach(files, cx);
                    log("feed-composer-restored", &[("chars", &body.len().to_string())]);
                });
            }
        })
        .detach();
    }

    fn attach(&mut self, paths: Vec<PathBuf>, cx: &mut Context<Self>) {
        for p in paths {
            if p.is_dir() || self.attachments.contains(&p) {
                continue;
            }
            log("feed-attach", &[("file", &p.display().to_string())]);
            self.attachments.push(p.clone());
            self.persist_composer(cx);
            let task = farfield_core::runtime().spawn_blocking({
                let p = p.clone();
                move || std::fs::read(&p).ok().and_then(|b| decode(&b, ATTACH_PX))
            });
            cx.spawn(async move |this, cx| {
                if let Ok(Some(im)) = task.await {
                    let _ = this.update(cx, |this, cx| {
                        if this.attachments.contains(&p) {
                            this.attach_previews.insert(p, im);
                        } else {
                            this.retired.push(im);
                        }
                        cx.notify();
                    });
                }
            })
            .detach();
        }
        cx.notify();
    }

    fn detach_file(&mut self, p: &PathBuf, cx: &mut Context<Self>) {
        self.attachments.retain(|a| a != p);
        self.persist_composer(cx);
        if let Some(im) = self.attach_previews.remove(p) {
            self.retired.push(im);
        }
        cx.notify();
    }

    fn pick_attach(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        let rx = cx.prompt_for_paths(gpui::PathPromptOptions {
            files: true,
            directories: false,
            multiple: true,
            prompt: Some("Attach".into()),
        });
        cx.spawn_in(w, async move |this, cx| {
            if let Ok(Ok(Some(paths))) = rx.await {
                let _ = this.update(cx, |this, cx| this.attach(paths, cx));
            }
        })
        .detach();
    }

    fn composer_text(&self, cx: &mut Context<Self>) -> String {
        self.composer.update(cx, |e, _| e.text())
    }

    /// Post what is in the composer — after one explicit confirmation.
    fn publish(&mut self, cx: &mut Context<Self>) {
        if self.posting.is_some() {
            return;
        }
        let text = self.composer_text(cx);
        let (body, tags) = feed::split_hashtags(&text);
        let files = self.attachments.clone();
        if body.trim().is_empty() && files.is_empty() {
            toast(cx, "Write something or attach a photo first.", true);
            return;
        }
        let first = body.lines().next().unwrap_or("").chars().take(80).collect::<String>();
        let mut detail = if first.is_empty() { "A post with only media".to_string() } else { format!("“{first}”") };
        if !files.is_empty() {
            detail.push_str(&format!(" with {} attachment{}", files.len(), if files.len() == 1 { "" } else { "s" }));
        }
        if !tags.is_empty() {
            detail
                .push_str(&format!(", tagged {}", tags.iter().map(|t| format!("#{t}")).collect::<Vec<_>>().join(" ")));
        }
        detail.push_str(" goes to the public feed.");
        let ent = cx.entity();
        confirm(cx, "Post this?", detail, "Post", false, move |_, cx| {
            ent.update(cx, |this, cx| this.send(body, tags, files, cx))
        });
    }

    fn send(&mut self, body: String, tags: Vec<String>, files: Vec<PathBuf>, cx: &mut Context<Self>) {
        let total: u64 = files.iter().filter_map(|f| std::fs::metadata(f).ok()).map(|m| m.len()).sum();
        let progress = Progress::new(total);
        self.posting = Some(progress.clone());
        log("feed-post-start", &[("files", &files.len().to_string())]);
        let s = app::session(cx);
        let pr = progress.clone();
        let task = farfield_core::spawn(async move {
            if files.is_empty() {
                feed::create(&s, &body, &tags).await
            } else {
                feed::create_with_media(&s, &body, &tags, &files, &pr).await
            }
        });
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
                this.posting = None;
                match r {
                    Ok(Ok(p)) => {
                        log("feed-posted", &[("slug", &p.value.slug)]);
                        this.composer.update(cx, |e, cx| e.set_text("", cx));
                        for a in std::mem::take(&mut this.attachments) {
                            if let Some(im) = this.attach_previews.remove(&a) {
                                this.retired.push(im);
                            }
                        }
                        this.persist_composer(cx);
                        this.selected = Some(p.value.slug.clone());
                        this.posts.insert(0, p.value);
                        toast(cx, "Posted.", false);
                        this.load(None, cx);
                    }
                    Ok(Err(ApiError::Cancelled)) => {
                        log("feed-post-cancelled", &[]);
                        toast(cx, "Cancelled — nothing was posted. Your text and attachments are still here.", false);
                    }
                    Ok(Err(e @ ApiError::Uncertain(_))) => {
                        log("feed-post-uncertain", &[("error", &e.to_string())]);
                        toast(cx, "The connection dropped mid-post — it may or may not have gone up. Check the timeline before posting again.", true);
                        this.load(None, cx);
                    }
                    Ok(Err(e)) => {
                        if e.is_auth() {
                            set_health(cx, "feed", Health::NoAuth);
                        } else if e.is_offline() {
                            set_health(cx, "feed", Health::Down(e.to_string()));
                        }
                        log("feed-post-failed", &[("error", &e.to_string())]);
                        toast(cx, format!("Not posted: {}", describe(&e)), true);
                    }
                    Err(e) => toast(cx, e.to_string(), true),
                }
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    // ── editing and deleting ──

    fn edit(&mut self, slug: String, w: &mut Window, cx: &mut Context<Self>) {
        self.selected = Some(slug.clone());
        if self.open.contains_key(&slug) {
            self.editing = Some(slug);
            cx.notify();
            return;
        }
        let s = app::session(cx);
        let k = slug.clone();
        let task = farfield_core::spawn(async move { sync::open::<FeedPost>(&s, &k).await });
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

    fn open_draft(&mut self, d: Draft, w: &mut Window, cx: &mut Context<Self>) {
        let key = d.key.clone();
        let doc = cx.new(|cx| DraftDoc::<FeedPost>::new(d, None, tag_specs(), "Write the post…", w, cx));
        cx.subscribe(&doc, |this, _doc, e: &DraftEvent, cx| match e {
            DraftEvent::Saved => {
                this.load_drafts(cx);
                this.load(None, cx);
            }
            DraftEvent::Touched => this.load_drafts(cx),
            DraftEvent::Renamed { .. } => {}
        })
        .detach();
        log("feed-edit", &[("slug", &key)]);
        self.open.insert(key.clone(), doc.clone());
        self.editing = Some(key.clone());
        self.selected = Some(key);
        doc.read(cx).editor.read(cx).focus_editor(w);
        cx.notify();
    }

    fn close_editor(&mut self, cx: &mut Context<Self>) {
        if let Some(k) = self.editing.take() {
            if let Some(d) = self.open.get(&k) {
                d.update(cx, |d, cx| d.flush(cx));
            }
        }
        cx.notify();
    }

    fn delete(&mut self, cx: &mut Context<Self>) {
        let Some(slug) = self.selected.clone() else { return };
        let doc = self.open.get(&slug).cloned();
        let post = self.post(&slug).cloned();
        // the version this person last saw: the open draft's base, else the list row's CID
        let if_match = match &doc {
            Some(d) => d.read(cx).draft.base_etag.clone(),
            None => post.as_ref().filter(|p| !p.cid.is_empty()).map(|p| format!("\"{}\"", p.cid)),
        };
        let media = post.as_ref().map(|p| ext_media::blob_refs(&p.body).len()).unwrap_or(0);
        let release = self.release_media && media > 0;
        let body = if release {
            format!("The post is removed, and its {media} media file{} too — unless another post or entry still embeds {}. Blobs have no backup.", if media == 1 { "" } else { "s" }, if media == 1 { "it" } else { "them" })
        } else if media > 0 {
            format!(
                "The post is removed. Its {media} media file{} stay{} in blobs.",
                if media == 1 { "" } else { "s" },
                if media == 1 { "s" } else { "" }
            )
        } else {
            "The post is removed from the feed.".into()
        };
        let ent = cx.entity();
        confirm(cx, "Delete this post?", body, "Delete", true, move |_, cx| {
            let s = app::session(cx);
            let k = slug.clone();
            let task = farfield_core::spawn(async move { feed::delete(&s, &k, release, if_match.as_deref()).await });
            ent.update(cx, |_, cx| {
                cx.spawn(async move |this, cx| {
                    let r = task.await;
                    let _ = this.update(cx, |this, cx| {
                        match r {
                            Ok(Ok(())) => {
                                log("feed-delete", &[("slug", &slug), ("release", if release { "1" } else { "0" })]);
                                if let Ok(d) = app::session(cx).drafts("feed") {
                                    let _ = d.discard("feed", &slug);
                                }
                                this.open.remove(&slug);
                                this.posts.retain(|p| p.slug != slug);
                                if this.editing.as_deref() == Some(slug.as_str()) {
                                    this.editing = None;
                                }
                                this.selected = None;
                                toast(
                                    cx,
                                    if release { "Deleted. Media not used elsewhere is released." } else { "Deleted." },
                                    false,
                                );
                                this.load_drafts(cx);
                                this.load(None, cx);
                            }
                            Ok(Err(ApiError::Precondition { .. })) => toast(
                                cx,
                                "Not deleted: it changed on the server since you opened it. ⌘R to see the change.",
                                true,
                            ),
                            Ok(Err(ApiError::NotFound)) => {
                                toast(cx, "Already gone from the server.", false);
                                this.posts.retain(|p| p.slug != slug);
                                this.selected = None;
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

    // ── rendering ──

    fn render_composer(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let text = self.composer.update(cx, |e, _| e.text());
        let (_, tags) = feed::split_hashtags(&text);
        let ready = !text.trim().is_empty() || !self.attachments.is_empty();
        let e = cx.entity();
        let e2 = cx.entity();
        let mut col = div()
            .flex()
            .flex_col()
            .gap(S2)
            .pb(S4)
            .child(ui::eyebrow("New post", cx))
            .child(div().h(px(132.)).w_full().border_b_1().border_color(t.rule).child(self.composer.clone()));
        if !self.attachments.is_empty() {
            col = col.child(div().flex().flex_wrap().gap(S2).pt(S1).children(self.attachments.iter().enumerate().map(
                |(i, p)| {
                    let name = p.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default();
                    let size = std::fs::metadata(p).map(|m| m.len() as i64).unwrap_or(0);
                    let face: AnyElement = match self.attach_previews.get(p) {
                        Some(im) => img(im.clone()).size_full().object_fit(ObjectFit::Cover).into_any_element(),
                        None => div()
                            .size_full()
                            .flex()
                            .items_center()
                            .justify_center()
                            .font_family(FONT_MONO)
                            .text_xs()
                            .text_color(t.ink_2)
                            .child(
                                p.extension()
                                    .map(|x| x.to_string_lossy().to_uppercase())
                                    .unwrap_or_else(|| "FILE".into()),
                            )
                            .into_any_element(),
                    };
                    let pp = p.clone();
                    let ent = cx.entity();
                    div()
                        .w(px(96.))
                        .flex()
                        .flex_col()
                        .gap(px(3.))
                        .child(div().w(px(96.)).h(px(96.)).rounded(px(4.)).overflow_hidden().bg(t.paper_2).child(face))
                        .child(div().text_xs().text_color(t.ink_2).truncate().child(name))
                        .child(div().flex().justify_between().child(ui::mono(ui::bytes(size), cx)).when(
                            self.posting.is_none(),
                            |d| {
                                d.child(
                                    div()
                                        .id(("detach", i))
                                        .text_xs()
                                        .text_color(t.ink_3)
                                        .cursor_pointer()
                                        .hover(|s| s.text_color(t.bad))
                                        .child("remove")
                                        .on_click(move |_, _, cx| ent.update(cx, |this, cx| this.detach_file(&pp, cx))),
                                )
                            },
                        ))
                },
            )));
        }
        let bar: AnyElement =
            if let Some(p) = &self.posting {
                let p = p.clone();
                let frac = p.fraction();
                div()
                    .flex()
                    .items_center()
                    .gap(S3)
                    .child(div().text_sm().text_color(t.ink).child(if p.total() == 0 {
                        "Posting…".to_string()
                    } else {
                        format!("Uploading {}%", (frac * 100.) as u32)
                    }))
                    .child(
                        div().flex_1().h(px(2.)).bg(t.rule).child(
                            div().h_full().w(gpui::relative(if p.total() == 0 { 1.0 } else { frac })).bg(t.accent),
                        ),
                    )
                    .child(ui::mono(format!("{} / {}", ui::bytes(p.sent() as i64), ui::bytes(p.total() as i64)), cx))
                    .child(ui::button("cancel-post", "Cancel", BtnKind::Quiet, cx, move |_, _, _| p.cancel()))
                    .into_any_element()
            } else {
                div()
                    .flex()
                    .items_center()
                    .gap(S2)
                    .child(ui::button("attach", "Attach…", BtnKind::Quiet, cx, move |_, w, cx| {
                        e.update(cx, |this, cx| this.pick_attach(w, cx))
                    }))
                    .child(div().flex_1().text_xs().text_color(t.ink_3).truncate().child(if tags.is_empty() {
                        "Drop photos on the text to attach them.".to_string()
                    } else {
                        format!("tags: {}", tags.iter().map(|t| format!("#{t}")).collect::<Vec<_>>().join(" "))
                    }))
                    .child(if ready {
                        ui::button("publish", "Post  ⌘↵", BtnKind::Primary, cx, move |_, _, cx| {
                            e2.update(cx, |this, cx| this.publish(cx))
                        })
                        .into_any_element()
                    } else {
                        ui::button_disabled("Post  ⌘↵", cx).into_any_element()
                    })
                    .into_any_element()
            };
        col.child(bar).into_any_element()
    }

    fn render_post(&self, p: &Post, i: usize, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let on = self.selected.as_deref() == Some(p.slug.as_str());
        let text = ext_media::strip_embeds(&p.body);
        let media = ext_media::blob_refs(&p.body);
        let edited_here = self.open.get(&p.slug).is_some_and(|d| d.read(cx).is_dirty())
            || self.drafts.iter().any(|d| d.key == p.slug);
        let edited = !p.updated_at.is_empty() && p.updated_at != p.created_at;
        let mut meta = div()
            .flex()
            .items_center()
            .gap(S2)
            .font_family(FONT_MONO)
            .text_xs()
            .text_color(t.ink_3)
            .child(ui::when(&p.created_at))
            .when(edited, |d| d.child("· edited"))
            .children(p.tags.iter().map(|tg| div().text_color(t.ink_2).child(format!("#{tg}"))));
        if edited_here {
            meta = meta.child(div().flex_1()).child(ui::chip("edited on this Mac", t.warn, cx));
        }
        let mut block = div()
            .id(SharedString::from(format!("post-{}", p.slug)))
            .flex()
            .flex_col()
            .gap(S2)
            .py(S4)
            .pl(S3)
            .border_l_2()
            .border_color(if on { t.accent } else { gpui::transparent_black() })
            .cursor_pointer()
            .child(meta);
        if !text.is_empty() {
            block = block.child(
                div().font_family(FONT_DOC).text_size(px(17.)).line_height(px(26.)).text_color(t.ink).child(text),
            );
        }
        if !media.is_empty() {
            block = block.child(self.render_media(&media, i, cx));
        }
        let slug = p.slug.clone();
        let slug2 = p.slug.clone();
        let e = cx.entity();
        div()
            .child(block.on_click(move |ev, w, cx| {
                let s = slug.clone();
                e.update(cx, |this, cx| {
                    if ev.click_count() >= 2 {
                        this.edit(s, w, cx)
                    } else {
                        this.selected = Some(s);
                        cx.notify()
                    }
                })
            }))
            .child(ui::rule(cx))
            .id(SharedString::from(format!("postwrap-{slug2}")))
            .into_any_element()
    }

    /// A post's photos: one is shown wide at its own aspect; several sit in
    /// a row of squares.
    fn render_media(&self, cids: &[String], i: usize, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let thumbs = self.thumbs.clone();
        if cids.len() == 1 {
            let cid = &cids[0];
            let th = thumbs.update(cx, |th, cx| th.for_cid(cid, cx));
            let meta = thumbs.read(cx).meta(cid).cloned();
            let aspect = meta
                .as_ref()
                .filter(|m| m.width > 0 && m.height > 0)
                .map(|m| m.height as f32 / m.width as f32)
                .unwrap_or(0.66);
            let w = 520f32;
            let h = (w * aspect).clamp(120., 420.);
            let bg = dominant(meta.as_ref(), &t);
            let face: AnyElement = match th {
                Thumb::Ready(im) => img(im).size_full().object_fit(ObjectFit::Cover).into_any_element(),
                Thumb::None => media_label(meta.as_ref(), &t),
                Thumb::Loading => div().into_any_element(),
            };
            return div()
                .id(("media1", i))
                .w(px(w))
                .max_w_full()
                .h(px(h))
                .rounded(px(4.))
                .overflow_hidden()
                .bg(bg)
                .child(face)
                .into_any_element();
        }
        let size = 132f32;
        div()
            .flex()
            .flex_wrap()
            .gap(S2)
            .children(cids.iter().take(8).map(|cid| {
                let th = thumbs.update(cx, |th, cx| th.for_cid(cid, cx));
                let meta = thumbs.read(cx).meta(cid).cloned();
                let face: AnyElement = match th {
                    Thumb::Ready(im) => img(im).size_full().object_fit(ObjectFit::Cover).into_any_element(),
                    Thumb::None => media_label(meta.as_ref(), &t),
                    Thumb::Loading => div().into_any_element(),
                };
                div()
                    .w(px(size))
                    .h(px(size))
                    .rounded(px(4.))
                    .overflow_hidden()
                    .bg(dominant(meta.as_ref(), &t))
                    .child(face)
            }))
            .when(cids.len() > 8, |d| {
                d.child(div().text_sm().text_color(t.ink_2).child(format!("+{}", cids.len() - 8)))
            })
            .into_any_element()
    }

    fn render_timeline(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let posts = self.visible(cx);
        let status: Option<AnyElement> = match (&self.error, &self.freshness) {
            (Some(e), _) => Some(ui::notice(e.clone(), t.bad, cx).into_any_element()),
            (None, Some(Freshness::Stale { age_ms, .. })) => Some(
                ui::notice(
                    format!(
                        "Offline — showing the timeline as it was {} ago. Posting waits until feed is back.",
                        crate::ws::blobs::ago(*age_ms)
                    ),
                    t.warn,
                    cx,
                )
                .into_any_element(),
            ),
            _ => None,
        };
        let readout = if self.posts.is_empty() {
            String::new()
        } else {
            format!(
                "{} post{} loaded{}",
                self.posts.len(),
                if self.posts.len() == 1 { "" } else { "s" },
                if self.has_more { " · more below" } else { "" }
            )
        };
        let mut col = div().w_full().flex().flex_col().pt(S5).child(
            div()
                .flex()
                .flex_col()
                .gap(S3)
                .pb(S4)
                .child(
                    div()
                        .flex()
                        .items_end()
                        .gap(S3)
                        .child(
                            div()
                                .text_size(px(20.))
                                .text_color(t.ink)
                                .font_weight(gpui::FontWeight::MEDIUM)
                                .child("Feed"),
                        )
                        .child(div().pb(px(3.)).font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(readout)),
                )
                .child(self.render_composer(cx)),
        );
        if let Some(s) = status {
            col = col.child(div().pb(S3).child(s));
        }
        if !self.drafts.is_empty() {
            let n = self.drafts.len();
            col = col.child(
                div()
                    .pb(S3)
                    .flex()
                    .flex_col()
                    .gap(px(4.))
                    .child(ui::notice(
                        format!(
                            "{n} edited post{} on this Mac {} not on the server yet.",
                            if n == 1 { "" } else { "s" },
                            if n == 1 { "is" } else { "are" }
                        ),
                        t.warn,
                        cx,
                    ))
                    .children(self.drafts.iter().map(|d| {
                        let k = d.key.clone();
                        let e = cx.entity();
                        let preview = d.local["body"]
                            .as_str()
                            .unwrap_or("")
                            .lines()
                            .next()
                            .unwrap_or("")
                            .chars()
                            .take(60)
                            .collect::<String>();
                        div()
                            .id(SharedString::from(format!("draft-{k}")))
                            .pl(S3)
                            .text_sm()
                            .text_color(t.accent)
                            .cursor_pointer()
                            .hover(|s| s.underline())
                            .child(format!("Open “{preview}”"))
                            .on_click(move |_, w, cx| e.update(cx, |this, cx| this.edit(k.clone(), w, cx)))
                    })),
            );
        }
        col = col.child(div().flex().items_center().gap(S3).pb(S2).child(div().flex_1().child(self.search.clone())));
        col = col.child(ui::rule(cx));
        // the scroll container's children are [head, post…, tail], so a
        // post's index there is 1 + its index in `posts` (see `step`)
        let mut rows: Vec<AnyElement> = Vec::new();
        if posts.is_empty() {
            let msg = if self.loading {
                "Loading the timeline…"
            } else if !self.posts.is_empty() {
                "No loaded post matches the filter."
            } else if self.error.is_some() {
                "The timeline couldn't be loaded. ⌘R tries again."
            } else {
                "Nothing posted yet. Write the first one above — ⌘↵ posts it."
            };
            rows.push(div().py(S5).text_sm().text_color(t.ink_2).child(msg).into_any_element());
        }
        for (i, p) in posts.iter().enumerate() {
            rows.push(self.render_post(p, i, cx));
        }
        if self.has_more {
            let e = cx.entity();
            rows.push(
                div()
                    .id("feed-more")
                    .py(S4)
                    .text_sm()
                    .text_color(t.accent)
                    .cursor_pointer()
                    .hover(|s| s.underline())
                    .child(if self.loading { "Loading…" } else { "Load more" })
                    .on_click(move |_, _, cx| e.update(cx, |this, cx| this.load_more(cx)))
                    .into_any_element(),
            );
        }
        rows.push(div().h(px(64.)).into_any_element());
        let wrap = |el: AnyElement| {
            div().w_full().flex().justify_center().child(div().w_full().max_w(MEASURE).px(px(32.)).child(el))
        };
        div()
            .id("feed-scroll")
            .size_full()
            .overflow_y_scroll()
            .track_scroll(&self.scroll)
            .flex()
            .flex_col()
            .child(wrap(col.into_any_element()))
            .children(rows.into_iter().map(wrap))
            .into_any_element()
    }

    fn render_release_toggle(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let on = self.release_media;
        div()
            .id("release-media")
            .flex()
            .items_center()
            .gap(S2)
            .py(px(4.))
            .cursor_pointer()
            .child(
                div()
                    .w(px(14.))
                    .h(px(14.))
                    .rounded(px(3.))
                    .border_1()
                    .border_color(if on { t.accent } else { t.rule_strong })
                    .bg(if on { t.accent } else { gpui::transparent_black() })
                    .flex()
                    .items_center()
                    .justify_center()
                    .text_color(t.accent_ink)
                    .text_xs()
                    .when(on, |d| d.child("✓")),
            )
            .child(div().text_sm().text_color(t.ink_2).child("Also release its media"))
            .on_click(cx.listener(|this, _, _, cx| {
                this.release_media = !this.release_media;
                cx.notify()
            }))
            .into_any_element()
    }
}

fn this_has(posts: &[Post], slug: &str) -> bool {
    posts.iter().any(|p| p.slug == slug)
}

fn media_label(m: Option<&farfield_core::api::blobs::Meta>, t: &crate::theme::Theme) -> AnyElement {
    div()
        .size_full()
        .flex()
        .items_center()
        .justify_center()
        .font_family(FONT_MONO)
        .text_xs()
        .text_color(t.ink_2)
        .child(m.map(crate::ws::blobs::kind_label).unwrap_or_else(|| "MEDIA".into()))
        .into_any_element()
}

impl Workspace for FeedWs {
    fn inspector(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> Option<AnyElement> {
        let t = theme(cx).clone();
        let Some(slug) = self.selected.clone() else {
            return Some(
                div()
                    .flex()
                    .flex_col()
                    .gap(S2)
                    .child(ui::eyebrow("Post", cx))
                    .child(
                        div().text_sm().text_color(t.ink_2).child(
                            "Choose a post to see its details. Double-click (or Enter from the filter) edits it.",
                        ),
                    )
                    .child(
                        div()
                            .text_xs()
                            .text_color(t.ink_3)
                            .child("⌘N writes a new one · ⌘↵ or ⌘S posts it · ⌘R refreshes."),
                    )
                    .into_any_element(),
            );
        };
        let post = self.post(&slug).cloned();
        let mut col = div().flex().flex_col().gap(S2);
        if let Some(doc) = self.open.get(&slug).cloned().filter(|_| self.editing.as_deref() == Some(slug.as_str())) {
            col = col.child(ui::eyebrow("Editing post", cx)).children(doc.update(cx, |d, cx| d.render_inspector(cx)));
            col = col.child(ui::rule(cx));
        } else {
            col = col.child(ui::eyebrow("Post", cx));
            let e = cx.entity();
            let s2 = slug.clone();
            col = col.child(ui::button("edit", "Edit post", BtnKind::Quiet, cx, move |_, w, cx| {
                let s = s2.clone();
                e.update(cx, |this, cx| this.edit(s, w, cx))
            }));
        }
        if let Some(p) = &post {
            let media = ext_media::blob_refs(&p.body);
            let readout = |label: &'static str, v: String| {
                div()
                    .flex()
                    .justify_between()
                    .gap(S3)
                    .py(px(3.))
                    .child(div().text_xs().text_color(t.ink_2).child(label))
                    .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink).truncate().child(v))
            };
            col = col
                .child(readout("Slug", p.slug.clone()))
                .child(readout("Posted", ui::when(&p.created_at)))
                .when(!p.updated_at.is_empty() && p.updated_at != p.created_at, |d| {
                    d.child(readout("Edited", ui::when(&p.updated_at)))
                })
                .child(readout(
                    "Tags",
                    if p.tags.is_empty() {
                        "—".into()
                    } else {
                        p.tags.iter().map(|t| format!("#{t}")).collect::<Vec<_>>().join(" ")
                    },
                ))
                .child(readout("Media", media.len().to_string()))
                .when(!p.cid.is_empty(), |d| d.child(ui::field_row("CID", ui::mono(p.cid.clone(), cx), cx)));
            if !media.is_empty() {
                col = col.child(ui::field_row(
                    "Embeds",
                    div().flex().flex_col().children(media.iter().map(|c| ui::mono(format!("blob://{c}"), cx))),
                    cx,
                ));
            }
        }
        let e = cx.entity();
        col = col
            .child(ui::rule(cx))
            .child(self.render_release_toggle(cx))
            .child(
                div()
                    .text_xs()
                    .text_color(t.ink_3)
                    .child("Released media is deleted from blobs unless another post or entry still embeds it."),
            )
            .child(ui::button("delete", "Delete post…", BtnKind::Danger, cx, move |_, _, cx| {
                e.update(cx, |this, cx| this.delete(cx))
            }));
        Some(col.into_any_element())
    }

    fn commands(&self, _cx: &App) -> Vec<(&'static str, String, &'static str)> {
        let mut v = vec![
            ("new", "Feed: write a new post".to_string(), "⌘N"),
            ("attach", "Feed: attach files to the new post…".into(), ""),
        ];
        if self.editing.is_some() {
            v.push(("save", "Feed: save this post to the server".into(), "⌘S"));
            v.push(("timeline", "Feed: back to the timeline".into(), ""));
        } else {
            v.push(("publish", "Feed: post it".into(), "⌘↵"));
        }
        if self.selected.is_some() {
            v.push(("edit", "Feed: edit this post".into(), "↵"));
            v.push((
                "toggle-release",
                format!("Feed: {} release of media on delete", if self.release_media { "turn off" } else { "turn on" }),
                "",
            ));
            v.push(("delete", "Feed: delete this post…".into(), ""));
        }
        v
    }

    fn run_command(&mut self, id: &str, w: &mut Window, cx: &mut Context<Self>) {
        match id {
            "new" => Workspace::new_item(self, w, cx),
            "attach" => self.pick_attach(w, cx),
            "save" | "publish" => Workspace::save(self, w, cx),
            "timeline" => self.close_editor(cx),
            "edit" => {
                if let Some(s) = self.selected.clone() {
                    self.edit(s, w, cx)
                }
            }
            "toggle-release" => {
                self.release_media = !self.release_media;
                cx.notify()
            }
            "delete" => self.delete(cx),
            _ => {}
        }
    }

    fn focus_search(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        if self.editing.is_some() {
            self.close_editor(cx);
        }
        self.search.read(cx).focus(w);
    }
    fn new_item(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        self.close_editor(cx);
        self.scroll.set_offset(gpui::point(px(0.), px(0.)));
        self.composer.read(cx).focus_editor(w);
        cx.notify();
    }
    /// ⌘S: save the post being edited, or post the composer.
    fn save(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        match self.editing.as_ref().and_then(|k| self.open.get(k)).cloned() {
            Some(d) => d.update(cx, |d, cx| d.save_server(cx)),
            None => self.publish(cx),
        }
    }
    fn refresh(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        self.reload(cx)
    }
    fn dirty(&self, cx: &App) -> bool {
        !self.attachments.is_empty()
            || self.posting.is_some()
            || !self.drafts.is_empty()
            || self.open.values().any(|d| d.read(cx).is_dirty())
    }
}

impl Render for FeedWs {
    fn render(&mut self, w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = theme(cx).clone();
        for old in self.thumbs.update(cx, |th, _| th.take_retired()).into_iter().chain(self.retired.drain(..)) {
            let _ = w.drop_image(old);
        }
        let editing = self.editing.as_ref().and_then(|k| self.open.get(k)).cloned();
        let body: AnyElement = match editing {
            Some(doc) => {
                let e = cx.entity();
                div()
                    .size_full()
                    .flex()
                    .flex_col()
                    .child(
                        div()
                            .flex()
                            .items_center()
                            .gap(S2)
                            .px(S4)
                            .py(S2)
                            .border_b_1()
                            .border_color(t.rule)
                            .child(ui::button("back", "← Timeline", BtnKind::Quiet, cx, move |_, _, cx| {
                                e.update(cx, |this, cx| this.close_editor(cx))
                            }))
                            .child(div().flex_1())
                            .child(
                                div()
                                    .text_xs()
                                    .text_color(t.ink_3)
                                    .child("Saved on this Mac as you type · ⌘S saves to the server"),
                            ),
                    )
                    .child(div().flex_1().min_h_0().child(doc))
                    .into_any_element()
            }
            None => self.render_timeline(cx),
        };
        div()
            .size_full()
            .key_context("Feed")
            .on_action(cx.listener(|this, _: &Publish, w, cx| Workspace::save(this, w, cx)))
            .child(body)
    }
}
