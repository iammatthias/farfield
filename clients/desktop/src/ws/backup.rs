//! Backup: the snapshot registry, observed. Health, then every snapshot
//! grouped by app (newest first, each app's total size), with the one you
//! pick in the inspector. Nothing here acts: taking, pruning and restoring
//! stay in the backup console, and backup has no public address at all —
//! the one service that can restore or destroy every database in the fleet
//! is reachable only over the tailnet, by design.

use crate::app::{self, describe, log, Health};
use crate::shell::{goto, set_health, toast};
use crate::theme::{theme, FONT_MONO, S1, S2, S3, S4, S5};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, Kind as BtnKind};
use crate::workspace::Workspace;
use farfield_core::api::ext_observe::backup::{self, Group, Snapshot};
use farfield_core::{ApiError, Freshness, Latest};
use gpui::{div, prelude::*, px, AnyElement, App, Context, Entity, ScrollHandle, Window};
use std::sync::Arc;

fn key_of(s: &Snapshot) -> String {
    // identical databases snapshot to the same CID: the CID alone is not unique
    format!("{}\n{}\n{}", s.app, s.cid, s.created_at)
}

pub struct BackupWs {
    snapshots: Vec<Snapshot>,
    loaded: bool,
    loading: bool,
    freshness: Option<Freshness>,
    error: Option<ApiError>,
    latest: Arc<Latest>,
    search: Entity<TextField>,
    selected: Option<String>,
    scroll: ScrollHandle,
}

impl BackupWs {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        let search = cx.new(|cx| TextField::new(w, cx, "", "Filter by app or CID  ⌘F"));
        cx.subscribe_in(&search, w, |this: &mut Self, _, e: &FieldEvent, _w, cx| match e {
            FieldEvent::Changed => cx.notify(),
            FieldEvent::Down => this.step(1, cx),
            FieldEvent::Up => this.step(-1, cx),
            _ => {}
        })
        .detach();
        let mut this = BackupWs {
            snapshots: Vec::new(),
            loaded: false,
            loading: false,
            freshness: None,
            error: None,
            latest: Arc::new(Latest::default()),
            search,
            selected: None,
            scroll: ScrollHandle::new(),
        };
        this.load(cx);
        this
    }

    fn load(&mut self, cx: &mut Context<Self>) {
        let ticket = self.latest.ticket();
        let latest = self.latest.clone();
        let s = app::session(cx);
        self.loading = true;
        cx.notify();
        let task = farfield_core::spawn(async move { backup::snapshots(&s).await });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                if !latest.is_current(ticket) {
                    return;
                }
                this.loading = false;
                match r {
                    Ok(Ok(l)) => {
                        set_health(
                            cx,
                            "backup",
                            match &l.freshness {
                                Freshness::Live => Health::Up,
                                Freshness::Stale { error, .. } => Health::Down(error.to_string()),
                            },
                        );
                        log("backup-snapshots", &[("count", &l.value.len().to_string())]);
                        this.snapshots = l.value;
                        this.freshness = Some(l.freshness);
                        this.error = None;
                        this.loaded = true;
                    }
                    Ok(Err(e)) => {
                        if e.is_auth() {
                            set_health(cx, "backup", Health::NoAuth);
                        } else if e.is_offline() {
                            set_health(cx, "backup", Health::Down(e.to_string()));
                        }
                        this.error = Some(e);
                    }
                    Err(e) => this.error = Some(ApiError::Decode(e.to_string())),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn groups(&self, cx: &App) -> Vec<Group> {
        let q = self.search.read(cx).text().to_lowercase();
        let list: Vec<Snapshot> = self
            .snapshots
            .iter()
            .filter(|s| q.is_empty() || s.app.to_lowercase().contains(&q) || s.cid.to_lowercase().contains(&q))
            .cloned()
            .collect();
        backup::group(&list)
    }

    fn step(&mut self, by: i32, cx: &mut Context<Self>) {
        let flat: Vec<String> =
            self.groups(cx).into_iter().flat_map(|g| g.snapshots.into_iter().map(|s| key_of(&s))).collect();
        if flat.is_empty() {
            return;
        }
        let cur = self.selected.as_ref().and_then(|k| flat.iter().position(|c| c == k));
        let next = match cur {
            None => 0,
            Some(i) => (i as i32 + by).clamp(0, flat.len() as i32 - 1) as usize,
        };
        self.selected = Some(flat[next].clone());
        cx.notify();
    }

    fn blocked(&self, e: &ApiError, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let (head, body) = match e {
            ApiError::Unauthorized(_) => (
                "Backup needs its key.",
                "The snapshot registry answers BACKUP_API_KEY only — minted keys don't open it. Add the key in Settings → Keys.".to_string(),
            ),
            ApiError::Unavailable(_) => (
                "Backup's read API is switched off.",
                "The server has no BACKUP_API_KEY configured, so it answers nothing here. Set one on the homelab to see snapshots in this app; the console works either way.".to_string(),
            ),
            ApiError::Offline(_) | ApiError::NotFound => (
                "Backup can't be reached.",
                "It has no public address — connect to your tailnet and ⌘R. Nothing else in the app depends on it.".to_string(),
            ),
            other => ("Backup didn't answer as expected.", describe(other)),
        };
        div()
            .flex()
            .flex_col()
            .gap(S3)
            .max_w(px(600.))
            .child(div().text_lg().text_color(t.ink).child(head))
            .child(div().text_sm().text_color(t.ink_2).child(body))
            .child(
                div()
                    .flex()
                    .gap(S2)
                    .when(e.is_auth(), |d| {
                        d.child(ui::button("conn", "Open Settings", BtnKind::Primary, cx, |_, _, cx| {
                            goto(cx, "connections")
                        }))
                    })
                    .child(ui::button("console", "Open the backup console", BtnKind::Quiet, cx, |_, _, cx| {
                        crate::ws::connections::open_console(cx, "backup")
                    })),
            )
            .into_any_element()
    }
}

impl Workspace for BackupWs {
    fn inspector(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> Option<AnyElement> {
        let t = theme(cx).clone();
        let k = self.selected.clone()?;
        let s = self.snapshots.iter().find(|s| key_of(s) == k)?.clone();
        let same: Vec<&Snapshot> = self.snapshots.iter().filter(|x| x.app == s.app).collect();
        let newer = same.iter().filter(|x| x.created_at > s.created_at).count();
        let cid = s.cid.clone();
        Some(
            div()
                .flex()
                .flex_col()
                .gap(S1)
                .child(ui::eyebrow("Snapshot", cx))
                .child(div().text_lg().text_color(t.ink).child(s.app.clone()))
                .child(ui::field_row("Taken", ui::mono(s.created_at.clone(), cx), cx))
                .child(ui::field_row("Size", ui::mono(format!("{}  ({} bytes)", ui::bytes(s.size), s.size), cx), cx))
                .child(ui::field_row("CID", ui::mono(s.cid.clone(), cx), cx))
                .child(ui::field_row(
                    "Place",
                    div().text_sm().text_color(t.ink).child(if newer == 0 {
                        format!("the newest of {}'s {} snapshots", s.app, same.len())
                    } else {
                        format!("{newer} newer of {} for {}", same.len(), s.app)
                    }),
                    cx,
                ))
                .child(div().pt(S2).child(ui::button("copy", "Copy CID", BtnKind::Quiet, cx, move |_, _, cx| {
                    cx.write_to_clipboard(gpui::ClipboardItem::new_string(cid.clone()));
                    toast(cx, "CID copied.", false);
                })))
                .child(
                    div()
                        .pt(S3)
                        .text_xs()
                        .text_color(t.ink_3)
                        .child("Restoring happens in the backup console, never from here."),
                )
                .into_any_element(),
        )
    }

    fn focus_search(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        self.search.read(cx).focus(w);
    }

    fn refresh(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        self.load(cx)
    }

    fn commands(&self, _cx: &App) -> Vec<(&'static str, String, &'static str)> {
        vec![
            ("refresh", "Backup: refresh snapshots".into(), "⌘R"),
            ("console", "Backup: open the console (browser)".into(), ""),
        ]
    }

    fn run_command(&mut self, id: &str, _w: &mut Window, cx: &mut Context<Self>) {
        match id {
            "refresh" => self.load(cx),
            "console" => crate::ws::connections::open_console(cx, "backup"),
            _ => {}
        }
    }
}

impl Render for BackupWs {
    fn render(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = theme(cx).clone();
        let mut col = div().flex().flex_col().gap(S4).px(px(40.)).py(S5).max_w(px(980.));
        col = col.child(
            div()
                .flex()
                .flex_col()
                .gap(S2)
                .child(div().text_xl().text_color(t.ink).child("Backup"))
                .child(div().text_sm().text_color(t.ink_2).max_w(px(640.)).child(
                    "Backup can restore or destroy every database in the fleet, so it has no public address: this view reads its snapshot registry over your tailnet and does nothing else. Taking, pruning and restoring snapshots stay in the backup console.",
                )),
        );
        if !self.loaded {
            col = col.child(match &self.error {
                Some(e) => self.blocked(&e.clone(), cx),
                None => ui::quiet_state("Reading the snapshot registry…", cx).into_any_element(),
            });
            return div().id("backup").size_full().overflow_y_scroll().track_scroll(&self.scroll).child(col);
        }
        if let Some(Freshness::Stale { age_ms, .. }) = &self.freshness {
            col = col.child(ui::notice(
                format!("Offline — showing the registry as it was {} ago.", crate::ws::content::ago(*age_ms)),
                t.warn,
                cx,
            ));
        } else if let Some(e) = &self.error {
            col = col.child(ui::notice(describe(e), t.bad, cx));
        }
        // health readouts
        let total: i64 = self.snapshots.iter().map(|s| s.size).sum();
        let newest = self.snapshots.iter().map(|s| s.created_at.clone()).max().unwrap_or_default();
        let apps = backup::group(&self.snapshots).len();
        let stat = |label: &str, v: String| {
            div()
                .flex()
                .flex_col()
                .gap(px(2.))
                .min_w(px(140.))
                .child(div().text_xs().text_color(t.ink_2).child(label.to_string()))
                .child(div().font_family(FONT_MONO).text_size(px(20.)).text_color(t.ink).child(v))
        };
        col = col.child(
            div()
                .flex()
                .gap(S5)
                .child(stat("Snapshots", self.snapshots.len().to_string()))
                .child(stat("Apps", apps.to_string()))
                .child(stat("Stored", ui::bytes(total)))
                .child(stat("Newest", if newest.is_empty() { "—".into() } else { ui::when(&newest) })),
        );
        col = col.child(div().w(px(340.)).child(self.search.clone()));
        let groups = self.groups(cx);
        if groups.is_empty() {
            col = col.child(ui::quiet_state(
                if self.snapshots.is_empty() {
                    "No snapshots yet — the scheduler takes the first within its interval, or take one in the console."
                } else {
                    "Nothing matches the filter."
                },
                cx,
            ));
        }
        let sel = self.selected.clone();
        for g in groups {
            col = col.child(
                div()
                    .pt(S3)
                    .pb(px(4.))
                    .flex()
                    .items_baseline()
                    .gap(S3)
                    .border_b_1()
                    .border_color(t.rule_strong)
                    .child(
                        div().text_base().text_color(t.ink).font_weight(gpui::FontWeight::MEDIUM).child(g.app.clone()),
                    )
                    .child(div().text_xs().text_color(t.ink_3).child(format!(
                        "{} snapshot{}",
                        g.snapshots.len(),
                        if g.snapshots.len() == 1 { "" } else { "s" }
                    )))
                    .child(div().flex_1())
                    .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_2).child(ui::bytes(g.total))),
            );
            let mut rows = div().flex().flex_col();
            for (i, s) in g.snapshots.iter().enumerate() {
                let cid = key_of(s);
                let on = sel.as_deref() == Some(cid.as_str());
                rows = rows.child(
                    ui::list_row(gpui::SharedString::from(format!("snap-{}-{i}", g.app)), on, &t)
                        .py(px(5.))
                        .flex()
                        .items_center()
                        .gap(S4)
                        .child(
                            div()
                                .w(px(150.))
                                .flex_none()
                                .font_family(FONT_MONO)
                                .text_xs()
                                .text_color(if i == 0 { t.ink } else { t.ink_2 })
                                .child(ui::when(&s.created_at)),
                        )
                        .child(
                            div()
                                .flex_1()
                                .min_w_0()
                                .font_family(FONT_MONO)
                                .text_xs()
                                .text_color(t.ink_3)
                                .truncate()
                                .child(s.cid.clone()),
                        )
                        .child(
                            div()
                                .w(px(90.))
                                .flex_none()
                                .flex()
                                .justify_end()
                                .font_family(FONT_MONO)
                                .text_xs()
                                .text_color(t.ink)
                                .child(ui::bytes(s.size)),
                        )
                        .on_click(cx.listener(move |this, _, _, cx| {
                            this.selected = Some(cid.clone());
                            cx.notify()
                        })),
                );
            }
            col = col.child(rows);
        }
        div().id("backup").size_full().overflow_y_scroll().track_scroll(&self.scroll).child(col)
    }
}
