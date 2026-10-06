//! Pulse: is the fleet up, and who is visiting. The status view lists every
//! target with its latest check and uptime windows in tidy mono columns, open
//! incidents called out in Horizon; the traffic view draws hits and uniques
//! per day as bars, with top paths, the status mix and referrers, for one app
//! or all over a chosen range. Read-only: target administration is the
//! console's (a hand-off to the browser; the password is typed there).
//!
//! Pulse answers only its read key (PULSE_READ_KEY); a wrong key is a 303 to
//! the console login, which the client reports as "needs a key".

use crate::app::{self, describe, log, Health};
use crate::shell::{goto, set_health};
use crate::theme::{theme, Theme, FONT_MONO, S1, S2, S3, S4, S5};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, Kind as BtnKind};
use crate::workspace::Workspace;
use farfield_core::api::ext_observe::pulse::{self, Overview, Target, Traffic};
use farfield_core::{ApiError, Freshness, Latest};
use gpui::{div, prelude::*, px, AnyElement, App, Context, Entity, Hsla, SharedString, Window};
use std::sync::Arc;

#[derive(Clone, Copy, PartialEq)]
enum Tab {
    Status,
    Traffic,
}

const RANGES: [(u32, &str); 4] = [(7, "7 days"), (14, "14 days"), (30, "30 days"), (90, "90 days")];

pub struct PulseWs {
    tab: Tab,
    overview: Option<Overview>,
    freshness: Option<Freshness>,
    error: Option<ApiError>,
    loading: bool,
    latest: Arc<Latest>,
    search: Entity<TextField>,
    selected: Option<i64>,
    traffic: Option<Traffic>,
    traffic_app: String,
    range: u32,
    traffic_error: Option<ApiError>,
    traffic_loading: bool,
    traffic_latest: Arc<Latest>,
    hover_day: Option<usize>,
}

/// YYYY-MM-DD for today minus `days` (UTC), without a date crate.
fn day_minus(days: u32) -> String {
    let secs = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_secs()).unwrap_or(0);
    let z = (secs / 86400) as i64 - days as i64 + 719468;
    // Howard Hinnant's civil_from_days
    let era = z.div_euclid(146097);
    let doe = z.rem_euclid(146097);
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = yoe + era * 400 + i64::from(m <= 2);
    format!("{y:04}-{m:02}-{d:02}")
}

fn pct(s: &str) -> Option<f32> {
    s.trim_end_matches('%').parse().ok()
}

impl PulseWs {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        let search = cx.new(|cx| TextField::new(w, cx, "", "Filter targets  ⌘F"));
        cx.subscribe_in(&search, w, |this: &mut Self, _, e: &FieldEvent, _w, cx| match e {
            FieldEvent::Changed => cx.notify(),
            FieldEvent::Down => this.step(1, cx),
            FieldEvent::Up => this.step(-1, cx),
            FieldEvent::Submit if this.selected.is_none() => this.step(1, cx),
            _ => {}
        })
        .detach();
        let mut this = PulseWs {
            tab: Tab::Status,
            overview: None,
            freshness: None,
            error: None,
            loading: false,
            latest: Arc::new(Latest::default()),
            search,
            selected: None,
            traffic: None,
            traffic_app: String::new(),
            range: 14,
            traffic_error: None,
            traffic_loading: false,
            traffic_latest: Arc::new(Latest::default()),
            hover_day: None,
        };
        this.load(cx);
        this
    }

    fn note_error(e: &ApiError, cx: &mut App) {
        if e.is_auth() {
            set_health(cx, "pulse", Health::NoAuth);
        } else if e.is_offline() {
            set_health(cx, "pulse", Health::Down(e.to_string()));
        }
    }

    fn load(&mut self, cx: &mut Context<Self>) {
        let ticket = self.latest.ticket();
        let latest = self.latest.clone();
        let s = app::session(cx);
        self.loading = true;
        cx.notify();
        let task = farfield_core::spawn(async move { pulse::overview(&s).await });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                if !latest.is_current(ticket) {
                    return;
                }
                this.loading = false;
                match r {
                    Ok(Ok(o)) => {
                        set_health(
                            cx,
                            "pulse",
                            match &o.freshness {
                                Freshness::Live => Health::Up,
                                Freshness::Stale { error, .. } => Health::Down(error.to_string()),
                            },
                        );
                        let down = o.value.targets.iter().filter(|t| t.up() == Some(false)).count();
                        log(
                            "pulse-overview",
                            &[("targets", &o.value.targets.len().to_string()), ("down", &down.to_string())],
                        );
                        this.overview = Some(o.value);
                        this.freshness = Some(o.freshness);
                        this.error = None;
                    }
                    Ok(Err(e)) => {
                        Self::note_error(&e, cx);
                        this.error = Some(e);
                    }
                    Err(e) => this.error = Some(ApiError::Decode(e.to_string())),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn load_traffic(&mut self, cx: &mut Context<Self>) {
        let ticket = self.traffic_latest.ticket();
        let latest = self.traffic_latest.clone();
        let s = app::session(cx);
        let (app_name, from, to) = (self.traffic_app.clone(), day_minus(self.range - 1), day_minus(0));
        self.traffic_loading = true;
        cx.notify();
        let task = farfield_core::spawn(async move { pulse::traffic(&s, &app_name, &from, &to).await });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                if !latest.is_current(ticket) {
                    return;
                }
                this.traffic_loading = false;
                match r {
                    Ok(Ok(t)) => {
                        log(
                            "pulse-traffic",
                            &[("app", &t.value.app), ("days", &t.value.hits_per_day.len().to_string())],
                        );
                        this.traffic = Some(t.value);
                        this.traffic_error = None;
                    }
                    Ok(Err(e)) => {
                        Self::note_error(&e, cx);
                        this.traffic_error = Some(e);
                    }
                    Err(e) => this.traffic_error = Some(ApiError::Decode(e.to_string())),
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn set_tab(&mut self, t: Tab, cx: &mut Context<Self>) {
        self.tab = t;
        if t == Tab::Traffic && self.traffic.is_none() {
            self.load_traffic(cx);
        }
        cx.notify();
    }

    fn rows(&self, cx: &App) -> Vec<Target> {
        let q = self.search.read(cx).text().to_lowercase();
        let mut v: Vec<Target> = self
            .overview
            .as_ref()
            .map(|o| {
                o.targets
                    .iter()
                    .filter(|t| q.is_empty() || t.name.to_lowercase().contains(&q) || t.url.to_lowercase().contains(&q))
                    .cloned()
                    .collect()
            })
            .unwrap_or_default();
        // what needs attention first: down, then open incidents, then by name
        v.sort_by_key(|t| (t.up() != Some(false), t.incident.is_none(), t.name.clone()));
        v
    }

    fn step(&mut self, by: i32, cx: &mut Context<Self>) {
        let rows = self.rows(cx);
        if rows.is_empty() {
            return;
        }
        let cur = self.selected.and_then(|id| rows.iter().position(|t| t.id == id));
        let next = match cur {
            None => 0,
            Some(i) => (i as i32 + by).clamp(0, rows.len() as i32 - 1) as usize,
        };
        self.selected = Some(rows[next].id);
        cx.notify();
    }

    fn selected_target(&self) -> Option<&Target> {
        let id = self.selected?;
        self.overview.as_ref()?.targets.iter().find(|t| t.id == id)
    }

    // ── rendering ────────────────────────────────────────────────────────

    fn tabs(&self, cx: &mut Context<Self>) -> AnyElement {
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
        div()
            .flex()
            .items_center()
            .gap(S2)
            .px(S5)
            .py(S2)
            .border_b_1()
            .border_color(t.rule)
            .child(
                tab("t-status", "Status", self.tab == Tab::Status)
                    .on_click(cx.listener(|this, _, _, cx| this.set_tab(Tab::Status, cx))),
            )
            .child(
                tab("t-traffic", "Traffic", self.tab == Tab::Traffic)
                    .on_click(cx.listener(|this, _, _, cx| this.set_tab(Tab::Traffic, cx))),
            )
            .child(div().flex_1())
            .child(ui::button("console", "Administer targets…", BtnKind::Quiet, cx, |_, _, cx| {
                crate::ws::connections::open_console(cx, "pulse")
            }))
            .into_any_element()
    }

    /// The designed moment for "no access": what's wrong and what to do.
    fn blocked(&self, e: &ApiError, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let (head, body) = match e {
            ApiError::Unauthorized(_) => (
                "Pulse needs its read key.",
                "Pulse answers a scoped read key (PULSE_READ_KEY on the server) and nothing else — the fleet's write keys don't open it. Paste the key in Settings → Keys, or use the console in your browser.",
            ),
            ApiError::Offline(_) => ("Pulse can't be reached.", "It's offline or your tailnet is down. The other workspaces keep working; ⌘R tries again."),
            _ => ("Pulse didn't answer as expected.", ""),
        };
        let detail = if body.is_empty() { describe(e) } else { body.to_string() };
        div()
            .p(px(40.))
            .flex()
            .flex_col()
            .gap(S3)
            .max_w(px(620.))
            .child(div().text_xl().text_color(t.ink).child(head))
            .child(div().text_sm().text_color(t.ink_2).child(detail))
            .child(
                div()
                    .flex()
                    .gap(S2)
                    .pt(S2)
                    .when(e.is_auth(), |d| {
                        d.child(ui::button("to-conn", "Open Settings", BtnKind::Primary, cx, |_, _, cx| {
                            goto(cx, "connections")
                        }))
                    })
                    .child(ui::button("to-console", "Open the pulse console", BtnKind::Quiet, cx, |_, _, cx| {
                        crate::ws::connections::open_console(cx, "pulse")
                    })),
            )
            .into_any_element()
    }

    fn status_view(&mut self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        if self.overview.is_none() {
            return match &self.error {
                Some(e) => self.blocked(&e.clone(), cx),
                None => ui::quiet_state("Asking pulse how the fleet is doing…", cx).into_any_element(),
            };
        }
        let o = self.overview.clone().unwrap_or_default();
        let rows = self.rows(cx);
        let up = o.targets.iter().filter(|t| t.up() == Some(true)).count();
        let down = o.targets.iter().filter(|t| t.up() == Some(false)).count();
        let open: Vec<_> = o.targets.iter().filter_map(|t| t.incident.clone().map(|i| (t.name.clone(), i))).collect();

        let mut col = div().flex().flex_col().px(S5).pt(S4).pb(px(64.)).gap(S3);
        // the headline
        col = col.child(
            div()
                .flex()
                .items_baseline()
                .gap(S3)
                .child(div().text_xl().text_color(t.ink).child(if down == 0 && open.is_empty() {
                    "Everything is up.".to_string()
                } else if down == 0 {
                    format!("Up, with {} open incident{}.", open.len(), if open.len() == 1 { "" } else { "s" })
                } else {
                    format!("{down} target{} down.", if down == 1 { "" } else { "s" })
                }))
                .child(ui::mono(format!("{} targets · {up} up · {down} down", o.targets.len()), cx)),
        );
        if let Some(Freshness::Stale { age_ms, .. }) = &self.freshness {
            col = col.child(ui::notice(
                format!("Offline — showing what pulse said {} ago.", crate::ws::content::ago(*age_ms)),
                t.warn,
                cx,
            ));
        } else if let Some(e) = &self.error {
            col = col.child(ui::notice(describe(e), t.bad, cx));
        }
        for (name, i) in &open {
            col = col.child(
                div()
                    .border_l_2()
                    .border_color(t.signal)
                    .pl(px(10.))
                    .py(px(4.))
                    .flex()
                    .flex_col()
                    .child(
                        div()
                            .text_sm()
                            .text_color(t.ink)
                            .child(format!("{name} — incident open since {}", ui::when(&i.opened_at))),
                    )
                    .when(!i.last_err.is_empty(), |d| {
                        d.child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_2).child(i.last_err.clone()))
                    }),
            );
        }
        col = col.child(div().w(px(320.)).pt(S2).child(self.search.clone()));
        // the table
        let num = |s: String, w: f32, c: Hsla| {
            div().w(px(w)).flex_none().flex().justify_end().font_family(FONT_MONO).text_xs().text_color(c).child(s)
        };
        col = col.child(
            div()
                .flex()
                .items_center()
                .px(px(14.))
                .pt(S2)
                .pb(px(6.))
                .border_b_1()
                .border_color(t.rule_strong)
                .text_xs()
                .text_color(t.ink_2)
                .child(div().w(px(70.)).flex_none().child("STATUS"))
                .child(div().flex_1().min_w_0().child("TARGET"))
                .child(num("LATENCY".into(), 84., t.ink_2))
                .child(num("24H".into(), 84., t.ink_2))
                .child(num("7D".into(), 84., t.ink_2))
                .child(num("30D".into(), 84., t.ink_2)),
        );
        if rows.is_empty() {
            col = col.child(ui::quiet_state(
                if o.targets.is_empty() {
                    "Pulse isn't watching anything yet — add targets in the console."
                } else {
                    "No target matches the filter."
                },
                cx,
            ));
        }
        let sel = self.selected;
        for (i, tg) in rows.iter().enumerate() {
            let (word, color) = match (tg.enabled, tg.up()) {
                (false, _) => ("paused", t.ink_3),
                (_, None) => ("pending", t.ink_3),
                (_, Some(true)) => ("up", t.good),
                (_, Some(false)) => ("down", t.bad),
            };
            let lat = tg.last.as_ref().map(|c| format!("{} ms", c.latency_ms)).unwrap_or_else(|| "—".into());
            let up_col = |s: &str| {
                let c = match pct(s) {
                    Some(p) if p < 99.0 => t.warn,
                    Some(_) => t.ink,
                    None => t.ink_3,
                };
                num(s.to_string(), 84., c)
            };
            let id = tg.id;
            let flagged = tg.incident.is_some();
            col = col.child(
                ui::list_row(("tg", i), sel == Some(id), &t)
                    .flex()
                    .items_center()
                    .when(flagged && sel != Some(id), |d| d.border_l_2().border_color(t.signal))
                    .child(div().w(px(70.)).flex_none().child(ui::chip(word, color, cx)))
                    .child(
                        div()
                            .flex_1()
                            .min_w_0()
                            .flex()
                            .flex_col()
                            .child(div().text_sm().text_color(t.ink).truncate().child(tg.name.clone()))
                            .child(
                                div()
                                    .font_family(FONT_MONO)
                                    .text_xs()
                                    .text_color(t.ink_3)
                                    .truncate()
                                    .child(tg.url.clone()),
                            ),
                    )
                    .child(num(lat, 84., t.ink))
                    .child(up_col(&tg.up_24h))
                    .child(up_col(&tg.up_7d))
                    .child(up_col(&tg.up_30d))
                    .on_click(cx.listener(move |this, _, _, cx| {
                        this.selected = Some(id);
                        cx.notify()
                    })),
            );
        }
        // recent incidents
        if !o.incidents.is_empty() {
            col = col.child(div().pt(S5).child(ui::eyebrow("Recent incidents", cx)));
            for inc in o.incidents.iter().take(20) {
                let name = if inc.target_name.is_empty() {
                    o.targets
                        .iter()
                        .find(|t| t.id == inc.target_id)
                        .map(|t| t.name.clone())
                        .unwrap_or_else(|| format!("target {}", inc.target_id))
                } else {
                    inc.target_name.clone()
                };
                col = col.child(
                    div()
                        .flex()
                        .gap(S3)
                        .py(px(6.))
                        .border_b_1()
                        .border_color(t.rule)
                        .child(div().w(px(10.)).flex_none().pt(px(6.)).child(
                            div().w(px(6.)).h(px(6.)).rounded_full().bg(if inc.open() { t.signal } else { t.ink_3 }),
                        ))
                        .child(div().w(px(160.)).flex_none().text_sm().text_color(t.ink).truncate().child(name))
                        .child(
                            div().w(px(300.)).flex_none().font_family(FONT_MONO).text_xs().text_color(t.ink_2).child(
                                format!(
                                    "{} → {}",
                                    ui::when(&inc.opened_at),
                                    if inc.open() { "ongoing".into() } else { ui::when(&inc.closed_at) }
                                ),
                            ),
                        )
                        .child(
                            div()
                                .flex_1()
                                .min_w_0()
                                .font_family(FONT_MONO)
                                .text_xs()
                                .text_color(t.ink_3)
                                .truncate()
                                .child(inc.last_err.clone()),
                        ),
                );
            }
        }
        col.into_any_element()
    }

    fn chip_btn(id: SharedString, label: String, on: bool, t: &Theme) -> gpui::Stateful<gpui::Div> {
        let wash = t.wash;
        div()
            .id(id)
            .px(S2)
            .py(px(2.))
            .rounded(px(3.))
            .text_xs()
            .cursor_pointer()
            .text_color(if on { t.accent_ink } else { t.ink_2 })
            .when(on, |d| d.bg(t.accent))
            .when(!on, move |d| d.hover(move |s| s.bg(wash)))
            .child(label)
    }

    fn traffic_view(&mut self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        if self.traffic.is_none() {
            return match &self.traffic_error {
                Some(e) => self.blocked(&e.clone(), cx),
                None => ui::quiet_state("Counting visits…", cx).into_any_element(),
            };
        }
        let tr = self.traffic.clone().unwrap_or_default();
        let mut col = div().flex().flex_col().px(S5).pt(S4).pb(px(64.)).gap(S4);

        // selectors: app and range
        let mut apps = div()
            .flex()
            .flex_wrap()
            .gap(px(6.))
            .items_center()
            .child(div().w(px(56.)).text_xs().text_color(t.ink_3).child("App"));
        let cur = self.traffic_app.clone();
        for a in std::iter::once(String::new()).chain(tr.apps.iter().cloned()) {
            let on = a == cur;
            let label = if a.is_empty() { "all".to_string() } else { a.clone() };
            apps = apps.child(Self::chip_btn(SharedString::from(format!("app-{label}")), label, on, &t).on_click(
                cx.listener(move |this, _, _, cx| {
                    this.traffic_app = a.clone();
                    this.load_traffic(cx)
                }),
            ));
        }
        let mut ranges = div()
            .flex()
            .gap(px(6.))
            .items_center()
            .child(div().w(px(56.)).text_xs().text_color(t.ink_3).child("Range"));
        for (days, label) in RANGES {
            ranges = ranges.child(
                Self::chip_btn(SharedString::from(format!("r-{days}")), label.into(), self.range == days, &t).on_click(
                    cx.listener(move |this, _, _, cx| {
                        this.range = days;
                        this.load_traffic(cx)
                    }),
                ),
            );
        }
        col = col.child(div().flex().flex_col().gap(S2).child(apps).child(ranges));
        if let Some(e) = &self.traffic_error {
            col = col.child(ui::notice(describe(e), t.bad, cx));
        }

        // headline numbers
        let hits: i64 = tr.hits_per_day.iter().map(|d| d.n).sum();
        let uniq: i64 = tr.uniques_per_day.iter().map(|d| d.n).sum();
        let peak = tr.hits_per_day.iter().max_by_key(|d| d.n).cloned();
        let stat = |label: &str, value: String, sub: String| {
            div()
                .flex()
                .flex_col()
                .gap(px(2.))
                .min_w(px(150.))
                .child(div().text_xs().text_color(t.ink_2).child(label.to_string()))
                .child(div().font_family(FONT_MONO).text_size(px(22.)).text_color(t.ink).child(value))
                .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(sub))
        };
        col = col.child(
            div()
                .flex()
                .gap(S5)
                .child(stat("Hits", hits.to_string(), format!("{} → {}", tr.from, tr.to)))
                .child(stat("Daily uniques, summed", uniq.to_string(), "a visitor counts once a day".into()))
                .child(stat(
                    "Busiest day",
                    peak.as_ref().filter(|p| p.n > 0).map(|p| p.n.to_string()).unwrap_or_else(|| "—".into()),
                    peak.filter(|p| p.n > 0).map(|p| p.day).unwrap_or_default(),
                ))
                .when(self.traffic_loading, |d| d.child(div().text_xs().text_color(t.ink_3).child("updating…"))),
        );

        // the chart: hits as the soft bar, uniques the solid one in front
        let max = tr.hits_per_day.iter().map(|d| d.n).max().unwrap_or(0).max(1) as f32;
        let h = 140.;
        let n = tr.hits_per_day.len();
        let hover = self.hover_day.filter(|i| *i < n);
        let readout = match hover {
            Some(i) => {
                let d = &tr.hits_per_day[i];
                let u = tr.uniques_per_day.get(i).map(|u| u.n).unwrap_or(0);
                format!("{}   {} hits · {} uniques", d.day, d.n, u)
            }
            None => "Hover a day for its numbers.".into(),
        };
        let bars = tr.hits_per_day.iter().enumerate().map(|(i, d)| {
            let u = tr.uniques_per_day.get(i).map(|u| u.n).unwrap_or(0) as f32;
            let on = hover == Some(i);
            div()
                .id(("bar", i))
                .flex_1()
                .h_full()
                .flex()
                .items_end()
                .relative()
                .when(on, |b| b.bg(t.wash))
                .on_hover(cx.listener(move |this, over: &bool, _, cx| {
                    if *over {
                        this.hover_day = Some(i);
                    } else if this.hover_day == Some(i) {
                        this.hover_day = None;
                    }
                    cx.notify()
                }))
                .child(
                    div()
                        .absolute()
                        .bottom_0()
                        .left(px(1.))
                        .right(px(1.))
                        .h(px((d.n as f32 / max * h).max(if d.n > 0 { 2. } else { 1. })))
                        .bg(if d.n > 0 { t.accent_soft } else { t.rule }),
                )
                .child(div().absolute().bottom_0().left(px(1.)).right(px(1.)).h(px(u / max * h)).bg(t.accent))
        });
        col = col.child(
            div()
                .flex()
                .flex_col()
                .gap(px(6.))
                .child(
                    div()
                        .font_family(FONT_MONO)
                        .text_xs()
                        .text_color(if hover.is_some() { t.ink } else { t.ink_3 })
                        .child(readout),
                )
                .child(
                    div()
                        .h(px(h))
                        .flex()
                        .items_end()
                        .gap(px(1.))
                        .border_b_1()
                        .border_color(t.rule_strong)
                        .children(bars),
                )
                .child(
                    div()
                        .flex()
                        .justify_between()
                        .font_family(FONT_MONO)
                        .text_xs()
                        .text_color(t.ink_3)
                        .child(tr.hits_per_day.first().map(|d| d.day.clone()).unwrap_or_default())
                        .child(
                            div()
                                .flex()
                                .gap(S3)
                                .child(
                                    div()
                                        .flex()
                                        .items_center()
                                        .gap(px(4.))
                                        .child(div().w(px(8.)).h(px(8.)).bg(t.accent_soft))
                                        .child("hits"),
                                )
                                .child(
                                    div()
                                        .flex()
                                        .items_center()
                                        .gap(px(4.))
                                        .child(div().w(px(8.)).h(px(8.)).bg(t.accent))
                                        .child("uniques"),
                                ),
                        )
                        .child(tr.hits_per_day.last().map(|d| d.day.clone()).unwrap_or_default()),
                ),
        );

        // status mix: one stacked bar and a legend
        let mix_total: i64 = tr.status_mix.iter().map(|b| b.hits).sum();
        if mix_total > 0 {
            let color = |b: &str| match b.chars().next() {
                Some('2') => t.good,
                Some('3') => t.ink_3,
                Some('4') => t.warn,
                Some('5') => t.bad,
                _ => t.rule_strong,
            };
            col = col.child(ui::rule(cx)).child(ui::eyebrow("Status mix", cx)).child(
                div().w_full().h(px(10.)).flex().rounded(px(2.)).overflow_hidden().children(
                    tr.status_mix.iter().filter(|b| b.hits > 0).map(|b| {
                        div()
                            .h_full()
                            .flex_grow()
                            .flex_basis(gpui::relative(b.hits as f32 / mix_total as f32))
                            .bg(color(&b.bucket))
                    }),
                ),
            );
            col = col.child(div().flex().gap(S4).children(tr.status_mix.iter().map(|b| {
                div()
                    .flex()
                    .items_center()
                    .gap(px(5.))
                    .font_family(FONT_MONO)
                    .text_xs()
                    .text_color(t.ink_2)
                    .child(div().w(px(8.)).h(px(8.)).rounded_full().bg(color(&b.bucket)))
                    .child(format!("{} {} ({:.1}%)", b.bucket, b.hits, b.hits as f32 * 100. / mix_total as f32))
            })));
        }

        // tables with proportional bars
        let table = |title: &str, rows: Vec<(String, String, i64, Option<i64>)>, cx: &App| {
            let max = rows.iter().map(|r| r.2).max().unwrap_or(1).max(1) as f32;
            let mut d = div().flex().flex_col().flex_1().min_w(px(300.)).child(ui::eyebrow(title.to_string(), cx));
            if rows.is_empty() {
                d = d.child(div().py(S2).text_sm().text_color(t.ink_3).child("Nothing in this range."));
            }
            for (label, sub, n, u) in rows {
                d = d.child(
                    div()
                        .relative()
                        .py(px(5.))
                        .border_b_1()
                        .border_color(t.rule)
                        .child(
                            div()
                                .absolute()
                                .left_0()
                                .top(px(2.))
                                .bottom(px(2.))
                                .w(gpui::relative(n as f32 / max))
                                .bg(t.wash),
                        )
                        .child(
                            div()
                                .relative()
                                .flex()
                                .items_center()
                                .gap(S2)
                                .px(px(4.))
                                .child(
                                    div()
                                        .flex_1()
                                        .min_w_0()
                                        .font_family(FONT_MONO)
                                        .text_xs()
                                        .text_color(t.ink)
                                        .truncate()
                                        .child(label),
                                )
                                .when(!sub.is_empty(), |r| r.child(div().text_xs().text_color(t.ink_3).child(sub)))
                                .child(
                                    div()
                                        .w(px(56.))
                                        .flex()
                                        .justify_end()
                                        .font_family(FONT_MONO)
                                        .text_xs()
                                        .text_color(t.ink)
                                        .child(n.to_string()),
                                )
                                .when_some(u, |r, u| {
                                    r.child(
                                        div()
                                            .w(px(56.))
                                            .flex()
                                            .justify_end()
                                            .font_family(FONT_MONO)
                                            .text_xs()
                                            .text_color(t.ink_2)
                                            .child(u.to_string()),
                                    )
                                }),
                        ),
                );
            }
            d
        };
        let all = self.traffic_app.is_empty();
        let paths = tr
            .top_paths
            .iter()
            .map(|p| (p.path.clone(), if all { p.app.clone() } else { String::new() }, p.hits, Some(p.uniques)))
            .collect();
        let refs = tr.top_referrers.iter().map(|r| (r.host.clone(), String::new(), r.hits, None)).collect();
        col = col.child(ui::rule(cx)).child(
            div().flex().flex_wrap().gap(S5).child(table("Top paths  ·  hits / uniques", paths, cx)).child(table(
                "Referrers",
                refs,
                cx,
            )),
        );
        col.into_any_element()
    }
}

impl Workspace for PulseWs {
    fn inspector(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> Option<AnyElement> {
        let t = theme(cx).clone();
        if self.tab != Tab::Status {
            return None;
        }
        let tg = self.selected_target()?.clone();
        let mut col = div().flex().flex_col().gap(S1);
        col = col.child(ui::eyebrow("Target", cx)).child(div().text_lg().text_color(t.ink).child(tg.name.clone()));
        col = col.child(ui::field_row("URL", ui::mono(tg.url.clone(), cx), cx));
        col = col.child(ui::field_row(
            "Check",
            ui::mono(format!("{} · expects {} · every {}s", tg.method, tg.expected_status, tg.interval_s), cx),
            cx,
        ));
        if !tg.enabled {
            col = col.child(ui::chip("paused — not being checked", t.ink_3, cx));
        }
        col = col.child(div().py(S2).child(ui::rule(cx))).child(ui::eyebrow("Latest check", cx));
        match &tg.last {
            Some(c) => {
                col = col
                    .child(ui::chip(if c.ok { "passed" } else { "failed" }, if c.ok { t.good } else { t.bad }, cx))
                    .child(ui::field_row("At", ui::mono(c.ts.clone(), cx), cx))
                    .child(ui::field_row(
                        "Status · latency",
                        ui::mono(format!("{} · {} ms", c.status_code, c.latency_ms), cx),
                        cx,
                    ));
                if !c.err.is_empty() {
                    col = col.child(ui::field_row("Error", ui::mono(c.err.clone(), cx).text_color(t.bad), cx));
                }
            }
            None => col = col.child(div().text_sm().text_color(t.ink_2).child("Not checked yet.")),
        }
        col = col.child(div().py(S2).child(ui::rule(cx))).child(ui::eyebrow("Uptime", cx)).child(
            div().flex().gap(S4).children(
                [("24h", tg.up_24h.clone()), ("7d", tg.up_7d.clone()), ("30d", tg.up_30d.clone())].into_iter().map(
                    |(l, v)| {
                        div()
                            .flex()
                            .flex_col()
                            .child(div().text_xs().text_color(t.ink_2).child(l))
                            .child(div().font_family(FONT_MONO).text_sm().text_color(t.ink).child(v))
                    },
                ),
            ),
        );
        if let Some(i) = &tg.incident {
            col = col
                .child(div().py(S2).child(ui::rule(cx)))
                .child(ui::eyebrow("Open incident", cx))
                .child(ui::notice(format!("Since {}", ui::when(&i.opened_at)), t.signal, cx))
                .when(!i.last_err.is_empty(), |d| {
                    d.child(ui::field_row("Last error", ui::mono(i.last_err.clone(), cx), cx))
                });
        }
        let u = tg.url.clone();
        col = col.child(
            div()
                .flex()
                .flex_wrap()
                .gap(S2)
                .pt(S3)
                .child(ui::button("open-url", "Open URL", BtnKind::Quiet, cx, move |_, _, cx| cx.open_url(&u)))
                .child(ui::button("admin", "Administer…", BtnKind::Quiet, cx, |_, _, cx| {
                    crate::ws::connections::open_console(cx, "pulse")
                })),
        );
        Some(col.into_any_element())
    }

    fn focus_search(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        if self.tab != Tab::Status {
            self.set_tab(Tab::Status, cx);
        }
        self.search.read(cx).focus(w);
    }

    fn refresh(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        self.load(cx);
        if self.tab == Tab::Traffic {
            self.load_traffic(cx);
        }
    }

    fn commands(&self, _cx: &App) -> Vec<(&'static str, String, &'static str)> {
        vec![
            ("status", "Pulse: status".into(), ""),
            ("traffic", "Pulse: traffic".into(), ""),
            ("r7", "Pulse: traffic over 7 days".into(), ""),
            ("r30", "Pulse: traffic over 30 days".into(), ""),
            ("r90", "Pulse: traffic over 90 days".into(), ""),
            ("console", "Pulse: administer targets (browser)".into(), ""),
            ("refresh", "Pulse: refresh".into(), "⌘R"),
        ]
    }

    fn run_command(&mut self, id: &str, w: &mut Window, cx: &mut Context<Self>) {
        match id {
            "status" => self.set_tab(Tab::Status, cx),
            "traffic" => self.set_tab(Tab::Traffic, cx),
            "r7" | "r30" | "r90" => {
                self.range = id[1..].parse().unwrap_or(14);
                self.tab = Tab::Traffic;
                self.load_traffic(cx)
            }
            "console" => crate::ws::connections::open_console(cx, "pulse"),
            "refresh" => Workspace::refresh(self, w, cx),
            _ => {}
        }
    }
}

impl Render for PulseWs {
    fn render(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let tabs = self.tabs(cx);
        let body = match self.tab {
            Tab::Status => self.status_view(cx),
            Tab::Traffic => self.traffic_view(cx),
        };
        div()
            .size_full()
            .flex()
            .flex_col()
            .child(tabs)
            .child(div().id("pulse-scroll").flex_1().min_h_0().overflow_y_scroll().child(body))
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn day_arithmetic() {
        let d = super::day_minus(0);
        assert_eq!(d.len(), 10);
        assert!(super::day_minus(1) < d && super::day_minus(400) < super::day_minus(1));
    }
}
