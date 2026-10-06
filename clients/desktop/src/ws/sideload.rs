//! Sideload: iOS builds grouped by app, their install links, and the
//! expiring share links minted for testers.
//!
//! Builds come newest first under each bundle id; a provisioning profile that
//! has expired or expires within a week is flagged in Horizon. Uploads stream
//! from disk with progress and cancel; the server is idempotent by content, so
//! the same IPA twice is the same build. Share links are minted with a TTL, an
//! install cap and a label, listed with their state, and revoked on request.

use crate::app::{self, describe, log, Health};
use crate::shell::{confirm, set_health, toast};
use crate::theme::{theme, Theme, FONT_MONO, S1, S2, S3, S4, S5};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, Kind as BtnKind};
use crate::workspace::{PaletteItem, Workspace};
use farfield_core::api::ext_uploads::sideload_links::{self as sl, Expiry};
use farfield_core::api::sideload::{self, Build, Share};
use farfield_core::upload::Progress;
use farfield_core::{ApiError, Freshness, Latest};
use gpui::{div, prelude::*, px, AnyElement, App, Context, Entity, ExternalPaths, Hsla, SharedString, Window};
use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

const TTLS: [(&str, &str); 3] = [("30m", "30 min"), ("2h", "2 hours"), ("24h", "24 hours")];
const MAXES: [(&str, &str); 3] = [("1", "1 install"), ("3", "3 installs"), ("unlimited", "unlimited")];

#[derive(Clone, Copy, PartialEq)]
enum View {
    Apps,
    Shares,
}

struct Up {
    name: String,
    progress: Progress,
}

pub struct SideloadWs {
    view: View,
    builds: Vec<Build>,
    shares: Vec<Share>,
    shares_error: Option<String>,
    selected_app: Option<String>,
    selected_build: Option<String>,
    selected_share: Option<String>,
    loading: bool,
    loaded: bool,
    freshness: Option<Freshness>,
    error: Option<String>,
    latest: Arc<Latest>,
    search: Entity<TextField>,
    notes: Entity<TextField>,
    label: Entity<TextField>,
    ttl: usize,
    max: usize,
    minted: Option<Share>,
    minting: bool,
    uploads: Vec<Up>,
}

fn expiry_chip(exp: &str, t: &Theme) -> (String, Hsla) {
    match sl::expiry(exp) {
        Expiry::Unknown => ("no profile date".into(), t.ink_3),
        Expiry::Expired => ("profile expired".into(), t.signal),
        Expiry::Soon(0) => ("profile expires today".into(), t.signal),
        Expiry::Soon(1) => ("profile expires tomorrow".into(), t.signal),
        Expiry::Soon(d) => (format!("profile expires in {d} days"), t.signal),
        Expiry::Ok(d) => (format!("profile good for {d} days"), t.good),
    }
}

fn short_expiry(exp: &str) -> String {
    match sl::expiry(exp) {
        Expiry::Unknown => "—".into(),
        Expiry::Expired => "expired".into(),
        Expiry::Soon(d) => format!("expires in {d}d"),
        Expiry::Ok(d) => format!("{d} days left"),
    }
}

/// "in 1h 52m" / "8m ago" for a share's expiry.
fn relative(s: &str) -> String {
    let Some(n) = sl::seconds_until(s) else { return if s.is_empty() { "never".into() } else { s.into() } };
    let a = n.unsigned_abs();
    let span = if a < 60 {
        format!("{a}s")
    } else if a < 3600 {
        format!("{}m", a / 60)
    } else if a < 86400 {
        format!("{}h {}m", a / 3600, (a % 3600) / 60)
    } else {
        format!("{}d", a / 86400)
    };
    if n >= 0 {
        format!("in {span}")
    } else {
        format!("{span} ago")
    }
}

fn build_label(b: &Build) -> String {
    if b.build_number.is_empty() {
        b.version.clone()
    } else {
        format!("{} ({})", b.version, b.build_number)
    }
}

impl SideloadWs {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        let search = cx.new(|cx| TextField::new(w, cx, "", "Filter apps  ⌘F"));
        cx.subscribe_in(&search, w, |this: &mut Self, _, e: &FieldEvent, _w, cx| match e {
            FieldEvent::Changed => cx.notify(),
            FieldEvent::Down => this.step(1, cx),
            FieldEvent::Up => this.step(-1, cx),
            _ => {}
        })
        .detach();
        let notes = cx.new(|cx| TextField::new(w, cx, "Notes for the next upload", "optional — what changed"));
        let label = cx.new(|cx| TextField::new(w, cx, "Label", "who it's for (optional)"));
        cx.subscribe_in(&label, w, |this: &mut Self, _, e: &FieldEvent, _w, cx| {
            if let FieldEvent::Submit = e {
                this.mint(cx);
            }
        })
        .detach();
        let mut this = SideloadWs {
            view: View::Apps,
            builds: Vec::new(),
            shares: Vec::new(),
            shares_error: None,
            selected_app: None,
            selected_build: None,
            selected_share: None,
            loading: false,
            loaded: false,
            freshness: None,
            error: None,
            latest: Arc::new(Latest::default()),
            search,
            notes,
            label,
            ttl: 1,
            max: 1,
            minted: None,
            minting: false,
            uploads: Vec::new(),
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
        let task = farfield_core::spawn(async move {
            let b = sideload::builds(&s).await;
            let sh = sideload::shares(&s).await;
            (b, sh)
        });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                if !latest.is_current(ticket) {
                    return;
                }
                this.loading = false;
                let Ok((b, sh)) = r else { return };
                match b {
                    Ok(l) => {
                        this.builds = l.value;
                        this.error = None;
                        this.loaded = true;
                        set_health(
                            cx,
                            "sideload",
                            match &l.freshness {
                                Freshness::Live => Health::Up,
                                Freshness::Stale { error, .. } => Health::Down(error.to_string()),
                            },
                        );
                        this.freshness = Some(l.freshness);
                        let apps = sideload::apps(&this.builds);
                        if this.selected_app.as_ref().is_none_or(|a| !apps.iter().any(|(b, _)| b == a)) {
                            this.selected_app = apps.first().map(|(b, _)| b.clone());
                            this.selected_build = apps.first().map(|(_, v)| v[0].id.clone());
                        }
                        if this.selected_build.as_ref().is_some_and(|id| !this.builds.iter().any(|b| &b.id == id)) {
                            this.selected_build = None;
                        }
                        log("sideload-loaded", &[("builds", &this.builds.len().to_string())]);
                    }
                    Err(e) => {
                        if e.is_auth() {
                            set_health(cx, "sideload", Health::NoAuth);
                        } else if e.is_offline() {
                            set_health(cx, "sideload", Health::Down(e.to_string()));
                        }
                        this.error = Some(describe(&e));
                    }
                }
                match sh {
                    Ok(l) => {
                        this.shares = l.value;
                        this.shares_error = None;
                    }
                    Err(e) => this.shares_error = Some(describe(&e)),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn apps(&self, cx: &App) -> Vec<(String, Vec<Build>)> {
        let q = self.search.read(cx).text().to_lowercase();
        sideload::apps(&self.builds)
            .into_iter()
            .filter(|(bundle, v)| {
                q.is_empty() || bundle.to_lowercase().contains(&q) || v[0].app_name.to_lowercase().contains(&q)
            })
            .collect()
    }

    fn filtered_shares(&self, cx: &App) -> Vec<Share> {
        let q = self.search.read(cx).text().to_lowercase();
        self.shares
            .iter()
            .filter(|s| {
                q.is_empty()
                    || s.app_name.to_lowercase().contains(&q)
                    || s.label.to_lowercase().contains(&q)
                    || s.state.contains(&q)
            })
            .cloned()
            .collect()
    }

    fn build(&self) -> Option<&Build> {
        let id = self.selected_build.as_ref()?;
        self.builds.iter().find(|b| &b.id == id)
    }

    fn share(&self) -> Option<&Share> {
        let t = self.selected_share.as_ref()?;
        self.shares.iter().find(|s| &s.token == t)
    }

    fn select_app(&mut self, bundle: String, cx: &mut Context<Self>) {
        let newest = sideload::apps(&self.builds).into_iter().find(|(b, _)| *b == bundle).map(|(_, v)| v[0].id.clone());
        self.selected_app = Some(bundle);
        self.selected_build = newest;
        self.minted = None;
        cx.notify();
    }

    fn select_build(&mut self, id: String, cx: &mut Context<Self>) {
        if self.selected_build.as_deref() != Some(&id) {
            self.minted = None;
        }
        self.selected_build = Some(id);
        cx.notify();
    }

    fn step(&mut self, by: i32, cx: &mut Context<Self>) {
        match self.view {
            View::Apps => {
                let apps = self.apps(cx);
                if apps.is_empty() {
                    return;
                }
                let cur = self.selected_app.as_ref().and_then(|a| apps.iter().position(|(b, _)| b == a));
                let next = cur.map(|i| (i as i32 + by).clamp(0, apps.len() as i32 - 1) as usize).unwrap_or(0);
                self.select_app(apps[next].0.clone(), cx);
            }
            View::Shares => {
                let sh = self.filtered_shares(cx);
                if sh.is_empty() {
                    return;
                }
                let cur = self.selected_share.as_ref().and_then(|t| sh.iter().position(|s| &s.token == t));
                let next = cur.map(|i| (i as i32 + by).clamp(0, sh.len() as i32 - 1) as usize).unwrap_or(0);
                self.selected_share = Some(sh[next].token.clone());
                cx.notify();
            }
        }
    }

    fn set_view(&mut self, v: View, cx: &mut Context<Self>) {
        self.view = v;
        if v == View::Shares && self.selected_share.is_none() {
            self.selected_share = self.shares.first().map(|s| s.token.clone());
        }
        cx.notify();
    }

    // ── uploads ──

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
        let notes = self.notes.read(cx).text().trim().to_string();
        for p in paths {
            let name = p.file_name().map(|n| n.to_string_lossy().to_string()).unwrap_or_default();
            if !p.extension().and_then(|e| e.to_str()).is_some_and(|e| e.eq_ignore_ascii_case("ipa")) {
                toast(cx, format!("{name} isn't an .ipa — sideload takes iOS app archives."), true);
                continue;
            }
            if self.uploads.iter().any(|u| u.name == name) {
                continue;
            }
            let progress = Progress::new(std::fs::metadata(&p).map(|m| m.len()).unwrap_or(0));
            self.uploads.push(Up { name: name.clone(), progress: progress.clone() });
            let s = app::session(cx);
            let (pr, n) = (progress.clone(), notes.clone());
            let task = farfield_core::spawn(async move { sideload::upload(&s, &p, &n, &pr).await });
            log("sideload-upload-start", &[("file", &name)]);
            let pr2 = progress.clone();
            cx.spawn(async move |this, cx| loop {
                cx.background_executor().timer(Duration::from_millis(150)).await;
                let done = pr2.is_cancelled() || (pr2.total() > 0 && pr2.sent() >= pr2.total());
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
                        Ok(Ok(b)) => {
                            let b = b.value;
                            log("sideload-upload-done", &[("file", &name), ("id", &b.id)]);
                            toast(cx, format!("{} {} is up.", b.app_name, build_label(&b)), false);
                            this.notes.update(cx, |f, cx| f.set_text("", cx));
                            this.selected_app = Some(b.bundle_id.clone());
                            this.selected_build = Some(b.id.clone());
                            this.view = View::Apps;
                            this.reload(cx);
                        }
                        Ok(Err(ApiError::Cancelled)) => toast(cx, format!("Upload of {name} cancelled."), false),
                        Ok(Err(ApiError::Uncertain(_))) => {
                            // idempotent by content: a reload shows whether it landed
                            toast(cx, format!("{name}: the connection dropped as it finished. Uploading it again is safe — the same IPA is the same build."), true);
                            this.reload(cx);
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

    // ── links ──

    fn copy(&self, s: String, what: &str, cx: &mut App) {
        cx.write_to_clipboard(gpui::ClipboardItem::new_string(s));
        toast(cx, format!("{what} copied."), false);
    }

    fn copy_install(&mut self, cx: &mut Context<Self>) {
        let Some(b) = self.build().cloned() else { return };
        match sideload::install_url(&app::session(cx), &b) {
            Some(u) => {
                log("sideload-copy-install", &[("id", &b.id)]);
                self.copy(u, "Install link", cx)
            }
            None => toast(cx, "This profile has no public address for sideload, so there's no link to share.", true),
        }
    }

    fn mint(&mut self, cx: &mut Context<Self>) {
        let Some(b) = self.build().cloned() else { return };
        if self.minting {
            return;
        }
        self.minting = true;
        let s = app::session(cx);
        let (ttl, max, label) = (TTLS[self.ttl].0, MAXES[self.max].0, self.label.read(cx).text().trim().to_string());
        let id = b.id.clone();
        let task = farfield_core::spawn(async move { sideload::share(&s, &id, ttl, max, &label).await });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                this.minting = false;
                match r {
                    Ok(Ok(sh)) => {
                        log("sideload-share", &[("build", &b.id), ("ttl", ttl), ("max", max)]);
                        let url = sl::share_url(&app::session(cx), &sh.value);
                        cx.write_to_clipboard(gpui::ClipboardItem::new_string(url));
                        toast(cx, "Share link minted and copied.", false);
                        this.label.update(cx, |f, cx| f.set_text("", cx));
                        this.minted = Some(sh.value);
                        this.reload(cx);
                    }
                    Ok(Err(e)) => toast(cx, describe(&e), true),
                    Err(e) => toast(cx, e.to_string(), true),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn revoke(&mut self, token: String, cx: &mut Context<Self>) {
        let Some(sh) = self.shares.iter().find(|s| s.token == token).cloned() else { return };
        let ent = cx.entity();
        let who = if sh.label.is_empty() { String::new() } else { format!(" (“{}”)", sh.label) };
        confirm(
            cx,
            "Revoke this share link?",
            format!("The link for {} {}{who} stops working now — nobody can start a new install from it. Installs already on devices stay.", sh.app_name, sh.version),
            "Revoke",
            true,
            move |_, cx| {
                let s = app::session(cx);
                let tok = token.clone();
                let task = farfield_core::spawn(async move { sideload::revoke_share(&s, &tok).await });
                ent.update(cx, |_, cx| {
                    cx.spawn(async move |this, cx| {
                        let r = task.await;
                        let _ = this.update(cx, |this, cx| match r {
                            Ok(Ok(())) => {
                                log("sideload-revoke", &[("share", &token)]);
                                toast(cx, "Share link revoked.", false);
                                if this.minted.as_ref().is_some_and(|m| m.token == token) {
                                    this.minted = None;
                                }
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

    fn delete_build(&mut self, cx: &mut Context<Self>) {
        let Some(b) = self.build().cloned() else { return };
        let ent = cx.entity();
        confirm(
            cx,
            "Delete this build?",
            format!(
                "{} {} and its IPA are removed. Its install page and every share link for it stop working.",
                b.app_name,
                build_label(&b)
            ),
            "Delete build",
            true,
            move |_, cx| {
                let s = app::session(cx);
                let id = b.id.clone();
                let task = farfield_core::spawn(async move { sideload::delete_build(&s, &id).await });
                ent.update(cx, |_, cx| {
                    cx.spawn(async move |this, cx| {
                        let r = task.await;
                        let _ = this.update(cx, |this, cx| match r {
                            Ok(Ok(())) | Ok(Err(ApiError::NotFound)) => {
                                log("sideload-delete-build", &[("id", &b.id)]);
                                toast(cx, format!("Deleted {} {}.", b.app_name, build_label(&b)), false);
                                this.selected_build = None;
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

    fn delete_app(&mut self, cx: &mut Context<Self>) {
        let Some(bundle) = self.selected_app.clone() else { return };
        let builds: Vec<Build> = self.builds.iter().filter(|b| b.bundle_id == bundle).cloned().collect();
        let Some(first) = builds.first() else { return };
        let name = first.app_name.clone();
        let n = builds.len();
        let ent = cx.entity();
        confirm(
            cx,
            format!("Delete {name} entirely?"),
            format!(
                "Every build of {name} ({bundle}) — {n} {} — is permanently deleted, with its IPAs, screenshots, registered devices and every share link. Testers can no longer install it. There is no undo.",
                if n == 1 { "build" } else { "builds" }
            ),
            format!("Delete {name} and all {n} builds"),
            true,
            move |_, cx| {
                let s = app::session(cx);
                let b = bundle.clone();
                let task = farfield_core::spawn(async move { sideload::delete_app(&s, &b).await });
                ent.update(cx, |_, cx| {
                    cx.spawn(async move |this, cx| {
                        let r = task.await;
                        let _ = this.update(cx, |this, cx| match r {
                            Ok(Ok(())) | Ok(Err(ApiError::NotFound)) => {
                                log("sideload-delete-app", &[("bundle", &bundle)]);
                                toast(cx, format!("Deleted {name}."), false);
                                this.selected_app = None;
                                this.selected_build = None;
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

    // ── rendering ──

    fn render_side(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
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
                .child(label)
        };
        let active = self.shares.iter().filter(|s| s.state == "active").count();
        let mut col = div().flex().flex_col().size_full().child(
            div()
                .flex()
                .flex_col()
                .gap(S3)
                .px(S4)
                .pt(S4)
                .pb(S3)
                .border_b_1()
                .border_color(t.rule)
                .child(
                    div()
                        .flex()
                        .gap(S2)
                        .child(
                            tab("v-apps", "Apps".into(), self.view == View::Apps)
                                .on_click(cx.listener(|this, _, _, cx| this.set_view(View::Apps, cx))),
                        )
                        .child(
                            tab(
                                "v-shares",
                                if active > 0 { format!("Share links · {active}") } else { "Share links".into() },
                                self.view == View::Shares,
                            )
                            .on_click(cx.listener(|this, _, _, cx| this.set_view(View::Shares, cx))),
                        ),
                )
                .child(self.search.clone()),
        );
        match self.view {
            View::Apps => {
                let apps = self.apps(cx);
                if apps.is_empty() && self.loaded {
                    col = col.child(ui::quiet_state(
                        if self.builds.is_empty() { "No builds yet." } else { "No app matches." },
                        cx,
                    ));
                }
                let mut list = div().id("apps").flex().flex_col().flex_1().overflow_y_scroll();
                for (bundle, builds) in apps {
                    let newest = &builds[0];
                    let on = self.selected_app.as_deref() == Some(bundle.as_str());
                    let (exp, color) = expiry_chip(&newest.profile_expiry, &t);
                    let flagged = matches!(sl::expiry(&newest.profile_expiry), Expiry::Expired | Expiry::Soon(_));
                    let b = bundle.clone();
                    list = list.child(
                        ui::list_row(SharedString::from(format!("app-{bundle}")), on, &t)
                            .flex()
                            .flex_col()
                            .gap(px(2.))
                            .child(
                                div()
                                    .flex()
                                    .justify_between()
                                    .gap(S2)
                                    .child(div().text_sm().text_color(t.ink).truncate().child(newest.app_name.clone()))
                                    .child(
                                        div()
                                            .font_family(FONT_MONO)
                                            .text_xs()
                                            .text_color(t.ink_3)
                                            .child(build_label(newest)),
                                    ),
                            )
                            .child(
                                div()
                                    .font_family(FONT_MONO)
                                    .text_xs()
                                    .text_color(t.ink_3)
                                    .truncate()
                                    .child(bundle.clone()),
                            )
                            .child(
                                div()
                                    .flex()
                                    .justify_between()
                                    .text_xs()
                                    .text_color(t.ink_3)
                                    .child(format!(
                                        "{} {}",
                                        builds.len(),
                                        if builds.len() == 1 { "build" } else { "builds" }
                                    ))
                                    .when(flagged, |d| d.child(ui::chip(exp, color, cx))),
                            )
                            .on_click(cx.listener(move |this, _, _, cx| this.select_app(b.clone(), cx))),
                    );
                }
                col = col.child(list);
            }
            View::Shares => {
                let shares = self.filtered_shares(cx);
                if shares.is_empty() && self.loaded {
                    col = col.child(ui::quiet_state(
                        "No share links yet. Pick a build and mint one from the inspector.",
                        cx,
                    ));
                }
                let mut list = div().id("shares").flex().flex_col().flex_1().overflow_y_scroll();
                for s in shares {
                    let on = self.selected_share.as_deref() == Some(s.token.as_str());
                    let tok = s.token.clone();
                    list = list.child(
                        ui::list_row(SharedString::from(format!("sh-{}", s.token)), on, &t)
                            .flex()
                            .flex_col()
                            .gap(px(2.))
                            .child(
                                div()
                                    .flex()
                                    .justify_between()
                                    .gap(S2)
                                    .child(div().text_sm().text_color(t.ink).truncate().child(if s.label.is_empty() {
                                        format!("{} {}", s.app_name, s.version)
                                    } else {
                                        s.label.clone()
                                    }))
                                    .child(state_chip(&s, &t, cx)),
                            )
                            .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(format!(
                                "{} {} · {} · {}",
                                s.app_name,
                                s.version,
                                installs(&s),
                                if s.state == "active" {
                                    format!("expires {}", relative(&s.expires_at))
                                } else {
                                    relative(&s.expires_at)
                                }
                            )))
                            .on_click(cx.listener(move |this, _, _, cx| {
                                this.selected_share = Some(tok.clone());
                                cx.notify()
                            })),
                    );
                }
                col = col.child(list);
            }
        }
        col.into_any_element()
    }

    fn render_uploads(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let mut d = div().flex().flex_col().gap(S2).px(S5).py(S3).border_b_1().border_color(t.rule);
        if self.uploads.is_empty() {
            d = d.child(div().flex().items_end().gap(S4).child(div().flex_1().child(self.notes.clone())).child(
                ui::button(
                    "upload",
                    "Upload IPA…  ⌘N",
                    BtnKind::Primary,
                    cx,
                    cx.listener(|this, _, w, cx| this.pick(w, cx)),
                ),
            ));
            d =
                d.child(div().text_xs().text_color(t.ink_3).child(
                    "Or drop .ipa files here. The same IPA twice is the same build — re-uploading is always safe.",
                ));
        }
        for (i, u) in self.uploads.iter().enumerate() {
            let p = u.progress.clone();
            let frac = p.fraction();
            let status = if p.total() > 0 && p.sent() >= p.total() {
                "Reading the archive…".to_string()
            } else {
                format!("{} of {}", ui::bytes(p.sent() as i64), ui::bytes(p.total() as i64))
            };
            d = d.child(
                div()
                    .flex()
                    .items_center()
                    .gap(S4)
                    .child(
                        div()
                            .flex_1()
                            .flex()
                            .flex_col()
                            .gap(px(5.))
                            .child(
                                div()
                                    .flex()
                                    .justify_between()
                                    .child(div().text_sm().text_color(t.ink).child(u.name.clone()))
                                    .child(
                                        div()
                                            .font_family(FONT_MONO)
                                            .text_xs()
                                            .text_color(t.ink_3)
                                            .child(format!("{}%", (frac * 100.) as u32)),
                                    ),
                            )
                            .child(
                                div()
                                    .w_full()
                                    .h(px(2.))
                                    .bg(t.rule)
                                    .child(div().h_full().w(gpui::relative(frac)).bg(t.accent)),
                            )
                            .child(div().text_xs().text_color(t.ink_2).child(status)),
                    )
                    .child(ui::button(
                        SharedString::from(format!("cancel-{i}")),
                        "Cancel",
                        BtnKind::Quiet,
                        cx,
                        move |_, _, _| p.cancel(),
                    )),
            );
        }
        d.into_any_element()
    }

    fn render_app(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let Some(bundle) = self.selected_app.clone() else {
            let msg = if self.loading && !self.loaded {
                "Loading builds…"
            } else if self.error.is_some() {
                ""
            } else {
                "No builds yet. Drop an .ipa here, or press ⌘N to choose one — it's parsed for its bundle, version and provisioning profile."
            };
            return div().max_w(px(560.)).child(ui::quiet_state(msg, cx)).into_any_element();
        };
        let builds: Vec<Build> =
            sideload::apps(&self.builds).into_iter().find(|(b, _)| *b == bundle).map(|(_, v)| v).unwrap_or_default();
        let Some(newest) = builds.first() else { return div().into_any_element() };
        let total: i64 = builds.iter().map(|b| b.size_bytes).sum();
        let mut d = div().flex().flex_col().px(S5).pt(S4).gap(S1);
        d = d.child(
            div()
                .flex()
                .items_end()
                .justify_between()
                .child(
                    div()
                        .flex()
                        .flex_col()
                        .gap(px(2.))
                        .child(
                            div()
                                .text_size(px(22.))
                                .font_weight(gpui::FontWeight::MEDIUM)
                                .text_color(t.ink)
                                .child(newest.app_name.clone()),
                        )
                        .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(format!(
                            "{bundle} · {} {} · {}",
                            builds.len(),
                            if builds.len() == 1 { "build" } else { "builds" },
                            ui::bytes(total)
                        ))),
                )
                .child(ui::button(
                    "delete-app",
                    "Delete app…",
                    BtnKind::Danger,
                    cx,
                    cx.listener(|this, _, _, cx| this.delete_app(cx)),
                )),
        );
        // builds table
        let head = |s: &'static str| div().text_xs().text_color(t.ink_3).child(s);
        // version + commit/notes take the slack; the readouts keep tidy widths
        let cols = |a: AnyElement, b: AnyElement, c: AnyElement, dd: AnyElement, e: AnyElement| {
            div()
                .flex()
                .items_center()
                .gap(S3)
                .child(div().flex_1().min_w(px(84.)).overflow_hidden().child(a))
                .child(div().w(px(92.)).flex_none().child(b))
                .child(div().w(px(96.)).flex_none().child(c))
                .child(div().w(px(48.)).flex_none().flex().justify_end().child(dd))
                .child(div().w(px(60.)).flex_none().flex().justify_end().child(e))
        };
        d = d.child(div().pt(S4).pb(S1).child(ui::eyebrow("Builds — newest first", cx)));
        d = d.child(div().py(px(4.)).border_b_1().border_color(t.rule).child(cols(
            head("Version").into_any_element(),
            head("Uploaded").into_any_element(),
            head("Profile").into_any_element(),
            head("Devices").into_any_element(),
            head("Size").into_any_element(),
        )));
        for b in &builds {
            let on = self.selected_build.as_deref() == Some(b.id.as_str());
            let (exp, color) = (short_expiry(&b.profile_expiry), t.signal);
            let flagged = matches!(sl::expiry(&b.profile_expiry), Expiry::Expired | Expiry::Soon(_));
            let sub = [b.git_commit.chars().take(7).collect::<String>(), b.notes.clone()]
                .into_iter()
                .filter(|s| !s.is_empty())
                .collect::<Vec<_>>()
                .join(" · ");
            let id = b.id.clone();
            let mono = |s: String, c: Hsla| div().font_family(FONT_MONO).text_xs().text_color(c).child(s);
            let wash = t.wash;
            d = d.child(
                div()
                    .id(SharedString::from(format!("b-{}", b.id)))
                    .py(px(8.))
                    .px(px(6.))
                    .mx(px(-6.))
                    .border_b_1()
                    .border_color(t.rule)
                    .cursor_pointer()
                    .when(on, |d| d.bg(t.accent_soft))
                    .when(!on, move |d| d.hover(move |s| s.bg(wash)))
                    .child(cols(
                        div()
                            .flex()
                            .flex_col()
                            .child(div().text_sm().text_color(if on { t.accent } else { t.ink }).child(build_label(b)))
                            .when(!sub.is_empty(), |d| {
                                d.child(div().text_xs().text_color(t.ink_3).truncate().child(sub.clone()))
                            })
                            .into_any_element(),
                        mono(ui::when(&b.created_at).get(5..).unwrap_or("").to_string(), t.ink_2).into_any_element(),
                        if flagged {
                            div().text_xs().text_color(color).child(exp).into_any_element()
                        } else {
                            mono(short_expiry(&b.profile_expiry), t.ink_2).into_any_element()
                        },
                        mono(b.device_count.to_string(), t.ink_2).into_any_element(),
                        mono(ui::bytes(b.size_bytes), t.ink_2).into_any_element(),
                    ))
                    .on_click(cx.listener(move |this, _, _, cx| this.select_build(id.clone(), cx))),
            );
        }
        // share links for this app
        let ids: Vec<&str> = builds.iter().map(|b| b.id.as_str()).collect();
        let shares: Vec<&Share> = self.shares.iter().filter(|s| ids.contains(&s.build_id.as_str())).collect();
        d = d.child(div().pt(S5).pb(S1).child(ui::eyebrow("Share links for this app", cx)));
        if shares.is_empty() {
            d = d.child(
                div()
                    .text_sm()
                    .text_color(t.ink_3)
                    .py(S2)
                    .child("None yet. Mint one for a build from the inspector — it expires on its own."),
            );
        }
        for s in shares {
            let tok = s.token.clone();
            let tok2 = s.token.clone();
            let url = sl::share_url(&app::session(cx), s);
            d = d.child(
                div()
                    .flex()
                    .items_center()
                    .gap(S3)
                    .py(px(6.))
                    .border_b_1()
                    .border_color(t.rule)
                    .child(div().w(px(120.)).text_sm().text_color(t.ink).child(s.version.clone()))
                    .child(
                        div()
                            .flex_1()
                            .min_w_0()
                            .text_sm()
                            .text_color(t.ink_2)
                            .truncate()
                            .child(if s.label.is_empty() { "—".to_string() } else { s.label.clone() }),
                    )
                    .child(div().w(px(110.)).child(state_chip(s, &t, cx)))
                    .child(div().w(px(70.)).font_family(FONT_MONO).text_xs().text_color(t.ink_2).child(installs(s)))
                    .child(
                        div()
                            .w(px(110.))
                            .font_family(FONT_MONO)
                            .text_xs()
                            .text_color(t.ink_3)
                            .child(relative(&s.expires_at)),
                    )
                    .child(ui::button(
                        SharedString::from(format!("cp-{tok}")),
                        "Copy",
                        BtnKind::Quiet,
                        cx,
                        move |_, _, cx| {
                            cx.write_to_clipboard(gpui::ClipboardItem::new_string(url.clone()));
                            toast(cx, "Share link copied.", false);
                        },
                    ))
                    .when(s.state == "active", |d| {
                        d.child(ui::button(
                            SharedString::from(format!("rv-{tok}")),
                            "Revoke…",
                            BtnKind::Danger,
                            cx,
                            cx.listener(move |this, _, _, cx| this.revoke(tok2.clone(), cx)),
                        ))
                    }),
            );
        }
        d.into_any_element()
    }

    fn inspector_build(&self, b: &Build, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let mut col = div().flex().flex_col().gap(S2);
        col = col.child(ui::eyebrow("Build", cx)).child(
            div().text_size(px(20.)).font_weight(gpui::FontWeight::MEDIUM).text_color(t.ink).child(format!(
                "{} {}",
                b.app_name,
                build_label(b)
            )),
        );
        let (exp, color) = expiry_chip(&b.profile_expiry, &t);
        col = col.child(ui::chip(exp, color, cx));
        if !b.notes.is_empty() {
            col = col.child(div().text_sm().text_color(t.ink_2).child(b.notes.clone()));
        }
        // install link
        col = col.child(ui::rule(cx)).child(ui::eyebrow("Install page", cx));
        match sideload::install_url(&app::session(cx), b) {
            Some(u) => {
                col = col.child(ui::mono(u, cx)).child(div().flex().child(ui::button(
                    "copy-install",
                    "Copy install link",
                    BtnKind::Quiet,
                    cx,
                    cx.listener(|this, _, _, cx| this.copy_install(cx)),
                )));
                col = col.child(
                    div()
                        .text_xs()
                        .text_color(t.ink_3)
                        .child("The canonical page — it asks you to sign in. Testers get a share link instead."),
                );
            }
            None => {
                col = col
                    .child(div().text_xs().text_color(t.ink_3).child("No public address for sideload in this profile."))
            }
        }
        // mint
        col = col.child(ui::rule(cx)).child(ui::eyebrow("Share with a tester", cx));
        let seg = |prefix: &'static str,
                   opts: &[(&'static str, &'static str)],
                   cur: usize,
                   cx: &mut Context<Self>,
                   set: fn(&mut Self, usize)| {
            let mut row = div().flex().gap(px(4.));
            for (i, (_, label)) in opts.iter().enumerate() {
                let on = i == cur;
                row = row.child(
                    div()
                        .id(SharedString::from(format!("{prefix}-{i}")))
                        .px(px(9.))
                        .py(px(3.))
                        .rounded(px(10.))
                        .text_xs()
                        .cursor_pointer()
                        .border_1()
                        .border_color(if on { t.accent } else { t.rule_strong })
                        .text_color(if on { t.accent } else { t.ink_2 })
                        .when(on, |d| d.bg(t.accent_soft))
                        .child(*label)
                        .on_click(cx.listener(move |this, _, _, cx| {
                            set(this, i);
                            cx.notify()
                        })),
                );
            }
            row
        };
        col = col
            .child(div().text_xs().text_color(t.ink_2).child("Expires after"))
            .child(seg("ttl", &TTLS, self.ttl, cx, |this, i| this.ttl = i))
            .child(div().text_xs().text_color(t.ink_2).pt(px(4.)).child("Installs allowed"))
            .child(seg("max", &MAXES, self.max, cx, |this, i| this.max = i))
            .child(div().pt(px(4.)).child(self.label.clone()))
            .child(div().flex().pt(px(4.)).child(if self.minting {
                ui::button_disabled("Minting…", cx).into_any_element()
            } else {
                ui::button("mint", "Mint share link", BtnKind::Primary, cx, cx.listener(|this, _, _, cx| this.mint(cx)))
                    .into_any_element()
            }));
        if let Some(m) = self.minted.as_ref().filter(|m| m.build_id.is_empty() || m.build_id == b.id) {
            let url = sl::share_url(&app::session(cx), m);
            let u2 = url.clone();
            col = col.child(
                div()
                    .flex()
                    .flex_col()
                    .gap(px(4.))
                    .pt(S2)
                    .child(
                        div()
                            .border_l_2()
                            .border_color(t.good)
                            .pl(px(10.))
                            .flex()
                            .flex_col()
                            .gap(px(3.))
                            .child(ui::mono(url, cx).text_color(t.ink))
                            .child(div().text_xs().text_color(t.ink_2).child(format!(
                                "Expires {} · {}",
                                relative(&m.expires_at),
                                if m.max_installs == 0 {
                                    "unlimited installs".to_string()
                                } else {
                                    format!(
                                        "{} {}",
                                        m.max_installs,
                                        if m.max_installs == 1 { "install" } else { "installs" }
                                    )
                                }
                            ))),
                    )
                    .child(div().flex().child(ui::button(
                        "copy-minted",
                        "Copy link",
                        BtnKind::Quiet,
                        cx,
                        move |_, _, cx| {
                            cx.write_to_clipboard(gpui::ClipboardItem::new_string(u2.clone()));
                            toast(cx, "Share link copied.", false);
                        },
                    ))),
            );
        }
        // facts
        col = col.child(ui::rule(cx));
        let facts: Vec<(&str, String)> = vec![
            ("Bundle", b.bundle_id.clone()),
            ("Team", b.team.clone()),
            ("Profile expiry", ui::when(&b.profile_expiry)),
            ("Devices in profile", b.device_count.to_string()),
            ("Size", ui::bytes(b.size_bytes)),
            ("Filename", b.filename.clone()),
            ("Commit", b.git_commit.clone()),
            ("Uploaded", ui::when(&b.created_at)),
            ("CID", b.cid.clone()),
        ];
        for (k, v) in facts.into_iter().filter(|(_, v)| !v.is_empty()) {
            col = col.child(ui::field_row(k, ui::mono(v, cx), cx));
        }
        col = col.child(div().flex().pt(S2).child(ui::button(
            "delete-build",
            "Delete build…",
            BtnKind::Danger,
            cx,
            cx.listener(|this, _, _, cx| this.delete_build(cx)),
        )));
        col.into_any_element()
    }

    fn inspector_share(&self, s: &Share, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let url = sl::share_url(&app::session(cx), s);
        let u2 = url.clone();
        let mut col =
            div()
                .flex()
                .flex_col()
                .gap(S2)
                .child(ui::eyebrow("Share link", cx))
                .child(
                    div().text_size(px(20.)).font_weight(gpui::FontWeight::MEDIUM).text_color(t.ink).child(
                        if s.label.is_empty() { format!("{} {}", s.app_name, s.version) } else { s.label.clone() },
                    ),
                )
                .child(state_chip(s, &t, cx))
                .child(ui::mono(url, cx).text_color(t.ink))
                .child(div().flex().gap(S2).child(ui::button(
                    "copy-share",
                    "Copy link",
                    BtnKind::Quiet,
                    cx,
                    move |_, _, cx| {
                        cx.write_to_clipboard(gpui::ClipboardItem::new_string(u2.clone()));
                        toast(cx, "Share link copied.", false);
                    },
                )))
                .child(ui::rule(cx));
        let facts: Vec<(&str, String)> = vec![
            ("Build", format!("{} {}", s.app_name, s.version)),
            ("Installs", installs(s)),
            ("Expires", format!("{} ({})", ui::when(&s.expires_at), relative(&s.expires_at))),
            ("Created", ui::when(&s.created_at)),
            ("Consumed", ui::when(&s.consumed_at)),
        ];
        for (k, v) in facts.into_iter().filter(|(_, v)| !v.is_empty()) {
            col = col.child(ui::field_row(k, ui::mono(v, cx), cx));
        }
        if s.state == "active" {
            let tok = s.token.clone();
            col = col.child(div().flex().pt(S2).child(ui::button(
                "revoke",
                "Revoke…",
                BtnKind::Danger,
                cx,
                cx.listener(move |this, _, _, cx| this.revoke(tok.clone(), cx)),
            )));
        }
        col.into_any_element()
    }
}

fn installs(s: &Share) -> String {
    if s.max_installs == 0 {
        format!("{}/∞", s.installs)
    } else {
        format!("{}/{}", s.installs, s.max_installs)
    }
}

fn state_chip(s: &Share, t: &Theme, cx: &App) -> gpui::Div {
    let expired = s.state == "active" && sl::seconds_until(&s.expires_at).is_some_and(|n| n < 0);
    let (word, c) = match s.state.as_str() {
        _ if expired => ("expired", t.ink_3),
        "active" if s.live => ("active", t.good),
        "active" => ("used up", t.ink_3),
        "consumed" => ("consumed", t.ink_3),
        "revoked" => ("revoked", t.bad),
        other => (other, t.ink_3),
    };
    ui::chip(word.to_string(), c, cx)
}

impl Workspace for SideloadWs {
    fn inspector(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> Option<AnyElement> {
        match self.view {
            View::Apps => {
                let b = self.build()?.clone();
                Some(self.inspector_build(&b, cx))
            }
            View::Shares => {
                let s = self.share()?.clone();
                Some(self.inspector_share(&s, cx))
            }
        }
    }

    fn palette(&self, _cx: &App) -> Vec<PaletteItem> {
        vec![
            PaletteItem::new("Sideload: upload an IPA…", "⌘N", |_, cx| crate::shell::goto(cx, "sideload")),
            PaletteItem::new("Sideload: share links", "", |_, cx| crate::shell::goto(cx, "sideload")),
        ]
    }

    fn commands(&self, _cx: &App) -> Vec<(&'static str, String, &'static str)> {
        let mut v = vec![("upload", "Sideload: upload an IPA…".to_string(), "⌘N")];
        v.push(match self.view {
            View::Apps => ("view-shares", "Sideload: show share links".into(), ""),
            View::Shares => ("view-apps", "Sideload: show apps".into(), ""),
        });
        match self.view {
            View::Apps => {
                if self.build().is_some() {
                    v.push((
                        "mint",
                        format!("Sideload: mint share link ({}, {})", TTLS[self.ttl].1, MAXES[self.max].1),
                        "",
                    ));
                    v.push(("copy-install", "Sideload: copy install link".into(), ""));
                    v.push(("delete-build", "Sideload: delete this build…".into(), ""));
                }
                if self.selected_app.is_some() {
                    v.push(("delete-app", "Sideload: delete this app and all its builds…".into(), ""));
                }
            }
            View::Shares => {
                if let Some(s) = self.share() {
                    v.push(("copy-share", "Sideload: copy share link".into(), ""));
                    if s.state == "active" {
                        v.push(("revoke", "Sideload: revoke this share link…".into(), ""));
                    }
                }
            }
        }
        v
    }

    fn run_command(&mut self, id: &str, w: &mut Window, cx: &mut Context<Self>) {
        match id {
            "upload" => self.pick(w, cx),
            "view-shares" => self.set_view(View::Shares, cx),
            "view-apps" => self.set_view(View::Apps, cx),
            "mint" => self.mint(cx),
            "copy-install" => self.copy_install(cx),
            "delete-build" => self.delete_build(cx),
            "delete-app" => self.delete_app(cx),
            "copy-share" => {
                if let Some(s) = self.share() {
                    let u = sl::share_url(&app::session(cx), s);
                    self.copy(u, "Share link", cx);
                }
            }
            "revoke" => {
                if let Some(t) = self.selected_share.clone() {
                    self.revoke(t, cx)
                }
            }
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
        self.reload(cx)
    }
    fn dirty(&self, _cx: &App) -> bool {
        !self.uploads.is_empty()
    }
}

impl Render for SideloadWs {
    fn render(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = theme(cx).clone();
        let status_line = match (&self.error, &self.freshness) {
            (Some(e), _) => Some(ui::notice(e.clone(), t.bad, cx)),
            (None, Some(Freshness::Stale { age_ms, .. })) => Some(ui::notice(
                format!("Offline — showing builds as loaded {} ago.", crate::ws::content::ago(*age_ms)),
                t.warn,
                cx,
            )),
            _ => None,
        };
        let shares_note = self
            .shares_error
            .clone()
            .filter(|_| self.error.is_none())
            .map(|e| ui::notice(format!("Share links unavailable: {e}"), t.warn, cx));
        let wash = t.wash;
        let main: AnyElement = match self.view {
            View::Apps => self.render_app(cx),
            View::Shares => {
                let n = self.shares.len();
                let active = self.shares.iter().filter(|s| s.state == "active").count();
                div()
                    .px(S5)
                    .pt(S4)
                    .flex()
                    .flex_col()
                    .gap(S2)
                    .child(div().text_size(px(22.)).font_weight(gpui::FontWeight::MEDIUM).text_color(t.ink).child("Share links"))
                    .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(format!("{n} minted · {active} active")))
                    .child(div().max_w(px(560.)).text_sm().text_color(t.ink_2).pt(S2).child(
                        "Each link installs one build for a limited time and number of installs. Pick one on the left to copy or revoke it; mint new ones from a build's inspector.",
                    ))
                    .into_any_element()
            }
        };
        div()
            .id("sideload")
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
            .child(div().w(px(300.)).flex_none().h_full().border_r_1().border_color(t.rule).child(self.render_side(cx)))
            .child(
                div()
                    .id("sideload-main")
                    .flex_1()
                    .min_w_0()
                    .h_full()
                    .flex()
                    .flex_col()
                    .overflow_y_scroll()
                    .child(self.render_uploads(cx))
                    .when_some(status_line, |d, s| d.child(div().px(S5).py(S2).child(s)))
                    .when_some(shares_note, |d, s| d.child(div().px(S5).py(S2).child(s)))
                    .child(main)
                    .child(div().h(S5)),
            )
    }
}
