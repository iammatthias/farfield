//! Switchboard: the text line to the fleet, inspected. Its health (/status:
//! messages logged, webhook secret configured, Photon line connected), the
//! recent messages (sender, body, route, status, reply) and the agent's jobs
//! (prompt, status, result or error, duration). Read-only by design — no
//! replay, no cancel. Refreshes itself every 30 s; a slow response that a
//! newer one overtook is dropped.

use crate::app::{self, describe, log, Health};
use crate::shell::{goto, set_health};
use crate::theme::{theme, FONT_MONO, S1, S2, S3, S4, S5};
use crate::ui::input::{FieldEvent, TextField};
use crate::ui::{self, Kind as BtnKind};
use crate::workspace::Workspace;
use farfield_core::api::ext_observe::switchboard::{self, Job, Message, Status};
use farfield_core::{ApiError, Freshness, Latest};
use gpui::{
    div, prelude::*, px, uniform_list, AnyElement, App, Context, Entity, Hsla, UniformListScrollHandle, Window,
};
use std::sync::Arc;
use std::time::{Duration, Instant};

const LIMIT: u32 = 200;
const EVERY: Duration = Duration::from_secs(30);

#[derive(Clone, Copy, PartialEq)]
enum Tab {
    Messages,
    Jobs,
}

#[derive(Clone)]
enum Row {
    Msg(Message),
    Job(Job),
}

impl Row {
    fn id(&self) -> &str {
        match self {
            Row::Msg(m) => &m.id,
            Row::Job(j) => &j.id,
        }
    }
}

pub struct SwitchboardWs {
    tab: Tab,
    status: Option<Status>,
    messages: Vec<Message>,
    jobs: Vec<Job>,
    loaded: bool,
    loading: bool,
    freshness: Option<Freshness>,
    error: Option<ApiError>,
    latest: Arc<Latest>,
    refreshed: Option<Instant>,
    search: Entity<TextField>,
    scroll: UniformListScrollHandle,
    selected: Option<String>,
}

/// Seconds between two RFC 3339 UTC timestamps (`…T12:00:05Z`), when both parse.
fn secs_between(a: &str, b: &str) -> Option<i64> {
    fn parse(s: &str) -> Option<i64> {
        let s = s.get(..19)?;
        let n = |r: std::ops::Range<usize>| s.get(r)?.parse::<i64>().ok();
        let (y, mo, d, h, mi, se) = (n(0..4)?, n(5..7)?, n(8..10)?, n(11..13)?, n(14..16)?, n(17..19)?);
        // days from civil (Hinnant)
        let y2 = if mo <= 2 { y - 1 } else { y };
        let era = y2.div_euclid(400);
        let yoe = y2 - era * 400;
        let mp = (mo + 9) % 12;
        let doy = (153 * mp + 2) / 5 + d - 1;
        let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
        Some((era * 146097 + doe - 719468) * 86400 + h * 3600 + mi * 60 + se)
    }
    Some(parse(b)? - parse(a)?)
}

fn duration(j: &Job) -> String {
    match secs_between(&j.started_at, &j.finished_at) {
        Some(s) if s >= 3600 => format!("{}h {:02}m", s / 3600, s % 3600 / 60),
        Some(s) if s >= 60 => format!("{}m {:02}s", s / 60, s % 60),
        Some(s) => format!("{s}s"),
        None if j.finished_at.is_empty() && !j.started_at.is_empty() => "running".into(),
        None => "—".into(),
    }
}

impl SwitchboardWs {
    pub fn new(w: &mut Window, cx: &mut Context<Self>) -> Self {
        let search = cx.new(|cx| TextField::new(w, cx, "", "Filter by sender, text, route  ⌘F"));
        cx.subscribe_in(&search, w, |this: &mut Self, _, e: &FieldEvent, _w, cx| match e {
            FieldEvent::Changed => cx.notify(),
            FieldEvent::Down => this.step(1, cx),
            FieldEvent::Up => this.step(-1, cx),
            FieldEvent::Submit if this.selected.is_none() => this.step(1, cx),
            _ => {}
        })
        .detach();
        let this = SwitchboardWs {
            tab: Tab::Messages,
            status: None,
            messages: Vec::new(),
            jobs: Vec::new(),
            loaded: false,
            loading: false,
            freshness: None,
            error: None,
            latest: Arc::new(Latest::default()),
            refreshed: None,
            search,
            scroll: UniformListScrollHandle::new(),
            selected: None,
        };
        // the poll: now, then every 30 s while this workspace exists
        cx.spawn(async move |this, cx| loop {
            if this.update(cx, |this, cx| this.load(cx)).is_err() {
                break;
            }
            cx.background_executor().timer(EVERY).await;
        })
        .detach();
        // a 5 s tick keeps "refreshed 12s ago" honest
        cx.spawn(async move |this, cx| loop {
            cx.background_executor().timer(Duration::from_secs(5)).await;
            if this.update(cx, |_, cx| cx.notify()).is_err() {
                break;
            }
        })
        .detach();
        this
    }

    fn load(&mut self, cx: &mut Context<Self>) {
        let ticket = self.latest.ticket();
        let latest = self.latest.clone();
        let s = app::session(cx);
        self.loading = true;
        cx.notify();
        let task = farfield_core::spawn(async move {
            let st = switchboard::status(&s).await;
            let m = switchboard::messages(&s, LIMIT).await;
            let j = switchboard::jobs(&s, LIMIT).await;
            (st, m, j)
        });
        cx.spawn(async move |this, cx| {
            let r = task.await;
            let _ = this.update(cx, |this, cx| {
                // a newer refresh superseded this one
                if !latest.is_current(ticket) {
                    return;
                }
                this.loading = false;
                let Ok((st, m, j)) = r else { return };
                if let Ok(st) = st {
                    this.status = Some(st);
                }
                match (m, j) {
                    (Ok(m), Ok(j)) => {
                        set_health(
                            cx,
                            "switchboard",
                            match &m.freshness {
                                Freshness::Live => Health::Up,
                                Freshness::Stale { error, .. } => Health::Down(error.to_string()),
                            },
                        );
                        log(
                            "switchboard-refresh",
                            &[("messages", &m.value.len().to_string()), ("jobs", &j.value.len().to_string())],
                        );
                        this.messages = m.value;
                        this.jobs = j.value;
                        this.freshness = Some(m.freshness);
                        this.error = None;
                        this.loaded = true;
                        this.refreshed = Some(Instant::now());
                    }
                    // one half failing must not throw away the half that arrived
                    (Ok(m), Err(e)) => {
                        this.messages = m.value;
                        this.loaded = true;
                        this.error = Some(e);
                    }
                    (Err(e), Ok(j)) => {
                        if e.is_auth() {
                            set_health(cx, "switchboard", Health::NoAuth);
                        }
                        this.jobs = j.value;
                        this.error = Some(e);
                    }
                    (Err(e), Err(_)) => {
                        if e.is_auth() {
                            set_health(cx, "switchboard", Health::NoAuth);
                        } else if e.is_offline() {
                            set_health(cx, "switchboard", Health::Down(e.to_string()));
                        }
                        this.error = Some(e);
                    }
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn rows(&self, cx: &App) -> Vec<Row> {
        let q = self.search.read(cx).text().to_lowercase();
        let has = |s: &str| s.to_lowercase().contains(&q);
        match self.tab {
            Tab::Messages => self
                .messages
                .iter()
                .filter(|m| {
                    q.is_empty() || has(&m.sender) || has(&m.body) || has(&m.route) || has(&m.reply) || has(&m.status)
                })
                .cloned()
                .map(Row::Msg)
                .collect(),
            Tab::Jobs => self
                .jobs
                .iter()
                .filter(|j| {
                    q.is_empty()
                        || has(&j.sender)
                        || has(&j.prompt)
                        || has(&j.status)
                        || has(&j.result)
                        || has(&j.error)
                })
                .cloned()
                .map(Row::Job)
                .collect(),
        }
    }

    fn step(&mut self, by: i32, cx: &mut Context<Self>) {
        let rows = self.rows(cx);
        if rows.is_empty() {
            return;
        }
        let cur = self.selected.as_ref().and_then(|k| rows.iter().position(|r| r.id() == k));
        let next = match cur {
            None => 0,
            Some(i) => (i as i32 + by).clamp(0, rows.len() as i32 - 1) as usize,
        };
        self.scroll.scroll_to_item(next, gpui::ScrollStrategy::Center);
        self.selected = Some(rows[next].id().to_string());
        cx.notify();
    }

    fn set_tab(&mut self, t: Tab, cx: &mut Context<Self>) {
        if self.tab != t {
            self.tab = t;
            self.selected = None;
            cx.notify();
        }
    }

    fn status_color(s: &str, t: &crate::theme::Theme) -> Hsla {
        match s {
            "replied" | "done" | "ok" | "sent" | "succeeded" | "success" | "complete" | "completed" => t.good,
            "failed" | "error" | "errored" => t.bad,
            "running" | "pending" | "queued" | "received" => t.warn,
            _ => t.ink_3,
        }
    }

    fn header(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let mut health = div().flex().flex_wrap().items_center().gap_x(S3).gap_y(px(4.));
        match &self.status {
            Some(st) => {
                health = health
                    .child(ui::mono(format!("{} messages logged", st.messages), cx))
                    .child(ui::chip(
                        if st.hook { "webhook secret set" } else { "no webhook secret" },
                        if st.hook { t.good } else { t.bad },
                        cx,
                    ))
                    .child(ui::chip(
                        if st.line { "line connected" } else { "line not connected" },
                        if st.line { t.good } else { t.bad },
                        cx,
                    ));
            }
            None => health = health.child(div().text_xs().text_color(t.ink_3).child("checking the line…")),
        }
        let ago = match (self.loading, self.refreshed) {
            (true, _) => "refreshing…".to_string(),
            (false, Some(at)) => format!("refreshed {}s ago · every 30s", at.elapsed().as_secs()),
            _ => String::new(),
        };
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
        div()
            .flex()
            .flex_col()
            .gap(S2)
            .px(S4)
            .pt(S3)
            .pb(S2)
            .border_b_1()
            .border_color(t.rule)
            .child(health)
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(S2)
                    .child(
                        tab("t-msg", format!("Messages {}", self.messages.len()), self.tab == Tab::Messages)
                            .on_click(cx.listener(|this, _, _, cx| this.set_tab(Tab::Messages, cx))),
                    )
                    .child(
                        tab("t-jobs", format!("Agent jobs {}", self.jobs.len()), self.tab == Tab::Jobs)
                            .on_click(cx.listener(|this, _, _, cx| this.set_tab(Tab::Jobs, cx))),
                    )
                    .child(div().flex_1()),
            )
            .child(self.search.clone())
            .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(ago))
            .into_any_element()
    }

    fn list(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        if !self.loaded {
            return match &self.error {
                Some(e) => {
                    let auth = e.is_auth();
                    div()
                        .p(px(32.))
                        .flex()
                        .flex_col()
                        .gap(S3)
                        .max_w(px(560.))
                        .child(div().text_lg().text_color(t.ink).child(if auth { "Switchboard needs its write key." } else { "Switchboard can't be read right now." }))
                        .child(div().text_sm().text_color(t.ink_2).child(if auth {
                            "The message log is private: it answers the write key only, and only over your tailnet. Add the key in Settings → Keys.".to_string()
                        } else {
                            describe(e)
                        }))
                        .when(auth, |d| d.child(div().child(ui::button("conn", "Open Settings", BtnKind::Primary, cx, |_, _, cx| goto(cx, "connections")))))
                        .into_any_element()
                }
                None => ui::quiet_state("Reading the line…", cx).into_any_element(),
            };
        }
        let rows = Arc::new(self.rows(cx));
        if rows.is_empty() {
            let q = !self.search.read(cx).text().is_empty();
            return ui::quiet_state(
                match (q, self.tab) {
                    (true, _) => "Nothing matches the filter.",
                    (false, Tab::Messages) => {
                        "No messages yet. Text the line and they'll appear here within 30 seconds."
                    }
                    (false, Tab::Jobs) => "The agent hasn't run yet — prose texts (not /commands) start a job.",
                },
                cx,
            )
            .into_any_element();
        }
        let sel = self.selected.clone();
        let ent = cx.entity();
        let n = rows.len();
        uniform_list("sb-list", n, move |range, _w, cx| {
            let t = theme(cx).clone();
            range
                .map(|i| {
                    let row = rows[i].clone();
                    let id = row.id().to_string();
                    let on = sel.as_deref() == Some(id.as_str());
                    let (who, when, text, status, meta) = match &row {
                        Row::Msg(m) => (
                            m.sender.clone(),
                            ui::when(&m.received_at),
                            m.body.replace('\n', " "),
                            m.status.clone(),
                            if m.route.is_empty() { String::new() } else { format!("→ {}", m.route) },
                        ),
                        Row::Job(j) => (
                            j.sender.clone(),
                            ui::when(&j.started_at),
                            j.prompt.replace('\n', " "),
                            j.status.clone(),
                            duration(j),
                        ),
                    };
                    let e = ent.clone();
                    ui::list_row(("sb", i), on, &t)
                        .flex()
                        .flex_col()
                        .gap(px(2.))
                        .child(
                            div()
                                .flex()
                                .gap(S2)
                                .items_center()
                                .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_2).truncate().child(who))
                                .child(div().flex_1())
                                .child(div().font_family(FONT_MONO).text_xs().text_color(t.ink_3).child(when)),
                        )
                        .child(div().text_sm().text_color(t.ink).truncate().child(if text.is_empty() {
                            "(empty)".into()
                        } else {
                            text
                        }))
                        .child(
                            div()
                                .flex()
                                .gap(S3)
                                .items_center()
                                .child(ui::chip(
                                    if status.is_empty() { "—".to_string() } else { status.clone() },
                                    Self::status_color(&status, &t),
                                    cx,
                                ))
                                .child(
                                    div().font_family(FONT_MONO).text_xs().text_color(t.ink_3).truncate().child(meta),
                                ),
                        )
                        .on_click(move |_, _, cx| {
                            e.update(cx, |this, cx| {
                                this.selected = Some(id.clone());
                                cx.notify()
                            })
                        })
                        .into_any_element()
                })
                .collect()
        })
        .track_scroll(self.scroll.clone())
        .flex_1()
        .into_any_element()
    }

    fn detail(&self, cx: &mut Context<Self>) -> AnyElement {
        let t = theme(cx).clone();
        let block = |label: &str, body: String, color: Hsla| {
            div().flex().flex_col().gap(S1).child(ui::eyebrow(label.to_string(), cx)).child(
                div().text_size(px(15.)).line_height(px(23.)).text_color(color).child(if body.is_empty() {
                    "—".into()
                } else {
                    body
                }),
            )
        };
        let Some(k) = &self.selected else {
            return ui::quiet_state(
                "Choose a message or a job to read it in full. ↑/↓ from the filter moves through the list.",
                cx,
            )
            .into_any_element();
        };
        let mut col = div().flex().flex_col().gap(S5).px(px(40.)).py(S5).max_w(px(760.));
        if let Some(m) = self.messages.iter().find(|m| &m.id == k).filter(|_| self.tab == Tab::Messages) {
            col = col
                .child(
                    div()
                        .flex()
                        .gap(S3)
                        .items_center()
                        .child(div().font_family(FONT_MONO).text_sm().text_color(t.ink).child(m.sender.clone()))
                        .child(ui::chip(m.status.clone(), Self::status_color(&m.status, &t), cx))
                        .child(div().flex_1())
                        .child(ui::mono(m.received_at.clone(), cx)),
                )
                .child(block("They sent", m.body.clone(), t.ink))
                .child(ui::rule(cx))
                .child(block("Switchboard replied", m.reply.clone(), t.ink));
            if let Some(j) = self.jobs.iter().find(|j| j.message_id == m.id) {
                let jid = j.id.clone();
                col = col.child(ui::button(
                    "to-job",
                    format!("Agent job · {} · {}", j.status, duration(j)),
                    BtnKind::Quiet,
                    cx,
                    {
                        let e = cx.entity();
                        move |_, _, cx| {
                            e.update(cx, |this, cx| {
                                this.tab = Tab::Jobs;
                                this.selected = Some(jid.clone());
                                cx.notify()
                            })
                        }
                    },
                ));
            }
        } else if let Some(j) = self.jobs.iter().find(|j| &j.id == k).filter(|_| self.tab == Tab::Jobs) {
            col = col
                .child(
                    div()
                        .flex()
                        .gap(S3)
                        .items_center()
                        .child(div().font_family(FONT_MONO).text_sm().text_color(t.ink).child(j.sender.clone()))
                        .child(ui::chip(j.status.clone(), Self::status_color(&j.status, &t), cx))
                        .child(div().flex_1())
                        .child(ui::mono(duration(j), cx)),
                )
                .child(block("Prompt", j.prompt.clone(), t.ink))
                .child(ui::rule(cx));
            if !j.error.is_empty() {
                col = col.child(block("Error", j.error.clone(), t.bad));
            }
            if !j.result.is_empty() || j.error.is_empty() {
                col = col.child(block("Result", j.result.clone(), t.ink));
            }
        } else {
            return ui::quiet_state("That item is no longer in the recent log.", cx).into_any_element();
        }
        col.into_any_element()
    }
}

impl Workspace for SwitchboardWs {
    fn inspector(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> Option<AnyElement> {
        let k = self.selected.clone()?;
        let mut col = div().flex().flex_col().gap(S1);
        if let Some(m) = self.messages.iter().find(|m| m.id == k).filter(|_| self.tab == Tab::Messages).cloned() {
            col = col
                .child(ui::eyebrow("Message", cx))
                .child(ui::field_row("Sender", ui::mono(m.sender, cx), cx))
                .child(ui::field_row("Received", ui::mono(m.received_at, cx), cx))
                .child(ui::field_row("Route", ui::mono(if m.route.is_empty() { "—".into() } else { m.route }, cx), cx))
                .child(ui::field_row(
                    "Ref",
                    ui::mono(if m.reference.is_empty() { "—".into() } else { m.reference }, cx),
                    cx,
                ))
                .child(ui::field_row("Direction", ui::mono(m.direction, cx), cx))
                .child(ui::field_row("ID", ui::mono(m.id, cx), cx));
        } else {
            let j = self.jobs.iter().find(|j| j.id == k).filter(|_| self.tab == Tab::Jobs).cloned()?;
            let d = duration(&j);
            col = col
                .child(ui::eyebrow("Agent job", cx))
                .child(ui::field_row("Sender", ui::mono(j.sender, cx), cx))
                .child(ui::field_row("Started", ui::mono(j.started_at, cx), cx))
                .child(ui::field_row(
                    "Finished",
                    ui::mono(if j.finished_at.is_empty() { "—".into() } else { j.finished_at }, cx),
                    cx,
                ))
                .child(ui::field_row("Duration", ui::mono(d, cx), cx))
                .child(ui::field_row("Message", ui::mono(j.message_id, cx), cx))
                .child(ui::field_row("ID", ui::mono(j.id, cx), cx));
        }
        col = col.child(
            div().pt(S3).text_xs().text_color(theme(cx).ink_3).child("Read-only: nothing here replays or cancels."),
        );
        Some(col.into_any_element())
    }

    fn focus_search(&mut self, w: &mut Window, cx: &mut Context<Self>) {
        self.search.read(cx).focus(w);
    }

    fn refresh(&mut self, _w: &mut Window, cx: &mut Context<Self>) {
        self.load(cx)
    }

    fn commands(&self, _cx: &App) -> Vec<(&'static str, String, &'static str)> {
        vec![
            ("messages", "Switchboard: messages".into(), ""),
            ("jobs", "Switchboard: agent jobs".into(), ""),
            ("refresh", "Switchboard: refresh now".into(), "⌘R"),
            ("console", "Switchboard: open the console (browser)".into(), ""),
        ]
    }

    fn run_command(&mut self, id: &str, _w: &mut Window, cx: &mut Context<Self>) {
        match id {
            "messages" => self.set_tab(Tab::Messages, cx),
            "jobs" => self.set_tab(Tab::Jobs, cx),
            "refresh" => self.load(cx),
            "console" => crate::ws::connections::open_console(cx, "switchboard"),
            _ => {}
        }
    }
}

impl Render for SwitchboardWs {
    fn render(&mut self, _w: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let t = theme(cx).clone();
        let notice = match (&self.error, &self.freshness) {
            (Some(e), _) if self.loaded => Some(ui::notice(describe(e), t.bad, cx)),
            (None, Some(Freshness::Stale { age_ms, .. })) => Some(ui::notice(
                format!("Offline — showing the log as it was {} ago.", crate::ws::content::ago(*age_ms)),
                t.warn,
                cx,
            )),
            _ => None,
        };
        let header = self.header(cx);
        let list = self.list(cx);
        let detail = self.detail(cx);
        div()
            .size_full()
            .flex()
            .child(
                div()
                    .w(px(380.))
                    .flex_none()
                    .h_full()
                    .flex()
                    .flex_col()
                    .border_r_1()
                    .border_color(t.rule)
                    .child(header)
                    .when_some(notice, |d, n| d.child(div().px(S4).py(S2).child(n)))
                    .child(list),
            )
            .child(div().id("sb-detail").flex_1().min_w_0().h_full().overflow_y_scroll().child(detail))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn durations() {
        let j = |a: &str, b: &str| Job { started_at: a.into(), finished_at: b.into(), ..Default::default() };
        assert_eq!(duration(&j("2026-10-05T23:59:50Z", "2026-10-06T00:01:05Z")), "1m 15s");
        assert_eq!(duration(&j("2026-10-05T10:00:00Z", "2026-10-05T10:00:09Z")), "9s");
        assert_eq!(duration(&j("2026-10-05T23:30:01Z", "2026-10-06T01:35:03Z")), "2h 05m");
        assert_eq!(duration(&j("2026-10-05T10:00:00Z", "")), "running");
        assert_eq!(duration(&j("", "")), "—");
    }
}
